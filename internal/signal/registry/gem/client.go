package gem

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Sentinel errors for registry responses.
var (
	ErrNotFound     = errors.New("gem not found on rubygems.org")
	ErrUnauthorized = errors.New("rubygems.org requires authentication")
)

const (
	defaultBaseURL = "https://rubygems.org"
	userAgent      = "signatory/0.1 (https://github.com/sarahmaeve/signatory)"
	maxBodyBytes   = 4 * 1024 * 1024 // 4 MiB cap on response bodies
)

// gemNameRe validates gem names: starts with a letter or digit, then
// allows letters, digits, hyphens, underscores, and dots.
var gemNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// Client queries the rubygems.org JSON API.
type Client struct {
	httpClient *http.Client
	baseURL    string
}

// NewClient returns a Client pointed at the production rubygems.org.
func NewClient() *Client {
	return &Client{
		httpClient: &http.Client{
			Timeout:       15 * time.Second,
			CheckRedirect: checkRedirect,
		},
		baseURL: defaultBaseURL,
	}
}

// NewClientWithBaseURL returns a Client pointed at a custom base URL.
// Primary use: tests with httptest servers.
func NewClientWithBaseURL(base string) *Client {
	return &Client{
		httpClient: &http.Client{
			Timeout:       15 * time.Second,
			CheckRedirect: checkRedirect,
		},
		baseURL: base,
	}
}

// checkRedirect enforces HTTPS-only redirects and bounds the chain.
// Symmetric with the npm, PyPI, cargo, maven, and gopublish clients —
// see those for the rationale: rubygems.org is HTTPS-only, so any
// scheme downgrade is either misconfiguration or a MITM attempting to
// tamper with owner / version metadata that feeds trust signals.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if req.URL.Scheme != "https" {
		return fmt.Errorf("refusing redirect to non-HTTPS URL %s", req.URL.Redacted())
	}
	if len(via) >= 10 {
		return fmt.Errorf("too many redirects")
	}
	return nil
}

// GetGem fetches metadata for a gem from /api/v1/gems/{name}.json.
func (c *Client) GetGem(ctx context.Context, name string) (*GemResponse, error) {
	if err := ValidateGemName(name); err != nil {
		return nil, err
	}

	url := c.baseURL + "/api/v1/gems/" + name + ".json"
	body, err := c.doGet(ctx, url)
	if err != nil {
		return nil, err
	}

	var resp GemResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parse gem response: %w", err)
	}
	return &resp, nil
}

// GetVersions fetches all versions from /api/v1/versions/{name}.json.
func (c *Client) GetVersions(ctx context.Context, name string) ([]VersionEntry, error) {
	if err := ValidateGemName(name); err != nil {
		return nil, err
	}

	url := c.baseURL + "/api/v1/versions/" + name + ".json"
	body, err := c.doGet(ctx, url)
	if err != nil {
		return nil, err
	}

	var versions []VersionEntry
	if err := json.Unmarshal(body, &versions); err != nil {
		return nil, fmt.Errorf("parse versions response: %w", err)
	}
	return versions, nil
}

// GetOwners fetches the owners list from /api/v1/gems/{name}/owners.json.
// Returns ErrUnauthorized when the endpoint requires an API key.
func (c *Client) GetOwners(ctx context.Context, name string) ([]OwnerEntry, error) {
	if err := ValidateGemName(name); err != nil {
		return nil, err
	}

	url := c.baseURL + "/api/v1/gems/" + name + "/owners.json"
	body, err := c.doGet(ctx, url)
	if err != nil {
		return nil, err
	}

	var owners []OwnerEntry
	if err := json.Unmarshal(body, &owners); err != nil {
		return nil, fmt.Errorf("parse owners response: %w", err)
	}
	return owners, nil
}

// ResolveRepoURL fetches gem metadata and returns the declared source
// repository URL, checking source_code_uri first, then homepage_uri.
// Only returns URLs that look like GitHub/GitLab repositories.
// Returns empty string (not an error) when no source is declared.
func (c *Client) ResolveRepoURL(ctx context.Context, name string) (string, error) {
	gem, err := c.GetGem(ctx, name)
	if err != nil {
		return "", err
	}

	// Priority chain: source_code_uri → homepage_uri.
	for _, candidate := range []string{gem.SourceCodeURI, gem.HomepageURI} {
		if isGitHostURL(candidate) {
			return normalizeRepoURL(candidate), nil
		}
	}

	return "", nil
}

// ValidateGemName checks that name is a plausible gem name. Rejects
// empty strings, path traversal, and characters that could cause
// URL injection.
func ValidateGemName(name string) error {
	if name == "" {
		return fmt.Errorf("gem name is empty")
	}
	if len(name) > 255 {
		return fmt.Errorf("gem name exceeds 255 characters")
	}
	if !gemNameRe.MatchString(name) {
		return fmt.Errorf("gem name %q contains invalid characters", name)
	}
	return nil
}

// doGet performs a GET request with context, User-Agent, and body cap.
func (c *Client) doGet(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("rubygems.org request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		// fall through to read body
	case http.StatusNotFound:
		return nil, ErrNotFound
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, ErrUnauthorized
	default:
		return nil, fmt.Errorf("rubygems.org returned HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}
	return body, nil
}

// isGitHostURL checks whether a URL points to a known git hosting
// service (GitHub, GitLab).
func isGitHostURL(u string) bool {
	if u == "" {
		return false
	}
	lower := strings.ToLower(u)
	return strings.Contains(lower, "github.com/") ||
		strings.Contains(lower, "gitlab.com/")
}

// normalizeRepoURL cleans up a repository URL: strips .git suffix,
// trailing slashes, query/fragment, and GitHub path suffixes like
// /tree/<ref> or /blob/<ref>/... that rubygems.org commonly sets as
// source_code_uri (pointing at the version's tree view rather than
// the bare repository).
func normalizeRepoURL(u string) string {
	// Strip query string and fragment.
	if idx := strings.IndexByte(u, '?'); idx >= 0 {
		u = u[:idx]
	}
	if idx := strings.IndexByte(u, '#'); idx >= 0 {
		u = u[:idx]
	}
	// Strip trailing slashes and .git suffix.
	u = strings.TrimRight(u, "/")
	u = strings.TrimSuffix(u, ".git")

	// Strip GitHub /tree/<ref>, /blob/<ref>, /commit/<sha> path
	// suffixes. These appear in rubygems source_code_uri when gem
	// authors point at a specific tag's tree view rather than the
	// repo root. The pattern is: host/owner/repo/{tree,blob,commit}/...
	// We need at least owner/repo before the keyword to avoid
	// false-positive stripping on repos literally named "tree".
	for _, keyword := range []string{"/tree/", "/blob/", "/commit/"} {
		if idx := strings.Index(u, keyword); idx > 0 {
			// Verify there are at least two path segments (owner/repo)
			// before the keyword by checking for github.com/X/Y shape.
			prefix := u[:idx]
			afterHost := strings.TrimPrefix(prefix, "https://")
			afterHost = strings.TrimPrefix(afterHost, "http://")
			// Count slashes after the host — need at least 2
			// (host/owner/repo → 2 slashes after stripping scheme).
			if strings.Count(afterHost, "/") >= 2 {
				u = prefix
				break
			}
		}
	}

	return u
}
