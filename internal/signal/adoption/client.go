// Package adoption collects the "adoption" signal: a count of inbound
// go.mod references to a Go module, computed by querying GitHub's
// code-search API for occurrences of "<importPath> filename:go.mod"
// across public repositories. Paired with the entity's star count
// (read from sibling collectors via InRunResult) to produce a
// refs_to_stars ratio that distinguishes direct adoption ("stars and
// importers grow together") from transitive adoption ("many importers,
// few stars — used inside the dependency graph, rarely watched
// directly").
//
// # Why a standalone collector
//
// adoption used to live inside the github collector — its
// collectAdoption sub-step hardcoded "github.com/<owner>/<repo>" as
// the search term, which meant codeberg- and gitlab-hosted Go
// modules never received an adoption signal. Lifting it out lets one
// collector serve every forge, parameterized on the host portion
// of the entity's URL.
//
// # API asymmetry: search is github-only
//
// Only GitHub publishes a code-search API. Codeberg/Forgejo and
// GitLab do not have an equivalent public index. So this collector
// hits api.github.com regardless of which forge owns the analyzed
// module — the host string in the search query (codeberg.org/X/Y)
// is forge-agnostic, but the backend index it consults is GitHub's.
// This is a real coverage caveat: a codeberg-hosted module imported
// only by other codeberg-hosted modules would produce go_mod_refs=0
// even when it has real downstream adoption, because GitHub's index
// doesn't see codeberg-hosted go.mod files. Document this on the
// signal type's registry entry when the limitation surfaces in
// posture work.
//
// # Source name on the emitted signal
//
// Signals emitted by this collector carry source="adoption" — the
// collector identity, per the codebase convention (github collector →
// source="github", forgejo → "forgejo", etc.). Pre-lift, adoption
// signals went out with source="github" because the github collector
// owned the emission. Downstream consumers querying signals by
// (entity_id, type="adoption") work unchanged; consumers querying
// by (source="github", type="adoption") need to migrate to
// source="adoption" or drop the source filter. The 24h TTL means
// old rows phase out within a day of upgrading.
//
// # Ecosystem and host gating
//
// adoption only makes sense for Go modules — the search query is
// "filename:go.mod" — so the collector self-gates on
// entity.Ecosystem ∈ {"", "golang", "go"}. Non-Go entities (npm /
// pypi / cargo / gem / maven packages) skip the entire collection.
// Pre-lift, the github collector emitted an always-zero adoption
// signal for non-Go github repos; this tightening removes that
// noise.
//
// The URL host must point at a recognized forge (github.com,
// codeberg.org, gitlab.com); other hosts skip because we have no
// confident derivation of the module import path. Self-hosted
// forges would need to land alongside the per-forge metadata
// collectors' allow-list mechanism.
//
// The defensive network discipline (HTTPS-only redirects, response-
// body cap, drain-and-discard on non-2xx, sanitized status errors)
// lives in httpx.SecureClient. This package owns the URL shape for
// GitHub's code-search endpoint, the auth/no-auth conditional, and
// the 403/429 → ErrRateLimit translation (via httpx's
// WithStatusInterceptor — the typed-error hook the github client
// will also use for its RateLimitError).
package adoption

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"

	"github.com/sarahmaeve/signatory/internal/httpx"
)

// ErrRateLimit is the sentinel callers compare via errors.Is when
// GitHub's search API responds 403 or 429. The search endpoint has
// its own rate-limit pool (30 req/min unauth, 5000/hr auth) distinct
// from the main API, and exhausting it returns Forbidden with an
// X-RateLimit-Reset header. The collector handles this gracefully by
// recording a retryable failure.
var ErrRateLimit = errors.New("adoption: github search rate limit exceeded")

// Client is the minimal HTTP client for the adoption collector. It
// hits GitHub's /search/code endpoint and counts results. Owns its
// own httpx.SecureClient rather than reusing the github package's so
// the two collectors stay independently constructible.
//
// Authorization is per-request (when token is non-empty) rather than
// constructor-time because httpx options apply to all requests; the
// token-conditional logic belongs in GoModRefCount where it's clear.
type Client struct {
	api   *httpx.SecureClient
	token string
}

// NewClient creates a client pointed at api.github.com. Reads
// GITHUB_TOKEN from the environment when present — search calls
// authenticated raise the rate limit from 30 req/min to 5000/hr,
// and adoption is the noisiest API consumer in the dispatch list
// because every Go-ecosystem analyze fires one search call.
//
// User-Agent is intentionally not set — the existing client relied
// on Go's stdlib default ("Go-http-client/1.1") and we preserve that
// to keep this port behavior-equivalent. A future improvement
// could set an explicit "signatory/0.1 (...)" UA in lockstep with
// the github client.
func NewClient() *Client {
	return &Client{
		api: httpx.NewSecureClient(
			httpx.WithBaseURL("https://api.github.com"),
			// Preserve the existing behavior of no explicit
			// User-Agent — Go's default fires.
			httpx.WithUserAgent(""),
		),
		token: os.Getenv("GITHUB_TOKEN"),
	}
}

// NewClientWithBaseURL returns a Client whose endpoint points at the
// supplied base. Primary use case: test harnesses pointing the
// client at an httptest server. Production code should call NewClient.
//
// Token is left empty in this constructor — production-pattern code
// reads GITHUB_TOKEN from env, but tests don't want env-dependent
// authentication leaking into the test environment.
func NewClientWithBaseURL(base string) *Client {
	return &Client{
		api: httpx.NewSecureClient(
			httpx.WithBaseURL(base),
			httpx.WithUserAgent(""),
		),
	}
}

// rateLimitInterceptor translates GitHub's 403 / 429 responses into
// ErrRateLimit before httpx's default status classification runs.
// Returning a non-nil error short-circuits httpx.GetJSON; the typed
// sentinel survives the boundary so the collector's
// errors.Is(err, adoption.ErrRateLimit) branch fires.
func rateLimitInterceptor(resp *http.Response) error {
	if resp.StatusCode == http.StatusForbidden ||
		resp.StatusCode == http.StatusTooManyRequests {
		return ErrRateLimit
	}
	return nil
}

// GoModRefCount queries GitHub's code-search API for the number of
// public go.mod files that mention modulePath. Returns the count
// from total_count. ErrRateLimit when the search rate limit has been
// hit; other non-200 responses surface as wrapped errors with the
// status code (body deliberately not included — #93 reasoning,
// enforced by httpx).
//
// modulePath is passed through url.QueryEscape so slashes and dots
// in the import path don't break the query string. The "+"
// joining modulePath and "filename:go.mod" is GitHub's search-
// grammar AND operator (literal +), not a URL-encoded space.
func (c *Client) GoModRefCount(ctx context.Context, modulePath string) (int, error) {
	q := url.QueryEscape(modulePath) + "+filename:go.mod"
	path := "/search/code?q=" + q + "&per_page=1"

	opts := []httpx.RequestOption{
		httpx.WithHeader("Accept", "application/vnd.github.v3+json"),
		httpx.WithStatusInterceptor(rateLimitInterceptor),
	}
	if c.token != "" {
		opts = append(opts, httpx.WithHeader("Authorization", "Bearer "+c.token))
	}

	var result struct {
		TotalCount int `json:"total_count"`
	}
	if err := c.api.GetJSON(ctx, path, &result, opts...); err != nil {
		if errors.Is(err, ErrRateLimit) {
			// Propagate the sentinel unwrapped so the collector's
			// errors.Is(err, ErrRateLimit) branch fires for retry
			// classification.
			return 0, err
		}
		return 0, fmt.Errorf("github search API request: %w", err)
	}
	return result.TotalCount, nil
}
