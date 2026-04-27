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

// Sample OTLP-JSON trace bodies with realistic shape — each
// represents one ExportTraceServiceRequest as it would land in
// raw/traces.jsonl.
//
// fixtureTraces (4 spans across 2 sessions):
//   - Session sess-A: 1 llm_request span (query_source=security-analyst),
//     1 tool span (Bash), 1 tool span (mcp__signatory__signatory_signals)
//   - Session sess-B: 1 llm_request span (query_source=other) — must
//     be filtered out when reporting on sess-A
const fixtureTraces = `{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"claude-code"}},{"key":"session.id","value":{"stringValue":"sess-A"}}]},"scopeSpans":[{"spans":[{"name":"claude_code.llm_request","spanId":"01","traceId":"0102030405060708090a0b0c0d0e0f10","attributes":[{"key":"query_source","value":{"stringValue":"security-analyst"}}]},{"name":"claude_code.tool","spanId":"02","traceId":"0102030405060708090a0b0c0d0e0f10","attributes":[{"key":"tool.name","value":{"stringValue":"Bash"}},{"key":"query_source","value":{"stringValue":"security-analyst"}}]}]}]}]}
{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"claude-code"}},{"key":"session.id","value":{"stringValue":"sess-A"}}]},"scopeSpans":[{"spans":[{"name":"claude_code.tool","spanId":"03","traceId":"aaaa","attributes":[{"key":"tool.name","value":{"stringValue":"mcp__signatory__signatory_signals"}},{"key":"query_source","value":{"stringValue":"security-analyst"}}]}]}]}]}
{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"claude-code"}},{"key":"session.id","value":{"stringValue":"sess-B"}}]},"scopeSpans":[{"spans":[{"name":"claude_code.llm_request","spanId":"99","traceId":"bbbb","attributes":[{"key":"query_source","value":{"stringValue":"other"}}]}]}]}]}`

// fixtureHooksA — hook events for session sess-A. Mix of
// classifications including the two dogfood-relevant ones
// (external_*, signatory_source).
const fixtureHooksA = `{"ts":"2026-04-27T15:00:00Z","event":"PreToolUse","session_id":"sess-A","tool_use_id":"tu1","tool_name":"Bash","classification":"external_curl","detail":"curl https://pypi.org/pypi/requests/json","cwd":"/Users/sarah/git/signatory"}
{"ts":"2026-04-27T15:00:01Z","event":"PreToolUse","session_id":"sess-A","tool_use_id":"tu2","tool_name":"WebFetch","classification":"external_web","detail":"https://github.com/owner/repo/blob/main/README.md","cwd":"/Users/sarah/git/signatory"}
{"ts":"2026-04-27T15:00:02Z","event":"PreToolUse","session_id":"sess-A","tool_use_id":"tu3","tool_name":"Read","classification":"signatory_source","detail":"/Users/sarah/git/signatory/internal/profile/uri.go","cwd":"/Users/sarah/git/signatory"}
{"ts":"2026-04-27T15:00:03Z","event":"PreToolUse","session_id":"sess-A","tool_use_id":"tu4","tool_name":"Read","classification":"signatory_source","detail":"/Users/sarah/git/signatory/cmd/signatory/main.go","cwd":"/Users/sarah/git/signatory"}
{"ts":"2026-04-27T15:00:04Z","event":"PreToolUse","session_id":"sess-A","tool_use_id":"tu5","tool_name":"Bash","classification":"local_signatory_cli","detail":"signatory analyze pkg:pypi/X","cwd":"/Users/sarah/git/signatory"}
{"ts":"2026-04-27T15:00:05Z","event":"PreToolUse","session_id":"sess-A","tool_use_id":"tu6","tool_name":"mcp__signatory__signatory_signals","classification":"local_db","detail":"mcp__signatory__signatory_signals","cwd":"/Users/sarah/git/signatory"}
{"ts":"2026-04-27T15:00:06Z","event":"PreToolUse","session_id":"sess-A","tool_use_id":"tu7","tool_name":"Bash","classification":"local_other","detail":"ls","cwd":"/Users/sarah/git/signatory"}`

// writeRawDir lays out the fixture files in a temp raw/ dir as
// the report subcommand expects to find them.
func writeRawDir(t *testing.T, traces, hooksA string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "traces.jsonl"), []byte(traces+"\n"), 0o644))
	if hooksA != "" {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "hooks-sess-A.jsonl"), []byte(hooksA+"\n"), 0o644))
	}
	return dir
}

// TestReport_RendersSectionHeaders pins the markdown structure —
// each report has the same five sections so dogfood readers know
// where to look. Section headings are the load-bearing stable
// surface; counts inside each section are checked separately.
func TestReport_RendersSectionHeaders(t *testing.T) {
	t.Parallel()
	rawDir := writeRawDir(t, fixtureTraces, fixtureHooksA)
	outDir := t.TempDir()

	require.NoError(t, runReport("sess-A", rawDir, outDir))

	report := readReport(t, outDir, "sess-A")
	for _, section := range []string{
		"# Dogfood report — session sess-A",
		"## Subagent activity",
		"## Tool-call classification",
		"## External calls (cache-miss candidates)",
		"## Source reads (underspecification candidates)",
	} {
		assert.Contains(t, report, section, "missing section: %s", section)
	}
}

// TestReport_FiltersBySessionID confirms that spans/hooks for
// other sessions don't pollute the report. fixtureTraces
// includes a sess-B span; it must NOT appear in the sess-A report.
func TestReport_FiltersBySessionID(t *testing.T) {
	t.Parallel()
	rawDir := writeRawDir(t, fixtureTraces, fixtureHooksA)
	outDir := t.TempDir()

	require.NoError(t, runReport("sess-A", rawDir, outDir))

	report := readReport(t, outDir, "sess-A")
	// "other" subagent (sess-B's query_source) should NOT appear as
	// a subagent table row. Substring match would false-positive on
	// `local_other` classification row, so we anchor the check to
	// the markdown row syntax.
	assert.NotRegexp(t, `(?m)^\| other \|`, report,
		"sess-B span (query_source=other) leaked into sess-A subagent table")
	// security-analyst (sess-A) SHOULD appear as a subagent row
	assert.Regexp(t, `(?m)^\| security-analyst \|`, report)
}

// TestReport_SubagentSpanCounts pins the per-subagent span tally.
// sess-A has 3 spans for security-analyst (1 llm_request, 2 tool).
func TestReport_SubagentSpanCounts(t *testing.T) {
	t.Parallel()
	rawDir := writeRawDir(t, fixtureTraces, fixtureHooksA)
	outDir := t.TempDir()

	require.NoError(t, runReport("sess-A", rawDir, outDir))

	report := readReport(t, outDir, "sess-A")
	// security-analyst row should show its span totals: 1 llm + 2 tool
	assert.Regexp(t, `(?m)\| security-analyst \| 1 \| 2 \|`, report,
		"subagent row should have 1 llm_request + 2 tool spans")
}

// TestReport_ClassificationCounts pins the tool-call breakdown.
// fixtureHooksA has 1 each of external_curl, external_web,
// 2 signatory_source, 1 local_signatory_cli, 1 local_db, 1 local_other.
func TestReport_ClassificationCounts(t *testing.T) {
	t.Parallel()
	rawDir := writeRawDir(t, fixtureTraces, fixtureHooksA)
	outDir := t.TempDir()

	require.NoError(t, runReport("sess-A", rawDir, outDir))

	report := readReport(t, outDir, "sess-A")
	cases := []struct {
		classification string
		count          int
	}{
		{"local_db", 1},
		{"local_signatory_cli", 1},
		{"local_other", 1},
		{"external_web", 1},
		{"external_curl", 1},
		{"signatory_source", 2},
	}
	for _, tc := range cases {
		// The classification table renders one row per category:
		// `| <classification> | <count> |`
		pattern := `(?m)\| ` + tc.classification + ` \| ` + itoa(tc.count) + ` \|`
		assert.Regexp(t, pattern, report,
			"classification %q should report %d", tc.classification, tc.count)
	}
}

// TestReport_ExternalCallsListed confirms the cache-miss-candidate
// section enumerates each external call with its detail string —
// that's what makes the section actionable ("did we have this URL
// in the local store?").
func TestReport_ExternalCallsListed(t *testing.T) {
	t.Parallel()
	rawDir := writeRawDir(t, fixtureTraces, fixtureHooksA)
	outDir := t.TempDir()

	require.NoError(t, runReport("sess-A", rawDir, outDir))

	report := readReport(t, outDir, "sess-A")
	assert.Contains(t, report, "curl https://pypi.org/pypi/requests/json",
		"external_curl detail should appear under 'External calls'")
	assert.Contains(t, report, "https://github.com/owner/repo/blob/main/README.md",
		"external_web detail should appear under 'External calls'")
}

// TestReport_SourceReadsListed confirms the underspecification
// section enumerates each source-read path. These are the
// "handoff prompt didn't tell the analyst what they needed"
// signals; the path tells us WHICH file the analyst reached for.
func TestReport_SourceReadsListed(t *testing.T) {
	t.Parallel()
	rawDir := writeRawDir(t, fixtureTraces, fixtureHooksA)
	outDir := t.TempDir()

	require.NoError(t, runReport("sess-A", rawDir, outDir))

	report := readReport(t, outDir, "sess-A")
	assert.Contains(t, report, "internal/profile/uri.go")
	assert.Contains(t, report, "cmd/signatory/main.go")
}

// TestReport_HandlesNoHookFile — if a session has OTEL traces but
// no hook events (e.g., the receiver was running but the
// PreToolUse hook wasn't registered yet), the report still
// renders with the trace-derived sections populated and the
// hook-derived sections empty.
func TestReport_HandlesNoHookFile(t *testing.T) {
	t.Parallel()
	rawDir := writeRawDir(t, fixtureTraces, "") // no hooks file
	outDir := t.TempDir()

	require.NoError(t, runReport("sess-A", rawDir, outDir))

	report := readReport(t, outDir, "sess-A")
	assert.Contains(t, report, "## Subagent activity",
		"trace-derived section must still render without hooks")
	assert.Contains(t, report, "## Tool-call classification")
	// And the classification table should be present but empty —
	// the section header lands but no classification rows do.
	assert.Contains(t, report, "no hook events recorded",
		"empty-hooks state should be documented in the report, not silent")
}

// TestReport_ErrorsOnUnknownSession — running a report against a
// session id that has no matching traces AND no hook file is a
// user error we surface with a clear message rather than emitting
// an empty report (which would look like "everything was clean"
// to a casual reader).
func TestReport_ErrorsOnUnknownSession(t *testing.T) {
	t.Parallel()
	rawDir := writeRawDir(t, fixtureTraces, fixtureHooksA)
	outDir := t.TempDir()

	err := runReport("sess-NONEXISTENT", rawDir, outDir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sess-NONEXISTENT",
		"error should name the session id the user asked for")
}

// readReport reads the rendered report.md from the out-dir
// session-id subdir.
func readReport(t *testing.T, outDir, sessionID string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(outDir, sessionID, "report.md")) //nolint:gosec // G304: test reads from t.TempDir
	require.NoError(t, err)
	return string(b)
}

// itoa is a small helper to keep the regex patterns above
// readable without dragging in strconv at every call site.
func itoa(n int) string {
	var buf bytes.Buffer
	if n == 0 {
		return "0"
	}
	if n < 0 {
		buf.WriteByte('-')
		n = -n
	}
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	buf.Write(digits)
	return strings.TrimSpace(buf.String())
}
