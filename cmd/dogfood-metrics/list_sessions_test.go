package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Hook events for two sessions, lined up so sess-EARLIER is older
// (last event at 14:00) and sess-NEWER is newer (last event at
// 15:00). Used by ordering tests.
const fixtureHooksEarlier = `{"ts":"2026-04-27T13:00:00Z","event":"PreToolUse","session_id":"sess-EARLIER","tool_name":"Bash","classification":"local_other","detail":"ls"}
{"ts":"2026-04-27T14:00:00Z","event":"PreToolUse","session_id":"sess-EARLIER","tool_name":"Bash","classification":"local_other","detail":"ls"}`

const fixtureHooksNewer = `{"ts":"2026-04-27T14:30:00Z","event":"PreToolUse","session_id":"sess-NEWER","tool_name":"Read","classification":"signatory_source","detail":"/x/internal/foo.go"}
{"ts":"2026-04-27T14:45:00Z","event":"PreToolUse","session_id":"sess-NEWER","tool_name":"Bash","classification":"external_curl","detail":"curl https://x"}
{"ts":"2026-04-27T15:00:00Z","event":"PreToolUse","session_id":"sess-NEWER","tool_name":"Bash","classification":"local_other","detail":"echo done"}`

// Two-session OTLP fixture. Each line is one
// ExportTraceServiceRequest. sess-EARLIER has 1 span, sess-NEWER
// has 2 spans across different resource batches.
const fixtureTracesTwoSessions = `{"resourceSpans":[{"resource":{"attributes":[{"key":"session.id","value":{"stringValue":"sess-EARLIER"}}]},"scopeSpans":[{"spans":[{"name":"claude_code.tool","spanId":"01","traceId":"a","attributes":[]}]}]}]}
{"resourceSpans":[{"resource":{"attributes":[{"key":"session.id","value":{"stringValue":"sess-NEWER"}}]},"scopeSpans":[{"spans":[{"name":"claude_code.llm_request","spanId":"02","traceId":"b","attributes":[]},{"name":"claude_code.tool","spanId":"03","traceId":"b","attributes":[]}]}]}]}`

// writeRawFiles lays out fixture files in a temp dir as
// list-sessions expects to find them.
func writeRawFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content+"\n"), 0o644))
	}
	return dir
}

// TestListSessions_EmptyDir — no files at all means empty
// session list. NOT an error: a fresh raw/ dir is a valid state.
func TestListSessions_EmptyDir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	var buf bytes.Buffer
	err := runListSessions(dir, &buf)
	require.NoError(t, err)
	assert.Contains(t, buf.String(), "no sessions",
		"empty dir should explicitly say so, not print an empty table")
}

// TestListSessions_HooksOnly — sessions with hook events but no
// trace stream show up with HOOKS count > 0 and SPANS count = 0.
func TestListSessions_HooksOnly(t *testing.T) {
	t.Parallel()
	dir := writeRawFiles(t, map[string]string{
		"hooks-sess-NEWER.jsonl": fixtureHooksNewer,
	})

	var buf bytes.Buffer
	require.NoError(t, runListSessions(dir, &buf))
	out := buf.String()

	assert.Contains(t, out, "sess-NEWER")
	// Three hook events in fixtureHooksNewer
	assert.Regexp(t, `sess-NEWER\s+.*\s+3\s+0`, out,
		"hooks-only session should show HOOKS=3, SPANS=0")
}

// TestListSessions_TracesOnly — sessions discovered solely from
// the OTLP trace stream (e.g., the receiver was up but the hook
// wasn't registered for that session).
func TestListSessions_TracesOnly(t *testing.T) {
	t.Parallel()
	dir := writeRawFiles(t, map[string]string{
		"traces.jsonl": fixtureTracesTwoSessions,
	})

	var buf bytes.Buffer
	require.NoError(t, runListSessions(dir, &buf))
	out := buf.String()

	assert.Contains(t, out, "sess-EARLIER")
	assert.Contains(t, out, "sess-NEWER")
	// EARLIER has 1 span; NEWER has 2 spans.
	assert.Regexp(t, `sess-EARLIER\s+.*\s+0\s+1`, out,
		"sess-EARLIER from traces-only should show HOOKS=0, SPANS=1")
	assert.Regexp(t, `sess-NEWER\s+.*\s+0\s+2`, out,
		"sess-NEWER from traces-only should show HOOKS=0, SPANS=2")
}

// TestListSessions_HookAndTraceMerge — a session with both hook
// events AND trace spans appears once (not duplicated) with both
// counts populated.
func TestListSessions_HookAndTraceMerge(t *testing.T) {
	t.Parallel()
	dir := writeRawFiles(t, map[string]string{
		"hooks-sess-NEWER.jsonl": fixtureHooksNewer,
		"traces.jsonl":           fixtureTracesTwoSessions,
	})

	var buf bytes.Buffer
	require.NoError(t, runListSessions(dir, &buf))
	out := buf.String()

	// sess-NEWER has 3 hooks AND 2 spans
	assert.Regexp(t, `sess-NEWER\s+.*\s+3\s+2`, out,
		"merged session should show HOOKS=3 SPANS=2")

	// sess-NEWER must appear exactly once, not duplicated
	count := strings.Count(out, "sess-NEWER")
	assert.Equal(t, 1, count, "sess-NEWER appeared %d times; should be exactly once", count)
}

// TestListSessions_SortedByLastSeen — newest last_seen at the
// top. Critical UX: when a fresh /analyze session lands, it
// should be the FIRST row — that's the row the user is looking
// for.
func TestListSessions_SortedByLastSeen(t *testing.T) {
	t.Parallel()
	dir := writeRawFiles(t, map[string]string{
		"hooks-sess-EARLIER.jsonl": fixtureHooksEarlier, // last_seen 14:00
		"hooks-sess-NEWER.jsonl":   fixtureHooksNewer,   // last_seen 15:00
	})

	var buf bytes.Buffer
	require.NoError(t, runListSessions(dir, &buf))
	out := buf.String()

	earlierIdx := strings.Index(out, "sess-EARLIER")
	newerIdx := strings.Index(out, "sess-NEWER")
	require.NotEqual(t, -1, earlierIdx)
	require.NotEqual(t, -1, newerIdx)
	assert.Less(t, newerIdx, earlierIdx,
		"newer session should appear before earlier — newest-first ordering")
}

// TestListSessions_HeaderFormat pins the table header columns —
// stable so users can rely on the column order when piping
// through awk/grep.
func TestListSessions_HeaderFormat(t *testing.T) {
	t.Parallel()
	dir := writeRawFiles(t, map[string]string{
		"hooks-sess-NEWER.jsonl": fixtureHooksNewer,
	})

	var buf bytes.Buffer
	require.NoError(t, runListSessions(dir, &buf))
	out := buf.String()

	for _, header := range []string{"SESSION ID", "FIRST SEEN", "LAST SEEN", "HOOKS", "SPANS"} {
		assert.Contains(t, out, header, "missing header column: %s", header)
	}
}

// TestListSessions_FirstAndLastSeen — first/last timestamps
// reflect the actual data range, not just the first event seen.
func TestListSessions_FirstAndLastSeen(t *testing.T) {
	t.Parallel()
	dir := writeRawFiles(t, map[string]string{
		"hooks-sess-NEWER.jsonl": fixtureHooksNewer,
	})

	var buf bytes.Buffer
	require.NoError(t, runListSessions(dir, &buf))
	out := buf.String()

	// fixtureHooksNewer spans 14:30 → 15:00
	assert.Contains(t, out, "14:30", "first_seen 14:30 should appear")
	assert.Contains(t, out, "15:00", "last_seen 15:00 should appear")
}
