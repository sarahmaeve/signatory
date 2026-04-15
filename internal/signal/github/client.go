package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// validGitHubName matches valid GitHub owner and repo names.
// GitHub allows alphanumeric, hyphens, dots, and underscores.
var validGitHubName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// validContentsPath constrains the `path` parameter of
// GetDirectoryContents and GetFileRaw to a safe shape. The path is
// concatenated directly into the GitHub contents API URL, so any
// user-controlled bytes become URL/query/fragment/path-traversal
// injection vectors. Today's callers all pass hardcoded constants
// (".github/workflows", "go.mod", etc.) so the bug is latent — but
// the function signature doesn't enforce that, and the next caller
// forwarding user input would create the bug.
//
// Allowed: A-Z, a-z, 0-9, dot, underscore, slash, hyphen.
var validContentsPath = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// maxContentsPathLength bounds the path length. GitHub contents paths
// in real-world use are well under this; real ones are typically under
// 50 chars. 256 is generous slack to avoid breaking unusual deep paths.
const maxContentsPathLength = 256

// validateContentsPath checks that path is safe to substitute into a
// GitHub contents API URL. Returns nil for valid paths, an error
// describing the violation otherwise. Issue #90.
func validateContentsPath(path string) error {
	if path == "" {
		return fmt.Errorf("contents path is empty")
	}
	if len(path) > maxContentsPathLength {
		return fmt.Errorf("contents path exceeds maximum length of %d bytes (got %d)",
			maxContentsPathLength, len(path))
	}
	if !validContentsPath.MatchString(path) {
		return fmt.Errorf("contents path %q contains disallowed characters (allowed: A-Z a-z 0-9 . _ / -)", path)
	}
	if strings.HasPrefix(path, "/") {
		return fmt.Errorf("contents path %q must not start with / (paths are repo-relative)", path)
	}
	if strings.HasSuffix(path, "/") {
		return fmt.Errorf("contents path %q must not end with /", path)
	}
	if strings.Contains(path, "..") {
		return fmt.Errorf("contents path %q must not contain '..' (path traversal)", path)
	}
	if strings.Contains(path, "//") {
		return fmt.Errorf("contents path %q must not contain '//' (empty path segment)", path)
	}
	return nil
}

var base64Std = base64.StdEncoding

// maxResponseSize limits the maximum response body size to prevent OOM
// from malicious or broken API servers. 10MB is generous for any
// legitimate GitHub API response.
const maxResponseSize = 10 * 1024 * 1024 // 10MB

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
		httpClient: &http.Client{
			Timeout:       60 * time.Second,
			CheckRedirect: checkRedirect,
		},
		token:   token,
		baseURL: "https://api.github.com",
	}
}

// checkRedirect is the http.Client redirect policy used by NewClient
// and the security tests. It enforces three invariants in order:
//
//  1. Refuse any redirect target that is not HTTPS (#89). Scheme
//     downgrade has no legitimate reason in the GitHub API context —
//     continuing would either leak the bearer token over plaintext
//     (the documented bug) or produce an unauthenticated request
//     that masks the misconfiguration. Refusing the redirect makes
//     the failure loud rather than silent.
//  2. Strip the Authorization header on cross-origin redirects (still
//     HTTPS at this point because of rule 1). The bearer token must
//     never reach an external host even over HTTPS — the token grants
//     access to api.github.com specifically.
//  3. Limit redirect chains to <10 hops.
func checkRedirect(req *http.Request, via []*http.Request) error {
	// Rule 1: refuse any non-HTTPS redirect target.
	if req.URL.Scheme != "https" {
		return fmt.Errorf("refusing redirect to non-HTTPS URL %s (token leak prevention)", req.URL.Redacted())
	}
	// Rule 2: strip Authorization on cross-origin redirects.
	if len(via) > 0 && req.URL.Host != via[0].URL.Host {
		req.Header.Del("Authorization")
	}
	// Rule 3: bound the redirect chain.
	if len(via) >= 10 {
		return fmt.Errorf("too many redirects")
	}
	return nil
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
	// Language is GitHub's guess at the primary language, derived from
	// file-extension frequency across the tree. Useful as a hint for
	// picking a language-specific analyst-prompt variant (e.g.,
	// Python-flavored vs. Go-flavored security review). Empty when the
	// repo is empty or contains only languages GitHub doesn't track.
	Language string `json:"language"`
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

	// Limit response body size to prevent OOM from malicious responses.
	limitedBody := io.LimitReader(resp.Body, maxResponseSize+1)

	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		resetAt := parseRateLimitReset(resp.Header.Get("X-RateLimit-Reset"))
		return &RateLimitError{ResetAt: resetAt}
	}

	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("not found: %s", path)
	}

	if resp.StatusCode != http.StatusOK {
		// Issue #93: do NOT include the response body in the error.
		// The body is attacker-influenceable bytes (a malicious or
		// compromised upstream can echo secrets, internal IPs, email
		// addresses, or controlled tokens) and the error propagates
		// up to stderr via main.go's Fprintln, where it lands in CI
		// logs, SIEM ingest, and LLM agent transcripts that treat
		// stderr as ground truth. Drop the body entirely; the status
		// code is the only piece a caller actually needs to classify.
		return fmt.Errorf("GitHub API returned status %d", resp.StatusCode)
	}

	if result != nil {
		body, err := io.ReadAll(limitedBody)
		if err != nil {
			return fmt.Errorf("read response: %w", err)
		}
		if int64(len(body)) > maxResponseSize {
			return fmt.Errorf("response too large: %d bytes exceeds %d byte limit", len(body), maxResponseSize)
		}
		if err := json.Unmarshal(body, result); err != nil {
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

	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("not found: %s", path)
	}

	if resp.StatusCode != http.StatusOK {
		// Issue #93: drop the response body — see client.get above
		// for the rationale.
		return "", fmt.Errorf("GitHub API returned status %d", resp.StatusCode)
	}

	if result != nil {
		limitedBody := io.LimitReader(resp.Body, maxResponseSize+1)
		body, err := io.ReadAll(limitedBody)
		if err != nil {
			return "", fmt.Errorf("read response: %w", err)
		}
		if int64(len(body)) > maxResponseSize {
			return "", fmt.Errorf("response too large: %d bytes exceeds %d byte limit", len(body), maxResponseSize)
		}
		if err := json.Unmarshal(body, result); err != nil {
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
	err := c.get(ctx, fmt.Sprintf("/search/code?q=%s+filename:go.mod&per_page=1", url.QueryEscape(modulePath)), &result)
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
	if err := validateContentsPath(path); err != nil {
		return nil, fmt.Errorf("invalid contents path: %w", err)
	}
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
	if err := validateContentsPath(path); err != nil {
		return nil, fmt.Errorf("invalid contents path: %w", err)
	}
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

// GetRepoLanguage returns GitHub's primary-language guess for a
// repo. It reuses GetRepo's single API call and projects out just
// the language string, which is useful for callers (e.g., the
// ecosystem detector) that don't need the rest of the repo metadata.
// Returns the empty string when GitHub has no language determination.
func (c *Client) GetRepoLanguage(ctx context.Context, owner, repoName string) (string, error) {
	r, err := c.GetRepo(ctx, owner, repoName)
	if err != nil {
		return "", err
	}
	return r.Language, nil
}

// ListRootFilenames returns the names of regular files at the repo
// root. It filters out directories so callers looking for manifest
// files (go.mod, package.json, Cargo.toml, …) don't need to reason
// about entry types. Returns an empty slice if the repo is empty;
// nil without error for a repo that doesn't exist.
//
// One API call. Useful for cheap ecosystem detection:
// `go.mod in root filenames → Go project`.
//
// Uses a dedicated URL construction (the empty-path root listing)
// rather than GetDirectoryContents("") because the contents-path
// validator rejects empty strings (per issue #90's SSRF defenses).
// The repo path components here come from validated owner/repoName,
// so the validator's protections still apply to user input upstream.
func (c *Client) ListRootFilenames(ctx context.Context, owner, repoName string) ([]string, error) {
	var entries []repoContent
	err := c.get(ctx, fmt.Sprintf("/repos/%s/%s/contents", owner, repoName), &entries)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			return nil, nil
		}
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.Type == "file" {
			names = append(names, e.Name)
		}
	}
	return names, nil
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

	owner = parts[0]
	repoName = parts[1]

	// Validate characters to prevent SSRF via path traversal, query
	// injection, or other URL manipulation. GitHub names allow only
	// alphanumeric, hyphens, dots, and underscores.
	if !validGitHubName.MatchString(owner) {
		return "", "", fmt.Errorf("invalid GitHub owner name %q: contains disallowed characters", owner)
	}
	if !validGitHubName.MatchString(repoName) {
		return "", "", fmt.Errorf("invalid GitHub repo name %q: contains disallowed characters", repoName)
	}

	return owner, repoName, nil
}
