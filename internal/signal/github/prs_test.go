package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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

// TestFetchPullRequest_NotFound guards the 404 → ErrNotFound sentinel
// translation through the PR detail endpoint. The detail call routes
// through c.get, which maps httpx's ErrNotFound onto the package-local
// github.ErrNotFound sentinel; FetchPullRequest then wraps it once more
// with %w, so errors.Is must still reach the sentinel at the call site.
// Callers (the pr-analyzer adapter, pr-scan) branch on this to treat a
// vanished/private PR as a soft signal rather than a hard collection
// failure — a regression that broke the wrap chain (e.g. %v instead of
// %w, or returning the raw status error) would silently turn absence
// into an opaque error and is what this test catches.
func TestFetchPullRequest_NotFound(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/octo/hello/pulls/404", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"Not Found"}`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client := NewClientWithBaseURLAndToken(server.URL, "test-token")

	_, err := client.FetchPullRequest(context.Background(), "octo", "hello", 404)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotFound),
		"404 on the PR detail endpoint must satisfy errors.Is(err, github.ErrNotFound); got %v", err)
}

// TestFetchPullRequest_RateLimit guards the 403 + X-RateLimit-Reset →
// typed *RateLimitError translation through the PR detail endpoint. The
// rateLimitInterceptor wired into every get fires before httpx's default
// status classification, so a rate-limited PR fetch must surface a typed
// error (carrying ResetAt for retry scheduling) and not a generic status
// error. A regression that dropped the interceptor, or mis-classified
// 403 as a plain error, would defeat the errors.As routing that callers
// rely on — this asserts the typed error survives FetchPullRequest's
// %w wrap.
func TestFetchPullRequest_RateLimit(t *testing.T) {
	t.Parallel()

	const reset = "1712700000"
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/octo/hello/pulls/9", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", reset)
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"API rate limit exceeded"}`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client := NewClientWithBaseURLAndToken(server.URL, "test-token")

	_, err := client.FetchPullRequest(context.Background(), "octo", "hello", 9)
	require.Error(t, err)

	var rateLimitErr *RateLimitError
	require.ErrorAs(t, err, &rateLimitErr,
		"403 with X-RateLimit-Reset on a PR endpoint must surface a typed *RateLimitError; got %v", err)
	assert.Equal(t, int64(1712700000), rateLimitErr.ResetAt.Unix(),
		"ResetAt must be parsed from the X-RateLimit-Reset header")
}

// TestFetchPullRequest_MalformedJSON guards the decode-error path on the
// PR detail endpoint: a 200 carrying a truncated body ("{" with no close)
// must return a non-nil error rather than silently yielding a partial /
// zero-valued PullRequest treated as success. json.Unmarshal in httpx's
// GetJSON returns "unexpected end of JSON input" for a truncated object;
// FetchPullRequest wraps that with %w. A regression that ignored the
// decode error (or swallowed it) would hand callers a garbage struct —
// this asserts the error surfaces and that no struct is produced on the
// error return.
func TestFetchPullRequest_MalformedJSON(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/octo/hello/pulls/8", func(w http.ResponseWriter, _ *http.Request) {
		// Truncated object: a decoder that tolerates this would emit a
		// zero/partial PullRequest. The detail decode must fail here.
		fmt.Fprint(w, `{"number": 8, "title": "trunc`)
	})
	// A VALID files endpoint is registered so that a swallowed detail
	// decode error cannot be masked by a downstream files-fetch failure:
	// if the detail decode were (wrongly) tolerated, execution would
	// reach a clean files page and FetchPullRequest would return nil —
	// which this test would then catch. The only legitimate error source
	// is the truncated detail body.
	mux.HandleFunc("/repos/octo/hello/pulls/8/files", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[]`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client := NewClientWithBaseURLAndToken(server.URL, "test-token")

	pr, err := client.FetchPullRequest(context.Background(), "octo", "hello", 8)
	require.Error(t, err, "a truncated PR detail body must not decode to a success")
	assert.Equal(t, PullRequest{}, pr,
		"on a decode error FetchPullRequest must return the zero PullRequest, not a partial struct")
}

// TestFetchPullRequest_MultiPageFiles guards the getAllPages accumulation
// loop end-to-end through FetchPullRequest's real /pulls/N/files call
// site. The server returns two FULL pages (each prPageSize items) then a
// short final page; the loop continues while a page is full and stops on
// the first short page, so FetchPullRequest must accumulate every file
// across all three pages. A regression that returned only the first page,
// or mis-judged the short-page terminator, would silently truncate the
// changed-file set that downstream code-shape analysis depends on.
func TestFetchPullRequest_MultiPageFiles(t *testing.T) {
	t.Parallel()

	const lastPageCount = 4
	totalFiles := prPageSize*2 + lastPageCount

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/octo/hello/pulls/11", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{
			"number": 11,
			"title": "Big change",
			"user": {"login": "alice", "type": "User"},
			"state": "open",
			"changed_files": `+fmt.Sprintf("%d", totalFiles)+`
		}`)
	})
	mux.HandleFunc("/repos/octo/hello/pulls/11/files", func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		// Each item carries a unique filename so accumulation order and
		// completeness are both observable.
		emit := func(start, count int) {
			parts := make([]string, count)
			for i := range count {
				parts[i] = fmt.Sprintf(
					`{"filename":"f%d.go","status":"modified","additions":1,"deletions":0}`,
					start+i)
			}
			fmt.Fprint(w, "["+strings.Join(parts, ",")+"]")
		}
		switch page {
		case "1":
			emit(0, prPageSize) // full page → loop continues
		case "2":
			emit(prPageSize, prPageSize) // full page → loop continues
		case "3":
			emit(prPageSize*2, lastPageCount) // short page → loop stops
		default:
			fmt.Fprint(w, `[]`)
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client := NewClientWithBaseURLAndToken(server.URL, "test-token")

	pr, err := client.FetchPullRequest(context.Background(), "octo", "hello", 11)
	require.NoError(t, err)

	require.Len(t, pr.Files, totalFiles,
		"FetchPullRequest must accumulate files across every page, not just the first")
	// Spot-check the boundary items so a silent reordering or page-drop
	// can't pass on count alone.
	assert.Equal(t, "f0.go", pr.Files[0].Path)
	assert.Equal(t, fmt.Sprintf("f%d.go", prPageSize), pr.Files[prPageSize].Path)
	assert.Equal(t, fmt.Sprintf("f%d.go", totalFiles-1), pr.Files[totalFiles-1].Path)
}
