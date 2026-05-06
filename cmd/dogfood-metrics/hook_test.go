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
		{"design/ is not source", cwd + "/design/vision.md", "local_other"},
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

// ----- Subagent dispatch (Task / Agent tool) classification -----
//
// The Task tool's tool_input shape is documented at the
// user-facing tool API level (description, prompt, subagent_type)
// but NOT pinned by Claude Code's hook reference at the inner-
// shape level. These tests cover the three fallback tiers of
// what the classifier extracts as the per-dispatch detail:
//
//  1. tool_input.description present  → use it
//  2. only tool_input.subagent_type   → use that as a coarse label
//  3. neither                          → "(unspecified)"
//
// All three paths classify as "subagent_dispatch" so the report's
// per-agent rollup can find them by classification regardless of
// which fallback fired. The tool_input_keys field in the recorded
// event is populated unconditionally for Agent invocations so the
// next dogfood's inspect output surfaces what fields actually
// arrived (validates / invalidates the assumption above without
// re-running the dogfood twice).

// TestHook_ClassifiesAgent_WithDescription: the happy path —
// Claude Code's PreToolUse passes through the description field
// the orchestrator wrote when calling Task. Detail is the
// description verbatim; classification is "subagent_dispatch".
func TestHook_ClassifiesAgent_WithDescription(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	in := mkInput(t, map[string]any{
		"session_id":  "sess-d",
		"cwd":         "/x",
		"tool_name":   "Agent",
		"tool_use_id": "tu-d",
		"tool_input": map[string]any{
			"description":   "Provenance review",
			"subagent_type": "general-purpose",
			"prompt":        "long prompt body...",
		},
	})
	require.NoError(t, runHook(bytes.NewReader(in), dir, "PreToolUse", fixedNow))

	out := parseLine(t, readFirstLine(t, dir, "sess-d"))
	assert.Equal(t, "subagent_dispatch", out["classification"],
		"Agent tool calls must classify as subagent_dispatch so the report's per-agent rollup can find them")
	assert.Equal(t, "Provenance review", out["detail"],
		"detail must be the description verbatim — that's the per-purpose label the orchestrator chose")
}

// TestHook_ClassifiesAgent_FallbackToSubagentType: when
// tool_input has subagent_type but no description (e.g., a
// Claude Code build that doesn't expose description, or a
// hand-crafted Task call without one), we fall back to the
// agent type. Less informative than the description but still
// better than a blank label — at least the report's bucket key
// distinguishes "general-purpose" from "Plan" or "Explore".
func TestHook_ClassifiesAgent_FallbackToSubagentType(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	in := mkInput(t, map[string]any{
		"session_id":  "sess-st",
		"cwd":         "/x",
		"tool_name":   "Agent",
		"tool_use_id": "tu-st",
		"tool_input": map[string]any{
			"subagent_type": "Plan",
			"prompt":        "design the migration",
		},
	})
	require.NoError(t, runHook(bytes.NewReader(in), dir, "PreToolUse", fixedNow))

	out := parseLine(t, readFirstLine(t, dir, "sess-st"))
	assert.Equal(t, "subagent_dispatch", out["classification"])
	assert.Equal(t, "Plan", out["detail"],
		"absent description, fall back to subagent_type — coarse but still a bucket key")
}

// TestHook_ClassifiesAgent_FallbackToUnspecified: neither
// description nor subagent_type present. Use a sentinel string
// so the report's per-agent rollup has SOMETHING to bucket on
// rather than an empty key colliding with other empty keys.
func TestHook_ClassifiesAgent_FallbackToUnspecified(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	in := mkInput(t, map[string]any{
		"session_id":  "sess-u",
		"cwd":         "/x",
		"tool_name":   "Agent",
		"tool_use_id": "tu-u",
		"tool_input":  map[string]any{},
	})
	require.NoError(t, runHook(bytes.NewReader(in), dir, "PreToolUse", fixedNow))

	out := parseLine(t, readFirstLine(t, dir, "sess-u"))
	assert.Equal(t, "subagent_dispatch", out["classification"])
	assert.Equal(t, "(unspecified)", out["detail"],
		"empty tool_input must produce a sentinel detail, not an empty string that would collide with other empty-detail events")
}

// TestHook_AgentDispatch_CapturesInputKeys: every Agent
// invocation records the SET of keys present in tool_input as a
// JSON array on the event. This is the visibility surface the
// inspect tool reads to surface "what fields actually arrived" —
// if a Claude Code build emits a key shape we didn't anticipate,
// the operator sees it without having to capture raw JSON
// separately. Uses sorted-string output for stable ordering.
//
// Non-Agent tool calls do NOT populate this field (omitempty);
// Bash invocations have a single key (`command`) which doesn't
// add value to surface and would just bloat hook lines.
func TestHook_AgentDispatch_CapturesInputKeys(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	in := mkInput(t, map[string]any{
		"session_id":  "sess-k",
		"cwd":         "/x",
		"tool_name":   "Agent",
		"tool_use_id": "tu-k",
		"tool_input": map[string]any{
			"description":   "x",
			"subagent_type": "general-purpose",
			"prompt":        "y",
		},
	})
	require.NoError(t, runHook(bytes.NewReader(in), dir, "PreToolUse", fixedNow))

	out := parseLine(t, readFirstLine(t, dir, "sess-k"))
	keys, ok := out["tool_input_keys"].([]any)
	require.True(t, ok, "tool_input_keys must be present on Agent dispatches and decode as an array")
	// Sorted: description, prompt, subagent_type.
	require.Len(t, keys, 3)
	assert.Equal(t, "description", keys[0])
	assert.Equal(t, "prompt", keys[1])
	assert.Equal(t, "subagent_type", keys[2])
}

// TestHook_NonAgent_OmitsInputKeys: keys-capture is Agent-only.
// Bash hook events should not gain a tool_input_keys field.
func TestHook_NonAgent_OmitsInputKeys(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	in := mkInput(t, map[string]any{
		"session_id":  "sess-b",
		"cwd":         "/x",
		"tool_name":   "Bash",
		"tool_use_id": "tu-b",
		"tool_input":  map[string]any{"command": "ls"},
	})
	require.NoError(t, runHook(bytes.NewReader(in), dir, "PreToolUse", fixedNow))

	out := parseLine(t, readFirstLine(t, dir, "sess-b"))
	_, present := out["tool_input_keys"]
	assert.False(t, present,
		"tool_input_keys must be omitted on non-Agent events — it's noise outside the Agent diagnostic")
}
