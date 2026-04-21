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

// TestNpmResolver_ResolveSource_NonGithubRepo covers the case where
// the npm package declares a gitlab or other non-github repo. The
// underlying NormalizeDeclaredRepoURL returns empty for non-github
// hosts; we propagate that as DeclaredSource{} — consistent with
// "we know the package exists but can't produce a GitHub source."
func TestNpmResolver_ResolveSource_NonGithubRepo(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"name":"gitlabby","repository":{"type":"git","url":"https://gitlab.com/foo/bar"}}`)
	}))
	defer srv.Close()

	r := NewNpmResolverWithClient(npm.NewClientWithBaseURL(srv.URL))
	got, err := r.ResolveSource(context.Background(), "gitlabby")
	require.NoError(t, err)
	assert.Empty(t, got.URI, "non-github source is reported as no-source until we support other platforms")
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
