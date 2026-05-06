package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/exchange"
	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/store"

	_ "modernc.org/sqlite"
)

// TestPipelineClose_HappyPath verifies the complete close flow:
// given a session with both analysts and a synthesis, --yes
// accepts the proposed posture and closes the session. The JSON
// output contains the synthesis output_id, the proposed tier, and
// a terminal status.
func TestPipelineClose_HappyPath(t *testing.T) {
	t.Parallel()

	globals := testGlobals(t)
	ctx := t.Context()

	s, err := globals.OpenStore(ctx)
	require.NoError(t, err)
	defer s.Close()

	sess := newTestAnalysisSession(t, s,
		"https://github.com/JedWatson/classnames",
		[]string{"signatory-security-v1", "signatory-provenance-v1", "signatory-synthesis-v1"},
	)

	// Land both analysts and the synthesist.
	ingestTestOutput(t, s, sess.ID, "signatory-security-v1")
	ingestTestOutput(t, s, sess.ID, "signatory-provenance-v1")
	synthOutputID := ingestTestSynthesis(t, s, sess.ID)

	var stdout bytes.Buffer
	cmd := &PipelineCloseCmd{
		SessionID: sess.ID,
		Status:    "completed",
		Yes:       true,
		Stdout:    &stdout,
		Stderr:    &bytes.Buffer{},
	}
	require.NoError(t, cmd.Run(globals))

	var result CloseResult
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &result),
		"stdout must be valid JSON; got: %s", stdout.String())

	assert.Equal(t, "closed", result.Status)
	assert.Equal(t, synthOutputID, result.SynthesisOutputID)
	assert.Equal(t, "trusted-for-now", result.ProposedTier)
	assert.True(t, result.PostureAccepted)

	// Verify session is actually closed in the store.
	closed, err := s.GetAnalysisSession(ctx, sess.ID)
	require.NoError(t, err)
	assert.Equal(t, profile.AnalysisSessionCompleted, closed.Status)
	assert.Equal(t, synthOutputID, closed.SynthesisOutputID)
}

// TestPipelineClose_DryRun verifies that without --yes, close
// returns the proposal but does NOT accept posture or close the
// session. The orchestrator presents this to the user for
// confirmation.
func TestPipelineClose_DryRun(t *testing.T) {
	t.Parallel()

	globals := testGlobals(t)
	ctx := t.Context()

	s, err := globals.OpenStore(ctx)
	require.NoError(t, err)
	defer s.Close()

	sess := newTestAnalysisSession(t, s,
		"https://github.com/JedWatson/classnames",
		[]string{"signatory-security-v1", "signatory-provenance-v1", "signatory-synthesis-v1"},
	)

	ingestTestOutput(t, s, sess.ID, "signatory-security-v1")
	ingestTestOutput(t, s, sess.ID, "signatory-provenance-v1")
	synthOutputID := ingestTestSynthesis(t, s, sess.ID)

	var stdout bytes.Buffer
	cmd := &PipelineCloseCmd{
		SessionID: sess.ID,
		Status:    "completed",
		Yes:       false, // dry run — no confirmation
		Stdout:    &stdout,
		Stderr:    &bytes.Buffer{},
	}
	require.NoError(t, cmd.Run(globals))

	var result CloseResult
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &result))

	assert.Equal(t, "proposal", result.Status,
		"without --yes, status must be 'proposal' not 'closed'")
	assert.Equal(t, synthOutputID, result.SynthesisOutputID)
	assert.Equal(t, "trusted-for-now", result.ProposedTier)
	assert.False(t, result.PostureAccepted,
		"posture must NOT be accepted without --yes")

	// Session must still be in_progress.
	open, err := s.GetAnalysisSession(ctx, sess.ID)
	require.NoError(t, err)
	assert.Equal(t, profile.AnalysisSessionInProgress, open.Status)
}

// TestPipelineClose_NoSynthesis verifies that closing a session
// with no synthesis output produces a clear error naming the
// missing role.
func TestPipelineClose_NoSynthesis(t *testing.T) {
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

	// Only analysts, no synthesist.
	ingestTestOutput(t, s, sess.ID, "signatory-security-v1")
	ingestTestOutput(t, s, sess.ID, "signatory-provenance-v1")

	var stdout bytes.Buffer
	cmd := &PipelineCloseCmd{
		SessionID: sess.ID,
		Status:    "completed",
		Yes:       true,
		Stdout:    &stdout,
		Stderr:    &bytes.Buffer{},
	}

	err = cmd.Run(globals)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "synthesis",
		"error must name the missing synthesis output")
	assert.Empty(t, stdout.String())
}

// TestPipelineClose_SessionNotFound verifies that a bogus session
// ID produces a clear "not found" error.
func TestPipelineClose_SessionNotFound(t *testing.T) {
	t.Parallel()

	globals := testGlobals(t)

	var stdout bytes.Buffer
	cmd := &PipelineCloseCmd{
		SessionID: "00000000-0000-0000-0000-000000000000",
		Status:    "completed",
		Yes:       true,
		Stdout:    &stdout,
		Stderr:    &bytes.Buffer{},
	}

	err := cmd.Run(globals)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
	assert.Empty(t, stdout.String())
}

// --- test helpers --------------------------------------------------------

// ingestTestSynthesis creates a minimal synthesis output linked to
// the given analysis session, including the SynthesisSupplement
// required by the synthesis gate. Returns the output_id for
// assertions.
func ingestTestSynthesis(
	t *testing.T,
	s store.Store,
	analysisSessionID string,
) string {
	t.Helper()
	ctx := t.Context()

	out := &exchange.AnalystOutput{
		Attribution: exchange.AgentAttribution{
			AnalystID: "signatory-synthesis-v1",
			// Model and InvokedAt server-stamped at ingest.
			Round: 1,
		},
		Target: "https://github.com/JedWatson/classnames",
		Conclusions: []exchange.Conclusion{{
			ID:        "S001",
			Verdict:   "synthesis test stub",
			Rationale: "minimal synthesis for pipeline close test",
			Severity:  exchange.Severity{Default: exchange.SeverityInformational},
			Category:  "synthesis",
		}},
		SynthesisSupplement: &exchange.SynthesisSupplement{
			ProposedPosture: exchange.ProposedPosture{
				Tier:             "trusted-for-now",
				RationaleSummary: "test: all signals nominal",
			},
			Reasoning: "Test synthesis reasoning.",
			Summary:   "Test synthesis summary.",
		},
	}

	result, err := s.IngestAnalystOutput(ctx, out, "",
		store.WithAnalysisSession(analysisSessionID))
	require.NoError(t, err)
	return result.OutputID
}
