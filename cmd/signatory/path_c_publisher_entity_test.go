package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal"
	npmcollector "github.com/sarahmaeve/signatory/internal/signal/registry/npm"
	"github.com/sarahmaeve/signatory/internal/store"
)

// Path C end-to-end: a `signatory analyze --refresh pkg:npm/X` run
// against an npm-ecosystem target must mint identity:npm/<login>
// rows for each maintainer + per-version publisher seen in the
// registry response. Mirrors the Path A end-to-end pattern (github
// owner entities) — exercises AnalyzeCmd.Run + the npm collector +
// the store's EnsureEntityByCanonicalURI, with httptest standing in
// for the real npm registry.
//
// Tests inject the npm collector via globals.Collectors so the
// httptest server's URL is reachable. The collectorsFor wiring
// (CollectOpts.EntityStore → npmcollector.WithEntityStore) is
// covered by compile-time checks plus the npm package's unit tests
// (entity_publisher_test.go); these tests pin the higher-level
// invariant that an analyze run produces the publisher-entity side
// effect.

// pathCRegistryBody is the minimal npm registry payload Path C
// needs to exercise both branches: a Maintainers list AND
// per-version publishers, with one login (jdalton) appearing in
// both to verify idempotency on overlap.
const pathCRegistryBody = `{
  "name": "lodash-test",
  "dist-tags": {"latest": "4.17.21"},
  "time": {
    "created": "2010-01-01T00:00:00Z",
    "4.17.21": "2021-02-20T00:00:00Z",
    "4.17.20": "2020-08-13T00:00:00Z"
  },
  "maintainers": [
    {"name": "jdalton", "email": "j@example.com"}
  ],
  "versions": {
    "4.17.21": {"scripts": {}, "dist": {"attestations": null}, "_npmUser": {"name": "jdalton"}},
    "4.17.20": {"scripts": {}, "dist": {"attestations": null}, "_npmUser": {"name": "bnjmnt4n"}}
  }
}`

// pathCDownloadsBody satisfies the weekly-downloads endpoint so the
// collector's per-target HTTP fetch doesn't fail and abort the run.
const pathCDownloadsBody = `{"downloads":50000000,"start":"2026-04-13","end":"2026-04-20","package":"lodash-test"}`

// pathCNpmServer returns an httptest server that multiplexes the
// registry endpoint (/<name>) and the downloads endpoint
// (/downloads/...) on a single host. Mirrors the npm package's
// newMultiEndpointServer test helper but accessible from cmd/.
func pathCNpmServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/downloads/"):
			_, _ = w.Write([]byte(pathCDownloadsBody))
		default:
			_, _ = w.Write([]byte(pathCRegistryBody))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// runAnalyzeWithNpmRegistry sets up a fresh test DB, builds an npm
// collector pointed at the supplied httptest server with
// WithEntityStore wired to the same DB, runs AnalyzeCmd, and
// returns the DB path so the caller can inspect post-run state.
// Mirrors path_a_owner_entity_test.go's runAnalyzeWithGitHubAPI.
func runAnalyzeWithNpmRegistry(t *testing.T, srv *httptest.Server, target string) string {
	t.Helper()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	// Pre-create the DB by opening + closing once so the schema
	// migrations run before the collector tries to attach.
	{
		s, err := store.OpenSQLite(t.Context(), dbPath)
		require.NoError(t, err)
		require.NoError(t, s.Close())
	}

	s, err := store.OpenSQLite(t.Context(), dbPath)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck

	npmClient := npmcollector.NewClientWithBaseURL(srv.URL)
	npmColl := npmcollector.NewCollectorWithClient(npmClient).WithEntityStore(s)

	globals := &Globals{
		DBPath:        dbPath,
		Collectors:    []signal.Collector{npmColl},
		AuditFilePath: filepath.Join(dir, "audit.log"),
		Context:       context.Background(),
		// Redirect the analyze flow's separate resolveNpmRepo step
		// at the orchestrator (analyze.go:386) to the same httptest
		// server. Without this it hits the real npm registry, which
		// will 404 or return a shape that fails to decode for the
		// test-only package name.
		NpmRegistryURL: srv.URL,
	}

	cmd := &AnalyzeCmd{Target: target, Refresh: true}
	require.NoError(t, cmd.Run(globals),
		"analyze --refresh against the mocked npm registry must succeed")

	return dbPath
}

// TestPathC_AnalyzeRefresh_MintsMaintainerAndPublisherEntities is
// the end-to-end pin: after analyze --refresh against an npm-hosted
// target, every maintainer + per-version publisher login lands as
// an identity:npm/<login> entity in the store. Includes the
// idempotency case (jdalton appears in both Maintainers and
// versions[].\_npmUser; one row should result, not a duplicate).
func TestPathC_AnalyzeRefresh_MintsMaintainerAndPublisherEntities(t *testing.T) {
	srv := pathCNpmServer(t)
	dbPath := runAnalyzeWithNpmRegistry(t, srv, "pkg:npm/lodash-test")

	s, err := store.OpenSQLite(t.Context(), dbPath)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck

	// jdalton appears in BOTH the Maintainers list AND as a version
	// publisher. EnsureEntityByCanonicalURI's "find OR mint"
	// contract means one row, not two; verify it materialised.
	jdalton, err := s.FindEntityByURI(t.Context(), "identity:npm/jdalton")
	require.NoError(t, err,
		"identity:npm/jdalton must exist after analyze --refresh — it's both a maintainer and a publisher")
	assert.Equal(t, profile.EntityIdentity, jdalton.Type,
		"npm-publisher entities must carry Type=EntityIdentity")
	assert.Equal(t, "jdalton", jdalton.ShortName,
		"the npm login flows through as the entity's short name")

	// bnjmnt4n appears ONLY as a per-version publisher (not in the
	// current Maintainers list — they published an older version
	// before being removed, the lodash takeover-pattern shape).
	// The collector must still mint their identity entity so a
	// future cascade-burn on bnjmnt4n has a row to attach to.
	bnjmnt4n, err := s.FindEntityByURI(t.Context(), "identity:npm/bnjmnt4n")
	require.NoError(t, err,
		"identity:npm/bnjmnt4n must exist — historical publishers are identity-relevant even when no longer in Maintainers")
	assert.Equal(t, profile.EntityIdentity, bnjmnt4n.Type)
}

// TestPathC_AnalyzeRefresh_PublisherEntityIdempotent verifies that
// running analyze twice against the same npm target does not
// produce duplicate publisher-entity rows. Parallel to Path A's
// idempotency test — pin the contract end-to-end so a future
// refactor that re-mints with a fresh UUID against the same URI
// (which the unique index would reject, breaking the second run)
// gets caught here.
func TestPathC_AnalyzeRefresh_PublisherEntityIdempotent(t *testing.T) {
	srv := pathCNpmServer(t)
	dbPath := runAnalyzeWithNpmRegistry(t, srv, "pkg:npm/lodash-test")

	s, err := store.OpenSQLite(t.Context(), dbPath)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck

	npmClient := npmcollector.NewClientWithBaseURL(srv.URL)
	npmColl := npmcollector.NewCollectorWithClient(npmClient).WithEntityStore(s)

	globals := &Globals{
		DBPath:         dbPath,
		Collectors:     []signal.Collector{npmColl},
		AuditFilePath:  filepath.Join(t.TempDir(), "audit.log"),
		Context:        context.Background(),
		NpmRegistryURL: srv.URL,
	}
	cmd := &AnalyzeCmd{Target: "pkg:npm/lodash-test", Refresh: true}
	require.NoError(t, cmd.Run(globals),
		"second analyze run must succeed — publisher-entity creation is idempotent")

	// FindEntityByURI returns at most one row (canonical_uri is
	// UNIQUE indexed); the assertion is that it returns successfully,
	// confirming no duplicate-row collision happened on the second mint.
	jdalton, err := s.FindEntityByURI(t.Context(), "identity:npm/jdalton")
	require.NoError(t, err)
	assert.Equal(t, "jdalton", jdalton.ShortName,
		"the existing publisher row must remain intact across re-runs — no overwrite, no duplicate")
}
