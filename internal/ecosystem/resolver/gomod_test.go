package resolver

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGoModResolver_GithubDirect covers the simplest case: a module
// whose path already starts with github.com/. The resolved URI and
// URL reconstruct cleanly without any lookup.
func TestGoModResolver_GithubDirect(t *testing.T) {
	t.Parallel()

	r := NewGoModResolver()
	got, err := r.ResolveSource(context.Background(), "github.com/alecthomas/kong")
	require.NoError(t, err)
	assert.Equal(t, "repo:github/alecthomas/kong", got.URI)
	assert.Equal(t, "https://github.com/alecthomas/kong", got.URL)
	assert.True(t, got.SelfReported)
}

// TestGoModResolver_GithubWithSubpath covers a module path with
// internal subpackage segments (e.g. github.com/owner/repo/v2 or
// github.com/owner/repo/internal/foo). Only the first two path
// components are load-bearing for source resolution.
func TestGoModResolver_GithubWithSubpath(t *testing.T) {
	t.Parallel()

	r := NewGoModResolver()

	for _, path := range []string{
		"github.com/foo/bar/v2",
		"github.com/foo/bar/internal/helper",
		"github.com/foo/bar/cmd/tool",
	} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			got, err := r.ResolveSource(context.Background(), path)
			require.NoError(t, err)
			assert.Equal(t, "repo:github/foo/bar", got.URI)
			assert.Equal(t, "https://github.com/foo/bar", got.URL)
		})
	}
}

// TestGoModResolver_GolangOrgX covers the canonical golang.org/x/*
// extension-module mapping.
func TestGoModResolver_GolangOrgX(t *testing.T) {
	t.Parallel()

	r := NewGoModResolver()

	cases := []struct {
		in      string
		wantURI string
	}{
		{"golang.org/x/mod", "repo:github/golang/mod"},
		{"golang.org/x/tools", "repo:github/golang/tools"},
		{"golang.org/x/crypto/ssh/terminal", "repo:github/golang/crypto"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got, err := r.ResolveSource(context.Background(), tc.in)
			require.NoError(t, err)
			assert.Equal(t, tc.wantURI, got.URI)
		})
	}
}

// TestGoModResolver_GopkgInSingleSegment covers the gopkg.in shape
// where the path has one name and maps to github.com/go-<name>/<name>.
func TestGoModResolver_GopkgInSingleSegment(t *testing.T) {
	t.Parallel()

	r := NewGoModResolver()

	cases := []struct {
		in      string
		wantURI string
	}{
		{"gopkg.in/yaml.v3", "repo:github/go-yaml/yaml"},
		{"gopkg.in/check.v1", "repo:github/go-check/check"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got, err := r.ResolveSource(context.Background(), tc.in)
			require.NoError(t, err)
			assert.Equal(t, tc.wantURI, got.URI)
		})
	}
}

// TestGoModResolver_GopkgInTwoSegment covers the user/pkg form of
// gopkg.in which maps to github.com/<user>/<pkg>.
func TestGoModResolver_GopkgInTwoSegment(t *testing.T) {
	t.Parallel()

	r := NewGoModResolver()
	got, err := r.ResolveSource(context.Background(), "gopkg.in/alice/project.v2")
	require.NoError(t, err)
	assert.Equal(t, "repo:github/alice/project", got.URI)
}

// TestGoModResolver_UnknownVanityHostReturnsEmpty covers the "we
// know it's a Go module but we don't know the source" case. Empty
// URI / URL with nil error matches the npm "no source declared"
// shape — callers branch on emptiness, not on errors.
func TestGoModResolver_UnknownVanityHostReturnsEmpty(t *testing.T) {
	t.Parallel()

	r := NewGoModResolver()

	for _, path := range []string{
		"k8s.io/client-go",
		"bitbucket.org/foo/bar",
		"example.com/custom/module",
	} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			got, err := r.ResolveSource(context.Background(), path)
			require.NoError(t, err)
			assert.Empty(t, got.URI, "unknown vanity paths resolve to empty — caller treats like npm no-source")
			assert.Empty(t, got.URL)
			assert.True(t, got.SelfReported,
				"SelfReported stays true even when URI is empty — signals the declared-source contract")
		})
	}
}

// TestGoModResolver_Rejects_Malformed covers the forms we DO error on
// (as opposed to empty-but-ok).
func TestGoModResolver_Rejects_Malformed(t *testing.T) {
	t.Parallel()

	r := NewGoModResolver()
	_, err := r.ResolveSource(context.Background(), "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty module path")
}

// TestGoModResolver_GopkgInEdgeCases covers malformed gopkg.in paths
// which should resolve to empty rather than panic.
func TestGoModResolver_GopkgInEdgeCases(t *testing.T) {
	t.Parallel()

	r := NewGoModResolver()

	// Missing .v version marker — gopkg.in requires one.
	for _, path := range []string{
		"gopkg.in/noversion",         // no .v<N>
		"gopkg.in/bad.vBAD",          // non-numeric version
		"gopkg.in/.v1",               // empty name
		"gopkg.in/user/pkg/extra.v1", // too many slashes in two-segment form
	} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			got, err := r.ResolveSource(context.Background(), path)
			require.NoError(t, err)
			assert.Empty(t, got.URI, "malformed gopkg.in resolves to empty URI")
		})
	}
}

// TestGoModResolver_GithubShortPath_ReturnsEmpty covers the
// github.com/<owner> (missing repo) case — not enough info to
// construct a source.
func TestGoModResolver_GithubShortPath_ReturnsEmpty(t *testing.T) {
	t.Parallel()

	r := NewGoModResolver()
	got, err := r.ResolveSource(context.Background(), "github.com/alecthomas")
	require.NoError(t, err)
	assert.Empty(t, got.URI)
}
