package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Default paths for service lifecycle state. Each can be overridden
// via the per-subcommand flags, but the defaults are a stable
// convention so callers can rely on `~/.signatory/serve.pid` and
// `~/.signatory/logs/serve.log` without cross-referencing the
// command invocation.
const (
	defaultPidPath = "~/.signatory/serve.pid"
	defaultLogPath = "~/.signatory/logs/serve.log"

	// stopGraceSeconds bounds how long Stop / Restart wait for a
	// SIGTERM'd service to exit before escalating to SIGKILL.
	// Five seconds is generous for a read-only HTTP server over
	// SQLite — any longer and something is genuinely wedged.
	stopGraceSeconds = 5

	// startProbeSeconds bounds how long Start waits for the new
	// process to begin listening on its port before reporting
	// failure. A healthy server binds in tens of milliseconds;
	// three seconds accommodates cold DB open + cert load on
	// slow disks.
	startProbeSeconds = 3
)

// ServeStartCmd launches the pipeline service detached in the
// background and returns once the port is confirmed listening.
//
// Replaces the bash ceremony `nohup signatory serve > log 2>&1
// </dev/null & disown`. Writes a pidfile so Stop / Status / Restart
// can find the running process; logs stdout+stderr to a file since
// the daemonized process has no terminal.
//
// Refuses to start if a pidfile already names a live process — use
// Restart or Stop explicitly rather than accidentally starting a
// second instance racing for the port.
type ServeStartCmd struct {
	ServeRunCmd // Embed so `--port`, `--tls-cert`, etc. are available.

	PidPath string `help:"Path to the pidfile used for lifecycle management." default:"~/.signatory/serve.pid" type:"path"`
	LogPath string `help:"Path to the service log file (appended on restart)." default:"~/.signatory/logs/serve.log" type:"path"`
}

func (cmd *ServeStartCmd) Run(_ *Globals) error {
	// Refuse to clobber a live instance.
	existing, err := readPidFile(cmd.PidPath)
	if err == nil && isProcessAlive(existing) {
		return fmt.Errorf("service already running (PID %d); use `signatory serve restart` or `signatory serve stop` first",
			existing)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		// Pidfile exists but unreadable / malformed → clear it
		// rather than refuse. The start path wins over a stale or
		// corrupt pidfile.
		_ = os.Remove(cmd.PidPath)
	}

	// Ensure log directory exists; open log in append mode so
	// restart history survives.
	logPath, err := expandPath(cmd.LogPath)
	if err != nil {
		return fmt.Errorf("resolve --log-path %q: %w", cmd.LogPath, err)
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o750); err != nil {
		return fmt.Errorf("create log directory %q: %w", filepath.Dir(logPath), err)
	}
	logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640) //nolint:gosec // G302/G304: user-owned log file, path validated via expandPath
	if err != nil {
		return fmt.Errorf("open log file %q: %w", logPath, err)
	}
	// Explicit: log file stays open across exec boundary — child
	// inherits the fd and writes to it. Parent closes its
	// reference once the child is started.
	defer logFile.Close() //nolint:errcheck // close at end of Start; errors here aren't actionable

	// Self-reexec with the "run" subcommand — the child runs the
	// normal foreground server, inheriting the flag set we were
	// called with. No env-var sentinel needed; the child's argv is
	// `signatory serve run ...`.
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate own executable: %w", err)
	}
	childArgs := []string{"serve", "run"}
	childArgs = append(childArgs, fmt.Sprintf("--port=%d", cmd.Port))
	if cmd.NoTLS {
		childArgs = append(childArgs, "--no-tls")
	} else {
		childArgs = append(childArgs, "--tls-cert="+cmd.TLSCert)
		childArgs = append(childArgs, "--tls-key="+cmd.TLSKey)
	}

	//nolint:gosec // G204: argv-form exec of this same binary; args are flag strings we constructed, not user input
	child := exec.Command(exe, childArgs...)
	child.Stdin = nil
	child.Stdout = logFile
	child.Stderr = logFile
	// Setsid detaches the child from our controlling terminal and
	// process group, so a later `kill -TERM <our-pid>` (or our
	// shell exit) does not cascade to the child.
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := child.Start(); err != nil {
		return fmt.Errorf("spawn detached service: %w", err)
	}
	// Capture the PID BEFORE Release() invalidates the handle.
	// Per os/exec docs: Release "renders [Process] unusable in the
	// future" — including Process.Pid field access, which returns
	// -1 after Release on some platforms. Reading first, releasing
	// second is the correct ordering.
	childPid := child.Process.Pid

	// Disown — don't wait on the child. This process exits after
	// the pidfile write and port probe.
	if err := child.Process.Release(); err != nil {
		// Release failure is benign (GC will clean up) but worth
		// flagging — we don't abort because the service did start.
		fmt.Fprintf(os.Stderr, "# warning: release child process: %v\n", err)
	}

	// Write pidfile BEFORE probing the port. If the probe fails,
	// Status still sees the PID and can report "running, but port
	// didn't come up" — more actionable than a silent orphan.
	if err := writePidFile(cmd.PidPath, childPid); err != nil {
		return fmt.Errorf("write pidfile %q: %w", cmd.PidPath, err)
	}

	// Probe the port to confirm the service is actually accepting
	// connections before we claim success. Without this, a start
	// that fails at cert-load or port-bind would print "started"
	// but leave the caller confused.
	if err := waitForPort(cmd.Port, startProbeSeconds*time.Second); err != nil {
		return fmt.Errorf("service started (PID %d) but port %d did not come up within %ds: %w; check %s",
			childPid, cmd.Port, startProbeSeconds, err, logPath)
	}

	fmt.Printf("signatory serve: started, PID %d, port %d, log %s\n",
		childPid, cmd.Port, logPath)
	return nil
}

// ServeStopCmd shuts down the detached service via its pidfile.
// Sends SIGTERM, polls for graceful exit up to stopGraceSeconds,
// then escalates to SIGKILL. Removes the pidfile on success
// regardless of which signal produced the exit.
//
// Exits 0 whether the service was running (stopped it) or not
// running (no-op) — the post-condition is "service is not
// running," which is satisfied either way.
type ServeStopCmd struct {
	PidPath string `help:"Path to the pidfile." default:"~/.signatory/serve.pid" type:"path"`
}

func (cmd *ServeStopCmd) Run(_ *Globals) error {
	pid, err := readPidFile(cmd.PidPath)
	if errors.Is(err, os.ErrNotExist) {
		fmt.Println("signatory serve: not running (no pidfile)")
		return nil
	}
	if err != nil {
		return fmt.Errorf("read pidfile %q: %w", cmd.PidPath, err)
	}
	if !isProcessAlive(pid) {
		fmt.Printf("signatory serve: pidfile named PID %d but the process is not alive; removing stale pidfile\n", pid)
		_ = os.Remove(cmd.PidPath)
		return nil
	}

	// Graceful shutdown first.
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return fmt.Errorf("send SIGTERM to PID %d: %w", pid, err)
	}
	if waitForExit(pid, stopGraceSeconds*time.Second) {
		_ = os.Remove(cmd.PidPath)
		fmt.Printf("signatory serve: stopped (PID %d)\n", pid)
		return nil
	}

	// Escalate.
	fmt.Fprintf(os.Stderr, "# signatory serve: PID %d did not exit within %ds, sending SIGKILL\n", pid, stopGraceSeconds)
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		return fmt.Errorf("send SIGKILL to PID %d: %w", pid, err)
	}
	if !waitForExit(pid, 2*time.Second) {
		return fmt.Errorf("PID %d still alive after SIGKILL; investigate manually", pid)
	}
	_ = os.Remove(cmd.PidPath)
	fmt.Printf("signatory serve: force-stopped (PID %d)\n", pid)
	return nil
}

// ServeStatusCmd reports whether the detached service is running.
// Resolves the pidfile, verifies the named PID is alive, and prints
// a one-line summary. Exits 0 if running, 1 if not — lets shell
// scripts do `signatory serve status >/dev/null && echo running`.
type ServeStatusCmd struct {
	PidPath string `help:"Path to the pidfile." default:"~/.signatory/serve.pid" type:"path"`
	Port    int    `help:"Port to probe for listen-readiness." default:"21517"`
}

func (cmd *ServeStatusCmd) Run(_ *Globals) error {
	pid, err := readPidFile(cmd.PidPath)
	if errors.Is(err, os.ErrNotExist) {
		fmt.Println("signatory serve: not running (no pidfile)")
		os.Exit(1) //nolint:gocritic // status commands use exit codes to signal state to shell callers
	}
	if err != nil {
		return fmt.Errorf("read pidfile %q: %w", cmd.PidPath, err)
	}
	if !isProcessAlive(pid) {
		fmt.Printf("signatory serve: not running (stale pidfile at %s named PID %d)\n", cmd.PidPath, pid)
		os.Exit(1) //nolint:gocritic // same reason
	}

	// Process is alive. Probe the port to distinguish "running and
	// healthy" from "running but port-bind failed" (the Start
	// escape hatch).
	portHealthy := probePort(cmd.Port, 500*time.Millisecond)
	healthStr := "listening"
	if !portHealthy {
		healthStr = "NOT listening (service alive but port-bind failed?)"
	}
	fmt.Printf("signatory serve: running, PID %d, port %d %s\n", pid, cmd.Port, healthStr)
	return nil
}

// ServeRestartCmd stops the service (if running) then starts it.
// Accepts the same flags as Start so a restart can also change
// port / TLS config. A clean Stop is a pre-condition; an unclean
// Stop (SIGKILL escalation) still proceeds to Start because the
// goal-oriented post-condition is "new instance running."
type ServeRestartCmd struct {
	ServeStartCmd
}

func (cmd *ServeRestartCmd) Run(globals *Globals) error {
	stop := &ServeStopCmd{PidPath: cmd.PidPath}
	if err := stop.Run(globals); err != nil {
		return fmt.Errorf("restart: stop phase: %w", err)
	}
	return cmd.ServeStartCmd.Run(globals)
}

// ServeLogsCmd prints the service log. Default behavior is to
// print the last 50 lines (mimics `tail` default); --follow streams
// new lines as they're written.
//
// Kept deliberately simple: defers to the system `tail` when
// available rather than re-implementing tail's edge cases
// (log-rotation, truncation, encoding quirks) in Go. On systems
// without tail, falls back to a Go-native reader.
type ServeLogsCmd struct {
	LogPath string `help:"Path to the service log file." default:"~/.signatory/logs/serve.log" type:"path"`
	Follow  bool   `help:"Stream new lines as they're written (like tail -f)." short:"f"`
	Lines   int    `help:"Number of trailing lines to print initially." default:"50" short:"n"`
}

func (cmd *ServeLogsCmd) Run(_ *Globals) error {
	logPath, err := expandPath(cmd.LogPath)
	if err != nil {
		return fmt.Errorf("resolve --log-path %q: %w", cmd.LogPath, err)
	}
	if _, err := os.Stat(logPath); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("log file %q does not exist (service may not have started yet)", logPath)
	} else if err != nil {
		return fmt.Errorf("stat log file %q: %w", logPath, err)
	}

	// Prefer system tail for correctness on long-running logs; it
	// handles rotation and truncation properly. Fall back to a
	// simple Go-native reader if tail is unavailable.
	if tailPath, err := exec.LookPath("tail"); err == nil {
		args := []string{"-n", strconv.Itoa(cmd.Lines)}
		if cmd.Follow {
			args = append(args, "-F") // -F follows through rename/truncate
		}
		args = append(args, logPath)
		//nolint:gosec // G204: tailPath is LookPath-resolved to system tail; args are flags + a validated path
		c := exec.Command(tailPath, args...)
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
		return c.Run()
	}

	// Fallback: print the tail in pure Go. Simple, not rotation-
	// aware, but adequate for initial-N-lines output on platforms
	// where tail isn't on PATH.
	return printTailSimple(logPath, cmd.Lines, cmd.Follow)
}

// ---- helpers ----

// readPidFile reads a PID from the given pidfile path and returns
// it. Errors with os.ErrNotExist when the file doesn't exist, so
// callers can distinguish "not running" from "unreadable pidfile"
// via errors.Is.
func readPidFile(path string) (int, error) {
	expanded, err := expandPath(path)
	if err != nil {
		return 0, err
	}
	data, err := os.ReadFile(expanded) //nolint:gosec // G304: pidfile path is operator-supplied via flag with a default; read-only
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(data))
	pid, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("pidfile %q contents %q not a number: %w", expanded, s, err)
	}
	if pid <= 0 {
		return 0, fmt.Errorf("pidfile %q contains non-positive PID %d", expanded, pid)
	}
	return pid, nil
}

// writePidFile atomically writes a PID to the pidfile path. Uses a
// temp-file-plus-rename to avoid a half-written pidfile if the
// writer is interrupted — a partial PID would look like a typo and
// confuse Status / Stop.
func writePidFile(path string, pid int) error {
	expanded, err := expandPath(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(expanded), 0o750); err != nil {
		return fmt.Errorf("create pidfile directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(expanded), filepath.Base(expanded)+".tmp-")
	if err != nil {
		return fmt.Errorf("create pidfile tempfile: %w", err)
	}
	if _, err := fmt.Fprintf(tmp, "%d\n", pid); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("write pidfile contents: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("close pidfile tempfile: %w", err)
	}
	if err := os.Rename(tmp.Name(), expanded); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("rename pidfile tempfile: %w", err)
	}
	return nil
}

// isProcessAlive returns true when the named PID exists and can
// receive signals. Uses signal-0 (the "probe" signal — doesn't
// actually deliver a signal, just checks existence and
// permissions).
//
// Caveat: on systems that recycle PIDs, an unrelated process
// with the same PID could have replaced a dead signatory. The
// pidfile approach is structurally vulnerable to PID recycling;
// a hardened version would also check argv / binary path. Adequate
// for v0.1's single-developer dogfood target.
func isProcessAlive(pid int) bool {
	return syscall.Kill(pid, syscall.Signal(0)) == nil
}

// waitForExit polls isProcessAlive until the named PID is gone or
// the deadline expires. Returns true if the process exited
// within timeout, false otherwise.
func waitForExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !isProcessAlive(pid) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return !isProcessAlive(pid)
}

// waitForPort polls the given port until it accepts a TCP
// connection on 127.0.0.1 or the deadline expires. Returns nil on
// first successful connect, error otherwise. Used by Start to
// confirm the child actually reached "listening" state.
func waitForPort(port int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		lastErr = err
		time.Sleep(100 * time.Millisecond)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("port probe timeout")
	}
	return lastErr
}

// probePort is the single-shot variant of waitForPort used by
// Status — a fast liveness check rather than a startup wait.
func probePort(port int, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// expandPath resolves `~` to $HOME and returns an absolute path.
// Kong's `type:"path"` does some of this, but explicit handling
// keeps helpers self-contained for tests and internal callers
// that didn't go through kong.
func expandPath(p string) (string, error) {
	if strings.HasPrefix(p, "~/") || p == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		if p == "~" {
			return home, nil
		}
		return filepath.Join(home, p[2:]), nil
	}
	return filepath.Abs(p)
}

// printTailSimple reads the last N lines of a file and prints
// them. If follow is true, streams new lines until SIGINT.
// Deliberately minimal — the system `tail` is preferred, this is
// only a fallback.
func printTailSimple(path string, n int, follow bool) error {
	f, err := os.Open(path) //nolint:gosec // G304: operator-supplied path with a sensible default; read-only
	if err != nil {
		return fmt.Errorf("open log %q: %w", path, err)
	}
	defer f.Close() //nolint:errcheck // close on end of command; any error here isn't actionable

	// Seek from end, walk back to find N newlines. For large logs
	// this is more efficient than reading the whole file — but we
	// keep the implementation simple since this path is the
	// fallback.
	const bufSize = 8192
	stat, err := f.Stat()
	if err != nil {
		return err
	}
	size := stat.Size()

	// Read in chunks from the end until we've found n+1 newlines
	// (+1 because the file may end with a newline we don't count).
	var buf []byte
	lineCount := 0
	pos := size
	for pos > 0 && lineCount <= n {
		chunk := int64(bufSize)
		if chunk > pos {
			chunk = pos
		}
		pos -= chunk
		tmp := make([]byte, chunk)
		if _, err := f.ReadAt(tmp, pos); err != nil {
			return fmt.Errorf("read log tail: %w", err)
		}
		buf = append(tmp, buf...)
		lineCount = 0
		for _, b := range buf {
			if b == '\n' {
				lineCount++
			}
		}
	}
	// Trim to exactly last N lines.
	if lineCount > n {
		// Find (lineCount-n)th newline from the start; print after it.
		skip := lineCount - n
		idx := 0
		for i, b := range buf {
			if b == '\n' {
				idx = i + 1
				skip--
				if skip == 0 {
					break
				}
			}
		}
		buf = buf[idx:]
	}
	if _, err := os.Stdout.Write(buf); err != nil {
		return fmt.Errorf("write tail to stdout: %w", err)
	}

	if !follow {
		return nil
	}

	// Follow mode: seek to end of file and stream new bytes.
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	readBuf := make([]byte, bufSize)
	for {
		n, err := f.Read(readBuf)
		if n > 0 {
			if _, werr := os.Stdout.Write(readBuf[:n]); werr != nil {
				return werr
			}
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		time.Sleep(200 * time.Millisecond)
	}
}
