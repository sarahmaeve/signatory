package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal"
	ghcollector "github.com/sarahmaeve/signatory/internal/signal/github"
	"github.com/sarahmaeve/signatory/internal/store"
)

// Path A end-to-end: a `signatory analyze --refresh repo:github/X/Y`
// run against a github-hosted target must mint the owner entity row
// (identity:github/X for User-typed owners; org:github/X for
// Organization-typed) alongside the existing owner_profile signal.
//
// These tests exercise the integration through AnalyzeCmd.Run +
// the github collector + the store's EnsureEntityByCanonicalURI
// helper. They use globals.Collectors to inject a github collector
// pointed at an httptest server (production code reads from
// api.github.com via a real Client; redirecting it requires
// constructing the Client manually and going through
// NewCollectorWithClient).
//
// The collectorsFor wiring (CollectOpts.EntityStore → ghcollector
// .WithEntityStore) is covered by compile-time checks plus the
// github package's unit tests (entity_owner_test.go); these tests
// pin the higher-level invariant that an analyze run produces the
// owner-entity side effect.

// minimalGitHubAPI returns an httptest server serving the smallest
// surface a github collector run needs. Endpoints not exercised by
// owner-profile collection return 404; the collector records those
// as absences without failing the overall run, which is the same
// behaviour real github responses produce on rate-limit / 5xx.
//
// owner-flavour controls the User vs Organization branch — pass
// "User" for the identity: case, "Organization" for org:.
func minimalGitHubAPI(t *testing.T, ownerLogin, repoName, ownerType string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	repoPath := fmt.Sprintf("/repos/%s/%s", ownerLogin, repoName)
	mux.HandleFunc(repoPath, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name":      repoName,
			"full_name": ownerLogin + "/" + repoName,
			"owner": map[string]any{
				"login": ownerLogin,
				"type":  ownerType,
			},
			"created_at":        time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339),
			"pushed_at":         time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339),
			"updated_at":        time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339),
			"stargazers_count":  100,
			"forks_count":       10,
			"open_issues_count": 5,
			"archived":          false,
			"license":           map[string]any{"spdx_id": "MIT"},
		})
	})

	mux.HandleFunc("/users/"+ownerLogin, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"login":        ownerLogin,
			"type":         ownerType,
			"created_at":   time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339),
			"public_repos": 5,
			"followers":    10,
		})
	})

	// Catch-all: return 404 so the collector records absences for
	// signals it can't fetch (contributors, commits, tags, etc.).
	// The owner-entity branch we're testing fires regardless.
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// runAnalyzeWithGitHubAPI sets up a fresh test DB and runs
// AnalyzeCmd against the supplied github API mock, with a real
// github collector wired to the test store via WithEntityStore.
// Returns the DB path so the caller can inspect post-run state.
func runAnalyzeWithGitHubAPI(t *testing.T, srv *httptest.Server, target string) string {
	t.Helper()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	// Open the store so we can wire it into the github collector
	// via WithEntityStore. Close before AnalyzeCmd opens its own
	// handle (sqlite3 allows multiple readers, but the test is
	// cleaner with single-handle ownership at any moment).
	{
		s, err := store.OpenSQLite(t.Context(), dbPath)
		require.NoError(t, err)
		require.NoError(t, s.Close())
	}

	// Re-open for the analyze run. Pass the same store handle to
	// the github collector via WithEntityStore so owner-entity
	// minting writes through to the same DB AnalyzeCmd is using.
	s, err := store.OpenSQLite(t.Context(), dbPath)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck

	githubClient := ghcollector.NewClientWithBaseURL(srv.URL)
	githubColl := ghcollector.NewCollectorWithClient(githubClient).WithEntityStore(s)

	globals := &Globals{
		DBPath:        dbPath,
		Collectors:    []signal.Collector{githubColl},
		AuditFilePath: filepath.Join(dir, "audit.log"),
		Context:       context.Background(),
	}

	cmd := &AnalyzeCmd{Target: target, Refresh: true}
	require.NoError(t, cmd.Run(globals),
		"analyze --refresh against the mocked github API must succeed")

	return dbPath
}

// TestPathA_AnalyzeRefresh_MintsIdentityEntity_ForUserOwner pins
// the User-flavour end-to-end: analyze a github repo whose owner
// is a User account, verify identity:github/<login> exists in the
// store after the run.
func TestPathA_AnalyzeRefresh_MintsIdentityEntity_ForUserOwner(t *testing.T) {
	srv := minimalGitHubAPI(t, "test-user", "test-repo", "User")
	dbPath := runAnalyzeWithGitHubAPI(t, srv, "repo:github/test-user/test-repo")

	s, err := store.OpenSQLite(t.Context(), dbPath)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck

	owner, err := s.FindEntityByURI(t.Context(), "identity:github/test-user")
	require.NoError(t, err,
		"after analyze --refresh against a User-owned github repo, identity:github/<login> must exist")
	assert.Equal(t, profile.EntityIdentity, owner.Type,
		"User-typed owners produce EntityIdentity rows")
	assert.Equal(t, "test-user", owner.ShortName,
		"the github login flows through as the short name")
}

// TestPathA_AnalyzeRefresh_MintsOrgEntity_ForOrganizationOwner is
// the Organization-flavour parallel.
func TestPathA_AnalyzeRefresh_MintsOrgEntity_ForOrganizationOwner(t *testing.T) {
	srv := minimalGitHubAPI(t, "test-org", "test-repo", "Organization")
	dbPath := runAnalyzeWithGitHubAPI(t, srv, "repo:github/test-org/test-repo")

	s, err := store.OpenSQLite(t.Context(), dbPath)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck

	owner, err := s.FindEntityByURI(t.Context(), "org:github/test-org")
	require.NoError(t, err,
		"after analyze --refresh against an Organization-owned github repo, org:github/<name> must exist")
	assert.Equal(t, profile.EntityOrg, owner.Type,
		"Organization-typed owners produce EntityOrg rows")

	// Sanity: the identity: variant must NOT exist for an
	// Organization owner. The branch in collectOwnerProfile picks
	// scheme by Type — getting both rows would mean we're
	// double-emitting.
	_, err = s.FindEntityByURI(t.Context(), "identity:github/test-org")
	require.Error(t, err,
		"identity:github/<org-name> must NOT be created for an Organization owner — the Type-driven URI scheme branch must pick exactly one")
}

// TestPathA_AnalyzeRefresh_OwnerEntityIdempotent verifies that
// running analyze twice against the same target does not produce
// duplicate owner-entity rows. EnsureEntityByCanonicalURI's "find
// OR mint" contract makes this a no-op on the second call, but
// pinning it end-to-end catches any wiring regression that would
// re-mint or attempt to insert with a fresh UUID against the same
// canonical_uri (which the unique index would reject, breaking
// the second analyze run).
func TestPathA_AnalyzeRefresh_OwnerEntityIdempotent(t *testing.T) {
	srv := minimalGitHubAPI(t, "test-user", "test-repo", "User")
	dbPath := runAnalyzeWithGitHubAPI(t, srv, "repo:github/test-user/test-repo")

	// Second analyze run against the same target — must succeed,
	// must not duplicate the owner entity row.
	s, err := store.OpenSQLite(t.Context(), dbPath)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck

	githubClient := ghcollector.NewClientWithBaseURL(srv.URL)
	githubColl := ghcollector.NewCollectorWithClient(githubClient).WithEntityStore(s)

	globals := &Globals{
		DBPath:        dbPath,
		Collectors:    []signal.Collector{githubColl},
		AuditFilePath: filepath.Join(t.TempDir(), "audit.log"),
		Context:       context.Background(),
	}
	cmd := &AnalyzeCmd{Target: "repo:github/test-user/test-repo", Refresh: true}
	require.NoError(t, cmd.Run(globals),
		"second analyze run must succeed — owner-entity creation is idempotent")

	// FindEntityByURI returns at most one row (canonical_uri is
	// UNIQUE indexed); the assertion is that it returns successfully,
	// confirming no duplicate-row collision happened on the second mint.
	owner, err := s.FindEntityByURI(t.Context(), "identity:github/test-user")
	require.NoError(t, err)
	assert.Equal(t, "test-user", owner.ShortName,
		"the existing owner row must remain intact across re-runs — no overwrite, no duplicate")
}
