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

// mapKeys returns the keys of m as a sorted slice, for use in
// exact-key-set assertions where the order of emission doesn't
// matter but the SET of keys is contractual.
func mapKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
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

	// All signals land, zero absences. (Five snapshot signals from
	// Phase A+B, three cross-version signals from Phase B.6, plus
	// version_count. The sample response has only a single version
	// entry, so the cross-version signals land with stable-state
	// payloads rather than transition flags.)
	assert.Equal(t, 9, result.SignalCount(),
		"all nine signals should land on happy path (5 snapshot + 3 cross-version + version_count)")
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

	// version_count
	require.True(t, hasSignal(result, "version_count"))
	vc := getSignalValue(t, result, "version_count")
	assert.EqualValues(t, 1, vc["count"],
		"sample response has one version entry")
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

	// Eight real signals (everything except weekly_downloads), one
	// absence for weekly_downloads. No short-circuit — downloads
	// failure must not poison the other signals. The three cross-
	// version signals land from the same-wire versions map.
	assert.Equal(t, 8, result.SignalCount())
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

	// Payload-hygiene lock-in: the signal MUST NOT emit the
	// postinstall script content. Scripts are often multi-line
	// shell or JS, can contain sensitive paths, and are not a
	// mechanical signal — their analysis is an analyst-level task.
	// A regression that added "postinstall_script" (or any other
	// script-content key) to the signal value would bloat the
	// payload and leak information not in our threat model's
	// emission contract.
	assert.NotContains(t, pi, "postinstall_script",
		"postinstall script content must never appear in the signal payload")
	assert.NotContains(t, pi, "script")
	assert.ElementsMatch(t,
		[]string{"present", "version_checked"},
		mapKeys(pi),
		"postinstall_present signal value should have exactly these two keys")
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
	assert.Equal(t, 9, result.SignalCount())

	assert.Equal(t, "/@types/node", registryPath,
		"registry request should preserve scope")
	assert.Equal(t, "/downloads/point/last-week/@types/node", downloadsPath,
		"downloads request should preserve scope")
}

// ===== Cross-version signals (Phase B.6) =====

// TestCollector_Collect_PostinstallIntroduced_DetectsTransition
// models the axios-2026 shape: the latest version has a postinstall
// script, older versions in the window do not. The signal should
// flag the transition and name the version where the script
// appeared.
func TestCollector_Collect_PostinstallIntroduced_DetectsTransition(t *testing.T) {
	t.Parallel()

	registryBody := `{
	  "name": "victim",
	  "dist-tags": {"latest": "2.1.0"},
	  "time": {
	    "1.0.0": "2024-01-01T00:00:00Z",
	    "2.0.0": "2025-06-01T00:00:00Z",
	    "2.1.0": "2026-04-20T00:00:00Z"
	  },
	  "maintainers": [{"name": "m"}],
	  "versions": {
	    "1.0.0": {"scripts": {}, "dist": {}, "_npmUser": {"name": "m"}},
	    "2.0.0": {"scripts": {}, "dist": {}, "_npmUser": {"name": "m"}},
	    "2.1.0": {"scripts": {"postinstall": "node attacker.js"}, "dist": {}, "_npmUser": {"name": "m"}}
	  }
	}`
	downloadsBody := `{"downloads":1,"start":"a","end":"b","package":"victim"}`
	srv := newMultiEndpointServer(t, registryBody, downloadsBody)
	defer srv.Close()

	result, err := newTestCollector(srv).Collect(context.Background(), npmEntity("victim"))
	require.NoError(t, err)

	pi := getSignalValue(t, result, "postinstall_introduced")
	assert.Equal(t, true, pi["present_in_latest"])
	assert.Equal(t, true, pi["introduced_recently"],
		"postinstall in latest + absent in older versions should flag a transition")
	assert.Equal(t, "2.1.0", pi["introduced_at_version"])
	assert.EqualValues(t, 2, pi["prior_versions_without"])
	assert.EqualValues(t, 3, pi["versions_checked"])
}

// TestCollector_Collect_PostinstallIntroduced_ConsistentAbsence is
// the zod-shape: no postinstall across any version. No transition,
// signal emits the consistent-absence fact.
func TestCollector_Collect_PostinstallIntroduced_ConsistentAbsence(t *testing.T) {
	t.Parallel()

	registryBody := `{
	  "name": "zod-like",
	  "dist-tags": {"latest": "3.0.0"},
	  "time": {
	    "1.0.0": "2024-01-01T00:00:00Z",
	    "2.0.0": "2025-01-01T00:00:00Z",
	    "3.0.0": "2026-01-01T00:00:00Z"
	  },
	  "maintainers": [{"name": "solo"}],
	  "versions": {
	    "1.0.0": {"scripts": {}, "dist": {}, "_npmUser": {"name": "solo"}},
	    "2.0.0": {"scripts": {}, "dist": {}, "_npmUser": {"name": "solo"}},
	    "3.0.0": {"scripts": {}, "dist": {}, "_npmUser": {"name": "solo"}}
	  }
	}`
	downloadsBody := `{"downloads":1,"start":"a","end":"b","package":"zod-like"}`
	srv := newMultiEndpointServer(t, registryBody, downloadsBody)
	defer srv.Close()

	result, err := newTestCollector(srv).Collect(context.Background(), npmEntity("zod-like"))
	require.NoError(t, err)

	pi := getSignalValue(t, result, "postinstall_introduced")
	assert.Equal(t, false, pi["present_in_latest"])
	assert.Equal(t, false, pi["introduced_recently"],
		"no postinstall anywhere in window should NOT flag a transition")
	// prior_versions_without is a literal count of older versions
	// that lack a postinstall. All three here lack it; with the
	// latest excluded, that's 2 older ones without. The count is
	// only interpretively meaningful when paired with
	// introduced_recently=true (which is the axios shape).
	assert.EqualValues(t, 2, pi["prior_versions_without"])
	assert.Equal(t, "", pi["introduced_at_version"])
}

// TestCollector_Collect_PostinstallIntroduced_ConsistentPresence —
// native-bindings shape: postinstall in every version in the
// window. That's typical for native-compiled packages; the
// transition flag stays false, which is what we want.
func TestCollector_Collect_PostinstallIntroduced_ConsistentPresence(t *testing.T) {
	t.Parallel()

	registryBody := `{
	  "name": "native-lib",
	  "dist-tags": {"latest": "3.0.0"},
	  "time": {
	    "1.0.0": "2024-01-01T00:00:00Z",
	    "2.0.0": "2025-01-01T00:00:00Z",
	    "3.0.0": "2026-01-01T00:00:00Z"
	  },
	  "maintainers": [{"name": "m"}],
	  "versions": {
	    "1.0.0": {"scripts": {"postinstall": "node-gyp rebuild"}, "dist": {}, "_npmUser": {"name": "m"}},
	    "2.0.0": {"scripts": {"postinstall": "node-gyp rebuild"}, "dist": {}, "_npmUser": {"name": "m"}},
	    "3.0.0": {"scripts": {"postinstall": "node-gyp rebuild"}, "dist": {}, "_npmUser": {"name": "m"}}
	  }
	}`
	downloadsBody := `{"downloads":1,"start":"a","end":"b","package":"native-lib"}`
	srv := newMultiEndpointServer(t, registryBody, downloadsBody)
	defer srv.Close()

	result, err := newTestCollector(srv).Collect(context.Background(), npmEntity("native-lib"))
	require.NoError(t, err)

	pi := getSignalValue(t, result, "postinstall_introduced")
	assert.Equal(t, true, pi["present_in_latest"])
	assert.Equal(t, false, pi["introduced_recently"],
		"postinstall in ALL versions is consistent — not a transition")
	assert.EqualValues(t, 0, pi["prior_versions_without"])
}

// TestCollector_Collect_PublishOriginConsistency_Stable — every
// recent version attested, single publisher. The healthy shape.
func TestCollector_Collect_PublishOriginConsistency_Stable(t *testing.T) {
	t.Parallel()

	registryBody := `{
	  "name": "hardened",
	  "dist-tags": {"latest": "3.0.0"},
	  "time": {
	    "1.0.0": "2024-01-01T00:00:00Z",
	    "2.0.0": "2025-01-01T00:00:00Z",
	    "3.0.0": "2026-01-01T00:00:00Z"
	  },
	  "maintainers": [{"name": "robot"}],
	  "versions": {
	    "1.0.0": {"scripts": {}, "dist": {"attestations": {"url": "x"}}, "_npmUser": {"name": "robot"}},
	    "2.0.0": {"scripts": {}, "dist": {"attestations": {"url": "x"}}, "_npmUser": {"name": "robot"}},
	    "3.0.0": {"scripts": {}, "dist": {"attestations": {"url": "x"}}, "_npmUser": {"name": "robot"}}
	  }
	}`
	downloadsBody := `{"downloads":1,"start":"a","end":"b","package":"hardened"}`
	srv := newMultiEndpointServer(t, registryBody, downloadsBody)
	defer srv.Close()

	result, err := newTestCollector(srv).Collect(context.Background(), npmEntity("hardened"))
	require.NoError(t, err)

	poc := getSignalValue(t, result, "publish_origin_consistency")
	assert.Equal(t, true, poc["latest_has_attestation"])
	assert.EqualValues(t, 0, poc["attestation_transitions"])
	assert.EqualValues(t, 1, poc["unique_publishers"])
	assert.Equal(t, "robot", poc["latest_publisher"])
}

// TestCollector_Collect_PublishOriginConsistency_AttestationLost
// models the axios-2026 attestation-chain-break: the malicious
// latest version lacks the OIDC attestation that prior versions
// had. The signal should flag a transition.
func TestCollector_Collect_PublishOriginConsistency_AttestationLost(t *testing.T) {
	t.Parallel()

	registryBody := `{
	  "name": "attested-but-recent-drop",
	  "dist-tags": {"latest": "2.0.0"},
	  "time": {
	    "1.0.0": "2024-01-01T00:00:00Z",
	    "1.1.0": "2025-06-01T00:00:00Z",
	    "2.0.0": "2026-04-20T00:00:00Z"
	  },
	  "maintainers": [{"name": "m"}],
	  "versions": {
	    "1.0.0": {"scripts": {}, "dist": {"attestations": {"url": "x"}}, "_npmUser": {"name": "m"}},
	    "1.1.0": {"scripts": {}, "dist": {"attestations": {"url": "x"}}, "_npmUser": {"name": "m"}},
	    "2.0.0": {"scripts": {}, "dist": {}, "_npmUser": {"name": "m"}}
	  }
	}`
	downloadsBody := `{"downloads":1,"start":"a","end":"b","package":"attested-but-recent-drop"}`
	srv := newMultiEndpointServer(t, registryBody, downloadsBody)
	defer srv.Close()

	result, err := newTestCollector(srv).Collect(context.Background(), npmEntity("attested-but-recent-drop"))
	require.NoError(t, err)

	poc := getSignalValue(t, result, "publish_origin_consistency")
	assert.Equal(t, false, poc["latest_has_attestation"])
	assert.EqualValues(t, 1, poc["attestation_transitions"],
		"exactly one transition when the most recent version drops attestation")
}

// TestCollector_Collect_PublishOriginConsistency_PublisherChurn
// models a maintainer-handoff-or-account-takeover: recent versions
// published under a different _npmUser than older ones.
func TestCollector_Collect_PublishOriginConsistency_PublisherChurn(t *testing.T) {
	t.Parallel()

	registryBody := `{
	  "name": "changed-hands",
	  "dist-tags": {"latest": "3.0.0"},
	  "time": {
	    "1.0.0": "2024-01-01T00:00:00Z",
	    "2.0.0": "2025-06-01T00:00:00Z",
	    "3.0.0": "2026-04-20T00:00:00Z"
	  },
	  "maintainers": [{"name": "new-owner"}],
	  "versions": {
	    "1.0.0": {"scripts": {}, "dist": {}, "_npmUser": {"name": "original-author"}},
	    "2.0.0": {"scripts": {}, "dist": {}, "_npmUser": {"name": "original-author"}},
	    "3.0.0": {"scripts": {}, "dist": {}, "_npmUser": {"name": "new-owner"}}
	  }
	}`
	downloadsBody := `{"downloads":1,"start":"a","end":"b","package":"changed-hands"}`
	srv := newMultiEndpointServer(t, registryBody, downloadsBody)
	defer srv.Close()

	result, err := newTestCollector(srv).Collect(context.Background(), npmEntity("changed-hands"))
	require.NoError(t, err)

	poc := getSignalValue(t, result, "publish_origin_consistency")
	assert.EqualValues(t, 2, poc["unique_publishers"],
		"two distinct _npmUser names across the window should land as unique_publishers=2")
	publishers, ok := poc["publishers"].([]any)
	require.True(t, ok)
	assert.ElementsMatch(t, []any{"new-owner", "original-author"}, publishers,
		"publishers list should contain both names, sorted deterministically")
	assert.Equal(t, "new-owner", poc["latest_publisher"],
		"latest_publisher is the publisher of the most-recent version")
}

// TestCollector_Collect_CrossVersion_WindowCap confirms the
// crossVersionWindow bound holds: given more versions than the
// window, we consider only the newest N. A postinstall added far
// outside the window won't flag a transition.
func TestCollector_Collect_CrossVersion_WindowCap(t *testing.T) {
	t.Parallel()

	// 12 versions — 2 older than the window. Window is 10.
	// Put postinstall in versions 0.1.0 and 0.2.0 (the two OLDEST,
	// which fall OUTSIDE the 10-version window), and nothing in
	// the 10 newer versions. If the cap is wrong, the signal will
	// see 0.1.0's postinstall and emit a false transition.
	registryBody := `{
	  "name": "long-history",
	  "dist-tags": {"latest": "1.11.0"},
	  "time": {
	    "0.1.0": "2020-01-01T00:00:00Z",
	    "0.2.0": "2020-02-01T00:00:00Z",
	    "1.0.0": "2021-01-01T00:00:00Z",
	    "1.1.0": "2021-06-01T00:00:00Z",
	    "1.2.0": "2022-01-01T00:00:00Z",
	    "1.3.0": "2022-06-01T00:00:00Z",
	    "1.4.0": "2023-01-01T00:00:00Z",
	    "1.5.0": "2023-06-01T00:00:00Z",
	    "1.6.0": "2024-01-01T00:00:00Z",
	    "1.7.0": "2024-06-01T00:00:00Z",
	    "1.8.0": "2025-01-01T00:00:00Z",
	    "1.9.0": "2025-06-01T00:00:00Z",
	    "1.10.0": "2026-01-01T00:00:00Z",
	    "1.11.0": "2026-04-01T00:00:00Z"
	  },
	  "maintainers": [{"name": "steady"}],
	  "versions": {
	    "0.1.0": {"scripts": {"postinstall": "x"}, "dist": {}, "_npmUser": {"name": "steady"}},
	    "0.2.0": {"scripts": {"postinstall": "x"}, "dist": {}, "_npmUser": {"name": "steady"}},
	    "1.0.0": {"scripts": {}, "dist": {}, "_npmUser": {"name": "steady"}},
	    "1.1.0": {"scripts": {}, "dist": {}, "_npmUser": {"name": "steady"}},
	    "1.2.0": {"scripts": {}, "dist": {}, "_npmUser": {"name": "steady"}},
	    "1.3.0": {"scripts": {}, "dist": {}, "_npmUser": {"name": "steady"}},
	    "1.4.0": {"scripts": {}, "dist": {}, "_npmUser": {"name": "steady"}},
	    "1.5.0": {"scripts": {}, "dist": {}, "_npmUser": {"name": "steady"}},
	    "1.6.0": {"scripts": {}, "dist": {}, "_npmUser": {"name": "steady"}},
	    "1.7.0": {"scripts": {}, "dist": {}, "_npmUser": {"name": "steady"}},
	    "1.8.0": {"scripts": {}, "dist": {}, "_npmUser": {"name": "steady"}},
	    "1.9.0": {"scripts": {}, "dist": {}, "_npmUser": {"name": "steady"}},
	    "1.10.0": {"scripts": {}, "dist": {}, "_npmUser": {"name": "steady"}},
	    "1.11.0": {"scripts": {}, "dist": {}, "_npmUser": {"name": "steady"}}
	  }
	}`
	downloadsBody := `{"downloads":1,"start":"a","end":"b","package":"long-history"}`
	srv := newMultiEndpointServer(t, registryBody, downloadsBody)
	defer srv.Close()

	result, err := newTestCollector(srv).Collect(context.Background(), npmEntity("long-history"))
	require.NoError(t, err)

	pi := getSignalValue(t, result, "postinstall_introduced")
	// Exactly 10 versions checked (the cap).
	assert.EqualValues(t, 10, pi["versions_checked"])
	// No transition — the postinstall-carrying versions are outside
	// the window, so the window's view is "consistent absence."
	assert.Equal(t, false, pi["introduced_recently"],
		"postinstall older than the window must not fire a transition")
	assert.Equal(t, false, pi["present_in_latest"])
}

// TestCollector_Collect_CrossVersion_TiebreakDeterministic pins
// sort-stability under timestamp collisions. The npm registry
// records time to millisecond precision but many fixtures (and some
// older publish records) truncate to the second. When two versions
// share an exact publish timestamp, recent[0] — which drives
// latest_publisher, latest_has_attestation, and introduced_at_version
// — must not flip between runs.
//
// Fixture: two versions with identical timestamps but different
// publisher names. The version string tiebreaker resolves to the
// lexically-greater "2.0.0" ahead of "1.9.0" after we reverse by
// time, so latest_publisher is the publisher of 2.0.0.
func TestCollector_Collect_CrossVersion_TiebreakDeterministic(t *testing.T) {
	t.Parallel()

	registryBody := `{
	  "name": "tiebreak",
	  "dist-tags": {"latest": "2.0.0"},
	  "time": {
	    "1.9.0": "2026-01-01T00:00:00Z",
	    "2.0.0": "2026-01-01T00:00:00Z"
	  },
	  "maintainers": [{"name": "m"}],
	  "versions": {
	    "1.9.0": {"scripts": {}, "dist": {}, "_npmUser": {"name": "old-publisher"}},
	    "2.0.0": {"scripts": {}, "dist": {}, "_npmUser": {"name": "new-publisher"}}
	  }
	}`
	downloadsBody := `{"downloads":1,"start":"a","end":"b","package":"tiebreak"}`
	srv := newMultiEndpointServer(t, registryBody, downloadsBody)
	defer srv.Close()

	// Run twice; the latest_publisher must be the same both times.
	// Without the stable-sort-plus-tiebreaker, this assertion flakes
	// because Go's map iteration and sort.Slice are both randomized.
	const runs = 5
	seen := make(map[string]struct{})
	for range runs {
		result, err := newTestCollector(srv).Collect(context.Background(), npmEntity("tiebreak"))
		require.NoError(t, err)
		poc := getSignalValue(t, result, "publish_origin_consistency")
		seen[poc["latest_publisher"].(string)] = struct{}{}
	}
	assert.Len(t, seen, 1,
		"latest_publisher must be stable across runs; got variants: %v", seen)
	// Lexical tiebreak: "2.0.0" > "1.9.0", so newer version wins.
	for pub := range seen {
		assert.Equal(t, "new-publisher", pub,
			"lexical tiebreaker should resolve to the alphabetically-greater version")
	}
}

// TestCollector_Collect_VersionPublishBurst_DetectsBurst models a
// rapid-fire publish campaign: 4 versions within 48 hours. The signal
// should flag burst_detected=true.
func TestCollector_Collect_VersionPublishBurst_DetectsBurst(t *testing.T) {
	t.Parallel()

	registryBody := `{
	  "name": "spam-pkg",
	  "dist-tags": {"latest": "1.3.0"},
	  "time": {
	    "1.0.0": "2026-04-10T06:00:00Z",
	    "1.1.0": "2026-04-10T18:00:00Z",
	    "1.2.0": "2026-04-11T06:00:00Z",
	    "1.3.0": "2026-04-11T18:00:00Z"
	  },
	  "maintainers": [{"name": "newacct"}],
	  "versions": {
	    "1.0.0": {"scripts": {}, "dist": {}, "_npmUser": {"name": "newacct"}},
	    "1.1.0": {"scripts": {}, "dist": {}, "_npmUser": {"name": "newacct"}},
	    "1.2.0": {"scripts": {}, "dist": {}, "_npmUser": {"name": "newacct"}},
	    "1.3.0": {"scripts": {}, "dist": {}, "_npmUser": {"name": "newacct"}}
	  }
	}`
	downloadsBody := `{"downloads":5,"start":"a","end":"b","package":"spam-pkg"}`
	srv := newMultiEndpointServer(t, registryBody, downloadsBody)
	defer srv.Close()

	result, err := newTestCollector(srv).Collect(context.Background(), npmEntity("spam-pkg"))
	require.NoError(t, err)

	vpb := getSignalValue(t, result, "version_publish_burst")
	assert.Equal(t, true, vpb["burst_detected"])
	assert.EqualValues(t, 4, vpb["versions_in_window"])
	assert.EqualValues(t, 36, vpb["window_hours"]) // 36h span across 4 versions
	assert.EqualValues(t, 4, vpb["versions_checked"])
}

// TestCollector_Collect_VersionPublishBurst_NoBurst — versions spread
// over months. No burst detected.
func TestCollector_Collect_VersionPublishBurst_NoBurst(t *testing.T) {
	t.Parallel()

	registryBody := `{
	  "name": "stable-pkg",
	  "dist-tags": {"latest": "3.0.0"},
	  "time": {
	    "1.0.0": "2024-01-15T00:00:00Z",
	    "2.0.0": "2025-03-20T00:00:00Z",
	    "3.0.0": "2026-02-10T00:00:00Z"
	  },
	  "maintainers": [{"name": "solid-dev"}],
	  "versions": {
	    "1.0.0": {"scripts": {}, "dist": {}, "_npmUser": {"name": "solid-dev"}},
	    "2.0.0": {"scripts": {}, "dist": {}, "_npmUser": {"name": "solid-dev"}},
	    "3.0.0": {"scripts": {}, "dist": {}, "_npmUser": {"name": "solid-dev"}}
	  }
	}`
	downloadsBody := `{"downloads":50000,"start":"a","end":"b","package":"stable-pkg"}`
	srv := newMultiEndpointServer(t, registryBody, downloadsBody)
	defer srv.Close()

	result, err := newTestCollector(srv).Collect(context.Background(), npmEntity("stable-pkg"))
	require.NoError(t, err)

	vpb := getSignalValue(t, result, "version_publish_burst")
	assert.Equal(t, false, vpb["burst_detected"])
	assert.EqualValues(t, 3, vpb["versions_in_window"])
	assert.EqualValues(t, 3, vpb["versions_checked"])
}

// TestCollector_Collect_CrossVersion_NoOrderableVersions — versions
// map has entries but no corresponding time entries, so we can't
// order them. Both cross-version signals emit absence.
func TestCollector_Collect_CrossVersion_NoOrderableVersions(t *testing.T) {
	t.Parallel()

	registryBody := `{
	  "name": "unorderable",
	  "dist-tags": {"latest": "1.0.0"},
	  "time": {},
	  "maintainers": [{"name": "m"}],
	  "versions": {
	    "1.0.0": {"scripts": {}, "dist": {}, "_npmUser": {"name": "m"}}
	  }
	}`
	downloadsBody := `{"downloads":1,"start":"a","end":"b","package":"unorderable"}`
	srv := newMultiEndpointServer(t, registryBody, downloadsBody)
	defer srv.Close()

	result, err := newTestCollector(srv).Collect(context.Background(), npmEntity("unorderable"))
	require.NoError(t, err)

	assert.True(t, hasAbsence(result, "postinstall_introduced"))
	assert.True(t, hasAbsence(result, "publish_origin_consistency"))
	assert.True(t, hasAbsence(result, "version_publish_burst"))
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
