// Package cargo provides an HTTP client for the crates.io registry
// API. Phase A exposes only the surface Layer 6's source resolver
// needs (GetCrate for the repository URL); Phase B extends it with
// owners, per-version metadata, and the full signal-collector surface.
//
// Mirrors the defensive patterns established by the npm and PyPI
// clients: HTTPS-only redirects, response-body size cap, crate-name
// validation before URL construction, error-body sanitization, and
// context-propagation testing.
package cargo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"time"
)

// ErrNotFound is returned (wrapped via %w) when crates.io responds
// 404 for a crate lookup. Callers compare with errors.Is.
var ErrNotFound = errors.New("cargo: not found")

// maxResponseSize bounds registry response bodies. Even large crates
// (serde with 300+ versions) produce <1 MB responses; 10 MB matches
// the npm and PyPI caps for consistency.
const maxResponseSize = 10 * 1024 * 1024

// maxCrateNameLength is crates.io's published cap.
const maxCrateNameLength = 64

// crateNamePattern matches the crates.io name grammar: starts with a
// letter, then letters, digits, hyphens, or underscores. Max 64 chars
// is enforced separately by length check.
var crateNamePattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]*$`)

// ValidateCrateName enforces the crate-name grammar before any URL
// construction. Per npm's #90 lesson: user-influenced bytes
// substituted into an HTTP URL are an injection surface — validate
// at the function boundary.
func ValidateCrateName(name string) error {
	if name == "" {
		return fmt.Errorf("crate name is empty")
	}
	if len(name) > maxCrateNameLength {
		return fmt.Errorf("crate name exceeds crates.io's %d-byte maximum (got %d)",
			maxCrateNameLength, len(name))
	}
	if !crateNamePattern.MatchString(name) {
		return fmt.Errorf("crate name %q does not match crates.io grammar "+
			"(allowed: a-z A-Z 0-9 _ -, must start with a letter)", name)
	}
	return nil
}

// Client is a narrow crates.io registry HTTP client. Phase A exposes
// only GetCrate (for source resolution); Phase B will add GetOwners,
// GetVersion, etc.
type Client struct {
	httpClient  *http.Client
	registryURL string
}

// NewClient returns a Client bound to the public crates.io endpoint.
// The 60s per-request timeout matches npm, PyPI, and gopublish.
func NewClient() *Client {
	return &Client{
		httpClient: &http.Client{
			Timeout:       60 * time.Second,
			CheckRedirect: checkRedirect,
		},
		registryURL: "https://crates.io",
	}
}

// NewClientWithBaseURL returns a Client pointing at the supplied base.
// Primary use: test harnesses with httptest servers.
func NewClientWithBaseURL(base string) *Client {
	return &Client{
		httpClient: &http.Client{
			Timeout:       60 * time.Second,
			CheckRedirect: checkRedirect,
		},
		registryURL: base,
	}
}

// checkRedirect enforces HTTPS-only redirects and bounds the chain.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if req.URL.Scheme != "https" {
		return fmt.Errorf("refusing redirect to non-HTTPS URL %s", req.URL.Redacted())
	}
	if len(via) >= 10 {
		return fmt.Errorf("too many redirects")
	}
	return nil
}

// GetCrate fetches the crate metadata from crates.io's JSON API.
// Returns ErrNotFound (wrapped) on 404. Other non-2xx statuses
// surface as sanitized errors (response body is never in the error
// string per #93).
//
// Phase A models only Crate.Repository (for source resolution);
// Phase B will extend CrateResponse with Versions, owners, etc.
func (c *Client) GetCrate(ctx context.Context, name string) (*CrateResponse, error) {
	if err := ValidateCrateName(name); err != nil {
		return nil, fmt.Errorf("get crate: %w", err)
	}

	escapedName := url.PathEscape(name)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.registryURL+"/api/v1/crates/"+escapedName, nil)
	if err != nil {
		return nil, fmt.Errorf("build request for %q: %w", name, err)
	}
	req.Header.Set("Accept", "application/json")
	// crates.io REQUIRES a User-Agent; requests without one get 403.
	req.Header.Set("User-Agent", "signatory/0.1 (https://github.com/sarahmaeve/signatory)")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("crates.io request for %q failed: %w", name, err)
	}
	defer resp.Body.Close() //nolint:errcheck // response body close

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Drain-and-discard; body NEVER in error string.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseSize))
		return nil, fmt.Errorf("crates.io returned status %d for %q",
			resp.StatusCode, name)
	}

	limited := io.LimitReader(resp.Body, maxResponseSize+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read crates.io response for %q: %w", name, err)
	}
	if int64(len(body)) > maxResponseSize {
		return nil, fmt.Errorf("crates.io response for %q exceeds %d-byte cap",
			name, maxResponseSize)
	}

	var cr CrateResponse
	if err := json.Unmarshal(body, &cr); err != nil {
		return nil, fmt.Errorf("decode crates.io response for %q: %w", name, err)
	}
	return &cr, nil
}

// ResolveRepoURL is the convenience method Layer 6's resolver wraps.
// Fetches crate metadata and returns the repository URL (normalized)
// or empty string if no repository is declared.
func (c *Client) ResolveRepoURL(ctx context.Context, name string) (string, error) {
	cr, err := c.GetCrate(ctx, name)
	if err != nil {
		return "", err
	}
	if cr.Crate.Repository == "" {
		return "", nil
	}
	return NormalizeDeclaredRepoURL(cr.Crate.Repository), nil
}
