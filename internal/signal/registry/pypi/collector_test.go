package pypi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
)

// pypiEntity returns a *profile.Entity shaped like what the
// orchestrator hands a registry collector for a PyPI package.
func pypiEntity(name string) *profile.Entity {
	return &profile.Entity{
		ID:           "e-" + name,
		CanonicalURI: "pkg:pypi/" + name,
		Type:         profile.EntityPackage,
		Ecosystem:    "pypi",
		ShortName:    name,
	}
}

// newTestCollector wraps a *Client pointed at an httptest server.
// Mirrors npm's helper of the same name; lives unexported so the
// pypi package's own tests reuse it without leaking the constructor
// into other packages.
func newTestCollector(srv *httptest.Server) *Collector {
	return NewCollectorWithClient(NewClientWithBaseURL(srv.URL))
}

// projectInfoServer responds to /pypi/<name>/json with the supplied
// Info block (wrapped in a Project envelope to match the registry's
// real shape). Test helper for collector behaviour tests; resolve
// tests use a similar but not identical helper that lives in
// resolve_test.go.
func projectInfoServer(t *testing.T, info Info) *httptest.Server {
	t.Helper()
	body, err := json.Marshal(Project{Info: info})
	require.NoError(t, err)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
}

// hasSignal returns true if result recorded a non-absence signal of
// the given type. Mirrors npm's helper of the same name.
func hasSignal(result *signalResultLike, signalType string) bool {
	for _, s := range result.signals {
		if !strings.HasPrefix(s.Type, "absence:") && s.Type == signalType {
			return true
		}
	}
	return false
}

// hasAbsence returns true if result recorded an absence of the
// given type.
func hasAbsence(result *signalResultLike, signalType string) bool {
	for _, s := range result.signals {
		if s.Type == "absence:"+signalType {
			return true
		}
	}
	return false
}

// signalResultLike abstracts the bits of *signal.CollectionResult
// the test helpers read. Avoids hard-coding the import here so
// Signals() can change shape independently.
type signalResultLike struct {
	signals []profile.Signal
}

func wrap(t *testing.T, result interface {
	Signals() []profile.Signal
}) *signalResultLike {
	t.Helper()
	return &signalResultLike{signals: result.Signals()}
}

// getSignalValue extracts the JSON-decoded value for the first
// matching signal type. Fails the test if no such signal landed.
func getSignalValue(t *testing.T, result *signalResultLike, signalType string) map[string]any {
	t.Helper()
	for _, s := range result.signals {
		if s.Type == signalType {
			var v map[string]any
			require.NoError(t, json.Unmarshal(s.Value, &v))
			return v
		}
	}
	t.Fatalf("signal %q not found in result", signalType)
	return nil
}

// ----- happy path: maintainer_count emits with the expected logins -----

func TestCollector_Collect_HappyPath_EmitsMaintainerCount(t *testing.T) {
	t.Parallel()
	srv := projectInfoServer(t, Info{
		Maintainer: "ofek",
	})
	defer srv.Close()

	raw, err := newTestCollector(srv).Collect(t.Context(), pypiEntity("hatch"))
	require.NoError(t, err)
	require.NotNil(t, raw)
	result := wrap(t, raw)

	require.True(t, hasSignal(result, "maintainer_count"),
		"a pypi package with a login-shaped maintainer must emit maintainer_count")
	mc := getSignalValue(t, result, "maintainer_count")
	assert.EqualValues(t, 1, mc["count"])
	logins, ok := mc["logins"].([]any)
	require.True(t, ok)
	assert.ElementsMatch(t, []any{"ofek"}, logins)
}

// TestCollector_Collect_NoLoginsExtractable_RecordsAbsence pins
// the contract for legacy display-name-only metadata: a package
// whose info.maintainer / info.author contain only free-text
// display names produces no logins → maintainer_count records as
// absence (not a synthetic count=0 signal). Symmetric with npm's
// behaviour when Maintainers is empty.
func TestCollector_Collect_NoLoginsExtractable_RecordsAbsence(t *testing.T) {
	t.Parallel()
	srv := projectInfoServer(t, Info{
		Author: "Saurabh Kumar", // display name, not a login
	})
	defer srv.Close()

	raw, err := newTestCollector(srv).Collect(t.Context(), pypiEntity("python-dotenv"))
	require.NoError(t, err)
	result := wrap(t, raw)

	assert.False(t, hasSignal(result, "maintainer_count"),
		"display-name-only metadata must not synthesise a maintainer_count signal")
	assert.True(t, hasAbsence(result, "maintainer_count"),
		"absence of inferable logins surfaces as absence:maintainer_count")
}

// ----- non-pypi entity: empty result, no HTTP calls -----

func TestCollector_Collect_NonPypiEntity_ReturnsEmpty(t *testing.T) {
	t.Parallel()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		calls++
	}))
	defer srv.Close()

	for _, e := range []*profile.Entity{
		{CanonicalURI: "repo:github/expressjs/express"},
		{CanonicalURI: "pkg:npm/lodash"},
		{CanonicalURI: "identity:github/alecthomas"},
		{CanonicalURI: ""},
		nil,
	} {
		raw, err := newTestCollector(srv).Collect(t.Context(), e)
		require.NoError(t, err)
		require.NotNil(t, raw)
		result := wrap(t, raw)
		assert.Equal(t, 0, len(result.signals),
			"non-pypi entity %v must produce empty result", e)
	}
	assert.Equal(t, 0, calls, "non-pypi entities must trigger zero HTTP requests")
}

// ----- registry 404: absence, not retryable -----

func TestCollector_Collect_RegistryNotFound_RecordsDefinitiveAbsence(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	raw, err := newTestCollector(srv).Collect(t.Context(), pypiEntity("nonexistent"))
	require.NoError(t, err,
		"a 404 from the registry must NOT bubble out as a Collect error — it's a per-signal absence")
	result := wrap(t, raw)

	assert.True(t, hasAbsence(result, "maintainer_count"),
		"a 404 records absence so the entity profile reflects 'we tried, registry says no'")
	require.NotEmpty(t, raw.Failures)
	assert.False(t, raw.Failures[0].Retryable,
		"404 is definitive — the package either exists or it doesn't")
}

// ----- registry 500: absence, retryable -----

func TestCollector_Collect_RegistryServerError_RecordsRetryableAbsence(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	raw, err := newTestCollector(srv).Collect(t.Context(), pypiEntity("hatch"))
	require.NoError(t, err)
	result := wrap(t, raw)
	assert.True(t, hasAbsence(result, "maintainer_count"))
	require.NotEmpty(t, raw.Failures)
	assert.True(t, raw.Failures[0].Retryable,
		"5xx is transient — re-running may succeed")
}

// ----- name preserved: the collector emits the signal source string -----

func TestCollector_Collect_SignalSourceIsPypiRegistry(t *testing.T) {
	t.Parallel()
	srv := projectInfoServer(t, Info{Maintainer: "ofek"})
	defer srv.Close()

	raw, err := newTestCollector(srv).Collect(t.Context(), pypiEntity("hatch"))
	require.NoError(t, err)

	for _, sig := range raw.Signals() {
		if sig.Type == "maintainer_count" {
			assert.Equal(t, "pypi-registry", sig.Source,
				"maintainer_count emitted from the pypi collector must carry source=pypi-registry — the cascade resolver reads this to dispatch identity:pypi/<login>")
			return
		}
	}
	t.Fatalf("expected a maintainer_count signal in the result; got none")
}

// projectServer returns an httptest server that responds with a full
// Project response including releases. Used by tests that exercise
// vitality/publication signals derived from release timestamps.
func projectServer(t *testing.T, proj Project) *httptest.Server {
	t.Helper()
	body, err := json.Marshal(proj)
	require.NoError(t, err)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
}

// TestCollector_Collect_VersionCount emits the total number of
// published versions from the releases map.
func TestCollector_Collect_VersionCount(t *testing.T) {
	t.Parallel()
	srv := projectServer(t, Project{
		Info: Info{Maintainer: "ofek"},
		Releases: map[string][]Distribution{
			"1.0.0": {{UploadTimeISO: "2024-01-01T00:00:00Z"}},
			"1.1.0": {{UploadTimeISO: "2025-01-01T00:00:00Z"}},
			"2.0.0": {{UploadTimeISO: "2026-01-01T00:00:00Z"}},
		},
	})
	defer srv.Close()

	raw, err := newTestCollector(srv).Collect(t.Context(), pypiEntity("hatch"))
	require.NoError(t, err)
	result := wrap(t, raw)

	require.True(t, hasSignal(result, "version_count"),
		"pypi package with releases must emit version_count")
	vc := getSignalValue(t, result, "version_count")
	assert.EqualValues(t, 3, vc["count"])
}

// TestCollector_Collect_LastPublish emits the latest version's
// publish timestamp from the releases map.
func TestCollector_Collect_LastPublish(t *testing.T) {
	t.Parallel()
	srv := projectServer(t, Project{
		Info: Info{Maintainer: "ofek"},
		Releases: map[string][]Distribution{
			"1.0.0": {{UploadTimeISO: "2024-01-01T00:00:00Z"}},
			"2.0.0": {{UploadTimeISO: "2026-03-15T12:00:00Z"}},
		},
	})
	defer srv.Close()

	raw, err := newTestCollector(srv).Collect(t.Context(), pypiEntity("hatch"))
	require.NoError(t, err)
	result := wrap(t, raw)

	require.True(t, hasSignal(result, "last_publish"),
		"pypi package with releases must emit last_publish")
	lp := getSignalValue(t, result, "last_publish")
	assert.Equal(t, "2.0.0", lp["latest_version"])
	assert.Equal(t, "2026-03-15T12:00:00Z", lp["published_at"])
	daysAgo, ok := lp["days_ago"].(float64)
	assert.True(t, ok)
	assert.GreaterOrEqual(t, daysAgo, float64(0))
}

// TestCollector_Collect_VersionPublishBurst_Burst detects rapid-fire
// publishing: 4 versions within 36 hours.
func TestCollector_Collect_VersionPublishBurst_Burst(t *testing.T) {
	t.Parallel()
	srv := projectServer(t, Project{
		Info: Info{Maintainer: "spam"},
		Releases: map[string][]Distribution{
			"1.0.0": {{UploadTimeISO: "2026-04-10T06:00:00Z"}},
			"1.1.0": {{UploadTimeISO: "2026-04-10T18:00:00Z"}},
			"1.2.0": {{UploadTimeISO: "2026-04-11T06:00:00Z"}},
			"1.3.0": {{UploadTimeISO: "2026-04-11T18:00:00Z"}},
		},
	})
	defer srv.Close()

	raw, err := newTestCollector(srv).Collect(t.Context(), pypiEntity("spam-pkg"))
	require.NoError(t, err)
	result := wrap(t, raw)

	require.True(t, hasSignal(result, "version_publish_burst"),
		"pypi package with recent releases must emit version_publish_burst")
	vpb := getSignalValue(t, result, "version_publish_burst")
	assert.Equal(t, true, vpb["burst_detected"])
	assert.EqualValues(t, 4, vpb["versions_in_window"])
	assert.EqualValues(t, 36, vpb["window_hours"])
}

// TestCollector_Collect_VersionPublishBurst_NoBurst — versions spread
// over months.
func TestCollector_Collect_VersionPublishBurst_NoBurst(t *testing.T) {
	t.Parallel()
	srv := projectServer(t, Project{
		Info: Info{Maintainer: "stable"},
		Releases: map[string][]Distribution{
			"1.0.0": {{UploadTimeISO: "2024-01-15T00:00:00Z"}},
			"2.0.0": {{UploadTimeISO: "2025-03-20T00:00:00Z"}},
			"3.0.0": {{UploadTimeISO: "2026-02-10T00:00:00Z"}},
		},
	})
	defer srv.Close()

	raw, err := newTestCollector(srv).Collect(t.Context(), pypiEntity("stable-pkg"))
	require.NoError(t, err)
	result := wrap(t, raw)

	require.True(t, hasSignal(result, "version_publish_burst"))
	vpb := getSignalValue(t, result, "version_publish_burst")
	assert.Equal(t, false, vpb["burst_detected"])
	assert.EqualValues(t, 3, vpb["versions_in_window"])
}

// TestCollector_Collect_NoReleases_VersionSignalsAbsent — when the
// releases map is empty, version-derived signals record absence.
func TestCollector_Collect_NoReleases_VersionSignalsAbsent(t *testing.T) {
	t.Parallel()
	srv := projectServer(t, Project{
		Info: Info{Maintainer: "ofek"},
		// No releases — common for newly-registered but unpublished projects.
	})
	defer srv.Close()

	raw, err := newTestCollector(srv).Collect(t.Context(), pypiEntity("empty-pkg"))
	require.NoError(t, err)
	result := wrap(t, raw)

	assert.True(t, hasAbsence(result, "last_publish"),
		"no releases → last_publish absence")
	assert.True(t, hasAbsence(result, "sdist_only_present"),
		"no releases → sdist_only_present absence")
	assert.True(t, hasAbsence(result, "sdist_only_introduced"),
		"no releases → sdist_only_introduced absence")
	// version_count should still emit with count=0
	require.True(t, hasSignal(result, "version_count"))
	vc := getSignalValue(t, result, "version_count")
	assert.EqualValues(t, 0, vc["count"])
	// yanked_release_count should emit with count=0
	require.True(t, hasSignal(result, "yanked_release_count"))
	yr := getSignalValue(t, result, "yanked_release_count")
	assert.EqualValues(t, 0, yr["count"])
	assert.EqualValues(t, 0, yr["total_versions"])
}

// ----- yanked_release_count -----

func TestCollector_Collect_YankedReleaseCount(t *testing.T) {
	t.Parallel()
	srv := projectServer(t, Project{
		Info: Info{Maintainer: "alice"},
		Releases: map[string][]Distribution{
			"1.0.0": {{UploadTimeISO: "2024-01-01T00:00:00Z", PackageType: "bdist_wheel"}},
			"1.1.0": {{UploadTimeISO: "2025-01-01T00:00:00Z", PackageType: "bdist_wheel", Yanked: true}},
			"1.2.0": {{UploadTimeISO: "2025-06-01T00:00:00Z", PackageType: "bdist_wheel", Yanked: true}},
			"2.0.0": {{UploadTimeISO: "2026-01-01T00:00:00Z", PackageType: "bdist_wheel"}},
		},
	})
	defer srv.Close()

	raw, err := newTestCollector(srv).Collect(t.Context(), pypiEntity("some-pkg"))
	require.NoError(t, err)
	result := wrap(t, raw)

	require.True(t, hasSignal(result, "yanked_release_count"))
	yr := getSignalValue(t, result, "yanked_release_count")
	assert.EqualValues(t, 2, yr["count"], "two versions are yanked")
	assert.EqualValues(t, 4, yr["total_versions"])
}

func TestCollector_Collect_YankedReleaseCount_NoneYanked(t *testing.T) {
	t.Parallel()
	srv := projectServer(t, Project{
		Info: Info{Maintainer: "bob"},
		Releases: map[string][]Distribution{
			"1.0.0": {{UploadTimeISO: "2024-01-01T00:00:00Z", PackageType: "bdist_wheel"}},
			"2.0.0": {{UploadTimeISO: "2026-01-01T00:00:00Z", PackageType: "bdist_wheel"}},
		},
	})
	defer srv.Close()

	raw, err := newTestCollector(srv).Collect(t.Context(), pypiEntity("clean-pkg"))
	require.NoError(t, err)
	result := wrap(t, raw)

	require.True(t, hasSignal(result, "yanked_release_count"))
	yr := getSignalValue(t, result, "yanked_release_count")
	assert.EqualValues(t, 0, yr["count"])
	assert.EqualValues(t, 2, yr["total_versions"])
}

// ----- sdist_only_present -----

func TestCollector_Collect_SdistOnlyPresent_True(t *testing.T) {
	t.Parallel()
	// Latest version has only sdist — setup.py runs at install.
	srv := projectServer(t, Project{
		Info: Info{Maintainer: "dev"},
		Releases: map[string][]Distribution{
			"1.0.0": {
				{UploadTimeISO: "2024-01-01T00:00:00Z", PackageType: "bdist_wheel"},
				{UploadTimeISO: "2024-01-01T00:00:00Z", PackageType: "sdist"},
			},
			"2.0.0": {
				{UploadTimeISO: "2026-01-01T00:00:00Z", PackageType: "sdist"},
			},
		},
	})
	defer srv.Close()

	raw, err := newTestCollector(srv).Collect(t.Context(), pypiEntity("native-pkg"))
	require.NoError(t, err)
	result := wrap(t, raw)

	require.True(t, hasSignal(result, "sdist_only_present"))
	sd := getSignalValue(t, result, "sdist_only_present")
	assert.Equal(t, true, sd["present"],
		"latest version with only sdist should flag present=true")
	assert.Equal(t, "2.0.0", sd["version_checked"])
}

func TestCollector_Collect_SdistOnlyPresent_False(t *testing.T) {
	t.Parallel()
	// Latest version has a wheel — no setup.py execution risk.
	srv := projectServer(t, Project{
		Info: Info{Maintainer: "dev"},
		Releases: map[string][]Distribution{
			"1.0.0": {
				{UploadTimeISO: "2024-01-01T00:00:00Z", PackageType: "bdist_wheel"},
			},
			"2.0.0": {
				{UploadTimeISO: "2026-01-01T00:00:00Z", PackageType: "bdist_wheel"},
				{UploadTimeISO: "2026-01-01T00:00:00Z", PackageType: "sdist"},
			},
		},
	})
	defer srv.Close()

	raw, err := newTestCollector(srv).Collect(t.Context(), pypiEntity("wheeled-pkg"))
	require.NoError(t, err)
	result := wrap(t, raw)

	require.True(t, hasSignal(result, "sdist_only_present"))
	sd := getSignalValue(t, result, "sdist_only_present")
	assert.Equal(t, false, sd["present"],
		"latest version with wheel should flag present=false")
}

// ----- sdist_only_introduced -----

func TestCollector_Collect_SdistOnlyIntroduced_DetectsTransition(t *testing.T) {
	t.Parallel()
	// Versions 1.0 and 1.1 had wheels; 2.0 dropped them — the
	// "setup.py forced" transition.
	srv := projectServer(t, Project{
		Info: Info{Maintainer: "attacker"},
		Releases: map[string][]Distribution{
			"1.0.0": {
				{UploadTimeISO: "2024-01-01T00:00:00Z", PackageType: "bdist_wheel"},
				{UploadTimeISO: "2024-01-01T00:00:00Z", PackageType: "sdist"},
			},
			"1.1.0": {
				{UploadTimeISO: "2025-01-01T00:00:00Z", PackageType: "bdist_wheel"},
				{UploadTimeISO: "2025-01-01T00:00:00Z", PackageType: "sdist"},
			},
			"2.0.0": {
				{UploadTimeISO: "2026-04-01T00:00:00Z", PackageType: "sdist"},
			},
		},
	})
	defer srv.Close()

	raw, err := newTestCollector(srv).Collect(t.Context(), pypiEntity("compromised"))
	require.NoError(t, err)
	result := wrap(t, raw)

	require.True(t, hasSignal(result, "sdist_only_introduced"))
	si := getSignalValue(t, result, "sdist_only_introduced")
	assert.Equal(t, true, si["present_in_latest"],
		"latest is sdist-only")
	assert.Equal(t, true, si["introduced_recently"],
		"transition from wheel to sdist-only should flag")
	assert.Equal(t, "2.0.0", si["introduced_at_version"])
	assert.EqualValues(t, 2, si["prior_versions_without"],
		"both prior versions had wheels (sdist_only=false)")
	assert.EqualValues(t, 3, si["versions_checked"])
}

func TestCollector_Collect_SdistOnlyIntroduced_ConsistentWheel(t *testing.T) {
	t.Parallel()
	// All versions have wheels — no transition.
	srv := projectServer(t, Project{
		Info: Info{Maintainer: "dev"},
		Releases: map[string][]Distribution{
			"1.0.0": {
				{UploadTimeISO: "2024-01-01T00:00:00Z", PackageType: "bdist_wheel"},
			},
			"2.0.0": {
				{UploadTimeISO: "2025-01-01T00:00:00Z", PackageType: "bdist_wheel"},
				{UploadTimeISO: "2025-01-01T00:00:00Z", PackageType: "sdist"},
			},
			"3.0.0": {
				{UploadTimeISO: "2026-01-01T00:00:00Z", PackageType: "bdist_wheel"},
			},
		},
	})
	defer srv.Close()

	raw, err := newTestCollector(srv).Collect(t.Context(), pypiEntity("safe-pkg"))
	require.NoError(t, err)
	result := wrap(t, raw)

	require.True(t, hasSignal(result, "sdist_only_introduced"))
	si := getSignalValue(t, result, "sdist_only_introduced")
	assert.Equal(t, false, si["present_in_latest"])
	assert.Equal(t, false, si["introduced_recently"])
}

func TestCollector_Collect_SdistOnlyIntroduced_AlwaysSdist(t *testing.T) {
	t.Parallel()
	// All versions are sdist-only — consistent, not a transition.
	srv := projectServer(t, Project{
		Info: Info{Maintainer: "legacy"},
		Releases: map[string][]Distribution{
			"0.1.0": {
				{UploadTimeISO: "2024-01-01T00:00:00Z", PackageType: "sdist"},
			},
			"0.2.0": {
				{UploadTimeISO: "2025-01-01T00:00:00Z", PackageType: "sdist"},
			},
			"0.3.0": {
				{UploadTimeISO: "2026-01-01T00:00:00Z", PackageType: "sdist"},
			},
		},
	})
	defer srv.Close()

	raw, err := newTestCollector(srv).Collect(t.Context(), pypiEntity("always-sdist"))
	require.NoError(t, err)
	result := wrap(t, raw)

	require.True(t, hasSignal(result, "sdist_only_introduced"))
	si := getSignalValue(t, result, "sdist_only_introduced")
	assert.Equal(t, true, si["present_in_latest"],
		"latest is sdist-only")
	assert.Equal(t, false, si["introduced_recently"],
		"all versions are sdist-only — not a transition")
	assert.EqualValues(t, 0, si["prior_versions_without"],
		"no prior version had wheels")
}

// ----- gpg_signature_present (legacy has_sig) -----

func TestCollector_Collect_GPGSignaturePresent_True(t *testing.T) {
	t.Parallel()
	// Latest version was uploaded with a GPG signature (pre-2023 artifact).
	srv := projectServer(t, Project{
		Info: Info{Maintainer: "signer"},
		Releases: map[string][]Distribution{
			"1.0.0": {
				{UploadTimeISO: "2022-06-01T00:00:00Z", PackageType: "bdist_wheel", HasSig: true},
				{UploadTimeISO: "2022-06-01T00:00:00Z", PackageType: "sdist", HasSig: true},
			},
		},
	})
	defer srv.Close()

	raw, err := newTestCollector(srv).Collect(t.Context(), pypiEntity("old-signed"))
	require.NoError(t, err)
	result := wrap(t, raw)

	require.True(t, hasSignal(result, "gpg_signature_present"))
	gs := getSignalValue(t, result, "gpg_signature_present")
	assert.Equal(t, true, gs["present"])
	assert.Equal(t, "1.0.0", gs["version_checked"])
}

func TestCollector_Collect_GPGSignaturePresent_False(t *testing.T) {
	t.Parallel()
	// Latest version has no GPG signature (post-2023, expected).
	srv := projectServer(t, Project{
		Info: Info{Maintainer: "modern"},
		Releases: map[string][]Distribution{
			"1.0.0": {
				{UploadTimeISO: "2022-01-01T00:00:00Z", PackageType: "bdist_wheel", HasSig: true},
			},
			"2.0.0": {
				{UploadTimeISO: "2026-01-01T00:00:00Z", PackageType: "bdist_wheel", HasSig: false},
			},
		},
	})
	defer srv.Close()

	raw, err := newTestCollector(srv).Collect(t.Context(), pypiEntity("no-sig"))
	require.NoError(t, err)
	result := wrap(t, raw)

	require.True(t, hasSignal(result, "gpg_signature_present"))
	gs := getSignalValue(t, result, "gpg_signature_present")
	assert.Equal(t, false, gs["present"])
	assert.Equal(t, "2.0.0", gs["version_checked"])
}

// ----- trusted_publishing (PEP 740 Sigstore attestation) -----

// attestationProjectServer serves both the /pypi/<name>/json endpoint
// and the /integrity/<project>/<version>/<filename>/provenance endpoint.
// Models the real PyPI architecture where attestation data is a
// separate API call from the project metadata.
func attestationProjectServer(t *testing.T, proj Project, attestation *AttestationResponse) *httptest.Server {
	t.Helper()
	projBody, err := json.Marshal(proj)
	require.NoError(t, err)

	var attestBody []byte
	if attestation != nil {
		attestBody, err = json.Marshal(attestation)
		require.NoError(t, err)
	}

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/integrity/") {
			if attestation == nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write(attestBody)
			return
		}
		_, _ = w.Write(projBody)
	}))
}

func TestCollector_Collect_TrustedPublishing_Present(t *testing.T) {
	t.Parallel()
	// Latest version has a Sigstore attestation from a GitHub Actions
	// trusted publisher.
	srv := attestationProjectServer(t,
		Project{
			Info: Info{Maintainer: "dev"},
			Releases: map[string][]Distribution{
				"1.0.0": {
					{UploadTimeISO: "2024-01-01T00:00:00Z", PackageType: "bdist_wheel", Filename: "pkg-1.0.0-py3-none-any.whl"},
				},
				"2.0.0": {
					{UploadTimeISO: "2026-01-01T00:00:00Z", PackageType: "bdist_wheel", Filename: "pkg-2.0.0-py3-none-any.whl"},
					{UploadTimeISO: "2026-01-01T00:00:00Z", PackageType: "sdist", Filename: "pkg-2.0.0.tar.gz"},
				},
			},
		},
		&AttestationResponse{
			Version: 1,
			Bundles: []AttestationBundle{
				{
					Publisher: AttestationPublisher{
						Kind:        "GitHub",
						Repository:  "octocat/pkg",
						Workflow:    "release.yml",
						Environment: "release",
					},
				},
			},
		},
	)
	defer srv.Close()

	raw, err := newTestCollector(srv).Collect(t.Context(), pypiEntity("pkg"))
	require.NoError(t, err)
	result := wrap(t, raw)

	require.True(t, hasSignal(result, "trusted_publishing"),
		"package with attestation must emit trusted_publishing")
	tp := getSignalValue(t, result, "trusted_publishing")
	assert.Equal(t, true, tp["present"])
	assert.Equal(t, "2.0.0", tp["version_checked"])
	assert.Equal(t, "GitHub", tp["publisher_kind"])
	assert.Equal(t, "octocat/pkg", tp["source_repository"])
	assert.Equal(t, "release.yml", tp["workflow"])
}

func TestCollector_Collect_TrustedPublishing_Absent(t *testing.T) {
	t.Parallel()
	// Latest version has no attestation (Integrity API returns 404).
	srv := attestationProjectServer(t,
		Project{
			Info: Info{Maintainer: "dev"},
			Releases: map[string][]Distribution{
				"1.0.0": {
					{UploadTimeISO: "2024-01-01T00:00:00Z", PackageType: "bdist_wheel", Filename: "old-1.0.0-py3-none-any.whl"},
				},
				"2.0.0": {
					{UploadTimeISO: "2026-01-01T00:00:00Z", PackageType: "bdist_wheel", Filename: "old-2.0.0-py3-none-any.whl"},
				},
			},
		},
		nil, // no attestation
	)
	defer srv.Close()

	raw, err := newTestCollector(srv).Collect(t.Context(), pypiEntity("old"))
	require.NoError(t, err)
	result := wrap(t, raw)

	require.True(t, hasSignal(result, "trusted_publishing"),
		"package without attestation must still emit trusted_publishing (with present=false)")
	tp := getSignalValue(t, result, "trusted_publishing")
	assert.Equal(t, false, tp["present"])
	assert.Equal(t, "2.0.0", tp["version_checked"])
}

func TestCollector_Collect_TrustedPublishing_IntegrityError_RecordsAbsence(t *testing.T) {
	t.Parallel()
	// The Integrity API returns 500 — the signal is recorded as absence
	// (retryable) rather than failing the entire collection.
	projBody, err := json.Marshal(Project{
		Info: Info{Maintainer: "dev"},
		Releases: map[string][]Distribution{
			"1.0.0": {
				{UploadTimeISO: "2026-01-01T00:00:00Z", PackageType: "bdist_wheel", Filename: "pkg-1.0.0-py3-none-any.whl"},
			},
		},
	})
	require.NoError(t, err)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/integrity/") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(projBody)
	}))
	defer srv.Close()

	raw, err := newTestCollector(srv).Collect(t.Context(), pypiEntity("broken"))
	require.NoError(t, err)
	result := wrap(t, raw)

	// The other signals should still emit normally.
	require.True(t, hasSignal(result, "last_publish"),
		"non-attestation signals must not be affected by integrity API failure")
	// trusted_publishing should be absent, not emitted.
	assert.True(t, hasAbsence(result, "trusted_publishing"),
		"integrity API 500 should produce absence:trusted_publishing")
}

func TestCollector_Collect_TrustedPublishing_NoReleases_SkipsAttestation(t *testing.T) {
	t.Parallel()
	// When there are no releases, the attestation check is skipped
	// (no version/filename to look up).
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/integrity/") {
			calls++
		}
		w.Header().Set("Content-Type", "application/json")
		body, _ := json.Marshal(Project{
			Info: Info{Maintainer: "dev"},
		})
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	raw, err := newTestCollector(srv).Collect(t.Context(), pypiEntity("empty"))
	require.NoError(t, err)
	_ = wrap(t, raw)

	assert.Equal(t, 0, calls, "no integrity API call when there are no releases")
}

// TestCollector_Name pins the collector identifier for orchestrator
// dispatch and progress narration. Mirrors the npm collector's name
// pattern so log lines read consistently.
func TestCollector_Name(t *testing.T) {
	t.Parallel()
	c := NewCollector()
	assert.Equal(t, "pypi-registry", c.Name())
}

// --- Phase B: attestation_consistency tests ---

// perVersionAttestationServer builds an httptest server that returns
// project metadata on /pypi/<name>/json and per-version attestation
// responses on /integrity/<project>/<version>/<filename>/provenance.
// The attestations map keys on version string; nil value → 404,
// non-nil → 200 with the response body. Unlisted versions return 404.
func perVersionAttestationServer(t *testing.T, proj Project, attestations map[string]*AttestationResponse) *httptest.Server {
	t.Helper()
	projBody, err := json.Marshal(proj)
	require.NoError(t, err)

	attestBodies := make(map[string][]byte, len(attestations))
	for ver, attest := range attestations {
		if attest != nil {
			body, err := json.Marshal(attest)
			require.NoError(t, err)
			attestBodies[ver] = body
		}
	}

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if after, ok := strings.CutPrefix(r.URL.Path, "/integrity/"); ok {
			// Path: /integrity/<project>/<version>/<filename>/provenance
			parts := strings.Split(after, "/")
			if len(parts) >= 2 {
				version := parts[1]
				if body, ok := attestBodies[version]; ok {
					_, _ = w.Write(body)
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(projBody)
	}))
}

func makeAttestation(publisher, repo, workflow string) *AttestationResponse {
	return &AttestationResponse{
		Version: 1,
		Bundles: []AttestationBundle{
			{
				Publisher: AttestationPublisher{
					Kind:       publisher,
					Repository: repo,
					Workflow:   workflow,
				},
			},
		},
	}
}

func TestCollector_AttestationConsistency_OnlyOneVersion_NoSignal(t *testing.T) {
	t.Parallel()
	// A package with only one version has no history to compare.
	// No attestation_consistency signal should be emitted.
	srv := perVersionAttestationServer(t,
		Project{
			Info: Info{Maintainer: "dev"},
			Releases: map[string][]Distribution{
				"1.0.0": {
					{UploadTimeISO: "2026-01-01T00:00:00Z", PackageType: "bdist_wheel", Filename: "pkg-1.0.0-py3-none-any.whl"},
				},
			},
		},
		map[string]*AttestationResponse{
			"1.0.0": makeAttestation("GitHub", "owner/pkg", "release.yml"),
		},
	)
	defer srv.Close()

	raw, err := newTestCollector(srv).Collect(t.Context(), pypiEntity("pkg"))
	require.NoError(t, err)
	result := wrap(t, raw)

	assert.False(t, hasSignal(result, "attestation_consistency"),
		"single-version package must not emit attestation_consistency (no history)")
}

func TestCollector_AttestationConsistency_NeverAdopted_NoSignal(t *testing.T) {
	t.Parallel()
	// Latest and first prior are both unattested — the package never
	// adopted trusted publishing. Progressive probe early-exits.
	srv := perVersionAttestationServer(t,
		Project{
			Info: Info{Maintainer: "dev"},
			Releases: map[string][]Distribution{
				"1.0.0": {
					{UploadTimeISO: "2024-01-01T00:00:00Z", PackageType: "bdist_wheel", Filename: "pkg-1.0.0-py3-none-any.whl"},
				},
				"2.0.0": {
					{UploadTimeISO: "2025-01-01T00:00:00Z", PackageType: "bdist_wheel", Filename: "pkg-2.0.0-py3-none-any.whl"},
				},
				"3.0.0": {
					{UploadTimeISO: "2026-01-01T00:00:00Z", PackageType: "bdist_wheel", Filename: "pkg-3.0.0-py3-none-any.whl"},
				},
			},
		},
		map[string]*AttestationResponse{
			// All versions return 404 — no attestations anywhere.
		},
	)
	defer srv.Close()

	raw, err := newTestCollector(srv).Collect(t.Context(), pypiEntity("pkg"))
	require.NoError(t, err)
	result := wrap(t, raw)

	assert.False(t, hasSignal(result, "attestation_consistency"),
		"never-adopted package must not emit attestation_consistency (probe early-exit)")
}

func TestCollector_AttestationConsistency_AxiosPattern_DetectsTransition(t *testing.T) {
	t.Parallel()
	// The attack scenario: latest version is unattested, but prior
	// versions were attested. This is the broken-chain fingerprint.
	ghAttest := makeAttestation("GitHub", "psf/requests", "release.yml")
	srv := perVersionAttestationServer(t,
		Project{
			Info: Info{Maintainer: "dev"},
			Releases: map[string][]Distribution{
				"2.28.0": {
					{UploadTimeISO: "2025-06-01T00:00:00Z", PackageType: "bdist_wheel", Filename: "requests-2.28.0-py3-none-any.whl"},
				},
				"2.29.0": {
					{UploadTimeISO: "2025-09-01T00:00:00Z", PackageType: "bdist_wheel", Filename: "requests-2.29.0-py3-none-any.whl"},
				},
				"2.30.0": {
					{UploadTimeISO: "2025-12-01T00:00:00Z", PackageType: "bdist_wheel", Filename: "requests-2.30.0-py3-none-any.whl"},
				},
				"2.31.0": {
					{UploadTimeISO: "2026-02-01T00:00:00Z", PackageType: "bdist_wheel", Filename: "requests-2.31.0-py3-none-any.whl"},
				},
				"2.32.0": {
					{UploadTimeISO: "2026-04-01T00:00:00Z", PackageType: "bdist_wheel", Filename: "requests-2.32.0-py3-none-any.whl"},
				},
			},
		},
		map[string]*AttestationResponse{
			"2.28.0": ghAttest,
			"2.29.0": ghAttest,
			"2.30.0": ghAttest,
			"2.31.0": ghAttest,
			// 2.32.0 (latest) is NOT attested — 404.
		},
	)
	defer srv.Close()

	raw, err := newTestCollector(srv).Collect(t.Context(), pypiEntity("requests"))
	require.NoError(t, err)
	result := wrap(t, raw)

	require.True(t, hasSignal(result, "attestation_consistency"),
		"broken attestation chain must emit attestation_consistency")
	val := getSignalValue(t, result, "attestation_consistency")
	assert.Equal(t, false, val["consistent"])
	assert.Equal(t, true, val["transition_detected"])
	assert.Equal(t, "attested_to_unattested", val["transition_direction"])
	assert.Equal(t, "2.32.0", val["transition_at_version"])
	assert.Equal(t, false, val["publisher_changed"])
}

func TestCollector_AttestationConsistency_AllAttested_Consistent(t *testing.T) {
	t.Parallel()
	// All versions have attestations from the same publisher.
	// This is the healthy state — unbroken chain.
	ghAttest := makeAttestation("GitHub", "pallets/flask", "publish.yml")
	srv := perVersionAttestationServer(t,
		Project{
			Info: Info{Maintainer: "dev"},
			Releases: map[string][]Distribution{
				"3.0.0": {
					{UploadTimeISO: "2025-06-01T00:00:00Z", PackageType: "bdist_wheel", Filename: "flask-3.0.0-py3-none-any.whl"},
				},
				"3.1.0": {
					{UploadTimeISO: "2025-09-01T00:00:00Z", PackageType: "bdist_wheel", Filename: "flask-3.1.0-py3-none-any.whl"},
				},
				"3.2.0": {
					{UploadTimeISO: "2026-01-01T00:00:00Z", PackageType: "bdist_wheel", Filename: "flask-3.2.0-py3-none-any.whl"},
				},
			},
		},
		map[string]*AttestationResponse{
			"3.0.0": ghAttest,
			"3.1.0": ghAttest,
			"3.2.0": ghAttest,
		},
	)
	defer srv.Close()

	raw, err := newTestCollector(srv).Collect(t.Context(), pypiEntity("flask"))
	require.NoError(t, err)
	result := wrap(t, raw)

	require.True(t, hasSignal(result, "attestation_consistency"),
		"fully-attested chain must emit attestation_consistency")
	val := getSignalValue(t, result, "attestation_consistency")
	assert.Equal(t, true, val["consistent"])
	assert.Equal(t, false, val["transition_detected"])
	assert.Equal(t, false, val["publisher_changed"])
}

func TestCollector_AttestationConsistency_ProbeError_RecordsAbsence(t *testing.T) {
	t.Parallel()
	// The probe call (first prior version) hits a 500. The signal
	// should be recorded as absence (retryable), not crash collection.
	projBody, err := json.Marshal(Project{
		Info: Info{Maintainer: "dev"},
		Releases: map[string][]Distribution{
			"1.0.0": {
				{UploadTimeISO: "2025-01-01T00:00:00Z", PackageType: "bdist_wheel", Filename: "pkg-1.0.0-py3-none-any.whl"},
			},
			"2.0.0": {
				{UploadTimeISO: "2026-01-01T00:00:00Z", PackageType: "bdist_wheel", Filename: "pkg-2.0.0-py3-none-any.whl"},
			},
		},
	})
	require.NoError(t, err)

	latestAttest, err := json.Marshal(makeAttestation("GitHub", "owner/pkg", "ci.yml"))
	require.NoError(t, err)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if after, ok := strings.CutPrefix(r.URL.Path, "/integrity/"); ok {
			parts := strings.Split(after, "/")
			if len(parts) >= 2 && parts[1] == "2.0.0" {
				// Latest version — return attestation (Phase A succeeds).
				_, _ = w.Write(latestAttest)
				return
			}
			// All other versions — 500 error.
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(projBody)
	}))
	defer srv.Close()

	raw, err := newTestCollector(srv).Collect(t.Context(), pypiEntity("broken"))
	require.NoError(t, err)
	result := wrap(t, raw)

	// Phase A should succeed (trusted_publishing emitted).
	require.True(t, hasSignal(result, "trusted_publishing"))

	// Phase B probe failed — should be absence, not a signal.
	assert.False(t, hasSignal(result, "attestation_consistency"),
		"probe error must not produce attestation_consistency signal")
	assert.True(t, hasAbsence(result, "attestation_consistency"),
		"probe error must record attestation_consistency absence")
}

func TestCollector_AttestationConsistency_PublisherChanged_DetectedCorrectly(t *testing.T) {
	t.Parallel()
	// Two different publishers across attested versions — verifies that
	// publisher_changed is reported correctly and prior_publisher is
	// extracted without corruption (regression: colon in field values
	// must not confuse the publisher identity comparison).
	srv := perVersionAttestationServer(t,
		Project{
			Info: Info{Maintainer: "dev"},
			Releases: map[string][]Distribution{
				"1.0.0": {
					{UploadTimeISO: "2025-06-01T00:00:00Z", PackageType: "bdist_wheel", Filename: "pkg-1.0.0-py3-none-any.whl"},
				},
				"2.0.0": {
					{UploadTimeISO: "2025-12-01T00:00:00Z", PackageType: "bdist_wheel", Filename: "pkg-2.0.0-py3-none-any.whl"},
				},
				"3.0.0": {
					{UploadTimeISO: "2026-03-01T00:00:00Z", PackageType: "bdist_wheel", Filename: "pkg-3.0.0-py3-none-any.whl"},
				},
			},
		},
		map[string]*AttestationResponse{
			"1.0.0": makeAttestation("GitHub", "original-org/pkg", "release.yml"),
			"2.0.0": makeAttestation("GitHub", "new-org/pkg", "publish.yml"),
			"3.0.0": makeAttestation("GitHub", "new-org/pkg", "publish.yml"),
		},
	)
	defer srv.Close()

	raw, err := newTestCollector(srv).Collect(t.Context(), pypiEntity("pkg"))
	require.NoError(t, err)
	result := wrap(t, raw)

	require.True(t, hasSignal(result, "attestation_consistency"),
		"all-attested chain with different publishers must emit signal")
	val := getSignalValue(t, result, "attestation_consistency")
	assert.Equal(t, true, val["consistent"],
		"all versions attested → consistent=true (publisher_changed is orthogonal)")
	assert.Equal(t, true, val["publisher_changed"],
		"different publishers across versions must set publisher_changed=true")

	// Verify prior_publisher is extracted correctly from the most recent
	// attested prior version (versions[1] = 2.0.0, publisher "new-org/pkg").
	pp, ok := val["prior_publisher"].(map[string]any)
	require.True(t, ok, "prior_publisher must be present as a map")
	assert.Equal(t, "GitHub", pp["kind"])
	assert.Equal(t, "new-org/pkg", pp["repository"])
	assert.Equal(t, "publish.yml", pp["workflow"])
}

func TestCollector_AttestationConsistency_PublisherKey_ColonInFieldsNoCollision(t *testing.T) {
	t.Parallel()
	// Regression test: if a publisher field contains ":", the publisher
	// identity comparison must still distinguish different publishers
	// and the prior_publisher extraction must not corrupt the values.
	//
	// Publisher A: Kind="GitHub", Repository="group:sub/repo", Workflow="ci.yml"
	// Publisher B: Kind="GitHub", Repository="group", Workflow="sub/repo:ci.yml"
	// These are different publishers — publisher_changed must be true.
	srv := perVersionAttestationServer(t,
		Project{
			Info: Info{Maintainer: "dev"},
			Releases: map[string][]Distribution{
				"1.0.0": {
					{UploadTimeISO: "2025-06-01T00:00:00Z", PackageType: "bdist_wheel", Filename: "pkg-1.0.0-py3-none-any.whl"},
				},
				"2.0.0": {
					{UploadTimeISO: "2026-01-01T00:00:00Z", PackageType: "bdist_wheel", Filename: "pkg-2.0.0-py3-none-any.whl"},
				},
			},
		},
		map[string]*AttestationResponse{
			// Publisher A — repository contains ":"
			"1.0.0": makeAttestation("GitHub", "group:sub/repo", "ci.yml"),
			// Publisher B — workflow contains ":"
			"2.0.0": makeAttestation("GitHub", "group", "sub/repo:ci.yml"),
		},
	)
	defer srv.Close()

	raw, err := newTestCollector(srv).Collect(t.Context(), pypiEntity("pkg"))
	require.NoError(t, err)
	result := wrap(t, raw)

	require.True(t, hasSignal(result, "attestation_consistency"))
	val := getSignalValue(t, result, "attestation_consistency")
	assert.Equal(t, true, val["publisher_changed"],
		"publishers with colon in different fields must NOT collide — they are distinct")

	// Verify prior_publisher reconstruction doesn't corrupt the fields.
	pp, ok := val["prior_publisher"].(map[string]any)
	require.True(t, ok, "prior_publisher must be present")
	assert.Equal(t, "GitHub", pp["kind"])
	assert.Equal(t, "group:sub/repo", pp["repository"],
		"repository field must survive round-trip without colon-separator corruption")
	assert.Equal(t, "ci.yml", pp["workflow"],
		"workflow field must survive round-trip without colon-separator corruption")
}

func TestCollector_AttestationConsistency_AdoptionTransition(t *testing.T) {
	t.Parallel()
	// Adoption: latest version IS attested but prior versions were NOT.
	// This is the positive transition (maintainer adopts trusted publishing).
	ghAttest := makeAttestation("GitHub", "owner/pkg", "release.yml")
	srv := perVersionAttestationServer(t,
		Project{
			Info: Info{Maintainer: "dev"},
			Releases: map[string][]Distribution{
				"1.0.0": {
					{UploadTimeISO: "2024-06-01T00:00:00Z", PackageType: "bdist_wheel", Filename: "pkg-1.0.0-py3-none-any.whl"},
				},
				"2.0.0": {
					{UploadTimeISO: "2025-06-01T00:00:00Z", PackageType: "bdist_wheel", Filename: "pkg-2.0.0-py3-none-any.whl"},
				},
				"3.0.0": {
					{UploadTimeISO: "2026-01-01T00:00:00Z", PackageType: "bdist_wheel", Filename: "pkg-3.0.0-py3-none-any.whl"},
				},
			},
		},
		map[string]*AttestationResponse{
			// Only the latest version is attested — prior versions were not.
			"3.0.0": ghAttest,
		},
	)
	defer srv.Close()

	raw, err := newTestCollector(srv).Collect(t.Context(), pypiEntity("pkg"))
	require.NoError(t, err)
	result := wrap(t, raw)

	require.True(t, hasSignal(result, "attestation_consistency"),
		"adoption transition must emit attestation_consistency")
	val := getSignalValue(t, result, "attestation_consistency")
	assert.Equal(t, false, val["consistent"])
	assert.Equal(t, true, val["transition_detected"])
	assert.Equal(t, "unattested_to_attested", val["transition_direction"])
	assert.Equal(t, "3.0.0", val["transition_at_version"])
	assert.Equal(t, float64(3), val["versions_checked"])
	assert.Equal(t, float64(1), val["versions_attested"])
	assert.Equal(t, float64(2), val["versions_unattested"])
}

func TestCollector_AttestationConsistency_DegradeGracefully_SkipsCounted(t *testing.T) {
	t.Parallel()
	// 5 versions: latest + first prior attested (probe passes), but
	// remaining 3 versions all return 500. The signal should still emit
	// with reduced versions_checked and report versions_skipped > 0.
	ghAttest := makeAttestation("GitHub", "owner/pkg", "release.yml")

	projBody, err := json.Marshal(Project{
		Info: Info{Maintainer: "dev"},
		Releases: map[string][]Distribution{
			"1.0.0": {{UploadTimeISO: "2024-01-01T00:00:00Z", PackageType: "bdist_wheel", Filename: "pkg-1.0.0-py3-none-any.whl"}},
			"2.0.0": {{UploadTimeISO: "2024-06-01T00:00:00Z", PackageType: "bdist_wheel", Filename: "pkg-2.0.0-py3-none-any.whl"}},
			"3.0.0": {{UploadTimeISO: "2025-01-01T00:00:00Z", PackageType: "bdist_wheel", Filename: "pkg-3.0.0-py3-none-any.whl"}},
			"4.0.0": {{UploadTimeISO: "2025-06-01T00:00:00Z", PackageType: "bdist_wheel", Filename: "pkg-4.0.0-py3-none-any.whl"}},
			"5.0.0": {{UploadTimeISO: "2026-01-01T00:00:00Z", PackageType: "bdist_wheel", Filename: "pkg-5.0.0-py3-none-any.whl"}},
		},
	})
	require.NoError(t, err)

	attestBody, err := json.Marshal(ghAttest)
	require.NoError(t, err)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if after, ok := strings.CutPrefix(r.URL.Path, "/integrity/"); ok {
			parts := strings.Split(after, "/")
			if len(parts) >= 2 {
				// Only latest (5.0.0) and first prior (4.0.0) succeed.
				if parts[1] == "5.0.0" || parts[1] == "4.0.0" {
					_, _ = w.Write(attestBody)
					return
				}
			}
			// All other versions: 500 error.
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(projBody)
	}))
	defer srv.Close()

	raw, err := newTestCollector(srv).Collect(t.Context(), pypiEntity("pkg"))
	require.NoError(t, err)
	result := wrap(t, raw)

	require.True(t, hasSignal(result, "attestation_consistency"),
		"signal should emit despite partial failures during full sweep")
	val := getSignalValue(t, result, "attestation_consistency")

	// Only 2 versions were successfully checked (5.0.0 + 4.0.0).
	assert.Equal(t, float64(2), val["versions_checked"])
	// 3 versions were attempted but failed — this should be visible.
	assert.Equal(t, float64(3), val["versions_skipped"],
		"versions_skipped must report how many versions were lost to errors")
	assert.Equal(t, true, val["consistent"],
		"the 2 successfully checked versions are both attested")
}

func TestCollector_AttestationConsistency_PhaseAError_SkipsPhaseB(t *testing.T) {
	t.Parallel()
	// When Phase A errors (Integrity API 500 for the latest version),
	// recordTrustedPublishing returns nil. Phase B must not misinterpret
	// this as "latest is unattested" — it has no basis for comparison.
	//
	// If Phase B ran anyway and the probe found a prior version attested,
	// it would falsely report attested_to_unattested (a false alarm).
	projBody, err := json.Marshal(Project{
		Info: Info{Maintainer: "dev"},
		Releases: map[string][]Distribution{
			"1.0.0": {{UploadTimeISO: "2025-01-01T00:00:00Z", PackageType: "bdist_wheel", Filename: "pkg-1.0.0-py3-none-any.whl"}},
			"2.0.0": {{UploadTimeISO: "2026-01-01T00:00:00Z", PackageType: "bdist_wheel", Filename: "pkg-2.0.0-py3-none-any.whl"}},
		},
	})
	require.NoError(t, err)

	attestBody, err := json.Marshal(makeAttestation("GitHub", "owner/pkg", "ci.yml"))
	require.NoError(t, err)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if after, ok := strings.CutPrefix(r.URL.Path, "/integrity/"); ok {
			parts := strings.Split(after, "/")
			if len(parts) >= 2 {
				if parts[1] == "2.0.0" {
					// Latest: 500 error (Phase A fails).
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				// Prior versions: attested (would trigger false alarm if probed).
				_, _ = w.Write(attestBody)
				return
			}
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(projBody)
	}))
	defer srv.Close()

	raw, err := newTestCollector(srv).Collect(t.Context(), pypiEntity("pkg"))
	require.NoError(t, err)
	result := wrap(t, raw)

	// Phase A should have recorded an absence for trusted_publishing.
	assert.True(t, hasAbsence(result, "trusted_publishing"),
		"Phase A error must record trusted_publishing absence")

	// Phase B must NOT emit a signal — it has no reliable latest state.
	assert.False(t, hasSignal(result, "attestation_consistency"),
		"Phase B must not emit when Phase A errored (nil means unknown, not absent)")
}

// ----- artifact_url (handoff to artifact-vs-repo collector) -----

// TestCollector_Collect_ArtifactURL_EmitsForLatestSdist is the
// smallest claim of the pypi→artifact-vs-repo wiring: the registry
// collector must emit an artifact_url signal carrying the latest
// version's sdist URL, version, integrity (sha256 from digests),
// and an empty git_head.
//
// Empty git_head mirrors cargo: PyPI exposes no publisher-stamped
// commit SHA in registry metadata. The downstream artifact collector
// will fall through to tag-match resolution against the local clone.
//
// Sdist (not wheel) is the right comparison surface because wheels
// are build outputs — the xz-shaped check is "what does the
// publication channel ship that the source-of-record git tree doesn't?"
// For Python that's the sdist.
func TestCollector_Collect_ArtifactURL_EmitsForLatestSdist(t *testing.T) {
	t.Parallel()
	srv := projectServer(t, Project{
		Info: Info{Maintainer: "ofek"},
		Releases: map[string][]Distribution{
			"1.2.3": {
				{
					UploadTimeISO: "2026-04-01T00:00:00Z",
					PackageType:   "sdist",
					Filename:      "hatch-1.2.3.tar.gz",
					URL:           "https://files.pythonhosted.org/packages/aa/bb/hatch-1.2.3.tar.gz",
					Digests:       Digests{SHA256: "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"},
				},
			},
		},
	})
	defer srv.Close()

	raw, err := newTestCollector(srv).Collect(t.Context(), pypiEntity("hatch"))
	require.NoError(t, err)
	result := wrap(t, raw)

	require.True(t, hasSignal(result, "artifact_url"),
		"pypi registry collector must emit artifact_url so the artifact-vs-repo "+
			"collector can fetch and pair the sdist tarball")

	au := getSignalValue(t, result, "artifact_url")
	assert.Equal(t, "https://files.pythonhosted.org/packages/aa/bb/hatch-1.2.3.tar.gz", au["url"],
		"url is the sdist's files.pythonhosted.org URL straight from the registry response")
	assert.Equal(t, "1.2.3", au["version"],
		"version is the latest non-yanked publish; downstream tag-match resolver pairs by this")
	assert.Equal(t, "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789", au["integrity"],
		"integrity is the sdist's digests.sha256 — opaque to current consumers but kept on "+
			"the wire for cross-checking against the hash signatory computes during fetch")
	assert.Equal(t, "", au["git_head"],
		"PyPI does not expose git_head in registry metadata; the downstream artifact "+
			"collector falls through to tag-match resolution")
}
