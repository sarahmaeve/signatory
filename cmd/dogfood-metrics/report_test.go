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

// fixtureTraces models real Claude Code OTEL emission as captured
// 2026-04-28 (verified via dogfood-metrics inspect on session
// 40366063). Two key shape facts the previous fixtures missed:
//
//  1. session.id is a SPAN attribute, not a RESOURCE attribute.
//     Resource carries only environment info (service.name,
//     os.type, etc.).
//
//  2. There is no `query_source` attribute. Subagent attribution
//     comes from `subagent_type` on `claude_code.tool` spans where
//     the orchestrator dispatched a Task; the dispatched subagent's
//     own activity lives in a separate session.id altogether.
//
// Five spans covering the report's new shape:
//   - sess-A: 1 interaction, 2 llm_request (different models +
//     tokens), 1 tool (Bash), 1 tool (Task with subagent_type)
//   - sess-B: 1 llm_request that must NOT contaminate sess-A
const fixtureTraces = `{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"claude-code"}}]},"scopeSpans":[{"spans":[{"name":"claude_code.interaction","spanId":"00","traceId":"0102030405060708090a0b0c0d0e0f10","attributes":[{"key":"session.id","value":{"stringValue":"sess-A"}}]},{"name":"claude_code.llm_request","spanId":"01","traceId":"0102030405060708090a0b0c0d0e0f10","attributes":[{"key":"session.id","value":{"stringValue":"sess-A"}},{"key":"gen_ai.request.model","value":{"stringValue":"claude-opus-4-7"}},{"key":"input_tokens","value":{"stringValue":"1000"}},{"key":"output_tokens","value":{"stringValue":"500"}},{"key":"cache_read_tokens","value":{"stringValue":"8000"}},{"key":"cache_creation_tokens","value":{"stringValue":"200"}},{"key":"duration_ms","value":{"stringValue":"3500"}},{"key":"ttft_ms","value":{"stringValue":"450"}}]},{"name":"claude_code.tool","spanId":"02","traceId":"0102030405060708090a0b0c0d0e0f10","attributes":[{"key":"session.id","value":{"stringValue":"sess-A"}},{"key":"tool_name","value":{"stringValue":"Bash"}}]}]}]}]}
{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"claude-code"}}]},"scopeSpans":[{"spans":[{"name":"claude_code.tool","spanId":"03","traceId":"aaaa","attributes":[{"key":"session.id","value":{"stringValue":"sess-A"}},{"key":"tool_name","value":{"stringValue":"Task"}},{"key":"subagent_type","value":{"stringValue":"security-analyst"}}]},{"name":"claude_code.llm_request","spanId":"04","traceId":"aaaa","attributes":[{"key":"session.id","value":{"stringValue":"sess-A"}},{"key":"gen_ai.request.model","value":{"stringValue":"claude-haiku-4-5"}},{"key":"input_tokens","value":{"stringValue":"200"}},{"key":"output_tokens","value":{"stringValue":"100"}},{"key":"duration_ms","value":{"stringValue":"800"}},{"key":"ttft_ms","value":{"stringValue":"300"}}]}]}]}]}
{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"claude-code"}}]},"scopeSpans":[{"spans":[{"name":"claude_code.llm_request","spanId":"99","traceId":"bbbb","attributes":[{"key":"session.id","value":{"stringValue":"sess-B"}},{"key":"gen_ai.request.model","value":{"stringValue":"claude-opus-4-7"}},{"key":"input_tokens","value":{"stringValue":"9999"}}]}]}]}]}`

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
// each report has the same sections so dogfood readers know where
// to look. Section headings are the load-bearing stable surface;
// counts inside each section are checked separately.
func TestReport_RendersSectionHeaders(t *testing.T) {
	t.Parallel()
	rawDir := writeRawDir(t, fixtureTraces, fixtureHooksA)
	outDir := t.TempDir()

	require.NoError(t, runReport("sess-A", rawDir, outDir))

	report := readReport(t, outDir, "sess-A")
	for _, section := range []string{
		"# Dogfood report — session sess-A",
		"## Session activity",
		"## LLM economics",
		"## Tool calls by name",
		"## Subagent dispatches",
		"## Tool-call classification",
		"## External calls (cache-miss candidates)",
		"## Source reads (underspecification candidates)",
	} {
		assert.Contains(t, report, section, "missing section: %s", section)
	}
}

// TestReport_FiltersBySessionID confirms that spans for OTHER
// sessions don't pollute the report. fixtureTraces includes a
// sess-B llm_request with input_tokens=9999 — that magic number
// must NOT appear in the sess-A LLM-economics table.
func TestReport_FiltersBySessionID(t *testing.T) {
	t.Parallel()
	rawDir := writeRawDir(t, fixtureTraces, fixtureHooksA)
	outDir := t.TempDir()

	require.NoError(t, runReport("sess-A", rawDir, outDir))

	report := readReport(t, outDir, "sess-A")
	assert.NotContains(t, report, "9999",
		"sess-B's llm_request (input_tokens=9999) leaked into the sess-A report")
}

// TestReport_SessionActivityCounts pins the high-level counts
// matching fixtureTraces' sess-A: 1 interaction, 2 LLM requests
// (one opus, one haiku), 2 tool calls (Bash + Task), 1 subagent
// dispatch (the Task spawn).
func TestReport_SessionActivityCounts(t *testing.T) {
	t.Parallel()
	rawDir := writeRawDir(t, fixtureTraces, fixtureHooksA)
	outDir := t.TempDir()

	require.NoError(t, runReport("sess-A", rawDir, outDir))

	report := readReport(t, outDir, "sess-A")
	assert.Contains(t, report, "1 user interaction(s)",
		"interaction count from claude_code.interaction span")
	assert.Contains(t, report, "2 LLM request(s)",
		"LLM-request count from claude_code.llm_request spans")
	assert.Contains(t, report, "2 tool call(s)",
		"tool-call count from claude_code.tool spans (must NOT triple-count via .execution / .blocked_on_user)")
	assert.Contains(t, report, "1 subagent dispatch(es)",
		"subagent dispatch count from tool spans with subagent_type set")
}

// TestReport_LLMEconomics_AcceptsIntValueEncoding pins the OTLP-JSON
// shape detail that token attributes use the protobuf `intValue`
// encoding (decimal string in the int field) rather than
// `stringValue`. Real Claude Code OTEL uses intValue; the previous
// fixture happened to use stringValue, which masked the
// shape-mismatch in the production reader.
//
// This test feeds tokens encoded as intValue and asserts the
// aggregator surfaces them, not zero. Pre-fix this fails because
// stringAttr only reads StringValue.
func TestReport_LLMEconomics_AcceptsIntValueEncoding(t *testing.T) {
	t.Parallel()
	intValueTraces := `{"resourceSpans":[{"resource":{"attributes":[]},"scopeSpans":[{"spans":[` +
		`{"name":"claude_code.llm_request","attributes":[` +
		`{"key":"session.id","value":{"stringValue":"sess-iv"}},` +
		`{"key":"gen_ai.request.model","value":{"stringValue":"claude-test"}},` +
		`{"key":"input_tokens","value":{"intValue":"42"}},` +
		`{"key":"output_tokens","value":{"intValue":"7"}},` +
		`{"key":"cache_read_tokens","value":{"intValue":"100"}},` +
		`{"key":"duration_ms","value":{"intValue":"2500"}},` +
		`{"key":"ttft_ms","value":{"intValue":"600"}}` +
		`]}` +
		`]}]}]}`
	rawDir := writeRawDir(t, intValueTraces, "")
	outDir := t.TempDir()

	require.NoError(t, runReport("sess-iv", rawDir, outDir))

	report := readReport(t, outDir, "sess-iv")
	assert.Regexp(t, `\| claude-test \| 1 \| 42 \| 7 \| 100 \| 0 \| 2500 \| 600 \|`, report,
		"intValue-encoded tokens must be parsed and aggregated; pre-fix the row would show all zeros")
}

// TestReport_LLMEconomics_AcceptsIntValueAsBareNumber pins the
// "either JSON encoding works" contract for OTLP intValue. The
// proto3-canonical form is `"intValue":"1234"` (string), but some
// producers emit `"intValue":1234` (bare number) instead. The
// reader must accept both — pre-fix, the bare-number form failed
// to decode and dropped entire batches silently, surfaced
// 2026-04-28 dogfood when 651 of 658 trace batches were marked
// malformed.
func TestReport_LLMEconomics_AcceptsIntValueAsBareNumber(t *testing.T) {
	t.Parallel()
	bareNumberTraces := `{"resourceSpans":[{"resource":{"attributes":[]},"scopeSpans":[{"spans":[` +
		`{"name":"claude_code.llm_request","attributes":[` +
		`{"key":"session.id","value":{"stringValue":"sess-bn"}},` +
		`{"key":"gen_ai.request.model","value":{"stringValue":"claude-test"}},` +
		`{"key":"input_tokens","value":{"intValue":42}},` +
		`{"key":"output_tokens","value":{"intValue":7}}` +
		`]}` +
		`]}]}]}`
	rawDir := writeRawDir(t, bareNumberTraces, "")
	outDir := t.TempDir()

	require.NoError(t, runReport("sess-bn", rawDir, outDir))

	report := readReport(t, outDir, "sess-bn")
	assert.Regexp(t, `\| claude-test \| 1 \| 42 \| 7 \|`, report,
		"bare-number intValue must parse and aggregate; the previous string-only decoder silently dropped these")
}

// TestReport_LLMEconomicsTokenAggregation: per-model token totals
// must accumulate across the session. fixtureTraces has one
// claude-opus-4-7 call (1000/500 input/output, 8000/200 cache) and
// one claude-haiku-4-5 call (200/100 input/output, no cache).
func TestReport_LLMEconomicsTokenAggregation(t *testing.T) {
	t.Parallel()
	rawDir := writeRawDir(t, fixtureTraces, fixtureHooksA)
	outDir := t.TempDir()

	require.NoError(t, runReport("sess-A", rawDir, outDir))

	report := readReport(t, outDir, "sess-A")
	// Per-model rows: opus first (alphabetical), then haiku.
	assert.Regexp(t,
		`\| claude-haiku-4-5 \| 1 \| 200 \| 100 \| 0 \| 0 \| 800 \| 300 \|`,
		report, "haiku row must show 1 call, 200 input, 100 output, 0 cache, 800ms, ttft 300")
	assert.Regexp(t,
		`\| claude-opus-4-7 \| 1 \| 1000 \| 500 \| 8000 \| 200 \| 3500 \| 450 \|`,
		report, "opus row must show 1 call, 1000 input, 500 output, 8000 cache_read, 200 cache_create, 3500ms, ttft 450")
	// Aggregate row is the sum across models.
	assert.Regexp(t,
		`\| \*\*TOTAL\*\* \| 2 \| 1200 \| 600 \| 8000 \| 200 \| 4300 \|`,
		report, "TOTAL row sums per-model values across the 2 LLM requests")
}

// TestReport_LLMEconomics_CacheHitRatio: cache hit ratio surfaces
// with its formula spelled out. fixtureTraces totals: 1200 input
// + 200 creation + 8000 read = 9400 input-side; 8000 read; ratio
// = 8000/9400 = 85.1%.
func TestReport_LLMEconomics_CacheHitRatio(t *testing.T) {
	t.Parallel()
	rawDir := writeRawDir(t, fixtureTraces, fixtureHooksA)
	outDir := t.TempDir()

	require.NoError(t, runReport("sess-A", rawDir, outDir))

	report := readReport(t, outDir, "sess-A")
	assert.Contains(t, report, "Cache hit ratio: 85.1%",
		"cache hit ratio must reflect cache_read / (input + cache_creation + cache_read)")
	assert.Contains(t, report, "`cache_read / (input + cache_creation + cache_read)`",
		"formula must surface inline so the operator can verify what they're reading")
}

// TestReport_ToolDistribution: per-tool counts. fixtureTraces has
// one Bash and one Task in sess-A.
func TestReport_ToolDistribution(t *testing.T) {
	t.Parallel()
	rawDir := writeRawDir(t, fixtureTraces, fixtureHooksA)
	outDir := t.TempDir()

	require.NoError(t, runReport("sess-A", rawDir, outDir))

	report := readReport(t, outDir, "sess-A")
	assert.Regexp(t, `\| Bash \| 1 \|`, report,
		"tool-distribution table must list Bash with count 1")
	assert.Regexp(t, `\| Task \| 1 \|`, report,
		"tool-distribution table must list Task with count 1")
}

// TestReport_SubagentDispatches: dispatches by subagent_type.
// fixtureTraces has one security-analyst dispatch.
func TestReport_SubagentDispatches(t *testing.T) {
	t.Parallel()
	rawDir := writeRawDir(t, fixtureTraces, fixtureHooksA)
	outDir := t.TempDir()

	require.NoError(t, runReport("sess-A", rawDir, outDir))

	report := readReport(t, outDir, "sess-A")
	assert.Regexp(t, `\| security-analyst \| 1 \|`, report,
		"subagent-dispatches table must list security-analyst with count 1")
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

// TestReport_FiltersLoopbackFromExternalCalls covers the
// dogfood-errors entry "report classifier under-categorizes
// external_web." WebFetch calls to loopback hosts (127.0.0.1,
// localhost, [::1]) are local pipeline / dev-tooling traffic,
// not cache-miss-candidate external network calls. The report
// excludes them from the "External calls" table and surfaces
// the count separately so reviewers don't lose visibility.
func TestReport_FiltersLoopbackFromExternalCalls(t *testing.T) {
	t.Parallel()
	hooks := `{"ts":"2026-04-27T15:00:00Z","event":"PreToolUse","session_id":"sess-LB","tool_name":"WebFetch","classification":"external_web","detail":"https://github.com/owner/repo","cwd":"/x"}
{"ts":"2026-04-27T15:00:01Z","event":"PreToolUse","session_id":"sess-LB","tool_name":"WebFetch","classification":"external_web","detail":"https://127.0.0.1:21517/api/sessions/abc/messages?role=security","cwd":"/x"}
{"ts":"2026-04-27T15:00:02Z","event":"PreToolUse","session_id":"sess-LB","tool_name":"WebFetch","classification":"external_web","detail":"http://localhost:8080/internal","cwd":"/x"}
{"ts":"2026-04-27T15:00:03Z","event":"PreToolUse","session_id":"sess-LB","tool_name":"WebFetch","classification":"external_web","detail":"https://[::1]:9000/local","cwd":"/x"}
{"ts":"2026-04-27T15:00:04Z","event":"PreToolUse","session_id":"sess-LB","tool_name":"WebFetch","classification":"external_web","detail":"https://api.deps.dev/v3/pkg/X","cwd":"/x"}`

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hooks-sess-LB.jsonl"), []byte(hooks+"\n"), 0o644))
	outDir := t.TempDir()

	require.NoError(t, runReport("sess-LB", dir, outDir))
	report := readReport(t, outDir, "sess-LB")

	// Real external URLs SHOULD appear in the table.
	assert.Contains(t, report, "github.com/owner/repo",
		"true external URL must remain in External calls table")
	assert.Contains(t, report, "api.deps.dev/v3/pkg/X",
		"true external URL must remain in External calls table")

	// Loopback URLs MUST NOT appear in the table — that's the bug fix.
	assert.NotContains(t, report, "127.0.0.1:21517",
		"127.0.0.1 loopback URL should be filtered out of External calls")
	assert.NotContains(t, report, "localhost:8080",
		"localhost URL should be filtered out of External calls")
	assert.NotContains(t, report, "[::1]:9000",
		"IPv6 loopback URL should be filtered out of External calls")

	// But the count of excluded loopback calls is surfaced
	// separately so reviewers don't silently lose visibility.
	assert.Regexp(t, `\(3 loopback (call|fetch)s? excluded`, report,
		"loopback exclusion count must be surfaced beneath the table")
}

// TestReport_LoopbackOnlyShowsZeroCount — when ALL external_web
// events are loopback, the External Calls table is empty and the
// "no external calls" placeholder text shouldn't lie. The
// loopback count surfaces so the reviewer knows there WAS web
// activity, just none of it cache-miss-relevant.
func TestReport_LoopbackOnlyShowsZeroCount(t *testing.T) {
	t.Parallel()
	hooks := `{"ts":"2026-04-27T15:00:00Z","event":"PreToolUse","session_id":"sess-LBO","tool_name":"WebFetch","classification":"external_web","detail":"https://127.0.0.1:21517/x","cwd":"/x"}
{"ts":"2026-04-27T15:00:01Z","event":"PreToolUse","session_id":"sess-LBO","tool_name":"WebFetch","classification":"external_web","detail":"https://localhost:8080/y","cwd":"/x"}`

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hooks-sess-LBO.jsonl"), []byte(hooks+"\n"), 0o644))
	outDir := t.TempDir()

	require.NoError(t, runReport("sess-LBO", dir, outDir))
	report := readReport(t, outDir, "sess-LBO")

	assert.Contains(t, report, "no external calls",
		"all-loopback session should still report 'no external calls'")
	assert.Regexp(t, `\(2 loopback`, report,
		"loopback count must surface even when no truly-external calls remain")
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
	assert.Contains(t, report, "## Session activity",
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
