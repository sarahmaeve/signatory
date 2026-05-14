package npm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/sarahmaeve/signatory/internal/httpx"
)

// ErrNotFound is the sentinel callers compare via errors.Is when
// the npm registry reports a package is absent. Wraps
// httpx.ErrNotFound so callers can also do errors.Is(err,
// httpx.ErrNotFound) for ecosystem-agnostic absence detection.
var ErrNotFound = fmt.Errorf("npm: %w", httpx.ErrNotFound)

// downloadsMaxSize bounds the api.npmjs.org downloads response. The
// downloads endpoint returns one small JSON object (a few hundred
// bytes); the 64 KiB ceiling is a much tighter bound than the
// registry endpoint's 10 MiB default — the smaller the legitimate
// payload, the smaller the abuse cap should be.
const downloadsMaxSize = 64 * 1024

// maxPackageNameLength matches npm's published limit.
const maxPackageNameLength = 214

// Package-name grammar for npm, per the registry's own rules plus
// pragmatic hardening:
//
//   - Unscoped: starts with an alphanumeric; subsequent characters
//     in [A-Za-z0-9._-].
//   - Scoped:   @<scope>/<name> where each of scope/name follows
//     the unscoped rules.
//
// Stricter than the historical npm spec (which allowed a wider
// character set) because we're gating a URL path component; the
// registry has long since converged on this subset for new
// publications.
var (
	npmUnscopedName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	npmScopedName   = regexp.MustCompile(`^@[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

// ValidatePackageName enforces the package-name grammar before any
// URL construction. Per #90's lesson: user-influenced bytes that get
// substituted into an HTTP URL are a path/query/fragment injection
// surface — validate at the function boundary, not after building
// the URL. Callers never pass attacker-controlled strings today, but
// the signature makes it easy for a future caller to introduce that
// bug; the validator closes the hole up front.
func ValidatePackageName(name string) error {
	if name == "" {
		return fmt.Errorf("package name is empty")
	}
	if len(name) > maxPackageNameLength {
		return fmt.Errorf("package name exceeds npm's %d-byte maximum (got %d)",
			maxPackageNameLength, len(name))
	}
	if strings.HasPrefix(name, "@") {
		if !npmScopedName.MatchString(name) {
			return fmt.Errorf("scoped package name %q does not match @scope/name grammar", name)
		}
		return nil
	}
	if !npmUnscopedName.MatchString(name) {
		return fmt.Errorf("package name %q contains disallowed characters (allowed: A-Z a-z 0-9 . _ -)", name)
	}
	return nil
}

// Client is a narrow npm registry HTTP client. The defensive network
// discipline (timeouts, HTTPS-only redirects, response-body cap,
// drain-and-discard on non-2xx, sanitized status errors) lives in
// httpx.SecureClient; this package owns input validation, sentinel-
// error wrapping, the polymorphic Repository decoder, and the
// downloads endpoint's strict-decode + tighter-cap policy.
//
// Two SecureClients because npm's API is split across two hosts:
// registry.npmjs.org serves package metadata; api.npmjs.org serves
// download statistics. Tests point both at a single httptest server
// that multiplexes on path; production separates them.
type Client struct {
	registryAPI  *httpx.SecureClient
	downloadsAPI *httpx.SecureClient
}

// NewClient returns a Client bound to the public npm endpoints.
func NewClient() *Client {
	return &Client{
		registryAPI:  httpx.NewSecureClient(httpx.WithBaseURL("https://registry.npmjs.org")),
		downloadsAPI: httpx.NewSecureClient(httpx.WithBaseURL("https://api.npmjs.org")),
	}
}

// NewClientWithBaseURL returns a Client whose endpoints both point
// at the supplied base. Primary use case: test harnesses pointing
// the client at an httptest server that multiplexes registry and
// downloads requests by URL path. Production code should call
// NewClient, which separates the two hosts as npm does.
func NewClientWithBaseURL(base string) *Client {
	return &Client{
		registryAPI:  httpx.NewSecureClient(httpx.WithBaseURL(base)),
		downloadsAPI: httpx.NewSecureClient(httpx.WithBaseURL(base)),
	}
}

// newClientWithBaseURL is the same, kept as a package-internal alias
// for the existing test suite.
func newClientWithBaseURL(base string) *Client {
	return NewClientWithBaseURL(base)
}

// RegistryPackage models the subset of the npm registry's package
// metadata response that signatory reads. Fields not modelled here
// are ignored — unlike our yaml-strict-mode discipline for analyst
// frontmatter (where we control the schema), the npm registry emits
// dozens of optional fields (readme, bugs, users, _id, _rev, ...) we
// don't care about. Strict-decode here would produce false-positive
// parse failures on normal traffic; instead we test the fields we DO
// read and accept drift on fields we don't.
type RegistryPackage struct {
	Name     string               `json:"name"`
	DistTags DistTags             `json:"dist-tags"`
	Time     map[string]time.Time `json:"time"`

	// Fields below are modelled for Phase B; Phase A ignores them
	// but they land here so the struct is stable across commits.

	Maintainers []Maintainer              `json:"maintainers"`
	Versions    map[string]PackageVersion `json:"versions"`
	Repository  Repository                `json:"repository"`
}

// DistTags carries the "latest" pointer the collector uses to pick
// the version record to read signals from.
type DistTags struct {
	Latest string `json:"latest"`
}

// Maintainer is a single entry in the top-level maintainers array.
type Maintainer struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// PackageVersion is the per-version metadata block. Phase B reads
// Scripts.Postinstall and Dist.Attestations; Phase B.6's longitudinal
// signals add NpmUser for cross-version publisher-continuity analysis.
// GitHead carries the publisher-stamped commit SHA (npm v≥5) the
// artifact-vs-repo collector uses for exact-pair confidence.
// Dependencies and OptionalDependencies feed git_url_dep_introduced
// (TanStack/Mini-Shai-Hulud 2026-05-11 injected
// `optionalDependencies: "@tanstack/setup": "github:tanstack/router#<sha>"`
// — a git-URL dep with a hardcoded SHA pointing at attacker content).
// Other fields on the wire are not modelled.
type PackageVersion struct {
	Scripts              Scripts           `json:"scripts"`
	Dist                 Dist              `json:"dist"`
	NpmUser              NpmUser           `json:"_npmUser"`
	GitHead              string            `json:"gitHead"`
	Dependencies         map[string]string `json:"dependencies"`
	OptionalDependencies map[string]string `json:"optionalDependencies"`
}

// NpmUser identifies who ran `npm publish` for a given version.
// The registry stamps this field at receive time — the maintainer
// cannot rewrite it post-publish — so transitions in NpmUser.Name
// across recent versions are a load-bearing publish-provenance
// signal. Email is deliberately NOT emitted downstream (PII); we
// parse it to avoid future-you being surprised by a strict-decode
// failure if someone tightens the struct later.
type NpmUser struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// Scripts holds lifecycle script declarations. postinstall is the
// axios-case-study-relevant surface.
type Scripts struct {
	Postinstall string `json:"postinstall"`
}

// Dist holds distribution metadata. Attestations are OIDC trusted-
// publishing records when present; modelled as RawMessage because
// the exact shape varies (and may change) and we only check presence
// for v0.1.
//
// Tarball is the registry-hosted URL of the published .tgz; Integrity
// is the npm-supplied subresource-integrity string for the same bytes.
// Both feed the artifact_url signal that the artifact-vs-repo collector
// consumes downstream — see internal/signal/artifact for the threat
// model anchor (CVE-2024-3094).
type Dist struct {
	Attestations json.RawMessage `json:"attestations"`
	Tarball      string          `json:"tarball"`
	Integrity    string          `json:"integrity"`
}

// Repository is polymorphic in the npm registry: may be a bare
// string ("https://github.com/expressjs/express") or an object
// ({type:"git", url:"..."}) or absent entirely. UnmarshalJSON
// normalizes both shapes to (Type, URL).
type Repository struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}

// UnmarshalJSON accepts either shape that appears in registry
// responses. Absence (null or missing) leaves the zero struct.
// Returns an error if the decoded URL or Type contains ASCII control
// characters — a legitimate registry response never includes them;
// their presence signals corruption or an adversarial payload.
func (r *Repository) UnmarshalJSON(data []byte) error {
	// Try string first: `"repository": "github:user/repo"` or
	// `"repository": "https://..."`.
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		if containsControlChars(s) {
			return fmt.Errorf("repository URL contains control characters")
		}
		r.URL = s
		return nil
	}
	// Fall through to object. Use a local type alias so the call
	// doesn't recurse into this UnmarshalJSON.
	type repoAlias Repository
	var obj repoAlias
	if err := json.Unmarshal(data, &obj); err != nil {
		return fmt.Errorf("repository field: expected string or object, got %s", string(data))
	}
	if containsControlChars(obj.URL) || containsControlChars(obj.Type) {
		return fmt.Errorf("repository object contains control characters")
	}
	*r = Repository(obj)
	return nil
}

// containsControlChars reports whether s includes any ASCII control
// character (0x00–0x1F) other than horizontal tab (0x09). These never
// appear in legitimate registry URLs or identifiers.
func containsControlChars(s string) bool {
	for _, r := range s {
		if r < 0x20 && r != '\t' {
			return true
		}
	}
	return false
}

// GetPackage fetches metadata for a package from the registry.
// Returns ErrNotFound (wrapped) on 404. Other non-2xx statuses
// surface as sanitized errors (the response body is discarded before
// it reaches the caller — #93's drain-on-error discipline, enforced
// by httpx).
func (c *Client) GetPackage(ctx context.Context, name string) (*RegistryPackage, error) {
	if err := ValidatePackageName(name); err != nil {
		return nil, fmt.Errorf("get package: %w", err)
	}

	// Path-escape the name — '/' between scope and package in a
	// scoped name must be percent-encoded when it lands in the URL
	// path segment, because the registry's URL grammar treats the
	// scope as a single path element. url.PathEscape handles the '@'
	// too.
	var pkg RegistryPackage
	err := c.registryAPI.GetJSON(ctx, "/"+url.PathEscape(name), &pkg,
		httpx.WithHeader("Accept", "application/json"))
	if err != nil {
		if errors.Is(err, httpx.ErrNotFound) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
		}
		return nil, fmt.Errorf("npm registry request for %q: %w", name, err)
	}
	return &pkg, nil
}

// downloadsResponse models the api.npmjs.org downloads endpoint.
// Schema is narrow and stable, so DisallowUnknownFields is
// applicable here (unlike the main registry response where we
// deliberately accept drift on fields we don't read).
type downloadsResponse struct {
	Downloads int    `json:"downloads"`
	Start     string `json:"start"`
	End       string `json:"end"`
	Package   string `json:"package"`
}

// GetWeeklyDownloads fetches the last-week download count for a
// package from api.npmjs.org/downloads. Returns ErrNotFound
// (wrapped) on 404 — which happens for packages the downloads
// service doesn't have stats for, or newly-published packages
// before their first reporting window.
//
// Counts are self-reported by the registry and gameable; the
// weekly_downloads signal's ForgeryResistance reflects that. Use
// as one input to a criticality picture, never as a sole basis for
// a trust decision.
func (c *Client) GetWeeklyDownloads(ctx context.Context, name string) (int, error) {
	if err := ValidatePackageName(name); err != nil {
		return 0, fmt.Errorf("get weekly downloads: %w", err)
	}

	var dl downloadsResponse
	err := c.downloadsAPI.GetJSON(ctx,
		"/downloads/point/last-week/"+url.PathEscape(name), &dl,
		httpx.WithHeader("Accept", "application/json"),
		httpx.WithRequestMaxBytes(downloadsMaxSize),
		// Strict decode: downloads schema is stable and narrow.
		// Unknown fields here signal real drift we want to notice —
		// unlike the main registry response where drift is normal
		// traffic.
		httpx.WithStrictJSONDecode(),
	)
	if err != nil {
		if errors.Is(err, httpx.ErrNotFound) {
			return 0, fmt.Errorf("%w: %s (no download stats)", ErrNotFound, name)
		}
		return 0, fmt.Errorf("npm downloads request for %q: %w", name, err)
	}

	// Guard against malicious/malformed responses serving negative
	// counts. JSON decodes them into int without complaint; downstream
	// code using this value for criticality scoring must not see
	// nonsensical negatives.
	if dl.Downloads < 0 {
		return 0, fmt.Errorf("npm downloads response for %q reports negative count (%d)",
			name, dl.Downloads)
	}
	return dl.Downloads, nil
}
