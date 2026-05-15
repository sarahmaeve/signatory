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
	// maintainer_count, owner_count, yanked_release_count, mfa_required,
	// native_extension_present, native_extension_introduced,
	// version_publish_burst, author_drift.
	// Total: 11 signals minimum.
	assert.GreaterOrEqual(t, result.SignalCount(), 11,
		"expected at least 11 signals, got %d", result.SignalCount())

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

// --- Longitudinal signal tests ---

// attackVersions returns a version sequence mimicking the BufferZoneCorp
// campaign: 4 versions in 72 hours, native extension introduced in the
// latest, author drift on the final version.
func attackVersions() []VersionEntry {
	return []VersionEntry{
		{Number: "0.4.0", CreatedAt: "2026-04-14T18:00:00Z", Platform: "x86_64-linux", Authors: "attacker@evil.com", Yanked: false},
		{Number: "0.3.0", CreatedAt: "2026-04-13T12:00:00Z", Platform: "ruby", Authors: "legitimate@example.com", Yanked: false},
		{Number: "0.2.0", CreatedAt: "2026-04-12T18:00:00Z", Platform: "ruby", Authors: "legitimate@example.com", Yanked: false},
		{Number: "0.1.0", CreatedAt: "2026-04-12T06:00:00Z", Platform: "ruby", Authors: "legitimate@example.com", Yanked: false},
	}
}

func TestCollector_NativeExtensionPresent(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/gems/knot-rack.json":
			json.NewEncoder(w).Encode(GemResponse{Name: "knot-rack", VersionDownloads: 100, Version: "0.4.0"}) //nolint:errcheck
		case "/api/v1/versions/knot-rack.json":
			json.NewEncoder(w).Encode(attackVersions()) //nolint:errcheck
		case "/api/v1/gems/knot-rack/owners.json":
			json.NewEncoder(w).Encode([]OwnerEntry{{Handle: "attacker"}}) //nolint:errcheck
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClientWithBaseURL(srv.URL)
	c := NewCollectorWithClient(client)
	entity := &profile.Entity{ID: "test-knot", CanonicalURI: "pkg:gem/knot-rack", Ecosystem: "gem"}

	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)

	signals := result.Signals()
	signalMap := map[string]json.RawMessage{}
	for _, s := range signals {
		signalMap[s.Type] = s.Value
	}

	// native_extension_present: latest version has platform != "ruby"
	require.Contains(t, signalMap, "native_extension_present")
	var nep map[string]any
	require.NoError(t, json.Unmarshal(signalMap["native_extension_present"], &nep))
	assert.Equal(t, true, nep["present"])
	assert.Equal(t, "x86_64-linux", nep["latest_platform"])
}

func TestCollector_NativeExtensionIntroduced(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/gems/knot-rack.json":
			json.NewEncoder(w).Encode(GemResponse{Name: "knot-rack", VersionDownloads: 100, Version: "0.4.0"}) //nolint:errcheck
		case "/api/v1/versions/knot-rack.json":
			json.NewEncoder(w).Encode(attackVersions()) //nolint:errcheck
		case "/api/v1/gems/knot-rack/owners.json":
			json.NewEncoder(w).Encode([]OwnerEntry{{Handle: "attacker"}}) //nolint:errcheck
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClientWithBaseURL(srv.URL)
	c := NewCollectorWithClient(client)
	entity := &profile.Entity{ID: "test-knot", CanonicalURI: "pkg:gem/knot-rack", Ecosystem: "gem"}

	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)

	signals := result.Signals()
	signalMap := map[string]json.RawMessage{}
	for _, s := range signals {
		signalMap[s.Type] = s.Value
	}

	// native_extension_introduced: latest has extension, priors don't
	require.Contains(t, signalMap, "native_extension_introduced")
	var nei map[string]any
	require.NoError(t, json.Unmarshal(signalMap["native_extension_introduced"], &nei))
	assert.Equal(t, true, nei["introduced_recently"])
	assert.Equal(t, "0.4.0", nei["introduced_at_version"])
	assert.Equal(t, float64(3), nei["prior_versions_without"])
	assert.Equal(t, float64(4), nei["versions_checked"])
}

func TestCollector_VersionPublishBurst(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/gems/knot-rack.json":
			json.NewEncoder(w).Encode(GemResponse{Name: "knot-rack", VersionDownloads: 100, Version: "0.4.0"}) //nolint:errcheck
		case "/api/v1/versions/knot-rack.json":
			json.NewEncoder(w).Encode(attackVersions()) //nolint:errcheck
		case "/api/v1/gems/knot-rack/owners.json":
			json.NewEncoder(w).Encode([]OwnerEntry{{Handle: "attacker"}}) //nolint:errcheck
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClientWithBaseURL(srv.URL)
	c := NewCollectorWithClient(client)
	entity := &profile.Entity{ID: "test-knot", CanonicalURI: "pkg:gem/knot-rack", Ecosystem: "gem"}

	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)

	signals := result.Signals()
	signalMap := map[string]json.RawMessage{}
	for _, s := range signals {
		signalMap[s.Type] = s.Value
	}

	// version_publish_burst: 4 versions in ~60h (well under 72h threshold)
	require.Contains(t, signalMap, "version_publish_burst")
	var vpb map[string]any
	require.NoError(t, json.Unmarshal(signalMap["version_publish_burst"], &vpb))
	assert.Equal(t, true, vpb["burst_detected"])
	assert.Equal(t, float64(4), vpb["versions_in_window"])
}

func TestCollector_AuthorDrift(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/gems/knot-rack.json":
			json.NewEncoder(w).Encode(GemResponse{Name: "knot-rack", VersionDownloads: 100, Version: "0.4.0"}) //nolint:errcheck
		case "/api/v1/versions/knot-rack.json":
			json.NewEncoder(w).Encode(attackVersions()) //nolint:errcheck
		case "/api/v1/gems/knot-rack/owners.json":
			json.NewEncoder(w).Encode([]OwnerEntry{{Handle: "attacker"}}) //nolint:errcheck
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClientWithBaseURL(srv.URL)
	c := NewCollectorWithClient(client)
	entity := &profile.Entity{ID: "test-knot", CanonicalURI: "pkg:gem/knot-rack", Ecosystem: "gem"}

	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)

	signals := result.Signals()
	signalMap := map[string]json.RawMessage{}
	for _, s := range signals {
		signalMap[s.Type] = s.Value
	}

	// author_drift: latest version has a different author than priors
	require.Contains(t, signalMap, "author_drift")
	var ad map[string]any
	require.NoError(t, json.Unmarshal(signalMap["author_drift"], &ad))
	assert.Equal(t, float64(2), ad["distinct_authors"])
	assert.Equal(t, float64(4), ad["versions_checked"])
}

// TestCollector_Longitudinal_HealthyGem verifies that the rails fixture
// (stable, single author, pure Ruby, spread-out publishing) produces
// clean/benign values for all longitudinal signals.
func TestCollector_Longitudinal_HealthyGem(t *testing.T) {
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
	entity := &profile.Entity{ID: "test-rails", CanonicalURI: "pkg:gem/rails", Ecosystem: "gem"}

	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)

	signals := result.Signals()
	signalMap := map[string]json.RawMessage{}
	for _, s := range signals {
		signalMap[s.Type] = s.Value
	}

	// native_extension_present: all ruby platform → not present
	require.Contains(t, signalMap, "native_extension_present")
	var nep map[string]any
	require.NoError(t, json.Unmarshal(signalMap["native_extension_present"], &nep))
	assert.Equal(t, false, nep["present"])

	// native_extension_introduced: never had one → not introduced
	require.Contains(t, signalMap, "native_extension_introduced")
	var nei map[string]any
	require.NoError(t, json.Unmarshal(signalMap["native_extension_introduced"], &nei))
	assert.Equal(t, false, nei["introduced_recently"])

	// version_publish_burst: versions spread over months → no burst
	require.Contains(t, signalMap, "version_publish_burst")
	var vpb map[string]any
	require.NoError(t, json.Unmarshal(signalMap["version_publish_burst"], &vpb))
	assert.Equal(t, false, vpb["burst_detected"])

	// author_drift: single author throughout → 1 distinct author
	require.Contains(t, signalMap, "author_drift")
	var ad map[string]any
	require.NoError(t, json.Unmarshal(signalMap["author_drift"], &ad))
	assert.Equal(t, float64(1), ad["distinct_authors"])
}

// mockEntityStore tracks which entity URIs were minted.
type mockEntityStore struct {
	minted []string
}

func (m *mockEntityStore) EnsureEntityByCanonicalURI(_ context.Context, uri, shortName string) (*profile.Entity, bool, error) {
	m.minted = append(m.minted, uri)
	return &profile.Entity{ID: "mock-" + shortName, CanonicalURI: uri}, true, nil
}

// TestCollector_RecordsArtifactURL exercises the producer side of
// the artifact-vs-repo divergence flow for gem. The gem registry
// collector must emit an artifact_url signal carrying the
// constructed `.gem` download URL, version, and integrity
// (the SHA256 from the version entry). git_head is empty —
// rubygems.org does not expose a publisher-stamped commit in
// registry metadata, so the downstream artifact collector falls
// through to tag-match resolution.
//
// The latest non-yanked, non-prerelease, ruby-platform version is
// the one chosen: native-platform builds (e.g. "x86_64-linux") are
// pre-compiled artifacts whose contents diverge from source by
// design and would produce noise in the diff.
func TestCollector_RecordsArtifactURL(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/gems/rails.json":
			json.NewEncoder(w).Encode(railsFixture()) //nolint:errcheck
		case "/api/v1/versions/rails.json":
			json.NewEncoder(w).Encode([]VersionEntry{
				{
					Number: "7.1.3", CreatedAt: "2024-01-16T10:00:00Z",
					Platform: "ruby", Authors: "DHH",
					SHA: "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
				},
				{
					Number: "7.1.2", CreatedAt: "2023-11-10T10:00:00Z",
					Platform: "ruby", Authors: "DHH",
					SHA: "1111111111111111111111111111111111111111111111111111111111111111",
				},
			}) //nolint:errcheck
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

	signals := result.Signals()
	signalMap := map[string]json.RawMessage{}
	for _, s := range signals {
		signalMap[s.Type] = s.Value
	}

	require.Contains(t, signalMap, "artifact_url",
		"gem registry collector must emit artifact_url so the "+
			"artifact-vs-repo collector can fetch and pair the .gem")

	var au map[string]any
	require.NoError(t, json.Unmarshal(signalMap["artifact_url"], &au))

	assert.Equal(t, "https://rubygems.org/downloads/rails-7.1.3.gem", au["url"],
		"rubygems.org .gem URL is constructed from name + version; the "+
			"canonical form is /downloads/{name}-{version}.gem")
	assert.Equal(t, "7.1.3", au["version"],
		"version is the latest non-yanked, non-prerelease, ruby-platform "+
			"publish; downstream tag-match resolver pairs by this")
	assert.Equal(t, "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789", au["integrity"],
		"integrity is the rubygems.org-supplied sha256 of the .gem")
	assert.Equal(t, "", au["git_head"],
		"rubygems.org does not expose git_head in registry metadata; "+
			"the artifact collector falls through to tag-match resolution")
}

// TestCollector_GemDependencies_EmitsUniformShape pins that the gem
// collector emits gem_dependencies with a value shape byte-identical
// to the other *_dependencies signals (direct_count, indirect_count,
// total_count, direct[]), so the deltas diff engine surfaces gem
// dependency drift through the same set-diff path. direct is the
// sorted, de-duplicated set of runtime dependency names from the gem
// JSON's `dependencies` object — already fetched via GetGem, no new
// request. development dependencies are excluded (the dev analog, not
// consumed by downstream gems, mirroring npm devDependencies / cargo
// dev / maven test). indirect_count is always 0: the gem JSON exposes
// only the displayed version's direct deps, never the resolved graph.
func TestCollector_GemDependencies_EmitsUniformShape(t *testing.T) {
	t.Parallel()

	gem := railsFixture()
	gem.Dependencies = GemDependencies{
		Runtime: []GemDependency{
			{Name: "actionpack", Requirements: "= 7.1.3"},
			{Name: "activesupport", Requirements: "= 7.1.3"},
			{Name: "actionpack", Requirements: "= 7.1.3"}, // dup → de-duped
		},
		Development: []GemDependency{
			{Name: "rspec", Requirements: ">= 0"},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/gems/rails.json":
			json.NewEncoder(w).Encode(gem) //nolint:errcheck
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

	signalMap := map[string]json.RawMessage{}
	for _, s := range result.Signals() {
		signalMap[s.Type] = s.Value
	}
	require.Contains(t, signalMap, "gem_dependencies",
		"gem_dependencies must be emitted for a gem with declared runtime deps")

	var dep map[string]any
	require.NoError(t, json.Unmarshal(signalMap["gem_dependencies"], &dep))

	keys := make([]string, 0, len(dep))
	for k := range dep {
		keys = append(keys, k)
	}
	assert.ElementsMatch(t,
		[]string{"direct_count", "indirect_count", "total_count", "direct"},
		keys,
		"gem_dependencies value keys must match the other *_dependencies signals exactly")

	assert.EqualValues(t, 2, dep["direct_count"],
		"actionpack + activesupport (de-duped); rspec (development) excluded")
	assert.EqualValues(t, 0, dep["indirect_count"])
	assert.EqualValues(t, 2, dep["total_count"])

	direct, ok := dep["direct"].([]any)
	require.True(t, ok, "direct must be a JSON array")
	assert.Equal(t, []any{"actionpack", "activesupport"}, direct,
		"direct is the sorted, de-duplicated runtime dependency-name set")
}
