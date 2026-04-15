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

// seedDependency records a dependency_observation row so the entity is
// visible to the unexamined query.
//
// The dependency_observations.project_id column has a FK reference to
// entities(id) per migration v2. The helper idempotently ensures the
// project entity exists before appending the observation, so tests
// can pass a stable projectID without worrying about setup order.
func seedDependency(t *testing.T, s interface {
	PutEntity(context.Context, *profile.Entity) error
	AppendDependencyObservations(context.Context, []profile.DependencyObservation) error
	FindEntityByURI(context.Context, string) (*profile.Entity, error)
	GetEntity(context.Context, string) (*profile.Entity, error)
}, entityID, uri, projectID string, observedAt time.Time) {
	t.Helper()
	ctx := context.Background()
	now := observedAt

	// Ensure the project entity exists. FK on dependency_observations.project_id
	// requires a matching entities.id row. Skip if already inserted by a prior
	// seedDependency call sharing the same projectID.
	if _, err := s.GetEntity(ctx, projectID); err != nil {
		require.NoError(t, s.PutEntity(ctx, &profile.Entity{
			ID:           projectID,
			CanonicalURI: "repo:local/" + projectID,
			Type:         profile.EntityProject,
			ShortName:    projectID,
			CreatedAt:    now,
			UpdatedAt:    now,
		}))
	}

	entity := &profile.Entity{
		ID:           entityID,
		CanonicalURI: uri,
		Type:         profile.EntityPackage,
		ShortName:    entityID,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	require.NoError(t, s.PutEntity(ctx, entity))
	require.NoError(t, s.AppendDependencyObservations(ctx, []profile.DependencyObservation{
		{
			ID:         entityID + "-obs",
			ProjectID:  projectID,
			EntityID:   entityID,
			Version:    "1.0.0",
			Direct:     true,
			ObservedAt: observedAt,
			SurveyID:   "survey-1",
		},
	}))
}

func TestUnexaminedResource_EmptyStore(t *testing.T) {
	t.Parallel()
	s := openTestDB(t)
	r := &resources.UnexaminedResource{Store: s}

	resp := r.Read(t.Context(), "signatory://unexamined")

	require.Equal(t, "ok", resp.Status)
	require.Nil(t, resp.Error)

	raw := mustMarshal(t, resp.Data)
	var arr []interface{}
	require.NoError(t, unmarshal(raw, &arr))
	assert.Empty(t, arr)
}

func TestUnexaminedResource_HappyPath(t *testing.T) {
	t.Parallel()
	s := openTestDB(t)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Second)
	projectID := "proj-1"

	// Two entities observed as dependencies.
	seedDependency(t, s, "dep-1", "pkg:npm/unvetted-a", projectID, now.Add(-time.Hour))
	seedDependency(t, s, "dep-2", "pkg:npm/unvetted-b", projectID, now)

	r := &resources.UnexaminedResource{Store: s}
	resp := r.Read(ctx, "signatory://unexamined")

	require.Equal(t, "ok", resp.Status)
	raw := mustMarshal(t, resp.Data)
	var entities []struct {
		EntityID     string `json:"entity_id"`
		CanonicalURI string `json:"canonical_uri"`
		ShortName    string `json:"short_name"`
		CreatedAt    string `json:"created_at"`
	}
	require.NoError(t, unmarshal(raw, &entities))
	assert.Len(t, entities, 2, "both unexamined deps should appear")

	for _, e := range entities {
		assert.NotEmpty(t, e.EntityID)
		assert.NotEmpty(t, e.CanonicalURI)
		assert.NotEmpty(t, e.CreatedAt)
	}
}

func TestUnexaminedResource_ExcludesExaminedEntities(t *testing.T) {
	t.Parallel()
	s := openTestDB(t)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Second)
	projectID := "proj-examined"

	// dep-3: has a dependency observation and NO posture → should appear
	seedDependency(t, s, "dep-3", "pkg:npm/no-posture", projectID, now)

	// dep-4: has a dependency observation AND a posture → must be excluded
	seedDependency(t, s, "dep-4", "pkg:npm/has-posture", projectID, now.Add(time.Second))
	require.NoError(t, s.SetPosture(ctx, &profile.Posture{
		EntityID:  "dep-4",
		Tier:      profile.PostureTrustedForNow,
		Rationale: "manually examined",
		SetBy:     "test",
		SetAt:     now,
	}))

	r := &resources.UnexaminedResource{Store: s}
	resp := r.Read(ctx, "signatory://unexamined")

	require.Equal(t, "ok", resp.Status)
	raw := mustMarshal(t, resp.Data)
	var entities []struct {
		EntityID string `json:"entity_id"`
	}
	require.NoError(t, unmarshal(raw, &entities))

	require.Len(t, entities, 1, "only the entity without a posture should appear")
	assert.Equal(t, "dep-3", entities[0].EntityID)
}

// TestUnexaminedResource_MutationVerify_PostureRemovesFromList is the
// mutation-verification test: adding a posture to a previously unexamined
// entity must cause it to disappear from the list.
func TestUnexaminedResource_MutationVerify_PostureRemovesFromList(t *testing.T) {
	t.Parallel()
	s := openTestDB(t)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Second)
	seedDependency(t, s, "mv-dep-1", "pkg:npm/mv-unexamined", "mv-proj", now)

	r := &resources.UnexaminedResource{Store: s}

	// Before: one unexamined entity.
	resp1 := r.Read(ctx, "signatory://unexamined")
	raw1 := mustMarshal(t, resp1.Data)
	var arr1 []interface{}
	require.NoError(t, unmarshal(raw1, &arr1))
	assert.Len(t, arr1, 1, "mutation-verify: before posture, entity must appear")

	// Add a posture → entity should disappear.
	require.NoError(t, s.SetPosture(ctx, &profile.Posture{
		EntityID:  "mv-dep-1",
		Tier:      profile.PostureVettedFrozen,
		Rationale: "examined now",
		SetBy:     "mutation-test",
		SetAt:     now.Add(time.Minute),
	}))

	resp2 := r.Read(ctx, "signatory://unexamined")
	raw2 := mustMarshal(t, resp2.Data)
	var arr2 []interface{}
	require.NoError(t, unmarshal(raw2, &arr2))
	assert.Empty(t, arr2, "mutation-verify: after posture, entity must not appear")
}

// ServerVersion handler-level assertion removed — stamped at dispatch
// emission, tested in internal/mcp/server_test.go.
