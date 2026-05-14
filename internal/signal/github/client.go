package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sarahmaeve/signatory/internal/httpx"
)

// ErrNotFound is the sentinel the client returns (wrapped via %w)
// when the GitHub API responds 404. Callers that treat absence as a
// signal (e.g., "does this file exist in the repo?") compare with
// errors.Is(err, github.ErrNotFound) rather than matching on the
// error string — string-matching would silently break on any future
// rewording of the error message. The three callers that care about
// the 404 semantic (GetDirectoryContents, GetFileRaw, GetGoModRefCount)
// translate this sentinel into the lightweight (nil, nil) "not
// present" signal their existing consumers check.
//
// Wraps httpx.ErrNotFound so callers can also do errors.Is(err,
// httpx.ErrNotFound) for ecosystem-agnostic absence detection.
var ErrNotFound = fmt.Errorf("github: %w", httpx.ErrNotFound)

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

// Client is a minimal GitHub REST API client. The defensive network
// discipline (timeouts, HTTPS-only redirects, response-body cap,
// drain-and-discard on non-2xx, sanitized status errors) lives in
// httpx.SecureClient; this package owns input validation, sentinel-
// error wrapping, the typed RateLimitError translation, and
// token-redaction on error paths (sanitizeError).
//
// Cross-origin Authorization stripping on redirect is handled by Go's
// stdlib http.Client via shouldCopyHeaderOnRedirect — the pre-port
// client added an explicit req.Header.Del("Authorization") inside
// checkRedirect as belt-and-suspenders; that's redundant with
// stdlib's default and is dropped in the httpx port. Production
// behavior is unchanged because stdlib already enforces the strip.
type Client struct {
	api   *httpx.SecureClient
	token string
}

// NewClient creates a GitHub API client. If token is empty, requests
// are unauthenticated (60 req/hr limit vs. 5000 authenticated).
func NewClient(token string) *Client {
	return newClient("https://api.github.com", token)
}

// NewClientWithBaseURL creates an unauthenticated Client pointed at
// the given base URL. Test injection point: lets cmd/signatory
// integration tests redirect API calls to an httptest.Server without
// needing internal field access. Production callers use NewClient.
func NewClientWithBaseURL(baseURL string) *Client {
	return newClient(baseURL, "")
}

// NewClientWithBaseURLAndToken creates a Client with both a custom
// base URL and a token. Used by security tests that need to verify
// token-handling discipline against an httptest server.
func NewClientWithBaseURLAndToken(baseURL, token string) *Client {
	return newClient(baseURL, token)
}

// newClient is the shared constructor — keeps the three exported
// constructors in lockstep on transport configuration.
func newClient(baseURL, token string) *Client {
	return &Client{
		api:   httpx.NewSecureClient(httpx.WithBaseURL(baseURL)),
		token: token,
	}
}

// RateLimitError is returned when the API rate limit is exceeded.
// The interceptor wired into every get / getWithLinkHeader call
// translates GitHub's 403 / 429 responses (with X-RateLimit-Reset)
// into this typed error before httpx's default status classification
// fires. Callers use errors.As(err, &github.RateLimitError{}) to
// route on the typed error and read ResetAt for retry scheduling.
type RateLimitError struct {
	ResetAt time.Time
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("GitHub API rate limit exceeded, resets at %s", e.ResetAt.Format(time.RFC3339))
}

// rateLimitInterceptor translates GitHub's 403 / 429 responses into a
// typed *RateLimitError. Returns nil for other non-2xx statuses so
// httpx's default classification (ErrNotFound for 404, status-only
// error for the rest) fires. Symmetric with adoption's interceptor.
func rateLimitInterceptor(resp *http.Response) error {
	if resp.StatusCode == http.StatusForbidden ||
		resp.StatusCode == http.StatusTooManyRequests {
		return &RateLimitError{ResetAt: parseRateLimitReset(resp.Header.Get("X-RateLimit-Reset"))}
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

// sanitizeError redacts token from err's rendered string when it
// appears, returning a fresh value carrying the redacted text.
//
// Behavior:
//   - nil err → nil (helper safe on success paths, where the
//     named-return + defer pattern fires unconditionally).
//   - empty token (unauthenticated client) → input err unchanged.
//     Without this guard, strings.ReplaceAll with an empty pattern
//     would interleave the marker between every character.
//   - token absent from err.Error() → input err unchanged. Chain
//     preserved so callers' errors.Is(err, ErrNotFound) keeps
//     working through the boundary.
//   - token present → fresh errors.New with all occurrences
//     replaced by [REDACTED-TOKEN]. Chain dropped (security beats
//     Unwrap convenience): preserving the chain via custom error
//     type would let errors.Unwrap(err).Error() recover the token.
func sanitizeError(err error, token string) error {
	if err == nil {
		return nil
	}
	if token == "" {
		return err
	}
	s := err.Error()
	if !strings.Contains(s, token) {
		return err
	}
	return errors.New(strings.ReplaceAll(s, token, "[REDACTED-TOKEN]"))
}

// requestOpts assembles the per-request httpx options that every
// authenticated GitHub call shares: Accept header, rate-limit
// interceptor, conditional Authorization header.
func (c *Client) requestOpts() []httpx.RequestOption {
	opts := []httpx.RequestOption{
		httpx.WithHeader("Accept", "application/vnd.github.v3+json"),
		httpx.WithStatusInterceptor(rateLimitInterceptor),
	}
	if c.token != "" {
		opts = append(opts, httpx.WithHeader("Authorization", "Bearer "+c.token))
	}
	return opts
}

// get performs a GET request to the GitHub API.
//
// Named return + deferred sanitizeError applies token redaction at
// every error-return path including those added in the future. See
// sanitizeError's doc for the threat model.
//
// If result is non-nil, the response body is JSON-decoded into it
// via httpx.GetJSON. If nil, the body is read and discarded — the
// caller only needs the success/failure signal (typical for the
// pagination probe in GetTotalCommitCount).
func (c *Client) get(ctx context.Context, path string, result any) (err error) {
	defer func() { err = sanitizeError(err, c.token) }()

	var gerr error
	if result == nil {
		// Drain the body so the connection is reusable; we still
		// want the status classification (ErrNotFound mapping,
		// rate-limit translation) that httpx applies.
		_, gerr = c.api.Get(ctx, path, c.requestOpts()...)
	} else {
		gerr = c.api.GetJSON(ctx, path, result, c.requestOpts()...)
	}
	if gerr != nil && errors.Is(gerr, httpx.ErrNotFound) {
		// Wrap with github.ErrNotFound so callers'
		// errors.Is(err, github.ErrNotFound) keeps working — the
		// httpx sentinel is also in the chain via the wrap.
		return fmt.Errorf("%w: %s", ErrNotFound, path)
	}
	return gerr
}

// getWithLinkHeader performs a GET and returns the Link header for
// pagination. Used by GetTotalCommitCount to extract page count from
// the rel="last" Link entry without paging through every commit.
//
// Named return + deferred sanitizeError matches get's pattern: every
// error-return path runs through token redaction at function exit.
// See sanitizeError's doc for the threat model.
func (c *Client) getWithLinkHeader(ctx context.Context, path string, result any) (link string, err error) {
	defer func() { err = sanitizeError(err, c.token) }()

	body, headers, _, gerr := c.api.GetWithResponse(ctx, path, c.requestOpts()...)
	if gerr != nil {
		if errors.Is(gerr, httpx.ErrNotFound) {
			return "", fmt.Errorf("%w: %s", ErrNotFound, path)
		}
		return "", gerr
	}
	if result != nil {
		if jerr := json.Unmarshal(body, result); jerr != nil {
			return "", fmt.Errorf("decode response: %w", jerr)
		}
	}
	return headers.Get("Link"), nil
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
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return contents, err
}

// fileContent is the base64-encoded representation GitHub returns for
// single-file fetches from the contents API.
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
		if errors.Is(err, ErrNotFound) {
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
		if errors.Is(err, ErrNotFound) {
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
