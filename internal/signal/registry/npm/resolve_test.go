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

		// Rejected forms → empty string.
		{"empty", "", ""},
		{"whitespace only", "   ", ""},
		{"gitlab host", "https://gitlab.com/foo/bar.git", ""},
		{"bitbucket host", "https://bitbucket.org/foo/bar.git", ""},
		{"git:// plaintext protocol", "git://github.com/foo/bar.git", ""},
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

// TestClient_ResolveRepoURL_NonGithubHost returns empty string: the
// github + git collectors only know how to talk to github, so a
// gitlab-hosted package has no downstream value at v0.1.
func TestClient_ResolveRepoURL_NonGithubHost(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"name":"gl","repository":"https://gitlab.com/foo/bar.git"}`)
	}))
	defer srv.Close()

	url, err := NewClientWithBaseURL(srv.URL).ResolveRepoURL(context.Background(), "gl")
	require.NoError(t, err)
	assert.Empty(t, url,
		"non-github host should produce empty URL, not trigger github collector")
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
