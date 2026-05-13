// Package maven provides an HTTP client and signal collector for Maven
// Central (repo1.maven.org). All operations — metadata, POM fetch,
// signature checks, timestamp resolution — go through repo1's static
// file layout. No Solr/search dependency.
package maven

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// ErrNotFound is returned (wrapped via %w) when Maven Central responds
// 404 for a lookup. Callers compare with errors.Is.
var ErrNotFound = errors.New("maven: not found")

// maxResponseSize bounds registry response bodies. 10 MB matches the
// npm, PyPI, and cargo caps for consistency.
const maxResponseSize = 10 * 1024 * 1024

// coordinatePattern matches Maven coordinate segments (groupId,
// artifactId). Starts with a letter or digit, then letters, digits,
// dots, hyphens, or underscores.
var coordinatePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

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

// Client is the Maven Central HTTP client. All requests go to
// repo1.maven.org — version discovery via maven-metadata.xml,
// POM retrieval, signature HEAD checks, and timestamp resolution.
type Client struct {
	httpClient *http.Client
	repoURL    string
}

// NewClient returns a Client bound to the public Maven Central repo1
// endpoint.
func NewClient() *Client {
	return &Client{
		httpClient: &http.Client{
			Timeout:       60 * time.Second,
			CheckRedirect: checkRedirect,
		},
		repoURL: "https://repo1.maven.org",
	}
}

// NewClientWithBaseURL returns a Client pointing at the supplied base.
// Primary use: test harnesses with httptest servers.
func NewClientWithBaseURL(repoBase string) *Client {
	return &Client{
		httpClient: &http.Client{
			Timeout:       60 * time.Second,
			CheckRedirect: checkRedirect,
		},
		repoURL: repoBase,
	}
}

// checkRedirect enforces HTTPS-only redirects and bounds the chain.
// repo1.maven.org is HTTPS-only; an HTTP redirect target is either a
// misconfiguration or a MITM attempting a scheme downgrade to tamper
// with POM / SCM URL / developer metadata that feeds trust signals.
// Symmetric with the npm, PyPI, cargo, gem, and gopublish clients.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if req.URL.Scheme != "https" {
		return fmt.Errorf("refusing redirect to non-HTTPS URL %s", req.URL.Redacted())
	}
	if len(via) >= 10 {
		return fmt.Errorf("too many redirects")
	}
	return nil
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

	metaURL := fmt.Sprintf("%s/maven2/%s/%s/maven-metadata.xml",
		c.repoURL, groupPath(groupID), artifactID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metaURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build metadata request for %s:%s: %w", groupID, artifactID, err)
	}
	req.Header.Set("User-Agent", "signatory/0.1 (https://github.com/sarahmaeve/signatory)")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("maven metadata request for %s:%s failed: %w", groupID, artifactID, err)
	}
	defer resp.Body.Close() //nolint:errcheck // response body close

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: %s:%s", ErrNotFound, groupID, artifactID)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseSize))
		return nil, fmt.Errorf("maven metadata returned status %d for %s:%s",
			resp.StatusCode, groupID, artifactID)
	}

	limited := io.LimitReader(resp.Body, maxResponseSize+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read maven metadata for %s:%s: %w", groupID, artifactID, err)
	}
	if int64(len(body)) > maxResponseSize {
		return nil, fmt.Errorf("maven metadata for %s:%s exceeds %d-byte cap",
			groupID, artifactID, maxResponseSize)
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

	jarURL := fmt.Sprintf("%s/maven2/%s/%s/%s/%s-%s.jar",
		c.repoURL, groupPath(groupID), artifactID, version, artifactID, version)

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, jarURL, nil)
	if err != nil {
		return time.Time{}, fmt.Errorf("build HEAD request for %s:%s:%s: %w",
			groupID, artifactID, version, err)
	}
	req.Header.Set("User-Agent", "signatory/0.1 (https://github.com/sarahmaeve/signatory)")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return time.Time{}, fmt.Errorf("maven HEAD request for %s:%s:%s failed: %w",
			groupID, artifactID, version, err)
	}
	defer resp.Body.Close() //nolint:errcheck // response body close

	if resp.StatusCode == http.StatusNotFound {
		return time.Time{}, fmt.Errorf("%w: jar for %s:%s:%s", ErrNotFound, groupID, artifactID, version)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return time.Time{}, fmt.Errorf("maven repo returned status %d for HEAD on %s:%s:%s",
			resp.StatusCode, groupID, artifactID, version)
	}

	lm := resp.Header.Get("Last-Modified")
	if lm == "" {
		return time.Time{}, nil
	}
	t, err := http.ParseTime(lm)
	if err != nil {
		return time.Time{}, nil // unparseable — degrade gracefully
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

	ascURL := fmt.Sprintf("%s/maven2/%s/%s/%s/%s-%s.jar.asc",
		c.repoURL, groupPath(groupID), artifactID, version, artifactID, version)

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, ascURL, nil)
	if err != nil {
		return false, fmt.Errorf("build signature request for %s:%s:%s: %w",
			groupID, artifactID, version, err)
	}
	req.Header.Set("User-Agent", "signatory/0.1 (https://github.com/sarahmaeve/signatory)")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("maven signature request for %s:%s:%s failed: %w",
			groupID, artifactID, version, err)
	}
	defer resp.Body.Close() //nolint:errcheck // response body close

	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("maven repo returned status %d for signature check on %s:%s:%s",
			resp.StatusCode, groupID, artifactID, version)
	}
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
func (c *Client) fetchPOM(ctx context.Context, groupID, artifactID, version string) ([]byte, error) {
	pomURL := fmt.Sprintf("%s/maven2/%s/%s/%s/%s-%s.pom",
		c.repoURL, groupPath(groupID), artifactID, version, artifactID, version)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pomURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build POM request for %s:%s:%s: %w",
			groupID, artifactID, version, err)
	}
	req.Header.Set("User-Agent", "signatory/0.1 (https://github.com/sarahmaeve/signatory)")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("maven POM request for %s:%s:%s failed: %w",
			groupID, artifactID, version, err)
	}
	defer resp.Body.Close() //nolint:errcheck // response body close

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: POM for %s:%s:%s", ErrNotFound, groupID, artifactID, version)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseSize))
		return nil, fmt.Errorf("maven repo returned status %d for POM of %s:%s:%s",
			resp.StatusCode, groupID, artifactID, version)
	}

	limited := io.LimitReader(resp.Body, maxResponseSize+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read POM response for %s:%s:%s: %w",
			groupID, artifactID, version, err)
	}
	if int64(len(body)) > maxResponseSize {
		return nil, fmt.Errorf("POM response for %s:%s:%s exceeds %d-byte cap",
			groupID, artifactID, version, maxResponseSize)
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
	close := "</" + tag + ">"
	start := strings.Index(xml, open)
	if start < 0 {
		return ""
	}
	start += len(open)
	end := strings.Index(xml[start:], close)
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
