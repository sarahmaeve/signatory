package cargo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
)

// serdeFixture returns a realistic CrateResponse modeled on serde.
func serdeFixture() CrateResponse {
	return CrateResponse{
		Crate: Crate{
			Name:            "serde",
			Repository:      "https://github.com/serde-rs/serde",
			Downloads:       150_000_000,
			RecentDownloads: 12_000_000,
			CreatedAt:       "2015-02-26T01:00:00Z",
			MaxStableVer:    "1.0.219",
		},
		Versions: []Version{
			{
				Num: "1.0.219", CreatedAt: "2025-12-01T10:00:00Z",
				Yanked: false, PublishedBy: &User{Login: "dtolnay"},
				HasBuildScript: true,
			},
			{
				Num: "1.0.218", CreatedAt: "2025-11-15T10:00:00Z",
				Yanked: false, PublishedBy: &User{Login: "dtolnay"},
				HasBuildScript: true,
			},
			{
				Num: "1.0.217", CreatedAt: "2025-11-01T10:00:00Z",
				Yanked: false, PublishedBy: &User{Login: "dtolnay"},
				HasBuildScript: true,
			},
			{
				Num: "1.0.216", CreatedAt: "2025-10-15T10:00:00Z",
				Yanked: true, PublishedBy: &User{Login: "dtolnay"},
				HasBuildScript: true,
			},
		},
	}
}

// serdeOwners returns the owners fixture.
func serdeOwners() OwnersResponse {
	return OwnersResponse{
		Users: []Owner{
			{Login: "dtolnay", Kind: "user", Name: "David Tolnay"},
			{Login: "rust-bus", Kind: "team", Name: "Rust Bus Factor"},
		},
	}
}

func TestCollector_Name(t *testing.T) {
	t.Parallel()
	c := NewCollector()
	assert.Equal(t, "cargo-registry", c.Name())
}

func TestCollector_NonCargoEntity(t *testing.T) {
	t.Parallel()

	c := NewCollector()
	entity := &profile.Entity{
		ID:           "test-npm-entity",
		CanonicalURI: "pkg:npm/express",
		Ecosystem:    "npm",
	}
	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)
	assert.Equal(t, 0, result.SignalCount())
}

func TestCollector_NilEntity(t *testing.T) {
	t.Parallel()

	c := NewCollector()
	result, err := c.Collect(context.Background(), nil)
	require.NoError(t, err)
	assert.Equal(t, 0, result.SignalCount())
}

func TestCollector_Success(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/crates/serde":
			json.NewEncoder(w).Encode(serdeFixture()) //nolint:errcheck
		case "/api/v1/crates/serde/owners":
			json.NewEncoder(w).Encode(serdeOwners()) //nolint:errcheck
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClientWithBaseURL(srv.URL)
	c := NewCollectorWithClient(client)

	entity := &profile.Entity{
		ID:           "test-serde",
		CanonicalURI: "pkg:cargo/serde",
		Ecosystem:    "cargo",
	}

	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)

	// Should emit: last_publish, maintainer_count, recent_downloads,
	// build_script_present, yanked_release_count, owner_count,
	// owner_team_present, publish_origin_consistency,
	// build_script_introduced.
	// Total: 9 signals.
	assert.GreaterOrEqual(t, result.SignalCount(), 9,
		"expected at least 9 signals, got %d: %s", result.SignalCount(), result.Summary())

	// Verify specific signals by type.
	signals := result.Signals()
	signalMap := map[string]json.RawMessage{}
	for _, s := range signals {
		signalMap[s.Type] = s.Value
	}

	// last_publish
	assert.Contains(t, signalMap, "last_publish")

	// maintainer_count from owners endpoint
	assert.Contains(t, signalMap, "maintainer_count")
	var mc map[string]any
	require.NoError(t, json.Unmarshal(signalMap["maintainer_count"], &mc))
	assert.Equal(t, float64(2), mc["count"])

	// recent_downloads
	assert.Contains(t, signalMap, "recent_downloads")
	var dl map[string]any
	require.NoError(t, json.Unmarshal(signalMap["recent_downloads"], &dl))
	assert.Equal(t, float64(12_000_000), dl["count"])

	// build_script_present
	assert.Contains(t, signalMap, "build_script_present")
	var bs map[string]any
	require.NoError(t, json.Unmarshal(signalMap["build_script_present"], &bs))
	assert.Equal(t, true, bs["present"])

	// yanked_release_count
	assert.Contains(t, signalMap, "yanked_release_count")
	var yr map[string]any
	require.NoError(t, json.Unmarshal(signalMap["yanked_release_count"], &yr))
	assert.Equal(t, float64(1), yr["count"])

	// owner_count (bus-factor)
	assert.Contains(t, signalMap, "owner_count")
	var oc map[string]any
	require.NoError(t, json.Unmarshal(signalMap["owner_count"], &oc))
	assert.Equal(t, float64(2), oc["count"])

	// owner_team_present
	assert.Contains(t, signalMap, "owner_team_present")
	var otp map[string]any
	require.NoError(t, json.Unmarshal(signalMap["owner_team_present"], &otp))
	assert.Equal(t, true, otp["present"])

	// publish_origin_consistency
	assert.Contains(t, signalMap, "publish_origin_consistency")
	var poc map[string]any
	require.NoError(t, json.Unmarshal(signalMap["publish_origin_consistency"], &poc))
	assert.Equal(t, float64(1), poc["distinct_publishers"])

	// build_script_introduced
	assert.Contains(t, signalMap, "build_script_introduced")
	var bsi map[string]any
	require.NoError(t, json.Unmarshal(signalMap["build_script_introduced"], &bsi))
	assert.Equal(t, false, bsi["introduced_recently"],
		"serde always had build.rs — no recent introduction")
}

func TestCollector_NotFound(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	client := NewClientWithBaseURL(srv.URL)
	c := NewCollectorWithClient(client)

	entity := &profile.Entity{
		ID:           "test-missing",
		CanonicalURI: "pkg:cargo/nonexistent",
		Ecosystem:    "cargo",
	}

	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)
	// Should record a failure/absence, not return an error.
	assert.True(t, result.HasFailures())
}

func TestCollector_OwnersEndpointFailure(t *testing.T) {
	t.Parallel()

	// Owners endpoint returns 500 but crate endpoint succeeds.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/crates/serde":
			json.NewEncoder(w).Encode(serdeFixture()) //nolint:errcheck
		case "/api/v1/crates/serde/owners":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClientWithBaseURL(srv.URL)
	c := NewCollectorWithClient(client)

	entity := &profile.Entity{
		ID:           "test-serde",
		CanonicalURI: "pkg:cargo/serde",
		Ecosystem:    "cargo",
	}

	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)
	// Should still emit crate-derived signals even when owners fails.
	assert.GreaterOrEqual(t, result.SignalCount(), 5,
		"crate-derived signals should still land when owners endpoint fails")
}

func TestCollector_BuildScriptIntroduced(t *testing.T) {
	t.Parallel()

	// Simulate a crate where the latest version has a build script but
	// older versions did not — the "build.rs introduced" anomaly.
	resp := CrateResponse{
		Crate: Crate{
			Name:            "suspicious-crate",
			RecentDownloads: 500,
			CreatedAt:       "2024-01-01T00:00:00Z",
			MaxStableVer:    "0.3.0",
		},
		Versions: []Version{
			{
				Num: "0.3.0", CreatedAt: "2026-04-01T10:00:00Z",
				Yanked: false, PublishedBy: &User{Login: "attacker"},
				HasBuildScript: true,
			},
			{
				Num: "0.2.0", CreatedAt: "2025-06-01T10:00:00Z",
				Yanked: false, PublishedBy: &User{Login: "original"},
				HasBuildScript: false,
			},
			{
				Num: "0.1.0", CreatedAt: "2024-06-01T10:00:00Z",
				Yanked: false, PublishedBy: &User{Login: "original"},
				HasBuildScript: false,
			},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/crates/suspicious-crate":
			json.NewEncoder(w).Encode(resp) //nolint:errcheck
		case "/api/v1/crates/suspicious-crate/owners":
			json.NewEncoder(w).Encode(OwnersResponse{
				Users: []Owner{{Login: "attacker", Kind: "user"}},
			}) //nolint:errcheck
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClientWithBaseURL(srv.URL)
	c := NewCollectorWithClient(client)

	entity := &profile.Entity{
		ID:           "test-suspicious",
		CanonicalURI: "pkg:cargo/suspicious-crate",
		Ecosystem:    "cargo",
	}

	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)

	signals := result.Signals()
	signalMap := map[string]json.RawMessage{}
	for _, s := range signals {
		signalMap[s.Type] = s.Value
	}

	// build_script_introduced should detect the transition.
	assert.Contains(t, signalMap, "build_script_introduced")
	var bsi map[string]any
	require.NoError(t, json.Unmarshal(signalMap["build_script_introduced"], &bsi))
	assert.Equal(t, true, bsi["introduced_recently"])
	assert.Equal(t, "0.3.0", bsi["introduced_at_version"])

	// publish_origin_consistency should flag multiple publishers.
	assert.Contains(t, signalMap, "publish_origin_consistency")
	var poc map[string]any
	require.NoError(t, json.Unmarshal(signalMap["publish_origin_consistency"], &poc))
	assert.Equal(t, float64(2), poc["distinct_publishers"],
		"attacker + original = 2 distinct publishers")
}

func TestCollector_EntityStore_MintsPublisherEntities(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/crates/serde":
			json.NewEncoder(w).Encode(serdeFixture()) //nolint:errcheck
		case "/api/v1/crates/serde/owners":
			json.NewEncoder(w).Encode(serdeOwners()) //nolint:errcheck
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClientWithBaseURL(srv.URL)
	store := &mockEntityStore{}
	c := NewCollectorWithClient(client).WithEntityStore(store)

	entity := &profile.Entity{
		ID:           "test-serde",
		CanonicalURI: "pkg:cargo/serde",
		Ecosystem:    "cargo",
	}

	_, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)

	// dtolnay should be minted (appears as publisher AND owner).
	assert.Contains(t, store.minted, "identity:cargo/dtolnay")
}

// mockEntityStore tracks which entity URIs were minted.
type mockEntityStore struct {
	minted []string
}

func (m *mockEntityStore) EnsureEntityByCanonicalURI(_ context.Context, uri, shortName string) (*profile.Entity, bool, error) {
	m.minted = append(m.minted, uri)
	return &profile.Entity{ID: "mock-" + shortName, CanonicalURI: uri}, true, nil
}
