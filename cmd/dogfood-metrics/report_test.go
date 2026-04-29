package main

import (
	"bytes"
	"fmt"
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
	// 1 ttft sample → p50 = p95 = 600.
	assert.Regexp(t, `\| claude-test \| 1 \| 42 \| 7 \| 100 \| 0 \| 2500 \| 600 \| 600 \|`, report,
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

// TestReport_LLMEconomics_AcceptsSemconvTokenNames pins the
// "either name works" contract for the two token attributes that
// have OTel GenAI semantic-conventions equivalents:
//
//   - input_tokens  ↔ gen_ai.usage.input_tokens
//   - output_tokens ↔ gen_ai.usage.output_tokens
//
// Today's Claude Code emits the unprefixed vendor names (verified
// 2026-04-27, Round 6). The fallback exists so that (a) a future
// Claude Code release migrating to semconv form doesn't silently
// regress every report to zero tokens, and (b) traces.jsonl
// captured from a different semconv-compliant producer can be
// replayed through `dogfood-metrics report` without code changes.
//
// Cache tokens (cache_read_tokens / cache_creation_tokens) and
// ttft_ms have no standardized semconv equivalent at this time, so
// they're not part of the dual-key contract — only the two
// covered-by-semconv attributes are tested here.
func TestReport_LLMEconomics_AcceptsSemconvTokenNames(t *testing.T) {
	t.Parallel()
	semconvTraces := `{"resourceSpans":[{"resource":{"attributes":[]},"scopeSpans":[{"spans":[` +
		`{"name":"claude_code.llm_request","attributes":[` +
		`{"key":"session.id","value":{"stringValue":"sess-sc"}},` +
		`{"key":"gen_ai.request.model","value":{"stringValue":"claude-test"}},` +
		`{"key":"gen_ai.usage.input_tokens","value":{"stringValue":"1234"}},` +
		`{"key":"gen_ai.usage.output_tokens","value":{"stringValue":"567"}}` +
		`]}` +
		`]}]}]}`
	rawDir := writeRawDir(t, semconvTraces, "")
	outDir := t.TempDir()

	require.NoError(t, runReport("sess-sc", rawDir, outDir))

	report := readReport(t, outDir, "sess-sc")
	// Per-model row: 1 call, 1234 input, 567 output, 0 cache. Pre-fix,
	// the row would show 0/0 for tokens because the reader only
	// looked at the unprefixed vendor keys.
	assert.Regexp(t,
		`\| claude-test \| 1 \| 1234 \| 567 \| 0 \| 0 \|`,
		report,
		"semconv-named tokens must aggregate; pre-fix they decoded to zero")
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
	// Per-model rows: haiku first (alphabetical), then opus. With
	// 1 ttft sample each, p50 = p95 = the sample value.
	assert.Regexp(t,
		`\| claude-haiku-4-5 \| 1 \| 200 \| 100 \| 0 \| 0 \| 800 \| 300 \| 300 \|`,
		report, "haiku row: 1 call, 200 input, 100 output, 0 cache, 800ms, ttft 300/300")
	assert.Regexp(t,
		`\| claude-opus-4-7 \| 1 \| 1000 \| 500 \| 8000 \| 200 \| 3500 \| 450 \| 450 \|`,
		report, "opus row: 1 call, 1000 input, 500 output, 8000 cache_read, 200 cache_create, 3500ms, ttft 450/450")
	// Aggregate row sums counts/tokens but renders em-dashes for
	// percentiles — cross-model latency comparison isn't meaningful
	// (different models have different speeds), so the TOTAL row
	// elides them by design.
	assert.Regexp(t,
		`\| \*\*TOTAL\*\* \| 2 \| 1200 \| 600 \| 8000 \| 200 \| 4300 \| — \| — \|`,
		report, "TOTAL row sums counts/tokens but renders em-dashes for percentile columns")
}

// TestReport_LLMEconomics_TTFTPercentiles: per-model TTFT
// distribution surfaces as p50 (median) and p95 (tail), not just
// a mean. Replaces the previous Mean TTFT column — for skewed
// latency distributions the mean is a poor summary; the tail (p95)
// is what determines whether a workflow feels fast or slow.
//
// Fixture: 10 TTFT samples per model so the percentile math is
// exercised. The samples are crafted so:
//   - p50 (5th of 10 by lower-mode definition) = 350
//   - p95 (10th of 10) = 1200
//
// for the "claude-spread" model. Pre-fix the renderer reports a
// mean only and this test fails for absence of percentile columns.
func TestReport_LLMEconomics_TTFTPercentiles(t *testing.T) {
	t.Parallel()
	// 10 spans, ttft_ms varying so percentiles are non-trivial.
	// Sorted: [100,150,200,250,300,350,400,500,750,1200]
	// p50 (closest-rank, lower mode) = sorted[ceil(0.5*10)-1] = sorted[4] = 300
	// p95 = sorted[ceil(0.95*10)-1] = sorted[9] = 1200
	ttfts := []string{"100", "150", "200", "250", "300", "350", "400", "500", "750", "1200"}
	var spans strings.Builder
	for i, ttft := range ttfts {
		if i > 0 {
			spans.WriteString(",")
		}
		fmt.Fprintf(&spans, `{"name":"claude_code.llm_request","attributes":[{"key":"session.id","value":{"stringValue":"sess-pct"}},{"key":"gen_ai.request.model","value":{"stringValue":"claude-spread"}},{"key":"ttft_ms","value":{"intValue":"%s"}}]}`, ttft)
	}
	traces := `{"resourceSpans":[{"resource":{"attributes":[]},"scopeSpans":[{"spans":[` + spans.String() + `]}]}]}`
	rawDir := writeRawDir(t, traces, "")
	outDir := t.TempDir()

	require.NoError(t, runReport("sess-pct", rawDir, outDir))

	report := readReport(t, outDir, "sess-pct")
	// Header row: must have both TTFT p50 and TTFT p95 columns.
	assert.Contains(t, report, "TTFT p50",
		"latency table must include p50 (median) — robust to outliers")
	assert.Contains(t, report, "TTFT p95",
		"latency table must include p95 (tail) — surfaces the slow calls that dominate user-perceived latency")
	// Data row: 10 calls, total ms = 0 (we didn't set duration_ms),
	// p50=300, p95=1200.
	assert.Regexp(t, `\| claude-spread \| 10 \| 0 \| 0 \| 0 \| 0 \| 0 \| 300 \| 1200 \|`, report,
		"row must show p50=300 (5th-percentile by closest-rank) and p95=1200 (10th-percentile)")
}

// TestReport_LLMEconomics_TTFTPercentiles_SmallSample: when there
// are fewer than 10 TTFT samples, p95 is statistically noisy. We
// still render it (a coarse signal beats no signal), but the test
// pins the small-sample behavior so the impl doesn't accidentally
// elide it.
func TestReport_LLMEconomics_TTFTPercentiles_SmallSample(t *testing.T) {
	t.Parallel()
	// 3 samples: 100, 200, 500. p50=200, p95=500 (both fall on
	// the highest-rank index when N is small).
	traces := `{"resourceSpans":[{"resource":{"attributes":[]},"scopeSpans":[{"spans":[` +
		`{"name":"claude_code.llm_request","attributes":[{"key":"session.id","value":{"stringValue":"sess-small"}},{"key":"gen_ai.request.model","value":{"stringValue":"claude-3"}},{"key":"ttft_ms","value":{"intValue":"100"}}]},` +
		`{"name":"claude_code.llm_request","attributes":[{"key":"session.id","value":{"stringValue":"sess-small"}},{"key":"gen_ai.request.model","value":{"stringValue":"claude-3"}},{"key":"ttft_ms","value":{"intValue":"500"}}]},` +
		`{"name":"claude_code.llm_request","attributes":[{"key":"session.id","value":{"stringValue":"sess-small"}},{"key":"gen_ai.request.model","value":{"stringValue":"claude-3"}},{"key":"ttft_ms","value":{"intValue":"200"}}]}` +
		`]}]}]}`
	rawDir := writeRawDir(t, traces, "")
	outDir := t.TempDir()

	require.NoError(t, runReport("sess-small", rawDir, outDir))

	report := readReport(t, outDir, "sess-small")
	// p50 of [100,200,500] = sorted[ceil(0.5*3)-1] = sorted[1] = 200
	// p95 of [100,200,500] = sorted[ceil(0.95*3)-1] = sorted[2] = 500
	assert.Regexp(t, `\| claude-3 \| 3 \| 0 \| 0 \| 0 \| 0 \| 0 \| 200 \| 500 \|`, report,
		"3-sample distribution: p50=200, p95=500 by closest-rank")
}

// TestReport_LLMEconomics_TTFTPercentiles_NoSamples: a model with
// no TTFT samples (e.g., spans without ttft_ms attribute) renders
// percentile columns as "—" rather than 0, so the operator
// distinguishes "fast first-token" from "no data."
func TestReport_LLMEconomics_TTFTPercentiles_NoSamples(t *testing.T) {
	t.Parallel()
	traces := `{"resourceSpans":[{"resource":{"attributes":[]},"scopeSpans":[{"spans":[` +
		`{"name":"claude_code.llm_request","attributes":[{"key":"session.id","value":{"stringValue":"sess-noTTFT"}},{"key":"gen_ai.request.model","value":{"stringValue":"claude-noTTFT"}}]}` +
		`]}]}]}`
	rawDir := writeRawDir(t, traces, "")
	outDir := t.TempDir()

	require.NoError(t, runReport("sess-noTTFT", rawDir, outDir))

	report := readReport(t, outDir, "sess-noTTFT")
	assert.Regexp(t, `\| claude-noTTFT \| 1 \| 0 \| 0 \| 0 \| 0 \| 0 \| — \| — \|`, report,
		"no-samples case must render em-dashes for both percentile columns, not zero")
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

// ----- Per-agent economics -----
//
// The 2026-04-28 trace-correlation work confirmed that current
// Claude Code does NOT fork a separate session.id on Task
// dispatch — children stay in the parent's session.id and traceId,
// nested only via parentSpanId. Per-agent attribution therefore is
// a parent-pointer walk WITHIN the session: an llm_request span
// belongs to subagent type X iff its nearest ancestor (via
// parentSpanId) carrying subagent_type=X is in the dispatch chain.
// LLM requests with no dispatch ancestor are the orchestrator's.

// fixtureAgentEconomics is a hand-built session with three
// distinct LLM-request shapes that exercise the per-agent
// attribution algorithm:
//
//   - llm_request "L1" — root-attached (no parent), orchestrator
//   - dispatch "D1"    — subagent_type=provenance-review
//   - llm_request "L2" — parent=D1, attribute to provenance-review
//   - dispatch "D2"    — subagent_type=security-review-go
//   - llm_request "L3" — parent=D2, attribute to security-review-go
//   - llm_request "L4" — parent=tool-span "T1" whose parent=D2,
//     attribute to security-review-go (transitive)
//   - tool span "T1"   — parent=D2, no LLM economics
//
// Tokens chosen so the buckets sum cleanly:
//
//	orchestrator: 1000+500
//	provenance:    100+ 50
//	security:      200+100  AND   80+ 40   = 280+140
//	TOTAL:        1660+790
const fixtureAgentEconomics = `{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"claude-code"}}]},"scopeSpans":[{"spans":[` +
	// L1 — orchestrator-direct llm_request, no parent.
	`{"name":"claude_code.llm_request","spanId":"L1","attributes":[` +
	`{"key":"session.id","value":{"stringValue":"agent-sess"}},` +
	`{"key":"gen_ai.request.model","value":{"stringValue":"claude-opus-4-7"}},` +
	`{"key":"input_tokens","value":{"stringValue":"1000"}},` +
	`{"key":"output_tokens","value":{"stringValue":"500"}},` +
	`{"key":"duration_ms","value":{"stringValue":"3000"}}` +
	`]},` +
	// D1 — provenance-review dispatch.
	`{"name":"claude_code.tool","spanId":"D1","attributes":[` +
	`{"key":"session.id","value":{"stringValue":"agent-sess"}},` +
	`{"key":"tool_name","value":{"stringValue":"Task"}},` +
	`{"key":"subagent_type","value":{"stringValue":"provenance-review"}}` +
	`]},` +
	// L2 — direct child of D1.
	`{"name":"claude_code.llm_request","spanId":"L2","parentSpanId":"D1","attributes":[` +
	`{"key":"session.id","value":{"stringValue":"agent-sess"}},` +
	`{"key":"gen_ai.request.model","value":{"stringValue":"claude-opus-4-7"}},` +
	`{"key":"input_tokens","value":{"stringValue":"100"}},` +
	`{"key":"output_tokens","value":{"stringValue":"50"}},` +
	`{"key":"duration_ms","value":{"stringValue":"500"}}` +
	`]},` +
	// D2 — security-review-go dispatch.
	`{"name":"claude_code.tool","spanId":"D2","attributes":[` +
	`{"key":"session.id","value":{"stringValue":"agent-sess"}},` +
	`{"key":"tool_name","value":{"stringValue":"Task"}},` +
	`{"key":"subagent_type","value":{"stringValue":"security-review-go"}}` +
	`]},` +
	// L3 — direct child of D2.
	`{"name":"claude_code.llm_request","spanId":"L3","parentSpanId":"D2","attributes":[` +
	`{"key":"session.id","value":{"stringValue":"agent-sess"}},` +
	`{"key":"gen_ai.request.model","value":{"stringValue":"claude-opus-4-7"}},` +
	`{"key":"input_tokens","value":{"stringValue":"200"}},` +
	`{"key":"output_tokens","value":{"stringValue":"100"}},` +
	`{"key":"duration_ms","value":{"stringValue":"800"}}` +
	`]},` +
	// T1 — tool span whose parent is D2 (a Read inside the security agent's work).
	`{"name":"claude_code.tool","spanId":"T1","parentSpanId":"D2","attributes":[` +
	`{"key":"session.id","value":{"stringValue":"agent-sess"}},` +
	`{"key":"tool_name","value":{"stringValue":"Read"}}` +
	`]},` +
	// L4 — child of T1, transitively descended from D2.
	`{"name":"claude_code.llm_request","spanId":"L4","parentSpanId":"T1","attributes":[` +
	`{"key":"session.id","value":{"stringValue":"agent-sess"}},` +
	`{"key":"gen_ai.request.model","value":{"stringValue":"claude-opus-4-7"}},` +
	`{"key":"input_tokens","value":{"stringValue":"80"}},` +
	`{"key":"output_tokens","value":{"stringValue":"40"}},` +
	`{"key":"duration_ms","value":{"stringValue":"400"}}` +
	`]}` +
	`]}]}]}`

// TestReport_AgentEconomics_OrchestratorRow: an llm_request with
// no parent (or a parent that doesn't lead to any dispatch span)
// is attributed to "(orchestrator)". L1 in fixtureAgentEconomics
// is parent-less; its 1000+500 tokens land in the orchestrator
// row.
func TestReport_AgentEconomics_OrchestratorRow(t *testing.T) {
	t.Parallel()
	rawDir := writeRawDir(t, fixtureAgentEconomics, "")
	outDir := t.TempDir()
	require.NoError(t, runReport("agent-sess", rawDir, outDir))

	report := readReport(t, outDir, "agent-sess")
	assert.Contains(t, report, "## LLM economics — by agent",
		"new section header must land")
	// Orchestrator row contains its 1000 input + 500 output.
	assert.Regexp(t, `\(orchestrator\).*1000.*500`, report,
		"orchestrator row must surface root-attached llm_request tokens")
}

// TestReport_AgentEconomics_DirectChild: an llm_request whose
// parentSpanId IS a dispatch span gets attributed to that
// dispatch's subagent_type. L2's parent is D1
// (provenance-review).
func TestReport_AgentEconomics_DirectChild(t *testing.T) {
	t.Parallel()
	rawDir := writeRawDir(t, fixtureAgentEconomics, "")
	outDir := t.TempDir()
	require.NoError(t, runReport("agent-sess", rawDir, outDir))

	report := readReport(t, outDir, "agent-sess")
	assert.Regexp(t, `provenance-review.*100.*50`, report,
		"direct child llm_request must attribute to its dispatch's subagent_type")
}

// TestReport_AgentEconomics_TransitiveAttribution: an llm_request
// whose parent is a NON-dispatch span (Read tool) but whose
// grandparent IS a dispatch span attributes to the grandparent's
// subagent_type. L4 → T1 → D2; L4's tokens belong to
// security-review-go.
//
// This is the load-bearing case: subagents in real Claude Code
// don't make LLM requests as direct children of the dispatch
// span — there's typically a tool/llm_request stack between. A
// naive "parent must be dispatch" check would miss these and
// silently undercount per-agent spend.
func TestReport_AgentEconomics_TransitiveAttribution(t *testing.T) {
	t.Parallel()
	rawDir := writeRawDir(t, fixtureAgentEconomics, "")
	outDir := t.TempDir()
	require.NoError(t, runReport("agent-sess", rawDir, outDir))

	report := readReport(t, outDir, "agent-sess")
	// security-review-go bucket should sum L3 (200+100) and L4 (80+40)
	// to 280 input + 140 output. Match both numbers in the same row.
	assert.Regexp(t, `security-review-go.*280.*140`, report,
		"transitive descendants of a dispatch span must attribute to that subagent_type")
}

// TestReport_AgentEconomics_TotalConservation: the TOTAL row
// equals the per-model TOTAL across the LLM-economics-by-model
// table. If the user's "shifted from one agent to another"
// concern materializes, this is the row whose number is supposed
// to stay constant — pinned by test so a future refactor can't
// silently break the conservation invariant.
func TestReport_AgentEconomics_TotalConservation(t *testing.T) {
	t.Parallel()
	rawDir := writeRawDir(t, fixtureAgentEconomics, "")
	outDir := t.TempDir()
	require.NoError(t, runReport("agent-sess", rawDir, outDir))

	report := readReport(t, outDir, "agent-sess")
	// Sum of orchestrator(1000+500) + provenance(100+50) +
	// security(280+140) = 1380 input + 690 output. The
	// per-model table for this fixture (single model) reports
	// the same numbers in its own TOTAL row.
	assert.Regexp(t, `\*\*TOTAL\*\*.*1380.*690`, report,
		"agent-economics TOTAL must equal sum of orchestrator + all subagent buckets")
}

// fixtureAgentEconomicsHooks pairs with fixtureAgentEconomics —
// two Agent hook events whose timestamps and order match the two
// dispatch spans (D1, D2) in the trace fixture. The descriptions
// here are the per-purpose labels the report's per-agent rollup
// MUST use as bucket keys, replacing the previous bucket-by-
// subagent_type behavior.
//
// The 1st hook event lines up with D1 (provenance-review →
// "Provenance review"). The 2nd lines up with D2
// (security-review-go → "Security audit (Go)"). Position-
// pairing is the join semantics: the Nth Agent hook event
// matches the Nth dispatch span in chronological order.
const fixtureAgentEconomicsHooks = `{"ts":"2026-04-27T15:00:00Z","event":"PreToolUse","session_id":"agent-sess","tool_use_id":"tu-D1","tool_name":"Agent","classification":"subagent_dispatch","detail":"Provenance review","tool_input_keys":["description","prompt","subagent_type"]}
{"ts":"2026-04-27T15:00:01Z","event":"PreToolUse","session_id":"agent-sess","tool_use_id":"tu-D2","tool_name":"Agent","classification":"subagent_dispatch","detail":"Security audit (Go)","tool_input_keys":["description","prompt","subagent_type"]}`

// writeRawDirAgent writes the agent-economics fixtures to a temp
// raw dir under the agent-sess session id (different from
// fixtureHooksA's sess-A). Kept separate so existing tests
// against fixtureTraces don't accidentally pick up these new
// hook events.
func writeRawDirAgent(t *testing.T, traces, hooks string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "traces.jsonl"), []byte(traces+"\n"), 0o644))
	if hooks != "" {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "hooks-agent-sess.jsonl"), []byte(hooks+"\n"), 0o644))
	}
	return dir
}

// TestReport_AgentEconomics_UsesHookDescriptionsAsBucketKeys:
// when the session's hooks file contains subagent_dispatch
// events, their `detail` field replaces subagent_type as the
// per-agent bucket key. This is the "general-purpose
// distinguishes nothing" fix — the Task tool dispatches all
// carry subagent_type=general-purpose; only the description
// distinguishes provenance from security from synthesis.
//
// Position-pairing: 1st dispatch span ↔ 1st Agent hook event,
// 2nd ↔ 2nd. Verified by asserting both descriptions appear as
// row labels in the per-agent table.
func TestReport_AgentEconomics_UsesHookDescriptionsAsBucketKeys(t *testing.T) {
	t.Parallel()
	rawDir := writeRawDirAgent(t, fixtureAgentEconomics, fixtureAgentEconomicsHooks)
	outDir := t.TempDir()
	require.NoError(t, runReport("agent-sess", rawDir, outDir))

	report := readReport(t, outDir, "agent-sess")
	// Both human-readable descriptions appear as row labels in
	// the per-agent table. (The raw subagent_type values like
	// "provenance-review" still appear in the unrelated
	// "Subagent dispatches" raw-counts section — that's a
	// different table; we don't assert against it.)
	agentSection := extractSection(report, "## LLM economics — by agent", "##")
	assert.Contains(t, agentSection, "Provenance review",
		"per-agent rollup must use the hook description for the first dispatch")
	assert.Contains(t, agentSection, "Security audit (Go)",
		"per-agent rollup must use the hook description for the second dispatch")
	// In the per-agent section specifically, the raw
	// subagent_type bucket key must NOT appear as a row label —
	// hook-derived labels supersede it for bucketing.
	assert.NotContains(t, agentSection, "provenance-review",
		"per-agent section must use description, not raw subagent_type, when hooks are present")
}

// extractSection slices the report.md text from `header` up to
// (but not including) the next line beginning with `nextPrefix`.
// Used by tests that need to assert ONLY against one section's
// content — the broader report contains many tables and a regex
// against the whole thing would otherwise match an unrelated
// occurrence.
func extractSection(report, header, nextPrefix string) string {
	_, rest, ok := strings.Cut(report, header)
	if !ok {
		return ""
	}
	if before, _, ok := strings.Cut(rest, "\n"+nextPrefix); ok {
		return before
	}
	return rest
}

// TestReport_AgentEconomics_FallsBackWhenHooksMissing: when no
// hooks file exists for the session, the per-agent rollup falls
// back to bucketing by subagent_type. Same guarantee as before
// the hook-description integration — historic sessions without
// the description-capturing hook still produce a meaningful
// rollup, just at lower fidelity.
func TestReport_AgentEconomics_FallsBackWhenHooksMissing(t *testing.T) {
	t.Parallel()
	rawDir := writeRawDirAgent(t, fixtureAgentEconomics, "") // no hooks
	outDir := t.TempDir()
	require.NoError(t, runReport("agent-sess", rawDir, outDir))

	report := readReport(t, outDir, "agent-sess")
	// With no hook events, the rollup falls back to subagent_type
	// values from the dispatch spans. Both expected.
	assert.Contains(t, report, "provenance-review",
		"missing hooks must fall back to subagent_type from the dispatch span")
	assert.Contains(t, report, "security-review-go",
		"missing hooks must fall back to subagent_type for the second dispatch too")
}

// TestReport_AgentEconomics_FallsBackPerDispatchWhenHookCountMismatch:
// hook events and dispatch spans don't have to be 1:1 in
// principle — receiver may have started mid-session, or hooks
// may not fire on a particular dispatch path. Position-pairing
// uses min(N_dispatches, N_hook_events); excess dispatches fall
// back to their subagent_type, excess hook events are ignored
// (they won't have a span to attribute to anyway).
func TestReport_AgentEconomics_FallsBackPerDispatchWhenHookCountMismatch(t *testing.T) {
	t.Parallel()
	// Only ONE hook event for TWO dispatches.
	hooksOneEvent := `{"ts":"2026-04-27T15:00:00Z","event":"PreToolUse","session_id":"agent-sess","tool_use_id":"tu-D1","tool_name":"Agent","classification":"subagent_dispatch","detail":"Provenance review","tool_input_keys":["description","prompt","subagent_type"]}`
	rawDir := writeRawDirAgent(t, fixtureAgentEconomics, hooksOneEvent)
	outDir := t.TempDir()
	require.NoError(t, runReport("agent-sess", rawDir, outDir))

	report := readReport(t, outDir, "agent-sess")
	// 1st dispatch (D1) gets its description.
	assert.Contains(t, report, "Provenance review",
		"first dispatch pairs with its hook event by position")
	// 2nd dispatch (D2) falls back to subagent_type because no
	// hook event remains.
	assert.Contains(t, report, "security-review-go",
		"second dispatch falls back to subagent_type when hook events run out")
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
