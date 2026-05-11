package gitlab

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/ecosystem"
)

// Tests for the narrow helper methods that the ecosystem detector
// consumes: GetRepoLanguage and ListRootFilenames. Mirrors the
// internal/signal/github/detection_helpers_test.go and
// internal/signal/forgejo/detection_helpers_test.go shapes so the
// per-forge Source implementations stay symmetric.

// Compile-time check: *Client must satisfy ecosystem.Source so
// --network-precheck can route gitlab.com targets through the same
// detector that handles github.com / codeberg.org. Drift here
// would break the platform-keyed dispatch in cmd/signatory/handoff.go.
var _ ecosystem.Source = (*Client)(nil)

// newHelperTestClient returns a *Client wired at a test server URL,
// matching the per-forge helper test pattern. Production NewClient
// bakes /api/v4 into baseURL; tests inject the bare server URL and
// the path-construction logic in get() ("baseURL + path") is what
// gets exercised.
func newHelperTestClient(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return &Client{
		httpClient: server.Client(),
		baseURL:    server.URL,
	}
}

func TestGetRepoLanguage_ReturnsTopPercentage(t *testing.T) {
	// GitLab's /languages endpoint returns a JSON object mapping
	// language name → percentage (float). The "primary" language is
	// the one with the highest percentage — matches github's
	// "primary language" semantic (which itself is the max-byte
	// language) closely enough that the ecosystem detector can
	// consume both interchangeably.
	//
	// Path encoding: GitLab addresses projects by URL-encoded namespace
	// path (gitlab-org/gitlab → gitlab-org%2Fgitlab). The test server
	// receives the already-encoded path verbatim.
	mux := http.NewServeMux()
	mux.HandleFunc("/projects/gitlab-org%2Fgitlab/languages", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]float64{
			"Ruby":       80.5,
			"JavaScript": 12.3,
			"CSS":        7.2,
		})
	})
	c := newHelperTestClient(t, mux)

	got, err := c.GetRepoLanguage(context.Background(), "gitlab-org", "gitlab")
	require.NoError(t, err)
	assert.Equal(t, "Ruby", got)
}

func TestGetRepoLanguage_EmptyWhenNoLanguages(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/projects/foo%2Fempty/languages", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]float64{})
	})
	c := newHelperTestClient(t, mux)

	got, err := c.GetRepoLanguage(context.Background(), "foo", "empty")
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestGetRepoLanguage_EmptyWhenMissingProject(t *testing.T) {
	// 404 on the languages endpoint folds to empty so the detector
	// can degrade to its manifest-only path. Matches github/forgejo
	// "no language" handling.
	mux := http.NewServeMux()
	mux.HandleFunc("/projects/foo%2Fmissing/languages", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"404 Project Not Found"}`, http.StatusNotFound)
	})
	c := newHelperTestClient(t, mux)

	got, err := c.GetRepoLanguage(context.Background(), "foo", "missing")
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestListRootFilenames_FiltersDirectories(t *testing.T) {
	// GitLab's /repository/tree endpoint returns entries each with
	// Type ∈ {"blob" (file), "tree" (directory), "commit" (submodule)}.
	// Only "blob" entries are useful for manifest detection.
	mux := http.NewServeMux()
	mux.HandleFunc("/projects/gitlab-org%2Fgitlab/repository/tree", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]treeEntry{
			{Name: ".gitlab", Type: "tree"},
			{Name: "app", Type: "tree"},
			{Name: "spec", Type: "tree"},
			{Name: "Gemfile", Type: "blob"},
			{Name: "Gemfile.lock", Type: "blob"},
			{Name: "LICENSE", Type: "blob"},
			{Name: "README.md", Type: "blob"},
		})
	})
	c := newHelperTestClient(t, mux)

	got, err := c.ListRootFilenames(context.Background(), "gitlab-org", "gitlab")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{
		"Gemfile", "Gemfile.lock", "LICENSE", "README.md",
	}, got)
}

func TestListRootFilenames_EmptyRepo(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/projects/foo%2Fempty/repository/tree", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]treeEntry{})
	})
	c := newHelperTestClient(t, mux)

	got, err := c.ListRootFilenames(context.Background(), "foo", "empty")
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestListRootFilenames_MissingProject(t *testing.T) {
	// 404 returns (nil, nil) so the detector can fall through to
	// language-only — matches github/forgejo helper shape.
	mux := http.NewServeMux()
	mux.HandleFunc("/projects/foo%2Fmissing/repository/tree", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"404 Project Not Found"}`, http.StatusNotFound)
	})
	c := newHelperTestClient(t, mux)

	got, err := c.ListRootFilenames(context.Background(), "foo", "missing")
	require.NoError(t, err)
	assert.Empty(t, got)
}
