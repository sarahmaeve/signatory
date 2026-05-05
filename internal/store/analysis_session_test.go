package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/exchange"
	"github.com/sarahmaeve/signatory/internal/profile"
)

// Analysis-session tests: lifecycle (create → optional ingest-with-
// session → terminal close), listing with filters, the crucial
// can't-reopen-a-terminal-session invariant, and the FK-link path
// that rides on IngestAnalystOutput(WithAnalysisSession(…)).
//
// Sessions are a pure store concept (no CLI or MCP surface yet);
// these tests cover only the store layer.

// newSessionFixture returns a minimally-valid AnalysisSession for
// testing, with an entity pre-created so the FK holds.
func newSessionFixture(t *testing.T, s *SQLite, targetURI string) *profile.AnalysisSession {
	t.Helper()
	ctx := context.Background()

	entity := &profile.Entity{
		ID:           uuid.NewString(),
		CanonicalURI: targetURI,
		Type:         profile.EntityProject,
		ShortName:    "test",
	}
	require.NoError(t, s.PutEntity(ctx, entity))

	return &profile.AnalysisSession{
		ID:        uuid.NewString(),
		EntityID:  entity.ID,
		TargetURI: targetURI,
		InvokedBy: "tester@example.com",
		StartedAt: time.Now().UTC(),
		Status:    profile.AnalysisSessionInProgress,
	}
}

// --- Create ---------------------------------------------------------------

func TestCreateAnalysisSession_HappyPath(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	sess := newSessionFixture(t, s, "pkg:npm/session-happy")
	sess.ExpectedAnalysts = []string{"external-sec-v1", "signatory-provenance"}
	sess.Notes = "dogfood re-run"
	require.NoError(t, s.CreateAnalysisSession(ctx, sess))

	got, err := s.GetAnalysisSession(ctx, sess.ID)
	require.NoError(t, err)
	assert.Equal(t, sess.ID, got.ID)
	assert.Equal(t, sess.EntityID, got.EntityID)
	assert.Equal(t, "pkg:npm/session-happy", got.TargetURI)
	assert.Equal(t, profile.AnalysisSessionInProgress, got.Status)
	assert.Nil(t, got.EndedAt, "in_progress session must have nil EndedAt")
	assert.Equal(t, []string{"external-sec-v1", "signatory-provenance"}, got.ExpectedAnalysts,
		"ExpectedAnalysts round-trips through comma-joined storage")
	assert.Equal(t, "dogfood re-run", got.Notes)
}

func TestCreateAnalysisSession_RejectsTerminalAtCreation(t *testing.T) {
	// Sessions must start in_progress — terminal states are only
	// reachable via CloseAnalysisSession. Guards against callers
	// trying to backdate a "completed" session.
	s := newTestDB(t)
	ctx := context.Background()

	sess := newSessionFixture(t, s, "pkg:npm/session-terminal-rejected")
	sess.Status = profile.AnalysisSessionCompleted

	err := s.CreateAnalysisSession(ctx, sess)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "in_progress",
		"error must name the expected initial state")
}

func TestCreateAnalysisSession_ValidatesRequiredFields(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	require.Error(t, s.CreateAnalysisSession(ctx, nil))

	sess := newSessionFixture(t, s, "pkg:npm/missing-id")
	sess.ID = ""
	require.Error(t, s.CreateAnalysisSession(ctx, sess))

	sess2 := newSessionFixture(t, s, "pkg:npm/missing-entity")
	sess2.EntityID = ""
	require.Error(t, s.CreateAnalysisSession(ctx, sess2))

	sess3 := newSessionFixture(t, s, "pkg:npm/zero-time")
	sess3.StartedAt = time.Time{}
	require.Error(t, s.CreateAnalysisSession(ctx, sess3))
}

func TestCreateAnalysisSession_EmptyExpectedAnalystsRoundTrips(t *testing.T) {
	// Zero-length slice should survive without turning into a
	// ghost-empty-string element on read.
	s := newTestDB(t)
	ctx := context.Background()

	sess := newSessionFixture(t, s, "pkg:npm/no-expected-analysts")
	require.NoError(t, s.CreateAnalysisSession(ctx, sess))

	got, err := s.GetAnalysisSession(ctx, sess.ID)
	require.NoError(t, err)
	assert.Empty(t, got.ExpectedAnalysts,
		"empty input must round-trip as nil/empty, not [\"\"]")
}

func TestGetAnalysisSession_NotFound(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	_, err := s.GetAnalysisSession(ctx, uuid.NewString())
	assert.ErrorIs(t, err, ErrNotFound)
}

// --- CloseAnalysisSession: lifecycle + tx-rollback regression --------------

func TestCloseAnalysisSession_TransitionsToCompleted(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	sess := newSessionFixture(t, s, "pkg:npm/completing")
	require.NoError(t, s.CreateAnalysisSession(ctx, sess))

	endedAt := sess.StartedAt.Add(5 * time.Minute)
	require.NoError(t, s.CloseAnalysisSession(ctx, sess.ID, profile.AnalysisSessionCloseParams{
		Status:  profile.AnalysisSessionCompleted,
		EndedAt: endedAt,
	}))

	got, err := s.GetAnalysisSession(ctx, sess.ID)
	require.NoError(t, err)
	assert.Equal(t, profile.AnalysisSessionCompleted, got.Status)
	require.NotNil(t, got.EndedAt)
	assert.Equal(t, endedAt.Format(time.RFC3339), got.EndedAt.Format(time.RFC3339))
}

func TestCloseAnalysisSession_WithSynthesisOutputID(t *testing.T) {
	// Auto-close path: synthesis ingest fires close with its
	// freshly-minted output_id. Covers the nullable FK round-trip.
	s := newTestDB(t)
	ctx := context.Background()

	sess := newSessionFixture(t, s, "pkg:npm/synth-close")
	require.NoError(t, s.CreateAnalysisSession(ctx, sess))

	// Need a real analyst_outputs row to satisfy the FK.
	ingestRes, err := s.IngestAnalystOutput(ctx,
		newSynthOutput("pkg:npm/synth-close", "synth-1"),
		"test", WithAnalysisSession(sess.ID))
	require.NoError(t, err)

	require.NoError(t, s.CloseAnalysisSession(ctx, sess.ID, profile.AnalysisSessionCloseParams{
		Status:            profile.AnalysisSessionCompleted,
		EndedAt:           time.Now().UTC(),
		SynthesisOutputID: ingestRes.OutputID,
	}))

	got, err := s.GetAnalysisSession(ctx, sess.ID)
	require.NoError(t, err)
	assert.Equal(t, ingestRes.OutputID, got.SynthesisOutputID)
}

// TestCloseAnalysisSession_RejectsTerminalReopen is the critical
// lifecycle invariant: once a session closes, it stays closed.
// Guards against a double-close race and against bugs that would
// try to reopen by writing in_progress over a terminal state.
//
// Historical note: this scenario previously deadlocked due to a
// tx-leak in the guard path. The deadlock surfaced because the
// second close call blocked on BeginTx against the single SQLite
// connection — the first guard-reject had returned without updating
// the outer err, so the deferred closure skipped Rollback(). The
// fix was to switch to unconditional `defer tx.Rollback()`; this
// test is now the regression anchor.
func TestCloseAnalysisSession_RejectsTerminalReopen(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	sess := newSessionFixture(t, s, "pkg:npm/no-reopen")
	require.NoError(t, s.CreateAnalysisSession(ctx, sess))

	// First close: succeeds.
	require.NoError(t, s.CloseAnalysisSession(ctx, sess.ID, profile.AnalysisSessionCloseParams{
		Status:  profile.AnalysisSessionCompleted,
		EndedAt: time.Now().UTC(),
	}))

	// Terminal→terminal: rejected.
	err := s.CloseAnalysisSession(ctx, sess.ID, profile.AnalysisSessionCloseParams{
		Status:  profile.AnalysisSessionFailed,
		EndedAt: time.Now().UTC(),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "terminal")

	// Follow-up read must succeed (tx was not leaked) — if the
	// rollback didn't fire, this call would deadlock waiting for
	// the single SQLite connection. Test the invariant directly.
	got, err := s.GetAnalysisSession(ctx, sess.ID)
	require.NoError(t, err, "follow-up read must not hang; tx leak would deadlock")
	assert.Equal(t, profile.AnalysisSessionCompleted, got.Status,
		"failed reopen must not have mutated state")
}

// TestCloseAnalysisSession_NotFoundDoesNotLeakTx is the second
// rollback-regression anchor: the unknown-id path in CloseAnalysisSession
// also needs to release its tx. Without unconditional rollback, a
// store that fielded one ErrNotFound close would be poisoned.
func TestCloseAnalysisSession_NotFoundDoesNotLeakTx(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	err := s.CloseAnalysisSession(ctx, uuid.NewString(), profile.AnalysisSessionCloseParams{
		Status:  profile.AnalysisSessionCompleted,
		EndedAt: time.Now().UTC(),
	})
	assert.ErrorIs(t, err, ErrNotFound)

	// Any follow-up op must succeed — a leaked tx would deadlock.
	sess := newSessionFixture(t, s, "pkg:npm/after-not-found")
	require.NoError(t, s.CreateAnalysisSession(ctx, sess),
		"follow-up write must not hang; tx leak would deadlock")
}

func TestCloseAnalysisSession_ValidatesRequiredFields(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	// Empty id.
	err := s.CloseAnalysisSession(ctx, "", profile.AnalysisSessionCloseParams{
		Status:  profile.AnalysisSessionCompleted,
		EndedAt: time.Now().UTC(),
	})
	require.Error(t, err)

	// Non-terminal status.
	sess := newSessionFixture(t, s, "pkg:npm/close-non-terminal")
	require.NoError(t, s.CreateAnalysisSession(ctx, sess))
	err = s.CloseAnalysisSession(ctx, sess.ID, profile.AnalysisSessionCloseParams{
		Status:  profile.AnalysisSessionInProgress,
		EndedAt: time.Now().UTC(),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "terminal")

	// Zero EndedAt.
	err = s.CloseAnalysisSession(ctx, sess.ID, profile.AnalysisSessionCloseParams{
		Status: profile.AnalysisSessionCompleted,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "EndedAt")
}

// --- ListAnalysisSessions: filter + sort coverage -------------------------

func TestListAnalysisSessions_EmptyStore(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	got, err := s.ListAnalysisSessions(ctx, AnalysisSessionFilter{})
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestListAnalysisSessions_FilterByStatus(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	inProgress1 := newSessionFixture(t, s, "pkg:npm/list-inp-1")
	require.NoError(t, s.CreateAnalysisSession(ctx, inProgress1))
	inProgress2 := newSessionFixture(t, s, "pkg:npm/list-inp-2")
	require.NoError(t, s.CreateAnalysisSession(ctx, inProgress2))
	completed := newSessionFixture(t, s, "pkg:npm/list-cpl")
	require.NoError(t, s.CreateAnalysisSession(ctx, completed))
	require.NoError(t, s.CloseAnalysisSession(ctx, completed.ID, profile.AnalysisSessionCloseParams{
		Status:  profile.AnalysisSessionCompleted,
		EndedAt: time.Now().UTC(),
	}))

	gotInProgress, err := s.ListAnalysisSessions(ctx, AnalysisSessionFilter{
		Status: profile.AnalysisSessionInProgress,
	})
	require.NoError(t, err)
	assert.Len(t, gotInProgress, 2)

	gotCompleted, err := s.ListAnalysisSessions(ctx, AnalysisSessionFilter{
		Status: profile.AnalysisSessionCompleted,
	})
	require.NoError(t, err)
	assert.Len(t, gotCompleted, 1)
	assert.Equal(t, completed.ID, gotCompleted[0].ID)
}

func TestListAnalysisSessions_FilterByEntityID(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	wanted := newSessionFixture(t, s, "pkg:npm/list-entity-wanted")
	require.NoError(t, s.CreateAnalysisSession(ctx, wanted))
	other := newSessionFixture(t, s, "pkg:npm/list-entity-other")
	require.NoError(t, s.CreateAnalysisSession(ctx, other))

	got, err := s.ListAnalysisSessions(ctx, AnalysisSessionFilter{
		EntityID: wanted.EntityID,
	})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, wanted.ID, got[0].ID)
}

func TestListAnalysisSessions_FilterByTargetVersion(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	v1 := newSessionFixture(t, s, "pkg:npm/versioned-filter")
	v1.TargetVersion = "1.0.0"
	require.NoError(t, s.CreateAnalysisSession(ctx, v1))
	v2 := newSessionFixture(t, s, "pkg:npm/versioned-filter-other")
	v2.TargetVersion = "2.0.0"
	require.NoError(t, s.CreateAnalysisSession(ctx, v2))

	got, err := s.ListAnalysisSessions(ctx, AnalysisSessionFilter{
		TargetVersion: "1.0.0",
	})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, v1.ID, got[0].ID)
}

func TestListAnalysisSessions_FilterBySince(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	now := time.Now().UTC()

	old := newSessionFixture(t, s, "pkg:npm/filter-since-old")
	old.StartedAt = now.Add(-2 * time.Hour)
	require.NoError(t, s.CreateAnalysisSession(ctx, old))

	recent := newSessionFixture(t, s, "pkg:npm/filter-since-recent")
	recent.StartedAt = now
	require.NoError(t, s.CreateAnalysisSession(ctx, recent))

	got, err := s.ListAnalysisSessions(ctx, AnalysisSessionFilter{
		Since: now.Add(-1 * time.Hour),
	})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, recent.ID, got[0].ID)
}

func TestListAnalysisSessions_Limit(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	now := time.Now().UTC()
	for i := 0; i < 5; i++ {
		// Distinct URIs so each iteration creates a fresh entity
		// (entities.canonical_uri is UNIQUE).
		sess := newSessionFixture(t, s, fmt.Sprintf("pkg:npm/limit-fixture-%d", i))
		// Offset started_at so ordering is deterministic.
		sess.StartedAt = now.Add(time.Duration(-i) * time.Minute)
		require.NoError(t, s.CreateAnalysisSession(ctx, sess))
	}

	got, err := s.ListAnalysisSessions(ctx, AnalysisSessionFilter{Limit: 2})
	require.NoError(t, err)
	assert.Len(t, got, 2)
}

func TestListAnalysisSessions_SortedNewestFirst(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	older := newSessionFixture(t, s, "pkg:npm/older")
	older.StartedAt = time.Now().UTC().Add(-2 * time.Hour)
	require.NoError(t, s.CreateAnalysisSession(ctx, older))

	newer := newSessionFixture(t, s, "pkg:npm/newer")
	newer.StartedAt = time.Now().UTC()
	require.NoError(t, s.CreateAnalysisSession(ctx, newer))

	got, err := s.ListAnalysisSessions(ctx, AnalysisSessionFilter{})
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, newer.ID, got[0].ID, "newest first")
	assert.Equal(t, older.ID, got[1].ID)
}

// --- Session linkage via IngestAnalystOutput(WithAnalysisSession) ---------

// TestIngestAnalystOutput_WithAnalysisSession confirms the folded
// contract: ingest with WithAnalysisSession stamps the FK at INSERT
// time, making the output surface via ListOutputsForSession.
func TestIngestAnalystOutput_WithAnalysisSession(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	sess := newSessionFixture(t, s, "pkg:npm/ingest-with-session")
	require.NoError(t, s.CreateAnalysisSession(ctx, sess))

	out := newTestAnalystOutput("pkg:npm/ingest-with-session", "external-sec-v1")
	res, err := s.IngestAnalystOutput(ctx, out, "test",
		WithAnalysisSession(sess.ID))
	require.NoError(t, err)
	assert.False(t, res.Idempotent)

	outputs, err := s.ListOutputsForSession(ctx, sess.ID)
	require.NoError(t, err)
	require.Len(t, outputs, 1)
	assert.Equal(t, res.OutputID, outputs[0].OutputID)
}

// TestIngestAnalystOutput_WithoutSession exercises the soft-cut
// contract: ingest without WithAnalysisSession still succeeds, the
// FK column stays NULL, ListOutputsForSession returns nothing for
// any session.
func TestIngestAnalystOutput_WithoutSession(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	sess := newSessionFixture(t, s, "pkg:npm/ingest-no-session")
	require.NoError(t, s.CreateAnalysisSession(ctx, sess))

	out := newTestAnalystOutput("pkg:npm/ingest-no-session", "external-sec-v1")
	_, err := s.IngestAnalystOutput(ctx, out, "test")
	require.NoError(t, err)

	outputs, err := s.ListOutputsForSession(ctx, sess.ID)
	require.NoError(t, err)
	assert.Empty(t, outputs,
		"soft-cut: unlinked outputs must not surface under any session")
}

// TestIngestAnalystOutput_WithAnalysisSession_UnknownSession: the
// FK constraint surfaces a clear error rather than silently writing
// a dangling row.
func TestIngestAnalystOutput_WithAnalysisSession_UnknownSession(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	out := newTestAnalystOutput("pkg:npm/unknown-session", "external-sec-v1")
	_, err := s.IngestAnalystOutput(ctx, out, "test",
		WithAnalysisSession(uuid.NewString()))
	require.Error(t, err,
		"bogus session id must trip the FK constraint at INSERT time")
}

func TestListOutputsForSession_Empty(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	sess := newSessionFixture(t, s, "pkg:npm/empty-session")
	require.NoError(t, s.CreateAnalysisSession(ctx, sess))

	outputs, err := s.ListOutputsForSession(ctx, sess.ID)
	require.NoError(t, err)
	assert.Empty(t, outputs)
}

// --- test helpers ---------------------------------------------------------

// newTestAnalystOutput returns a minimally-valid v1 AnalystOutput
// for ingest tests. Keeps the surface narrow; per-test customization
// happens by mutating the returned pointer.
func newTestAnalystOutput(target, analystID string) *exchange.AnalystOutput {
	lineStart := 1
	return &exchange.AnalystOutput{
		Attribution: exchange.AgentAttribution{
			AnalystID: analystID,
			Model:     "test",
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
}

// newSynthOutput returns a synthesis AnalystOutput suitable for
// populating synthesis_output_id in CloseAnalysisSession tests.
// The v1 validator requires SynthesisSupplement iff analyst_id is
// a synthesis role; this helper wires up a minimal supplement.
// _ = analystID keeps the param for future per-test customization
// without bikeshedding the signature.
func newSynthOutput(target, _ string) *exchange.AnalystOutput {
	out := newTestAnalystOutput(target, "signatory-synthesis-v1")
	out.SynthesisSupplement = &exchange.SynthesisSupplement{
		ProposedPosture: exchange.ProposedPosture{
			Tier:             "trusted-for-now",
			VersionScope:     "1.0.0",
			RationaleSummary: "test-fixture synthesis",
		},
		Reasoning: "test-fixture reasoning body",
		Summary:   "test-fixture summary",
	}
	return out
}
