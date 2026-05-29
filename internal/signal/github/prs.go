package github

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// PullRequest is the subset of GitHub's pull-request detail response
// that signatory consumes. The field set mirrors what the pr-analyzer
// code-shape / engineer-profile collectors read; unmodeled response
// fields are decoded leniently and dropped.
//
// Exported (unlike the repo / commit / user payload types in client.go)
// because the pr-analyzer adapter in internal/signal/pranalyzer maps
// these values into pr-analyzer's own analyzer.PR type and so must be
// able to name them.
type PullRequest struct {
	Number            int
	Title             string
	Author            string
	URL               string
	State             string
	Draft             bool
	BaseRef           string
	HeadRef           string
	BaseSHA           string
	HeadSHA           string
	Additions         int
	Deletions         int
	ChangedFiles      int
	Labels            []string
	AuthorAssociation string
	CreatedAt         time.Time
	UpdatedAt         time.Time
	Files             []PullRequestFile
}

// PullRequestFile is one entry from a PR's file list.
type PullRequestFile struct {
	Path      string
	Status    string
	Additions int
	Deletions int
}

const (
	// prPageSize is the per_page value for paginated PR endpoints.
	// GitHub caps per_page at 100 for these collections.
	prPageSize = 100
)

// prDetailPayload is the wire shape of GitHub's PR detail response.
type prDetailPayload struct {
	Number            int           `json:"number"`
	Title             string        `json:"title"`
	HTMLURL           string        `json:"html_url"`
	State             string        `json:"state"`
	Draft             bool          `json:"draft"`
	User              prUserPayload `json:"user"`
	Base              prRefPayload  `json:"base"`
	Head              prRefPayload  `json:"head"`
	Additions         int           `json:"additions"`
	Deletions         int           `json:"deletions"`
	ChangedFiles      int           `json:"changed_files"`
	Labels            []prLabel     `json:"labels"`
	AuthorAssociation string        `json:"author_association"`
	CreatedAt         time.Time     `json:"created_at"`
	UpdatedAt         time.Time     `json:"updated_at"`
}

type prUserPayload struct {
	Login string `json:"login"`
}

type prRefPayload struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

type prLabel struct {
	Name string `json:"name"`
}

type prFilePayload struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
}

// prListPayload is the trimmed shape of one element in the PR listing
// response. The listing endpoint omits additions / deletions /
// changed_files / files; only the number is load-bearing here, and
// callers follow up with FetchPullRequest per number.
type prListPayload struct {
	Number int `json:"number"`
}

// IsGitHubHost reports whether rawURL points at github.com. It is the
// exported form of the collector's internal host gate (isGitHubHost),
// used by sibling collectors (e.g. internal/signal/pranalyzer) that must
// self-gate to GitHub-hosted entities before issuing GitHub API calls.
func IsGitHubHost(rawURL string) bool {
	return isGitHubHost(rawURL)
}

// FetchPullRequest fetches a single PR's detail plus its full (paginated)
// file list. Both calls route through c.get, so token redaction and the
// rate-limit interceptor apply.
func (c *Client) FetchPullRequest(ctx context.Context, owner, repoName string, number int) (PullRequest, error) {
	var p prDetailPayload
	if err := c.get(ctx, fmt.Sprintf("/repos/%s/%s/pulls/%d", owner, repoName, number), &p); err != nil {
		return PullRequest{}, fmt.Errorf("fetch PR detail %s/%s#%d: %w", owner, repoName, number, err)
	}

	rawFiles, err := getAllPages[prFilePayload](ctx,
		c, fmt.Sprintf("/repos/%s/%s/pulls/%d/files", owner, repoName, number), prPageSize)
	if err != nil {
		return PullRequest{}, fmt.Errorf("fetch PR files %s/%s#%d: %w", owner, repoName, number, err)
	}

	labels := make([]string, len(p.Labels))
	for i, l := range p.Labels {
		labels[i] = l.Name
	}
	files := make([]PullRequestFile, len(rawFiles))
	for i, f := range rawFiles {
		files[i] = PullRequestFile{
			Path:      f.Filename,
			Status:    f.Status,
			Additions: f.Additions,
			Deletions: f.Deletions,
		}
	}

	return PullRequest{
		Number:            p.Number,
		Title:             p.Title,
		Author:            p.User.Login,
		URL:               p.HTMLURL,
		State:             p.State,
		Draft:             p.Draft,
		BaseRef:           p.Base.Ref,
		HeadRef:           p.Head.Ref,
		BaseSHA:           p.Base.SHA,
		HeadSHA:           p.Head.SHA,
		Additions:         p.Additions,
		Deletions:         p.Deletions,
		ChangedFiles:      p.ChangedFiles,
		Labels:            labels,
		AuthorAssociation: p.AuthorAssociation,
		CreatedAt:         p.CreatedAt,
		UpdatedAt:         p.UpdatedAt,
		Files:             files,
	}, nil
}

// ListOpenPullRequestNumbers returns the numbers of every open PR for
// owner/repo, in the order GitHub lists them (newest-first by default).
// Pagination is followed to completion so the count is not silently
// truncated; per-PR cost bounding is the caller's responsibility.
func (c *Client) ListOpenPullRequestNumbers(ctx context.Context, owner, repoName string) ([]int, error) {
	items, err := getAllPages[prListPayload](ctx,
		c, fmt.Sprintf("/repos/%s/%s/pulls?state=open", owner, repoName), prPageSize)
	if err != nil {
		return nil, fmt.Errorf("list open PRs %s/%s: %w", owner, repoName, err)
	}
	numbers := make([]int, len(items))
	for i, it := range items {
		numbers[i] = it.Number
	}
	return numbers, nil
}

// getAllPages walks GitHub's page=N pagination for a collection endpoint,
// accumulating decoded items across pages until a page returns fewer than
// perPage items. basePath may already carry query parameters; per_page
// and page are appended with the correct separator. This sidesteps the
// absolute-URL / same-origin handling that Link-header following would
// require — every request is the client's own baseURL + a controlled path.
func getAllPages[T any](ctx context.Context, c *Client, basePath string, perPage int) ([]T, error) {
	sep := "?"
	if strings.Contains(basePath, "?") {
		sep = "&"
	}
	var all []T
	for page := 1; ; page++ {
		var batch []T
		path := fmt.Sprintf("%s%sper_page=%d&page=%d", basePath, sep, perPage, page)
		if err := c.get(ctx, path, &batch); err != nil {
			return nil, err
		}
		all = append(all, batch...)
		if len(batch) < perPage {
			return all, nil
		}
	}
}
