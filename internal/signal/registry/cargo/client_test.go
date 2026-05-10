package cargo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateCrateName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"valid simple", "serde", false},
		{"valid with hyphen", "serde-json", false},
		{"valid with underscore", "serde_json", false},
		{"valid single char name", "a", false},
		{"valid mixed", "tokio-macros", false},
		{"empty", "", true},
		{"starts with digit", "123abc", true},
		{"starts with hyphen", "-serde", true},
		{"contains space", "my crate", true},
		{"contains slash", "my/crate", true},
		{"contains dot", "my.crate", true},
		{"too long", string(make([]byte, 65)), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateCrateName(tc.input)
			if tc.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestGetCrate_Success(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/crates/serde", r.URL.Path)
		assert.Contains(t, r.Header.Get("User-Agent"), "signatory")

		resp := CrateResponse{
			Crate: Crate{
				Name:       "serde",
				Repository: "https://github.com/serde-rs/serde",
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	}))
	defer srv.Close()

	client := NewClientWithBaseURL(srv.URL)
	cr, err := client.GetCrate(context.Background(), "serde")
	require.NoError(t, err)
	assert.Equal(t, "serde", cr.Crate.Name)
	assert.Equal(t, "https://github.com/serde-rs/serde", cr.Crate.Repository)
}

func TestGetCrate_NotFound(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	client := NewClientWithBaseURL(srv.URL)
	_, err := client.GetCrate(context.Background(), "nonexistent-crate-xyz")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestGetCrate_InvalidName(t *testing.T) {
	t.Parallel()

	client := NewClient()
	_, err := client.GetCrate(context.Background(), "../../etc/passwd")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not match")
}

func TestGetCrate_ContextCanceled(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Simulate slow response — context should cancel first.
		select {}
	}))
	defer srv.Close()

	client := NewClientWithBaseURL(srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := client.GetCrate(ctx, "serde")
	require.Error(t, err)
}

func TestResolveRepoURL_Success(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := CrateResponse{
			Crate: Crate{
				Name:       "ripgrep",
				Repository: "https://github.com/BurntSushi/ripgrep",
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	}))
	defer srv.Close()

	client := NewClientWithBaseURL(srv.URL)
	repoURL, err := client.ResolveRepoURL(context.Background(), "ripgrep")
	require.NoError(t, err)
	// CloneURL is lowercased per profile.CloneURLForRepoPlatform —
	// case-insensitive forge hosts (github, codeberg, gitlab)
	// canonicalize owner+repo to lowercase to keep store entities
	// from fragmenting across casing variants (issue #53).
	assert.Equal(t, "https://github.com/burntsushi/ripgrep", repoURL)
}

func TestResolveRepoURL_NoRepository(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := CrateResponse{
			Crate: Crate{
				Name:       "no-repo-crate",
				Repository: "",
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	}))
	defer srv.Close()

	client := NewClientWithBaseURL(srv.URL)
	repoURL, err := client.ResolveRepoURL(context.Background(), "no-repo-crate")
	require.NoError(t, err)
	assert.Empty(t, repoURL)
}

// TestResolveRepoURL_GitLabRepository pins multi-forge resolution:
// crates.io declarations pointing at gitlab.com (the second-largest
// open-source git forge after github) now resolve to the canonical
// https URL the downstream git collector clones from. Pre-multi-forge,
// this returned empty.
func TestResolveRepoURL_GitLabRepository(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := CrateResponse{
			Crate: Crate{
				Name:       "gitlab-crate",
				Repository: "https://gitlab.com/some/project",
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	}))
	defer srv.Close()

	client := NewClientWithBaseURL(srv.URL)
	repoURL, err := client.ResolveRepoURL(context.Background(), "gitlab-crate")
	require.NoError(t, err)
	assert.Equal(t, "https://gitlab.com/some/project", repoURL)
}

// TestResolveRepoURL_CodebergRepository — same shape for Codeberg.
func TestResolveRepoURL_CodebergRepository(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := CrateResponse{
			Crate: Crate{
				Name:       "codeberg-crate",
				Repository: "https://codeberg.org/forgejo/runner",
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	}))
	defer srv.Close()

	client := NewClientWithBaseURL(srv.URL)
	repoURL, err := client.ResolveRepoURL(context.Background(), "codeberg-crate")
	require.NoError(t, err)
	assert.Equal(t, "https://codeberg.org/forgejo/runner", repoURL)
}

// TestResolveRepoURL_UnsupportedForgeRepository pins that forges NOT
// yet first-classed (bitbucket, self-hosted) still resolve to empty.
// The URL gate (rejectNonGitHubURL) is the source of truth for which
// forges are accepted; CloneURLForRepoPlatform returns "" for the rest.
func TestResolveRepoURL_UnsupportedForgeRepository(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := CrateResponse{
			Crate: Crate{
				Name:       "bitbucket-crate",
				Repository: "https://bitbucket.org/team/project",
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	}))
	defer srv.Close()

	client := NewClientWithBaseURL(srv.URL)
	repoURL, err := client.ResolveRepoURL(context.Background(), "bitbucket-crate")
	require.NoError(t, err)
	assert.Empty(t, repoURL, "unsupported forges still resolve to empty")
}
