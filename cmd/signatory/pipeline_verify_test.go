package main

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/exchange"
	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/store"

	_ "modernc.org/sqlite"
)

// TestPipelineVerify_AllLanded verifies that when every expected
// analyst has ingested output, verify returns status
// "ready_for_synthesis" with no missing entries.
func TestPipelineVerify_AllLanded(t *testing.T) {
	t.Parallel()

	globals := testGlobals(t)
	ctx := t.Context()

	// Set up an analysis session with two expected analysts.
	s, err := globals.OpenStore(ctx)
	require.NoError(t, err)
	defer s.Close()

	sess := newTestAnalysisSession(t, s,
		"https://github.com/JedWatson/classnames",
		[]string{"signatory-security-v1", "signatory-provenance-v1"},
	)

	// Simulate both analysts landing output.
	ingestTestOutput(t, s, sess.ID, "signatory-security-v1")
	ingestTestOutput(t, s, sess.ID, "signatory-provenance-v1")

	var stdout bytes.Buffer
	cmd := &PipelineVerifyCmd{
		SessionID: sess.ID,
		Stdout:    &stdout,
	}
	require.NoError(t, cmd.Run(globals))

	var result VerifyResult
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &result),
		"stdout must be valid JSON; got: %s", stdout.String())

	assert.Equal(t, "ready_for_synthesis", result.Status)
	assert.ElementsMatch(t,
		[]string{"signatory-security-v1", "signatory-provenance-v1"},
		result.Expected)
	assert.ElementsMatch(t,
		[]string{"signatory-security-v1", "signatory-provenance-v1"},
		result.Landed)
	assert.Empty(t, result.Missing)
	assert.Len(t, result.OutputIDs, 2,
		"output_ids map must have one entry per landed analyst")
}

// TestPipelineVerify_MissingAnalyst verifies that when one expected
// analyst hasn't landed, verify returns "missing_analysts" with the
// missing role named.
func TestPipelineVerify_MissingAnalyst(t *testing.T) {
	t.Parallel()

	globals := testGlobals(t)
	ctx := t.Context()

	s, err := globals.OpenStore(ctx)
	require.NoError(t, err)
	defer s.Close()

	sess := newTestAnalysisSession(t, s,
		"https://github.com/JedWatson/classnames",
		[]string{"signatory-security-v1", "signatory-provenance-v1"},
	)

	// Only security lands.
	ingestTestOutput(t, s, sess.ID, "signatory-security-v1")

	var stdout bytes.Buffer
	cmd := &PipelineVerifyCmd{
		SessionID: sess.ID,
		Stdout:    &stdout,
	}
	require.NoError(t, cmd.Run(globals))

	var result VerifyResult
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &result))

	assert.Equal(t, "missing_analysts", result.Status)
	assert.Equal(t, []string{"signatory-provenance-v1"}, result.Missing)
	assert.Equal(t, []string{"signatory-security-v1"}, result.Landed)
}

// TestPipelineVerify_SessionNotFound verifies that a bogus session
// ID produces a clear error.
func TestPipelineVerify_SessionNotFound(t *testing.T) {
	t.Parallel()

	globals := testGlobals(t)

	var stdout bytes.Buffer
	cmd := &PipelineVerifyCmd{
		SessionID: "00000000-0000-0000-0000-000000000000",
		Stdout:    &stdout,
	}

	err := cmd.Run(globals)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
	assert.Empty(t, stdout.String())
}

// --- test helpers --------------------------------------------------------

// newTestAnalysisSession creates an in_progress analysis session in
// the store. Returns the session for callers that need the ID.
func newTestAnalysisSession(
	t *testing.T,
	s store.Store,
	target string,
	expectedAnalysts []string,
) *profile.AnalysisSession {
	t.Helper()
	ctx := t.Context()

	entity, err := ensureEntity(ctx, s, target)
	require.NoError(t, err)

	sess := &profile.AnalysisSession{
		ID:               uuid.NewString(),
		EntityID:         entity.ID,
		TargetURI:        target,
		InvokedBy:        "team:test",
		ExpectedAnalysts: expectedAnalysts,
		StartedAt:        time.Now().UTC(),
		Status:           profile.AnalysisSessionInProgress,
	}
	require.NoError(t, s.CreateAnalysisSession(ctx, sess))
	return sess
}

// ingestTestOutput creates a minimal analyst_outputs row linked to
// the given analysis session. The output is a bare-minimum v1 stub
// — just enough to satisfy exchange.Validate and the store's FK and
// NOT-NULL constraints.
func ingestTestOutput(
	t *testing.T,
	s store.Store,
	analysisSessionID, analystID string,
) {
	t.Helper()
	ctx := t.Context()

	out := &exchange.AnalystOutput{
		Attribution: exchange.AgentAttribution{
			AnalystID: analystID,
			Model:     "test-model",
			InvokedAt: time.Now().UTC().Format(time.RFC3339),
			Round:     1,
		},
		Target: "https://github.com/JedWatson/classnames",
		Conclusions: []exchange.Conclusion{{
			ID:        "T001",
			Verdict:   "test stub",
			Rationale: "minimal output for pipeline verify test",
			Severity:  exchange.Severity{Default: exchange.SeverityInformational},
			Category:  "test",
		}},
	}

	_, err := s.IngestAnalystOutput(ctx, out, "",
		store.WithAnalysisSession(analysisSessionID))
	require.NoError(t, err)
}
