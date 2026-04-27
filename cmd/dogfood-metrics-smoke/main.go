// Command dogfood-metrics-smoke drives an end-to-end test against
// a freshly-built `dogfood-metrics` binary — receiver subprocess
// over real HTTP plus hook subprocess over real stdin.
//
// What it proves (in order):
//
//  1. The binary builds cleanly from cmd/dogfood-metrics/.
//  2. `serve` binds and accepts POST /v1/traces (ungzipped) with
//     202 Accepted, writes to <out-dir>/traces.jsonl.
//  3. `serve` decompresses gzipped bodies (Content-Encoding: gzip)
//     and writes the decompressed body to disk — the round-4
//     verification flagged Claude Code's compression behavior as
//     undocumented, so this path matters.
//  4. `serve` accepts POST /v1/logs and writes to logs.jsonl.
//  5. `serve` returns 400 on malformed JSON without writing.
//  6. `serve` returns 404 on unknown paths and 405 on non-POST.
//  7. The on-disk JSONL files have one valid JSON value per line.
//  8. `hook` subprocess reads JSON from stdin, classifies, writes
//     to hooks-<session_id>.jsonl with the right shape.
//  9. `report` subprocess reads the receiver+hook outputs, joins
//     by session id, writes a markdown report under
//     <out-dir>/sessions/<id>/report.md with the expected
//     sections populated.
//
// Why this exists alongside cmd/dogfood-metrics/*_test.go: the
// unit tests cover the http.Handler in isolation (httptest
// recorder) and runHook in isolation (bytes.Reader). This driver
// proves the binary works end-to-end: real http.ListenAndServe,
// real subprocess, real stdin pipe, real file writes from a
// process that isn't the test runner. Was previously a series of
// curl/echo bash invocations in the slice 1+2 verification; per
// the project's "tests are permanent fixtures, not temporary
// scripts" rule, the verification belongs in code that stays
// useful across sessions.
//
// Usage (from repo root):
//
//	go run ./cmd/dogfood-metrics-smoke
//
// Exit codes: 0 on all assertions passing, 1 on first failure, 2
// on setup error (can't build, can't bind, etc.).
package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Sample OTLP-JSON bodies — the minimum shape the receiver
// decodes. Mirrors cmd/dogfood-metrics/receiver_test.go's
// sampleTracesBody / sampleLogsBody so the same wire shape is
// exercised in both the unit tests and this end-to-end driver.
const (
	sampleTracesBody = `{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"claude-code"}},{"key":"session.id","value":{"stringValue":"smoke-sess"}}]},"scopeSpans":[{"spans":[{"name":"claude_code.tool","spanId":"0102030405060708","traceId":"0102030405060708090a0b0c0d0e0f10","startTimeUnixNano":"1714250000000000000","endTimeUnixNano":"1714250001000000000","attributes":[{"key":"tool.name","value":{"stringValue":"Bash"}}]}]}]}]}`
	sampleLogsBody   = `{"resourceLogs":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"claude-code"}}]},"scopeLogs":[{"logRecords":[{"timeUnixNano":"1714250000000000000","body":{"stringValue":"test"}}]}]}]}`
	sampleMalformed  = `{"unterminated":`
	sampleHookInput  = `{"session_id":"smoke-sess","cwd":"/Users/sarah/git/signatory","tool_name":"Bash","tool_input":{"command":"curl https://example.com"},"tool_use_id":"tu-smoke-1"}`
)

// httpTimeout bounds how long any single HTTP request waits.
// Receiver responses for these tiny bodies should complete in
// milliseconds; anything longer indicates a hang.
const httpTimeout = 5 * time.Second

// bindWaitTimeout bounds how long we wait for the receiver to
// start listening after `cmd.Start()` returns.
const bindWaitTimeout = 10 * time.Second

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "dogfood-metrics-smoke: %v\n", err)
		os.Exit(2)
	}
}

func run() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tmp, err := os.MkdirTemp("", "dogfood-smoke-*")
	if err != nil {
		return fmt.Errorf("mktemp: %w", err)
	}
	defer os.RemoveAll(tmp) //nolint:errcheck // best-effort cleanup; tempdir leaks are not actionable

	binPath := filepath.Join(tmp, "dogfood-metrics-bin")
	outDir := filepath.Join(tmp, "raw")
	sessionsDir := filepath.Join(tmp, "sessions")

	if err := buildTarget(ctx, binPath); err != nil {
		return fmt.Errorf("build: %w", err)
	}

	port, err := pickFreePort()
	if err != nil {
		return fmt.Errorf("pick port: %w", err)
	}

	rcv, err := startReceiver(ctx, binPath, port, outDir)
	if err != nil {
		return fmt.Errorf("start receiver: %w", err)
	}
	defer rcv.Stop()

	if err := waitForBind(port, bindWaitTimeout); err != nil {
		return fmt.Errorf("wait for receiver bind: %w (stderr: %s)", err, rcv.StderrSnapshot())
	}

	rep := &reporter{}

	// === Receiver checks ===

	rep.assertEqual("receiver: ungzipped POST /v1/traces → 202",
		http.StatusAccepted, postBody(port, "/v1/traces", []byte(sampleTracesBody), false))
	rep.assertEqual("receiver: gzipped POST /v1/traces → 202",
		http.StatusAccepted, postBody(port, "/v1/traces", []byte(sampleTracesBody), true))
	rep.assertEqual("receiver: POST /v1/logs → 202",
		http.StatusAccepted, postBody(port, "/v1/logs", []byte(sampleLogsBody), false))
	rep.assertEqual("receiver: malformed JSON → 400",
		http.StatusBadRequest, postBody(port, "/v1/traces", []byte(sampleMalformed), false))
	rep.assertEqual("receiver: unknown path /v1/metrics → 404",
		http.StatusNotFound, postBody(port, "/v1/metrics", []byte("{}"), false))
	rep.assertEqual("receiver: GET /v1/traces → 405",
		http.StatusMethodNotAllowed, getRequest(port, "/v1/traces"))

	// === On-disk file checks ===

	tracesContents, err := os.ReadFile(filepath.Join(outDir, "traces.jsonl")) //nolint:gosec // G304: outDir is mktemp-derived, not user input
	if err != nil {
		rep.fail("read traces.jsonl: " + err.Error())
	} else {
		lines := splitLines(string(tracesContents))
		rep.assertEqual("traces.jsonl: line count after 2 successful POSTs", 2, len(lines))
		for i, line := range lines {
			var m map[string]any
			rep.assertTrue(fmt.Sprintf("traces.jsonl: line %d valid JSON", i),
				json.Unmarshal([]byte(line), &m) == nil)
			_, hasResourceSpans := m["resourceSpans"]
			rep.assertTrue(fmt.Sprintf("traces.jsonl: line %d has resourceSpans", i), hasResourceSpans)
		}
	}

	logsContents, err := os.ReadFile(filepath.Join(outDir, "logs.jsonl")) //nolint:gosec // G304: outDir is mktemp-derived, not user input
	if err != nil {
		rep.fail("read logs.jsonl: " + err.Error())
	} else {
		lines := splitLines(string(logsContents))
		rep.assertEqual("logs.jsonl: line count after 1 successful POST", 1, len(lines))
	}

	// === Hook subcommand checks ===

	if err := runHookSubprocess(ctx, binPath, sampleHookInput, outDir); err != nil {
		rep.fail("hook subprocess: " + err.Error())
	} else {
		hookContents, err := os.ReadFile(filepath.Join(outDir, "hooks-smoke-sess.jsonl")) //nolint:gosec // G304: outDir is mktemp-derived
		if err != nil {
			rep.fail("read hooks-smoke-sess.jsonl: " + err.Error())
		} else {
			var ev map[string]any
			line := strings.TrimRight(string(hookContents), "\n")
			if err := json.Unmarshal([]byte(line), &ev); err != nil {
				rep.fail("hook event line not valid JSON: " + err.Error())
			} else {
				rep.assertEqual("hook event: classification", "external_curl", ev["classification"])
				rep.assertEqual("hook event: tool_name", "Bash", ev["tool_name"])
				rep.assertEqual("hook event: tool_use_id", "tu-smoke-1", ev["tool_use_id"])
				rep.assertEqual("hook event: session_id", "smoke-sess", ev["session_id"])
				rep.assertEqual("hook event: event tag", "PreToolUse", ev["event"])
			}
		}
	}

	// === Report subcommand checks ===

	if err := runReportSubprocess(ctx, binPath, "smoke-sess", outDir, sessionsDir); err != nil {
		rep.fail("report subprocess: " + err.Error())
	} else {
		reportPath := filepath.Join(sessionsDir, "smoke-sess", "report.md")
		reportContents, err := os.ReadFile(reportPath) //nolint:gosec // G304: sessionsDir is mktemp-derived
		if err != nil {
			rep.fail("read report.md: " + err.Error())
		} else {
			report := string(reportContents)
			// Section headers — stable contract per
			// design/agent-otel.md slice 3 plan.
			rep.assertTrue("report: top-level header",
				strings.Contains(report, "# Dogfood report — session smoke-sess"))
			rep.assertTrue("report: Subagent activity section",
				strings.Contains(report, "## Subagent activity"))
			rep.assertTrue("report: Tool-call classification section",
				strings.Contains(report, "## Tool-call classification"))
			rep.assertTrue("report: External calls section",
				strings.Contains(report, "## External calls (cache-miss candidates)"))
			rep.assertTrue("report: Source reads section",
				strings.Contains(report, "## Source reads (underspecification candidates)"))
			// The hook event we sent (curl ...) should land as an
			// external_curl entry under "External calls."
			rep.assertTrue("report: external_curl entry",
				strings.Contains(report, "curl https://example.com"))
			// The trace POST we sent included session.id=smoke-sess
			// but no query_source attribute, so the subagent table
			// should show the "(unknown)" bucket — confirms the
			// trace stream is being read at all.
			rep.assertTrue("report: subagent (unknown) row",
				strings.Contains(report, "| (unknown) |"))
		}
	}

	return rep.finish(rcv)
}

// buildTarget compiles cmd/dogfood-metrics into binPath.
func buildTarget(ctx context.Context, binPath string) error {
	fmt.Println("[build] compiling dogfood-metrics")
	// binPath comes from os.MkdirTemp — not user input. gosec's
	// taint analysis can't see that, so we annotate.
	cmd := exec.CommandContext(ctx, "go", "build", //nolint:gosec // G204: args are const and tempdir-derived, not user input
		"-o", binPath,
		"./cmd/dogfood-metrics",
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// pickFreePort asks the OS for an unused TCP port via :0 binding,
// then closes the listener and returns the chosen port. Race
// condition: another process could grab the port between our
// close and the receiver's bind. Acceptable for a dev tool.
func pickFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		return 0, err
	}
	return port, nil
}

// receiver wraps the running `dogfood-metrics serve` subprocess.
type receiver struct {
	cmd       *exec.Cmd
	stderrBuf *safeBuffer
}

// startReceiver spawns the binary in `serve` mode bound to the
// given port and out-dir. Stderr is captured so we can echo it on
// failure (server-side diagnostics often go there).
func startReceiver(ctx context.Context, binPath string, port int, outDir string) (*receiver, error) {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	fmt.Printf("[run] spawning receiver: %s serve --addr %s --out-dir %s\n", binPath, addr, outDir)
	// binPath, addr, outDir are all tempdir-derived, not user input.
	cmd := exec.CommandContext(ctx, binPath, "serve", "--addr", addr, "--out-dir", outDir) //nolint:gosec // G204: all paths are tempdir-derived
	stderrBuf := &safeBuffer{}
	cmd.Stderr = stderrBuf
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &receiver{cmd: cmd, stderrBuf: stderrBuf}, nil
}

// Stop sends SIGTERM and waits for the receiver to exit. Best-
// effort — we don't fail the smoke run if shutdown hangs.
func (r *receiver) Stop() {
	if r.cmd.Process != nil {
		_ = r.cmd.Process.Signal(os.Interrupt)
	}
	_ = r.cmd.Wait()
}

// StderrSnapshot returns the receiver's stderr so far for use in
// failure diagnostics.
func (r *receiver) StderrSnapshot() string { return r.stderrBuf.String() }

// waitForBind polls the receiver's listen address until a TCP
// dial succeeds or the deadline expires.
func waitForBind(port int, timeout time.Duration) error {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("receiver did not bind on %s within %s", addr, timeout)
}

// postBody POSTs body to /<path> on the receiver and returns the
// response status code. When gz is true, the body is gzip-encoded
// and Content-Encoding: gzip is set — the round-4-verification
// path the receiver must handle.
func postBody(port int, path string, body []byte, gz bool) int {
	url := fmt.Sprintf("http://127.0.0.1:%d%s", port, path)
	var reqBody io.Reader = bytes.NewReader(body)
	if gz {
		var buf bytes.Buffer
		w := gzip.NewWriter(&buf)
		if _, err := w.Write(body); err != nil {
			return -1
		}
		if err := w.Close(); err != nil {
			return -1
		}
		reqBody = &buf
	}
	req, err := http.NewRequest(http.MethodPost, url, reqBody)
	if err != nil {
		return -1
	}
	req.Header.Set("Content-Type", "application/json")
	if gz {
		req.Header.Set("Content-Encoding", "gzip")
	}
	client := &http.Client{Timeout: httpTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return -1
	}
	defer resp.Body.Close() //nolint:errcheck // close after read; err not actionable for status-only check
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// getRequest issues a GET to /<path> and returns the response
// status code. Used to confirm the receiver's 405 path.
func getRequest(port int, path string) int {
	url := fmt.Sprintf("http://127.0.0.1:%d%s", port, path)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return -1
	}
	client := &http.Client{Timeout: httpTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return -1
	}
	defer resp.Body.Close() //nolint:errcheck
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// runHookSubprocess execs the binary's `hook` subcommand with the
// given JSON piped to stdin and verifies it exits 0. The hook's
// side effect (writing to outDir) is checked by the caller after
// this returns.
func runHookSubprocess(ctx context.Context, binPath, jsonInput, outDir string) error {
	// binPath and outDir are tempdir-derived; jsonInput is a const.
	cmd := exec.CommandContext(ctx, binPath, "hook", "--event", "PreToolUse", "--out-dir", outDir) //nolint:gosec // G204: all paths are tempdir-derived
	cmd.Stdin = strings.NewReader(jsonInput)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// runReportSubprocess execs the binary's `report` subcommand and
// verifies it exits 0. The rendered markdown lands at
// <sessionsDir>/<sessionID>/report.md, which the caller reads to
// assert section structure.
func runReportSubprocess(ctx context.Context, binPath, sessionID, inDir, sessionsDir string) error {
	// binPath, inDir, sessionsDir all tempdir-derived; sessionID is
	// a const for the smoke run.
	// flag.Parse stops at the first non-flag argument, so flags
	// must precede the positional session-id arg.
	cmd := exec.CommandContext(ctx, binPath, "report", "--in-dir", inDir, "--out-dir", sessionsDir, sessionID) //nolint:gosec // G204: all paths are tempdir-derived
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// splitLines splits a JSONL blob into trimmed lines. Empty
// trailing line (from terminal newline) is dropped.
func splitLines(s string) []string {
	out := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(out) == 1 && out[0] == "" {
		return nil
	}
	return out
}

// reporter mirrors smoke-mcp's pattern: track pass/fail,
// keep going past first fail so the developer sees the full
// picture, return non-nil error if any assertion failed.
type reporter struct {
	passes int
	fails  []string
}

func (r *reporter) assertEqual(label string, want, got any) {
	if fmt.Sprintf("%v", want) == fmt.Sprintf("%v", got) {
		r.pass(label)
		return
	}
	r.fail(fmt.Sprintf("%s (want=%v got=%v)", label, want, got))
}

func (r *reporter) assertTrue(label string, cond bool) {
	if cond {
		r.pass(label)
		return
	}
	r.fail(label)
}

func (r *reporter) pass(label string) {
	r.passes++
	fmt.Printf("[PASS %d] %s\n", r.passes+len(r.fails), label)
}

func (r *reporter) fail(label string) {
	r.fails = append(r.fails, label)
	fmt.Printf("[FAIL] %s\n", label)
}

// finish prints a summary and returns a non-nil error iff any
// assertion failed. The receiver's stderr is echoed on failure.
func (r *reporter) finish(rcv *receiver) error {
	total := r.passes + len(r.fails)
	fmt.Println()
	if len(r.fails) > 0 {
		fmt.Printf("%d/%d assertions failed.\n", len(r.fails), total)
		if stderr := rcv.StderrSnapshot(); stderr != "" {
			fmt.Fprintln(os.Stderr, "--- receiver stderr: ---")
			fmt.Fprintln(os.Stderr, stderr)
		}
		return fmt.Errorf("smoke test failed")
	}
	fmt.Printf("All %d assertions passed.\n", total)
	return nil
}

// safeBuffer is an io.Writer backed by a byte slice that
// serializes concurrent writes. exec.Cmd writes to stderr from
// its own goroutine; we read at failure time and need consistent
// reads. (Same pattern as cmd/smoke-mcp.)
type safeBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}
