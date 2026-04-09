package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var base64Std = base64.StdEncoding

// Client is a minimal GitHub REST API client.
type Client struct {
	httpClient *http.Client
	token      string
	baseURL    string
}

// NewClient creates a GitHub API client. If token is empty, requests
// are unauthenticated (60 req/hr limit vs. 5000 authenticated).
// The per-request timeout is generous (60s) because GitHub API can be
// slow under load, and a timeout should not collapse the entire
// collection — partial results with absence records are preferable
// to a total failure.
func NewClient(token string) *Client {
	return &Client{
		httpClient: &http.Client{Timeout: 60 * time.Second},
		token:      token,
		baseURL:    "https://api.github.com",
	}
}

// repo represents GitHub repository metadata.
type repo struct {
	Name            string    `json:"name"`
	FullName        string    `json:"full_name"`
	Description     string    `json:"description"`
	Owner           repoOwner `json:"owner"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	PushedAt        time.Time `json:"pushed_at"`
	StargazersCount int       `json:"stargazers_count"`
	ForksCount      int       `json:"forks_count"`
	OpenIssuesCount int       `json:"open_issues_count"`
	Archived        bool      `json:"archived"`
	License         *license  `json:"license"`
}

type repoOwner struct {
	Login string `json:"login"`
	Type  string `json:"type"` // "User" or "Organization"
}

type license struct {
	SPDXID string `json:"spdx_id"`
}

// contributor represents a GitHub contributor.
type contributor struct {
	Login         string `json:"login"`
	Contributions int    `json:"contributions"`
}

// commit represents a GitHub commit.
type commit struct {
	SHA    string     `json:"sha"`
	Commit commitData `json:"commit"`
}

type commitData struct {
	Author       commitPerson `json:"author"`
	Message      string       `json:"message"`
	Verification verification `json:"verification"`
}

type commitPerson struct {
	Name  string    `json:"name"`
	Email string    `json:"email"`
	Date  time.Time `json:"date"`
}

type verification struct {
	Verified bool `json:"verified"`
}

// tag represents a GitHub tag.
type tag struct {
	Name string `json:"name"`
}

// user represents a GitHub user profile.
type user struct {
	Login       string    `json:"login"`
	Name        string    `json:"name"`
	Company     string    `json:"company"`
	CreatedAt   time.Time `json:"created_at"`
	PublicRepos int       `json:"public_repos"`
	Followers   int       `json:"followers"`
	Type        string    `json:"type"`
}

// RateLimitError is returned when the API rate limit is exceeded.
type RateLimitError struct {
	ResetAt time.Time
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("GitHub API rate limit exceeded, resets at %s", e.ResetAt.Format(time.RFC3339))
}

// get performs a GET request to the GitHub API.
func (c *Client) get(ctx context.Context, path string, result interface{}) error {
	url := c.baseURL + path

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Accept", "application/vnd.github.v3+json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		resetAt := parseRateLimitReset(resp.Header.Get("X-RateLimit-Reset"))
		return &RateLimitError{ResetAt: resetAt}
	}

	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("not found: %s", path)
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("GitHub API error %d: %s", resp.StatusCode, string(body))
	}

	if result != nil {
		if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}

	return nil
}

// getWithLinkHeader performs a GET and returns the Link header for pagination.
func (c *Client) getWithLinkHeader(ctx context.Context, path string, result interface{}) (string, error) {
	url := c.baseURL + path

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Accept", "application/vnd.github.v3+json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		resetAt := parseRateLimitReset(resp.Header.Get("X-RateLimit-Reset"))
		return "", &RateLimitError{ResetAt: resetAt}
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("GitHub API error %d: %s", resp.StatusCode, string(body))
	}

	if result != nil {
		if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
			return "", fmt.Errorf("decode response: %w", err)
		}
	}

	return resp.Header.Get("Link"), nil
}

// GetRepo fetches repository metadata.
func (c *Client) GetRepo(ctx context.Context, owner, repoName string) (*repo, error) {
	var r repo
	if err := c.get(ctx, fmt.Sprintf("/repos/%s/%s", owner, repoName), &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// GetContributors fetches the top contributors.
func (c *Client) GetContributors(ctx context.Context, owner, repoName string) ([]contributor, error) {
	var contributors []contributor
	err := c.get(ctx, fmt.Sprintf("/repos/%s/%s/contributors?per_page=10", owner, repoName), &contributors)
	return contributors, err
}

// GetRecentCommits fetches the most recent commits.
func (c *Client) GetRecentCommits(ctx context.Context, owner, repoName string) ([]commit, error) {
	var commits []commit
	err := c.get(ctx, fmt.Sprintf("/repos/%s/%s/commits?per_page=10", owner, repoName), &commits)
	return commits, err
}

// GetTotalCommitCount estimates the total commit count using pagination.
func (c *Client) GetTotalCommitCount(ctx context.Context, owner, repoName string) (int, error) {
	linkHeader, err := c.getWithLinkHeader(ctx,
		fmt.Sprintf("/repos/%s/%s/commits?per_page=1", owner, repoName), nil)
	if err != nil {
		return 0, err
	}
	return parseTotalFromLink(linkHeader), nil
}

// GetTags fetches the most recent tags.
func (c *Client) GetTags(ctx context.Context, owner, repoName string) ([]tag, error) {
	var tags []tag
	err := c.get(ctx, fmt.Sprintf("/repos/%s/%s/tags?per_page=10", owner, repoName), &tags)
	return tags, err
}

// GetUser fetches a user or org profile.
func (c *Client) GetUser(ctx context.Context, username string) (*user, error) {
	var u user
	if err := c.get(ctx, fmt.Sprintf("/users/%s", username), &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// searchResult represents a GitHub code search response.
type searchResult struct {
	TotalCount int `json:"total_count"`
}

// GetGoModRefCount searches for how many go.mod files reference the given module.
func (c *Client) GetGoModRefCount(ctx context.Context, modulePath string) (int, error) {
	var result searchResult
	// The search API uses a different path and rate limit pool.
	err := c.get(ctx, fmt.Sprintf("/search/code?q=%s+filename:go.mod&per_page=1", modulePath), &result)
	if err != nil {
		return 0, err
	}
	return result.TotalCount, nil
}

// repoContent represents a file or directory entry from the contents API.
type repoContent struct {
	Name string `json:"name"`
	Type string `json:"type"` // "file" or "dir"
	Path string `json:"path"`
}

// GetDirectoryContents fetches the contents of a directory in a repo.
// Returns nil without error if the path does not exist.
func (c *Client) GetDirectoryContents(ctx context.Context, owner, repoName, path string) ([]repoContent, error) {
	var contents []repoContent
	err := c.get(ctx, fmt.Sprintf("/repos/%s/%s/contents/%s", owner, repoName, path), &contents)
	if err != nil && strings.Contains(err.Error(), "not found") {
		return nil, nil
	}
	return contents, err
}

// GetFileContent fetches the raw content of a single file (base64-encoded).
type fileContent struct {
	Content  string `json:"content"`
	Encoding string `json:"encoding"`
}

// GetFileRaw fetches a file's content from a repo. Returns nil without error if not found.
func (c *Client) GetFileRaw(ctx context.Context, owner, repoName, path string) ([]byte, error) {
	var fc fileContent
	err := c.get(ctx, fmt.Sprintf("/repos/%s/%s/contents/%s", owner, repoName, path), &fc)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			return nil, nil
		}
		return nil, err
	}
	if fc.Encoding != "base64" {
		return nil, fmt.Errorf("unexpected encoding: %s", fc.Encoding)
	}

	// GitHub base64 content has newlines — strip them before decoding.
	cleaned := strings.ReplaceAll(fc.Content, "\n", "")
	data, err := base64Std.DecodeString(cleaned)
	if err != nil {
		return nil, fmt.Errorf("decode base64 content: %w", err)
	}
	return data, nil
}

// parseRateLimitReset parses the X-RateLimit-Reset header.
func parseRateLimitReset(header string) time.Time {
	if header == "" {
		return time.Now().Add(time.Minute)
	}
	unix, err := strconv.ParseInt(header, 10, 64)
	if err != nil {
		return time.Now().Add(time.Minute)
	}
	return time.Unix(unix, 0)
}

// parseTotalFromLink extracts the last page number from a Link header.
// Link: <...?page=2>; rel="next", <...?page=467>; rel="last"
func parseTotalFromLink(link string) int {
	if link == "" {
		return 1
	}
	for _, part := range strings.Split(link, ",") {
		if strings.Contains(part, `rel="last"`) {
			// Find the last "page=" in the URL to avoid matching "per_page=".
			start := strings.LastIndex(part, "page=")
			if start == -1 {
				continue
			}
			start += 5
			end := strings.IndexAny(part[start:], ">&>;")
			if end == -1 {
				end = len(part[start:])
			}
			n, err := strconv.Atoi(part[start : start+end])
			if err != nil {
				return 0
			}
			return n
		}
	}
	return 1
}

// ParseRepoURL extracts owner and repo from various input formats:
//   - "owner/repo"
//   - "github.com/owner/repo"
//   - "https://github.com/owner/repo"
func ParseRepoURL(input string) (owner, repoName string, err error) {
	input = strings.TrimSpace(input)
	input = strings.TrimSuffix(input, ".git")
	input = strings.TrimPrefix(input, "https://")
	input = strings.TrimPrefix(input, "http://")
	input = strings.TrimPrefix(input, "github.com/")

	parts := strings.SplitN(input, "/", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("cannot parse GitHub repo from %q: expected owner/repo", input)
	}
	return parts[0], parts[1], nil
}
