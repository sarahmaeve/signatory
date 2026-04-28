package gopublish

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ErrNotFound is returned (wrapped via %w) when an upstream
// endpoint responds 404 — for the proxy that means "module/version
// not in the proxy"; for sum.golang.org it means "no transparency
// log entry." Callers compare via errors.Is.
var ErrNotFound = errors.New("gopublish: not found")

// maxResponseSize bounds upstream response bodies to prevent OOM
// from unbounded streams. proxy responses are typically a few KB
// (.info JSON) up to a few hundred KB (the @v/list of a
// long-lived module); 10 MiB is generous slack symmetric with the
// npm client. sum.golang.org records are a few hundred bytes; the
// same cap is more than enough.
const maxResponseSize = 10 * 1024 * 1024

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

// Client is a narrow Go-publish data-plane HTTP client. Surface is
// kept to the four endpoints v0.1's collector reads:
// @latest, @v/list, @v/<v>.info on the proxy, plus /lookup on the
// sumdb. Extending the surface should follow the same TDD shape
// (test the response model, then add the method).
type Client struct {
	httpClient *http.Client
	proxyURL   string
	sumURL     string
}

// NewClient returns a Client bound to the public Go endpoints —
// proxy.golang.org for module metadata and sum.golang.org for
// transparency-log lookups. Timeout matches the npm + github
// clients' 60s; the Go data plane is generally faster than that
// but the upper bound keeps a slow upstream from collapsing a
// collection run into a blanket absence.
func NewClient() *Client {
	return &Client{
		httpClient: &http.Client{
			Timeout:       60 * time.Second,
			CheckRedirect: checkRedirect,
		},
		proxyURL: "https://proxy.golang.org",
		sumURL:   "https://sum.golang.org",
	}
}

// NewClientWithBaseURL returns a Client whose endpoints both point
// at the supplied bases. Tests typically pass the same httptest
// server URL twice; production wires the two distinct hosts.
func NewClientWithBaseURL(proxy, sum string) *Client {
	return &Client{
		httpClient: &http.Client{
			Timeout:       60 * time.Second,
			CheckRedirect: checkRedirect,
		},
		proxyURL: proxy,
		sumURL:   sum,
	}
}

// checkRedirect refuses any non-HTTPS redirect target and bounds
// the redirect chain. Symmetric with the npm + github clients —
// scheme downgrade has no legitimate use case on these public
// services, and a uniform policy lowers the review burden when a
// future collector ships against another endpoint.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if req.URL.Scheme != "https" {
		return fmt.Errorf("refusing redirect to non-HTTPS URL %s", req.URL.Redacted())
	}
	if len(via) >= 10 {
		return fmt.Errorf("too many redirects")
	}
	return nil
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
// ErrNotFound (wrapped) on 404.
func (c *Client) GetLatest(ctx context.Context, modulePath string) (*LatestInfo, error) {
	if err := ValidateModulePath(modulePath); err != nil {
		return nil, fmt.Errorf("get latest: %w", err)
	}
	encoded := encodeModulePath(modulePath)
	url := c.proxyURL + "/" + encoded + "/@latest"
	body, err := c.doGetJSON(ctx, url, modulePath)
	if err != nil {
		return nil, err
	}
	var li LatestInfo
	if err := json.Unmarshal(body, &li); err != nil {
		return nil, fmt.Errorf("decode @latest response for %q: %w", modulePath, err)
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
	encoded := encodeModulePath(modulePath)
	url := c.proxyURL + "/" + encoded + "/@v/list"
	body, err := c.doGetBytes(ctx, url, modulePath)
	if err != nil {
		return nil, err
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
	encodedPath := encodeModulePath(modulePath)
	encodedVer := encodeModulePath(version)
	url := c.proxyURL + "/" + encodedPath + "/@v/" + encodedVer + ".info"
	body, err := c.doGetJSON(ctx, url, modulePath)
	if err != nil {
		return nil, err
	}
	var vi VersionInfo
	if err := json.Unmarshal(body, &vi); err != nil {
		return nil, fmt.Errorf("decode @v/%s.info response for %q: %w", version, modulePath, err)
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
	encodedPath := encodeModulePath(modulePath)
	url := c.sumURL + "/lookup/" + encodedPath + "@" + version

	body, err := c.doGetBytes(ctx, url, modulePath)
	if err != nil {
		return nil, err
	}
	rec := &TransparencyRecord{RawRecord: string(body)}
	// Parse the leaf id from line 1. Empty/non-numeric line 1 is
	// not fatal — record presence is the load-bearing signal, and
	// we model LeafID=0 as "couldn't parse" rather than treating
	// the whole response as malformed.
	if idx := bytes.IndexByte(body, '\n'); idx > 0 {
		first := strings.TrimSpace(string(body[:idx]))
		if n, perr := strconv.ParseInt(first, 10, 64); perr == nil {
			rec.LeafID = n
		}
	}
	return rec, nil
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

// doGetJSON is the shared HTTP path for endpoints that return
// JSON: build request, dispatch, drain and bound the body, return
// raw bytes. Caller decodes.
func (c *Client) doGetJSON(ctx context.Context, url, modulePath string) ([]byte, error) {
	return c.doGet(ctx, url, modulePath, "application/json")
}

// doGetBytes is the shared HTTP path for endpoints that return
// non-JSON (newline-list, transparency-log record). Same shape;
// different Accept header to nudge content-negotiation toward the
// expected response.
func (c *Client) doGetBytes(ctx context.Context, url, modulePath string) ([]byte, error) {
	return c.doGet(ctx, url, modulePath, "text/plain")
}

func (c *Client) doGet(ctx context.Context, url, modulePath, accept string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request for %q: %w", modulePath, err)
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", "signatory/0.1")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request for %q failed: %w", modulePath, err)
	}
	defer resp.Body.Close() //nolint:errcheck // response body close; err is not actionable

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, modulePath)
	}
	if resp.StatusCode == http.StatusGone {
		// 410 from the proxy means "the version was retracted /
		// deleted." Treat the same as NotFound for collection
		// purposes — the collector records absence.
		return nil, fmt.Errorf("%w: %s (410 Gone)", ErrNotFound, modulePath)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Discard body without surfacing it (#93 npm-client lesson):
		// the response can carry server-debug noise.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseSize))
		return nil, fmt.Errorf("upstream returned status %d for %q", resp.StatusCode, modulePath)
	}

	limited := io.LimitReader(resp.Body, maxResponseSize+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read response for %q: %w", modulePath, err)
	}
	if int64(len(body)) > maxResponseSize {
		return nil, fmt.Errorf("response for %q exceeds %d-byte cap", modulePath, maxResponseSize)
	}
	return body, nil
}
