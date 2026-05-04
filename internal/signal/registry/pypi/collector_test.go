package pypi

import (
	"context"
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

	raw, err := newTestCollector(srv).Collect(context.Background(), pypiEntity("hatch"))
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

	raw, err := newTestCollector(srv).Collect(context.Background(), pypiEntity("python-dotenv"))
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
		raw, err := newTestCollector(srv).Collect(context.Background(), e)
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

	raw, err := newTestCollector(srv).Collect(context.Background(), pypiEntity("nonexistent"))
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

	raw, err := newTestCollector(srv).Collect(context.Background(), pypiEntity("hatch"))
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

	raw, err := newTestCollector(srv).Collect(context.Background(), pypiEntity("hatch"))
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

	raw, err := newTestCollector(srv).Collect(context.Background(), pypiEntity("hatch"))
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

	raw, err := newTestCollector(srv).Collect(context.Background(), pypiEntity("hatch"))
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

	raw, err := newTestCollector(srv).Collect(context.Background(), pypiEntity("spam-pkg"))
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

	raw, err := newTestCollector(srv).Collect(context.Background(), pypiEntity("stable-pkg"))
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

	raw, err := newTestCollector(srv).Collect(context.Background(), pypiEntity("empty-pkg"))
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

	raw, err := newTestCollector(srv).Collect(context.Background(), pypiEntity("some-pkg"))
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

	raw, err := newTestCollector(srv).Collect(context.Background(), pypiEntity("clean-pkg"))
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

	raw, err := newTestCollector(srv).Collect(context.Background(), pypiEntity("native-pkg"))
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

	raw, err := newTestCollector(srv).Collect(context.Background(), pypiEntity("wheeled-pkg"))
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

	raw, err := newTestCollector(srv).Collect(context.Background(), pypiEntity("compromised"))
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

	raw, err := newTestCollector(srv).Collect(context.Background(), pypiEntity("safe-pkg"))
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

	raw, err := newTestCollector(srv).Collect(context.Background(), pypiEntity("always-sdist"))
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

	raw, err := newTestCollector(srv).Collect(context.Background(), pypiEntity("old-signed"))
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

	raw, err := newTestCollector(srv).Collect(context.Background(), pypiEntity("no-sig"))
	require.NoError(t, err)
	result := wrap(t, raw)

	require.True(t, hasSignal(result, "gpg_signature_present"))
	gs := getSignalValue(t, result, "gpg_signature_present")
	assert.Equal(t, false, gs["present"])
	assert.Equal(t, "2.0.0", gs["version_checked"])
}

// TestCollector_Name pins the collector identifier for orchestrator
// dispatch and progress narration. Mirrors the npm collector's name
// pattern so log lines read consistently.
func TestCollector_Name(t *testing.T) {
	t.Parallel()
	c := NewCollector()
	assert.Equal(t, "pypi-registry", c.Name())
}
