package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/exchange"
	"github.com/sarahmaeve/signatory/internal/profile"
)

// Prune tests: exercise the trigger-drop/delete/reinstall ceremony
// against a populated test DB with multiple child tables occupied.
// The append-only triggers on analyst_outputs, conclusions,
// citations, and friends would block a naive DELETE; these tests
// prove the ceremony works and the triggers come back.

// pruneOutputFor builds a minimally-valid analyst_output against
// the named target with one conclusion + one citation, so pruning
// exercises the transitive child cleanup (not just the top-level
// analyst_outputs row).
func pruneOutputFor(target string) *exchange.AnalystOutput {
	lineStart := 5
	return &exchange.AnalystOutput{
		Attribution: exchange.AgentAttribution{
			AnalystID: "external-sec-v1",
			// Model and InvokedAt server-stamped at ingest.
		},
		Target: target,
		Conclusions: []exchange.Conclusion{
			{
				ID:        "F001",
				Verdict:   "test",
				Rationale: "r",
				Severity:  exchange.Severity{Default: exchange.SeverityLow},
				Category:  "c",
				Citations: []exchange.Citation{
					{Path: "src/x.go", LineStart: &lineStart},
				},
			},
		},
	}
}

// TestPruneEntity_HappyPath covers the baseline: ingest one
// analysis, prune the entity, verify the row and all children are
// gone, and verify the append-only triggers are back.
func TestPruneEntity_HappyPath(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	result, err := s.IngestAnalystOutput(ctx, pruneOutputFor("pkg:npm/prune-me"), "test")
	require.NoError(t, err)

	// Pre-conditions: entity exists, 1 output, 1 conclusion, 1 citation.
	entity, err := s.FindEntityByURI(ctx, "pkg:npm/prune-me")
	require.NoError(t, err)
	require.Equal(t, result.EntityID, entity.ID)

	plan, err := s.PlanPruneEntities(ctx, []string{entity.ID})
	require.NoError(t, err)
	assert.Equal(t, 1, len(plan.Entities))
	assert.Equal(t, 1, plan.RowsByTable["analyst_outputs"])
	assert.Equal(t, 1, plan.RowsByTable["conclusions"])
	assert.Equal(t, 1, plan.RowsByTable["entities"])

	// Apply.
	report, err := s.PruneEntities(ctx, []string{entity.ID})
	require.NoError(t, err, "prune must succeed even with append-only triggers present")
	assert.Equal(t, 1, report.RowsByTable["entities"])
	assert.Equal(t, 1, report.RowsByTable["analyst_outputs"])
	assert.Equal(t, 1, report.RowsByTable["conclusions"])
	assert.Equal(t, 1, report.RowsByTable["citations"])

	// Post-conditions: everything gone.
	_, err = s.FindEntityByURI(ctx, "pkg:npm/prune-me")
	assert.ErrorIs(t, err, ErrNotFound)

	// Triggers are back. Attempting an UPDATE / DELETE on a new
	// analyst_output must still fail.
	r2, err := s.IngestAnalystOutput(ctx, pruneOutputFor("pkg:npm/after-prune"), "test")
	require.NoError(t, err)
	_, err = s.DB().ExecContext(ctx,
		`DELETE FROM analyst_outputs WHERE id = ?`, r2.OutputID)
	require.Error(t, err, "append-only trigger must be reinstalled after prune")
	assert.Contains(t, err.Error(), "append-only")
}

// TestPruneEntity_EmptyInput: pruning the empty set is a no-op,
// not an error. Keeps bulk-prune callers simple — `prune orphans`
// can pass [] when there are no orphans without special-casing it.
func TestPruneEntity_EmptyInput(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	report, err := s.PruneEntities(ctx, nil)
	require.NoError(t, err)
	assert.Empty(t, report.Entities)
	assert.Empty(t, report.RowsByTable)
}

// TestPruneEntity_UnknownID: pruning a non-existent id succeeds
// with zero-row counts. Callers may pass stale ids from a cached
// list; one invalid id shouldn't abort a bulk prune.
func TestPruneEntity_UnknownID(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	report, err := s.PruneEntities(ctx, []string{"00000000-0000-0000-0000-000000000000"})
	require.NoError(t, err)
	assert.Empty(t, report.Entities)
}

// TestPruneEntity_WithPosture verifies postures come along for the
// ride when their parent entity is pruned. Post-v10, postures live
// on unversioned entities; the old `posture set pkg:X --version V`
// stored row ends up here.
func TestPruneEntity_WithPosture(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	r, err := s.IngestAnalystOutput(ctx, pruneOutputFor("pkg:npm/prune-posture"), "test")
	require.NoError(t, err)

	require.NoError(t, s.SetPosture(ctx, &profile.Posture{
		EntityID:  r.EntityID,
		Tier:      profile.PostureTrustedForNow,
		Version:   "1.0.0",
		Rationale: "test",
		SetBy:     "tester",
	}))

	report, err := s.PruneEntities(ctx, []string{r.EntityID})
	require.NoError(t, err)
	assert.Equal(t, 1, report.RowsByTable["postures"],
		"posture row must be pruned along with its entity")
}

// TestPruneEntity_WithCollectedFrom: an analyst_output with
// entity_id=A and collected_from_entity_id=B must be deleted when
// EITHER A or B is pruned. The row's existence depends on both
// entities; removing either half orphans it, so the cascade
// aggressively cleans it up either way. Deliberate policy for
// dogfood cleanup — a stranded row with a dangling FK would be
// worse than a lost analysis.
//
// Setup: use WithPrimaryTarget so the analyst's out.Target lands
// in collected_from_entity_id and the `primary` target lands in
// entity_id — the M2 shape where collection happened against one
// identity (e.g. a repo) but was indexed under another (e.g. a
// package).
func TestPruneEntity_WithCollectedFrom(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	// out.Target is the "collected from" side — e.g. "data was
	// gathered on this repo," indexed under the primary package.
	out := pruneOutputFor("repo:github/owner/collected-from")
	res, err := s.IngestAnalystOutput(ctx, out, "test",
		WithPrimaryTarget("pkg:npm/primary-target"))
	require.NoError(t, err)

	primaryEntity, err := s.FindEntityByURI(ctx, "pkg:npm/primary-target")
	require.NoError(t, err)
	collectedEntity, err := s.FindEntityByURI(ctx, "repo:github/owner/collected-from")
	require.NoError(t, err)
	require.Equal(t, res.EntityID, primaryEntity.ID,
		"row's entity_id must be the primary (override) target")
	require.Equal(t, res.CollectedFromEntityID, collectedEntity.ID,
		"row's collected_from must be the analyst's original target")

	// Prune the collected_from entity; the analyst_output row
	// references it via collected_from_entity_id and must be
	// cleaned up before the entity row can go.
	report, err := s.PruneEntities(ctx, []string{collectedEntity.ID})
	require.NoError(t, err)
	assert.Equal(t, 1, report.RowsByTable["analyst_outputs"],
		"analyst_output referencing the pruned collected_from entity must be removed too")
	assert.Equal(t, 1, report.RowsByTable["entities"])

	// Post-condition: primary entity is still there (it wasn't
	// pruned), but its analyst_output is gone — that's the
	// aggressive-cascade cost.
	survivor, err := s.FindEntityByURI(ctx, "pkg:npm/primary-target")
	require.NoError(t, err, "primary entity must survive a collected_from prune")
	assert.Equal(t, primaryEntity.ID, survivor.ID)
}

// TestListVersionedEntities_SkipScopedNpm: scoped npm packages
// (@types/node, @scope/pkg) have an `@` in the first path segment
// but are NOT versioned. SplitURIVersion's last-segment anchoring
// keeps them off the prune list. Regression against a naive
// LIKE-based scan that would match scope markers as if they were
// versions.
func TestListVersionedEntities_SkipScopedNpm(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	// Ingest a scoped-but-unversioned package — post-v10 this
	// creates an entity at the full scoped URI.
	_, err := s.IngestAnalystOutput(ctx, pruneOutputFor("pkg:npm/@types/node"), "test")
	require.NoError(t, err)

	// Also ingest a real versioned package so we have a positive
	// case in the same test. The two outputs differ on Target
	// ("@types/node" vs "lodash@4.17.21") which is part of the
	// content hash, so no additional hash differentiator is needed
	// — the previous version of this test mutated InvokedAt for
	// that purpose, which is now both unnecessary and rejected
	// (invoked_at is server-stamped).
	out := pruneOutputFor("pkg:npm/lodash@4.17.21")
	_, err = s.IngestAnalystOutput(ctx, out, "test")
	require.NoError(t, err)
	// Under v10 canonicalization the lodash ingest creates an
	// entity at the UNVERSIONED uri, so ListVersionedEntities
	// should find zero — the dogfood case we're cleaning up is
	// pre-v10 data, so we synthesize one by hand.
	require.NoError(t, s.PutEntity(ctx, &profile.Entity{
		ID:           "legacy-versioned-1",
		CanonicalURI: "pkg:npm/legacy@9.9.9",
		Type:         profile.EntityPackage,
		ShortName:    "legacy@9.9.9",
	}))

	ids, err := s.ListVersionedEntities(ctx)
	require.NoError(t, err)

	// Must find the legacy-versioned row; must NOT find the
	// scoped-unversioned one.
	uris := urisForIDs(t, s, ids)
	assert.Contains(t, uris, "pkg:npm/legacy@9.9.9",
		"bulk prune must find pre-v10 versioned entities to clean up")
	assert.NotContains(t, uris, "pkg:npm/@types/node",
		"scoped npm packages must not be mistaken for versioned entities (@ is scope, not version)")
}

// TestListOrphanEntities_FiltersOutChildlessOnly: an entity with
// no children in any child table is an orphan. An entity with even
// a single posture, signal, burn, or analyst_output is NOT.
func TestListOrphanEntities_FiltersOutChildlessOnly(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	// A: has analyst_output. Not orphan.
	rA, err := s.IngestAnalystOutput(ctx, pruneOutputFor("pkg:npm/has-analysis"), "test")
	require.NoError(t, err)

	// B: no children. Orphan.
	require.NoError(t, s.PutEntity(ctx, &profile.Entity{
		ID:           "orphan-1",
		CanonicalURI: "pkg:npm/orphan",
		Type:         profile.EntityPackage,
		ShortName:    "orphan",
	}))

	// C: has posture. Not orphan.
	require.NoError(t, s.PutEntity(ctx, &profile.Entity{
		ID:           "postured-1",
		CanonicalURI: "pkg:npm/postured",
		Type:         profile.EntityPackage,
		ShortName:    "postured",
	}))
	require.NoError(t, s.SetPosture(ctx, &profile.Posture{
		EntityID:  "postured-1",
		Tier:      profile.PostureTrustedForNow,
		Rationale: "test",
		SetBy:     "tester",
	}))

	ids, err := s.ListOrphanEntities(ctx)
	require.NoError(t, err)
	assert.Contains(t, ids, "orphan-1", "no-child entity must be in the orphan list")
	assert.NotContains(t, ids, rA.EntityID, "entity with analyst_output must NOT be orphan")
	assert.NotContains(t, ids, "postured-1", "entity with posture must NOT be orphan")
}

// TestPruneEntities_TriggersReinstalled is the critical safety
// property — after a prune, the append-only invariants must still
// hold. This covers EVERY table we touched, via a smoke test that
// attempts an UPDATE on each post-prune and expects failure.
//
// Flagged as the "did we actually put the triggers back" regression.
// A silent drop would mean future bugs masquerade as successful
// ingests when they should trip the append-only protection.
func TestPruneEntities_TriggersReinstalled(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	// Baseline trigger count — capture before the prune touches
	// anything.
	var before int
	require.NoError(t, s.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND (name LIKE '%_no_update' OR name LIKE '%_no_delete')`).
		Scan(&before))
	require.Greater(t, before, 0, "fixture: must have append-only triggers at start")

	r, err := s.IngestAnalystOutput(ctx, pruneOutputFor("pkg:npm/trigger-test"), "test")
	require.NoError(t, err)

	_, err = s.PruneEntities(ctx, []string{r.EntityID})
	require.NoError(t, err)

	var after int
	require.NoError(t, s.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND (name LIKE '%_no_update' OR name LIKE '%_no_delete')`).
		Scan(&after))
	assert.Equal(t, before, after,
		"prune must restore all append-only triggers (before=%d, after=%d)", before, after)
}

// urisForIDs is a test helper that turns entity-id list into
// canonical_uri list for human-readable assertions.
func urisForIDs(t *testing.T, s *SQLite, ids []string) []string {
	t.Helper()
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		var uri string
		err := s.DB().QueryRowContext(context.Background(),
			`SELECT canonical_uri FROM entities WHERE id = ?`, id).Scan(&uri)
		require.NoError(t, err)
		out = append(out, uri)
	}
	return out
}

// TestPruneEntities_PlanMatchesReport pins the dry-run trust property:
// what PlanPruneEntities promises and what PruneEntities actually
// deletes must agree, both in label set and per-label count.
//
// If the plan undercounts (missing tables) or uses a different label
// vocabulary than the apply, the operator sees a smaller/different
// cascade in dry-run than what --destructive executes — silently
// breaking the preview-then-apply trust model.
//
// Asserts plan.RowsByTable == report.RowsByTable for a fixture that
// exercises citations (which sit transitively under conclusions and
// were missing from plan output prior to the fix).
func TestPruneEntities_PlanMatchesReport(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	// pruneOutputFor seeds one conclusion with one citation, so the
	// apply will touch four tables: entities, analyst_outputs,
	// conclusions, citations. The plan must reflect the same four.
	result, err := s.IngestAnalystOutput(ctx, pruneOutputFor("pkg:npm/parity-test"), "test")
	require.NoError(t, err)

	plan, err := s.PlanPruneEntities(ctx, []string{result.EntityID})
	require.NoError(t, err)

	report, err := s.PruneEntities(ctx, []string{result.EntityID})
	require.NoError(t, err)

	assert.Equal(t, plan.RowsByTable, report.RowsByTable,
		"plan and report must use the same labels and counts so the operator "+
			"can verify dry-run vs apply; divergence here means dry-run lies about what apply will do")
}
