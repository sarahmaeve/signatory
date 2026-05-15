// Package maven provides an HTTP client and signal collector for Maven
// Central (repo1.maven.org). All operations — metadata, POM fetch,
// signature checks, timestamp resolution — go through repo1's static
// file layout. No Solr/search dependency.
//
// The defensive network discipline (HTTPS-only redirects, response-
// body cap, drain-and-discard on non-2xx, sanitized status errors)
// lives in httpx.SecureClient. This package owns input validation,
// XML decoding, the HEAD-based signature-presence and last-modified
// probes, and the parent-POM chase for SCM URL resolution.
package maven

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sarahmaeve/signatory/internal/httpx"
)

// ErrNotFound is returned (wrapped via %w) when Maven Central responds
// 404 for a lookup. Wraps httpx.ErrNotFound so callers can also do
// errors.Is(err, httpx.ErrNotFound) for ecosystem-agnostic absence
// detection.
var ErrNotFound = fmt.Errorf("maven: %w", httpx.ErrNotFound)

// coordinatePattern matches Maven coordinate segments (groupId,
// artifactId). Starts with a letter or digit, then letters, digits,
// dots, hyphens, or underscores.
var coordinatePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// mavenUserAgent is the User-Agent the maven client sends. Maven
// Central serves anonymous traffic but the contact URL helps the
// operators route abuse reports — same rationale as the cargo client.
const mavenUserAgent = "signatory/0.1 (https://github.com/sarahmaeve/signatory)"

// ValidateCoordinate enforces the Maven coordinate grammar for a single
// segment (groupId or artifactId) before URL construction.
func ValidateCoordinate(segment string) error {
	if segment == "" {
		return fmt.Errorf("maven coordinate segment is empty")
	}
	if !coordinatePattern.MatchString(segment) {
		return fmt.Errorf("maven coordinate segment %q does not match allowed grammar "+
			"(allowed: a-z A-Z 0-9 . _ -, must start with letter or digit)", segment)
	}
	return nil
}

// maxVersionLength is a defensive cap on version strings before
// validation runs. Real Maven versions are short (typically under
// 30 chars even with qualifiers like "-SNAPSHOT" or ".RELEASE");
// capping at 128 keeps validation cheap on adversarial inputs
// without rejecting any legitimate version.
const maxVersionLength = 128

// ValidateVersion guards version strings before they land in a URL.
// Maven versions in real artifacts use a narrow ASCII grammar
// (digits, dots, hyphens, plus, underscores, tildes, and letters
// for qualifiers like "RC1", "-jre", ".RELEASE"). Reject anything
// outside that grammar — particularly `/`, `?`, `#`, and `..` which
// would re-parse the request path or smuggle traversal segments.
//
// Mirrors gopublish.validateVersion. Maven Central rejects
// non-conforming versions server-side, but #90 discipline says
// validate at the function boundary before substitution — the
// fetchPOM path inside ResolveRepoURL takes versions from
// parseParent(body) (POM XML), and an adversarial POM upload could
// embed traversal-shaped strings that extractXMLElement doesn't
// filter (it only rejects control chars + non-UTF-8).
func ValidateVersion(v string) error {
	if v == "" {
		return fmt.Errorf("version is empty")
	}
	if len(v) > maxVersionLength {
		return fmt.Errorf("version exceeds %d-byte cap (got %d)", maxVersionLength, len(v))
	}
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '.', r == '-', r == '_', r == '+', r == '~':
			// allowed
		default:
			return fmt.Errorf("version %q contains disallowed character %q", v, r)
		}
	}
	return nil
}

// Client is the Maven Central HTTP client. All requests go to
// repo1.maven.org — version discovery via maven-metadata.xml,
// POM retrieval, signature HEAD checks, and timestamp resolution.
type Client struct {
	api *httpx.SecureClient
}

// NewClient returns a Client bound to the public Maven Central repo1
// endpoint.
func NewClient() *Client {
	return &Client{
		api: httpx.NewSecureClient(
			httpx.WithBaseURL("https://repo1.maven.org"),
			httpx.WithUserAgent(mavenUserAgent),
		),
	}
}

// NewClientWithBaseURL returns a Client pointing at the supplied base.
// Primary use: test harnesses with httptest servers.
func NewClientWithBaseURL(repoBase string) *Client {
	return &Client{
		api: httpx.NewSecureClient(
			httpx.WithBaseURL(repoBase),
			httpx.WithUserAgent(mavenUserAgent),
		),
	}
}

// groupPath converts a dotted groupId to the slash-separated path
// Maven Central uses: "com.google.guava" → "com/google/guava".
func groupPath(groupID string) string {
	return strings.ReplaceAll(groupID, ".", "/")
}

// FetchMetadata retrieves and parses the maven-metadata.xml for the
// given groupID:artifactID coordinate. The metadata contains the
// version list, latest release, and last-updated timestamp.
func (c *Client) FetchMetadata(ctx context.Context, groupID, artifactID string) (*Metadata, error) {
	if err := ValidateCoordinate(groupID); err != nil {
		return nil, fmt.Errorf("fetch metadata: groupID: %w", err)
	}
	if err := ValidateCoordinate(artifactID); err != nil {
		return nil, fmt.Errorf("fetch metadata: artifactID: %w", err)
	}

	path := fmt.Sprintf("/maven2/%s/%s/maven-metadata.xml", groupPath(groupID), artifactID)
	body, err := c.api.Get(ctx, path)
	if err != nil {
		if errors.Is(err, httpx.ErrNotFound) {
			return nil, fmt.Errorf("%w: %s:%s", ErrNotFound, groupID, artifactID)
		}
		return nil, fmt.Errorf("maven metadata request for %s:%s: %w", groupID, artifactID, err)
	}

	var meta Metadata
	if err := xml.Unmarshal(body, &meta); err != nil {
		return nil, fmt.Errorf("decode maven metadata for %s:%s: %w", groupID, artifactID, err)
	}
	return &meta, nil
}

// HeadTimestamp issues a HEAD request for the jar of the given artifact
// version and returns the Last-Modified timestamp. Returns zero time if
// the header is missing or unparseable.
func (c *Client) HeadTimestamp(ctx context.Context, groupID, artifactID, version string) (time.Time, error) {
	if err := ValidateCoordinate(groupID); err != nil {
		return time.Time{}, fmt.Errorf("head timestamp: groupID: %w", err)
	}
	if err := ValidateCoordinate(artifactID); err != nil {
		return time.Time{}, fmt.Errorf("head timestamp: artifactID: %w", err)
	}
	if err := ValidateVersion(version); err != nil {
		return time.Time{}, fmt.Errorf("head timestamp: version: %w", err)
	}

	path := fmt.Sprintf("/maven2/%s/%s/%s/%s-%s.jar",
		groupPath(groupID), artifactID, version, artifactID, version)
	headers, _, err := c.api.Head(ctx, path)
	if err != nil {
		if errors.Is(err, httpx.ErrNotFound) {
			return time.Time{}, fmt.Errorf("%w: jar for %s:%s:%s",
				ErrNotFound, groupID, artifactID, version)
		}
		return time.Time{}, fmt.Errorf("maven HEAD request for %s:%s:%s: %w",
			groupID, artifactID, version, err)
	}

	lm := headers.Get("Last-Modified")
	if lm == "" {
		return time.Time{}, nil
	}
	t, perr := http.ParseTime(lm)
	if perr != nil {
		// Intentional graceful degradation: an unparseable
		// Last-Modified yields the zero time with a nil error so the
		// collector falls back ("no timestamp for this version")
		// instead of one malformed header failing the whole
		// timestamp-resolution loop. Contract pinned by
		// TestHeadTimestamp_DegradesOnBadLastModified.
		return time.Time{}, nil //nolint:nilerr // perr is deliberately swallowed; see comment + test
	}
	return t.UTC(), nil
}

// CheckSignature issues a HEAD request for the .asc signature file of
// the given artifact version on repo1.maven.org. Returns true if 200,
// false if 404, and an error for other statuses or network failures.
func (c *Client) CheckSignature(ctx context.Context, groupID, artifactID, version string) (bool, error) {
	if err := ValidateCoordinate(groupID); err != nil {
		return false, fmt.Errorf("check signature: groupID: %w", err)
	}
	if err := ValidateCoordinate(artifactID); err != nil {
		return false, fmt.Errorf("check signature: artifactID: %w", err)
	}
	if err := ValidateVersion(version); err != nil {
		return false, fmt.Errorf("check signature: version: %w", err)
	}

	path := fmt.Sprintf("/maven2/%s/%s/%s/%s-%s.jar.asc",
		groupPath(groupID), artifactID, version, artifactID, version)
	_, _, err := c.api.Head(ctx, path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, httpx.ErrNotFound) {
		return false, nil
	}
	return false, fmt.Errorf("maven HEAD request for signature of %s:%s:%s: %w",
		groupID, artifactID, version, err)
}

// maxParentDepth limits parent POM chasing to prevent infinite loops
// from circular parent references. 5 levels is generous — real-world
// Maven parent chains rarely exceed 3 (artifact → parent → grandparent).
const maxParentDepth = 5

// ResolveRepoURL fetches the POM for the given artifact version and
// parses the <scm><url> or <scm><connection> element to find the source
// repository URL. If the artifact's own POM has no <scm> section but
// declares a <parent>, the parent POM is fetched and scanned — up to
// maxParentDepth levels. Returns empty string if no SCM URL is found
// in the chain.
func (c *Client) ResolveRepoURL(ctx context.Context, groupID, artifactID, version string) (string, error) {
	if err := ValidateCoordinate(groupID); err != nil {
		return "", fmt.Errorf("resolve repo URL: groupID: %w", err)
	}
	if err := ValidateCoordinate(artifactID); err != nil {
		return "", fmt.Errorf("resolve repo URL: artifactID: %w", err)
	}

	g, a, v := groupID, artifactID, version
	for range maxParentDepth {
		body, err := c.fetchPOM(ctx, g, a, v)
		if err != nil {
			return "", err
		}

		// parseSCMURL returns the raw <url> verbatim and the
		// scm:git:-stripped <connection>. NormalizeDeclaredRepoURL
		// reduces both to signatory's canonical https://<forge>/<owner>/<name>
		// form (handling the SCP-shorthand case Go's url.Parse can't accept,
		// among others) and returns "" for non-first-classed forges.
		// Empty after normalization is treated like "no <scm> in this POM"
		// — the parent-chase below proceeds, preferring a usable parent
		// SCM to stamping a non-forge URL the downstream collectors can't
		// clone from.
		if scm := NormalizeDeclaredRepoURL(parseSCMURL(body)); scm != "" {
			return scm, nil
		}

		// No SCM — check for a <parent> to chase.
		pg, pa, pv := parseParent(body)
		if pg == "" || pa == "" || pv == "" {
			return "", nil // no parent, no SCM anywhere in the chain
		}
		g, a, v = pg, pa, pv
	}

	return "", nil // hit depth limit
}

// fetchPOM retrieves a single POM file from repo1 and returns the raw body.
//
// Validates all three coordinate inputs even though the public entry
// points already do — this is the load-bearing guard inside the
// ResolveRepoURL parent-chase loop, where the loop reassigns
// (g, a, v) from parseParent(body) on each iteration. Those values
// come from POM XML, and extractXMLElement only filters control
// chars + non-UTF-8; it does NOT filter `/`, `?`, `#`, or `..`.
// Validating at the URL boundary closes that hole.
func (c *Client) fetchPOM(ctx context.Context, groupID, artifactID, version string) ([]byte, error) {
	if err := ValidateCoordinate(groupID); err != nil {
		return nil, fmt.Errorf("fetch POM: groupID: %w", err)
	}
	if err := ValidateCoordinate(artifactID); err != nil {
		return nil, fmt.Errorf("fetch POM: artifactID: %w", err)
	}
	if err := ValidateVersion(version); err != nil {
		return nil, fmt.Errorf("fetch POM: version: %w", err)
	}
	path := fmt.Sprintf("/maven2/%s/%s/%s/%s-%s.pom",
		groupPath(groupID), artifactID, version, artifactID, version)

	body, err := c.api.Get(ctx, path)
	if err != nil {
		if errors.Is(err, httpx.ErrNotFound) {
			return nil, fmt.Errorf("%w: POM for %s:%s:%s",
				ErrNotFound, groupID, artifactID, version)
		}
		return nil, fmt.Errorf("maven POM request for %s:%s:%s: %w",
			groupID, artifactID, version, err)
	}
	return body, nil
}

// parseParent extracts the <parent> groupId, artifactId, and version
// from a POM. Returns ("", "", "") if no parent is declared.
func parseParent(pomBytes []byte) (groupID, artifactID, version string) {
	pom := string(pomBytes)

	start := strings.Index(pom, "<parent>")
	if start < 0 {
		return "", "", ""
	}
	end := strings.Index(pom[start:], "</parent>")
	if end < 0 {
		return "", "", ""
	}
	parent := pom[start : start+end]

	g := extractXMLElement(parent, "groupId")
	a := extractXMLElement(parent, "artifactId")
	v := extractXMLElement(parent, "version")
	return g, a, v
}

// parseSCMURL does a simple scan of POM XML for <scm><url> or
// <scm><connection>. This is intentionally not a full XML parser —
// Maven POMs are deeply nested and we only need one field.
func parseSCMURL(pomBytes []byte) string {
	pom := string(pomBytes)

	// Look for <scm> section.
	scmStart := strings.Index(pom, "<scm>")
	if scmStart < 0 {
		return ""
	}
	scmEnd := strings.Index(pom[scmStart:], "</scm>")
	if scmEnd < 0 {
		return ""
	}
	scm := pom[scmStart : scmStart+scmEnd]

	// Prefer <url> over <connection>.
	if u := extractXMLElement(scm, "url"); u != "" {
		return u
	}
	if conn := extractXMLElement(scm, "connection"); conn != "" {
		// Strip scm: prefix if present (e.g., "scm:git:https://...")
		conn = strings.TrimPrefix(conn, "scm:git:")
		conn = strings.TrimPrefix(conn, "scm:svn:")
		return conn
	}
	return ""
}

// parseDevelopers extracts developer names from a POM's <developers>
// section. Falls back to <id> when <name> is absent. Returns nil if no
// developers are declared.
func parseDevelopers(pomBytes []byte) []string {
	pom := string(pomBytes)

	devStart := strings.Index(pom, "<developers>")
	if devStart < 0 {
		return nil
	}
	devEnd := strings.Index(pom[devStart:], "</developers>")
	if devEnd < 0 {
		return nil
	}
	devSection := pom[devStart : devStart+devEnd]

	var names []string
	remaining := devSection
	for {
		start := strings.Index(remaining, "<developer>")
		if start < 0 {
			break
		}
		end := strings.Index(remaining[start:], "</developer>")
		if end < 0 {
			break
		}
		dev := remaining[start : start+end]
		remaining = remaining[start+end:]

		name := extractXMLElement(dev, "name")
		if name == "" {
			name = extractXMLElement(dev, "id")
		}
		if name != "" {
			names = append(names, name)
		}
	}

	if len(names) == 0 {
		return nil
	}
	return names
}

// parseDependencies extracts the project's directly-declared
// dependencies from a POM as Maven groupId:artifactId coordinates.
// Returns nil if the POM is unparseable or declares none.
//
// Uses encoding/xml (consistent with FetchMetadata) rather than the
// string-scan approach of parseParent/parseSCMURL: dependency
// extraction must distinguish <project><dependencies> from
// <dependencyManagement><dependencies> and plugin-level
// <dependencies>, which path-aware unmarshalling does structurally
// and a substring scan cannot do reliably.
//
// Scope filtering mirrors the dev-exclusion rule used for npm
// (devDependencies) and cargo (kind=dev): a "test"-scoped dependency
// is not consumed transitively by downstream and is dropped. Absent
// scope means Maven's default "compile" and is kept. Optional deps
// are kept — optional still declares the surface. The returned slice
// is unsorted; the collector sorts and de-duplicates, mirroring
// parseDevelopers' raw-return contract.
func parseDependencies(pomBytes []byte) []string {
	var p pomForDeps
	if err := xml.Unmarshal(pomBytes, &p); err != nil {
		return nil
	}
	var out []string
	for _, d := range p.Dependencies.Dependency {
		if strings.EqualFold(strings.TrimSpace(d.Scope), "test") {
			continue
		}
		g := strings.TrimSpace(d.GroupID)
		a := strings.TrimSpace(d.ArtifactID)
		if g == "" || a == "" {
			continue
		}
		out = append(out, g+":"+a)
	}
	return out
}

// FetchDependencies retrieves the POM for the given version and
// returns the project's directly-declared dependencies as
// groupId:artifactId coordinates. Mirrors FetchDevelopers: one POM
// GET, parse, return; ErrNotFound (wrapped) propagates on 404.
func (c *Client) FetchDependencies(ctx context.Context, groupID, artifactID, version string) ([]string, error) {
	body, err := c.fetchPOM(ctx, groupID, artifactID, version)
	if err != nil {
		return nil, err
	}
	return parseDependencies(body), nil
}

// FetchDevelopers retrieves the POM for the given version and returns
// the developer names from the <developers> section.
func (c *Client) FetchDevelopers(ctx context.Context, groupID, artifactID, version string) ([]string, error) {
	body, err := c.fetchPOM(ctx, groupID, artifactID, version)
	if err != nil {
		return nil, err
	}
	return parseDevelopers(body), nil
}

// extractXMLElement extracts the text content of a simple XML element.
// Returns "" if the element is absent, its content is not valid UTF-8,
// or it contains ASCII control characters (0x00–0x1F except TAB).
// A POM with control characters in a coordinate or name is either
// corrupted or adversarial — treat as absent rather than propagate.
func extractXMLElement(xml, tag string) string {
	open := "<" + tag + ">"
	closeTag := "</" + tag + ">"
	start := strings.Index(xml, open)
	if start < 0 {
		return ""
	}
	start += len(open)
	end := strings.Index(xml[start:], closeTag)
	if end < 0 {
		return ""
	}
	content := strings.TrimSpace(xml[start : start+end])
	if !utf8.ValidString(content) {
		return ""
	}
	if containsControlChars(content) {
		return ""
	}
	return content
}

// containsControlChars reports whether s includes any ASCII control
// character (0x00–0x1F) other than horizontal tab (0x09). These never
// appear in legitimate Maven coordinates, URLs, or developer names.
func containsControlChars(s string) bool {
	for _, r := range s {
		if r < 0x20 && r != '\t' {
			return true
		}
	}
	return false
}
