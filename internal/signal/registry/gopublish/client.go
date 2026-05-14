package gopublish

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sarahmaeve/signatory/internal/httpx"
)

// ErrNotFound is the sentinel callers compare via errors.Is when an
// upstream endpoint reports the module / version is absent. The
// proxy returns 404 for "module not in the proxy" and 410 Gone for
// retracted / deleted versions; both map to ErrNotFound — the
// collector records absence either way. sum.golang.org's /lookup
// also returns 404 when the (module, version) isn't in the log.
//
// Wraps httpx.ErrNotFound so callers can also do
// errors.Is(err, httpx.ErrNotFound) for ecosystem-agnostic absence
// detection.
var ErrNotFound = fmt.Errorf("gopublish: %w", httpx.ErrNotFound)

// modulePathMaxLen is a defensive cap on the module-path length
// before validation runs. Real module paths top out under 200
// bytes; capping at 1 KiB keeps validation cheap on adversarial
// inputs without ever rejecting a legitimate module.
const modulePathMaxLen = 1024

// ValidateModulePath enforces the grammar of a Go module path
// before the client substitutes it into a URL. The Go reference
// (https://golang.org/ref/mod#go-mod-file-ident) constrains
// module paths to:
//
//   - Path elements separated by single forward slashes, none of
//     which is empty.
//   - Each path element a non-empty sequence of ASCII letters,
//     digits, dots, hyphens, underscores, plus tilde for legacy
//     paths. (We accept the Go-published character set; we do
//     NOT accept whitespace, control characters, or URL-syntactic
//     punctuation that would re-parse the request.)
//   - At least two path elements (a module path always has a
//     domain followed by at least one segment).
//
// Validation is at the function boundary (per the npm client's
// #90 lesson) so a future caller threading in attacker-controlled
// strings can't smuggle path/query/fragment metacharacters into a
// proxy URL.
func ValidateModulePath(path string) error {
	if path == "" {
		return fmt.Errorf("module path is empty")
	}
	if len(path) > modulePathMaxLen {
		return fmt.Errorf("module path exceeds %d-byte cap (got %d)", modulePathMaxLen, len(path))
	}
	if strings.HasPrefix(path, "/") {
		return fmt.Errorf("module path must not start with '/'")
	}
	if strings.HasSuffix(path, "/") {
		return fmt.Errorf("module path must not end with '/'")
	}
	if strings.Contains(path, "//") {
		return fmt.Errorf("module path must not contain '//'")
	}
	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		return fmt.Errorf("module path needs at least one '/' (got %q)", path)
	}
	for _, p := range parts {
		if p == "" {
			return fmt.Errorf("module path %q has empty segment", path)
		}
		if p == "." || p == ".." {
			return fmt.Errorf("module path %q contains traversal segment %q", path, p)
		}
		for _, r := range p {
			if !isModulePathRune(r) {
				return fmt.Errorf("module path %q contains disallowed character %q", path, r)
			}
		}
	}
	return nil
}

// isModulePathRune is the per-segment character predicate. Tighter
// than the Go reference (which allows additional Unicode classes
// in legacy paths) — we keep ASCII-only since proxy.golang.org
// rejects non-ASCII anyway and rejecting at our boundary keeps
// the URL trivially safe.
func isModulePathRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z':
		return true
	case r >= 'A' && r <= 'Z':
		return true
	case r >= '0' && r <= '9':
		return true
	case r == '.' || r == '-' || r == '_' || r == '~':
		return true
	}
	return false
}

// encodeModulePath applies proxy.golang.org's case-encoding rule:
// every uppercase letter becomes `!` followed by its lower-case
// equivalent. Path separators and other allowed punctuation pass
// through unchanged. Reference:
// https://golang.org/ref/mod#goproxy-protocol — "the case of
// letters in the module path or version is encoded by replacing
// every uppercase letter with an exclamation mark followed by the
// corresponding lower-case letter."
//
// Without this step, modules with capitalized owners (e.g.,
// `github.com/AzureAD/...`) 404 against the real proxy because
// the proxy's storage is case-sensitive over the bang-encoded
// name.
func encodeModulePath(path string) string {
	var b strings.Builder
	b.Grow(len(path) + 8)
	for _, r := range path {
		if r >= 'A' && r <= 'Z' {
			b.WriteByte('!')
			b.WriteRune(r + ('a' - 'A'))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Client is a narrow Go-publish data-plane HTTP client. The
// defensive network discipline (timeouts, HTTPS-only redirects,
// response-body cap, drain-and-discard on non-2xx, sanitized status
// errors) lives in three httpx.SecureClients: one each for the
// proxy, sum, and vanity-host endpoints. This package owns input
// validation, the proxy's @v/list newline shape, the transparency-
// log leaf-id parsing, and the vanity-host's "all errors are not-
// resolvable, never an error" semantic.
//
// The proxy and sum SecureClients are configured with
// WithNotFoundStatuses(404, 410): a 410 Gone from the proxy
// (retracted / deleted version) is treated the same as a 404 for
// collection purposes — both mean "record absence."
type Client struct {
	proxyAPI   *httpx.SecureClient
	sumAPI     *httpx.SecureClient
	metaTagAPI *httpx.SecureClient

	// proxyURL is duplicated from proxyAPI's baseURL because ZipURL
	// builds a URL string (consumed by the artifact-vs-repo
	// collector) without making an HTTP request. The string must
	// match what proxyAPI sends on the wire.
	proxyURL string

	// metaTagURLPrefix overrides the vanity-host base for resolve.go's
	// meta-tag fallback. Empty (production) → resolveViaMetaTag fetches
	// "https://<modulePath>?go-get=1" directly. Non-empty (tests) →
	// "<metaTagURLPrefix>/<modulePath>?go-get=1" so the fetch hits
	// the test's httptest server. NEVER set this in production —
	// it would route legitimate vanity-host fetches through an
	// arbitrary URL.
	metaTagURLPrefix string
}

// Production endpoints. Kept as constants so the test-only
// NewClientWithBaseURL[s] can sit alongside the production NewClient
// in one file.
const (
	publicProxyURL = "https://proxy.golang.org"
	publicSumURL   = "https://sum.golang.org"
)

// NewClient returns a Client bound to the public Go endpoints —
// proxy.golang.org for module metadata and sum.golang.org for
// transparency-log lookups.
func NewClient() *Client {
	return buildClient(publicProxyURL, publicSumURL, "")
}

// NewClientWithBaseURL returns a Client whose proxy and sum
// endpoints point at the supplied bases. Tests typically pass the
// same httptest server URL twice; production wires the two distinct
// hosts via NewClient.
//
// Meta-tag fetches (the vanity-host fallback inside ResolveRepoURL)
// route to the live https://<modulePath>?go-get=1 URL; tests that
// exercise the fallback need to use NewClientWithBaseURLs to
// override that target.
func NewClientWithBaseURL(proxy, sum string) *Client {
	return buildClient(proxy, sum, "")
}

// NewClientWithBaseURLs returns a Client with explicit overrides
// for all three URL bases: proxy, sum, and the meta-tag vanity-host
// prefix. Tests use this to wire ResolveRepoURL's meta-tag fallback
// into an httptest server so no real vanity hosts are contacted.
//
// Production callers should use NewClient (live endpoints).
// metaTagBase empty preserves the production behavior of fetching
// the live <modulePath>?go-get=1 URL.
func NewClientWithBaseURLs(proxy, sum, metaTagBase string) *Client {
	return buildClient(proxy, sum, metaTagBase)
}

// buildClient is the shared constructor implementation. Three
// SecureClients are constructed because the three endpoints carry
// independent base URLs (and the meta-tag client has none — it
// receives a full URL per request).
func buildClient(proxy, sum, metaTagBase string) *Client {
	notFound := httpx.WithNotFoundStatuses(http.StatusNotFound, http.StatusGone)
	return &Client{
		proxyAPI: httpx.NewSecureClient(
			httpx.WithBaseURL(proxy),
			notFound,
		),
		sumAPI: httpx.NewSecureClient(
			httpx.WithBaseURL(sum),
			notFound,
		),
		// metaTagAPI has no baseURL — resolveViaMetaTag passes a
		// fully-qualified URL because each module maps to a
		// different vanity host.
		metaTagAPI:       httpx.NewSecureClient(),
		proxyURL:         proxy,
		metaTagURLPrefix: metaTagBase,
	}
}

// LatestInfo is the @latest response shape — Version is the
// proxy's current "what's the highest version we know about"
// pointer, Time is the publish timestamp.
type LatestInfo struct {
	Version string    `json:"Version"`
	Time    time.Time `json:"Time"`
}

// VersionInfo is the .info response shape with Origin metadata
// included for go ≥ 1.20 publishers. Origin gives the proxy's
// declared VCS source for the version — useful as an additional
// cross-check against the entity's resolved repo URL.
type VersionInfo struct {
	Version string    `json:"Version"`
	Time    time.Time `json:"Time"`
	Origin  Origin    `json:"Origin"`
}

// Origin records the proxy-declared VCS source. Fields are
// optional — pre-1.20 publishes lack the block entirely, and we
// model it best-effort.
type Origin struct {
	VCS  string `json:"VCS"`
	URL  string `json:"URL"`
	Ref  string `json:"Ref"`
	Hash string `json:"Hash"`
}

// TransparencyRecord is the parsed sum.golang.org /lookup
// response. LeafID is the tree's append-only sequence number
// (line 1 of the response). RawRecord retains the full body so
// callers that want to dig into the hash payload can; the
// collector itself only emits presence + leaf-id.
type TransparencyRecord struct {
	LeafID    int64
	RawRecord string
}

// GetLatest fetches the @latest pointer from the proxy. Returns
// ErrNotFound (wrapped) on 404 or 410.
func (c *Client) GetLatest(ctx context.Context, modulePath string) (*LatestInfo, error) {
	if err := ValidateModulePath(modulePath); err != nil {
		return nil, fmt.Errorf("get latest: %w", err)
	}
	path := "/" + encodeModulePath(modulePath) + "/@latest"
	var li LatestInfo
	err := c.proxyAPI.GetJSON(ctx, path, &li,
		httpx.WithHeader("Accept", "application/json"))
	if err != nil {
		return nil, c.wrapProxyErr(err, modulePath)
	}
	return &li, nil
}

// GetVersionList fetches the @v/list path from the proxy. The
// response is newline-delimited version strings; we split, filter
// blanks, and return the slice. Order is preserved — the proxy
// emits versions in publish order, and downstream code that wants
// a different ordering applies its own sort.
func (c *Client) GetVersionList(ctx context.Context, modulePath string) ([]string, error) {
	if err := ValidateModulePath(modulePath); err != nil {
		return nil, fmt.Errorf("get version list: %w", err)
	}
	path := "/" + encodeModulePath(modulePath) + "/@v/list"
	body, err := c.proxyAPI.Get(ctx, path,
		httpx.WithHeader("Accept", "text/plain"))
	if err != nil {
		return nil, c.wrapProxyErr(err, modulePath)
	}
	var versions []string
	scanner := bufio.NewScanner(bytes.NewReader(body))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		versions = append(versions, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan @v/list for %q: %w", modulePath, err)
	}
	return versions, nil
}

// ZipURL returns the canonical proxy.golang.org module-zip URL for
// the given module path and version: <proxy>/<encoded-path>/@v/<encoded-version>.zip.
//
// Used by the artifact_url handoff signal so the downstream
// artifact-vs-repo collector can fetch the module zip and pair it
// against the source repo. Pure URL construction — does not perform
// any HTTP. Both module-path components apply proxy.golang.org's
// `!`-escape rule (uppercase letters → !lowercase) so callers pass
// plain module paths.
func (c *Client) ZipURL(modulePath, version string) string {
	encodedPath := encodeModulePath(modulePath)
	encodedVer := encodeModulePath(version)
	return c.proxyURL + "/" + encodedPath + "/@v/" + encodedVer + ".zip"
}

// GetVersionInfo fetches @v/<version>.info from the proxy. Both
// the module path and version land in the URL, so both are
// validated before substitution.
func (c *Client) GetVersionInfo(ctx context.Context, modulePath, version string) (*VersionInfo, error) {
	if err := ValidateModulePath(modulePath); err != nil {
		return nil, fmt.Errorf("get version info: %w", err)
	}
	if err := validateVersion(version); err != nil {
		return nil, fmt.Errorf("get version info: %w", err)
	}
	path := "/" + encodeModulePath(modulePath) + "/@v/" + encodeModulePath(version) + ".info"
	var vi VersionInfo
	err := c.proxyAPI.GetJSON(ctx, path, &vi,
		httpx.WithHeader("Accept", "application/json"))
	if err != nil {
		return nil, c.wrapProxyErr(err, modulePath)
	}
	return &vi, nil
}

// LookupTransparency queries sum.golang.org/lookup for a
// (module, version) pair. The body shape is the documented
// transparency-log lookup response: line 1 is the leaf ID, lines
// 2+ are the module hash records, ending in a tree-signed line.
// We parse leaf-id + retain the raw body; downstream collector
// emits presence and leaf-id but not the raw text.
func (c *Client) LookupTransparency(ctx context.Context, modulePath, version string) (*TransparencyRecord, error) {
	if err := ValidateModulePath(modulePath); err != nil {
		return nil, fmt.Errorf("lookup transparency: %w", err)
	}
	if err := validateVersion(version); err != nil {
		return nil, fmt.Errorf("lookup transparency: %w", err)
	}
	// sum.golang.org's /lookup path uses the bang-encoded module
	// path the same way the proxy does. The version follows the
	// `@` separator without bang-encoding because version strings
	// are already lowercased (`v0.20.0`).
	path := "/lookup/" + encodeModulePath(modulePath) + "@" + version
	body, err := c.sumAPI.Get(ctx, path,
		httpx.WithHeader("Accept", "text/plain"))
	if err != nil {
		return nil, c.wrapProxyErr(err, modulePath)
	}
	rec := &TransparencyRecord{RawRecord: string(body)}
	// Parse the leaf id from line 1. Empty/non-numeric line 1 is
	// not fatal — record presence is the load-bearing signal, and
	// we model LeafID=0 as "couldn't parse" rather than treating
	// the whole response as malformed.
	if idx := bytes.IndexByte(body, '\n'); idx > 0 {
		first := strings.TrimSpace(string(body[:idx]))
		if n, perr := strconv.ParseInt(first, 10, 64); perr == nil && n >= 0 {
			rec.LeafID = n
		}
	}
	return rec, nil
}

// wrapProxyErr maps httpx errors to the per-package shape callers
// expect: ErrNotFound for absent-upstream paths (404 / 410), and a
// contextual wrap for other failures. Centralized so adding a new
// method doesn't drift from this convention.
func (c *Client) wrapProxyErr(err error, modulePath string) error {
	if errors.Is(err, httpx.ErrNotFound) {
		return fmt.Errorf("%w: %s", ErrNotFound, modulePath)
	}
	return fmt.Errorf("request for %q: %w", modulePath, err)
}

// validateVersion guards version strings before they land in a
// URL. Versions are simpler than module paths — `v` followed by
// dotted-numeric components, optional `-pre.<n>` or `+meta`
// suffixes — but we only need to assert "no path/query/fragment
// metacharacters" for URL safety; the proxy validates the full
// semver grammar and returns 404 on a malformed version.
func validateVersion(v string) error {
	if v == "" {
		return fmt.Errorf("version is empty")
	}
	if len(v) > 128 {
		return fmt.Errorf("version exceeds 128-byte cap (got %d)", len(v))
	}
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '.', r == '-', r == '_', r == '+', r == '~', r == 'v':
			// allowed
		default:
			return fmt.Errorf("version %q contains disallowed character %q", v, r)
		}
	}
	return nil
}
