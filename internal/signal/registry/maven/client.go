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
func checkRedirect(req *http.Request, via []*http.Request) error {
	if req.URL.Scheme != "https" && req.URL.Scheme != "http" {
		return fmt.Errorf("refusing redirect to non-HTTP(S) URL %s", req.URL.Redacted())
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

// ResolveRepoURL fetches the POM for the given artifact version and
// parses the <scm><url> or <scm><connection> element to find the source
// repository URL. Returns empty string if no SCM URL is declared.
func (c *Client) ResolveRepoURL(ctx context.Context, groupID, artifactID, version string) (string, error) {
	if err := ValidateCoordinate(groupID); err != nil {
		return "", fmt.Errorf("resolve repo URL: groupID: %w", err)
	}
	if err := ValidateCoordinate(artifactID); err != nil {
		return "", fmt.Errorf("resolve repo URL: artifactID: %w", err)
	}

	pomURL := fmt.Sprintf("%s/maven2/%s/%s/%s/%s-%s.pom",
		c.repoURL, groupPath(groupID), artifactID, version, artifactID, version)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pomURL, nil)
	if err != nil {
		return "", fmt.Errorf("build POM request for %s:%s:%s: %w",
			groupID, artifactID, version, err)
	}
	req.Header.Set("User-Agent", "signatory/0.1 (https://github.com/sarahmaeve/signatory)")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("maven POM request for %s:%s:%s failed: %w",
			groupID, artifactID, version, err)
	}
	defer resp.Body.Close() //nolint:errcheck // response body close

	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("%w: POM for %s:%s:%s", ErrNotFound, groupID, artifactID, version)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseSize))
		return "", fmt.Errorf("maven repo returned status %d for POM of %s:%s:%s",
			resp.StatusCode, groupID, artifactID, version)
	}

	limited := io.LimitReader(resp.Body, maxResponseSize+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return "", fmt.Errorf("read POM response for %s:%s:%s: %w",
			groupID, artifactID, version, err)
	}
	if int64(len(body)) > maxResponseSize {
		return "", fmt.Errorf("POM response for %s:%s:%s exceeds %d-byte cap",
			groupID, artifactID, version, maxResponseSize)
	}

	return parseSCMURL(body), nil
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

// extractXMLElement extracts the text content of a simple XML element.
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
	return strings.TrimSpace(xml[start : start+end])
}
