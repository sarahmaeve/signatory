package pypi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestResolveRepoURL_PriorityOrder pins the project_urls key
// priority. PyPI's project_urls is free-form, so different
// publishers use different keys for the same concept ("Repository"
// vs. "Source" vs. "Source Code"). The lookup walks a fixed order
// so two equivalently-tagged repos always pick the same key, and
// callers downstream get deterministic output.
//
// Each subtest seeds a Project with ONE project_urls key set to
// the canonical github URL and confirms the resolver returns it.
// Run together they exercise the priority list end to end.
func TestResolveRepoURL_PriorityOrder(t *testing.T) {
	t.Parallel()

	canonical := "https://github.com/theskumar/python-dotenv"

	keys := []string{
		"Repository",
		"Source",
		"Source Code",
		"SourceCode",
		"source",
		"Code",
		"GitHub",
		"Repo",
		"Homepage",
	}
	for _, k := range keys {
		t.Run(k, func(t *testing.T) {
			t.Parallel()
			srv := newProjectServer(t, Project{
				Info: Info{
					ProjectURLs: map[string]string{k: canonical},
				},
			})
			defer srv.Close()

			c := NewClientWithBaseURL(srv.URL)
			got, err := c.ResolveRepoURL(context.Background(), "python-dotenv")
			require.NoError(t, err)
			assert.Equal(t, canonical, got,
				"key %q should resolve to canonical github URL", k)
		})
	}
}

// TestResolveRepoURL_PicksHighestPriority confirms that when
// multiple keys point to different repos, the first key in the
// priority list wins. Real-world cause: a project that lists both
// "Repository" (canonical) and "Homepage" (a docs site) should
// resolve to the Repository value.
func TestResolveRepoURL_PicksHighestPriority(t *testing.T) {
	t.Parallel()

	srv := newProjectServer(t, Project{
		Info: Info{
			ProjectURLs: map[string]string{
				"Repository": "https://github.com/theskumar/python-dotenv",
				"Homepage":   "https://github.com/some/other-repo",
				"Source":     "https://github.com/yet/another-repo",
			},
		},
	})
	defer srv.Close()

	c := NewClientWithBaseURL(srv.URL)
	got, err := c.ResolveRepoURL(context.Background(), "python-dotenv")
	require.NoError(t, err)
	assert.Equal(t, "https://github.com/theskumar/python-dotenv", got,
		"Repository key should win over Homepage and Source")
}

// TestResolveRepoURL_HomePageFallback covers the deprecated PEP 621
// info.home_page field. Older PyPI releases populated it instead of
// project_urls; for those, it's the only repo declaration available.
// The lookup falls back to home_page only when no project_urls key
// matches.
func TestResolveRepoURL_HomePageFallback(t *testing.T) {
	t.Parallel()

	srv := newProjectServer(t, Project{
		Info: Info{
			ProjectURLs: nil, // pre-PEP-621 shape: no project_urls
			HomePage:    "https://github.com/legacy/project",
		},
	})
	defer srv.Close()

	c := NewClientWithBaseURL(srv.URL)
	got, err := c.ResolveRepoURL(context.Background(), "legacy")
	require.NoError(t, err)
	assert.Equal(t, "https://github.com/legacy/project", got)
}

// TestResolveRepoURL_HomePageNotPreferred guards the priority of
// home_page being LAST: if any project_urls key resolves, home_page
// is ignored even when it points at github. (A common case is
// home_page = docs site, project_urls.Repository = the actual repo.)
func TestResolveRepoURL_HomePageNotPreferred(t *testing.T) {
	t.Parallel()

	srv := newProjectServer(t, Project{
		Info: Info{
			ProjectURLs: map[string]string{
				"Repository": "https://github.com/correct/repo",
			},
			HomePage: "https://github.com/wrong/repo",
		},
	})
	defer srv.Close()

	c := NewClientWithBaseURL(srv.URL)
	got, err := c.ResolveRepoURL(context.Background(), "p")
	require.NoError(t, err)
	assert.Equal(t, "https://github.com/correct/repo", got,
		"project_urls.Repository must win over info.home_page")
}

// TestResolveRepoURL_NoSourceDeclared is the legitimate empty case:
// publisher declares no repo at all (or declares only non-github
// URLs like a docs site). Returns ("", nil) — caller distinguishes
// "no source declared" from "couldn't reach the registry."
func TestResolveRepoURL_NoSourceDeclared(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		info Info
	}{
		{
			name: "empty info",
			info: Info{},
		},
		{
			name: "only non-github URLs",
			info: Info{
				ProjectURLs: map[string]string{
					"Homepage":      "https://example.com/",
					"Documentation": "https://docs.example.com/",
				},
				HomePage: "https://example.com/",
			},
		},
		{
			name: "all keys empty strings",
			info: Info{
				ProjectURLs: map[string]string{
					"Repository": "",
					"Source":     "",
				},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := newProjectServer(t, Project{Info: tc.info})
			defer srv.Close()

			c := NewClientWithBaseURL(srv.URL)
			got, err := c.ResolveRepoURL(context.Background(), "anything")
			require.NoError(t, err, "no-source is a legitimate case, not an error")
			assert.Empty(t, got, "expected empty string, got %q", got)
		})
	}
}

// TestResolveRepoURL_PropagatesFetchError pins that fetch failures
// (network, 404, body cap) reach the caller as errors — empty string
// is reserved exclusively for "no source declared."
func TestResolveRepoURL_PropagatesFetchError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL(srv.URL)
	_, err := c.ResolveRepoURL(context.Background(), "missing")
	assert.Error(t, err, "404 must surface as error, not empty string")
}

// newProjectServer builds an httptest server that responds to the
// /pypi/<name>/json endpoint with a JSON-encoded Project. Test
// helper to keep the per-test setup compact.
func newProjectServer(t *testing.T, p Project) *httptest.Server {
	t.Helper()
	body, err := json.Marshal(p)
	require.NoError(t, err)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
}
