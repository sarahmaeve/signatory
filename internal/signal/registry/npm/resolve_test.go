package npm

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNormalizeDeclaredRepoURL covers every shape npm-registry
// packages are known to emit in practice plus the forms that must
// be rejected. Run under t.Parallel with subtests named after the
// input for debuggability.
func TestNormalizeDeclaredRepoURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		// Accepted github forms → normalized https URL.
		{"git+https with .git", "git+https://github.com/expressjs/express.git", "https://github.com/expressjs/express"},
		{"https with .git", "https://github.com/expressjs/express.git", "https://github.com/expressjs/express"},
		{"https no .git", "https://github.com/expressjs/express", "https://github.com/expressjs/express"},
		{"github shorthand", "github:expressjs/express", "https://github.com/expressjs/express"},
		{"github shorthand with branch fragment", "github:expressjs/express#main", "https://github.com/expressjs/express"},
		{"ssh with git+ prefix", "git+ssh://git@github.com/expressjs/express.git", "https://github.com/expressjs/express"},
		{"ssh alone", "ssh://git@github.com/expressjs/express.git", "https://github.com/expressjs/express"},
		{"git@ SCP form", "git@github.com:expressjs/express.git", "https://github.com/expressjs/express"},
		{"scoped repo", "git+https://github.com/types/node.git", "https://github.com/types/node"},
		// git:// on github.com: the scheme is plaintext but the host
		// anchors the identity, and we never actually clone over git://
		// — downstream collectors hit https. Rewriting here recovers
		// the long tail of older packages (e.g., image-size) that set
		// repository.url once a decade ago and never updated it.
		{"git:// github upgraded to https", "git://github.com/image-size/image-size.git", "https://github.com/image-size/image-size"},
		{"git:// github with fragment", "git://github.com/foo/bar.git#main", "https://github.com/foo/bar"},

		// Multi-forge sources: codeberg and gitlab now resolve as
		// first-classed forges. npm packages can declare
		// repository.url against either and produce a clone-able
		// downstream URL.
		{"codeberg https", "https://codeberg.org/forgejo/forgejo.git",
			"https://codeberg.org/forgejo/forgejo"},
		{"gitlab https", "https://gitlab.com/foo/bar.git",
			"https://gitlab.com/foo/bar"},

		// Rejected forms → empty string.
		{"empty", "", ""},
		{"whitespace only", "   ", ""},
		{"bitbucket host", "https://bitbucket.org/foo/bar.git", ""},
		// git:// on a non-github host stays refused — the upgrade
		// trick only works because we know github serves https on the
		// same identity. We can't make that promise for arbitrary
		// hosts (including codeberg/gitlab — adding it would require
		// per-forge plaintext-to-https policy).
		{"git:// gitlab still rejected", "git://gitlab.com/foo/bar.git", ""},
		{"git:// codeberg still rejected", "git://codeberg.org/foo/bar.git", ""},
		{"unrecognized scheme", "svn+ssh://example.com/foo", ""},
		{"garbage", "not a url at all", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeDeclaredRepoURL(tc.in)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestClient_ResolveRepoURL_HappyPath exercises the provider-layer
// API the analyze orchestrator calls: one registry fetch, returns a
// normalized clone URL.
func TestClient_ResolveRepoURL_HappyPath(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"name":"express","repository":{"type":"git","url":"git+https://github.com/expressjs/express.git"}}`)
	}))
	defer srv.Close()

	url, err := NewClientWithBaseURL(srv.URL).ResolveRepoURL(context.Background(), "express")
	require.NoError(t, err)
	assert.Equal(t, "https://github.com/expressjs/express", url)
}

// TestClient_ResolveRepoURL_NoRepoDeclared returns empty string
// (not error) when the registry response lacks a repository field.
// The caller treats empty-string as "no github-side collection
// should run for this entity," distinct from "couldn't reach the
// registry."
func TestClient_ResolveRepoURL_NoRepoDeclared(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"name":"orphan","dist-tags":{"latest":"1.0.0"}}`)
	}))
	defer srv.Close()

	url, err := NewClientWithBaseURL(srv.URL).ResolveRepoURL(context.Background(), "orphan")
	require.NoError(t, err, "missing repository is not an error — it's an absence")
	assert.Empty(t, url)
}

// TestClient_ResolveRepoURL_GitLabHost pins multi-forge resolution
// at the provider layer: an npm package declaring repository.url at
// gitlab.com produces the canonical https URL, the same shape any
// downstream collector that opens a clone (the git collector,
// repofiles, exfilwatch) operates against. Pre-multi-forge this
// returned empty.
func TestClient_ResolveRepoURL_GitLabHost(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"name":"gl","repository":"https://gitlab.com/foo/bar.git"}`)
	}))
	defer srv.Close()

	url, err := NewClientWithBaseURL(srv.URL).ResolveRepoURL(context.Background(), "gl")
	require.NoError(t, err)
	assert.Equal(t, "https://gitlab.com/foo/bar", url)
}

// TestClient_ResolveRepoURL_CodebergHost — same shape for Codeberg.
func TestClient_ResolveRepoURL_CodebergHost(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"name":"cb","repository":"https://codeberg.org/forgejo/forgejo.git"}`)
	}))
	defer srv.Close()

	url, err := NewClientWithBaseURL(srv.URL).ResolveRepoURL(context.Background(), "cb")
	require.NoError(t, err)
	assert.Equal(t, "https://codeberg.org/forgejo/forgejo", url)
}

// TestClient_ResolveRepoURL_UnsupportedForge pins that forges NOT
// yet first-classed still produce empty URL.
func TestClient_ResolveRepoURL_UnsupportedForge(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"name":"bb","repository":"https://bitbucket.org/team/repo.git"}`)
	}))
	defer srv.Close()

	url, err := NewClientWithBaseURL(srv.URL).ResolveRepoURL(context.Background(), "bb")
	require.NoError(t, err)
	assert.Empty(t, url,
		"unsupported forges (bitbucket, self-hosted) should produce empty URL")
}

// TestClient_ResolveRepoURL_UpstreamError propagates the client's
// error classes upward unchanged; the caller is responsible for
// deciding whether to log, absence-record, or surface to the
// operator.
func TestClient_ResolveRepoURL_UpstreamError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := NewClientWithBaseURL(srv.URL).ResolveRepoURL(context.Background(), "nope")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound)
}
