package adoption

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal"
)

// Compile-time interface check — pins the collector to the
// signal.Collector contract so collectorsFor can dispatch without
// per-collector type knowledge.
var _ signal.Collector = (*Collector)(nil)

// newTestCollector wires a Collector to an httptest.Server that
// pretends to be api.github.com's code-search endpoint. The adoption
// collector hits GitHub regardless of which forge owns the analyzed
// module — code search exists only on GitHub, so the API backend is
// github even for codeberg/gitlab module paths. The test discipline
// (deterministic stub, no network) matches the per-forge collector
// test setups in internal/signal/{github,forgejo,gitlab}.
func newTestCollector(t *testing.T, handler http.Handler) *Collector {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	client := &Client{
		httpClient: server.Client(),
		baseURL:    server.URL,
	}
	return NewCollectorWithClient(client)
}

// stubSearch returns a handler that asserts the request is the
// expected code-search shape and replies with the given total_count.
// Captures the queried module path so per-forge tests can assert it
// matches the expected <host>/<owner>/<repo> form.
func stubSearch(t *testing.T, wantModulePath string, totalCount int) (http.Handler, *string) {
	t.Helper()
	var sawQ string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search/code" {
			t.Errorf("expected path /search/code, got %q", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		sawQ = r.URL.Query().Get("q")
		// Expected query shape: <modulePath>+filename:go.mod
		// where "+" is GitHub's search-grammar AND operator (literal +
		// in the query string, not URL encoding for space). net/url
		// decodes the "+" back to a space when populating Query(),
		// so the captured sawQ reads like "<modulePath> filename:go.mod"
		// with a single space. Assert on the two substrings that
		// survive decoding rather than trying to round-trip the raw
		// query — far less brittle.
		if !strings.Contains(sawQ, wantModulePath) || !strings.Contains(sawQ, "filename:go.mod") {
			t.Errorf("q=%q must contain module path %q and 'filename:go.mod'",
				sawQ, wantModulePath)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": totalCount})
	})
	return handler, &sawQ
}

// TestCollector_GitHubModule_EmitsAdoption pins the original github
// behavior: a github-hosted Go module produces an adoption signal
// with go_mod_refs from the search count, and the canonical fields
// (stars / refs_to_stars / adoption_type) populated when stars is
// available from inRunResult. Same signal shape the github
// collector previously emitted before the lift-out; this test pins
// the regression-resistance of "github still works."
func TestCollector_GitHubModule_EmitsAdoption(t *testing.T) {
	t.Parallel()
	handler, _ := stubSearch(t, "github.com/alecthomas/kong", 2008)
	c := newTestCollector(t, handler)

	// Pre-seed an in-run result with the entity's stars (as a forge
	// metadata collector would have emitted before adoption runs).
	inRun := &signal.CollectionResult{}
	inRun.RecordSignal("e1", "stars", "github", time.Now().UTC(), 24*time.Hour,
		map[string]any{"count": 3023})
	c.WithInRun(inRun)

	entity := &profile.Entity{
		ID:        "e1",
		Type:      profile.EntityProject,
		Ecosystem: "golang",
		ShortName: "alecthomas/kong",
		URL:       "https://github.com/alecthomas/kong",
	}

	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)

	signals := result.Signals()
	require.Len(t, signals, 1, "exactly one adoption signal expected")
	sig := signals[0]
	assert.Equal(t, "adoption", sig.Type)
	assert.Equal(t, "adoption", sig.Source,
		"new collector emits source=adoption, replacing the previous source=github stamp (collector-identity convention; documented behavior change)")

	var v map[string]any
	require.NoError(t, json.Unmarshal(sig.Value, &v))
	assert.Equal(t, float64(2008), v["go_mod_refs"],
		"go_mod_refs from the github code-search total_count must round-trip into the signal value")
	assert.Equal(t, float64(3023), v["stars"],
		"stars from inRunResult must reach the signal value so the ratio can be reconstructed")
	assert.InDelta(t, 0.66, v["refs_to_stars"], 0.01)
	assert.Equal(t, "direct", v["adoption_type"])
}

// TestCollector_CodebergModule_EmitsAdoption pins the Tier 2-lift
// payoff: a codeberg-hosted Go module produces an adoption signal
// using GitHub's code search to count inbound go.mod refs to the
// codeberg.org/<owner>/<repo> path. The module path is the only
// forge-agnostic piece; the search backend stays github (only forge
// with a public code-search index).
func TestCollector_CodebergModule_EmitsAdoption(t *testing.T) {
	t.Parallel()
	handler, _ := stubSearch(t, "codeberg.org/owner/cleave", 42)
	c := newTestCollector(t, handler)

	inRun := &signal.CollectionResult{}
	inRun.RecordSignal("e1", "stars", "forgejo", time.Now().UTC(), 24*time.Hour,
		map[string]any{"count": 100})
	c.WithInRun(inRun)

	entity := &profile.Entity{
		ID:        "e1",
		Type:      profile.EntityProject,
		Ecosystem: "golang",
		ShortName: "owner/cleave",
		URL:       "https://codeberg.org/owner/cleave",
	}

	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)
	require.Len(t, result.Signals(), 1, "codeberg Go module must emit one adoption signal")

	var v map[string]any
	require.NoError(t, json.Unmarshal(result.Signals()[0].Value, &v))
	assert.Equal(t, float64(42), v["go_mod_refs"],
		"codeberg module path must reach github code-search as the q= module-path token")
	assert.Equal(t, float64(100), v["stars"],
		"stars from the forgejo collector's emission (in inRunResult) must populate the ratio")
}

// TestCollector_GitLabModule_EmitsAdoption pins the same shape for
// gitlab.com modules. Mirror of the codeberg case — different host,
// same search-API path.
func TestCollector_GitLabModule_EmitsAdoption(t *testing.T) {
	t.Parallel()
	handler, _ := stubSearch(t, "gitlab.com/gitlab-org/gitlab", 7)
	c := newTestCollector(t, handler)

	entity := &profile.Entity{
		ID:        "e1",
		Type:      profile.EntityProject,
		Ecosystem: "go",
		ShortName: "gitlab-org/gitlab",
		URL:       "https://gitlab.com/gitlab-org/gitlab",
	}

	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)
	require.Len(t, result.Signals(), 1)

	var v map[string]any
	require.NoError(t, json.Unmarshal(result.Signals()[0].Value, &v))
	assert.Equal(t, float64(7), v["go_mod_refs"])
}

// TestCollector_GitLabNestedNamespace_IncludesFullPath pins that
// gitlab's nested-group module paths (gitlab.com/group/subgroup/proj)
// are passed verbatim into the search query, with no truncation. A
// future "only take first two path segments" simplification would
// silently misclassify nested-group modules' adoption.
func TestCollector_GitLabNestedNamespace_IncludesFullPath(t *testing.T) {
	t.Parallel()
	handler, _ := stubSearch(t, "gitlab.com/gitlab-org/security/foo", 3)
	c := newTestCollector(t, handler)

	entity := &profile.Entity{
		ID:        "e1",
		Type:      profile.EntityProject,
		Ecosystem: "golang",
		ShortName: "gitlab-org/security/foo",
		URL:       "https://gitlab.com/gitlab-org/security/foo",
	}

	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)
	require.Len(t, result.Signals(), 1, "nested-namespace gitlab module must still emit adoption")

	var v map[string]any
	require.NoError(t, json.Unmarshal(result.Signals()[0].Value, &v))
	assert.Equal(t, float64(3), v["go_mod_refs"])
}

// TestCollector_NoStarsInRun_StillEmits pins graceful degradation:
// when inRunResult doesn't carry a stars signal (e.g. the forge
// metadata collector failed, or the dispatcher reordered), adoption
// still emits with go_mod_refs and stars=0. ratio computes to 0
// because dividing by zero is gated. Without this branch, a
// missing-stars run would either panic or drop adoption entirely —
// both worse than emitting "we couldn't compute the ratio."
func TestCollector_NoStarsInRun_StillEmits(t *testing.T) {
	t.Parallel()
	handler, _ := stubSearch(t, "github.com/alecthomas/kong", 100)
	c := newTestCollector(t, handler) // no WithInRun call

	entity := &profile.Entity{
		ID:        "e1",
		Type:      profile.EntityProject,
		Ecosystem: "golang",
		ShortName: "alecthomas/kong",
		URL:       "https://github.com/alecthomas/kong",
	}

	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)
	require.Len(t, result.Signals(), 1)

	var v map[string]any
	require.NoError(t, json.Unmarshal(result.Signals()[0].Value, &v))
	assert.Equal(t, float64(100), v["go_mod_refs"])
	assert.Equal(t, float64(0), v["stars"], "missing stars → zero, not panic")
	assert.Equal(t, float64(0), v["refs_to_stars"], "ratio must be 0 when stars is 0 (no divide-by-zero)")
}

// TestCollector_NonGoEcosystem_ReturnsEmpty pins the ecosystem gate.
// adoption is a Go-specific signal (filename:go.mod query); emitting
// it for npm/pypi/cargo entities would always return 0 and pollute
// the signal set with meaningless zeros. The new collector skips
// non-Go ecosystems entirely.
//
// Behavior change from the pre-lift github collector, which called
// collectAdoption unconditionally and emitted an always-zero
// adoption signal for non-Go github repos. The github tests's
// expected-types list relies on this old behavior and must be
// updated alongside this commit; this test fails RED until the
// gate exists.
func TestCollector_NonGoEcosystem_ReturnsEmpty(t *testing.T) {
	t.Parallel()
	for _, eco := range []string{"npm", "pypi", "cargo", "gem", "maven"} {
		t.Run(eco, func(t *testing.T) {
			t.Parallel()
			handler := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				t.Fatalf("non-Go ecosystem must short-circuit before any API call (saw %s)", r.URL.Path)
			})
			c := newTestCollector(t, handler)
			entity := &profile.Entity{
				ID:        "e1",
				Type:      profile.EntityPackage,
				Ecosystem: eco,
				ShortName: "any/thing",
				URL:       "https://github.com/owner/repo",
			}
			result, err := c.Collect(context.Background(), entity)
			require.NoError(t, err)
			assert.Empty(t, result.Signals(),
				"non-Go ecosystem %q must produce zero adoption signals (the filename:go.mod query is always 0 for non-Go projects; emitting a zero is noise)", eco)
		})
	}
}

// TestCollector_NonForgeURL_ReturnsEmpty pins the URL gate. Entities
// whose URL host isn't one of the recognized forges (github,
// codeberg, gitlab) have no derivable Go module path that pkg.go.dev
// indexes consistently, so adoption can't be computed.
func TestCollector_NonForgeURL_ReturnsEmpty(t *testing.T) {
	t.Parallel()
	cases := []string{
		"https://bitbucket.org/team/repo",
		"https://self-hosted.example.com/owner/repo",
		"",
	}
	for _, u := range cases {
		t.Run(u, func(t *testing.T) {
			t.Parallel()
			handler := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
				t.Fatalf("non-forge URL must short-circuit before any API call")
			})
			c := newTestCollector(t, handler)
			entity := &profile.Entity{
				ID:        "e1",
				Type:      profile.EntityProject,
				Ecosystem: "golang",
				ShortName: "ignored",
				URL:       u,
			}
			result, err := c.Collect(context.Background(), entity)
			require.NoError(t, err)
			assert.Empty(t, result.Signals())
		})
	}
}

// TestCollector_SearchAPIError_RecordsFailure pins the API-error
// handling: a 5xx (or other transport error) from github's search
// API surfaces as a RecordFailure in the result, NOT a Collect
// error. Matches the original github collectAdoption's failure
// shape — the search call's own rate limit makes intermittent
// failures expected, and a hard error would prevent later collectors
// from running.
func TestCollector_SearchAPIError_RecordsFailure(t *testing.T) {
	t.Parallel()
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	c := newTestCollector(t, handler)

	entity := &profile.Entity{
		ID:        "e1",
		Type:      profile.EntityProject,
		Ecosystem: "golang",
		ShortName: "alecthomas/kong",
		URL:       "https://github.com/alecthomas/kong",
	}

	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err, "search-API failure must NOT bubble out — record as a per-signal failure instead")

	assert.NotEmpty(t, result.Failures, "API failure must be recorded for run summary")
	assert.Equal(t, "adoption", result.Failures[0].SignalType)
}

// TestCollector_Name pins the collector identifier the orchestrator
// keys on. Changing this would silently mislabel the per-collector
// "[adoption] Collected N signals" line in stderr narration.
func TestCollector_Name(t *testing.T) {
	t.Parallel()
	c := NewCollector()
	assert.Equal(t, "adoption", c.Name())
}
