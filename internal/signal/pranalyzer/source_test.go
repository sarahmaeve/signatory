package pranalyzer

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sarahmaeve/pr-analyzer/analyzer"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/signal/github"
)

// TestGitHubSource drives the PRSource adapter end-to-end through
// signatory's github.Client against an httptest server, proving the
// adapter wires analyzer.Analyze's per-PR fetch onto our hardened HTTP
// path and maps the github payload into analyzer's types.
func TestGitHubSource(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/octo/hello/pulls", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"number":7},{"number":5}]`)
	})
	mux.HandleFunc("/repos/octo/hello/pulls/7", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{
			"number": 7,
			"user": {"login": "alice"},
			"html_url": "https://github.com/octo/hello/pull/7",
			"additions": 10,
			"deletions": 2,
			"changed_files": 1,
			"author_association": "CONTRIBUTOR",
			"base": {"ref": "main", "sha": "base0000000000000000000000000000000000000"},
			"head": {"ref": "feature", "sha": "head1111111111111111111111111111111111111"}
		}`)
	})
	mux.HandleFunc("/repos/octo/hello/pulls/7/files", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"filename": "a.go", "status": "modified", "additions": 10, "deletions": 2}]`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	src := newGitHubSource(github.NewClientWithBaseURL(server.URL))

	refs, err := src.ListOpenPRs(context.Background(), "octo", "hello")
	require.NoError(t, err)
	require.Len(t, refs, 2)
	assert.Equal(t, analyzer.PRRef{Owner: "octo", Repo: "hello", Number: 7}, refs[0])
	assert.Equal(t, analyzer.PRRef{Owner: "octo", Repo: "hello", Number: 5}, refs[1])

	pr, err := src.FetchPR(context.Background(), refs[0])
	require.NoError(t, err)
	assert.Equal(t, analyzer.PRRef{Owner: "octo", Repo: "hello", Number: 7}, pr.Ref)
	assert.Equal(t, "alice", pr.Author)
	assert.Equal(t, "https://github.com/octo/hello/pull/7", pr.URL)
	assert.Equal(t, 10, pr.Additions)
	assert.Equal(t, 2, pr.Deletions)
	assert.Equal(t, "CONTRIBUTOR", pr.AuthorAssociation)
	assert.Equal(t, "head1111111111111111111111111111111111111", pr.HeadSHA)
	assert.Equal(t, "base0000000000000000000000000000000000000", pr.BaseSHA)
	require.Len(t, pr.Files, 1)
	assert.Equal(t, "a.go", pr.Files[0].Path)
	assert.Equal(t, "modified", pr.Files[0].Status)
}

// ghSource must satisfy the interface analyzer.Analyze consumes.
var _ analyzer.PRSource = (*ghSource)(nil)
