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
