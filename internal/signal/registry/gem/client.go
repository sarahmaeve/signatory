package gem

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/sarahmaeve/signatory/internal/httpx"
)

// Sentinel errors for registry responses. ErrNotFound wraps
// httpx.ErrNotFound so callers can also do errors.Is(err,
// httpx.ErrNotFound) for ecosystem-agnostic absence detection.
// ErrUnauthorized is gem-specific (rubygems.org's owners endpoint
// requires an API key for some queries) and surfaces via a status
// interceptor that maps 401/403 to this sentinel before the default
// status classification runs.
var (
	ErrNotFound     = fmt.Errorf("gem not found on rubygems.org: %w", httpx.ErrNotFound)
	ErrUnauthorized = errors.New("rubygems.org requires authentication")
)

const (
	defaultBaseURL = "https://rubygems.org"
	userAgent      = "signatory/0.1 (https://github.com/sarahmaeve/signatory)"
	// gemTimeout is tighter than the 60s default because rubygems.org
	// API responses are small (a few KB) and the original gem client
	// used 15s. Preserved for behavior parity.
	gemTimeout = 15 * time.Second
	// gemMaxBytes is tighter than the 10 MiB httpx default because
	// gem JSON responses are small. Preserved for behavior parity.
	gemMaxBytes = 4 * 1024 * 1024
)

// gemNameRe validates gem names: starts with a letter or digit, then
// allows letters, digits, hyphens, underscores, and dots.
var gemNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// Client queries the rubygems.org JSON API. The defensive network
// discipline (HTTPS-only redirects, body cap, drain-and-discard on
// non-2xx, sanitized status errors) lives in httpx.SecureClient. This
// package owns input validation, sentinel-error wrapping, and the
// 401/403 → ErrUnauthorized translation via WithStatusInterceptor.
type Client struct {
	api *httpx.SecureClient
}

// NewClient returns a Client pointed at the production rubygems.org.
func NewClient() *Client {
	return &Client{api: buildAPI(defaultBaseURL)}
}

// NewClientWithBaseURL returns a Client pointed at a custom base URL.
// Primary use: tests with httptest servers.
func NewClientWithBaseURL(base string) *Client {
	return &Client{api: buildAPI(base)}
}

// buildAPI keeps the two constructors in lockstep on transport
// configuration: tighter 15s timeout, tighter 4 MiB body cap, gem
// User-Agent, and the 401/403 interceptor that maps to
// ErrUnauthorized.
func buildAPI(base string) *httpx.SecureClient {
	return httpx.NewSecureClient(
		httpx.WithBaseURL(base),
		httpx.WithTimeout(gemTimeout),
		httpx.WithMaxBytes(gemMaxBytes),
		httpx.WithUserAgent(userAgent),
	)
}

// unauthorizedInterceptor maps rubygems' 401 / 403 responses to
// ErrUnauthorized before httpx's default status classification runs.
// Called per-request because the interceptor option is per-request;
// passing the same function each time keeps it simple.
func unauthorizedInterceptor(resp *http.Response) error {
	if resp.StatusCode == http.StatusUnauthorized ||
		resp.StatusCode == http.StatusForbidden {
		return ErrUnauthorized
	}
	return nil
}

// GetGem fetches metadata for a gem from /api/v1/gems/{name}.json.
func (c *Client) GetGem(ctx context.Context, name string) (*GemResponse, error) {
	if err := ValidateGemName(name); err != nil {
		return nil, err
	}
	var resp GemResponse
	if err := c.fetchJSON(ctx, "/api/v1/gems/"+name+".json", &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// GetVersions fetches all versions from /api/v1/versions/{name}.json.
func (c *Client) GetVersions(ctx context.Context, name string) ([]VersionEntry, error) {
	if err := ValidateGemName(name); err != nil {
		return nil, err
	}
	var versions []VersionEntry
	if err := c.fetchJSON(ctx, "/api/v1/versions/"+name+".json", &versions); err != nil {
		return nil, err
	}
	return versions, nil
}

// GetOwners fetches the owners list from /api/v1/gems/{name}/owners.json.
// Returns ErrUnauthorized when the endpoint requires an API key.
func (c *Client) GetOwners(ctx context.Context, name string) ([]OwnerEntry, error) {
	if err := ValidateGemName(name); err != nil {
		return nil, err
	}
	var owners []OwnerEntry
	if err := c.fetchJSON(ctx, "/api/v1/gems/"+name+"/owners.json", &owners); err != nil {
		return nil, err
	}
	return owners, nil
}

// fetchJSON is the shared per-method helper. Wraps the httpx call
// with the gem-specific error translation (httpx.ErrNotFound →
// gem.ErrNotFound; interceptor 401/403 → ErrUnauthorized which
// surfaces here as-is; other errors get a contextual wrap).
func (c *Client) fetchJSON(ctx context.Context, path string, result any) error {
	err := c.api.GetJSON(ctx, path, result,
		httpx.WithHeader("Accept", "application/json"),
		httpx.WithStatusInterceptor(unauthorizedInterceptor),
	)
	if err != nil {
		if errors.Is(err, ErrUnauthorized) {
			// Propagate the typed sentinel unwrapped so callers'
			// errors.Is(err, ErrUnauthorized) branch fires.
			return err
		}
		if errors.Is(err, httpx.ErrNotFound) {
			return ErrNotFound
		}
		return fmt.Errorf("rubygems.org request: %w", err)
	}
	return nil
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

// isGitHostURL checks whether a URL points to a known git hosting
// service (GitHub, Codeberg, GitLab). The substring check is
// permissive — it doesn't host-anchor — which matches the rest of
// the v0.1 gem resolver but is laxer than the host-anchored
// rejectUnrecognizedForgeURL gate the other ecosystems' resolvers route
// through. Hardening to host-anchored is tracked separately; for
// now, expanding the recognized set keeps gem in step with the rest
// of the multi-forge work.
func isGitHostURL(u string) bool {
	if u == "" {
		return false
	}
	lower := strings.ToLower(u)
	return strings.Contains(lower, "github.com/") ||
		strings.Contains(lower, "codeberg.org/") ||
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
