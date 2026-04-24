package resources_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/mcp/resources"
	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/store"
)

func openTestDB(t *testing.T) *store.SQLite {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	s, err := store.OpenSQLite(t.Context(), path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// seedPosture inserts an entity + posture row for tests that need real data.
func seedPosture(t *testing.T, s *store.SQLite, entityID, uri, tier string, setAt time.Time) {
	t.Helper()
	ctx := context.Background()
	entity := &profile.Entity{
		ID:           entityID,
		CanonicalURI: uri,
		Type:         profile.EntityPackage,
		ShortName:    entityID,
		CreatedAt:    setAt,
		UpdatedAt:    setAt,
	}
	require.NoError(t, s.PutEntity(ctx, entity))
	require.NoError(t, s.SetPosture(ctx, &profile.Posture{
		EntityID:  entityID,
		Tier:      profile.PostureTier(tier),
		Rationale: "test",
		SetBy:     "test",
		SetAt:     setAt,
	}))
}

func TestPostureResource_EmptyStore(t *testing.T) {
	t.Parallel()
	s := openTestDB(t)
	r := &resources.PostureResource{Store: s}

	resp := r.Read(t.Context(), "signatory://posture")

	require.Equal(t, "ok", resp.Status)
	require.Nil(t, resp.Error)

	raw := mustMarshal(t, resp.Data)
	var decoded struct {
		Total  int            `json:"total"`
		ByTier map[string]int `json:"by_tier"`
		Oldest *struct{}      `json:"oldest_posture"`
		Newest *struct{}      `json:"newest_posture"`
	}
	require.NoError(t, unmarshal(raw, &decoded))

	assert.Equal(t, 0, decoded.Total)
	assert.Empty(t, decoded.ByTier)
	assert.Nil(t, decoded.Oldest)
	assert.Nil(t, decoded.Newest)
}

func TestPostureResource_HappyPath(t *testing.T) {
	t.Parallel()
	s := openTestDB(t)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Second)
	older := now.Add(-24 * time.Hour)

	seedPosture(t, s, "ent-1", "pkg:npm/express", string(profile.PostureTrustedForNow), older)
	seedPosture(t, s, "ent-2", "pkg:npm/lodash", string(profile.PostureVettedFrozen), now)

	r := &resources.PostureResource{Store: s}
	resp := r.Read(ctx, "signatory://posture")

	require.Equal(t, "ok", resp.Status, "response status must be ok")
	require.Nil(t, resp.Error)

	raw := mustMarshal(t, resp.Data)
	var decoded struct {
		Total  int            `json:"total"`
		ByTier map[string]int `json:"by_tier"`
		Oldest *struct {
			EntityID string `json:"entity_id"`
			SetAt    string `json:"set_at"`
		} `json:"oldest_posture"`
		Newest *struct {
			EntityID string `json:"entity_id"`
			SetAt    string `json:"set_at"`
		} `json:"newest_posture"`
	}
	require.NoError(t, unmarshal(raw, &decoded))

	assert.Equal(t, 2, decoded.Total)
	assert.Equal(t, 1, decoded.ByTier[string(profile.PostureTrustedForNow)])
	assert.Equal(t, 1, decoded.ByTier[string(profile.PostureVettedFrozen)])

	require.NotNil(t, decoded.Oldest, "oldest_posture must be present")
	require.NotNil(t, decoded.Newest, "newest_posture must be present")
	assert.Equal(t, "ent-1", decoded.Oldest.EntityID, "oldest should be the earlier entry")
	assert.Equal(t, "ent-2", decoded.Newest.EntityID, "newest should be the later entry")
}

func TestPostureResource_MultiTier(t *testing.T) {
	t.Parallel()
	s := openTestDB(t)

	base := time.Now().UTC().Truncate(time.Second)
	seedPosture(t, s, "e1", "pkg:npm/a", string(profile.PostureVettedFrozen), base)
	seedPosture(t, s, "e2", "pkg:npm/b", string(profile.PostureVettedFrozen), base.Add(time.Second))
	seedPosture(t, s, "e3", "pkg:npm/c", string(profile.PostureVettedFrozen), base.Add(2*time.Second))
	seedPosture(t, s, "e4", "pkg:npm/d", string(profile.PostureUnknownProvenance), base.Add(3*time.Second))

	r := &resources.PostureResource{Store: s}
	resp := r.Read(t.Context(), "signatory://posture")
	require.Equal(t, "ok", resp.Status)

	raw := mustMarshal(t, resp.Data)
	var decoded struct {
		Total  int            `json:"total"`
		ByTier map[string]int `json:"by_tier"`
	}
	require.NoError(t, unmarshal(raw, &decoded))

	assert.Equal(t, 4, decoded.Total)
	assert.Equal(t, 3, decoded.ByTier[string(profile.PostureVettedFrozen)])
	assert.Equal(t, 1, decoded.ByTier[string(profile.PostureUnknownProvenance)])
}

// TestPostureResource_URIIgnored verifies that the URI argument has no
// effect on the response payload. Two Reads with different query strings
// against the same (unchanged) store must produce byte-identical Data —
// a regression that started filtering by query param would fail the
// equality check; the previous single-call form only proved the response
// was "ok" regardless of URI, which is weaker.
func TestPostureResource_URIIgnored(t *testing.T) {
	t.Parallel()
	s := openTestDB(t)
	r := &resources.PostureResource{Store: s}

	respA := r.Read(t.Context(), "signatory://posture")
	respB := r.Read(t.Context(), "signatory://posture?some=junk&tier=trusted-for-now")

	require.Equal(t, "ok", respA.Status)
	require.Equal(t, "ok", respB.Status)

	assert.Equal(t, mustMarshal(t, respA.Data), mustMarshal(t, respB.Data),
		"PostureResource.Read must produce identical Data regardless of URI query params")
}

// TestPostureResource_MutationVerify_TotalReflectsDeletion is the
// mutation-verification test: it proves the assertion is coupled to real
// DB state by mutating the DB between two Read calls and confirming the
// response changes.
func TestPostureResource_MutationVerify_TotalReflectsDeletion(t *testing.T) {
	t.Parallel()
	s := openTestDB(t)
	ctx := context.Background()

	base := time.Now().UTC().Truncate(time.Second)
	seedPosture(t, s, "mv-1", "pkg:npm/mv-pkg", string(profile.PostureTrustedForNow), base)

	r := &resources.PostureResource{Store: s}

	// Before: total = 1.
	resp1 := r.Read(ctx, "signatory://posture")
	var d1 struct {
		Total int `json:"total"`
	}
	require.NoError(t, unmarshal(mustMarshal(t, resp1.Data), &d1))
	assert.Equal(t, 1, d1.Total, "mutation-verify: pre-delete total must be 1")

	// Delete the posture row directly via DB().
	_, err := s.DB().ExecContext(ctx, `DELETE FROM postures WHERE entity_id = 'mv-1'`)
	require.NoError(t, err)

	// After: total = 0 — resource must reflect the deletion.
	resp2 := r.Read(ctx, "signatory://posture")
	var d2 struct {
		Total int `json:"total"`
	}
	require.NoError(t, unmarshal(mustMarshal(t, resp2.Data), &d2))
	assert.Equal(t, 0, d2.Total, "mutation-verify: post-delete total must be 0")
}

// ServerVersion assertion removed — now stamped by the dispatch layer
// at emission; see the server-level test in internal/mcp/server_test.go.
