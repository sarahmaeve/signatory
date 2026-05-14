package pypi

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"

	"github.com/sarahmaeve/signatory/internal/httpx"
)

// ErrNotFound is the sentinel callers compare via errors.Is when PyPI
// reports a project is absent. It wraps httpx.ErrNotFound so callers
// can also do errors.Is(err, httpx.ErrNotFound) for ecosystem-agnostic
// absence detection — both checks resolve true against the chain.
var ErrNotFound = fmt.Errorf("pypi: %w", httpx.ErrNotFound)

// maxResponseSize is the registry response-body cap (10 MiB). The
// bounding itself is performed by httpx, whose default matches this
// value; the constant is retained because tests reference it to drive
// the oversize-body cap check.
const maxResponseSize = 10 * 1024 * 1024

// attestationMaxSize is the tighter cap for the Integrity API.
// Real attestation responses are typically <10 KiB; the 256 KiB
// ceiling is a tight abuse defense passed per-request via
// httpx.WithRequestMaxBytes.
const attestationMaxSize = 256 * 1024

// maxPackageNameLength is PyPI's published cap on package names.
const maxPackageNameLength = 100

// pypiPackageName matches the PEP 508 case-insensitive name grammar.
// Names that pass through profile.NormalizePyPIName arrive here in
// PEP 503 canonical form (lowercase, separators collapsed); the
// permissive case-insensitive grammar accepts both shapes —
// defense in depth for any caller that bypasses the normalize step.
//
// Stricter than the published PEP 508 in two ways the registry has
// converged on:
//
//   - Single-character names are accepted (registry has them).
//   - The name body excludes path-traversal characters, percent-
//     encoding, query separators, and whitespace by construction
//     (the character class is the union allowed by PEP 508).
var pypiPackageName = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]*[A-Za-z0-9])?$`)

// ValidatePackageName enforces the package-name grammar before any
// URL construction. Per npm's #90 lesson: user-influenced bytes
// substituted into an HTTP URL are a path/query/fragment injection
// surface — validate at the function boundary, not after building
// the URL. Callers never pass attacker-controlled strings today,
// but the signature makes it easy for a future caller to introduce
// that bug; the validator closes the hole up front.
func ValidatePackageName(name string) error {
	if name == "" {
		return fmt.Errorf("package name is empty")
	}
	if len(name) > maxPackageNameLength {
		return fmt.Errorf("package name exceeds PyPI's %d-byte maximum (got %d)",
			maxPackageNameLength, len(name))
	}
	if !pypiPackageName.MatchString(name) {
		return fmt.Errorf("package name %q does not match PEP 508 grammar (allowed: A-Z a-z 0-9 . _ -, must start and end with alphanumeric)", name)
	}
	return nil
}

// Client is a narrow PyPI registry HTTP client. The defensive
// network discipline (timeouts, HTTPS-only redirects, response-body
// cap, drain-and-discard on non-2xx, sanitized status errors) lives
// in httpx.SecureClient; this package owns input validation,
// sentinel-error wrapping, and the PEP 740 Integrity API's
// nil-on-404 semantic.
type Client struct {
	api *httpx.SecureClient
}

// NewClient returns a Client bound to the public PyPI endpoint.
func NewClient() *Client {
	return &Client{
		api: httpx.NewSecureClient(httpx.WithBaseURL("https://pypi.org")),
	}
}

// NewClientWithBaseURL returns a Client whose endpoint points at
// the supplied base. Primary use case: test harnesses pointing the
// client at an httptest server. Production code calls NewClient.
func NewClientWithBaseURL(base string) *Client {
	return &Client{
		api: httpx.NewSecureClient(httpx.WithBaseURL(base)),
	}
}

// GetProject fetches the full project response from PyPI's legacy
// JSON endpoint. Returns ErrNotFound (wrapped) on 404. Other non-2xx
// statuses surface as a sanitized error — the response body is
// discarded before reaching the caller (#93's drain-on-error
// discipline, enforced by httpx).
func (c *Client) GetProject(ctx context.Context, name string) (*Project, error) {
	if err := ValidatePackageName(name); err != nil {
		return nil, fmt.Errorf("get project: %w", err)
	}

	// Path-escape the name for defense-in-depth even though
	// ValidatePackageName already constrains the byte set to one
	// that needs no escaping. If the validator ever loosens, the
	// URL construction stays safe.
	var proj Project
	err := c.api.GetJSON(ctx, "/pypi/"+url.PathEscape(name)+"/json", &proj,
		httpx.WithHeader("Accept", "application/json"))
	if err != nil {
		if errors.Is(err, httpx.ErrNotFound) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
		}
		return nil, fmt.Errorf("pypi registry request for %q: %w", name, err)
	}
	return &proj, nil
}

// GetProjectInfo fetches the info block for a project from PyPI's
// legacy JSON endpoint. Convenience wrapper around GetProject for
// callers that only need the Info section (e.g., the resolver).
func (c *Client) GetProjectInfo(ctx context.Context, name string) (*Info, error) {
	proj, err := c.GetProject(ctx, name)
	if err != nil {
		return nil, err
	}
	return &proj.Info, nil
}

// GetAttestation fetches PEP 740 Sigstore attestation data for a
// specific distribution file from PyPI's Integrity API. The endpoint
// is /integrity/<project>/<version>/<filename>/provenance.
//
// Returns nil (not an error) on 404 — the absence of attestation is
// a valid signal state (the publisher hasn't opted into trusted
// publishing). Other non-2xx statuses surface as errors.
//
// The Integrity API is GA since November 2024 and returns a JSON
// envelope containing attestation bundles with publisher OIDC
// identity. Phase A reads the publisher block only; Phase B (future)
// would verify the DSSE envelope against Sigstore's transparency log.
func (c *Client) GetAttestation(ctx context.Context, project, version, filename string) (*AttestationResponse, error) {
	if err := ValidatePackageName(project); err != nil {
		return nil, fmt.Errorf("get attestation: %w", err)
	}
	if version == "" {
		return nil, fmt.Errorf("get attestation: version is empty")
	}
	if filename == "" {
		return nil, fmt.Errorf("get attestation: filename is empty")
	}

	// Each path segment is escaped independently — defense-in-depth
	// against injection via version or filename strings (both are
	// publisher-supplied values from the release data, not attacker-
	// controlled in practice, but the client boundary closes the hole
	// up front).
	path := "/integrity/" + url.PathEscape(project) + "/" +
		url.PathEscape(version) + "/" +
		url.PathEscape(filename) + "/provenance"

	var attest AttestationResponse
	err := c.api.GetJSON(ctx, path, &attest,
		httpx.WithHeader("Accept", "application/json"),
		httpx.WithRequestMaxBytes(attestationMaxSize),
	)
	if err != nil {
		if errors.Is(err, httpx.ErrNotFound) {
			// 404 means no attestation exists for this distribution.
			// Valid state — publisher hasn't opted in.
			return nil, nil
		}
		return nil, fmt.Errorf("pypi integrity request for %s/%s/%s: %w",
			project, version, filename, err)
	}

	// Validate publisher identity fields. These are security-load-
	// bearing: signatory uses Kind+Repository+Workflow to determine
	// whether a package has trusted CI/CD provenance. Control chars
	// or invalid UTF-8 in these fields indicate a corrupted or
	// adversarial response — reject rather than propagate garbage
	// that might false-match a trust rule.
	for i, bundle := range attest.Bundles {
		pub := bundle.Publisher
		for _, s := range []string{pub.Kind, pub.Repository, pub.Workflow, pub.Environment} {
			if containsControlChars(s) {
				return nil, fmt.Errorf("pypi attestation bundle[%d] publisher contains control characters",
					i)
			}
		}
	}
	return &attest, nil
}

// containsControlChars reports whether s includes any ASCII control
// character (0x00–0x1F) other than horizontal tab (0x09). These never
// appear in legitimate publisher identities or workflow paths.
func containsControlChars(s string) bool {
	for _, r := range s {
		if r < 0x20 && r != '\t' {
			return true
		}
	}
	return false
}
