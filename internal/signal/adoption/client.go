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
package adoption

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"
)

// ErrRateLimit is the sentinel callers compare via errors.Is when
// GitHub's search API responds 403 or 429. The search endpoint has
// its own rate-limit pool (30 req/min unauth, 5000/hr auth) distinct
// from the main API, and exhausting it returns Forbidden with an
// X-RateLimit-Reset header. The github collector handles this
// gracefully by recording a retryable failure; this collector
// mirrors the discipline.
var ErrRateLimit = errors.New("adoption: github search rate limit exceeded")

// maxResponseSize bounds the search-response body. Mirrors the
// github / forgejo / gitlab clients' 10 MiB cap.
const maxResponseSize = 10 * 1024 * 1024

// Client is the minimal HTTP client for the adoption collector. It
// hits GitHub's /search/code endpoint and counts results. Owns its
// own client rather than reusing the github package's so the two
// collectors stay independently constructible (the github collector
// no longer needs the search code path after this commit's lift-out).
type Client struct {
	httpClient *http.Client
	baseURL    string
	token      string
}

// NewClient creates a client pointed at api.github.com with a 60s
// timeout (matches the per-forge metadata clients). Reads
// GITHUB_TOKEN from the environment when present — search calls
// authenticated raise the rate limit from 30 req/min to 5000/hr,
// and adoption is the noisiest API consumer in the dispatch list
// because every Go-ecosystem analyze fires one search call.
func NewClient() *Client {
	return &Client{
		httpClient: &http.Client{
			Timeout:       60 * time.Second,
			CheckRedirect: checkRedirect,
		},
		baseURL: "https://api.github.com",
		token:   os.Getenv("GITHUB_TOKEN"),
	}
}

// checkRedirect mirrors the per-forge clients' redirect policy:
// refuse non-HTTPS, bound the chain. Also strips the Authorization
// header on cross-origin redirects so the GITHUB_TOKEN doesn't
// leak to an attacker-controlled redirect target (same rule the
// github collector's checkRedirect enforces — see
// internal/signal/github/client.go for the full rationale).
func checkRedirect(req *http.Request, via []*http.Request) error {
	if req.URL.Scheme != "https" {
		return fmt.Errorf("refusing redirect to non-HTTPS URL %s", req.URL.Redacted())
	}
	if len(via) > 0 && req.URL.Host != via[0].URL.Host {
		req.Header.Del("Authorization")
	}
	if len(via) >= 10 {
		return fmt.Errorf("too many redirects")
	}
	return nil
}

// GoModRefCount queries GitHub's code-search API for the number of
// public go.mod files that mention modulePath. Returns the count
// from total_count. ErrRateLimit (wrapped) when the search rate
// limit has been hit; other non-200 responses surface as
// status-only errors (body deliberately not included — same issue
// #93 reasoning as the per-forge clients).
//
// modulePath is passed through url.QueryEscape so slashes and dots
// in the import path don't break the query string. The "+"
// joining modulePath and "filename:go.mod" is GitHub's search-
// grammar AND operator (literal +), not a URL-encoded space.
func (c *Client) GoModRefCount(ctx context.Context, modulePath string) (int, error) {
	q := url.QueryEscape(modulePath) + "+filename:go.mod"
	path := "/search/code?q=" + q + "&per_page=1"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return 0, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("execute request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // close-after-read; body consumed below, error here is not actionable

	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		return 0, ErrRateLimit
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("github search API returned status %d", resp.StatusCode)
	}

	limited := io.LimitReader(resp.Body, maxResponseSize+1)
	body, readErr := io.ReadAll(limited)
	if readErr != nil {
		return 0, fmt.Errorf("read response: %w", readErr)
	}
	if int64(len(body)) > maxResponseSize {
		return 0, fmt.Errorf("response too large: %d bytes exceeds %d byte limit", len(body), maxResponseSize)
	}

	var result struct {
		TotalCount int `json:"total_count"`
	}
	if jsonErr := json.Unmarshal(body, &result); jsonErr != nil {
		return 0, fmt.Errorf("decode response: %w", jsonErr)
	}
	return result.TotalCount, nil
}
