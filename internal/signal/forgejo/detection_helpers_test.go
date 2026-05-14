package forgejo

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
// internal/signal/github/detection_helpers_test.go shape so the
// per-forge Source implementations stay symmetric.

// Compile-time check: *Client must satisfy ecosystem.Source so
// --network-precheck can route codeberg.org targets through the same
// detector that handles github.com today. Drift here would break the
// platform-keyed dispatch in cmd/signatory/handoff.go.
var _ ecosystem.Source = (*Client)(nil)

// newHelperTestClient returns a *Client wired at a test server URL,
// matching newTestCollector's shape but exposing the bare Client so
// these tests don't have to round-trip through a Collector.
func newHelperTestClient(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return NewClientWithBaseURL(server.URL)
}

func TestGetRepoLanguage_ReturnsTopByteCount(t *testing.T) {
	// Forgejo's /languages endpoint returns a JSON object mapping
	// language name → byte count. The "primary" language is the one
	// with the highest byte count — same definition github uses.
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/forgejo/forgejo/languages", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]int64{
			"Go":         12_345_678,
			"JavaScript": 2_222_222,
			"CSS":        1_111_111,
		})
	})
	c := newHelperTestClient(t, mux)

	got, err := c.GetRepoLanguage(context.Background(), "forgejo", "forgejo")
	require.NoError(t, err)
	assert.Equal(t, "Go", got)
}

func TestGetRepoLanguage_EmptyWhenNoLanguages(t *testing.T) {
	// Empty repos / docs-only repos return an empty object. We want
	// the empty string back, not an error.
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/foo/empty/languages", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]int64{})
	})
	c := newHelperTestClient(t, mux)

	got, err := c.GetRepoLanguage(context.Background(), "foo", "empty")
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestGetRepoLanguage_EmptyWhenMissingRepo(t *testing.T) {
	// 404 on the languages endpoint means we have nothing to report;
	// the ecosystem detector treats an empty language as "unknown"
	// and falls back to the manifest-only path. Matches github's
	// "no language" handling.
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/foo/missing/languages", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	})
	c := newHelperTestClient(t, mux)

	got, err := c.GetRepoLanguage(context.Background(), "foo", "missing")
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestListRootFilenames_FiltersDirectories(t *testing.T) {
	// Forgejo's /contents endpoint mirrors github's: each entry has
	// Type ∈ {"file", "dir", "symlink", "submodule"}. Only "file"
	// entries are useful for manifest detection.
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/forgejo/forgejo/contents", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]repoContent{
			{Name: ".github", Type: "dir"},
			{Name: "cmd", Type: "dir"},
			{Name: "go.mod", Type: "file"},
			{Name: "go.sum", Type: "file"},
			{Name: "LICENSE", Type: "file"},
			{Name: "README.md", Type: "file"},
		})
	})
	c := newHelperTestClient(t, mux)

	got, err := c.ListRootFilenames(context.Background(), "forgejo", "forgejo")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{
		"go.mod", "go.sum", "LICENSE", "README.md",
	}, got)
}

func TestListRootFilenames_EmptyRepo(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/foo/empty/contents", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]repoContent{})
	})
	c := newHelperTestClient(t, mux)

	got, err := c.ListRootFilenames(context.Background(), "foo", "empty")
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestListRootFilenames_MissingRepo(t *testing.T) {
	// Matches github's helper: 404 returns (nil, nil) so the
	// detector falls through to its language-only path rather than
	// surfacing a hard error.
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/foo/missing/contents", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	})
	c := newHelperTestClient(t, mux)

	got, err := c.ListRootFilenames(context.Background(), "foo", "missing")
	require.NoError(t, err)
	assert.Empty(t, got)
}
