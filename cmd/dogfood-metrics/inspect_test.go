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

// writeTracesFile is the test-fixture helper: drops a traces.jsonl
// file into a fresh temp dir and returns the dir. Each input line
// is expected to be one JSON-encoded ExportTraceServiceRequest-shaped
// batch (matching the receiver's output shape).
func writeTracesFile(t *testing.T, lines ...string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "traces.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600))
	return dir
}

// TestInspect_ReportsSpanNameDistribution covers the basic counting:
// given two spans with different names against the same session, the
// inspect output must surface both names with their counts.
func TestInspect_ReportsSpanNameDistribution(t *testing.T) {
	t.Parallel()
	dir := writeTracesFile(t,
		`{"resourceSpans":[{"resource":{"attributes":[{"key":"session.id","value":{"stringValue":"S1"}}]},"scopeSpans":[{"spans":[`+
			`{"name":"claude_code.llm_request","attributes":[]},`+
			`{"name":"claude_code.tool","attributes":[]},`+
			`{"name":"claude_code.tool","attributes":[]}`+
			`]}]}]}`,
	)

	var out bytes.Buffer
	require.NoError(t, runInspect("S1", dir, &out))

	got := out.String()
	assert.Contains(t, got, "claude_code.llm_request",
		"span name distribution must list every observed name")
	assert.Contains(t, got, "claude_code.tool")
	// Counts surface in the rendered table.
	assert.Contains(t, got, "| claude_code.tool | 2 |")
	assert.Contains(t, got, "| claude_code.llm_request | 1 |")
}

// TestInspect_FilterDiagnosis_AllAttributesPresent: sample span has
// session.id at resource level, name in the report's expected set,
// and a query_source attr. All three filter checks pass; the
// diagnosis line says spans pass every filter.
func TestInspect_FilterDiagnosis_AllAttributesPresent(t *testing.T) {
	t.Parallel()
	dir := writeTracesFile(t,
		`{"resourceSpans":[{"resource":{"attributes":[{"key":"session.id","value":{"stringValue":"S1"}}]},"scopeSpans":[{"spans":[`+
			`{"name":"claude_code.llm_request","attributes":[{"key":"query_source","value":{"stringValue":"agent-a"}}]}`+
			`]}]}]}`,
	)

	var out bytes.Buffer
	require.NoError(t, runInspect("S1", dir, &out))
	got := out.String()

	// All three filter steps the report applies should report 1 span.
	assert.Regexp(t, `session\.id resource attr matches.*1`, got,
		"diagnosis must report exact session-match span count")
	assert.Regexp(t, `name in.*1`, got,
		"diagnosis must report span-name-filter pass count")
	assert.Regexp(t, `query_source attribute present.*1`, got,
		"diagnosis must report attribute-presence count")
}

// TestInspect_FilterDiagnosis_NameMismatch: session matches but the
// span's name is unfamiliar. Inspect must surface this so the
// operator sees "the data is here, the report's name filter is
// dropping it." Reproduces the dogfood-surfaced shape where
// traces.jsonl had 553 lines but the report said "no trace spans
// recorded."
func TestInspect_FilterDiagnosis_NameMismatch(t *testing.T) {
	t.Parallel()
	dir := writeTracesFile(t,
		`{"resourceSpans":[{"resource":{"attributes":[{"key":"session.id","value":{"stringValue":"S1"}}]},"scopeSpans":[{"spans":[`+
			`{"name":"some_other_span_name","attributes":[]}`+
			`]}]}]}`,
	)

	var out bytes.Buffer
	require.NoError(t, runInspect("S1", dir, &out))
	got := out.String()

	// Session-match: 1; name-filter pass: 0. The asymmetry is the
	// diagnosis: data IS for this session, but the report's name
	// filter excludes it.
	assert.Regexp(t, `session\.id resource attr matches.*1`, got)
	assert.Regexp(t, `name in.*0`, got)
	assert.Contains(t, got, "some_other_span_name",
		"unrecognized name must appear in the distribution table so operator sees what's actually there")
}

// TestInspect_SessionIDOnSpanInsteadOfResource: a Claude Code build
// where session.id is a SPAN attribute rather than a RESOURCE
// attribute. The report's filter (which keys on resource session.id)
// would drop these spans entirely. Inspect surfaces this by
// reporting zero resource matches but non-zero span attributes
// containing session.id.
//
// A real-world failure mode worth testing — OTEL SDKs in different
// agent generations have moved this attribute around.
func TestInspect_SessionIDOnSpanInsteadOfResource(t *testing.T) {
	t.Parallel()
	dir := writeTracesFile(t,
		`{"resourceSpans":[{"resource":{"attributes":[]},"scopeSpans":[{"spans":[`+
			`{"name":"claude_code.tool","attributes":[{"key":"session.id","value":{"stringValue":"S1"}}]}`+
			`]}]}]}`,
	)

	var out bytes.Buffer
	require.NoError(t, runInspect("S1", dir, &out))
	got := out.String()

	// Resource match is 0 — that's the bug the inspect tool surfaces.
	assert.Regexp(t, `session\.id resource attr matches.*0`, got)
	// But "session.id present at SPAN level" should reach 1, the
	// signal that explains where the data went.
	assert.Regexp(t, `session\.id appears as a SPAN attribute.*1`, got,
		"inspect must report the on-span fallback so operator sees data is present, just at the wrong attribute level")
}

// TestInspect_AcrossSessions: traces.jsonl carries spans from
// multiple sessions. The inspect tool reports both totals (across
// the whole file) and per-session breakdowns, so the operator can
// distinguish "no data for this session" from "no data at all."
func TestInspect_AcrossSessions(t *testing.T) {
	t.Parallel()
	dir := writeTracesFile(t,
		`{"resourceSpans":[{"resource":{"attributes":[{"key":"session.id","value":{"stringValue":"OTHER"}}]},"scopeSpans":[{"spans":[{"name":"claude_code.tool","attributes":[]}]}]}]}`,
		`{"resourceSpans":[{"resource":{"attributes":[{"key":"session.id","value":{"stringValue":"S1"}}]},"scopeSpans":[{"spans":[{"name":"claude_code.tool","attributes":[]}]}]}]}`,
	)

	var out bytes.Buffer
	require.NoError(t, runInspect("S1", dir, &out))
	got := out.String()

	// Header reports total spans seen (across all sessions) and
	// matching-this-session count, so a "no data for me" vs "file
	// is empty" distinction is obvious.
	assert.Regexp(t, `total spans seen across all sessions.*2`, got)
	assert.Regexp(t, `spans matching session S1.*1`, got)
}

// TestInspect_SpanLevelSessionMatch_ContributesToDistributions —
// when session.id appears on the span (not the resource), the spans
// must still contribute to the name distribution + attribute-keys
// tables. Otherwise the operator sees "filter diagnosis says 283
// spans match via span-attribute fallback" but every other table is
// empty, which is a worse diagnostic experience than "no data."
//
// Reproduces the live shape on session 40366063: 283 spans matched
// only via span-level session.id; the original inspect impl reported
// the count but couldn't show name distribution because those spans
// failed the resource-level gate.
func TestInspect_SpanLevelSessionMatch_ContributesToDistributions(t *testing.T) {
	t.Parallel()
	dir := writeTracesFile(t,
		`{"resourceSpans":[{"resource":{"attributes":[]},"scopeSpans":[{"spans":[`+
			`{"name":"claude_code.tool","attributes":[{"key":"session.id","value":{"stringValue":"S1"}},{"key":"tool.name","value":{"stringValue":"Bash"}}]},`+
			`{"name":"claude_code.tool","attributes":[{"key":"session.id","value":{"stringValue":"S1"}},{"key":"tool.name","value":{"stringValue":"Read"}}]},`+
			`{"name":"some_other_span","attributes":[{"key":"session.id","value":{"stringValue":"S1"}}]}`+
			`]}]}]}`,
	)

	var out bytes.Buffer
	require.NoError(t, runInspect("S1", dir, &out))
	got := out.String()

	// All 3 spans match this session via the span-attribute fallback;
	// distribution tables must reflect that, not be empty.
	assert.Regexp(t, `spans matching session S1.*3`, got,
		"span-level session matches must count toward the matching-spans header")
	assert.Contains(t, got, "| claude_code.tool | 2 |",
		"span name distribution must include the 2 claude_code.tool spans matched via span-level session.id")
	assert.Contains(t, got, "| some_other_span | 1 |")
	// Attribute keys table for claude_code.tool must include
	// tool.name, even though the spans were matched at span level.
	assert.Contains(t, got, "tool.name",
		"attribute-key table must surface the keys present on span-level-matched spans")
}

// TestInspect_MissingFile_ReportsCleanly: the inspect tool surfaces
// missing-file as a diagnosis, not a stack trace.
func TestInspect_MissingFile_ReportsCleanly(t *testing.T) {
	t.Parallel()
	dir := t.TempDir() // empty — no traces.jsonl
	var out bytes.Buffer
	err := runInspect("S1", dir, &out)
	require.Error(t, err, "missing traces.jsonl is operator-actionable; surface as error not silent")
	assert.Contains(t, err.Error(), "traces.jsonl",
		"error must name the file the inspector tried to open")
}

// TestInspect_TraceCorrelation_ListsAllSessionsInFile: the inspect
// tool surfaces every session.id present in the trace stream, not
// just the requested one. Answers the operator question "are there
// other sessions in this file my report could run against?" without
// requiring a separate `list-sessions` invocation, AND lets the
// operator see whether a Task-tool dispatch forked into a fresh
// session.id (the hypothesis we tested 2026-04-28; in current
// Claude Code, dispatches do NOT fork — children share parent
// session.id).
func TestInspect_TraceCorrelation_ListsAllSessionsInFile(t *testing.T) {
	t.Parallel()
	dir := writeTracesFile(t,
		// Session A — 2 spans
		`{"resourceSpans":[{"resource":{"attributes":[{"key":"session.id","value":{"stringValue":"S1"}}]},"scopeSpans":[{"spans":[`+
			`{"name":"claude_code.tool","attributes":[]},`+
			`{"name":"claude_code.tool","attributes":[]}`+
			`]}]}]}`,
		// Session B — 1 span (different session.id, same trace file)
		`{"resourceSpans":[{"resource":{"attributes":[{"key":"session.id","value":{"stringValue":"OTHER"}}]},"scopeSpans":[{"spans":[`+
			`{"name":"claude_code.llm_request","attributes":[]}`+
			`]}]}]}`,
	)
	var out bytes.Buffer
	require.NoError(t, runInspect("S1", dir, &out))
	got := out.String()

	// The new section header should appear.
	assert.Contains(t, got, "## Trace correlation",
		"trace-correlation section is the new diagnostic surface")
	// Both sessions must be listed with their span counts so the
	// operator can pick the right one.
	assert.Contains(t, got, "| S1 | 2 |",
		"trace correlation must list the requested session with its span count")
	assert.Contains(t, got, "| OTHER | 1 |",
		"trace correlation must list other sessions in the file too")
}

// TestInspect_TraceCorrelation_DispatchSpanLinkage: when a
// claude_code.tool span carries subagent_type and other spans
// reference it via parentSpanId, inspect surfaces the linkage.
//
// The 2026-04-28 dogfood verification showed children share the
// parent's session.id (Task does NOT fork to a new session). This
// test pins that finding into the inspect output: the operator
// asking "did this dispatch fork or stay in the same session?"
// gets the answer from the diagnostic, not from running custom
// Python against the raw file.
func TestInspect_TraceCorrelation_DispatchSpanLinkage(t *testing.T) {
	t.Parallel()
	dir := writeTracesFile(t,
		`{"resourceSpans":[{"resource":{"attributes":[{"key":"session.id","value":{"stringValue":"S1"}}]},"scopeSpans":[{"spans":[`+
			// The dispatch span — has subagent_type, has its own spanId.
			`{"name":"claude_code.tool","spanId":"dispatch01","traceId":"trace0001","attributes":[`+
			`{"key":"subagent_type","value":{"stringValue":"provenance-review"}},`+
			`{"key":"tool_name","value":{"stringValue":"Task"}}`+
			`]},`+
			// Two children whose parentSpanId points to the dispatch.
			`{"name":"claude_code.llm_request","spanId":"child0001","parentSpanId":"dispatch01","traceId":"trace0001","attributes":[]},`+
			`{"name":"claude_code.tool","spanId":"child0002","parentSpanId":"dispatch01","traceId":"trace0001","attributes":[]}`+
			`]}]}]}`,
	)
	var out bytes.Buffer
	require.NoError(t, runInspect("S1", dir, &out))
	got := out.String()

	// The dispatch row surfaces subagent_type and child count. Both
	// data points are actionable: subagent_type tells the operator
	// which agent ran; child count tells them whether spans landed
	// for that dispatch at all.
	assert.Contains(t, got, "provenance-review",
		"dispatch row must surface subagent_type so the operator sees which agent's work this is")
	assert.Regexp(t, `provenance-review.*\| 2`, got,
		"dispatch row must report child-span count")
}

// TestInspect_TraceCorrelation_ReportsSessionContinuity: the
// dispatch-children check distinguishes "all children share parent
// session.id" (the current Claude Code shape) from "children fork
// to a different session.id" (the shape we'd need to handle if Task
// semantics changed). Pinning the distinction in a test means the
// inspect tool stays useful as a diagnostic across both shapes.
func TestInspect_TraceCorrelation_ReportsSessionContinuity(t *testing.T) {
	t.Parallel()
	dir := writeTracesFile(t,
		`{"resourceSpans":[{"resource":{"attributes":[{"key":"session.id","value":{"stringValue":"PARENT"}}]},"scopeSpans":[{"spans":[`+
			// Dispatch — session=PARENT
			`{"name":"claude_code.tool","spanId":"dispatchAA","traceId":"traceXX","attributes":[`+
			`{"key":"subagent_type","value":{"stringValue":"general-purpose"}},`+
			`{"key":"session.id","value":{"stringValue":"PARENT"}}`+
			`]},`+
			// Child stays in PARENT session.
			`{"name":"claude_code.llm_request","spanId":"childAA01","parentSpanId":"dispatchAA","traceId":"traceXX","attributes":[`+
			`{"key":"session.id","value":{"stringValue":"PARENT"}}`+
			`]}`+
			`]}]}]}`,
	)
	var out bytes.Buffer
	require.NoError(t, runInspect("PARENT", dir, &out))
	got := out.String()

	// "session continuity" copy in the row tells the operator at a
	// glance whether the children stayed in-session or forked. The
	// exact word is the contract a future change to Task semantics
	// would break loudly.
	assert.Contains(t, got, "same-session",
		"dispatch row must classify children as same-session vs forked so operator knows whether per-agent attribution requires walking spans or running against a different session.id")
}
