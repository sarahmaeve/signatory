package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// Filenames within the out-dir for daemon-mode bookkeeping.
// Both gitignored as part of dogfood-metrics/raw/.
const (
	pidFilename = ".receiver.pid"
	logFilename = ".receiver.log"
)

// daemonStartTimeout bounds how long `start` waits for the
// child receiver to bind its listening socket. If the receiver
// doesn't bind in time (port already in use, build failure,
// crash on startup), `start` reports failure rather than
// silently succeeding with a dead child.
const daemonStartTimeout = 5 * time.Second

// daemonStopTimeout bounds how long `stop` waits for the
// receiver to exit after SIGTERM before escalating to SIGKILL.
const daemonStopTimeout = 5 * time.Second

// runStartCmd is the entry point for the `start` subcommand.
// Spawns the current binary in `serve` mode as a detached
// subprocess (Setpgid: true so it survives parent exit),
// redirects its output to <out-dir>/.receiver.log, writes its
// PID to <out-dir>/.receiver.pid, waits for bind, exits.
func runStartCmd(args []string) {
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	addr := fs.String("addr", ":4318", "HTTP listen address for the receiver")
	outDir := fs.String("out-dir", "dogfood-metrics/raw", "directory for raw OTLP-JSON files (also holds .receiver.pid + .receiver.log)")
	if err := fs.Parse(args); err != nil {
		log.Fatalf("parse flags: %v", err)
	}
	if err := startDaemon(*addr, *outDir); err != nil {
		log.Fatalf("start: %v", err)
	}
}

// runStopCmd is the entry point for the `stop` subcommand.
// Reads the PID file, sends SIGTERM, waits for exit, removes
// the PID file. Surfaces clear errors for the not-running and
// stale-PID-file cases.
func runStopCmd(args []string) {
	fs := flag.NewFlagSet("stop", flag.ExitOnError)
	outDir := fs.String("out-dir", "dogfood-metrics/raw", "directory holding .receiver.pid")
	if err := fs.Parse(args); err != nil {
		log.Fatalf("parse flags: %v", err)
	}
	if err := stopDaemon(*outDir); err != nil {
		log.Fatalf("stop: %v", err)
	}
}

// runRestartCmd is the entry point for the `restart` subcommand.
// Stops any running receiver (best-effort — not-running is fine)
// then starts fresh. Brief pause after stop to let the port
// release before re-bind.
func runRestartCmd(args []string) {
	fs := flag.NewFlagSet("restart", flag.ExitOnError)
	addr := fs.String("addr", ":4318", "HTTP listen address for the receiver")
	outDir := fs.String("out-dir", "dogfood-metrics/raw", "directory for raw OTLP-JSON files")
	if err := fs.Parse(args); err != nil {
		log.Fatalf("parse flags: %v", err)
	}
	// Best-effort stop — the not-running case isn't fatal.
	if err := stopDaemon(*outDir); err != nil && !errors.Is(err, errNotRunning) {
		log.Fatalf("restart: stop phase: %v", err)
	}
	// Brief wait for the port to release. Without this,
	// re-bind on the same port can fail with EADDRINUSE for a
	// few hundred ms even after the previous receiver exits.
	time.Sleep(500 * time.Millisecond)
	if err := startDaemon(*addr, *outDir); err != nil {
		log.Fatalf("restart: start phase: %v", err)
	}
}

// errNotRunning is returned by stopDaemon when no receiver is
// running. Callers (notably restart) compare with errors.Is to
// distinguish "we have nothing to stop" from real failures.
var errNotRunning = errors.New("no receiver running")

// startDaemon spawns the receiver subprocess and waits for it
// to bind. Pre-flight: refuses to start if a live receiver is
// already running per the PID file.
func startDaemon(addr, outDir string) error {
	if err := os.MkdirAll(outDir, 0o750); err != nil {
		return fmt.Errorf("mkdir %s: %w", outDir, err)
	}

	pidPath := filepath.Join(outDir, pidFilename)
	if pid, ok := readPIDFile(pidPath); ok && isProcessAlive(pid) {
		return fmt.Errorf("receiver already running with PID %d (PID file: %s)", pid, pidPath)
	}

	logPath := filepath.Join(outDir, logFilename)
	logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644) //nolint:gosec // G304: logPath constructed from caller-supplied outDir
	if err != nil {
		return fmt.Errorf("open log file %s: %w", logPath, err)
	}
	defer logFile.Close() //nolint:errcheck // close after exec.Cmd inherits the FD; err not actionable

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate own binary: %w", err)
	}

	// self comes from os.Executable; addr/outDir are caller args.
	// All are operator-controlled, not attacker-controlled at this
	// boundary.
	cmd := exec.Command(self, "serve", "--addr", addr, "--out-dir", outDir) //nolint:gosec // G204: self/addr/outDir are operator-controlled
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// Setpgid: true gives the child its own process group, so it
	// survives when our short-lived `start` process exits. Without
	// this, the child would inherit our pgid and likely die when
	// we exit.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn receiver: %w", err)
	}

	// Wait for the receiver to bind. If it fails (port in use,
	// crash on startup), kill the child and report.
	port, err := portFromAddr(addr)
	if err != nil {
		// Can't validate bind without a port — write the PID
		// anyway and trust the caller to inspect the log.
		return writePIDFileFor(pidPath, cmd.Process.Pid)
	}
	if err := waitForReceiverBind(port, daemonStartTimeout); err != nil {
		// Don't leak a half-spawned child.
		_ = cmd.Process.Kill()
		return fmt.Errorf("receiver did not bind within %s (check %s for details)", daemonStartTimeout, logPath)
	}

	// Capture PID BEFORE Release — Release invalidates the
	// Process struct and Pid returns -1 thereafter.
	pid := cmd.Process.Pid

	if err := writePIDFileFor(pidPath, pid); err != nil {
		// Bound and serving, but PID file write failed. Leave
		// the receiver running rather than killing it — the user
		// can still find it via lsof/ps.
		fmt.Fprintf(os.Stderr, "warning: receiver started (PID %d) but PID file write failed: %v\n", pid, err)
	}

	// Release the child to the OS — don't Wait on it (would
	// block until child exits, defeating the purpose of detach).
	if err := cmd.Process.Release(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: process release failed: %v\n", err)
	}

	fmt.Printf("receiver started: PID %d, listening on %s, logging to %s\n",
		pid, addr, logPath)
	return nil
}

// stopDaemon reads the PID file, sends SIGTERM, waits for exit
// (escalating to SIGKILL on timeout), removes the PID file.
func stopDaemon(outDir string) error {
	pidPath := filepath.Join(outDir, pidFilename)
	pid, ok := readPIDFile(pidPath)
	if !ok {
		return errNotRunning
	}
	if !isProcessAlive(pid) {
		// PID file is stale — clean it up so the next `start`
		// works without a confusing "already running" error.
		_ = os.Remove(pidPath)
		return fmt.Errorf("%w (PID file at %s referenced dead process %d; cleaned up)",
			errNotRunning, pidPath, pid)
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find process %d: %w", pid, err)
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("SIGTERM PID %d: %w", pid, err)
	}

	// Poll for exit, escalate to SIGKILL on timeout.
	deadline := time.Now().Add(daemonStopTimeout)
	for time.Now().Before(deadline) {
		if !isProcessAlive(pid) {
			_ = os.Remove(pidPath)
			fmt.Printf("receiver stopped (PID %d)\n", pid)
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Hard kill — receiver didn't honor SIGTERM in time.
	_ = proc.Signal(syscall.SIGKILL)
	_ = os.Remove(pidPath)
	fmt.Fprintf(os.Stderr, "receiver did not exit within %s; sent SIGKILL\n", daemonStopTimeout)
	return nil
}

// readPIDFile reads and parses the PID from path. Returns
// (0, false) when the file is missing or unparseable — both
// treated as "no recorded daemon."
func readPIDFile(path string) (int, bool) {
	b, err := os.ReadFile(path) //nolint:gosec // G304: path constructed from caller-supplied outDir
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(string(trimPIDBytes(b)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

// trimPIDBytes strips the trailing newline that writePIDFileFor
// emits, plus any incidental whitespace.
func trimPIDBytes(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r' || b[len(b)-1] == ' ' || b[len(b)-1] == '\t') {
		b = b[:len(b)-1]
	}
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\t') {
		b = b[1:]
	}
	return b
}

// writePIDFileFor writes pid (followed by a newline, the
// convention) to path with 0644 permissions.
func writePIDFileFor(path string, pid int) error {
	if err := os.WriteFile(path, []byte(strconv.Itoa(pid)+"\n"), 0o644); err != nil { //nolint:gosec // G306: PID file is dev tooling, not a secret
		return fmt.Errorf("write PID file %s: %w", path, err)
	}
	return nil
}

// isProcessAlive returns true iff a process with the given PID
// exists and is reachable. Uses signal 0 — POSIX's "no-op
// signal" used precisely for this existence check. ESRCH means
// the process is gone; EPERM means the process exists but we
// can't signal it (also counts as alive for our purposes).
func isProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	if errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH) {
		return false
	}
	// Other errors (notably EPERM) mean the process exists but
	// we can't signal it. Treat as alive.
	return true
}

// portFromAddr extracts the port number from a `host:port` or
// `:port` address string. Returns -1 + error when no port is
// resolvable.
func portFromAddr(addr string) (int, error) {
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return -1, err
	}
	return strconv.Atoi(portStr)
}

// waitForReceiverBind polls 127.0.0.1:port until a TCP dial
// succeeds or timeout expires. Used by `start` to verify the
// child receiver actually came up before we report success.
func waitForReceiverBind(port int, timeout time.Duration) error {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("did not bind on %s within %s", addr, timeout)
}
