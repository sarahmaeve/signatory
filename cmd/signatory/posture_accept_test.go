package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/exchange"
	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/store"
)

// synthesisForAccept returns a minimally-valid synthesis output
// with a proposal suitable for acceptance. Tests customize fields
// after calling; the baseline keeps them terse.
func synthesisForAccept() *exchange.AnalystOutput {
	return &exchange.AnalystOutput{
		Attribution: exchange.AgentAttribution{
			AnalystID: "signatory-synthesis-v1",
			// Model and InvokedAt server-stamped at ingest.
		},
		Target: "pkg:npm/accept-example",
		SynthesisSupplement: &exchange.SynthesisSupplement{
			ProposedPosture: exchange.ProposedPosture{
				Tier:             exchange.ProposedTierTrustedForNow,
				VersionScope:     "1.2.3",
				RationaleSummary: "proposed rationale from the synthesist",
			},
			Reasoning: "synthesis reasoning body",
			Summary:   "synthesis summary",
		},
	}
}

// ingestSynthesisForAccept is the common test setup: ingest a
// synthesis output into g's store and return the resulting
// OutputID so the test can pass it to PostureAcceptCmd.
//
// Creates an analysis session for the synthesis output's target
// and links the ingested output to it — synthesis ingest requires
// a non-empty analysis_session_id post-Bug-1 enforcement (see
// design/dogfood-errors.md). The session is otherwise unused by
// these tests.
func ingestSynthesisForAccept(t *testing.T, g *Globals, out *exchange.AnalystOutput) string {
	t.Helper()
	ctx := t.Context()
	s, err := g.OpenStore(ctx)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck // test cleanup

	// Create an entity for the target so the session FK holds, then
	// the session itself. The store's IngestAnalystOutput will reuse
	// the entity it finds at the given URI; session-create needs the
	// entity row up front.
	//
	// Plan-A canonicalization strips any @version suffix from the
	// target before lookup/insert; so create our pre-session entity
	// at the unversioned form to avoid leaving an orphan versioned
	// entity that breaks the @V-stripping regression assertions.
	sqliteStore, ok := s.(*store.SQLite)
	require.True(t, ok, "test setup expects *store.SQLite")

	entityURI, _ := profile.SplitURIVersion(out.Target)
	entity := &profile.Entity{
		ID:           profile.NewEntityID(),
		CanonicalURI: entityURI,
		Type:         profile.EntityProject,
		ShortName:    "test",
	}
	require.NoError(t, sqliteStore.PutEntity(ctx, entity))

	session := &profile.AnalysisSession{
		ID:        profile.NewEntityID(),
		EntityID:  entity.ID,
		TargetURI: entityURI,
		InvokedBy: "posture-accept-test",
		StartedAt: time.Now().UTC(),
		Status:    profile.AnalysisSessionInProgress,
	}
	require.NoError(t, sqliteStore.CreateAnalysisSession(ctx, session))

	result, err := s.IngestAnalystOutput(ctx, out, "synthesis-for-accept-test",
		store.WithAnalysisSession(session.ID))
	require.NoError(t, err)
	return result.OutputID
}

// latestAcceptAuditDetail returns the parsed detail JSON of the
// most recent audit_log row with action == "accept_posture" for
// entityID. Fails the test if no such row exists.
func latestAcceptAuditDetail(t *testing.T, g *Globals, entityID string) map[string]any {
	t.Helper()
	ctx := context.Background()
	s, err := g.OpenStore(ctx)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck // test cleanup
	sqliteStore, ok := s.(*store.SQLite)
	require.True(t, ok, "store impl should be *store.SQLite for test inspection")

	var rawDetail string
	err = sqliteStore.DB().QueryRowContext(ctx,
		`SELECT detail FROM audit_log
		 WHERE action = ? AND entity_id = ?
		 ORDER BY timestamp DESC LIMIT 1`,
		"accept_posture", entityID).Scan(&rawDetail)
	require.NoError(t, err, "expected an accept_posture audit row for entity %s", entityID)

	var detail map[string]any
	require.NoError(t, json.Unmarshal([]byte(rawDetail), &detail),
		"audit detail must be valid JSON")
	return detail
}

// TestPostureAccept_HappyPath_WritesProposalAsPosture is the M6d
// happy path: a synthesis proposal is accepted verbatim, producing
// a posture row whose tier/version/rationale match the proposal.
// No overrides, `--yes` skips the confirmation prompt.
func TestPostureAccept_HappyPath_WritesProposalAsPosture(t *testing.T) {
	g := newTestGlobals(t)
	outputID := ingestSynthesisForAccept(t, g, synthesisForAccept())

	cmd := &PostureAcceptCmd{
		OutputID: outputID,
		Yes:      true,
	}
	require.NoError(t, cmd.Run(g))

	// Reopen the store to verify the posture row landed with the
	// proposal's values.
	ctx := t.Context()
	s, err := g.OpenStore(ctx)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck // test cleanup

	entity, err := s.FindEntityByURI(ctx, "pkg:npm/accept-example")
	require.NoError(t, err)

	posture, err := s.GetPosture(ctx, entity.ID, "1.2.3")
	require.NoError(t, err)
	assert.Equal(t, profile.PostureTier("trusted-for-now"), posture.Tier)
	assert.Equal(t, "1.2.3", posture.Version)
	assert.Equal(t, "proposed rationale from the synthesist", posture.Rationale)
}

// TestPostureAccept_Audit_NoOverrides asserts the audit detail for
// a zero-override acceptance includes `accepted_from_synthesis_id`
// and none of the `proposed_*` deviation fields. Per the contract:
// presence of a proposed_* field signals a deviation; its absence
// means "accepted verbatim."
func TestPostureAccept_Audit_NoOverrides(t *testing.T) {
	g := newTestGlobals(t)
	outputID := ingestSynthesisForAccept(t, g, synthesisForAccept())

	cmd := &PostureAcceptCmd{
		OutputID: outputID,
		Yes:      true,
	}
	require.NoError(t, cmd.Run(g))

	// Find the entity id so we can query the audit log.
	ctx := t.Context()
	s, err := g.OpenStore(ctx)
	require.NoError(t, err)
	entity, err := s.FindEntityByURI(ctx, "pkg:npm/accept-example")
	require.NoError(t, err)
	s.Close() //nolint:errcheck // test cleanup

	detail := latestAcceptAuditDetail(t, g, entity.ID)
	assert.Equal(t, outputID, detail["accepted_from_synthesis_id"],
		"audit must link the accepted posture back to its source synthesis")
	assert.NotContains(t, detail, "proposed_tier",
		"no tier override → no proposed_tier field")
	assert.NotContains(t, detail, "proposed_version_scope",
		"no version override → no proposed_version_scope field")
	assert.NotContains(t, detail, "proposed_rationale_summary",
		"no rationale override → no proposed_rationale_summary field")
}

// TestPostureAccept_TierOverride_RecordsDeviation asserts that
// overriding the tier lands the override in the posture row AND
// records the synthesist's original proposal under proposed_tier
// in the audit detail. This is how the "I disagreed with the
// synthesist" signal becomes auditable.
func TestPostureAccept_TierOverride_RecordsDeviation(t *testing.T) {
	g := newTestGlobals(t)
	outputID := ingestSynthesisForAccept(t, g, synthesisForAccept())

	cmd := &PostureAcceptCmd{
		OutputID: outputID,
		Tier:     "rejected",
		Yes:      true,
	}
	require.NoError(t, cmd.Run(g))

	ctx := t.Context()
	s, err := g.OpenStore(ctx)
	require.NoError(t, err)
	entity, err := s.FindEntityByURI(ctx, "pkg:npm/accept-example")
	require.NoError(t, err)

	posture, err := s.GetPosture(ctx, entity.ID, "1.2.3")
	require.NoError(t, err)
	assert.Equal(t, profile.PostureTier("rejected"), posture.Tier,
		"override must produce the final tier")
	s.Close() //nolint:errcheck // test cleanup

	detail := latestAcceptAuditDetail(t, g, entity.ID)
	assert.Equal(t, "rejected", detail["tier"],
		"audit must record the final tier")
	assert.Equal(t, "trusted-for-now", detail["proposed_tier"],
		"audit must record the synthesist's original proposal under proposed_tier")
}

// TestPostureAccept_RationaleOverride_RecordsDeviation asserts the
// same pattern for a rationale override: final rationale from the
// user, synthesist's original under proposed_rationale_summary.
func TestPostureAccept_RationaleOverride_RecordsDeviation(t *testing.T) {
	g := newTestGlobals(t)
	outputID := ingestSynthesisForAccept(t, g, synthesisForAccept())

	cmd := &PostureAcceptCmd{
		OutputID:  outputID,
		Rationale: "user disagreed with the synthesist's framing",
		Yes:       true,
	}
	require.NoError(t, cmd.Run(g))

	ctx := t.Context()
	s, err := g.OpenStore(ctx)
	require.NoError(t, err)
	entity, err := s.FindEntityByURI(ctx, "pkg:npm/accept-example")
	require.NoError(t, err)
	posture, err := s.GetPosture(ctx, entity.ID, "1.2.3")
	require.NoError(t, err)
	assert.Equal(t, "user disagreed with the synthesist's framing", posture.Rationale)
	s.Close() //nolint:errcheck // test cleanup

	detail := latestAcceptAuditDetail(t, g, entity.ID)
	assert.Equal(t, "user disagreed with the synthesist's framing", detail["rationale"])
	assert.Equal(t, "proposed rationale from the synthesist", detail["proposed_rationale_summary"])
}

// TestPostureAccept_VersionOverride_RecordsDeviation: same pattern
// for version_scope.
func TestPostureAccept_VersionOverride_RecordsDeviation(t *testing.T) {
	g := newTestGlobals(t)
	outputID := ingestSynthesisForAccept(t, g, synthesisForAccept())

	cmd := &PostureAcceptCmd{
		OutputID: outputID,
		Version:  "1.2.4",
		Yes:      true,
	}
	require.NoError(t, cmd.Run(g))

	ctx := t.Context()
	s, err := g.OpenStore(ctx)
	require.NoError(t, err)
	entity, err := s.FindEntityByURI(ctx, "pkg:npm/accept-example")
	require.NoError(t, err)
	posture, err := s.GetPosture(ctx, entity.ID, "1.2.4")
	require.NoError(t, err,
		"override version should produce a posture row under the new version")
	assert.Equal(t, "1.2.4", posture.Version)
	s.Close() //nolint:errcheck // test cleanup

	detail := latestAcceptAuditDetail(t, g, entity.ID)
	assert.Equal(t, "1.2.4", detail["version"])
	assert.Equal(t, "1.2.3", detail["proposed_version_scope"])
}

// TestPostureAccept_UnknownOutputID_Errors asserts the CLI fails
// cleanly when handed a UUID that isn't in the store at all.
func TestPostureAccept_UnknownOutputID_Errors(t *testing.T) {
	g := newTestGlobals(t)

	cmd := &PostureAcceptCmd{
		OutputID: "00000000-0000-0000-0000-000000000000",
		Yes:      true,
	}
	err := cmd.Run(g)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no synthesis")
}

// TestPostureAccept_NonSynthesisOutput_Errors asserts the CLI
// refuses to accept from a non-synthesis output (security/
// provenance outputs don't carry a proposed_posture). The
// GetSynthesisProposal helper returns ErrNotFound in both
// "unknown id" and "id-but-not-synthesis" cases; the user-facing
// message has to cover both.
func TestPostureAccept_NonSynthesisOutput_Errors(t *testing.T) {
	g := newTestGlobals(t)

	// Ingest a plain security output (no supplement).
	lineStart := 10
	analyst := &exchange.AnalystOutput{
		Attribution: exchange.AgentAttribution{
			AnalystID: "external-sec-v1",
			// Model and InvokedAt server-stamped at ingest.
		},
		Target: "pkg:npm/security-only",
		Conclusions: []exchange.Conclusion{
			{
				ID: "F001", Verdict: "v", Rationale: "r",
				Severity: exchange.Severity{Default: exchange.SeverityLow},
				Category: "c",
				Citations: []exchange.Citation{
					{Path: "src/x.go", LineStart: &lineStart},
				},
			},
		},
	}
	ctx := t.Context()
	s, err := g.OpenStore(ctx)
	require.NoError(t, err)
	result, err := s.IngestAnalystOutput(ctx, analyst, "test")
	require.NoError(t, err)
	s.Close() //nolint:errcheck // test cleanup

	cmd := &PostureAcceptCmd{
		OutputID: result.OutputID,
		Yes:      true,
	}
	err = cmd.Run(g)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no synthesis")
}

// TestPostureAccept_DryRun_DoesNotWrite asserts --dry-run prints
// the proposal without writing anything to the store or audit log.
func TestPostureAccept_DryRun_DoesNotWrite(t *testing.T) {
	g := newTestGlobals(t)
	outputID := ingestSynthesisForAccept(t, g, synthesisForAccept())

	cmd := &PostureAcceptCmd{
		OutputID: outputID,
		Yes:      true,
		DryRun:   true,
	}
	require.NoError(t, cmd.Run(g))

	// No posture row should exist.
	ctx := t.Context()
	s, err := g.OpenStore(ctx)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck // test cleanup
	entity, err := s.FindEntityByURI(ctx, "pkg:npm/accept-example")
	require.NoError(t, err)

	_, err = s.GetPosture(ctx, entity.ID, "1.2.3")
	require.ErrorIs(t, err, store.ErrNotFound,
		"dry-run must not write a posture row")

	// No accept_posture audit row should exist.
	sqliteStore, ok := s.(*store.SQLite)
	require.True(t, ok)
	var count int
	require.NoError(t, sqliteStore.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM audit_log WHERE action = 'accept_posture'`).
		Scan(&count))
	assert.Equal(t, 0, count, "dry-run must not write an audit row")
}

// TestPostureAccept_VersionedEntityTarget_pkg is the 2026-04-22
// testify dogfood regression: `signatory posture accept <id> --yes`
// on a synthesis whose target carries `@V`. The original bug was
// that the accept verb re-derived the entity URI from the synthesis
// target, stripped `@V` via SplitURIVersion, and then FindEntityByURI
// missed — because `ensureEntityForTarget` of the day preserved `@V`
// on the entity row. The fix routes accept through the
// analyst_output.entity_id FK (commit 79c2833); this test locks it
// down so subsequent refactors can't reintroduce the URI round-trip.
//
// Post-v10 (Plan-A canonicalization), `ensureEntityForTarget` strips
// @V on ingest, so the entity row lives at the unversioned URI and
// the versioned form is preserved on analyst_outputs.target. That
// makes the accept path's FK walk the right semantic: the FK points
// at the unversioned entity, which is exactly where posture rows
// belong under Plan A.
//
// Observable post-conditions:
//   - Entity exists at `pkg:npm/accept-versioned` (unversioned).
//   - No entity at `pkg:npm/accept-versioned@1.2.3` (stripped).
//   - Posture for (entity, version="1.2.3") lands on the unversioned
//     entity — matches `posture set pkg:npm/accept-versioned@1.2.3`
//     storage shape, keeping the two entry paths on one row.
func TestPostureAccept_VersionedEntityTarget_pkg(t *testing.T) {
	g := newTestGlobals(t)

	// Target carries `@1.2.3` — the form the /analyze synthesist
	// emitted in the testify@v1.11.1 dogfood run.
	out := synthesisForAccept()
	out.Target = "pkg:npm/accept-versioned@1.2.3"
	outputID := ingestSynthesisForAccept(t, g, out)

	cmd := &PostureAcceptCmd{
		OutputID: outputID,
		Yes:      true,
	}
	require.NoError(t, cmd.Run(g),
		"posture accept on a versioned synthesis target must route through the entity FK, not re-derive via URI stripping")

	ctx := t.Context()
	s, err := g.OpenStore(ctx)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck // test cleanup

	// Post-v10: entity is at the unversioned URI, and the versioned
	// form is recorded on analyst_outputs.target (verified indirectly
	// via posture landing on the unversioned entity).
	entity, err := s.FindEntityByURI(ctx, "pkg:npm/accept-versioned")
	require.NoError(t, err, "entity must exist at the unversioned URI (Plan-A canonicalization)")
	_, err = s.FindEntityByURI(ctx, "pkg:npm/accept-versioned@1.2.3")
	assert.ErrorIs(t, err, store.ErrNotFound,
		"versioned entity must NOT exist — ingest strips @V under Plan A")

	posture, err := s.GetPosture(ctx, entity.ID, "1.2.3")
	require.NoError(t, err)
	assert.Equal(t, profile.PostureTier("trusted-for-now"), posture.Tier)
	assert.Equal(t, "1.2.3", posture.Version)
}

// TestPostureAccept_VersionedEntityTarget_repo is the repo:-scheme
// half of the versioned-target regression. repo: URIs have their
// own shape in SplitURIVersion (distinct from pkg:), so we exercise
// both independently — post-v10 behavior is unified (both end up at
// unversioned entity), but that's a property worth asserting rather
// than assuming.
func TestPostureAccept_VersionedEntityTarget_repo(t *testing.T) {
	g := newTestGlobals(t)

	out := synthesisForAccept()
	out.Target = "repo:github/example/proj@v1.11.1"
	out.SynthesisSupplement.ProposedPosture.VersionScope = "v1.11.1"
	outputID := ingestSynthesisForAccept(t, g, out)

	cmd := &PostureAcceptCmd{
		OutputID: outputID,
		Yes:      true,
	}
	require.NoError(t, cmd.Run(g))

	ctx := t.Context()
	s, err := g.OpenStore(ctx)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck // test cleanup

	entity, err := s.FindEntityByURI(ctx, "repo:github/example/proj")
	require.NoError(t, err, "entity must exist at the unversioned repo URI")
	_, err = s.FindEntityByURI(ctx, "repo:github/example/proj@v1.11.1")
	assert.ErrorIs(t, err, store.ErrNotFound,
		"versioned repo entity must NOT exist — ingest strips @V for repo: URIs too")

	posture, err := s.GetPosture(ctx, entity.ID, "v1.11.1")
	require.NoError(t, err)
	assert.Equal(t, profile.PostureTier("trusted-for-now"), posture.Tier)
	assert.Equal(t, "v1.11.1", posture.Version)
}

// TestPostureAccept_NonInteractiveWithoutYes_Errors asserts the CLI
// refuses to prompt without --yes when stdin is not a terminal.
// Simulates non-TTY via the IsTTY injection hook so the test
// doesn't depend on how `go test` was invoked (tests from a
// terminal inherit a TTY stdin; tests from CI don't).
func TestPostureAccept_NonInteractiveWithoutYes_Errors(t *testing.T) {
	g := newTestGlobals(t)
	outputID := ingestSynthesisForAccept(t, g, synthesisForAccept())

	cmd := &PostureAcceptCmd{
		OutputID: outputID,
		IsTTY:    func() bool { return false },
		// Yes deliberately unset.
	}
	err := cmd.Run(g)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--yes",
		"non-TTY without --yes must produce a usage error that names the flag")
}
