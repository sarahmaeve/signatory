package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
)

// seedConsolidationEntity inserts a minimal entity row at the given
// canonical_uri and returns the new row's id. Tests use this to
// construct fragmented store states without going through ingest
// (which would auto-canonicalize and prevent the fragmentation
// shapes we need to test).
func seedConsolidationEntity(t *testing.T, s *SQLite, uri string) string {
	t.Helper()
	id := profile.NewEntityID()
	now := time.Now().UTC()
	require.NoError(t, s.PutEntity(context.Background(), &profile.Entity{
		ID:           id,
		CanonicalURI: uri,
		Type:         profile.EntityProject,
		ShortName:    "x",
		CreatedAt:    now,
		UpdatedAt:    now,
	}))
	return id
}

// insertAnalystOutputDirect bypasses IngestAnalystOutput's
// canonicalization to insert one analyst_output row with the exact
// entity_id and collected_from_entity_id specified. Tests use this
// to construct fragmentation shapes (e.g. an output whose
// collected_from points at a stub entity) that the production
// ingest path would prevent.
//
// Defined as a method on *SQLite (in _test.go so it's only visible
// to tests in the store package) so test fixtures can hand the row
// the precise child-relationship state needed to exercise
// ApplyConsolidation's retarget paths.
func (s *SQLite) insertAnalystOutputDirect(ctx context.Context, entityID, collectedFromID string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	id := profile.NewEntityID()
	hash := id // unique per row; content_hash has a UNIQUE constraint, so just reuse the row id
	var collectedFrom any
	if collectedFromID != "" {
		collectedFrom = collectedFromID
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO analyst_outputs
			(id, entity_id, analyst_id, model, prompt_version,
			 invoked_at, ingested_at, round, target_commit, round_notes,
			 source_path, content_hash, collected_from_entity_id, target, target_version)
		 VALUES (?, ?, 'test-analyst', 'test-model', '',
		         ?, ?, 1, '', '',
		         '', ?, ?, '', '')`,
		id, entityID, now, now, hash, collectedFrom)
	return err
}

// TestListDuplicateFragmentations_DetectsCaseFoldWithSibling covers
// the BurntSushi/toml dogfood case: a stub entity at the mixed-case
// URI alongside a canonical lowercase entity. Detection must produce
// a single merge op pointing at the canonical sibling.
func TestListDuplicateFragmentations_DetectsCaseFoldWithSibling(t *testing.T) {
	t.Parallel()
	s := newTestDB(t)
	ctx := context.Background()

	stub := seedConsolidationEntity(t, s, "repo:github/BurntSushi/toml")
	canonical := seedConsolidationEntity(t, s, "repo:github/burntsushi/toml")

	plan, err := s.ListDuplicateFragmentations(ctx)
	require.NoError(t, err)
	require.Len(t, plan.Ops, 1, "exactly one fragmentation present")

	op := plan.Ops[0]
	assert.Equal(t, ConsolidationActionMerge, op.Action)
	assert.Equal(t, ConsolidationClassCaseFold, op.Class)
	assert.Equal(t, stub, op.Source.ID)
	assert.Equal(t, "repo:github/BurntSushi/toml", op.Source.CanonicalURI)
	assert.Equal(t, "repo:github/burntsushi/toml", op.CanonicalURI)
	require.NotNil(t, op.CanonicalSibling)
	assert.Equal(t, canonical, op.CanonicalSibling.ID)
}

// TestListDuplicateFragmentations_DetectsCaseFoldRename covers the
// case where a non-canonical row exists but no canonical sibling
// does. The op is a rename (in-place URI update), not a merge.
func TestListDuplicateFragmentations_DetectsCaseFoldRename(t *testing.T) {
	t.Parallel()
	s := newTestDB(t)
	ctx := context.Background()

	stub := seedConsolidationEntity(t, s, "repo:github/Owner/Repo")
	// No canonical sibling at repo:github/owner/repo.

	plan, err := s.ListDuplicateFragmentations(ctx)
	require.NoError(t, err)
	require.Len(t, plan.Ops, 1)

	op := plan.Ops[0]
	assert.Equal(t, ConsolidationActionRename, op.Action)
	assert.Equal(t, ConsolidationClassCaseFold, op.Class)
	assert.Equal(t, stub, op.Source.ID)
	assert.Equal(t, "repo:github/owner/repo", op.CanonicalURI)
	assert.Nil(t, op.CanonicalSibling)
}

// TestListDuplicateFragmentations_DetectsEcosystemPrefix covers the
// pkg:go → pkg:golang class. yaml.v3-shaped split (sibling exists)
// produces a merge; modernc.org-shaped (no sibling) produces a rename.
func TestListDuplicateFragmentations_DetectsEcosystemPrefix(t *testing.T) {
	t.Parallel()
	s := newTestDB(t)
	ctx := context.Background()

	// Merge case: pkg:go/yaml.v3 + pkg:golang/yaml.v3 sibling
	yamlGo := seedConsolidationEntity(t, s, "pkg:go/gopkg.in/yaml.v3")
	yamlGolang := seedConsolidationEntity(t, s, "pkg:golang/gopkg.in/yaml.v3")
	// Rename case: pkg:go/sqlite, no sibling
	sqlite := seedConsolidationEntity(t, s, "pkg:go/modernc.org/sqlite")

	plan, err := s.ListDuplicateFragmentations(ctx)
	require.NoError(t, err)
	require.Len(t, plan.Ops, 2, "yaml (merge) + sqlite (rename)")

	bySource := map[string]ConsolidationOp{}
	for _, op := range plan.Ops {
		bySource[op.Source.ID] = op
	}

	yamlOp := bySource[yamlGo]
	assert.Equal(t, ConsolidationActionMerge, yamlOp.Action)
	assert.Equal(t, ConsolidationClassEcosystemPrefix, yamlOp.Class)
	assert.Equal(t, "pkg:golang/gopkg.in/yaml.v3", yamlOp.CanonicalURI)
	require.NotNil(t, yamlOp.CanonicalSibling)
	assert.Equal(t, yamlGolang, yamlOp.CanonicalSibling.ID)

	sqliteOp := bySource[sqlite]
	assert.Equal(t, ConsolidationActionRename, sqliteOp.Action)
	assert.Equal(t, ConsolidationClassEcosystemPrefix, sqliteOp.Class)
	assert.Equal(t, "pkg:golang/modernc.org/sqlite", sqliteOp.CanonicalURI)
	assert.Nil(t, sqliteOp.CanonicalSibling)
}

// TestListDuplicateFragmentations_DetectsVersionedEntity covers the
// Plan-A M1 violation: <base>@V entity rows. testify shape — both
// the @V row and (in the actual dogfood data) a cross-scheme @V row
// exist; here we test the simpler single-versioned-entity case.
func TestListDuplicateFragmentations_DetectsVersionedEntity(t *testing.T) {
	t.Parallel()
	s := newTestDB(t)
	ctx := context.Background()

	// Versioned entity, no base sibling — pure rename.
	versioned := seedConsolidationEntity(t, s, "repo:github/stretchr/testify@v1.11.1")

	plan, err := s.ListDuplicateFragmentations(ctx)
	require.NoError(t, err)
	require.Len(t, plan.Ops, 1)

	op := plan.Ops[0]
	assert.Equal(t, ConsolidationActionRename, op.Action)
	assert.Equal(t, ConsolidationClassVersionedEntity, op.Class)
	assert.Equal(t, versioned, op.Source.ID)
	assert.Equal(t, "repo:github/stretchr/testify", op.CanonicalURI)
}

// TestListDuplicateFragmentations_FreshStoreIsEmpty: no fragmentation
// in a clean store yields an empty plan. Critical for the no-op-on-
// fresh-checkout case.
func TestListDuplicateFragmentations_FreshStoreIsEmpty(t *testing.T) {
	t.Parallel()
	s := newTestDB(t)
	ctx := context.Background()

	// Seed only canonical entities — no fragmentation.
	seedConsolidationEntity(t, s, "repo:github/alecthomas/kong")
	seedConsolidationEntity(t, s, "pkg:golang/gopkg.in/yaml.v3")
	seedConsolidationEntity(t, s, "pkg:npm/@types/node")

	plan, err := s.ListDuplicateFragmentations(ctx)
	require.NoError(t, err)
	assert.Empty(t, plan.Ops, "canonical-only store must produce an empty plan")
}

// TestListDuplicateFragmentations_NpmScopedNotMisclassified — guards
// against a regression where pkg:npm/@scope/name (with @ in the FIRST
// path segment, not the last) gets misread as a versioned entity by
// SplitURIVersion. The scoped npm form is canonical; no op should fire.
func TestListDuplicateFragmentations_NpmScopedNotMisclassified(t *testing.T) {
	t.Parallel()
	s := newTestDB(t)
	ctx := context.Background()

	seedConsolidationEntity(t, s, "pkg:npm/@types/node")

	plan, err := s.ListDuplicateFragmentations(ctx)
	require.NoError(t, err)
	assert.Empty(t, plan.Ops, "scoped npm packages must NOT be flagged as versioned entities")
}

// TestApplyConsolidation_MergeCaseFoldRetargetsCollectedFrom: real
// BurntSushi/toml shape — the stub entity has 3 analyst_outputs
// pointing at it via collected_from_entity_id; canonical sibling
// holds the actual postures. Apply must:
//   - Retarget the 3 collected_from FKs to the canonical entity
//   - Delete the stub
//   - Preserve canonical's postures and the analyst_outputs themselves
//   - Reinstall append-only triggers
func TestApplyConsolidation_MergeCaseFoldRetargetsCollectedFrom(t *testing.T) {
	t.Parallel()
	s := newTestDB(t)
	ctx := context.Background()

	stub := seedConsolidationEntity(t, s, "repo:github/BurntSushi/toml")
	canonical := seedConsolidationEntity(t, s, "repo:github/burntsushi/toml")

	// Seed 3 analyst_outputs whose entity_id is the canonical row but
	// whose collected_from points at the stub. Mimics the dogfood
	// shape captured 2026-04-28.
	for range 3 {
		require.NoError(t, s.insertAnalystOutputDirect(ctx, canonical, stub))
	}

	// Seed a posture on canonical so we can verify it survives the merge.
	require.NoError(t, s.SetPosture(ctx, &profile.Posture{
		EntityID:  canonical,
		Tier:      profile.PostureTrustedForNow,
		Version:   "v1.6.0",
		Rationale: "must survive the consolidation",
		SetBy:     "test",
		SetAt:     time.Now().UTC(),
	}))

	plan, err := s.ListDuplicateFragmentations(ctx)
	require.NoError(t, err)
	require.Len(t, plan.Ops, 1, "one merge op for the case-fold pair")

	report, err := s.ApplyConsolidation(ctx, plan)
	require.NoError(t, err)
	assert.Equal(t, 1, report.MergedCount, "one merge applied")
	assert.Equal(t, 0, report.RenamedCount)
	assert.GreaterOrEqual(t, report.RowsByTable["analyst_outputs (collected_from)"], 3,
		"all 3 collected_from FKs must be retargeted")

	// Stub is gone.
	_, err = s.GetEntity(ctx, stub)
	assert.ErrorIs(t, err, ErrNotFound, "stub entity must be deleted after merge")

	// Canonical survives.
	canonicalEntity, err := s.GetEntity(ctx, canonical)
	require.NoError(t, err)
	assert.Equal(t, "repo:github/burntsushi/toml", canonicalEntity.CanonicalURI)

	// Posture on canonical survives.
	postures, err := s.GetPostures(ctx, canonical)
	require.NoError(t, err)
	assert.Len(t, postures, 1, "canonical's posture must NOT be touched by the merge")

	// The 3 analyst_outputs must no longer reference the deleted stub.
	var withStubCollectedFrom int
	require.NoError(t, s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM analyst_outputs WHERE collected_from_entity_id = ?`, stub).Scan(&withStubCollectedFrom))
	assert.Equal(t, 0, withStubCollectedFrom,
		"no analyst_output should still reference the deleted stub via collected_from")

	// Post-merge, the 3 rows have entity_id=canonical AND were
	// retargeted to collected_from=canonical, which makes the
	// collected_from link self-referential. applyMerge NULLs those
	// (M2 invariant: collected_from is set only when the analysis was
	// performed against a DIFFERENT identity than entity_id; a self-
	// loop carries no information). End state: rows still belong to
	// canonical via entity_id, with collected_from = NULL.
	var canonicalRowsWithNullCF int
	require.NoError(t, s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM analyst_outputs
		 WHERE entity_id = ? AND collected_from_entity_id IS NULL`, canonical).Scan(&canonicalRowsWithNullCF))
	assert.GreaterOrEqual(t, canonicalRowsWithNullCF, 3,
		"the 3 retargeted rows must end with entity_id=canonical and collected_from=NULL — self-referential M2 links are NULLed by applyMerge")

	// Just to be explicit: no self-referential collected_from rows
	// should exist anywhere post-merge.
	var selfRef int
	require.NoError(t, s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM analyst_outputs WHERE entity_id = collected_from_entity_id`).Scan(&selfRef))
	assert.Equal(t, 0, selfRef,
		"applyMerge must leave no self-referential collected_from links — those carry no information and would clutter cross-walk queries")
}

// TestApplyConsolidation_RenameUpdatesCanonicalURI: the rename case
// for a non-canonical entity with no sibling. The single UPDATE
// changes the entity's canonical_uri; child rows need no retargeting
// (they already reference the same entity ID).
func TestApplyConsolidation_RenameUpdatesCanonicalURI(t *testing.T) {
	t.Parallel()
	s := newTestDB(t)
	ctx := context.Background()

	source := seedConsolidationEntity(t, s, "repo:github/Owner/Repo")

	plan, err := s.ListDuplicateFragmentations(ctx)
	require.NoError(t, err)
	require.Len(t, plan.Ops, 1)
	require.Equal(t, ConsolidationActionRename, plan.Ops[0].Action)

	report, err := s.ApplyConsolidation(ctx, plan)
	require.NoError(t, err)
	assert.Equal(t, 1, report.RenamedCount)
	assert.Equal(t, 0, report.MergedCount)

	// Same entity ID, new canonical_uri.
	got, err := s.GetEntity(ctx, source)
	require.NoError(t, err)
	assert.Equal(t, "repo:github/owner/repo", got.CanonicalURI,
		"rename must change canonical_uri while preserving the row's ID and child references")
}

// TestApplyConsolidation_PreservesAppendOnlyTriggers: after the
// consolidation, the analyst_outputs append-only triggers must be
// back in place. Without this guard, a botched reinstall would
// silently leave the table mutable.
func TestApplyConsolidation_PreservesAppendOnlyTriggers(t *testing.T) {
	t.Parallel()
	s := newTestDB(t)
	ctx := context.Background()

	stub := seedConsolidationEntity(t, s, "repo:github/Owner/Repo")
	require.NoError(t, s.insertAnalystOutputDirect(ctx, stub, ""))

	plan, err := s.ListDuplicateFragmentations(ctx)
	require.NoError(t, err)
	_, err = s.ApplyConsolidation(ctx, plan)
	require.NoError(t, err)

	// Probe: try to UPDATE an analyst_output. The append-only trigger
	// must reject this.
	_, err = s.db.ExecContext(ctx,
		`UPDATE analyst_outputs SET round = round + 1`)
	require.Error(t, err, "append-only trigger must be reinstalled — UPDATE on analyst_outputs must be blocked")
	assert.Contains(t, err.Error(), "append-only")
}

// TestApplyConsolidation_EmptyPlanIsNoOp: a plan with no ops must
// not touch the store, must return zero-valued report. Important
// for the fresh-store case where ListDuplicateFragmentations
// returns an empty plan.
func TestApplyConsolidation_EmptyPlanIsNoOp(t *testing.T) {
	t.Parallel()
	s := newTestDB(t)
	ctx := context.Background()

	report, err := s.ApplyConsolidation(ctx, &ConsolidationPlan{})
	require.NoError(t, err)
	assert.Equal(t, 0, report.MergedCount)
	assert.Equal(t, 0, report.RenamedCount)
	assert.Empty(t, report.RowsByTable)
}
