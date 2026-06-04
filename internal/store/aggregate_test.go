package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
)

// seedPostureRow inserts an entity + active posture in one shot.
func seedPostureRow(t *testing.T, s *SQLite, id, uri, tier string, at time.Time) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, s.PutEntity(ctx, testEntity(id, uri, id, at)))
	require.NoError(t, s.SetPosture(ctx, &profile.Posture{
		EntityID:  id,
		Tier:      profile.PostureTier(tier),
		Rationale: "test",
		SetBy:     "test",
		SetAt:     at,
	}))
}

// seedDependencyObs inserts a project entity (idempotently), a dependency
// entity, and a dependency_observation linking them — the shape the
// unexamined query keys on.
func seedDependencyObs(t *testing.T, s *SQLite, entityID, uri, projectID string, at time.Time) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.GetEntity(ctx, projectID); err != nil {
		require.NoError(t, s.PutEntity(ctx, testEntity(projectID, "repo:local/"+projectID, projectID, at)))
	}
	require.NoError(t, s.PutEntity(ctx, testEntity(entityID, uri, entityID, at)))
	require.NoError(t, s.AppendDependencyObservations(ctx, []profile.DependencyObservation{{
		ID:         entityID + "-obs",
		ProjectID:  projectID,
		EntityID:   entityID,
		Version:    "1.0.0",
		Direct:     true,
		ObservedAt: at,
		SurveyID:   "survey-1",
	}}))
}

func TestCountPosturesByTier_Empty(t *testing.T) {
	s := newTestDB(t)
	byTier, total, err := s.CountPosturesByTier(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 0, total)
	assert.Empty(t, byTier)
}

func TestCountPosturesByTier_GroupsByTier(t *testing.T) {
	s := newTestDB(t)
	base := time.Now().UTC().Truncate(time.Second)

	seedPostureRow(t, s, "e1", "pkg:npm/a", string(profile.PostureVettedFrozen), base)
	seedPostureRow(t, s, "e2", "pkg:npm/b", string(profile.PostureVettedFrozen), base.Add(time.Second))
	seedPostureRow(t, s, "e3", "pkg:npm/c", string(profile.PostureVettedFrozen), base.Add(2*time.Second))
	seedPostureRow(t, s, "e4", "pkg:npm/d", string(profile.PostureUnknownProvenance), base.Add(3*time.Second))

	byTier, total, err := s.CountPosturesByTier(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 4, total)
	assert.Equal(t, 3, byTier[string(profile.PostureVettedFrozen)])
	assert.Equal(t, 1, byTier[string(profile.PostureUnknownProvenance)])
}

func TestPostureBoundaries_Empty(t *testing.T) {
	s := newTestDB(t)
	oldest, newest, err := s.PostureBoundaries(t.Context())
	require.NoError(t, err)
	assert.Nil(t, oldest)
	assert.Nil(t, newest)
}

func TestPostureBoundaries_OldestAndNewest(t *testing.T) {
	s := newTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	older := now.Add(-24 * time.Hour)

	seedPostureRow(t, s, "ent-old", "pkg:npm/express", string(profile.PostureTrustedForNow), older)
	seedPostureRow(t, s, "ent-new", "pkg:npm/lodash", string(profile.PostureVettedFrozen), now)

	oldest, newest, err := s.PostureBoundaries(t.Context())
	require.NoError(t, err)
	require.NotNil(t, oldest)
	require.NotNil(t, newest)
	assert.Equal(t, "ent-old", oldest.EntityID, "oldest should be the earlier set_at")
	assert.Equal(t, "ent-new", newest.EntityID, "newest should be the later set_at")
}

func TestListUnexaminedEntities_Empty(t *testing.T) {
	s := newTestDB(t)
	got, err := s.ListUnexaminedEntities(t.Context())
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestListUnexaminedEntities_ExcludesPostured(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	// dep-noposture: observed, no posture → must appear.
	seedDependencyObs(t, s, "dep-noposture", "pkg:npm/no-posture", "proj", now)

	// dep-postured: observed AND postured → must be excluded.
	seedDependencyObs(t, s, "dep-postured", "pkg:npm/has-posture", "proj", now.Add(time.Second))
	require.NoError(t, s.SetPosture(ctx, &profile.Posture{
		EntityID:  "dep-postured",
		Tier:      profile.PostureTrustedForNow,
		Rationale: "examined",
		SetBy:     "test",
		SetAt:     now,
	}))

	got, err := s.ListUnexaminedEntities(t.Context())
	require.NoError(t, err)
	require.Len(t, got, 1, "only the entity without a posture should appear")
	assert.Equal(t, "dep-noposture", got[0].EntityID)
	assert.Equal(t, "pkg:npm/no-posture", got[0].CanonicalURI)
	assert.NotEmpty(t, got[0].CreatedAt)
}

// Withdrawn postures are retracted decisions, kept only for audit
// continuity; every active store read filters withdrawn_at = ''. These
// tests pin that the overview aggregations do the same — a withdrawn
// posture must not be counted, must not bound the range, and must not
// keep its entity off the unexamined list.

func TestCountPosturesByTier_ExcludesWithdrawn(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Second)

	seedPostureRow(t, s, "active", "pkg:npm/active", string(profile.PostureVettedFrozen), base)
	seedPostureRow(t, s, "gone", "pkg:npm/gone", string(profile.PostureTrustedForNow), base)
	require.NoError(t, s.WithdrawPosture(ctx, "gone", "", "tester", "decision was wrong", base.Add(time.Minute)))

	byTier, total, err := s.CountPosturesByTier(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, total, "withdrawn posture must not be counted")
	assert.Equal(t, 1, byTier[string(profile.PostureVettedFrozen)])
	assert.Zero(t, byTier[string(profile.PostureTrustedForNow)], "withdrawn tier must be absent")
}

func TestPostureBoundaries_ExcludesWithdrawn(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	base := time.Now().UTC().Truncate(time.Second)

	seedPostureRow(t, s, "active", "pkg:npm/active", string(profile.PostureVettedFrozen), base)
	// Newer posture, then withdrawn — must not become the newest boundary.
	seedPostureRow(t, s, "gone", "pkg:npm/gone", string(profile.PostureTrustedForNow), base.Add(time.Hour))
	require.NoError(t, s.WithdrawPosture(ctx, "gone", "", "tester", "reassessment pending", base.Add(2*time.Hour)))

	oldest, newest, err := s.PostureBoundaries(ctx)
	require.NoError(t, err)
	require.NotNil(t, oldest)
	require.NotNil(t, newest)
	assert.Equal(t, "active", newest.EntityID, "withdrawn row must not be the newest boundary")
	assert.Equal(t, "active", oldest.EntityID, "only the active row should bound the range")
}

func TestListUnexaminedEntities_WithdrawnPostureReappears(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	seedDependencyObs(t, s, "dep", "pkg:npm/dep", "proj", now)
	require.NoError(t, s.SetPosture(ctx, &profile.Posture{
		EntityID:  "dep",
		Tier:      profile.PostureTrustedForNow,
		Rationale: "examined",
		SetBy:     "test",
		SetAt:     now,
	}))
	// Withdraw the decision — the entity is unassessed again.
	require.NoError(t, s.WithdrawPosture(ctx, "dep", "", "tester", "decision was wrong", now.Add(time.Minute)))

	got, err := s.ListUnexaminedEntities(ctx)
	require.NoError(t, err)
	require.Len(t, got, 1, "an entity whose only posture was withdrawn is unexamined again")
	assert.Equal(t, "dep", got[0].EntityID)
}
