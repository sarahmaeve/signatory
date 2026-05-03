package gem

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

// railsFixture returns a realistic GemResponse modeled on rails.
func railsFixture() GemResponse {
	return GemResponse{
		Name:             "rails",
		Downloads:        500_000_000,
		VersionDownloads: 2_000_000,
		Version:          "7.1.3",
		VersionCreatedAt: "2024-01-16T10:00:00Z",
		CreatedAt:        "2004-07-01T00:00:00Z",
		Authors:          "David Heinemeier Hansson",
		SourceCodeURI:    "https://github.com/rails/rails",
		HomepageURI:      "https://rubyonrails.org",
		MFARequired:      true,
	}
}

// railsVersions returns version entries for rails.
func railsVersions() []VersionEntry {
	return []VersionEntry{
		{Number: "7.1.3", CreatedAt: "2024-01-16T10:00:00Z", Platform: "ruby", Authors: "David Heinemeier Hansson"},
		{Number: "7.1.2", CreatedAt: "2023-11-10T10:00:00Z", Platform: "ruby", Authors: "David Heinemeier Hansson"},
		{Number: "7.1.1", CreatedAt: "2023-10-11T10:00:00Z", Platform: "ruby", Authors: "David Heinemeier Hansson"},
		{Number: "7.1.0", CreatedAt: "2023-10-05T10:00:00Z", Platform: "ruby", Authors: "David Heinemeier Hansson"},
		{Number: "7.0.8", CreatedAt: "2023-09-01T10:00:00Z", Yanked: true, Platform: "ruby", Authors: "David Heinemeier Hansson"},
	}
}

// railsOwners returns the owners fixture.
func railsOwners() []OwnerEntry {
	return []OwnerEntry{
		{Handle: "dhh"},
		{Handle: "rafaelfranca"},
		{Handle: "tenderlove"},
	}
}

func TestCollector_Name(t *testing.T) {
	t.Parallel()
	c := NewCollector()
	assert.Equal(t, "gem-registry", c.Name())
}

func TestCollector_NonGemEntity(t *testing.T) {
	t.Parallel()

	c := NewCollector()
	entity := &profile.Entity{
		ID:           "test-npm",
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
		case "/api/v1/gems/rails.json":
			json.NewEncoder(w).Encode(railsFixture()) //nolint:errcheck
		case "/api/v1/versions/rails.json":
			json.NewEncoder(w).Encode(railsVersions()) //nolint:errcheck
		case "/api/v1/gems/rails/owners.json":
			json.NewEncoder(w).Encode(railsOwners()) //nolint:errcheck
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClientWithBaseURL(srv.URL)
	c := NewCollectorWithClient(client)

	entity := &profile.Entity{
		ID:           "test-rails",
		CanonicalURI: "pkg:gem/rails",
		Ecosystem:    "gem",
	}

	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)

	// Should emit: last_publish, version_count, recent_downloads,
	// maintainer_count, owner_count, yanked_release_count, mfa_required.
	// Total: 7 signals minimum.
	assert.GreaterOrEqual(t, result.SignalCount(), 7,
		"expected at least 7 signals, got %d", result.SignalCount())

	signals := result.Signals()
	signalMap := map[string]json.RawMessage{}
	for _, s := range signals {
		signalMap[s.Type] = s.Value
	}

	// last_publish
	assert.Contains(t, signalMap, "last_publish")
	var lp map[string]any
	require.NoError(t, json.Unmarshal(signalMap["last_publish"], &lp))
	assert.Equal(t, "7.1.3", lp["latest_version"])

	// version_count
	assert.Contains(t, signalMap, "version_count")
	var vc map[string]any
	require.NoError(t, json.Unmarshal(signalMap["version_count"], &vc))
	assert.Equal(t, float64(5), vc["count"])

	// recent_downloads
	assert.Contains(t, signalMap, "recent_downloads")
	var rd map[string]any
	require.NoError(t, json.Unmarshal(signalMap["recent_downloads"], &rd))
	assert.Equal(t, float64(2_000_000), rd["count"])

	// maintainer_count
	assert.Contains(t, signalMap, "maintainer_count")
	var mc map[string]any
	require.NoError(t, json.Unmarshal(signalMap["maintainer_count"], &mc))
	assert.Equal(t, float64(3), mc["count"])

	// owner_count
	assert.Contains(t, signalMap, "owner_count")

	// yanked_release_count
	assert.Contains(t, signalMap, "yanked_release_count")
	var yr map[string]any
	require.NoError(t, json.Unmarshal(signalMap["yanked_release_count"], &yr))
	assert.Equal(t, float64(1), yr["count"])

	// mfa_required
	assert.Contains(t, signalMap, "mfa_required")
	var mfa map[string]any
	require.NoError(t, json.Unmarshal(signalMap["mfa_required"], &mfa))
	assert.Equal(t, true, mfa["required"])
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
		CanonicalURI: "pkg:gem/nonexistent",
		Ecosystem:    "gem",
	}

	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)
	assert.True(t, result.HasFailures())
}

func TestCollector_OwnersEndpointFailure(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/gems/sidekiq.json":
			json.NewEncoder(w).Encode(GemResponse{
				Name:             "sidekiq",
				VersionDownloads: 500_000,
				Version:          "7.2.0",
				VersionCreatedAt: "2024-02-01T00:00:00Z",
				MFARequired:      false,
			}) //nolint:errcheck
		case "/api/v1/versions/sidekiq.json":
			json.NewEncoder(w).Encode([]VersionEntry{
				{Number: "7.2.0", CreatedAt: "2024-02-01T00:00:00Z", Platform: "ruby"},
			}) //nolint:errcheck
		case "/api/v1/gems/sidekiq/owners.json":
			w.WriteHeader(http.StatusUnauthorized)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClientWithBaseURL(srv.URL)
	c := NewCollectorWithClient(client)

	entity := &profile.Entity{
		ID:           "test-sidekiq",
		CanonicalURI: "pkg:gem/sidekiq",
		Ecosystem:    "gem",
	}

	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)
	// Should still emit gem-info-derived and version-derived signals.
	assert.GreaterOrEqual(t, result.SignalCount(), 4,
		"version-derived signals should still land when owners endpoint fails")
}

func TestCollector_EntityStore_MintsOwnerEntities(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/gems/rails.json":
			json.NewEncoder(w).Encode(railsFixture()) //nolint:errcheck
		case "/api/v1/versions/rails.json":
			json.NewEncoder(w).Encode(railsVersions()) //nolint:errcheck
		case "/api/v1/gems/rails/owners.json":
			json.NewEncoder(w).Encode(railsOwners()) //nolint:errcheck
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClientWithBaseURL(srv.URL)
	store := &mockEntityStore{}
	c := NewCollectorWithClient(client).WithEntityStore(store)

	entity := &profile.Entity{
		ID:           "test-rails",
		CanonicalURI: "pkg:gem/rails",
		Ecosystem:    "gem",
	}

	_, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)

	assert.Contains(t, store.minted, "identity:rubygems/dhh")
	assert.Contains(t, store.minted, "identity:rubygems/rafaelfranca")
	assert.Contains(t, store.minted, "identity:rubygems/tenderlove")
}

// mockEntityStore tracks which entity URIs were minted.
type mockEntityStore struct {
	minted []string
}

func (m *mockEntityStore) EnsureEntityByCanonicalURI(_ context.Context, uri, shortName string) (*profile.Entity, bool, error) {
	m.minted = append(m.minted, uri)
	return &profile.Entity{ID: "mock-" + shortName, CanonicalURI: uri}, true, nil
}
