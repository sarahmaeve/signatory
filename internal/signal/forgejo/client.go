// Package forgejo collects trust signals from the Forgejo API. Forgejo
// is a soft-fork of Gitea; codeberg.org is its largest public deployment
// and the only host this collector targets in v0.1. Self-hosted Forgejo
// instances need an explicit allow-list (same threat-model deferral as
// self-hosted GitLab) and ship in a follow-up.
//
// Tier 1 (single GET against /api/v1/repos/{owner}/{repo}):
//
//   - stars        (stars_count → count)
//   - forks        (forks_count → count)
//   - open_issues  (open_issues_count → count)
//   - archived     (archived → archived)
//   - repo_age     (created_at → created, age_days)
//   - last_push    (updated_at → date, era; Forgejo doesn't expose a
//     separate pushed_at so updated_at is the closest
//     analog and advances on push)
//
// Tier 1.5 (one extra round-trip against /api/v1/orgs/{name}):
//
//   - owner_type   (200 → Organization, 404 → User; Forgejo's repo
//     response carries no User/Organization discriminator
//     on the owner so the probe is mandatory)
//
// Tier 2 (one further round-trip against /api/v1/users/{name}):
//
//   - owner_profile (login, name, created, account_age_days, followers,
//     type; same canonical shape as github's signal so
//     cross-forge posture rules read uniform fields)
//
// Still deferred: contributors, license. Each would need a further
// per-signal API call against /repos/{o}/{r}/contributors or a
// license-detection helper.
//
// Source name: signals carry source="forgejo" (the API kind), not
// "codeberg" (the deployment). Same layering choice as github — source
// names the API contract so future Forgejo deployments fold under the
// same emission discipline.
//
// The github collector is the reference implementation. Where this
// package's discipline matches github's (timeouts, response-size
// limits, redirect policy, error sanitization), the rationale lives
// in internal/signal/github/client.go and is not repeated here.
package forgejo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// ErrNotFound is the sentinel callers compare via errors.Is when the
// Forgejo API responds 404. Mirrors github.ErrNotFound's role.
var ErrNotFound = errors.New("forgejo: not found")

// maxResponseSize bounds the JSON body we'll read. Mirrors github's
// 10 MiB cap; Forgejo /repos responses are typically <50 KiB so this
// is generous slack against malicious or runaway upstreams.
const maxResponseSize = 10 * 1024 * 1024

// Client is a minimal Forgejo REST client. baseURL is the API root
// (e.g. https://codeberg.org/api/v1) — caller-injectable for tests,
// fixed to the codeberg.org root in production via NewClient.
type Client struct {
	httpClient *http.Client
	baseURL    string
}

// NewClient creates a Forgejo client pointed at codeberg.org's API
// root with a 60s per-request timeout (matches github's client). v0.1
// is unauthenticated; the codeberg.org public-API rate limit (1000
// req/h) is sufficient for a single-target analyze. Authenticated
// access via a CODEBERG_TOKEN env var is a follow-up when survey-side
// usage emerges.
func NewClient() *Client {
	return &Client{
		httpClient: &http.Client{
			Timeout:       60 * time.Second,
			CheckRedirect: checkRedirect,
		},
		baseURL: "https://codeberg.org/api/v1",
	}
}

// checkRedirect mirrors the github client's redirect policy: refuse
// non-HTTPS targets, bound the chain to <10 hops. The Authorization-
// strip rule is dropped because v0.1 is unauthenticated; bringing
// auth back in a follow-up requires re-introducing the cross-origin
// header-strip rule (see internal/signal/github/client.go for the
// rationale).
func checkRedirect(req *http.Request, via []*http.Request) error {
	if req.URL.Scheme != "https" {
		return fmt.Errorf("refusing redirect to non-HTTPS URL %s", req.URL.Redacted())
	}
	if len(via) >= 10 {
		return fmt.Errorf("too many redirects")
	}
	return nil
}

// repo represents the subset of /api/v1/repos/{owner}/{repo} response
// fields this collector consumes. The full Forgejo response carries
// 50+ fields; only the Tier 1 set is decoded.
//
// Field-name notes vs. github (the forge that uses different
// conventions for the same data):
//
//   - StarsCount maps to JSON "stars_count" (github: "stargazers_count")
//   - UpdatedAt maps to "updated_at" — closest analog to github's
//     PushedAt; Forgejo doesn't expose a distinct pushed_at field.
//   - Owner is the repo owner User object; Forgejo's User struct does
//     NOT carry an explicit User/Organization Type field, so
//     owner_type cannot be emitted from this single call. Adding it
//     requires a second /users/{u} or /orgs/{o} round-trip — deferred
//     to Tier 2.
type repo struct {
	Name            string    `json:"name"`
	FullName        string    `json:"full_name"`
	Description     string    `json:"description"`
	Owner           repoOwner `json:"owner"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	StarsCount      int       `json:"stars_count"`
	ForksCount      int       `json:"forks_count"`
	OpenIssuesCount int       `json:"open_issues_count"`
	Archived        bool      `json:"archived"`
}

// repoOwner is the subset of the embedded owner User struct this
// collector reads. login is the only field used today; full owner
// profile / type lives in Tier 2.
type repoOwner struct {
	Login string `json:"login"`
}

// get performs a GET against the Forgejo API at path, decoding the
// response into result. Returns ErrNotFound (wrapped) on 404 so
// callers can use errors.Is. Other non-200 responses surface as a
// status-only error — body is intentionally NOT included for the
// same reason github's client drops it (issue #93: response bodies
// are attacker-influenceable bytes that propagate to stderr / CI logs).
func (c *Client) get(ctx context.Context, path string, result any) error {
	url := c.baseURL + path

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
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
		return fmt.Errorf("forgejo API returned status %d", resp.StatusCode)
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

// GetRepo fetches /api/v1/repos/{owner}/{repo}. Returns ErrNotFound
// (wrapped) when the repo doesn't exist or is private to the
// unauthenticated client.
//
// owner and repoName are url.PathEscape'd before path concatenation as
// defense-in-depth: production callers already validate the inputs
// upstream (profile.NormalizeForgeRepoInput), and Forgejo's server-side
// login grammar rejects path-traversal-shaped names, but escaping at
// the call site keeps the client safe under any future caller and
// matches the gitlab client's projectIDPath discipline.
func (c *Client) GetRepo(ctx context.Context, owner, repoName string) (*repo, error) {
	var r repo
	if err := c.get(ctx, "/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(repoName), &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// IsOrg probes /api/v1/orgs/{name} to discriminate organization
// owners from user-account owners. Returns (true, nil) on 200,
// (false, nil) on 404 (== "not an org, must be a user"), and
// (false, err) on any other status or transport error.
//
// This is the discriminator for owner_type (Tier 1.5): Forgejo's
// repo response carries owner.login but no User/Organization
// discriminator on the embedded owner struct, unlike github
// (owner.type) and gitlab (namespace.kind on the same project
// call). The body of the org response is discarded here — only
// the status code matters for the user/org discrimination.
// owner_profile metadata comes from a separate /users/{name}
// call (see GetUser below), which works for both user accounts
// and organizations in Forgejo's data model.
func (c *Client) IsOrg(ctx context.Context, name string) (bool, error) {
	// url.PathEscape: defense-in-depth, see GetRepo for rationale.
	err := c.get(ctx, "/orgs/"+url.PathEscape(name), nil)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return false, err
}

// userProfile is the subset of /api/v1/users/{name} fields the
// owner_profile signal consumes. Forgejo's User schema includes a
// dozen-plus fields; only the ones that map onto the canonical
// owner_profile shape github's collector emits are decoded.
//
// Field-name notes vs. github's user struct:
//
//   - Username maps to JSON "username" (github: "login"). Both
//     fields semantically represent the login handle; the signal
//     value uses the canonical name "login".
//   - FullName maps to "full_name" (github: "name").
//   - Created maps to "created" (github: "created_at").
//   - FollowersCount maps to "followers_count" (github: "followers").
//
// Public-repo count and "company" don't appear in Forgejo's basic
// /users response — emitted as zero / empty in the signal value.
type userProfile struct {
	Username       string    `json:"username"`
	FullName       string    `json:"full_name"`
	Created        time.Time `json:"created"`
	FollowersCount int       `json:"followers_count"`
}

// GetUser fetches /api/v1/users/{name}. Works for BOTH user
// accounts and organization owners in Forgejo's data model
// (organizations are users-with-type-org and the same endpoint
// serves both record kinds). owner_profile callers route every
// owner through this call regardless of the IsOrg probe result.
//
// Returns ErrNotFound (wrapped) on 404. Other non-200 statuses
// surface as status-only errors per the client convention.
func (c *Client) GetUser(ctx context.Context, name string) (*userProfile, error) {
	var u userProfile
	// url.PathEscape: defense-in-depth, see GetRepo for rationale.
	if err := c.get(ctx, "/users/"+url.PathEscape(name), &u); err != nil {
		return nil, err
	}
	return &u, nil
}
