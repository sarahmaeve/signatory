package resources_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/mcp/resources"
	"github.com/sarahmaeve/signatory/internal/profile"
)

// seedBurn inserts an entity + burn record. Returns the entity_id used.
func seedBurn(t *testing.T, s interface {
	PutEntity(context.Context, *profile.Entity) error
	SetBurn(context.Context, *profile.Burn) error
}, entityID, uri, reason string, burnedAt time.Time) {
	t.Helper()
	ctx := context.Background()
	entity := &profile.Entity{
		ID:           entityID,
		CanonicalURI: uri,
		Type:         profile.EntityPackage,
		ShortName:    entityID,
		CreatedAt:    burnedAt,
		UpdatedAt:    burnedAt,
	}
	require.NoError(t, s.PutEntity(ctx, entity))
	require.NoError(t, s.SetBurn(ctx, &profile.Burn{
		EntityID: entityID,
		Reason:   reason,
		Source:   profile.BurnSourceLocal,
		BurnedAt: burnedAt,
		BurnedBy: "test",
	}))
}

func TestBurnsResource_EmptyStore(t *testing.T) {
	t.Parallel()
	s := openTestDB(t)
	r := &resources.BurnsResource{Store: s}

	resp := r.Read(t.Context(), "signatory://burns")

	require.Equal(t, "ok", resp.Status)
	require.Nil(t, resp.Error)

	// Data should be an empty array, not null.
	raw := mustMarshal(t, resp.Data)
	var arr []interface{}
	require.NoError(t, unmarshal(raw, &arr))
	assert.Empty(t, arr, "empty store should return empty array, not null")
}

func TestBurnsResource_HappyPath(t *testing.T) {
	t.Parallel()
	s := openTestDB(t)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Second)
	seedBurn(t, s, "b-1", "pkg:npm/evil-pkg", "maintainer compromised", now)
	seedBurn(t, s, "b-2", "pkg:npm/bad-lib", "supply chain attack", now.Add(time.Second))

	r := &resources.BurnsResource{Store: s}
	resp := r.Read(ctx, "signatory://burns")

	require.Equal(t, "ok", resp.Status)
	require.Nil(t, resp.Error)

	raw := mustMarshal(t, resp.Data)
	var burns []struct {
		EntityID string `json:"entity_id"`
		Reason   string `json:"reason"`
		Source   string `json:"source"`
		BurnedBy string `json:"burned_by"`
	}
	require.NoError(t, unmarshal(raw, &burns))

	assert.Len(t, burns, 2, "should return both burn records")
	// Verify all entity_ids are present (order may vary)
	ids := map[string]bool{}
	for _, b := range burns {
		ids[b.EntityID] = true
		assert.NotEmpty(t, b.Reason, "reason must be populated")
		assert.Equal(t, string(profile.BurnSourceLocal), b.Source)
	}
	assert.True(t, ids["b-1"])
	assert.True(t, ids["b-2"])
}

// TestBurnsResource_MutationVerify_CountChangesOnInsert is the mutation-
// verification test: proves the assertion is coupled to real DB state by
// inserting a second burn between two Read calls.
func TestBurnsResource_MutationVerify_CountChangesOnInsert(t *testing.T) {
	t.Parallel()
	s := openTestDB(t)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Second)
	seedBurn(t, s, "mv-burn-1", "pkg:npm/first", "first burn", now)

	r := &resources.BurnsResource{Store: s}

	// Before: one burn.
	resp1 := r.Read(ctx, "signatory://burns")
	raw1 := mustMarshal(t, resp1.Data)
	var arr1 []interface{}
	require.NoError(t, unmarshal(raw1, &arr1))
	assert.Len(t, arr1, 1, "mutation-verify: before insert count must be 1")

	// Add a second burn.
	seedBurn(t, s, "mv-burn-2", "pkg:npm/second", "second burn", now.Add(time.Second))

	// After: two burns.
	resp2 := r.Read(ctx, "signatory://burns")
	raw2 := mustMarshal(t, resp2.Data)
	var arr2 []interface{}
	require.NoError(t, unmarshal(raw2, &arr2))
	assert.Len(t, arr2, 2, "mutation-verify: after insert count must be 2")
}

// Metadata.ServerVersion is stamped by the Server's dispatch layer at
// emission time, not by handlers. The server-level test in
// internal/mcp/server_test.go verifies the stamped value is non-empty
// in emitted MCP responses. Handler-level assertions were removed when
// the package-level ServerVersion var was deleted — those tests were
// asserting a convention that moved off the handler boundary.
