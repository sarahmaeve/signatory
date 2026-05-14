// Package cargo provides an HTTP client for the crates.io registry
// API. Phase A exposes only the surface Layer 6's source resolver
// needs (GetCrate for the repository URL); Phase B extends it with
// owners, per-version metadata, and the full signal-collector surface.
//
// The defensive network discipline (HTTPS-only redirects, response-
// body cap, drain-and-discard on non-2xx, sanitized status errors)
// lives in httpx.SecureClient; this package owns input validation,
// sentinel-error wrapping, and crates.io's mandatory User-Agent
// requirement.
package cargo

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"

	"github.com/sarahmaeve/signatory/internal/httpx"
)

// ErrNotFound is the sentinel callers compare via errors.Is when
// crates.io reports a crate is absent. Wraps httpx.ErrNotFound so
// callers can also do errors.Is(err, httpx.ErrNotFound) for
// ecosystem-agnostic absence detection — both checks resolve true.
var ErrNotFound = fmt.Errorf("cargo: %w", httpx.ErrNotFound)

// maxCrateNameLength is crates.io's published cap.
const maxCrateNameLength = 64

// crateNamePattern matches the crates.io name grammar: starts with a
// letter, then letters, digits, hyphens, or underscores. Max 64 chars
// is enforced separately by length check.
var crateNamePattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]*$`)

// crateUserAgent is the User-Agent the cargo client sends. crates.io
// REQUIRES a User-Agent; requests without one get 403. Per crates.io's
// published guidance, operators include a contact URL so the registry
// team can reach the source of unusual traffic.
const crateUserAgent = "signatory/0.1 (https://github.com/sarahmaeve/signatory)"

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
// GetCrate (for source resolution) and GetOwners; Phase B will extend
// the surface with version metadata.
type Client struct {
	api *httpx.SecureClient
}

// NewClient returns a Client bound to the public crates.io endpoint.
func NewClient() *Client {
	return &Client{
		api: httpx.NewSecureClient(
			httpx.WithBaseURL("https://crates.io"),
			httpx.WithUserAgent(crateUserAgent),
		),
	}
}

// NewClientWithBaseURL returns a Client pointing at the supplied base.
// Primary use: test harnesses with httptest servers.
func NewClientWithBaseURL(base string) *Client {
	return &Client{
		api: httpx.NewSecureClient(
			httpx.WithBaseURL(base),
			httpx.WithUserAgent(crateUserAgent),
		),
	}
}

// GetCrate fetches the crate metadata from crates.io's JSON API.
// Returns ErrNotFound (wrapped) on 404. Other non-2xx statuses
// surface as sanitized errors (the response body is discarded before
// it reaches the caller — #93's drain-on-error discipline, enforced
// by httpx).
//
// Phase A models only Crate.Repository (for source resolution);
// Phase B extends CrateResponse with Versions, owners, etc.
func (c *Client) GetCrate(ctx context.Context, name string) (*CrateResponse, error) {
	if err := ValidateCrateName(name); err != nil {
		return nil, fmt.Errorf("get crate: %w", err)
	}

	var cr CrateResponse
	err := c.api.GetJSON(ctx, "/api/v1/crates/"+url.PathEscape(name), &cr,
		httpx.WithHeader("Accept", "application/json"))
	if err != nil {
		if errors.Is(err, httpx.ErrNotFound) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
		}
		return nil, fmt.Errorf("crates.io request for %q: %w", name, err)
	}
	return &cr, nil
}

// GetOwners fetches the crate's owner list from crates.io. Returns
// ErrNotFound (wrapped) on 404. A non-nil error on this endpoint
// does NOT block signal collection — callers degrade gracefully and
// record the absence for owner-derived signals only.
func (c *Client) GetOwners(ctx context.Context, name string) (*OwnersResponse, error) {
	if err := ValidateCrateName(name); err != nil {
		return nil, fmt.Errorf("get owners: %w", err)
	}

	var or OwnersResponse
	err := c.api.GetJSON(ctx, "/api/v1/crates/"+url.PathEscape(name)+"/owners", &or,
		httpx.WithHeader("Accept", "application/json"))
	if err != nil {
		if errors.Is(err, httpx.ErrNotFound) {
			return nil, fmt.Errorf("%w: owners for %s", ErrNotFound, name)
		}
		return nil, fmt.Errorf("crates.io owners request for %q: %w", name, err)
	}
	return &or, nil
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
