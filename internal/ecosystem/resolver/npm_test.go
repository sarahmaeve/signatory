package resolver

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/signal/registry/npm"
)

// TestNpmResolver_ResolveSource_HappyPath covers the full round trip:
// httptest registry → client → resolver → DeclaredSource with both
// URI and URL populated.
func TestNpmResolver_ResolveSource_HappyPath(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"name":"express","repository":{"type":"git","url":"git+https://github.com/expressjs/express.git"}}`)
	}))
	defer srv.Close()

	r := NewNpmResolverWithClient(npm.NewClientWithBaseURL(srv.URL))
	got, err := r.ResolveSource(context.Background(), "express")
	require.NoError(t, err)
	assert.Equal(t, "repo:github/expressjs/express", got.URI)
	assert.Equal(t, "https://github.com/expressjs/express", got.URL)
	assert.True(t, got.SelfReported,
		"npm-resolver sources are always self-reported until cryptographic binding lands")
}

// TestNpmResolver_ResolveSource_NoDeclaredRepo covers the legitimate
// "we have the package but no repository field" case. Empty URI/URL
// with nil error — same shape as the Go resolver's unknown-vanity
// case so callers can branch uniformly.
func TestNpmResolver_ResolveSource_NoDeclaredRepo(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"name":"orphan"}`)
	}))
	defer srv.Close()

	r := NewNpmResolverWithClient(npm.NewClientWithBaseURL(srv.URL))
	got, err := r.ResolveSource(context.Background(), "orphan")
	require.NoError(t, err)
	assert.Empty(t, got.URI)
	assert.Empty(t, got.URL)
	assert.True(t, got.SelfReported)
}

// TestNpmResolver_ResolveSource_GitLabRepo pins multi-forge resolution
// at the ecosystem-resolver layer: an npm package declaring a gitlab
// repository now produces a fully populated DeclaredSource (canonical
// URI + clone URL), the same shape a github source has produced
// since v0.1. Pre-multi-forge this returned empty.
func TestNpmResolver_ResolveSource_GitLabRepo(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"name":"gitlabby","repository":{"type":"git","url":"https://gitlab.com/foo/bar"}}`)
	}))
	defer srv.Close()

	r := NewNpmResolverWithClient(npm.NewClientWithBaseURL(srv.URL))
	got, err := r.ResolveSource(context.Background(), "gitlabby")
	require.NoError(t, err)
	assert.Equal(t, "repo:gitlab/foo/bar", got.URI)
	assert.Equal(t, "https://gitlab.com/foo/bar", got.URL)
}

// TestNpmResolver_ResolveSource_CodebergRepo — same for Codeberg.
func TestNpmResolver_ResolveSource_CodebergRepo(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"name":"cb","repository":{"type":"git","url":"https://codeberg.org/forgejo/forgejo"}}`)
	}))
	defer srv.Close()

	r := NewNpmResolverWithClient(npm.NewClientWithBaseURL(srv.URL))
	got, err := r.ResolveSource(context.Background(), "cb")
	require.NoError(t, err)
	assert.Equal(t, "repo:codeberg/forgejo/forgejo", got.URI)
	assert.Equal(t, "https://codeberg.org/forgejo/forgejo", got.URL)
}

// TestNpmResolver_ResolveSource_UnsupportedForgeRepo keeps the
// rejection invariant in place for forges NOT yet first-classed.
// The downstream NormalizeDeclaredRepoURL returns empty; the
// resolver propagates as DeclaredSource{} — "package exists but
// has no recognized source."
func TestNpmResolver_ResolveSource_UnsupportedForgeRepo(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"name":"bb","repository":{"type":"git","url":"https://bitbucket.org/team/repo"}}`)
	}))
	defer srv.Close()

	r := NewNpmResolverWithClient(npm.NewClientWithBaseURL(srv.URL))
	got, err := r.ResolveSource(context.Background(), "bb")
	require.NoError(t, err)
	assert.Empty(t, got.URI,
		"unsupported forges (bitbucket, self-hosted) still resolve to empty until first-classed")
}

// TestNpmResolver_ResolveSource_RegistryError covers the failure
// path: a 500 response from the registry surfaces as an error.
func TestNpmResolver_ResolveSource_RegistryError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	r := NewNpmResolverWithClient(npm.NewClientWithBaseURL(srv.URL))
	_, err := r.ResolveSource(context.Background(), "anything")
	require.Error(t, err)
}
