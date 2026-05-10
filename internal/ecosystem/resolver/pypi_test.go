package resolver

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/signal/registry/pypi"
)

// TestPyPIResolver_ResolveSource_HappyPath covers the full round
// trip: httptest registry → client → resolver → DeclaredSource
// with both URI and URL populated. Mirrors the npm resolver's
// happy-path test for parity.
func TestPyPIResolver_ResolveSource_HappyPath(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"info":{"project_urls":{"Repository":"https://github.com/theskumar/python-dotenv"}}}`)
	}))
	defer srv.Close()

	r := NewPyPIResolverWithClient(pypi.NewClientWithBaseURL(srv.URL))
	got, err := r.ResolveSource(context.Background(), "python-dotenv")
	require.NoError(t, err)
	assert.Equal(t, "repo:github/theskumar/python-dotenv", got.URI)
	assert.Equal(t, "https://github.com/theskumar/python-dotenv", got.URL)
	assert.True(t, got.SelfReported,
		"pypi-resolver sources are always self-reported until cryptographic binding lands")
}

// TestPyPIResolver_ResolveSource_NoDeclaredRepo covers the
// legitimate "registry has the package but no resolvable repo
// declaration" case — empty info, only docs URLs, etc. Returns
// DeclaredSource with SelfReported=true and empty URI/URL, same
// shape as npm.
func TestPyPIResolver_ResolveSource_NoDeclaredRepo(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"info":{"project_urls":{"Documentation":"https://docs.example.com/"}}}`)
	}))
	defer srv.Close()

	r := NewPyPIResolverWithClient(pypi.NewClientWithBaseURL(srv.URL))
	got, err := r.ResolveSource(context.Background(), "docs-only")
	require.NoError(t, err)
	assert.Empty(t, got.URI)
	assert.Empty(t, got.URL)
	assert.True(t, got.SelfReported)
}

// TestPyPIResolver_ResolveSource_GitLabRepo pins multi-forge
// resolution at the ecosystem-resolver layer: a project declaring a
// gitlab repository in info.project_urls now produces a fully
// populated DeclaredSource (canonical URI + clone URL), the same
// shape a github source has produced since v0.1. Pre-multi-forge
// this returned empty.
func TestPyPIResolver_ResolveSource_GitLabRepo(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"info":{"project_urls":{"Repository":"https://gitlab.com/foo/bar"}}}`)
	}))
	defer srv.Close()

	r := NewPyPIResolverWithClient(pypi.NewClientWithBaseURL(srv.URL))
	got, err := r.ResolveSource(context.Background(), "gitlabby")
	require.NoError(t, err)
	assert.Equal(t, "repo:gitlab/foo/bar", got.URI)
	assert.Equal(t, "https://gitlab.com/foo/bar", got.URL)
}

// TestPyPIResolver_ResolveSource_CodebergRepo — same for Codeberg.
func TestPyPIResolver_ResolveSource_CodebergRepo(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"info":{"project_urls":{"Repository":"https://codeberg.org/forgejo/forgejo"}}}`)
	}))
	defer srv.Close()

	r := NewPyPIResolverWithClient(pypi.NewClientWithBaseURL(srv.URL))
	got, err := r.ResolveSource(context.Background(), "cb")
	require.NoError(t, err)
	assert.Equal(t, "repo:codeberg/forgejo/forgejo", got.URI)
	assert.Equal(t, "https://codeberg.org/forgejo/forgejo", got.URL)
}

// TestPyPIResolver_ResolveSource_UnsupportedForgeRepo keeps the
// rejection invariant in place for forges NOT yet first-classed.
func TestPyPIResolver_ResolveSource_UnsupportedForgeRepo(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"info":{"project_urls":{"Repository":"https://bitbucket.org/team/repo"}}}`)
	}))
	defer srv.Close()

	r := NewPyPIResolverWithClient(pypi.NewClientWithBaseURL(srv.URL))
	got, err := r.ResolveSource(context.Background(), "bb")
	require.NoError(t, err)
	assert.Empty(t, got.URI,
		"unsupported forges (bitbucket, self-hosted) still resolve to empty until first-classed")
}

// TestPyPIResolver_ResolveSource_HomePageFallback covers the
// deprecated info.home_page path: legacy releases that populate
// only home_page (no project_urls) still resolve.
func TestPyPIResolver_ResolveSource_HomePageFallback(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"info":{"home_page":"https://github.com/legacy/project"}}`)
	}))
	defer srv.Close()

	r := NewPyPIResolverWithClient(pypi.NewClientWithBaseURL(srv.URL))
	got, err := r.ResolveSource(context.Background(), "legacy")
	require.NoError(t, err)
	assert.Equal(t, "repo:github/legacy/project", got.URI)
}

// TestPyPIResolver_ResolveSource_RegistryError covers the failure
// path: a 500 response from the registry surfaces as an error
// (not as DeclaredSource{}, which is reserved for "no source
// declared").
func TestPyPIResolver_ResolveSource_RegistryError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	r := NewPyPIResolverWithClient(pypi.NewClientWithBaseURL(srv.URL))
	_, err := r.ResolveSource(context.Background(), "anything")
	require.Error(t, err)
}

// TestPyPIResolver_RegisteredInDefault confirms init() wires the
// resolver into the package-level Default registry under the
// "pypi" key. Without this, callers that use Default.Resolve to
// route by ecosystem would still see ErrNoResolver for pypi
// targets.
func TestPyPIResolver_RegisteredInDefault(t *testing.T) {
	t.Parallel()

	ecos := Default.Ecosystems()
	assert.Contains(t, ecos, "pypi",
		"PyPI resolver should be auto-registered with Default; got %v", ecos)
}
