// Package gitlab collects trust signals from the GitLab API. v0.1
// targets gitlab.com only; self-hosted GitLab instances need an
// explicit allow-list mechanism (same threat-model deferral as
// self-hosted Forgejo) and ship in a follow-up.
//
// Tier 1 scope (this commit): the metadata signals derivable from a
// single GET against /api/v4/projects/{namespace_url_encoded} —
//
//   - stars        (star_count → count)
//   - forks        (forks_count → count)
//   - open_issues  (open_issues_count → count)
//   - archived     (archived → archived)
//   - repo_age     (created_at → created, age_days)
//   - last_push    (last_activity_at → date, era; GitLab doesn't
//     expose a separate pushed_at and last_activity_at advances on
//     push and on issue/MR activity)
//
// Tier 2 (deferred): owner_type, owner_profile, contributors,
// license. owner_type is technically free on the same call
// (namespace.kind discriminates user/group), but Tier 1 keeps the
// signal set symmetric with the forgejo collector — landing
// owner_type for both forges in one Tier 1.5 commit lets the
// per-forge cost asymmetry (forgejo needs a second call, gitlab
// doesn't) live in one place.
//
// Source name: signals carry source="gitlab". Same layering choice
// as github / forgejo — source names the API contract.
//
// Project ID encoding: GitLab addresses projects by URL-encoded
// namespace path (gitlab-org/gitlab → gitlab-org%2Fgitlab). Nested
// groups deepen the path (gitlab-org/security/foo →
// gitlab-org%2Fsecurity%2Ffoo) and EVERY slash must encode — partial
// encoding would route to a different gitlab path or 404. See
// projectIDPath for the encoding rule.
//
// The github / forgejo collectors are the reference implementations.
// Where this package's discipline matches theirs (timeouts,
// response-size limits, redirect policy), the rationale lives in
// internal/signal/github/client.go and is not repeated here.
package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrNotFound is the sentinel callers compare via errors.Is when the
// GitLab API responds 404. Mirrors github.ErrNotFound /
// forgejo.ErrNotFound.
var ErrNotFound = errors.New("gitlab: not found")

// maxResponseSize bounds the JSON body we'll read. Mirrors
// github/forgejo's 10 MiB cap; GitLab project responses are typically
// <100 KiB so this is generous slack against malicious or runaway
// upstreams.
const maxResponseSize = 10 * 1024 * 1024

// Client is a minimal GitLab REST client. baseURL is the API root
// (e.g. https://gitlab.com/api/v4) — caller-injectable for tests,
// fixed to the gitlab.com root in production via NewClient.
type Client struct {
	httpClient *http.Client
	baseURL    string
}

// NewClient creates a GitLab client pointed at gitlab.com's public
// API root with a 60s per-request timeout (matches github/forgejo
// clients). v0.1 is unauthenticated; gitlab.com's public-API rate
// limit (600 req/min unauth) is sufficient for a single-target
// analyze. Authenticated access via a GITLAB_TOKEN env var is a
// follow-up when survey-side usage emerges.
func NewClient() *Client {
	return &Client{
		httpClient: &http.Client{
			Timeout:       60 * time.Second,
			CheckRedirect: checkRedirect,
		},
		baseURL: "https://gitlab.com/api/v4",
	}
}

// checkRedirect mirrors the github / forgejo redirect policy: refuse
// non-HTTPS targets, bound the chain to <10 hops.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if req.URL.Scheme != "https" {
		return fmt.Errorf("refusing redirect to non-HTTPS URL %s", req.URL.Redacted())
	}
	if len(via) >= 10 {
		return fmt.Errorf("too many redirects")
	}
	return nil
}

// project represents the subset of /api/v4/projects/{id} response
// fields this collector consumes. The full GitLab response carries
// 100+ fields; only the Tier 1 set is decoded.
//
// Field-name notes vs. github AND forgejo (three forges, three
// names for the same concept):
//
//   - StarCount maps to JSON "star_count" (forgejo: stars_count;
//     github: stargazers_count)
//   - LastActivityAt maps to "last_activity_at" — closest analog to
//     github's PushedAt and forgejo's UpdatedAt. Advances on push
//     AND on issue/MR activity, so it's slightly noisier than
//     github's pushed_at, but for last_push's "is this project
//     active" purpose the noise is acceptable.
//   - PathWithNS ("path_with_namespace") is gitlab's full project
//     path (e.g. gitlab-org/gitlab). Tier 1 doesn't emit a signal
//     from it but the field exists for cross-checking the requested
//     namespace landed on the right project.
//   - Namespace.Kind discriminates "user" vs "group" (gitlab's
//     equivalent of Organization). Available on the same response
//     unlike Forgejo, but Tier 1 defers owner_type to keep the
//     per-forge collector signal sets symmetric.
type project struct {
	ID              int              `json:"id"`
	Name            string           `json:"name"`
	PathWithNS      string           `json:"path_with_namespace"`
	Description     string           `json:"description"`
	Namespace       projectNamespace `json:"namespace"`
	CreatedAt       time.Time        `json:"created_at"`
	LastActivityAt  time.Time        `json:"last_activity_at"`
	StarCount       int              `json:"star_count"`
	ForksCount      int              `json:"forks_count"`
	OpenIssuesCount int              `json:"open_issues_count"`
	Archived        bool             `json:"archived"`
}

// projectNamespace is the embedded namespace object on a project
// response. Path is the namespace path (no project name); Kind is
// "user" or "group". v0.1 reads neither; Tier 1.5 will use Kind
// for owner_type.
type projectNamespace struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
}

// projectIDPath returns the URL path segment for a GitLab project
// ID. Accepts namespace+name in "owner/name" or "owner/sub/name"
// shape and produces the URL-encoded form GitLab's API requires
// (every slash → %2F). Pass the result directly into
// "/projects/" + projectIDPath(...).
//
// url.PathEscape is the WRONG primitive here — it preserves '/' as
// a path separator. We need '/' encoded as %2F because GitLab
// treats the entire namespace path as a single project identifier.
// url.QueryEscape is closer (it does encode '/'), but it also
// translates ' ' to '+' which is not what we want for path data.
// Manual replacement after PathEscape is the simplest approach
// that's correct for both cases.
func projectIDPath(namespacePath string) string {
	// PathEscape handles every other special character correctly
	// (Unicode normalization, URL-reserved characters except '/').
	escaped := url.PathEscape(namespacePath)
	// Replace any literal '/' that survived PathEscape with %2F.
	return strings.ReplaceAll(escaped, "/", "%2F")
}

// get performs a GET against the GitLab API at path, decoding the
// response into result. Returns ErrNotFound (wrapped) on 404 so
// callers can use errors.Is. Other non-200 responses surface as a
// status-only error — body intentionally NOT included for the same
// reason github's client drops it (issue #93).
func (c *Client) get(ctx context.Context, path string, result any) error {
	endpoint := c.baseURL + path

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("execute request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // close-after-read; body consumed below, error here is not actionable

	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w: %s", ErrNotFound, path)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("gitlab API returned status %d", resp.StatusCode)
	}

	if result != nil {
		limited := io.LimitReader(resp.Body, maxResponseSize+1)
		body, readErr := io.ReadAll(limited)
		if readErr != nil {
			return fmt.Errorf("read response: %w", readErr)
		}
		if int64(len(body)) > maxResponseSize {
			return fmt.Errorf("response too large: %d bytes exceeds %d byte limit", len(body), maxResponseSize)
		}
		if jsonErr := json.Unmarshal(body, result); jsonErr != nil {
			return fmt.Errorf("decode response: %w", jsonErr)
		}
	}
	return nil
}

// GetProject fetches /api/v4/projects/{namespace_url_encoded}.
// namespacePath is the full path (e.g. "gitlab-org/gitlab" or
// "gitlab-org/security/foo"); URL encoding happens inside via
// projectIDPath. Returns ErrNotFound (wrapped) when the project
// doesn't exist or is private to the unauthenticated client.
func (c *Client) GetProject(ctx context.Context, namespacePath string) (*project, error) {
	var p project
	if err := c.get(ctx, "/projects/"+projectIDPath(namespacePath), &p); err != nil {
		return nil, err
	}
	return &p, nil
}
