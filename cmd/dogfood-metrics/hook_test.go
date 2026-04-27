package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Frozen timestamp for deterministic test output.
var fixedNow = time.Date(2026, 4, 27, 15, 0, 0, 0, time.UTC)

// hookInput is the shape Claude Code passes on hook stdin per
// round-3 verification: session_id, cwd, transcript_path,
// tool_name, tool_input, tool_use_id. Tests construct these
// inline and feed them to runHook.
func mkInput(t *testing.T, fields map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(fields)
	require.NoError(t, err)
	return b
}

// readFirstLine returns the trimmed first line of the per-session
// hook JSONL file. Used to assert the hook wrote what we expected.
func readFirstLine(t *testing.T, dir, sessionID string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "hooks-"+sessionID+".jsonl")) //nolint:gosec // G304: test reads from t.TempDir
	require.NoError(t, err)
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	require.NotEmpty(t, lines)
	return lines[0]
}

// parseLine unmarshals a written JSONL line back to a map for
// field assertions.
func parseLine(t *testing.T, line string) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(line), &m))
	return m
}

// TestHook_ClassifiesMCPSignatory — tool calls to our own MCP
// server are local DB hits. The dominant tool category in /analyze.
func TestHook_ClassifiesMCPSignatory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	in := mkInput(t, map[string]any{
		"session_id":  "sess-1",
		"cwd":         "/x",
		"tool_name":   "mcp__signatory__signatory_signals",
		"tool_input":  map[string]any{"target": "pkg:npm/express"},
		"tool_use_id": "tu-1",
	})
	require.NoError(t, runHook(bytes.NewReader(in), dir, "PreToolUse", fixedNow))

	out := parseLine(t, readFirstLine(t, dir, "sess-1"))
	assert.Equal(t, "local_db", out["classification"])
	assert.Equal(t, "mcp__signatory__signatory_signals", out["tool_name"])
	assert.Equal(t, "PreToolUse", out["event"])
	assert.Equal(t, "tu-1", out["tool_use_id"])
}

// TestHook_ClassifiesWebFetch — WebFetch / WebSearch are always
// external network calls.
func TestHook_ClassifiesWebFetch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	in := mkInput(t, map[string]any{
		"session_id":  "sess-w",
		"cwd":         "/x",
		"tool_name":   "WebFetch",
		"tool_input":  map[string]any{"url": "https://pypi.org/pypi/requests/json"},
		"tool_use_id": "tu-w",
	})
	require.NoError(t, runHook(bytes.NewReader(in), dir, "PreToolUse", fixedNow))

	out := parseLine(t, readFirstLine(t, dir, "sess-w"))
	assert.Equal(t, "external_web", out["classification"])
	assert.Equal(t, "https://pypi.org/pypi/requests/json", out["detail"])
}

// TestHook_ClassifiesBash covers the Bash command classifier — the
// most nuanced branch. Subtests pin each pattern.
func TestHook_ClassifiesBash(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name           string
		cmd            string
		wantClassified string
	}{
		// Local — running our own CLI / tests
		{"signatory CLI direct", "signatory analyze pkg:pypi/X", "local_signatory_cli"},
		{"signatory via go run", "go run ./cmd/signatory analyze pkg:pypi/X", "local_signatory_cli"},
		{"signatory subcommand only", "signatory", "local_signatory_cli"},
		{"signatory with redirection", "signatory show-analyses 2>&1 | head", "local_signatory_cli"},

		// External — network calls. These are the dogfood-relevant
		// "the LLM reached for the network when it should have used
		// the local store" patterns.
		{"curl basic", "curl -sS https://pypi.org/pypi/requests/json", "external_curl"},
		{"curl with flags", "curl -fLo out.json https://example.com/", "external_curl"},
		{"wget", "wget https://example.com/file", "external_curl"},
		{"gh api call", "gh api repos/owner/repo", "external_curl"},
		{"gh pr list", "gh pr list", "external_curl"},
		{"git clone remote", "git clone https://github.com/owner/repo", "external_git"},
		{"git fetch", "git fetch origin", "external_git"},
		{"git ls-remote", "git ls-remote https://github.com/owner/repo refs/tags/v1.0", "external_git"},

		// Local — anything else
		{"ls", "ls -la /tmp", "local_other"},
		{"cat", "cat /etc/hostname", "local_other"},
		{"git log local", "git log --oneline -5", "local_other"},
		{"go test", "go test ./...", "local_other"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			in := mkInput(t, map[string]any{
				"session_id":  "sess-b",
				"cwd":         "/x",
				"tool_name":   "Bash",
				"tool_input":  map[string]any{"command": tc.cmd},
				"tool_use_id": "tu-b",
			})
			require.NoError(t, runHook(bytes.NewReader(in), dir, "PreToolUse", fixedNow))
			out := parseLine(t, readFirstLine(t, dir, "sess-b"))
			assert.Equal(t, tc.wantClassified, out["classification"],
				"command %q: classification mismatch", tc.cmd)
		})
	}
}

// TestHook_ClassifiesReadAgainstSignatorySource — the
// underspecification signal. If an LLM analyst (or our own work)
// reaches for signatory's own source code (internal/, cmd/,
// templates/handoffs/) inside the project's CWD, that's evidence
// the handoff template or MCP surface didn't tell them what they
// needed.
func TestHook_ClassifiesReadAgainstSignatorySource(t *testing.T) {
	t.Parallel()
	cwd := "/Users/sarah/git/signatory"
	cases := []struct {
		name           string
		path           string
		wantClassified string
	}{
		{"internal/", cwd + "/internal/profile/uri.go", "signatory_source"},
		{"cmd/", cwd + "/cmd/signatory/main.go", "signatory_source"},
		{"templates/handoffs/", cwd + "/templates/handoffs/security-review-v1.md", "signatory_source"},
		{"design/ is not source", cwd + "/design/agent-otel.md", "local_other"},
		{"README at root is not source", cwd + "/README.md", "local_other"},
		{"outside cwd entirely", "/tmp/something.go", "local_other"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			in := mkInput(t, map[string]any{
				"session_id":  "sess-r",
				"cwd":         cwd,
				"tool_name":   "Read",
				"tool_input":  map[string]any{"file_path": tc.path},
				"tool_use_id": "tu-r",
			})
			require.NoError(t, runHook(bytes.NewReader(in), dir, "PreToolUse", fixedNow))
			out := parseLine(t, readFirstLine(t, dir, "sess-r"))
			assert.Equal(t, tc.wantClassified, out["classification"],
				"path %q: classification mismatch", tc.path)
		})
	}
}

// TestHook_AppendsToSessionFile — multiple invocations against the
// same session_id append, not overwrite. Also confirms the per-session
// filename pattern (hooks-<session_id>.jsonl).
func TestHook_AppendsToSessionFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for i := range 3 {
		in := mkInput(t, map[string]any{
			"session_id":  "sess-append",
			"cwd":         "/x",
			"tool_name":   "Bash",
			"tool_input":  map[string]any{"command": "ls"},
			"tool_use_id": "tu-" + string(rune('a'+i)),
		})
		require.NoError(t, runHook(bytes.NewReader(in), dir, "PreToolUse", fixedNow))
	}
	b, err := os.ReadFile(filepath.Join(dir, "hooks-sess-append.jsonl"))
	require.NoError(t, err)
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	assert.Len(t, lines, 3)
}

// TestHook_HandlesMalformedInput — bad input must NOT exit non-zero
// (would block the tool call per round-3 verification: exit 2 = deny).
// Hook records a malformed-event line so we know something happened
// and returns nil.
func TestHook_HandlesMalformedInput(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	in := []byte(`{not json`)
	err := runHook(bytes.NewReader(in), dir, "PreToolUse", fixedNow)
	assert.NoError(t, err, "malformed input must not surface as error (would block the tool call)")

	// Malformed events land in a generic file since we can't extract session_id.
	matches, err := filepath.Glob(filepath.Join(dir, "hooks-*.jsonl"))
	require.NoError(t, err)
	require.Len(t, matches, 1, "exactly one malformed-events file should exist")
	b, err := os.ReadFile(matches[0]) //nolint:gosec // G304: test reads from t.TempDir
	require.NoError(t, err)
	out := parseLine(t, strings.TrimRight(string(b), "\n"))
	assert.Equal(t, "malformed", out["event"])
}

// TestHook_PrefersPayloadEventOverFlag — when Claude Code includes
// hook_event_name in the stdin JSON (round 6 verification confirms
// it always does on current versions), use that as the canonical
// event identifier in the output line. The --event flag remains as
// a fallback for older Claude Code versions where the payload
// might omit the field, but the payload value wins when present.
func TestHook_PrefersPayloadEventOverFlag(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	in := mkInput(t, map[string]any{
		"session_id":      "sess-payload",
		"cwd":             "/x",
		"hook_event_name": "PostToolUse", // payload says PostToolUse
		"tool_name":       "Bash",
		"tool_input":      map[string]any{"command": "ls"},
		"tool_use_id":     "tu-p",
	})
	// --event arg says PreToolUse; payload wins.
	require.NoError(t, runHook(bytes.NewReader(in), dir, "PreToolUse", fixedNow))
	out := parseLine(t, readFirstLine(t, dir, "sess-payload"))
	assert.Equal(t, "PostToolUse", out["event"],
		"payload's hook_event_name should override --event flag value")
}

// TestHook_FallsBackToFlagWhenPayloadOmitsEvent — older Claude
// Code or other hook-protocol senders might not include
// hook_event_name. In that case the --event flag is what we
// fall back to.
func TestHook_FallsBackToFlagWhenPayloadOmitsEvent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	in := mkInput(t, map[string]any{
		"session_id":  "sess-fallback",
		"cwd":         "/x",
		"tool_name":   "Bash",
		"tool_input":  map[string]any{"command": "ls"},
		"tool_use_id": "tu-f",
		// no hook_event_name field
	})
	require.NoError(t, runHook(bytes.NewReader(in), dir, "PreToolUse", fixedNow))
	out := parseLine(t, readFirstLine(t, dir, "sess-fallback"))
	assert.Equal(t, "PreToolUse", out["event"],
		"missing hook_event_name should fall back to --event flag value")
}

// TestHook_TimestampIsISO8601UTC — the ts field must be RFC 3339
// with a trailing Z so per-session reports can sort events by
// arrival without timezone math.
func TestHook_TimestampIsISO8601UTC(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	in := mkInput(t, map[string]any{
		"session_id":  "sess-t",
		"cwd":         "/x",
		"tool_name":   "Bash",
		"tool_input":  map[string]any{"command": "ls"},
		"tool_use_id": "tu-t",
	})
	require.NoError(t, runHook(bytes.NewReader(in), dir, "PreToolUse", fixedNow))
	out := parseLine(t, readFirstLine(t, dir, "sess-t"))
	assert.Equal(t, "2026-04-27T15:00:00Z", out["ts"])
}
