package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for the narrow helper methods that the ecosystem detector
// consumes: GetRepoLanguage and ListRootFilenames. The goal is to
// confirm both helpers reduce their underlying API responses to
// exactly what a detector needs — no broader coupling to the repo
// struct or repoContent type.

// newHelperTestClient returns a *Client wired at a test server URL.
// Similar to newTestCollector but exposes the Client directly so we
// can exercise methods that don't flow through a Collector.
func newHelperTestClient(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return NewClientWithBaseURLAndToken(server.URL, "test-token")
}

func TestGetRepoLanguage_ReturnsRepoField(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/atuinsh/atuin", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(repo{
			Name:     "atuin",
			FullName: "atuinsh/atuin",
			Owner:    repoOwner{Login: "atuinsh", Type: "Organization"},
			Language: "Rust",
		})
	})
	c := newHelperTestClient(t, mux)

	got, err := c.GetRepoLanguage(context.Background(), "atuinsh", "atuin")
	require.NoError(t, err)
	assert.Equal(t, "Rust", got)
}

func TestGetRepoLanguage_EmptyWhenAbsent(t *testing.T) {
	// GitHub returns language=null for repos with no detectable
	// language (empty repos, README-only, etc.). We want the empty
	// string, not a nil-pointer deref or an error.
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/foo/empty", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(repo{
			Name:  "empty",
			Owner: repoOwner{Login: "foo", Type: "User"},
			// Language intentionally omitted → JSON null → Go zero value "".
		})
	})
	c := newHelperTestClient(t, mux)

	got, err := c.GetRepoLanguage(context.Background(), "foo", "empty")
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestListRootFilenames_FiltersDirectories(t *testing.T) {
	// The root listing contains files AND directories; only the file
	// names are useful for manifest detection. Confirm the helper
	// strips directories.
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/nvbn/thefuck/contents", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]repoContent{
			{Name: ".github", Type: "dir", Path: ".github"},
			{Name: "thefuck", Type: "dir", Path: "thefuck"},
			{Name: "tests", Type: "dir", Path: "tests"},
			{Name: "LICENSE.md", Type: "file", Path: "LICENSE.md"},
			{Name: "README.md", Type: "file", Path: "README.md"},
			{Name: "setup.py", Type: "file", Path: "setup.py"},
			{Name: "requirements.txt", Type: "file", Path: "requirements.txt"},
			{Name: "release.py", Type: "file", Path: "release.py"},
		})
	})
	c := newHelperTestClient(t, mux)

	got, err := c.ListRootFilenames(context.Background(), "nvbn", "thefuck")
	require.NoError(t, err)
	// Directories ".github", "thefuck", "tests" must be absent;
	// the five files must be present.
	assert.ElementsMatch(t, []string{
		"LICENSE.md", "README.md", "setup.py", "requirements.txt", "release.py",
	}, got)
}

func TestListRootFilenames_EmptyRepo(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/foo/empty/contents", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]repoContent{})
	})
	c := newHelperTestClient(t, mux)

	got, err := c.ListRootFilenames(context.Background(), "foo", "empty")
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestListRootFilenames_MissingRepo(t *testing.T) {
	// GetDirectoryContents returns nil without error for a missing
	// path (intentional API). ListRootFilenames inherits that shape.
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/foo/missing/contents", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	})
	c := newHelperTestClient(t, mux)

	got, err := c.ListRootFilenames(context.Background(), "foo", "missing")
	require.NoError(t, err)
	assert.Empty(t, got)
}
