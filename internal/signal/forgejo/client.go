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
// The defensive network discipline (HTTPS-only redirects, response-
// body cap, drain-and-discard on non-2xx, sanitized status errors)
// lives in httpx.SecureClient; this package owns input validation,
// the IsOrg / ListRootFilenames / GetRepoLanguage 404-as-zero-value
// semantics, and the per-forge response-shape decoding.
package forgejo

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/sarahmaeve/signatory/internal/httpx"
)

// ErrNotFound is the sentinel callers compare via errors.Is when the
// Forgejo API responds 404. Wraps httpx.ErrNotFound so callers can
// also do errors.Is(err, httpx.ErrNotFound) for ecosystem-agnostic
// absence detection.
var ErrNotFound = fmt.Errorf("forgejo: %w", httpx.ErrNotFound)

// Client is a minimal Forgejo REST client. The defensive HTTP
// pipeline is delegated to httpx.SecureClient; this package owns
// response-shape decoding and the per-method 404 semantics.
type Client struct {
	api *httpx.SecureClient
}

// NewClient creates a Forgejo client pointed at codeberg.org's API
// root. v0.1 is unauthenticated; the codeberg.org public-API rate
// limit (1000 req/h) is sufficient for a single-target analyze.
// Authenticated access via a CODEBERG_TOKEN env var is a follow-up
// when survey-side usage emerges.
func NewClient() *Client {
	return &Client{
		api: httpx.NewSecureClient(httpx.WithBaseURL("https://codeberg.org/api/v1")),
	}
}

// NewClientWithBaseURL returns a Client whose endpoint points at the
// supplied base. Primary use case: test harnesses pointing the
// client at an httptest server. Production code should call NewClient.
func NewClientWithBaseURL(base string) *Client {
	return &Client{
		api: httpx.NewSecureClient(httpx.WithBaseURL(base)),
	}
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

// GetRepo fetches /api/v1/repos/{owner}/{repo}. Returns ErrNotFound
// (wrapped) when the repo doesn't exist or is private to the
// unauthenticated client.
//
// owner and repoName are url.PathEscape'd before path concatenation
// as defense-in-depth: production callers already validate the inputs
// upstream (profile.NormalizeForgeRepoInput), and Forgejo's server-
// side login grammar rejects path-traversal-shaped names, but
// escaping at the call site keeps the client safe under any future
// caller.
func (c *Client) GetRepo(ctx context.Context, owner, repoName string) (*repo, error) {
	path := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repoName)
	var r repo
	err := c.api.GetJSON(ctx, path, &r, httpx.WithHeader("Accept", "application/json"))
	if err != nil {
		if errors.Is(err, httpx.ErrNotFound) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, path)
		}
		return nil, fmt.Errorf("forgejo API request for %s: %w", path, err)
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
	path := "/orgs/" + url.PathEscape(name)
	_, err := c.api.Get(ctx, path, httpx.WithHeader("Accept", "application/json"))
	if err == nil {
		return true, nil
	}
	if errors.Is(err, httpx.ErrNotFound) {
		return false, nil
	}
	return false, fmt.Errorf("forgejo API request for %s: %w", path, err)
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
	path := "/users/" + url.PathEscape(name)
	var u userProfile
	err := c.api.GetJSON(ctx, path, &u, httpx.WithHeader("Accept", "application/json"))
	if err != nil {
		if errors.Is(err, httpx.ErrNotFound) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, path)
		}
		return nil, fmt.Errorf("forgejo API request for %s: %w", path, err)
	}
	return &u, nil
}

// repoContent is the subset of /api/v1/repos/{owner}/{repo}/contents
// entries this client reads. Forgejo's content endpoint mirrors
// github's: each entry carries Name and Type ∈ {"file", "dir",
// "symlink", "submodule"}. Only "file" entries inform ecosystem
// manifest detection — directories, symlinks, and submodules are
// dropped by ListRootFilenames.
//
// Field-name alignment with github.repoContent is deliberate: the
// ecosystem.Source contract (ListRootFilenames) returns []string of
// file names, so the per-forge decoded shape is internal to each
// client. Keeping the field names parallel makes the two
// implementations cheap to read side-by-side.
type repoContent struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// ListRootFilenames returns the names of regular files at the repo
// root. Implements the ecosystem.Source contract so --network-precheck
// can detect ecosystems (go.mod, package.json, Cargo.toml, ...) on
// codeberg.org targets the same way it does on github.com.
//
// Returns (nil, nil) for a missing or private repo (ErrNotFound), so
// the detector can degrade to its language-only path instead of
// surfacing a hard error. Matches github.Client.ListRootFilenames's
// shape for the same reason — the ecosystem detector reads from a
// single uniform interface across forges.
//
// One API call. Directories, symlinks, and submodules are filtered
// out client-side; ecosystem detection only cares about files.
func (c *Client) ListRootFilenames(ctx context.Context, owner, repoName string) ([]string, error) {
	path := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repoName) + "/contents"
	var entries []repoContent
	err := c.api.GetJSON(ctx, path, &entries, httpx.WithHeader("Accept", "application/json"))
	if err != nil {
		if errors.Is(err, httpx.ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("forgejo API request for %s: %w", path, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.Type == "file" {
			names = append(names, e.Name)
		}
	}
	return names, nil
}

// GetRepoLanguage returns Forgejo's primary-language guess for a
// repo — defined as the language with the highest byte count in
// /api/v1/repos/{owner}/{repo}/languages. Forgejo, like Gitea,
// computes per-language byte counts from the repo tree and exposes
// them as a JSON object {"Go": 12345, "JavaScript": 678, ...}; we
// project to the single max-byte language so the result matches
// github's "primary language" semantic.
//
// Returns the empty string (not an error) when:
//   - The languages endpoint responds 404 (repo doesn't exist or
//     is private to the unauthenticated client).
//   - The languages object is empty (docs-only / empty repo).
//
// These align with github.GetRepoLanguage's "empty when absent"
// handling so the ecosystem detector can apply the same
// "language=” → fall back to manifest" reasoning across forges.
func (c *Client) GetRepoLanguage(ctx context.Context, owner, repoName string) (string, error) {
	path := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repoName) + "/languages"
	var languages map[string]int64
	err := c.api.GetJSON(ctx, path, &languages, httpx.WithHeader("Accept", "application/json"))
	if err != nil {
		if errors.Is(err, httpx.ErrNotFound) {
			return "", nil
		}
		return "", fmt.Errorf("forgejo API request for %s: %w", path, err)
	}
	var topLang string
	var topBytes int64
	for lang, bytes := range languages {
		if bytes > topBytes {
			topBytes = bytes
			topLang = lang
		}
	}
	return topLang, nil
}
