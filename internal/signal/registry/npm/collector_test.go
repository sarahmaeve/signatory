package npm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
)

// newMultiEndpointServer builds an httptest server that handles both
// the registry package endpoint (/<name>) and the downloads endpoint
// (/downloads/point/last-week/<name>) against the same host. Used
// by collector tests that want BOTH layers exercised end-to-end;
// tests targeting a single endpoint can still stand up a narrower
// server.
func newMultiEndpointServer(t *testing.T, registryBody, downloadsBody string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/downloads/"):
			fmt.Fprint(w, downloadsBody)
		default:
			fmt.Fprint(w, registryBody)
		}
	}))
}

func newTestCollector(srv *httptest.Server) *Collector {
	return newCollectorWithClient(newClientWithBaseURL(srv.URL))
}

func npmEntity(name string) *profile.Entity {
	return &profile.Entity{
		ID:           "e-" + name,
		CanonicalURI: "pkg:npm/" + name,
		Type:         profile.EntityPackage,
		Ecosystem:    "npm",
		ShortName:    name,
	}
}

// hasSignal returns true if result recorded a non-absence signal of
// the given type. Used for exact-match assertions without caring
// about emission order.
func hasSignal(result anySignals, signalType string) bool {
	for _, s := range result.Signals() {
		if !strings.HasPrefix(s.Type, "absence:") && s.Type == signalType {
			return true
		}
	}
	return false
}

// hasAbsence returns true if result recorded an absence of the
// given type.
func hasAbsence(result anySignals, signalType string) bool {
	for _, s := range result.Signals() {
		if s.Type == "absence:"+signalType {
			return true
		}
	}
	return false
}

// getSignalValue extracts the JSON-decoded value for the first
// matching signal type. Callers that need the raw signal can walk
// result.Signals() directly.
func getSignalValue(t *testing.T, result anySignals, signalType string) map[string]any {
	t.Helper()
	for _, s := range result.Signals() {
		if s.Type == signalType {
			var v map[string]any
			require.NoError(t, json.Unmarshal(s.Value, &v))
			return v
		}
	}
	t.Fatalf("signal %q not found in result", signalType)
	return nil
}

// anySignals is a local tiny interface so helpers above work
// against both *signal.CollectionResult and any other type that
// exposes Signals().
type anySignals interface {
	Signals() []profile.Signal
}

// ----- happy path: all five signals land -----

func TestCollector_Collect_HappyPath_EmitsFullSignalSet(t *testing.T) {
	t.Parallel()

	registryBody := sampleRegistryResponse
	downloadsBody := `{"downloads": 28500000, "start": "2026-04-13", "end": "2026-04-20", "package": "express"}`
	srv := newMultiEndpointServer(t, registryBody, downloadsBody)
	defer srv.Close()

	result, err := newTestCollector(srv).Collect(context.Background(), npmEntity("express"))
	require.NoError(t, err)
	require.NotNil(t, result)

	// Five real signals, zero absences.
	assert.Equal(t, 5, result.SignalCount(), "all five signals should land on happy path")
	assert.Equal(t, 0, result.AbsenceCount())

	// last_publish
	require.True(t, hasSignal(result, "last_publish"))
	lp := getSignalValue(t, result, "last_publish")
	assert.Equal(t, "4.18.2", lp["latest_version"])
	assert.Equal(t, "2022-10-08T19:08:35Z", lp["published_at"])

	// maintainer_count
	require.True(t, hasSignal(result, "maintainer_count"))
	mc := getSignalValue(t, result, "maintainer_count")
	assert.EqualValues(t, 2, mc["count"])
	logins, ok := mc["logins"].([]any)
	require.True(t, ok)
	assert.ElementsMatch(t, []any{"dougwilson", "linusu"}, logins)

	// postinstall_present
	require.True(t, hasSignal(result, "postinstall_present"))
	pi := getSignalValue(t, result, "postinstall_present")
	assert.Equal(t, false, pi["present"],
		"sample response has empty postinstall → present=false")
	assert.Equal(t, "4.18.2", pi["version_checked"])

	// trusted_publishing
	require.True(t, hasSignal(result, "trusted_publishing"))
	tp := getSignalValue(t, result, "trusted_publishing")
	assert.Equal(t, false, tp["present"],
		"sample response has attestations:null → present=false")

	// weekly_downloads
	require.True(t, hasSignal(result, "weekly_downloads"))
	wd := getSignalValue(t, result, "weekly_downloads")
	assert.EqualValues(t, 28_500_000, wd["count"])
	assert.Equal(t, "last-week", wd["window"])
}

// ----- non-npm entity: empty result, no HTTP calls -----

func TestCollector_Collect_NonNpmEntity_ReturnsEmpty(t *testing.T) {
	t.Parallel()

	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		calls++
	}))
	defer srv.Close()

	for _, e := range []*profile.Entity{
		{CanonicalURI: "repo:github/expressjs/express"},
		{CanonicalURI: "pkg:pypi/requests"},
		{CanonicalURI: "identity:github/alecthomas"},
		{CanonicalURI: ""},
		nil,
	} {
		result, err := newTestCollector(srv).Collect(context.Background(), e)
		require.NoError(t, err)
		require.NotNil(t, result)
		assert.Equal(t, 0, result.SignalCount())
		assert.Equal(t, 0, result.AbsenceCount())
	}
	assert.Equal(t, 0, calls, "non-npm entities must not trigger any request")
}

// ----- registry 404 short-circuits -----

func TestCollector_Collect_RegistryNotFound_ShortCircuits(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	result, err := newTestCollector(srv).Collect(context.Background(), npmEntity("nonexistent"))
	require.NoError(t, err)

	// The registry failure is definitive: other signals can't be
	// computed, so we emit one absence for last_publish and stop.
	assert.Equal(t, 0, result.SignalCount())
	assert.Equal(t, 1, result.AbsenceCount(),
		"registry 404 emits one absence for last_publish and short-circuits — other signals can't be derived")
	require.Len(t, result.Failures, 1)
	assert.False(t, result.Failures[0].Retryable,
		"404 on the package is definitive")
}

// ----- registry 500: absence, retryable -----

func TestCollector_Collect_RegistryServerError_RecordsRetryableAbsence(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	result, err := newTestCollector(srv).Collect(context.Background(), npmEntity("express"))
	require.NoError(t, err)
	assert.Equal(t, 1, result.AbsenceCount())
	require.Len(t, result.Failures, 1)
	assert.True(t, result.Failures[0].Retryable)
}

// ----- downloads 404 doesn't block other signals -----

func TestCollector_Collect_DownloadsNotFound_AbsenceOnly(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/downloads/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, sampleRegistryResponse)
	}))
	defer srv.Close()

	result, err := newTestCollector(srv).Collect(context.Background(), npmEntity("express"))
	require.NoError(t, err)

	// Four real signals (everything except weekly_downloads), one
	// absence for weekly_downloads. No short-circuit — downloads
	// failure must not poison the other signals.
	assert.Equal(t, 4, result.SignalCount())
	assert.True(t, hasAbsence(result, "weekly_downloads"))
	assert.True(t, hasSignal(result, "last_publish"))
	assert.True(t, hasSignal(result, "maintainer_count"))
	assert.True(t, hasSignal(result, "postinstall_present"))
	assert.True(t, hasSignal(result, "trusted_publishing"))

	// The downloads failure is registered as retryable (500-class
	// network behavior): a fresh request might succeed.
	require.Len(t, result.Failures, 1)
	assert.Equal(t, "weekly_downloads", result.Failures[0].SignalType)
	assert.False(t, result.Failures[0].Retryable,
		"404 is definitive even on the downloads endpoint — the package either has stats or it doesn't")
}

// ----- trusted_publishing: attestations present -----

func TestCollector_Collect_TrustedPublishing_Present(t *testing.T) {
	t.Parallel()

	// Sample response modelled on a real OIDC-trusted-publishing
	// npm release. The dist.attestations block is a non-empty
	// object; the exact shape can vary so we keep our signal
	// presence/absence rather than parsing the block.
	registryBody := `{
	  "name": "hardened-package",
	  "dist-tags": {"latest": "1.0.0"},
	  "time": {"1.0.0": "2026-04-01T00:00:00Z"},
	  "maintainers": [{"name": "careful-maintainer"}],
	  "versions": {
	    "1.0.0": {
	      "scripts": {},
	      "dist": {
	        "attestations": {
	          "url": "https://registry.npmjs.org/-/npm/v1/attestations/hardened-package@1.0.0",
	          "provenance": {"predicateType": "https://slsa.dev/provenance/v1"}
	        }
	      }
	    }
	  }
	}`
	downloadsBody := `{"downloads": 1000, "start": "2026-04-13", "end": "2026-04-20", "package": "hardened-package"}`
	srv := newMultiEndpointServer(t, registryBody, downloadsBody)
	defer srv.Close()

	result, err := newTestCollector(srv).Collect(context.Background(), npmEntity("hardened-package"))
	require.NoError(t, err)

	tp := getSignalValue(t, result, "trusted_publishing")
	assert.Equal(t, true, tp["present"],
		"non-null attestations object should register as present")
}

// ----- trusted_publishing: explicit null -----

func TestCollector_Collect_TrustedPublishing_Null(t *testing.T) {
	t.Parallel()

	registryBody := `{
	  "name": "legacy-package",
	  "dist-tags": {"latest": "0.1.0"},
	  "time": {"0.1.0": "2020-01-01T00:00:00Z"},
	  "maintainers": [{"name": "old-maintainer"}],
	  "versions": {
	    "0.1.0": {
	      "scripts": {},
	      "dist": {"attestations": null}
	    }
	  }
	}`
	downloadsBody := `{"downloads": 1, "start": "2026-04-13", "end": "2026-04-20", "package": "legacy-package"}`
	srv := newMultiEndpointServer(t, registryBody, downloadsBody)
	defer srv.Close()

	result, err := newTestCollector(srv).Collect(context.Background(), npmEntity("legacy-package"))
	require.NoError(t, err)

	tp := getSignalValue(t, result, "trusted_publishing")
	assert.Equal(t, false, tp["present"],
		"explicit null attestations should register as not-present")
}

// ----- trusted_publishing: field missing entirely -----

func TestCollector_Collect_TrustedPublishing_FieldAbsent(t *testing.T) {
	t.Parallel()

	registryBody := `{
	  "name": "pre-attestation-package",
	  "dist-tags": {"latest": "0.1.0"},
	  "time": {"0.1.0": "2020-01-01T00:00:00Z"},
	  "maintainers": [{"name": "old-maintainer"}],
	  "versions": {
	    "0.1.0": {
	      "scripts": {},
	      "dist": {}
	    }
	  }
	}`
	downloadsBody := `{"downloads": 1, "start": "2026-04-13", "end": "2026-04-20", "package": "pre-attestation-package"}`
	srv := newMultiEndpointServer(t, registryBody, downloadsBody)
	defer srv.Close()

	result, err := newTestCollector(srv).Collect(context.Background(), npmEntity("pre-attestation-package"))
	require.NoError(t, err)

	tp := getSignalValue(t, result, "trusted_publishing")
	assert.Equal(t, false, tp["present"],
		"missing attestations field registers as not-present (same as explicit null)")
}

// ----- postinstall_present: script declared -----

func TestCollector_Collect_Postinstall_Present(t *testing.T) {
	t.Parallel()

	registryBody := `{
	  "name": "native-binding",
	  "dist-tags": {"latest": "1.0.0"},
	  "time": {"1.0.0": "2026-01-01T00:00:00Z"},
	  "maintainers": [{"name": "nb-maintainer"}],
	  "versions": {
	    "1.0.0": {
	      "scripts": {"postinstall": "node-gyp rebuild"},
	      "dist": {"attestations": null}
	    }
	  }
	}`
	downloadsBody := `{"downloads": 0, "start": "2026-04-13", "end": "2026-04-20", "package": "native-binding"}`
	srv := newMultiEndpointServer(t, registryBody, downloadsBody)
	defer srv.Close()

	result, err := newTestCollector(srv).Collect(context.Background(), npmEntity("native-binding"))
	require.NoError(t, err)

	pi := getSignalValue(t, result, "postinstall_present")
	assert.Equal(t, true, pi["present"])
	assert.Equal(t, "1.0.0", pi["version_checked"])
}

// ----- maintainer_count: empty maintainers list -----

func TestCollector_Collect_NoMaintainers_RecordsAbsence(t *testing.T) {
	t.Parallel()

	registryBody := `{
	  "name": "orphaned",
	  "dist-tags": {"latest": "1.0.0"},
	  "time": {"1.0.0": "2026-01-01T00:00:00Z"},
	  "versions": {"1.0.0": {"scripts": {}, "dist": {}}}
	}`
	downloadsBody := `{"downloads": 0, "start": "2026-04-13", "end": "2026-04-20", "package": "orphaned"}`
	srv := newMultiEndpointServer(t, registryBody, downloadsBody)
	defer srv.Close()

	result, err := newTestCollector(srv).Collect(context.Background(), npmEntity("orphaned"))
	require.NoError(t, err)

	assert.True(t, hasAbsence(result, "maintainer_count"))
	assert.False(t, hasSignal(result, "maintainer_count"))
}

// ----- no dist-tags.latest: per-version absences for all three -----

func TestCollector_Collect_NoLatestVersion_AbsencesForDependent(t *testing.T) {
	t.Parallel()

	registryBody := `{
	  "name": "broken",
	  "maintainers": [{"name": "who-knows"}],
	  "time": {"1.0.0": "2024-01-01T00:00:00Z"}
	}`
	downloadsBody := `{"downloads": 0, "start": "2026-04-13", "end": "2026-04-20", "package": "broken"}`
	srv := newMultiEndpointServer(t, registryBody, downloadsBody)
	defer srv.Close()

	result, err := newTestCollector(srv).Collect(context.Background(), npmEntity("broken"))
	require.NoError(t, err)

	// Signals that depend on dist-tags.latest are all absent.
	assert.True(t, hasAbsence(result, "last_publish"))
	assert.True(t, hasAbsence(result, "postinstall_present"))
	assert.True(t, hasAbsence(result, "trusted_publishing"))

	// Signals independent of latest-version still land.
	assert.True(t, hasSignal(result, "maintainer_count"))
	assert.True(t, hasSignal(result, "weekly_downloads"))
}

// ----- no time entry for latest version -----

func TestCollector_Collect_NoTimeForLatest_LastPublishAbsence(t *testing.T) {
	t.Parallel()

	registryBody := `{
	  "name": "x",
	  "dist-tags": {"latest": "1.0.0"},
	  "time": {},
	  "maintainers": [{"name": "m"}],
	  "versions": {"1.0.0": {"scripts": {}, "dist": {}}}
	}`
	downloadsBody := `{"downloads": 0, "start": "2026-04-13", "end": "2026-04-20", "package": "x"}`
	srv := newMultiEndpointServer(t, registryBody, downloadsBody)
	defer srv.Close()

	result, err := newTestCollector(srv).Collect(context.Background(), npmEntity("x"))
	require.NoError(t, err)

	assert.True(t, hasAbsence(result, "last_publish"),
		"missing time entry should surface as last_publish absence")

	// The signals derived from versions[latest] still land because
	// they don't depend on the time map.
	assert.True(t, hasSignal(result, "postinstall_present"))
	assert.True(t, hasSignal(result, "trusted_publishing"))
	assert.True(t, hasSignal(result, "maintainer_count"))
}

// ----- scoped packages flow end-to-end -----

func TestCollector_Collect_ScopedPackage_AllEndpointsUseFullName(t *testing.T) {
	t.Parallel()

	var registryPath, downloadsPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/downloads/"):
			downloadsPath = r.URL.Path
			fmt.Fprint(w, `{"downloads":42000,"start":"x","end":"y","package":"@types/node"}`)
		default:
			registryPath = r.URL.Path
			fmt.Fprint(w, `{
			  "name":"@types/node",
			  "dist-tags":{"latest":"20.0.0"},
			  "time":{"20.0.0":"2024-01-01T00:00:00Z"},
			  "maintainers":[{"name":"types-bot"}],
			  "versions":{"20.0.0":{"scripts":{},"dist":{}}}
			}`)
		}
	}))
	defer srv.Close()

	result, err := newTestCollector(srv).Collect(context.Background(), npmEntity("@types/node"))
	require.NoError(t, err)
	assert.Equal(t, 5, result.SignalCount())

	assert.Equal(t, "/@types/node", registryPath,
		"registry request should preserve scope")
	assert.Equal(t, "/downloads/point/last-week/@types/node", downloadsPath,
		"downloads request should preserve scope")
}

// ----- extractNpmPackageName unit test stays the same -----

func TestExtractNpmPackageName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		uri         string
		wantName    string
		wantMatched bool
	}{
		{"unscoped", "pkg:npm/express", "express", true},
		{"scoped", "pkg:npm/@types/node", "@types/node", true},
		{"scoped with hyphens", "pkg:npm/@angular/core", "@angular/core", true},
		{"empty after prefix", "pkg:npm/", "", false},
		{"different ecosystem", "pkg:pypi/requests", "", false},
		{"repo uri", "repo:github/x/y", "", false},
		{"identity", "identity:github/alecthomas", "", false},
		{"empty uri", "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := extractNpmPackageName(&profile.Entity{CanonicalURI: tc.uri})
			assert.Equal(t, tc.wantMatched, ok)
			assert.Equal(t, tc.wantName, got)
		})
	}

	got, ok := extractNpmPackageName(nil)
	assert.False(t, ok)
	assert.Equal(t, "", got)
}
