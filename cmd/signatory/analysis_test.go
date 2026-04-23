package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/exchange"
	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/store"
)

// Phase 3 CLI tests: begin/end/list/show against a real SQLite store.
// Verb implementations route through Globals.OpenStore so the tests
// exercise the same code path production uses. `--invoked-by` is
// passed explicitly rather than relying on SIGNATORY_TEAM, so the
// test harness is independent of the host's identity configuration.

const testActor = "team:cli-test+opus"

// --- analysis begin --------------------------------------------------------

func TestAnalysisBegin_HappyPath(t *testing.T) {
	g := newTestGlobals(t)

	var stdout bytes.Buffer
	cmd := &AnalysisBeginCmd{
		Target:           "pkg:npm/begin-happy",
		ExpectedAnalysts: []string{"external-sec-v1", "signatory-provenance"},
		Notes:            "happy path",
		InvokedBy:        testActor,
		Stdout:           &stdout,
	}
	require.NoError(t, cmd.Run(g))

	sessionID := strings.TrimSpace(stdout.String())
	_, err := uuid.Parse(sessionID)
	require.NoError(t, err, "stdout must be a single UUID when --json not set")

	// Verify the row landed by reading it back via the store.
	s, err := g.OpenStore(t.Context())
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck

	sess, err := s.GetAnalysisSession(t.Context(), sessionID)
	require.NoError(t, err)
	assert.Equal(t, profile.AnalysisSessionInProgress, sess.Status)
	assert.Equal(t, "pkg:npm/begin-happy", sess.TargetURI)
	assert.Equal(t, testActor, sess.InvokedBy)
	assert.Equal(t, []string{"external-sec-v1", "signatory-provenance"}, sess.ExpectedAnalysts)
	assert.Equal(t, "happy path", sess.Notes)
	assert.Nil(t, sess.EndedAt, "in_progress session must have nil EndedAt")
}

func TestAnalysisBegin_VersionEmbeddedInURI(t *testing.T) {
	// `pkg:npm/X@1.2.3` must split into TargetURI=<raw> +
	// TargetVersion=1.2.3 so filter queries can find it by version.
	g := newTestGlobals(t)

	var stdout bytes.Buffer
	cmd := &AnalysisBeginCmd{
		Target:    "pkg:npm/versioned-uri@1.2.3",
		InvokedBy: testActor,
		Stdout:    &stdout,
	}
	require.NoError(t, cmd.Run(g))

	sessionID := strings.TrimSpace(stdout.String())
	s, err := g.OpenStore(t.Context())
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck

	sess, err := s.GetAnalysisSession(t.Context(), sessionID)
	require.NoError(t, err)
	assert.Equal(t, "1.2.3", sess.TargetVersion,
		"version suffix in the URI must be split into TargetVersion")
}

func TestAnalysisBegin_VersionViaFlag(t *testing.T) {
	g := newTestGlobals(t)

	var stdout bytes.Buffer
	cmd := &AnalysisBeginCmd{
		Target:    "pkg:npm/flag-version",
		Version:   "2.0.0",
		InvokedBy: testActor,
		Stdout:    &stdout,
	}
	require.NoError(t, cmd.Run(g))

	sessionID := strings.TrimSpace(stdout.String())
	s, err := g.OpenStore(t.Context())
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck

	sess, err := s.GetAnalysisSession(t.Context(), sessionID)
	require.NoError(t, err)
	assert.Equal(t, "2.0.0", sess.TargetVersion)
}

func TestAnalysisBegin_ConflictingVersions(t *testing.T) {
	// @V suffix and --version that disagree must be a usage error.
	// This mirrors the posture set/get contract.
	g := newTestGlobals(t)

	cmd := &AnalysisBeginCmd{
		Target:    "pkg:npm/conflict@1.2.3",
		Version:   "2.0.0",
		InvokedBy: testActor,
		Stdout:    &bytes.Buffer{},
	}
	err := cmd.Run(g)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUsage,
		"conflicting version inputs must produce a usage error, not a silent pick")
}

func TestAnalysisBegin_JSONOutput(t *testing.T) {
	g := newTestGlobals(t)

	var stdout bytes.Buffer
	cmd := &AnalysisBeginCmd{
		Target:           "pkg:npm/begin-json",
		ExpectedAnalysts: []string{"external-sec-v1"},
		InvokedBy:        testActor,
		JSON:             true,
		Stdout:           &stdout,
	}
	require.NoError(t, cmd.Run(g))

	var sess profile.AnalysisSession
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &sess),
		"--json output must be a decodable AnalysisSession")
	assert.Equal(t, "pkg:npm/begin-json", sess.TargetURI)
	assert.Nil(t, sess.EndedAt,
		"--json for an in_progress session must have ended_at absent, not zero-time")

	// Verify the absent-field invariant at the wire level too —
	// the primary bug the Phase 3 rework fixed was `ended_at`
	// serializing `"0001-01-01T00:00:00Z"`.
	assert.NotContains(t, stdout.String(), "0001-01-01",
		"zero time.Time must not bleed into the JSON envelope")
	assert.NotContains(t, stdout.String(), `"ended_at"`,
		"omitempty on *time.Time must actually omit the field")
}

func TestAnalysisBegin_ExpectedAnalystsEmptySlice(t *testing.T) {
	// Zero-length expected-analysts must round-trip as nil (not
	// a single empty-string element). Matches the store-layer
	// invariant exercised in analysis_session_test.go.
	g := newTestGlobals(t)

	var stdout bytes.Buffer
	cmd := &AnalysisBeginCmd{
		Target:    "pkg:npm/no-expected",
		InvokedBy: testActor,
		Stdout:    &stdout,
	}
	require.NoError(t, cmd.Run(g))

	sessionID := strings.TrimSpace(stdout.String())
	s, err := g.OpenStore(t.Context())
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck

	sess, err := s.GetAnalysisSession(t.Context(), sessionID)
	require.NoError(t, err)
	assert.Empty(t, sess.ExpectedAnalysts)
}

// --- analysis end ----------------------------------------------------------

func TestAnalysisEnd_HappyPath(t *testing.T) {
	g := newTestGlobals(t)
	sessionID := beginSessionViaCmd(t, g, "pkg:npm/end-happy")

	var stdout, stderr bytes.Buffer
	cmd := &AnalysisEndCmd{
		SessionID: sessionID,
		Status:    "completed",
		Stdout:    &stdout,
		Stderr:    &stderr,
	}
	require.NoError(t, cmd.Run(g))
	// Confirmation goes to stderr so stdout stays pipeable for
	// `... | jq` usage against the --json branch. The human mode
	// produces nothing on stdout.
	assert.Empty(t, stdout.String(),
		"stdout must be empty in human mode so it stays pipeable")
	assert.Contains(t, stderr.String(), "completed")
	assert.Contains(t, stderr.String(), sessionID)

	// Verify terminal + ended_at set.
	s, err := g.OpenStore(t.Context())
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck

	sess, err := s.GetAnalysisSession(t.Context(), sessionID)
	require.NoError(t, err)
	assert.Equal(t, profile.AnalysisSessionCompleted, sess.Status)
	require.NotNil(t, sess.EndedAt, "closed session must have ended_at set")
}

func TestAnalysisEnd_JSONOutput(t *testing.T) {
	// `--json` returns the updated session row on stdout. Confirms
	// the close landed (Status terminal, EndedAt set) without a
	// separate Get round-trip.
	g := newTestGlobals(t)
	sessionID := beginSessionViaCmd(t, g, "pkg:npm/end-json")

	var stdout bytes.Buffer
	cmd := &AnalysisEndCmd{
		SessionID: sessionID,
		Status:    "completed",
		JSON:      true,
		Stdout:    &stdout,
	}
	require.NoError(t, cmd.Run(g))

	var sess profile.AnalysisSession
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &sess))
	assert.Equal(t, sessionID, sess.ID)
	assert.Equal(t, profile.AnalysisSessionCompleted, sess.Status)
	require.NotNil(t, sess.EndedAt, "--json must include a populated ended_at after close")
}

func TestAnalysisEnd_SynthesisOutputIDWritesFK(t *testing.T) {
	g := newTestGlobals(t)
	sessionID := beginSessionViaCmd(t, g, "pkg:npm/end-with-synth")

	// Ingest a synthesis-shaped output to satisfy the FK.
	synthID := ingestSynthesisForSession(t, g, "pkg:npm/end-with-synth", sessionID)

	cmd := &AnalysisEndCmd{
		SessionID:         sessionID,
		Status:            "completed",
		SynthesisOutputID: synthID,
		Stdout:            &bytes.Buffer{},
	}
	require.NoError(t, cmd.Run(g))

	s, err := g.OpenStore(t.Context())
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck

	sess, err := s.GetAnalysisSession(t.Context(), sessionID)
	require.NoError(t, err)
	assert.Equal(t, synthID, sess.SynthesisOutputID)
}

func TestAnalysisEnd_UnknownSessionIsNotFound(t *testing.T) {
	g := newTestGlobals(t)

	cmd := &AnalysisEndCmd{
		SessionID: uuid.NewString(),
		Status:    "completed",
		Stdout:    &bytes.Buffer{},
	}
	err := cmd.Run(g)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found",
		"error must name the unknown-session condition clearly")
}

// TestAnalysisEnd_TerminalRejectDoesNotLeakTx is the regression
// anchor at the CLI layer for the tx-leak bug family. A double-close
// attempt must fail cleanly AND leave the store usable for a
// follow-up op — if the tx leaked, the follow-up would block on
// BeginTx against the single SQLite connection.
func TestAnalysisEnd_TerminalRejectDoesNotLeakTx(t *testing.T) {
	g := newTestGlobals(t)
	sessionID := beginSessionViaCmd(t, g, "pkg:npm/no-reopen")

	// First close: succeeds.
	cmd1 := &AnalysisEndCmd{
		SessionID: sessionID,
		Status:    "completed",
		Stdout:    &bytes.Buffer{},
	}
	require.NoError(t, cmd1.Run(g))

	// Second close: rejected. Store must not be poisoned by a
	// leaked tx.
	cmd2 := &AnalysisEndCmd{
		SessionID: sessionID,
		Status:    "failed",
		Stdout:    &bytes.Buffer{},
	}
	require.Error(t, cmd2.Run(g))

	// Follow-up op must succeed — a leaked tx would deadlock.
	s, err := g.OpenStore(t.Context())
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err = s.GetAnalysisSession(ctx, sessionID)
	require.NoError(t, err,
		"follow-up read must not hang; tx leak would deadlock the single-connection pool")
}

// --- analysis list ---------------------------------------------------------

func TestAnalysisList_EmptyStore(t *testing.T) {
	g := newTestGlobals(t)
	var stdout bytes.Buffer
	cmd := &AnalysisListCmd{Stdout: &stdout}
	require.NoError(t, cmd.Run(g))
	assert.Contains(t, stdout.String(), "No analysis sessions")
}

func TestAnalysisList_RendersSessionsNewestFirst(t *testing.T) {
	g := newTestGlobals(t)
	_ = beginSessionViaCmd(t, g, "pkg:npm/list-older")
	// Sleep one second so the second session has a distinct
	// started_at (RFC3339 granularity is one second).
	time.Sleep(1100 * time.Millisecond)
	newerID := beginSessionViaCmd(t, g, "pkg:npm/list-newer")

	var stdout bytes.Buffer
	cmd := &AnalysisListCmd{Stdout: &stdout}
	require.NoError(t, cmd.Run(g))

	out := stdout.String()
	assert.Contains(t, out, "pkg:npm/list-older")
	assert.Contains(t, out, "pkg:npm/list-newer")

	// Assert ordering: newer appears before older in the output.
	newerIdx := strings.Index(out, newerID[:8])
	olderIdx := strings.Index(out, "list-older")
	require.GreaterOrEqual(t, newerIdx, 0)
	require.GreaterOrEqual(t, olderIdx, 0)
	assert.Less(t, newerIdx, olderIdx, "newer session must render first")
}

func TestAnalysisList_FilterByStatus(t *testing.T) {
	g := newTestGlobals(t)
	openID := beginSessionViaCmd(t, g, "pkg:npm/list-open")
	closedID := beginSessionViaCmd(t, g, "pkg:npm/list-closed")
	require.NoError(t, (&AnalysisEndCmd{
		SessionID: closedID,
		Status:    "completed",
		Stdout:    &bytes.Buffer{},
	}).Run(g))

	var stdout bytes.Buffer
	cmd := &AnalysisListCmd{
		Status: "in_progress",
		Stdout: &stdout,
	}
	require.NoError(t, cmd.Run(g))
	out := stdout.String()
	assert.Contains(t, out, openID[:8])
	assert.NotContains(t, out, closedID[:8],
		"--status=in_progress must exclude closed sessions")
}

func TestAnalysisList_FilterByEntity(t *testing.T) {
	g := newTestGlobals(t)
	wantedID := beginSessionViaCmd(t, g, "pkg:npm/list-wanted")
	_ = beginSessionViaCmd(t, g, "pkg:npm/list-unwanted")

	var stdout bytes.Buffer
	cmd := &AnalysisListCmd{
		Entity: "pkg:npm/list-wanted",
		Stdout: &stdout,
	}
	require.NoError(t, cmd.Run(g))

	out := stdout.String()
	assert.Contains(t, out, wantedID[:8])
	assert.NotContains(t, out, "list-unwanted")
}

func TestAnalysisList_FilterByEntity_UnknownTargetIsUsageError(t *testing.T) {
	// Silently-empty results on a typo target mask user mistakes.
	// Resolver must surface the "not found" condition.
	g := newTestGlobals(t)

	cmd := &AnalysisListCmd{
		Entity: "pkg:npm/never-existed",
		Stdout: &bytes.Buffer{},
	}
	err := cmd.Run(g)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUsage)
}

func TestAnalysisList_FilterByTargetVersion(t *testing.T) {
	g := newTestGlobals(t)
	v1ID := beginSessionViaCmdWithVersion(t, g, "pkg:npm/version-filter", "1.0.0")
	_ = beginSessionViaCmdWithVersion(t, g, "pkg:npm/version-filter", "2.0.0")

	var stdout bytes.Buffer
	cmd := &AnalysisListCmd{
		TargetVersion: "1.0.0",
		Stdout:        &stdout,
	}
	require.NoError(t, cmd.Run(g))

	out := stdout.String()
	assert.Contains(t, out, v1ID[:8])
	assert.Contains(t, out, "@1.0.0")
	assert.NotContains(t, out, "@2.0.0")
}

func TestAnalysisList_SinceAsDuration(t *testing.T) {
	g := newTestGlobals(t)
	_ = beginSessionViaCmd(t, g, "pkg:npm/since-test")

	// 1h window: the just-created session must be inside it.
	var stdout bytes.Buffer
	cmd := &AnalysisListCmd{
		Since:  "1h",
		Stdout: &stdout,
	}
	require.NoError(t, cmd.Run(g))
	assert.Contains(t, stdout.String(), "since-test")
}

func TestAnalysisList_SinceAsRFC3339(t *testing.T) {
	// Scripted callers persist the previous-run cutoff as an
	// RFC3339 timestamp; that's the other accepted --since form.
	g := newTestGlobals(t)
	_ = beginSessionViaCmd(t, g, "pkg:npm/since-rfc")

	var stdout bytes.Buffer
	cmd := &AnalysisListCmd{
		Since:  time.Now().UTC().Add(-time.Hour).Format(time.RFC3339),
		Stdout: &stdout,
	}
	require.NoError(t, cmd.Run(g))
	assert.Contains(t, stdout.String(), "since-rfc")
}

func TestAnalysisList_SinceBogusInputIsUsageError(t *testing.T) {
	g := newTestGlobals(t)
	cmd := &AnalysisListCmd{
		Since:  "yesterday",
		Stdout: &bytes.Buffer{},
	}
	err := cmd.Run(g)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUsage)
}

func TestAnalysisList_Limit(t *testing.T) {
	g := newTestGlobals(t)
	for i := 0; i < 4; i++ {
		_ = beginSessionViaCmd(t, g, "pkg:npm/limit-"+string(rune('a'+i)))
	}

	var stdout bytes.Buffer
	cmd := &AnalysisListCmd{
		Limit:  2,
		Stdout: &stdout,
	}
	require.NoError(t, cmd.Run(g))
	lines := nonEmptyLines(stdout.String())
	assert.Len(t, lines, 2, "--limit must cap the row count")
}

func TestAnalysisList_JSONOutput(t *testing.T) {
	g := newTestGlobals(t)
	_ = beginSessionViaCmd(t, g, "pkg:npm/list-json")

	var stdout bytes.Buffer
	cmd := &AnalysisListCmd{JSON: true, Stdout: &stdout}
	require.NoError(t, cmd.Run(g))

	var sessions []profile.AnalysisSession
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &sessions))
	require.Len(t, sessions, 1)
	assert.Equal(t, "pkg:npm/list-json", sessions[0].TargetURI)
}

// --- analysis show ---------------------------------------------------------

func TestAnalysisShow_UnknownSession(t *testing.T) {
	g := newTestGlobals(t)
	cmd := &AnalysisShowCmd{
		SessionID: uuid.NewString(),
		Stdout:    &bytes.Buffer{},
	}
	err := cmd.Run(g)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestAnalysisShow_ExpectedVsLandedDiff(t *testing.T) {
	// Session with expected=[a,b] but only analyst b ingested an
	// output. Show must flag a as missing.
	g := newTestGlobals(t)
	sessionID := beginSessionWithExpectedViaCmd(t, g,
		"pkg:npm/show-diff",
		[]string{"external-sec-v1", "signatory-provenance"})

	// Ingest output only from signatory-provenance.
	ingestAnalystForSession(t, g, "pkg:npm/show-diff", "signatory-provenance", sessionID)

	var stdout bytes.Buffer
	cmd := &AnalysisShowCmd{
		SessionID: sessionID,
		Stdout:    &stdout,
	}
	require.NoError(t, cmd.Run(g))

	out := stdout.String()
	assert.Contains(t, out, "signatory-provenance", "landed analyst must appear")
	assert.Contains(t, out, "missing: external-sec-v1",
		"missing tag must name the expected-but-not-landed analyst")
}

func TestAnalysisShow_UnexpectedLanded(t *testing.T) {
	// Session expects external-sec-v1; an unexpected analyst also
	// lands. Show must surface the unexpected tag.
	g := newTestGlobals(t)
	sessionID := beginSessionWithExpectedViaCmd(t, g,
		"pkg:npm/show-unexpected",
		[]string{"external-sec-v1"})

	ingestAnalystForSession(t, g, "pkg:npm/show-unexpected", "external-sec-v1", sessionID)
	ingestAnalystForSession(t, g, "pkg:npm/show-unexpected", "surprise-analyst-v1", sessionID)

	var stdout bytes.Buffer
	cmd := &AnalysisShowCmd{
		SessionID: sessionID,
		Stdout:    &stdout,
	}
	require.NoError(t, cmd.Run(g))
	assert.Contains(t, stdout.String(), "unexpected: surprise-analyst-v1")
}

func TestAnalysisShow_JSONOutput(t *testing.T) {
	g := newTestGlobals(t)
	sessionID := beginSessionWithExpectedViaCmd(t, g,
		"pkg:npm/show-json",
		[]string{"external-sec-v1"})
	ingestAnalystForSession(t, g, "pkg:npm/show-json", "external-sec-v1", sessionID)

	var stdout bytes.Buffer
	cmd := &AnalysisShowCmd{
		SessionID: sessionID,
		JSON:      true,
		Stdout:    &stdout,
	}
	require.NoError(t, cmd.Run(g))

	var data AnalysisShowData
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &data))
	assert.Equal(t, sessionID, data.Session.ID)
	require.Len(t, data.Outputs, 1)
	assert.Equal(t, []string{"external-sec-v1"}, data.Landed)
	assert.Empty(t, data.Missing)
}

func TestAnalysisShow_ClosedRendersWallClock(t *testing.T) {
	g := newTestGlobals(t)
	sessionID := beginSessionViaCmd(t, g, "pkg:npm/show-wall")
	time.Sleep(1100 * time.Millisecond)
	require.NoError(t, (&AnalysisEndCmd{
		SessionID: sessionID,
		Status:    "completed",
		Stdout:    &bytes.Buffer{},
	}).Run(g))

	var stdout bytes.Buffer
	cmd := &AnalysisShowCmd{SessionID: sessionID, Stdout: &stdout}
	require.NoError(t, cmd.Run(g))

	out := stdout.String()
	assert.Contains(t, out, "wall=",
		"closed session must show a wall-clock duration")
	assert.Contains(t, out, "[completed]")
}

// --- test helpers ----------------------------------------------------------

func beginSessionViaCmd(t *testing.T, g *Globals, target string) string {
	t.Helper()
	return runBegin(t, g, target, "", nil)
}

func beginSessionViaCmdWithVersion(t *testing.T, g *Globals, target, version string) string {
	t.Helper()
	return runBegin(t, g, target, version, nil)
}

func beginSessionWithExpectedViaCmd(t *testing.T, g *Globals, target string, expected []string) string {
	t.Helper()
	return runBegin(t, g, target, "", expected)
}

func runBegin(t *testing.T, g *Globals, target, version string, expected []string) string {
	t.Helper()
	var stdout bytes.Buffer
	cmd := &AnalysisBeginCmd{
		Target:           target,
		Version:          version,
		ExpectedAnalysts: expected,
		InvokedBy:        testActor,
		Stdout:           &stdout,
	}
	require.NoError(t, cmd.Run(g))
	return strings.TrimSpace(stdout.String())
}

// ingestAnalystForSession writes a minimally-valid analyst output
// linked to the session via WithAnalysisSession.
func ingestAnalystForSession(t *testing.T, g *Globals, target, analystID, sessionID string) string {
	t.Helper()
	s, err := g.OpenStore(t.Context())
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck

	lineStart := 1
	out := &exchange.AnalystOutput{
		Attribution: exchange.AgentAttribution{
			AnalystID: analystID,
			Model:     "test-model",
			InvokedAt: time.Now().UTC().Format(time.RFC3339),
		},
		Target: target,
		Conclusions: []exchange.Conclusion{{
			ID: "F001", Verdict: "v", Rationale: "r",
			Severity: exchange.Severity{Default: exchange.SeverityLow},
			Category: "c",
			Citations: []exchange.Citation{{
				Path:      "src/x.go",
				LineStart: &lineStart,
			}},
		}},
	}
	res, err := s.IngestAnalystOutput(t.Context(), out, "cli-test",
		store.WithAnalysisSession(sessionID))
	require.NoError(t, err)
	return res.OutputID
}

// ingestSynthesisForSession writes a synthesis-shaped analyst output
// linked to the session. Used to satisfy the FK when testing the
// --synthesis-output-id close-time flag.
func ingestSynthesisForSession(t *testing.T, g *Globals, target, sessionID string) string {
	t.Helper()
	s, err := g.OpenStore(t.Context())
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck

	lineStart := 1
	out := &exchange.AnalystOutput{
		Attribution: exchange.AgentAttribution{
			AnalystID: "signatory-synthesist-v1",
			Model:     "test-model",
			InvokedAt: time.Now().UTC().Format(time.RFC3339),
		},
		Target: target,
		Conclusions: []exchange.Conclusion{{
			ID: "F001", Verdict: "v", Rationale: "r",
			Severity: exchange.Severity{Default: exchange.SeverityLow},
			Category: "c",
			Citations: []exchange.Citation{{
				Path:      "src/x.go",
				LineStart: &lineStart,
			}},
		}},
		SynthesisSupplement: &exchange.SynthesisSupplement{
			ProposedPosture: exchange.ProposedPosture{
				Tier:             "trusted-for-now",
				RationaleSummary: "test-fixture synthesis",
			},
			Reasoning: "test-fixture reasoning body",
			Summary:   "test-fixture summary",
		},
	}
	res, err := s.IngestAnalystOutput(t.Context(), out, "cli-test",
		store.WithAnalysisSession(sessionID))
	require.NoError(t, err)
	return res.OutputID
}

// nonEmptyLines trims and drops blank lines — used by the --limit
// test so trailing newlines don't inflate the count.
func nonEmptyLines(s string) []string {
	raw := strings.Split(s, "\n")
	out := make([]string, 0, len(raw))
	for _, line := range raw {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}
