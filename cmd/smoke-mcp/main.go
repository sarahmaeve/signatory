// Command smoke-mcp drives an end-to-end MCP 2025-11-25 session against
// a freshly-built `signatory mcp` binary over stdio.
//
// What it proves (in order):
//
//  1. initialize round-trips: the server returns our protocolVersion
//     and the serverInfo.version equals the string we injected via
//     -ldflags "-X main.version=...". This is the proof that the
//     ServerVersion package-var removal works end-to-end — the value
//     threads from ldflags → main.version → NewServer(version) →
//     handshake.version → response.
//
//  2. tools/list and resources/list expose the full Phase 1 surface
//     (7 tools, 6 resources).
//
//  3. Every envelope returned by a tool or resource handler has
//     metadata.server_version stamped by the dispatch layer (the
//     point of the refactor).
//
//  4. signatory://config's data.mcp_version reflects the Version field
//     injected into ConfigResource at construction.
//
//  5. signatory://posture against an empty DB returns a valid envelope
//     with total=0 — proves the rows.Close errcheck cleanup and the
//     FK-safe empty-store path are intact.
//
// Why this exists alongside the Go _test.go suites: the unit tests
// cover each piece in isolation. This driver proves they integrate —
// one process, real stdin/stdout, real JSON-RPC framing, real ldflags
// plumbing. Was previously a bash script; rewritten in Go because bash
// 3.x on macOS lacks `coproc` and bash quoting is its own hazard.
//
// Usage (from repo root):
//
//	go run ./cmd/smoke-mcp
//
// Exit codes: 0 on all assertions passing, 1 on first failure, 2 on
// setup error (can't build, can't spawn, etc.).
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// targetVersion is the version string we inject via ldflags into the
// test binary. It must be unusual enough to prove it came from us and
// not from a coincidental default (e.g., avoid "dev" or "0.1.0").
const targetVersion = "0.1.0-smoke"

// frameTimeout bounds how long we wait for any single response. The
// server handlers are all in-memory for Phase 1 — anything that takes
// longer than this indicates a hang, not a slow query.
const frameTimeout = 5 * time.Second

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "smoke-mcp: %v\n", err)
		os.Exit(2)
	}
}

func run() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tmp, err := os.MkdirTemp("", "smoke-mcp-*")
	if err != nil {
		return fmt.Errorf("mktemp: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	binPath := filepath.Join(tmp, "signatory")
	dbPath := filepath.Join(tmp, "signatory.db")

	if err := buildTarget(ctx, binPath); err != nil {
		return fmt.Errorf("build target: %w", err)
	}

	session, err := startSession(ctx, binPath, dbPath)
	if err != nil {
		return fmt.Errorf("start session: %w", err)
	}
	defer session.Close()

	rep := &reporter{}

	// 1. initialize — spec-compliant: we send it first and wait for
	//    the response before any other frame.
	initResult, err := session.request(1, "initialize", map[string]any{
		"protocolVersion": "2025-11-25",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "smoke-mcp", "version": "0.0.0"},
	})
	if err != nil {
		rep.fail("initialize: transport error: " + err.Error())
		return rep.finish(session)
	}

	var initDecoded struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
		Instructions string `json:"instructions"`
	}
	if err := json.Unmarshal(initResult, &initDecoded); err != nil {
		rep.fail("initialize: decode result: " + err.Error())
		return rep.finish(session)
	}

	rep.assertEqual("initialize: protocolVersion",
		"2025-11-25", initDecoded.ProtocolVersion)
	rep.assertEqual("initialize: serverInfo.name",
		"signatory", initDecoded.ServerInfo.Name)
	// The core ldflags plumbing assertion: serverInfo.version must
	// equal the string we injected at build time, not "dev" or some
	// leftover default.
	rep.assertEqual("initialize: serverInfo.version matches ldflags",
		targetVersion, initDecoded.ServerInfo.Version)
	// Instructions is the server-level routing nudge that rides in
	// every handshake's response. Non-empty is the dogfood-driven
	// invariant: an empty instructions field was the root cause of
	// the first-session "why did Claude Code reach for grep?" finding.
	rep.assertTrue("initialize: instructions is non-empty",
		initDecoded.Instructions != "")
	// Sanity anchor — the text should mention the project name and
	// point at the help resource. If this fails the serverInstructions
	// constant has drifted into something unrecognisable.
	rep.assertTrue("initialize: instructions mentions signatory",
		containsAll(initDecoded.Instructions, "signatory", "signatory://help"))

	// 2. notifications/initialized — no response expected. Sending it
	//    now advances the server from stateInitialized to
	//    stateOperational so subsequent requests are accepted.
	if err := session.notify("notifications/initialized", nil); err != nil {
		rep.fail("notifications/initialized: send: " + err.Error())
		return rep.finish(session)
	}

	// 3. tools/list — every Phase 1 tool must be present.
	toolsResult, err := session.request(2, "tools/list", nil)
	if err != nil {
		rep.fail("tools/list: " + err.Error())
		return rep.finish(session)
	}
	var tools struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(toolsResult, &tools); err != nil {
		rep.fail("tools/list: decode: " + err.Error())
		return rep.finish(session)
	}
	toolSet := map[string]bool{}
	for _, t := range tools.Tools {
		toolSet[t.Name] = true
	}
	for _, want := range []string{
		"signatory_analyze",
		"signatory_detail",
		"signatory_signals",
		"signatory_show_analyses",
		"signatory_show_findings",
		"signatory_show_methodology",
		"signatory_survey",
	} {
		rep.assertTrue("tools/list includes "+want, toolSet[want])
	}

	// 4. resources/list — every Phase 1 resource must be present.
	resResult, err := session.request(3, "resources/list", nil)
	if err != nil {
		rep.fail("resources/list: " + err.Error())
		return rep.finish(session)
	}
	var resList struct {
		Resources []struct {
			URI string `json:"uri"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(resResult, &resList); err != nil {
		rep.fail("resources/list: decode: " + err.Error())
		return rep.finish(session)
	}
	resSet := map[string]bool{}
	for _, r := range resList.Resources {
		resSet[r.URI] = true
	}
	for _, want := range []string{
		"signatory://help",
		"signatory://config",
		"signatory://signal-types",
		"signatory://burns",
		"signatory://posture",
		"signatory://unexamined",
		"signatory://analyses",
	} {
		rep.assertTrue("resources/list includes "+want, resSet[want])
	}

	// 5. resources/read signatory://help — orientation guide must be
	//    present and contain recognisable anchors. This is the deeper
	//    companion to the initialize-time Instructions; without it,
	//    an LLM that wants more context than instructions gave has
	//    nowhere to go. Empty or missing = dogfood regression.
	helpEnvelope, err := session.readResource(4, "signatory://help")
	if err != nil {
		rep.fail("help read: " + err.Error())
		return rep.finish(session)
	}
	rep.assertEqual("help: envelope status", "ok", helpEnvelope.Status)
	var helpData struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(helpEnvelope.Data, &helpData); err != nil {
		rep.fail("help: decode data: " + err.Error())
		return rep.finish(session)
	}
	rep.assertTrue("help: content is non-empty", helpData.Content != "")
	rep.assertTrue("help: content mentions analyze/show_findings/posture",
		containsAll(helpData.Content,
			"signatory_analyze", "signatory_show_findings", "signatory://posture"))

	// 6. resources/read signatory://config — the refactor-critical
	//    assertion: the dispatch layer stamped metadata.server_version
	//    AND the ConfigResource's injected Version field surfaces as
	//    data.mcp_version.
	cfgEnvelope, err := session.readResource(5, "signatory://config")
	if err != nil {
		rep.fail("config read: " + err.Error())
		return rep.finish(session)
	}
	rep.assertEqual("config: envelope status", "ok", cfgEnvelope.Status)
	rep.assertEqual("config: metadata.server_version stamped by dispatch",
		targetVersion, cfgEnvelope.Metadata.ServerVersion)

	var cfgData struct {
		MCPVersion string `json:"mcp_version"`
		Transport  string `json:"transport"`
	}
	if err := json.Unmarshal(cfgEnvelope.Data, &cfgData); err != nil {
		rep.fail("config: decode data: " + err.Error())
		return rep.finish(session)
	}
	rep.assertEqual("config: data.mcp_version reflects injected Version field",
		targetVersion, cfgData.MCPVersion)
	rep.assertEqual("config: data.transport", "stdio", cfgData.Transport)

	// 7. resources/read signatory://posture — empty-DB handler path.
	//    Proves the rows cursor cleanup and empty-store envelope shape
	//    are still intact, and that dispatch stamps the version on a
	//    handler that touches the real database.
	posEnvelope, err := session.readResource(6, "signatory://posture")
	if err != nil {
		rep.fail("posture read: " + err.Error())
		return rep.finish(session)
	}
	rep.assertEqual("posture: envelope status on empty store",
		"ok", posEnvelope.Status)
	rep.assertEqual("posture: metadata.server_version stamped by dispatch",
		targetVersion, posEnvelope.Metadata.ServerVersion)

	var posData struct {
		Total int `json:"total"`
	}
	if err := json.Unmarshal(posEnvelope.Data, &posData); err != nil {
		rep.fail("posture: decode data: " + err.Error())
		return rep.finish(session)
	}
	rep.assertEqual("posture: total=0 on empty store", 0, posData.Total)

	return rep.finish(session)
}

// buildTarget compiles cmd/signatory into binPath with a known version
// injected via ldflags. We need the version to be an unusual string so
// we can prove it round-tripped from ldflags to the wire, not picked
// up by accident from some default.
func buildTarget(ctx context.Context, binPath string) error {
	fmt.Println("[build] compiling signatory with -X main.version=" + targetVersion)
	// targetVersion is a package-level const and binPath comes from
	// os.MkdirTemp — neither is user input. gosec's taint analysis
	// can't see that, so we annotate.
	cmd := exec.CommandContext(ctx, "go", "build", //nolint:gosec // G204: args are const and tempdir-derived, not user input
		"-ldflags", "-X main.version="+targetVersion+" -X main.commit=smoke",
		"-o", binPath,
		"./cmd/signatory",
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// session is a running signatory mcp subprocess with pipes for
// bidirectional JSON-RPC traffic.
type session struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stdout    *bufio.Reader
	stderrBuf *safeBuffer
	nextErr   error
}

// startSession launches the target binary with stdin/stdout piped.
// The server's stderr is captured into a buffer so we can surface it
// on failure (diagnostic messages go there per the mcp.go design —
// stdout is reserved for protocol traffic).
func startSession(ctx context.Context, binPath, dbPath string) (*session, error) {
	fmt.Println("[run] spawning server: " + binPath + " --db " + dbPath + " mcp")
	// binPath and dbPath both come from os.MkdirTemp in this same
	// process — neither is user input. gosec flags any variable-path
	// exec as G204; rationale inline matches the house pattern.
	cmd := exec.CommandContext(ctx, binPath, "--db", dbPath, "mcp") //nolint:gosec // G204: both paths are tempdir-derived, not user input

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderrBuf := &safeBuffer{}
	cmd.Stderr = stderrBuf

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start: %w", err)
	}

	return &session{
		cmd:       cmd,
		stdin:     stdin,
		stdout:    bufio.NewReader(stdout),
		stderrBuf: stderrBuf,
	}, nil
}

// request sends a JSON-RPC request and returns its .result as raw JSON.
// It returns an error if the response carries an .error object, if the
// id doesn't match, or if the read times out.
func (s *session) request(id int, method string, params any) (json.RawMessage, error) {
	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
	}
	if params != nil {
		req["params"] = params
	}
	if err := s.writeFrame(req); err != nil {
		return nil, fmt.Errorf("write %s: %w", method, err)
	}
	resp, err := s.readFrame()
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", method, err)
	}
	var envelope struct {
		ID     json.RawMessage `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(resp, &envelope); err != nil {
		return nil, fmt.Errorf("decode response: %w (raw: %s)", err, string(resp))
	}
	if envelope.Error != nil {
		return nil, fmt.Errorf("server error: code=%d msg=%s",
			envelope.Error.Code, envelope.Error.Message)
	}
	// We send requests one at a time and wait for each response, so
	// id mismatch would indicate a serious server bug. Verify anyway —
	// the cost is one Unmarshal we were already doing.
	var gotID int
	if err := json.Unmarshal(envelope.ID, &gotID); err != nil {
		return nil, fmt.Errorf("response id not an integer: %s", string(envelope.ID))
	}
	if gotID != id {
		return nil, fmt.Errorf("response id mismatch: sent %d, got %d", id, gotID)
	}
	return envelope.Result, nil
}

// notify sends a JSON-RPC notification (no id, no response).
func (s *session) notify(method string, params any) error {
	req := map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
	}
	if params != nil {
		req["params"] = params
	}
	return s.writeFrame(req)
}

// handlerEnvelope is the JSON shape of an mcp.Response as seen on the
// wire. Keep this decoupled from the internal type — this is what we
// parse out of the resources/read content text field, and our
// assertions are expressed in terms of the protocol surface, not the
// internal Go types.
type handlerEnvelope struct {
	Status   string          `json:"status"`
	Data     json.RawMessage `json:"data"`
	Metadata struct {
		ServerVersion string `json:"server_version"`
		ElapsedMs     int64  `json:"elapsed_ms"`
	} `json:"metadata"`
}

// readResource sends resources/read and unwraps the envelope from
// result.contents[0].text, which per the MCP spec carries the
// JSON-serialized handler Response.
func (s *session) readResource(id int, uri string) (*handlerEnvelope, error) {
	result, err := s.request(id, "resources/read",
		map[string]any{"uri": uri})
	if err != nil {
		return nil, err
	}
	var wrapper struct {
		Contents []struct {
			URI      string `json:"uri"`
			MIMEType string `json:"mimeType"`
			Text     string `json:"text"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(result, &wrapper); err != nil {
		return nil, fmt.Errorf("decode resources/read result: %w", err)
	}
	if len(wrapper.Contents) != 1 {
		return nil, fmt.Errorf("expected 1 content entry, got %d", len(wrapper.Contents))
	}
	var env handlerEnvelope
	if err := json.Unmarshal([]byte(wrapper.Contents[0].Text), &env); err != nil {
		return nil, fmt.Errorf("decode inner envelope: %w (raw: %s)",
			err, wrapper.Contents[0].Text)
	}
	return &env, nil
}

func (s *session) writeFrame(frame any) error {
	b, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = s.stdin.Write(b)
	return err
}

// readFrame reads one newline-delimited JSON response. Times out after
// frameTimeout so a hung server produces a clear error, not a silent
// hang. The read happens in a goroutine so we can race it against a
// timer without abandoning the underlying reader (bufio.Reader is not
// safe for concurrent use, but we only have one consumer of it).
func (s *session) readFrame() ([]byte, error) {
	type result struct {
		line []byte
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := s.stdout.ReadBytes('\n')
		ch <- result{line: line, err: err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			if errors.Is(r.err, io.EOF) {
				return nil, fmt.Errorf("server closed stdout unexpectedly (stderr: %s)",
					s.stderrBuf.String())
			}
			return nil, r.err
		}
		return r.line, nil
	case <-time.After(frameTimeout):
		return nil, fmt.Errorf("timed out after %s waiting for response (stderr: %s)",
			frameTimeout, s.stderrBuf.String())
	}
}

// Close shuts the server down cleanly by closing stdin (the MCP stdio
// shutdown signal), then waits for the process to exit. A non-zero
// exit is surfaced through nextErr so callers who want to assert clean
// shutdown can see it.
func (s *session) Close() {
	_ = s.stdin.Close()
	s.nextErr = s.cmd.Wait()
}

// StderrSnapshot returns the server's stderr output so far, for use in
// failure diagnostics.
func (s *session) StderrSnapshot() string { return s.stderrBuf.String() }

// reporter tracks assertion pass/fail and prints results. First failure
// marks the run as failed but we keep going through cheap assertions so
// the developer sees the full picture, not just the first broken thing.
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
// assertion failed. The session's stderr is echoed on failure so the
// developer can see any server-side diagnostics.
func (r *reporter) finish(s *session) error {
	total := r.passes + len(r.fails)
	fmt.Println()
	if len(r.fails) > 0 {
		fmt.Printf("%d/%d assertions failed.\n", len(r.fails), total)
		if stderr := s.StderrSnapshot(); stderr != "" {
			fmt.Fprintln(os.Stderr, "--- server stderr: ---")
			fmt.Fprintln(os.Stderr, stderr)
		}
		return fmt.Errorf("smoke test failed")
	}
	fmt.Printf("All %d assertions passed.\n", total)
	return nil
}

// safeBuffer is an io.Writer backed by a byte slice that serializes
// concurrent writes. exec.Cmd may write to stderr from its own
// goroutine, and we read from it at failure time — we need the reads
// to see a consistent view.
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

// containsAll reports whether s contains every substring in needles.
// Used by assertions that check multiple required anchors in a text
// blob (initialize.instructions, help content) without bloating the
// assertion list — one failure logs one line, which is enough to
// prompt the developer to look at the text and see what's missing.
func containsAll(s string, needles ...string) bool {
	for _, n := range needles {
		if !strings.Contains(s, n) {
			return false
		}
	}
	return true
}
