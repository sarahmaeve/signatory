package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
)

// TestListEntitiesByType pins that ListEntitiesByType returns exactly the
// entities of the requested type — the enumeration `pr-scan summary`
// needs to list every captured patch: entity.
func TestListEntitiesByType(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	patch1 := testEntity("p1", "patch:github/octo/hello/1", "hello#1", now)
	patch1.Type = profile.EntityPatch
	patch2 := testEntity("p2", "patch:github/octo/hello/2", "hello#2", now)
	patch2.Type = profile.EntityPatch
	repo := testEntity("r1", "repo:github/octo/hello", "hello", now)
	repo.Type = profile.EntityProject

	for _, e := range []*profile.Entity{patch1, patch2, repo} {
		require.NoError(t, s.PutEntity(ctx, e))
	}

	patches, err := s.ListEntitiesByType(ctx, profile.EntityPatch)
	require.NoError(t, err)
	require.Len(t, patches, 2)
	assert.ElementsMatch(t,
		[]string{"patch:github/octo/hello/1", "patch:github/octo/hello/2"},
		[]string{patches[0].CanonicalURI, patches[1].CanonicalURI})

	repos, err := s.ListEntitiesByType(ctx, profile.EntityProject)
	require.NoError(t, err)
	require.Len(t, repos, 1)
	assert.Equal(t, "repo:github/octo/hello", repos[0].CanonicalURI)

	// A type with no rows yields an empty (non-error) result.
	ids, err := s.ListEntitiesByType(ctx, profile.EntityIdentity)
	require.NoError(t, err)
	assert.Empty(t, ids)
}
