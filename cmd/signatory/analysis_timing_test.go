package main

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/store"
)

// Timing tests: two layers.
//
//  1. computeSessionTiming — the pure derivation, exercised with
//     hand-built fixtures so every branch (no outputs, malformed
//     timestamps, clock skew, synthesis presence/absence) runs
//     without standing up SQLite.
//
//  2. AnalysisTimingCmd — CLI integration against a real SQLite
//     store, mirroring the analysis_test.go style.

// --- computeSessionTiming (pure-function tests) ----------------------------

// buildSession assembles a session fixture at a fixed epoch so
// absolute math in the tests is easy to reason about.
func buildSession(id string, started time.Time, ended *time.Time, expected []string) *profile.AnalysisSession {
	return &profile.AnalysisSession{
		ID:               id,
		EntityID:         "ent-" + id,
		TargetURI:        "pkg:npm/timing-test",
		StartedAt:        started,
		EndedAt:          ended,
		Status:           profile.AnalysisSessionInProgress,
		ExpectedAnalysts: expected,
	}
}

// buildOutput returns a minimally-valid summary row for
// computeSessionTiming (the derivation only reads InvokedAt,
// IngestedAt, AnalystID, OutputID).
func buildOutput(analystID, outputID, invokedAt, ingestedAt string) store.AnalystOutputSummary {
	return store.AnalystOutputSummary{
		OutputID:   outputID,
		AnalystID:  analystID,
		InvokedAt:  invokedAt,
		IngestedAt: ingestedAt,
	}
}

func TestComputeSessionTiming_NoOutputs(t *testing.T) {
	// Open session with no outputs: every latency field is nil,
	// missing-analyst list surfaces the dispatched analysts.
	started := mustParseTime(t, "2026-04-23T15:00:00Z")
	sess := buildSession("s1", started, nil, []string{"external-sec-v1"})

	got := computeSessionTiming(sess, nil)

	assert.Empty(t, got.Analysts)
	assert.Nil(t, got.SessionWallMS)
	assert.Nil(t, got.TimeToFirstOutputMS)
	assert.Nil(t, got.TimeToLastAnalystMS)
	assert.Nil(t, got.TimeToSynthesisMS)
	assert.Nil(t, got.SynthesisToCloseMS)
	assert.Equal(t, []string{"external-sec-v1"}, got.Missing)
	assert.Empty(t, got.Unexpected)
}

func TestComputeSessionTiming_HappyPath(t *testing.T) {
	// Full flow: two analysts, synthesis, session closes. Verify
	// each derived duration matches hand-computed arithmetic.
	started := mustParseTime(t, "2026-04-23T15:00:00Z")
	ended := mustParseTime(t, "2026-04-23T15:03:10Z")
	sess := buildSession("s2", started, &ended,
		[]string{"external-sec-v1", "signatory-provenance"})

	outputs := []store.AnalystOutputSummary{
		buildOutput("external-sec-v1", "o1",
			"2026-04-23T15:00:10Z",
			"2026-04-23T15:02:00Z"), // 1m50s agent, 2m session
		buildOutput("signatory-provenance", "o2",
			"2026-04-23T15:00:10Z",
			"2026-04-23T15:02:30Z"),
		buildOutput("signatory-synthesis-v1", "o3",
			"2026-04-23T15:02:35Z",
			"2026-04-23T15:03:05Z"),
	}

	got := computeSessionTiming(sess, outputs)
	require.Len(t, got.Analysts, 3)

	first := got.Analysts[0]
	assert.Equal(t, "external-sec-v1", first.AnalystID)
	requirePtrEqual(t, int64(110_000), first.AgentWallMS, "1m50s")
	requirePtrEqual(t, int64(120_000), first.SessionWallMS, "2m")
	assert.False(t, first.IsSynthesis)

	assert.True(t, got.Analysts[2].IsSynthesis, "signatory-synthesis-* must be flagged")

	requirePtrEqual(t, int64(190_000), got.SessionWallMS, "session total 3m10s")
	requirePtrEqual(t, int64(120_000), got.TimeToFirstOutputMS, "first output at 2m")
	requirePtrEqual(t, int64(150_000), got.TimeToLastAnalystMS, "last non-synth at 2m30s")
	requirePtrEqual(t, int64(185_000), got.TimeToSynthesisMS, "synthesis at 3m5s")
	requirePtrEqual(t, int64(5_000), got.SynthesisToCloseMS, "synth→close 5s")

	assert.Empty(t, got.Missing)
	assert.Empty(t, got.Unexpected)
}

func TestComputeSessionTiming_UnparseableInvokedAt(t *testing.T) {
	// Graceful degradation: bad invoked_at drops agent_wall on that
	// row but doesn't break session-wall (ingested_at still parses)
	// or session-level aggregates.
	started := mustParseTime(t, "2026-04-23T15:00:00Z")
	sess := buildSession("s3", started, nil, nil)
	outputs := []store.AnalystOutputSummary{
		buildOutput("external-sec-v1", "o1",
			"garbage-timestamp",
			"2026-04-23T15:02:00Z"),
	}

	got := computeSessionTiming(sess, outputs)
	require.Len(t, got.Analysts, 1)
	assert.Nil(t, got.Analysts[0].AgentWallMS,
		"agent_wall must be nil when invoked_at is unparseable")
	requirePtrEqual(t, int64(120_000), got.Analysts[0].SessionWallMS,
		"session_wall still computes from ingested_at")
	assert.Contains(t, got.Analysts[0].WallNotes, "unparseable")
	requirePtrEqual(t, int64(120_000), got.TimeToFirstOutputMS,
		"session-level aggregates still compute")
}

func TestComputeSessionTiming_ClockSkew(t *testing.T) {
	// invoked_at > ingested_at — drop agent_wall, flag it.
	started := mustParseTime(t, "2026-04-23T15:00:00Z")
	sess := buildSession("s4", started, nil, nil)
	outputs := []store.AnalystOutputSummary{
		buildOutput("external-sec-v1", "o1",
			"2026-04-23T15:02:10Z", // AFTER ingested
			"2026-04-23T15:02:00Z"),
	}

	got := computeSessionTiming(sess, outputs)
	require.Len(t, got.Analysts, 1)
	assert.Nil(t, got.Analysts[0].AgentWallMS)
	assert.Contains(t, got.Analysts[0].WallNotes, "clock skew")
}

func TestComputeSessionTiming_UnparseableIngestedAt(t *testing.T) {
	// ingested_at is server-stamped so this shouldn't happen, but
	// defensive coverage keeps the pure function from panicking on
	// legacy rows.
	started := mustParseTime(t, "2026-04-23T15:00:00Z")
	sess := buildSession("s5", started, nil, nil)
	outputs := []store.AnalystOutputSummary{
		buildOutput("external-sec-v1", "o1",
			"2026-04-23T15:00:10Z",
			"not-a-timestamp"),
	}

	got := computeSessionTiming(sess, outputs)
	require.Len(t, got.Analysts, 1)
	assert.Nil(t, got.Analysts[0].SessionWallMS,
		"session_wall must be nil when ingested_at is unparseable")
	assert.Nil(t, got.TimeToFirstOutputMS,
		"session-level aggregates must exclude rows with unparseable ingested_at")
	assert.Contains(t, got.Analysts[0].WallNotes, "ingested_at")
}

func TestComputeSessionTiming_OrderingByIngestedAt(t *testing.T) {
	// Renderer walks chronologically. Verify the stable sort is
	// correct even when the store returns an arbitrary order.
	started := mustParseTime(t, "2026-04-23T15:00:00Z")
	sess := buildSession("s6", started, nil, nil)
	outputs := []store.AnalystOutputSummary{
		buildOutput("third", "o3", "2026-04-23T15:00:00Z", "2026-04-23T15:03:00Z"),
		buildOutput("first", "o1", "2026-04-23T15:00:00Z", "2026-04-23T15:01:00Z"),
		buildOutput("second", "o2", "2026-04-23T15:00:00Z", "2026-04-23T15:02:00Z"),
	}

	got := computeSessionTiming(sess, outputs)
	require.Len(t, got.Analysts, 3)
	assert.Equal(t, "first", got.Analysts[0].AnalystID)
	assert.Equal(t, "second", got.Analysts[1].AnalystID)
	assert.Equal(t, "third", got.Analysts[2].AnalystID)
}

func TestComputeSessionTiming_SynthesisOnly(t *testing.T) {
	// Only synthesis landed. time_to_last_analyst stays nil
	// (no non-synth outputs). First-output and synthesis collapse.
	started := mustParseTime(t, "2026-04-23T15:00:00Z")
	ended := mustParseTime(t, "2026-04-23T15:01:00Z")
	sess := buildSession("s7", started, &ended, nil)
	outputs := []store.AnalystOutputSummary{
		buildOutput("signatory-synthesis-v1", "o1",
			"2026-04-23T15:00:10Z",
			"2026-04-23T15:00:55Z"),
	}

	got := computeSessionTiming(sess, outputs)
	requirePtrEqual(t, int64(55_000), got.TimeToFirstOutputMS, "first output 55s")
	requirePtrEqual(t, int64(55_000), got.TimeToSynthesisMS, "synthesis 55s")
	assert.Nil(t, got.TimeToLastAnalystMS,
		"with no non-synth outputs, time_to_last_analyst must be nil")
	requirePtrEqual(t, int64(5_000), got.SynthesisToCloseMS, "synth→close 5s")
}

func TestComputeSessionTiming_UnexpectedLandedAndMissing(t *testing.T) {
	started := mustParseTime(t, "2026-04-23T15:00:00Z")
	sess := buildSession("s8", started, nil,
		[]string{"external-sec-v1", "signatory-provenance"})
	outputs := []store.AnalystOutputSummary{
		buildOutput("external-sec-v1", "o1",
			"2026-04-23T15:00:10Z",
			"2026-04-23T15:02:00Z"),
		buildOutput("unexpected-analyst-v1", "o2",
			"2026-04-23T15:00:10Z",
			"2026-04-23T15:02:30Z"),
	}

	got := computeSessionTiming(sess, outputs)
	assert.Equal(t, []string{"signatory-provenance"}, got.Missing)
	assert.Equal(t, []string{"unexpected-analyst-v1"}, got.Unexpected)
}

// --- CLI integration tests (AnalysisTimingCmd) -----------------------------

func TestAnalysisTiming_UnknownSession(t *testing.T) {
	g := newTestGlobals(t)
	cmd := &AnalysisTimingCmd{
		SessionID: uuid.NewString(),
		Stdout:    &bytes.Buffer{},
	}
	err := cmd.Run(g)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUsage)
}

func TestAnalysisTiming_HappyPath(t *testing.T) {
	// End-to-end: begin → ingest two analysts + synthesis → end.
	// Content-level timing arithmetic is covered by the pure-function
	// tests; this test anchors the CLI renderer against real SQLite.
	g := newTestGlobals(t)
	sessionID := beginSessionWithExpectedViaCmd(t, g,
		"pkg:npm/timing-happy",
		[]string{"external-sec-v1", "signatory-provenance"})

	ingestAnalystForSession(t, g, "pkg:npm/timing-happy", "external-sec-v1", sessionID)
	ingestAnalystForSession(t, g, "pkg:npm/timing-happy", "signatory-provenance", sessionID)
	synthID := ingestSynthesisForSession(t, g, "pkg:npm/timing-happy", sessionID)

	require.NoError(t, (&AnalysisEndCmd{
		SessionID:         sessionID,
		Status:            "completed",
		SynthesisOutputID: synthID,
		Stdout:            &bytes.Buffer{},
	}).Run(g))

	var stdout bytes.Buffer
	cmd := &AnalysisTimingCmd{SessionID: sessionID, Stdout: &stdout}
	require.NoError(t, cmd.Run(g))

	out := stdout.String()
	assert.Contains(t, out, "[completed]")
	assert.Contains(t, out, "external-sec-v1")
	assert.Contains(t, out, "signatory-provenance")
	assert.Contains(t, out, "[synthesis]")
	assert.Contains(t, out, "latency decomposition:")
	assert.Contains(t, out, "begin → first output")
	assert.Contains(t, out, "begin → synthesis")
	assert.Contains(t, out, "total (begin → close)")
}

func TestAnalysisTiming_OpenSessionRendersGracefully(t *testing.T) {
	// Open session: no ended_at, no negative durations, the "total"
	// line switches to the in-progress sentinel.
	g := newTestGlobals(t)
	sessionID := beginSessionViaCmd(t, g, "pkg:npm/timing-open")

	var stdout bytes.Buffer
	cmd := &AnalysisTimingCmd{SessionID: sessionID, Stdout: &stdout}
	require.NoError(t, cmd.Run(g))
	out := stdout.String()
	assert.Contains(t, out, "[in_progress]")
	assert.Contains(t, out, "session still in_progress")
}

func TestAnalysisTiming_JSONOutput(t *testing.T) {
	// --json decodes cleanly. Pointer-valued duration fields are
	// present (not nil) when the source timestamps parsed — the
	// value itself may be 0 for sub-second runs, which is legitimate.
	g := newTestGlobals(t)
	sessionID := beginSessionWithExpectedViaCmd(t, g,
		"pkg:npm/timing-json", []string{"external-sec-v1"})
	ingestAnalystForSession(t, g, "pkg:npm/timing-json", "external-sec-v1", sessionID)

	var stdout bytes.Buffer
	cmd := &AnalysisTimingCmd{SessionID: sessionID, JSON: true, Stdout: &stdout}
	require.NoError(t, cmd.Run(g))

	var got SessionTiming
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &got))
	assert.Equal(t, sessionID, got.Session.ID)
	require.Len(t, got.Analysts, 1)
	require.NotNil(t, got.Analysts[0].SessionWallMS,
		"parseable ingested_at → SessionWallMS must be set")
	assert.GreaterOrEqual(t, *got.Analysts[0].SessionWallMS, int64(0))
}

func TestAnalysisTiming_JSONOmitsUnsetFields(t *testing.T) {
	// Open session with no outputs: every session-level latency
	// field is nil + omitempty → absent from the wire. Consumers
	// distinguish "unset" from "zero" by key presence.
	g := newTestGlobals(t)
	sessionID := beginSessionViaCmd(t, g, "pkg:npm/timing-omit")

	var stdout bytes.Buffer
	cmd := &AnalysisTimingCmd{SessionID: sessionID, JSON: true, Stdout: &stdout}
	require.NoError(t, cmd.Run(g))

	raw := stdout.String()
	assert.NotContains(t, raw, `"session_wall_ms"`)
	assert.NotContains(t, raw, `"time_to_first_output_ms"`)
	assert.NotContains(t, raw, `"time_to_synthesis_ms"`)
}

// --- small test helpers ----------------------------------------------------

// mustParseTime parses an RFC3339 string or fails the test.
func mustParseTime(t *testing.T, s string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, s)
	require.NoError(t, err)
	return parsed
}

// requirePtrEqual asserts a *int64 is non-nil and dereferences to
// the expected value. Shortcut for the pointer dance that would
// otherwise repeat across the pure-function tests.
func requirePtrEqual(t *testing.T, want int64, got *int64, label string) {
	t.Helper()
	require.NotNil(t, got, "%s: expected %d, got nil", label, want)
	assert.Equal(t, want, *got, label)
}
