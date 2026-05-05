package pypi

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

// ErrNotFound is returned (wrapped via %w) when the PyPI registry
// responds 404 for a project lookup. Callers that treat absence as
// a signal compare with errors.Is.
var ErrNotFound = errors.New("pypi: not found")

// maxResponseSize bounds registry response bodies to prevent OOM
// from unbounded streams. The legacy /pypi/<name>/json endpoint
// embeds the entire historical release map, which can run to several
// MB for popular packages (boto3, numpy, tensorflow); 10 MB matches
// npm's cap and covers the long tail of real PyPI projects.
//
// Outlier mega-packages at unversioned URIs that exceed the cap are
// expected to fail closed — the workaround for those is to pin a
// version (uses the per-release endpoint, which is always small).
// That optimization is deferred until Layer 5 lands.
const maxResponseSize = 10 * 1024 * 1024

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

// Client is a narrow PyPI registry HTTP client. Exposes only the
// methods Layer 6's source resolver needs; extending the surface
// (Layer 5's collector, PEP 740 trusted-publishing, downloads stats)
// requires modelling additional response structures plus adding
// validation and size-bound tests for each new call.
type Client struct {
	httpClient  *http.Client
	registryURL string
}

// NewClient returns a Client bound to the public PyPI endpoint.
// The 60s per-request timeout matches the npm and github clients —
// the registry can be slow under load, and a shorter deadline would
// collapse the resolution run into a blanket absence.
func NewClient() *Client {
	return &Client{
		httpClient: &http.Client{
			Timeout:       60 * time.Second,
			CheckRedirect: checkRedirect,
		},
		registryURL: "https://pypi.org",
	}
}

// NewClientWithBaseURL returns a Client whose endpoint points at
// the supplied base. Primary use case: test harnesses pointing the
// client at an httptest server. Production code calls NewClient.
func NewClientWithBaseURL(base string) *Client {
	return &Client{
		httpClient: &http.Client{
			Timeout:       60 * time.Second,
			CheckRedirect: checkRedirect,
		},
		registryURL: base,
	}
}

// checkRedirect enforces the two redirect invariants the npm and
// github clients also enforce:
//
//  1. Refuse any redirect target that isn't HTTPS. Scheme downgrade
//     to plaintext has no legitimate use case on the public registry;
//     refusing is loud, silently following would mask a misconfig.
//  2. Bound the redirect chain to <10 hops.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if req.URL.Scheme != "https" {
		return fmt.Errorf("refusing redirect to non-HTTPS URL %s", req.URL.Redacted())
	}
	if len(via) >= 10 {
		return fmt.Errorf("too many redirects")
	}
	return nil
}

// GetProject fetches the full project response from PyPI's legacy
// JSON endpoint. Returns ErrNotFound (wrapped) on 404. Other non-2xx
// statuses surface as a sanitized error — the response body is
// discarded before reaching the caller (npm's #93 applies
// symmetrically to PyPI).
func (c *Client) GetProject(ctx context.Context, name string) (*Project, error) {
	if err := ValidatePackageName(name); err != nil {
		return nil, fmt.Errorf("get project: %w", err)
	}

	// Path-escape the name for defense-in-depth even though
	// ValidatePackageName already constrains the byte set to one
	// that needs no escaping. If the validator ever loosens, the
	// URL construction stays safe.
	escapedName := url.PathEscape(name)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.registryURL+"/pypi/"+escapedName+"/json", nil)
	if err != nil {
		return nil, fmt.Errorf("build request for %q: %w", name, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "signatory/0.1")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pypi registry request for %q failed: %w", name, err)
	}
	defer resp.Body.Close() //nolint:errcheck // response body close; err is not actionable

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Drain-and-discard so the connection can be reused; do
		// NOT include the body in the error string (#93 lesson).
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseSize))
		return nil, fmt.Errorf("pypi registry returned status %d for %q",
			resp.StatusCode, name)
	}

	// Bound the response before decoding. A malicious or malfunc-
	// tioning server streaming an unbounded body could otherwise
	// exhaust memory — the json.Decoder doesn't cap input size.
	limited := io.LimitReader(resp.Body, maxResponseSize+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read pypi registry response for %q: %w", name, err)
	}
	if int64(len(body)) > maxResponseSize {
		return nil, fmt.Errorf("pypi registry response for %q exceeds %d-byte cap",
			name, maxResponseSize)
	}

	var proj Project
	if err := json.Unmarshal(body, &proj); err != nil {
		return nil, fmt.Errorf("decode pypi registry response for %q: %w", name, err)
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

	// Construct the Integrity API URL. Each path segment is escaped
	// independently — defense-in-depth against injection via version
	// or filename strings (both are publisher-supplied values from the
	// release data, not attacker-controlled in practice, but the client
	// boundary closes the hole up front).
	escapedProject := url.PathEscape(project)
	escapedVersion := url.PathEscape(version)
	escapedFilename := url.PathEscape(filename)

	reqURL := c.registryURL + "/integrity/" + escapedProject + "/" +
		escapedVersion + "/" + escapedFilename + "/provenance"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build attestation request for %s/%s/%s: %w",
			project, version, filename, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "signatory/0.1")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pypi integrity request for %s/%s/%s failed: %w",
			project, version, filename, err)
	}
	defer resp.Body.Close() //nolint:errcheck // response body close; err is not actionable

	if resp.StatusCode == http.StatusNotFound {
		// 404 means no attestation exists for this distribution.
		// This is a valid state — the publisher hasn't opted in.
		return nil, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseSize))
		return nil, fmt.Errorf("pypi integrity returned status %d for %s/%s/%s",
			resp.StatusCode, project, version, filename)
	}

	// Attestation responses are small (typically < 10 KB); use a
	// tighter cap than the project endpoint.
	const attestationMaxSize = 256 * 1024
	limited := io.LimitReader(resp.Body, attestationMaxSize+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read pypi integrity response for %s/%s/%s: %w",
			project, version, filename, err)
	}
	if int64(len(body)) > attestationMaxSize {
		return nil, fmt.Errorf("pypi integrity response for %s/%s/%s exceeds %d-byte cap",
			project, version, filename, attestationMaxSize)
	}

	var attest AttestationResponse
	if err := json.Unmarshal(body, &attest); err != nil {
		return nil, fmt.Errorf("decode pypi integrity response for %s/%s/%s: %w",
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
