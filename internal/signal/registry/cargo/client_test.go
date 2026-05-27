package cargo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestValidateCrateVersion pins the version-string grammar gate.
// crates.io versions flow into two trust-boundary-sensitive sinks:
//
//   - git argv via resolveTagSHA's `rev-parse --verify <version>^{commit}`
//     — a `--`-prefixed Num would be parsed as a flag, violating the
//     gitenv.NewCmd "argv-validation is the call site's responsibility"
//     contract.
//   - HTTP URL path via fetchVCSInfoSHA. url.PathEscape is defense in
//     depth, but the grammar gate is the load-bearing trust boundary.
//
// Publisher controls v.Num in the crates.io JSON. The validator
// enforces the cargo / semver grammar (`[0-9A-Za-z][0-9A-Za-z._+-]*`)
// and a length cap, rejecting leading-dash and embedded control
// bytes before either sink sees the string.
func TestValidateCrateVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		// -- valid semver shapes --
		{"valid semver basic", "1.2.3", false},
		{"valid single digit", "1", false},
		{"valid double-digit components", "10.20.30", false},
		{"valid pre-release", "1.2.3-alpha.1", false},
		{"valid build metadata", "1.2.3+build.42", false},
		{"valid pre-release + build", "1.0.0-rc.1+sha.abcdef", false},
		{"valid underscore in tag", "1.0.0-a_b", false},
		{"valid alpha-only", "alpha", false}, // non-conventional but in-grammar

		// -- empty / length --
		{"empty", "", true},
		{"too long", strings.Repeat("1", 257), true},

		// -- leading-dash (git-flag injection) --
		{"leading dash", "-rf", true},
		{"git long flag form", "--upload-pack=foo", true},
		{"bare dash", "-", true},
		{"double dash", "--", true},

		// -- non-alphanumeric leading char --
		{"leading dot", ".1.0", true},
		{"leading plus", "+1.0", true},
		{"leading underscore", "_1.0", true},

		// -- control bytes (newline / CR / null) --
		{"embedded newline", "1.0.0\n--malicious", true},
		{"embedded CR", "1.0.0\r--malicious", true},
		{"trailing newline", "1.0.0\n", true},
		{"null byte", "1.0.0\x00", true},
		{"tab", "1.0.0\t", true},

		// -- shell / URL metachars (defense in depth; not directly
		// exploitable today but out-of-grammar regardless) --
		{"embedded space", "1.0.0 1.0.1", true},
		{"leading space", " 1.0.0", true},
		{"slash", "1.0.0/x", true},
		{"backslash", "1.0.0\\x", true},
		{"semicolon", "1.0.0;ls", true},
		{"backtick", "1.0.0`ls`", true},
		{"dollar", "1.0.0$x", true},
		{"pipe", "1.0.0|x", true},
		{"ampersand", "1.0.0&x", true},
		{"redirect", "1.0.0>x", true},
		{"colon", "1.0.0:x", true},
		{"comma", "1.0.0,x", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateCrateVersion(tc.input)
			if tc.wantErr {
				assert.Error(t, err,
					"ValidateCrateVersion(%q) should reject", tc.input)
			} else {
				assert.NoError(t, err,
					"ValidateCrateVersion(%q) should accept", tc.input)
			}
		})
	}
}

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
// The URL gate (rejectUnrecognizedForgeURL) is the source of truth for which
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

func TestGetDependencies_Success(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/crates/serde/1.0.219/dependencies", r.URL.Path)
		assert.Contains(t, r.Header.Get("User-Agent"), "signatory")

		resp := DependenciesResponse{
			Dependencies: []Dependency{
				{CrateID: "serde_derive", Req: "=1.0.219", Kind: "normal", Optional: false},
				{CrateID: "syn", Req: "^2", Kind: "build", Optional: false},
				{CrateID: "trybuild", Req: "^1", Kind: "dev", Optional: false},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	}))
	defer srv.Close()

	client := NewClientWithBaseURL(srv.URL)
	deps, err := client.GetDependencies(context.Background(), "serde", "1.0.219")
	require.NoError(t, err)
	require.Len(t, deps.Dependencies, 3)
	assert.Equal(t, "serde_derive", deps.Dependencies[0].CrateID)
	assert.Equal(t, "build", deps.Dependencies[1].Kind)
	assert.Equal(t, "dev", deps.Dependencies[2].Kind)
}

func TestGetDependencies_NotFound(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	client := NewClientWithBaseURL(srv.URL)
	_, err := client.GetDependencies(context.Background(), "serde", "9.9.9")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestGetDependencies_InvalidName(t *testing.T) {
	t.Parallel()

	client := NewClient()
	_, err := client.GetDependencies(context.Background(), "../../etc/passwd", "1.0.0")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not match")
}
