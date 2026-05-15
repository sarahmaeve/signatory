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
	// build_script_introduced, version_count, version_publish_burst.
	// Total: 11 signals.
	assert.GreaterOrEqual(t, result.SignalCount(), 11,
		"expected at least 11 signals, got %d: %s", result.SignalCount(), result.Summary())

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

	// version_count
	assert.Contains(t, signalMap, "version_count")
	var vc map[string]any
	require.NoError(t, json.Unmarshal(signalMap["version_count"], &vc))
	assert.Equal(t, float64(4), vc["count"], "fixture has 4 versions")
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

func TestCollector_VersionPublishBurst_DetectsBurst(t *testing.T) {
	t.Parallel()

	// Four versions published within 36 hours — the version-pumping shape.
	resp := CrateResponse{
		Crate: Crate{
			Name:            "spam-crate",
			RecentDownloads: 5,
			CreatedAt:       "2026-04-10T00:00:00Z",
			MaxStableVer:    "1.3.0",
		},
		Versions: []Version{
			{
				Num: "1.3.0", CreatedAt: "2026-04-11T18:00:00Z",
				Yanked: false, PublishedBy: &User{Login: "newacct"},
			},
			{
				Num: "1.2.0", CreatedAt: "2026-04-11T06:00:00Z",
				Yanked: false, PublishedBy: &User{Login: "newacct"},
			},
			{
				Num: "1.1.0", CreatedAt: "2026-04-10T18:00:00Z",
				Yanked: false, PublishedBy: &User{Login: "newacct"},
			},
			{
				Num: "1.0.0", CreatedAt: "2026-04-10T06:00:00Z",
				Yanked: false, PublishedBy: &User{Login: "newacct"},
			},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/crates/spam-crate":
			json.NewEncoder(w).Encode(resp) //nolint:errcheck
		case "/api/v1/crates/spam-crate/owners":
			json.NewEncoder(w).Encode(OwnersResponse{
				Users: []Owner{{Login: "newacct", Kind: "user"}},
			}) //nolint:errcheck
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClientWithBaseURL(srv.URL)
	c := NewCollectorWithClient(client)

	entity := &profile.Entity{
		ID:           "test-spam",
		CanonicalURI: "pkg:cargo/spam-crate",
		Ecosystem:    "cargo",
	}

	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)

	signals := result.Signals()
	signalMap := map[string]json.RawMessage{}
	for _, s := range signals {
		signalMap[s.Type] = s.Value
	}

	require.Contains(t, signalMap, "version_publish_burst")
	var vpb map[string]any
	require.NoError(t, json.Unmarshal(signalMap["version_publish_burst"], &vpb))
	assert.Equal(t, true, vpb["burst_detected"],
		"4 versions in 36 hours should trigger burst detection")
	assert.EqualValues(t, 4, vpb["versions_in_window"])
	assert.EqualValues(t, 36, vpb["window_hours"])
	assert.EqualValues(t, 4, vpb["versions_checked"])
}

func TestCollector_VersionPublishBurst_NoBurst(t *testing.T) {
	t.Parallel()

	// Versions spread over months — healthy cadence.
	resp := CrateResponse{
		Crate: Crate{
			Name:            "steady-crate",
			RecentDownloads: 50000,
			CreatedAt:       "2023-01-01T00:00:00Z",
			MaxStableVer:    "3.0.0",
		},
		Versions: []Version{
			{
				Num: "3.0.0", CreatedAt: "2026-02-10T00:00:00Z",
				Yanked: false, PublishedBy: &User{Login: "solid"},
			},
			{
				Num: "2.0.0", CreatedAt: "2025-03-20T00:00:00Z",
				Yanked: false, PublishedBy: &User{Login: "solid"},
			},
			{
				Num: "1.0.0", CreatedAt: "2024-01-15T00:00:00Z",
				Yanked: false, PublishedBy: &User{Login: "solid"},
			},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/crates/steady-crate":
			json.NewEncoder(w).Encode(resp) //nolint:errcheck
		case "/api/v1/crates/steady-crate/owners":
			json.NewEncoder(w).Encode(OwnersResponse{
				Users: []Owner{{Login: "solid", Kind: "user"}},
			}) //nolint:errcheck
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClientWithBaseURL(srv.URL)
	c := NewCollectorWithClient(client)

	entity := &profile.Entity{
		ID:           "test-steady",
		CanonicalURI: "pkg:cargo/steady-crate",
		Ecosystem:    "cargo",
	}

	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)

	signals := result.Signals()
	signalMap := map[string]json.RawMessage{}
	for _, s := range signals {
		signalMap[s.Type] = s.Value
	}

	require.Contains(t, signalMap, "version_publish_burst")
	var vpb map[string]any
	require.NoError(t, json.Unmarshal(signalMap["version_publish_burst"], &vpb))
	assert.Equal(t, false, vpb["burst_detected"],
		"versions spread over months should not trigger burst")
	assert.EqualValues(t, 3, vpb["versions_in_window"])
	assert.EqualValues(t, 3, vpb["versions_checked"])
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

// TestCollector_RecordsArtifactURL exercises the producer side of the
// artifact-vs-repo divergence flow for cargo. The cargo registry
// collector must emit an artifact_url signal carrying the constructed
// tarball URL, version, and integrity (hex sha256 from the crates.io
// `checksum` field). git_head is empty for cargo — crates.io does not
// expose a publisher-stamped commit in registry metadata; the
// downstream artifact collector recovers the SHA from
// .cargo_vcs_info.json embedded in the tarball.
func TestCollector_RecordsArtifactURL(t *testing.T) {
	t.Parallel()

	resp := CrateResponse{
		Crate: Crate{
			Name:            "blake3",
			Repository:      "https://github.com/BLAKE3-team/BLAKE3",
			RecentDownloads: 25_000_000,
			CreatedAt:       "2019-12-02T00:00:00Z",
			MaxStableVer:    "1.8.5",
		},
		Versions: []Version{
			{
				Num: "1.8.5", CreatedAt: "2026-04-25T00:51:44Z",
				Yanked: false, PublishedBy: &User{Login: "oconnor663"},
				Checksum: "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
			},
			{
				Num: "1.8.4", CreatedAt: "2026-03-20T00:00:00Z",
				Yanked: false, PublishedBy: &User{Login: "oconnor663"},
				Checksum: "1111111111111111111111111111111111111111111111111111111111111111",
			},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/crates/blake3":
			json.NewEncoder(w).Encode(resp) //nolint:errcheck
		case "/api/v1/crates/blake3/owners":
			json.NewEncoder(w).Encode(OwnersResponse{
				Users: []Owner{{Login: "oconnor663", Kind: "user"}},
			}) //nolint:errcheck
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClientWithBaseURL(srv.URL)
	c := NewCollectorWithClient(client)

	entity := &profile.Entity{
		ID:           "test-blake3",
		CanonicalURI: "pkg:cargo/blake3",
		Ecosystem:    "cargo",
	}

	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)

	signals := result.Signals()
	signalMap := map[string]json.RawMessage{}
	for _, s := range signals {
		signalMap[s.Type] = s.Value
	}

	require.Contains(t, signalMap, "artifact_url",
		"cargo registry collector must emit artifact_url so the artifact-vs-repo "+
			"collector can fetch and pair the .crate tarball")

	var au map[string]any
	require.NoError(t, json.Unmarshal(signalMap["artifact_url"], &au))

	assert.Equal(t, "https://static.crates.io/crates/blake3/1.8.5/download", au["url"],
		"crates.io tarball URL is constructed (not parsed from registry metadata); "+
			"the canonical form is /crates/{name}/{version}/download")
	assert.Equal(t, "1.8.5", au["version"],
		"version is the latest non-yanked publish; downstream tag-match resolver pairs by this")
	assert.Equal(t, "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789", au["integrity"],
		"integrity is the crates.io checksum (hex sha256). Opaque to current consumers "+
			"but kept on the wire for cross-checking")
	assert.Equal(t, "", au["git_head"],
		"crates.io does not expose git_head in registry metadata; the artifact collector "+
			"recovers the SHA from .cargo_vcs_info.json inside the tarball")
}

// TestCollector_RecordsArtifactURL_AbsenceWhenNoVersions confirms the
// absence path: a crate response with no orderable non-yanked versions
// (e.g. all versions yanked) records an artifact_url absence rather
// than emitting a malformed URL or panicking.
func TestCollector_RecordsArtifactURL_AbsenceWhenNoVersions(t *testing.T) {
	t.Parallel()

	resp := CrateResponse{
		Crate: Crate{
			Name:         "all-yanked",
			MaxStableVer: "0.1.0",
		},
		Versions: []Version{
			{Num: "0.1.0", Yanked: true, CreatedAt: "2024-01-01T00:00:00Z"},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/crates/all-yanked":
			json.NewEncoder(w).Encode(resp) //nolint:errcheck
		case "/api/v1/crates/all-yanked/owners":
			json.NewEncoder(w).Encode(OwnersResponse{}) //nolint:errcheck
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client := NewClientWithBaseURL(srv.URL)
	c := NewCollectorWithClient(client)

	entity := &profile.Entity{
		ID:           "test-all-yanked",
		CanonicalURI: "pkg:cargo/all-yanked",
		Ecosystem:    "cargo",
	}

	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)

	// Find the artifact_url absence on the result. Absences live in
	// CollectionResult.Collected alongside signals, distinguished by
	// IsAbsence() — there is no separate Absences() accessor.
	found := false
	for i := range result.Collected {
		entry := result.Collected[i]
		if entry.IsAbsence() && entry.Absence.SignalType == "artifact_url" {
			found = true
			break
		}
	}
	assert.True(t, found,
		"with no orderable non-yanked versions, artifact_url must be recorded as absence "+
			"rather than emitted with a fabricated URL")
}

// TestCollector_CargoDependencies_EmitsUniformShape pins that the
// cargo collector emits cargo_dependencies with a value shape byte-
// identical to go_dependencies and npm_dependencies (direct_count,
// indirect_count, total_count, direct[]), so the deltas diff engine
// surfaces cargo dependency drift through the same set-diff path.
//
// direct is the sorted unique set of crate_ids whose kind is "normal"
// or "build". dev-dependencies are excluded because they are never
// pulled transitively by downstream consumers (mirrors npm excluding
// devDependencies); build-dependencies ARE included because build.rs
// executes arbitrary code on the consumer's machine at build time —
// the same supply-chain reasoning that folds npm optionalDependencies
// in. indirect_count is always 0: the crates.io dependencies endpoint
// returns only the directly-declared edges for that version.
func TestCollector_CargoDependencies_EmitsUniformShape(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/crates/serde":
			json.NewEncoder(w).Encode(serdeFixture()) //nolint:errcheck
		case "/api/v1/crates/serde/owners":
			json.NewEncoder(w).Encode(serdeOwners()) //nolint:errcheck
		case "/api/v1/crates/serde/1.0.219/dependencies":
			json.NewEncoder(w).Encode(DependenciesResponse{ //nolint:errcheck
				Dependencies: []Dependency{
					{CrateID: "serde_derive", Req: "=1.0.219", Kind: "normal", Optional: true},
					{CrateID: "syn", Req: "^2", Kind: "build", Optional: false},
					{CrateID: "trybuild", Req: "^1", Kind: "dev", Optional: false},
					{CrateID: "serde_derive", Req: "=1.0.219", Kind: "normal", Optional: false},
				},
			})
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

	signalMap := map[string]json.RawMessage{}
	for _, s := range result.Signals() {
		signalMap[s.Type] = s.Value
	}
	require.Contains(t, signalMap, "cargo_dependencies",
		"cargo_dependencies must be emitted for a crate with declared deps")

	var dep map[string]any
	require.NoError(t, json.Unmarshal(signalMap["cargo_dependencies"], &dep))

	assert.ElementsMatch(t,
		[]string{"direct_count", "indirect_count", "total_count", "direct"},
		mapKeysOf(dep),
		"cargo_dependencies value keys must match go_dependencies/npm_dependencies exactly")

	assert.EqualValues(t, 2, dep["direct_count"],
		"normal serde_derive (deduped) + build syn = 2; dev trybuild excluded")
	assert.EqualValues(t, 0, dep["indirect_count"])
	assert.EqualValues(t, 2, dep["total_count"])

	direct, ok := dep["direct"].([]any)
	require.True(t, ok, "direct must be a JSON array")
	assert.Equal(t, []any{"serde_derive", "syn"}, direct,
		"direct is the sorted, de-duplicated union of normal+build crate_ids")
}

// TestCollector_CargoDependencies_EndpointFailureDegrades pins that a
// failing dependencies endpoint records a retryable failure for
// cargo_dependencies only and does not poison the rest of the signal
// set — mirroring the owners-endpoint degradation contract.
func TestCollector_CargoDependencies_EndpointFailureDegrades(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/crates/serde":
			json.NewEncoder(w).Encode(serdeFixture()) //nolint:errcheck
		case "/api/v1/crates/serde/owners":
			json.NewEncoder(w).Encode(serdeOwners()) //nolint:errcheck
		case "/api/v1/crates/serde/1.0.219/dependencies":
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

	for _, s := range result.Signals() {
		assert.NotEqual(t, "cargo_dependencies", s.Type,
			"a failed dependencies fetch must not emit a cargo_dependencies signal")
	}
	require.Len(t, result.Failures, 1)
	assert.Equal(t, "cargo_dependencies", result.Failures[0].SignalType)
	assert.True(t, result.Failures[0].Retryable, "a 500 is retryable")

	// Other signals still land — the dependencies failure is isolated.
	signalMap := map[string]json.RawMessage{}
	for _, s := range result.Signals() {
		signalMap[s.Type] = s.Value
	}
	assert.Contains(t, signalMap, "last_publish")
	assert.Contains(t, signalMap, "maintainer_count")
}

// mapKeysOf returns the keys of m, for exact-key-set assertions where
// emission order is irrelevant but the SET of keys is contractual.
func mapKeysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// mockEntityStore tracks which entity URIs were minted.
type mockEntityStore struct {
	minted []string
}

func (m *mockEntityStore) EnsureEntityByCanonicalURI(_ context.Context, uri, shortName string) (*profile.Entity, bool, error) {
	m.minted = append(m.minted, uri)
	return &profile.Entity{ID: "mock-" + shortName, CanonicalURI: uri}, true, nil
}
