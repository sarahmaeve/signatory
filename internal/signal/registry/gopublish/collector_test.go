package gopublish

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

// goEntity builds a minimal Entity that mirrors what the analyze
// pipeline produces for a Go module target — pkg:golang/<path> URI
// with Ecosystem="golang". Tests that exercise the older "go"
// label use goEntityWithEcosystem for explicit override.
func goEntity(modulePath string) *profile.Entity {
	return &profile.Entity{
		ID:           "e-" + modulePath,
		CanonicalURI: "pkg:golang/" + modulePath,
		Type:         profile.EntityPackage,
		Ecosystem:    "golang",
		ShortName:    modulePath,
	}
}

// fakeProxyAndSum stands up a single httptest server that
// multiplexes the proxy and sum endpoints by URL path prefix. The
// fixtures exercise the four endpoints the collector reads:
// @latest, @v/list, @v/<v>.info, and /lookup/<module>@<v>.
func fakeProxyAndSum(t *testing.T, modulePath string, fx fixtures) *httptest.Server {
	t.Helper()
	encoded := encodeModulePath(modulePath)

	mux := http.NewServeMux()
	mux.HandleFunc("/"+encoded+"/@latest", func(w http.ResponseWriter, r *http.Request) {
		if fx.latestStatus == 0 {
			fx.latestStatus = http.StatusOK
		}
		w.WriteHeader(fx.latestStatus)
		if fx.latestStatus == http.StatusOK {
			fmt.Fprint(w, fx.latestBody)
		}
	})
	mux.HandleFunc("/"+encoded+"/@v/list", func(w http.ResponseWriter, r *http.Request) {
		if fx.listStatus == 0 {
			fx.listStatus = http.StatusOK
		}
		w.WriteHeader(fx.listStatus)
		if fx.listStatus == http.StatusOK {
			fmt.Fprint(w, fx.listBody)
		}
	})
	mux.HandleFunc("/lookup/"+encoded+"@"+fx.lookupVersion, func(w http.ResponseWriter, r *http.Request) {
		if fx.lookupStatus == 0 {
			fx.lookupStatus = http.StatusOK
		}
		w.WriteHeader(fx.lookupStatus)
		if fx.lookupStatus == http.StatusOK {
			fmt.Fprint(w, fx.lookupBody)
		}
	})
	// Per-version .info handler for the legacy single-version
	// fixture: matches @v/<lookupVersion>.info as a fixed path so
	// it takes precedence over the broader prefix handler below.
	mux.HandleFunc("/"+encoded+"/@v/"+fx.lookupVersion+".info", func(w http.ResponseWriter, r *http.Request) {
		if fx.infoStatus == 0 {
			fx.infoStatus = http.StatusOK
		}
		w.WriteHeader(fx.infoStatus)
		if fx.infoStatus == http.StatusOK {
			fmt.Fprint(w, fx.infoBody)
		}
	})
	// Per-version .info handler for multi-version fixtures used by
	// pin-table tests. The prefix `/<encoded>/@v/` catches any
	// version-suffixed request that the more-specific list and
	// lookupVersion-info handlers above don't match. Requests for
	// versions in fx.versionInfos are answered from that map; any
	// other request 404s.
	mux.HandleFunc("/"+encoded+"/@v/", func(w http.ResponseWriter, r *http.Request) {
		suffix := strings.TrimPrefix(r.URL.Path, "/"+encoded+"/@v/")
		if !strings.HasSuffix(suffix, ".info") {
			http.NotFound(w, r)
			return
		}
		version := strings.TrimSuffix(suffix, ".info")
		body, ok := fx.versionInfos[version]
		if !ok {
			http.NotFound(w, r)
			return
		}
		status := http.StatusOK
		if s, override := fx.versionInfoStatuses[version]; override {
			status = s
		}
		w.WriteHeader(status)
		if status == http.StatusOK {
			fmt.Fprint(w, body)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// fixtures bundles the endpoint response fixtures the collector
// tests vary. Default zeros mean "200 with empty body"; each test
// overrides the fields it cares about.
//
// versionInfos / versionInfoStatuses drive the multi-version
// .info responses used by pin-table tests. Map key is the version
// string (e.g., "v0.20.0"); value is the JSON body. Statuses
// default to 200 and only override when explicitly set.
type fixtures struct {
	latestBody          string
	latestStatus        int
	listBody            string
	listStatus          int
	infoBody            string
	infoStatus          int
	lookupBody          string
	lookupStatus        int
	lookupVersion       string // version inserted into the legacy @v/<v>.info and /lookup paths
	versionInfos        map[string]string
	versionInfoStatuses map[string]int
}

// happyPathFixtures returns a fixtures bundle modelling a
// well-published, well-attested Go module — proxy answers with a
// recent latest version, the version list has multiple entries,
// every .info block carries an Origin section, and sum.golang.org
// has a transparency-log entry.
//
// versionInfos is populated for every version in listBody so the
// version_pin_table emission lands a full pin set on the happy
// path. Tests that want a specific version to fail / lack Origin
// override the relevant map entry.
func happyPathFixtures() fixtures {
	v20Body := `{
			"Version": "v0.20.0",
			"Time": "2026-04-15T10:00:00Z",
			"Origin": {"VCS":"git","URL":"https://go.googlesource.com/sync","Ref":"refs/tags/v0.20.0","Hash":"ec11c4a93de22cde2abe2bf74d70791033c2464c"}
		}`
	return fixtures{
		latestBody:    `{"Version":"v0.20.0","Time":"2026-04-15T10:00:00Z"}`,
		listBody:      "v0.16.0\nv0.17.0\nv0.18.0\nv0.19.0\nv0.20.0\n",
		infoBody:      v20Body,
		lookupBody:    "12345\ngolang.org/x/sync v0.20.0 h1:fakebase64\ngolang.org/x/sync v0.20.0/go.mod h1:fakebase64\n\n— sum.golang.org Az3grx...\n",
		lookupVersion: "v0.20.0",
		versionInfos: map[string]string{
			"v0.16.0": versionInfoBody("v0.16.0", "1616161616161616161616161616161616161616", "2026-03-01T00:00:00Z"),
			"v0.17.0": versionInfoBody("v0.17.0", "1717171717171717171717171717171717171717", "2026-03-15T00:00:00Z"),
			"v0.18.0": versionInfoBody("v0.18.0", "1818181818181818181818181818181818181818", "2026-04-01T00:00:00Z"),
			"v0.19.0": versionInfoBody("v0.19.0", "1919191919191919191919191919191919191919", "2026-04-08T00:00:00Z"),
			"v0.20.0": v20Body,
		},
	}
}

// noJitterCollector returns a Collector wired to the test server
// with the pin-fetch jitter disabled, so iterating up to 12 versions
// adds at most a few milliseconds rather than 800-3200ms × 12 to
// test runtime. Same-package field access is the established test
// seam pattern; production code never sets these to zero.
func noJitterCollector(srvURL string) *Collector {
	c := NewCollectorWithClient(NewClientWithBaseURL(srvURL, srvURL))
	c.jitterMin = 0
	c.jitterMax = 0
	return c
}

// hasSignal reports whether the result recorded a non-absence
// signal of the given type. Mirrors the npm collector test
// helpers — kept local so this package is self-contained.
func hasSignal(result anySignals, signalType string) bool {
	for _, s := range result.Signals() {
		if !strings.HasPrefix(s.Type, "absence:") && s.Type == signalType {
			return true
		}
	}
	return false
}

func hasAbsence(result anySignals, signalType string) bool {
	for _, s := range result.Signals() {
		if s.Type == "absence:"+signalType {
			return true
		}
	}
	return false
}

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

type anySignals interface {
	Signals() []profile.Signal
}

// ----- happy path -----

// TestCollector_HappyPath: a Go module with all five data points
// available emits the full signal set — last_publish, version_count,
// transparency_log_present, publish_origin, version_pin_table —
// with zero absences.
func TestCollector_HappyPath(t *testing.T) {
	t.Parallel()
	fx := happyPathFixtures()
	srv := fakeProxyAndSum(t, "golang.org/x/sync", fx)

	c := noJitterCollector(srv.URL)
	result, err := c.Collect(context.Background(), goEntity("golang.org/x/sync"))
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.Equal(t, 5, result.SignalCount(), "all five signals should land on happy path")
	assert.Equal(t, 0, result.AbsenceCount())

	require.True(t, hasSignal(result, "last_publish"))
	lp := getSignalValue(t, result, "last_publish")
	assert.Equal(t, "v0.20.0", lp["latest_version"])
	assert.Equal(t, "2026-04-15T10:00:00Z", lp["published_at"])

	require.True(t, hasSignal(result, "version_count"))
	vc := getSignalValue(t, result, "version_count")
	assert.Equal(t, float64(5), vc["count"])

	require.True(t, hasSignal(result, "transparency_log_present"))
	tl := getSignalValue(t, result, "transparency_log_present")
	assert.Equal(t, true, tl["present"])
	assert.Equal(t, float64(12345), tl["leaf_id"])

	require.True(t, hasSignal(result, "publish_origin"))
	po := getSignalValue(t, result, "publish_origin")
	assert.Equal(t, "git", po["vcs"])
	assert.Equal(t, "https://go.googlesource.com/sync", po["url"])
	assert.Equal(t, "refs/tags/v0.20.0", po["ref"])
	assert.Equal(t, "ec11c4a93de22cde2abe2bf74d70791033c2464c", po["hash"])
}

// TestCollector_NonGoEntity: a non-pkg:golang entity yields an
// empty CollectionResult with no error. Lets the orchestrator
// dispatch this collector unconditionally.
func TestCollector_NonGoEntity(t *testing.T) {
	t.Parallel()
	c := NewCollectorWithClient(NewClient())
	npmEnt := &profile.Entity{
		ID:           "e-express",
		CanonicalURI: "pkg:npm/express",
		Type:         profile.EntityPackage,
		Ecosystem:    "npm",
	}
	result, err := c.Collect(context.Background(), npmEnt)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, 0, result.SignalCount())
	assert.Equal(t, 0, result.AbsenceCount())
}

// TestCollector_PkgGoLegacyURI: an entity whose URI uses the
// older `pkg:go/` form (pre-purl-canonicalization) is still
// matched. v0.1's resolver registry registers under both names;
// the collector matches both for parity.
func TestCollector_PkgGoLegacyURI(t *testing.T) {
	t.Parallel()
	fx := happyPathFixtures()
	srv := fakeProxyAndSum(t, "golang.org/x/sync", fx)

	legacy := &profile.Entity{
		ID:           "e-legacy",
		CanonicalURI: "pkg:go/golang.org/x/sync",
		Type:         profile.EntityPackage,
		Ecosystem:    "go",
		ShortName:    "golang.org/x/sync",
	}
	c := noJitterCollector(srv.URL)
	result, err := c.Collect(context.Background(), legacy)
	require.NoError(t, err)
	assert.Equal(t, 5, result.SignalCount(), "legacy pkg:go URI should still resolve")
}

// TestCollector_LatestNotFound: @latest 404 turns into a fetch
// failure. Without a latest version, the per-version signals
// (transparency log, publish origin) cannot fire — they're
// recorded as absences with a useful reason.
func TestCollector_LatestNotFound(t *testing.T) {
	t.Parallel()
	fx := happyPathFixtures()
	fx.latestStatus = http.StatusNotFound
	srv := fakeProxyAndSum(t, "golang.org/x/sync", fx)

	c := noJitterCollector(srv.URL)
	result, err := c.Collect(context.Background(), goEntity("golang.org/x/sync"))
	require.NoError(t, err)
	require.NotNil(t, result)

	// last_publish is the failure that's recorded; downstream
	// per-version signals can't run without a known version.
	assert.True(t, hasAbsence(result, "last_publish"))
	assert.True(t, hasAbsence(result, "transparency_log_present"))
	assert.True(t, hasAbsence(result, "publish_origin"))
}

// TestCollector_TransparencyAbsent: the proxy has the module but
// sum.golang.org doesn't. transparency_log_present lands as a
// signal with present=false (NOT an absence — absence means "we
// couldn't tell"; we DID tell, the answer is "no record"). The
// shape distinction matters: an honest investigation can act on
// "no entry" with confidence; an absence record means we have to
// retry or escalate first.
func TestCollector_TransparencyAbsent(t *testing.T) {
	t.Parallel()
	fx := happyPathFixtures()
	fx.lookupStatus = http.StatusNotFound
	srv := fakeProxyAndSum(t, "golang.org/x/sync", fx)

	c := noJitterCollector(srv.URL)
	result, err := c.Collect(context.Background(), goEntity("golang.org/x/sync"))
	require.NoError(t, err)

	require.True(t, hasSignal(result, "transparency_log_present"))
	tl := getSignalValue(t, result, "transparency_log_present")
	assert.Equal(t, false, tl["present"])
	// leaf_id field is present-but-zero so the schema stays stable.
	assert.Equal(t, float64(0), tl["leaf_id"])
}

// TestCollector_OriginAbsent: the .info block lacks the Origin
// section (pre-go-1.20 publishes still happen). publish_origin
// lands as an absence with a clear reason — this is "we tried
// and the proxy didn't tell us."
func TestCollector_OriginAbsent(t *testing.T) {
	t.Parallel()
	fx := happyPathFixtures()
	fx.infoBody = `{"Version":"v0.20.0","Time":"2026-04-15T10:00:00Z"}`
	srv := fakeProxyAndSum(t, "golang.org/x/sync", fx)

	c := noJitterCollector(srv.URL)
	result, err := c.Collect(context.Background(), goEntity("golang.org/x/sync"))
	require.NoError(t, err)
	assert.True(t, hasAbsence(result, "publish_origin"))
}
