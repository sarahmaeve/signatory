package github

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pagedItem is a throwaway shape for exercising getAllPages directly.
type pagedItem struct {
	N int `json:"n"`
}

// TestGetAllPages drives the page=N accumulation loop with a tiny
// per-page size so multi-page behavior is exercised without a
// hundred-element fixture. The loop must follow pages until a short
// page (fewer than perPage items) signals the end.
func TestGetAllPages(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/things", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("page") {
		case "1":
			fmt.Fprint(w, `[{"n":1},{"n":2}]`)
		case "2":
			fmt.Fprint(w, `[{"n":3},{"n":4}]`)
		case "3":
			fmt.Fprint(w, `[{"n":5}]`) // short page → loop stops
		default:
			fmt.Fprint(w, `[]`)
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client := NewClientWithBaseURLAndToken(server.URL, "test-token")

	items, err := getAllPages[pagedItem](context.Background(), client, "/things", 2)
	require.NoError(t, err)
	got := make([]int, len(items))
	for i, it := range items {
		got[i] = it.N
	}
	assert.Equal(t, []int{1, 2, 3, 4, 5}, got)
}

func TestFetchPullRequest(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/octo/hello/pulls/7", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{
			"number": 7,
			"title": "Add widget",
			"user": {"login": "alice", "type": "User"},
			"html_url": "https://github.com/octo/hello/pull/7",
			"state": "open",
			"draft": false,
			"base": {"ref": "main", "sha": "base0000000000000000000000000000000000000"},
			"head": {"ref": "feature", "sha": "head1111111111111111111111111111111111111"},
			"additions": 187,
			"deletions": 7,
			"changed_files": 2,
			"labels": [{"name": "bug"}, {"name": "area/core"}],
			"author_association": "CONTRIBUTOR",
			"created_at": "2026-01-02T03:04:05Z",
			"updated_at": "2026-01-03T03:04:05Z"
		}`)
	})
	mux.HandleFunc("/repos/octo/hello/pulls/7/files", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[
			{"filename": "widget.go", "status": "modified", "additions": 180, "deletions": 5},
			{"filename": "widget_test.go", "status": "added", "additions": 7, "deletions": 2}
		]`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client := NewClientWithBaseURLAndToken(server.URL, "test-token")

	pr, err := client.FetchPullRequest(context.Background(), "octo", "hello", 7)
	require.NoError(t, err)

	assert.Equal(t, 7, pr.Number)
	assert.Equal(t, "Add widget", pr.Title)
	assert.Equal(t, "alice", pr.Author)
	assert.Equal(t, "https://github.com/octo/hello/pull/7", pr.URL)
	assert.Equal(t, "open", pr.State)
	assert.False(t, pr.Draft)
	assert.Equal(t, "main", pr.BaseRef)
	assert.Equal(t, "feature", pr.HeadRef)
	assert.Equal(t, 187, pr.Additions)
	assert.Equal(t, 7, pr.Deletions)
	assert.Equal(t, 2, pr.ChangedFiles)
	assert.Equal(t, []string{"bug", "area/core"}, pr.Labels)
	assert.Equal(t, "CONTRIBUTOR", pr.AuthorAssociation)
	assert.Equal(t, "head1111111111111111111111111111111111111", pr.HeadSHA)
	assert.Equal(t, "base0000000000000000000000000000000000000", pr.BaseSHA)
	assert.Equal(t, "User", pr.AuthorType)
	require.Len(t, pr.Files, 2)
	assert.Equal(t, "widget.go", pr.Files[0].Path)
	assert.Equal(t, "modified", pr.Files[0].Status)
	assert.Equal(t, "widget_test.go", pr.Files[1].Path)
}

func TestFetchUserProfile(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/users/octocat", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"login":"octocat","name":"The Octocat","company":"@github","type":"User",
			"created_at":"2011-01-25T18:44:36Z","public_repos":8,"followers":9999}`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client := NewClientWithBaseURLAndToken(server.URL, "test-token")

	p, err := client.FetchUserProfile(context.Background(), "octocat")
	require.NoError(t, err)
	assert.Equal(t, "octocat", p.Login)
	assert.Equal(t, "User", p.Type)
	assert.Equal(t, 8, p.PublicRepos)
	assert.Equal(t, 9999, p.Followers)
	assert.Equal(t, 2011, p.CreatedAt.Year())
}

func TestListOpenPullRequestNumbers(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/octo/hello/pulls", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "open", r.URL.Query().Get("state"))
		// Single short page (newest-first): loop terminates.
		fmt.Fprint(w, `[{"number": 7}, {"number": 5}, {"number": 3}]`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client := NewClientWithBaseURLAndToken(server.URL, "test-token")

	numbers, err := client.ListOpenPullRequestNumbers(context.Background(), "octo", "hello")
	require.NoError(t, err)
	assert.Equal(t, []int{7, 5, 3}, numbers)
}
