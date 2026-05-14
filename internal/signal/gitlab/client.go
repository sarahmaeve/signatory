// Package gitlab collects trust signals from the GitLab API. v0.1
// targets gitlab.com only; self-hosted GitLab instances need an
// explicit allow-list mechanism (same threat-model deferral as
// self-hosted Forgejo) and ship in a follow-up.
//
// Tier 1 (single GET against /api/v4/projects/{namespace_url_encoded}):
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
// Tier 1.5 (free on the same /projects call — no extra round-trip):
//
//   - owner_type   (namespace.kind: "group" → Organization, anything
//     else → User; the per-forge cost asymmetry vs. forgejo
//     — which needs a separate /orgs/{name} probe — is
//     deliberately landed in lockstep so cross-forge
//     posture rules consume one signal alphabet)
//
// Tier 2 (one extra round-trip routed by namespace.kind — /groups/<path>
// when "group", /users?username=<login> when "user"):
//
//   - owner_profile (login, name, created, account_age_days, type; same
//     canonical shape as github / forgejo, with missing
//     fields like public_repos / followers emitted as
//     zero so the shape stays consistent across forges)
//
// Still deferred: contributors, license. Each would need a further
// per-signal API call.
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
// The defensive network discipline (HTTPS-only redirects, response-
// body cap, drain-and-discard on non-2xx, sanitized status errors)
// lives in httpx.SecureClient; this package owns the per-method 404
// semantics (ListRootFilenames / GetRepoLanguage return zero on 404;
// GetProject / GetGroup propagate ErrNotFound; GetUserByUsername
// also folds "empty result array" into ErrNotFound) and the
// nested-namespace URL-encoding rule.
package gitlab

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/sarahmaeve/signatory/internal/httpx"
)

// ErrNotFound is the sentinel callers compare via errors.Is when the
// GitLab API responds 404. Wraps httpx.ErrNotFound so callers can
// also do errors.Is(err, httpx.ErrNotFound) for ecosystem-agnostic
// absence detection.
var ErrNotFound = fmt.Errorf("gitlab: %w", httpx.ErrNotFound)

// Client is a minimal GitLab REST client. The defensive HTTP pipeline
// is delegated to httpx.SecureClient; this package owns response-shape
// decoding and the per-method 404 semantics.
type Client struct {
	api *httpx.SecureClient
}

// NewClient creates a GitLab client pointed at gitlab.com's public
// API root. v0.1 is unauthenticated; gitlab.com's public-API rate
// limit (600 req/min unauth) is sufficient for a single-target
// analyze. Authenticated access via a GITLAB_TOKEN env var is a
// follow-up when survey-side usage emerges.
func NewClient() *Client {
	return &Client{
		api: httpx.NewSecureClient(httpx.WithBaseURL("https://gitlab.com/api/v4")),
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

// GetProject fetches /api/v4/projects/{namespace_url_encoded}.
// namespacePath is the full path (e.g. "gitlab-org/gitlab" or
// "gitlab-org/security/foo"); URL encoding happens inside via
// projectIDPath. Returns ErrNotFound (wrapped) when the project
// doesn't exist or is private to the unauthenticated client.
func (c *Client) GetProject(ctx context.Context, namespacePath string) (*project, error) {
	path := "/projects/" + projectIDPath(namespacePath)
	var p project
	err := c.api.GetJSON(ctx, path, &p, httpx.WithHeader("Accept", "application/json"))
	if err != nil {
		if errors.Is(err, httpx.ErrNotFound) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, path)
		}
		return nil, fmt.Errorf("gitlab API request for %s: %w", path, err)
	}
	return &p, nil
}

// group is the subset of /api/v4/groups/{path} response fields the
// owner_profile signal consumes when namespace.kind="group" on the
// project response. GitLab's full Group schema is 30+ fields; only
// the ones that map onto the canonical owner_profile shape (login,
// name, created) are decoded.
//
// Field-name notes vs. github's user struct:
//
//   - Path maps to "path" (github user: "login"). Both fields are
//     the URL-safe handle.
//   - Name maps to "name" (same field name as github's user.name).
//   - CreatedAt maps to "created_at".
//
// public_repos and followers don't appear in gitlab's basic
// /groups response — those would need separate /groups/<id>/projects
// counts and a member-count call respectively. Emitted as zero in
// the signal value so the shape stays consistent.
type group struct {
	ID        int       `json:"id"`
	Name      string    `json:"name"`
	Path      string    `json:"path"`
	FullPath  string    `json:"full_path"`
	CreatedAt time.Time `json:"created_at"`
}

// GetGroup fetches /api/v4/groups/{path_url_encoded}. namespacePath
// is the gitlab group full path (e.g. "gitlab-org" or nested-group
// "gitlab-org/security"); same URL-encoding rule as GetProject.
// Returns ErrNotFound (wrapped) on 404.
func (c *Client) GetGroup(ctx context.Context, namespacePath string) (*group, error) {
	path := "/groups/" + projectIDPath(namespacePath)
	var g group
	err := c.api.GetJSON(ctx, path, &g, httpx.WithHeader("Accept", "application/json"))
	if err != nil {
		if errors.Is(err, httpx.ErrNotFound) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, path)
		}
		return nil, fmt.Errorf("gitlab API request for %s: %w", path, err)
	}
	return &g, nil
}

// user is the subset of /api/v4/users response fields the
// owner_profile signal consumes when namespace.kind="user" on the
// project response.
type user struct {
	ID        int       `json:"id"`
	Username  string    `json:"username"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	State     string    `json:"state"`
}

// GetUserByUsername fetches /api/v4/users?username=<name> and
// returns the first matching user. GitLab's users API doesn't
// expose a /users/<username> form for unauthenticated clients —
// the search endpoint is the public lookup. Returns ErrNotFound
// (wrapped) when the search returns an empty array OR when the
// HTTP response is 404. Distinguishes the two: the array-empty
// case is a legitimate "no such user" and folds into ErrNotFound
// so callers can errors.Is against the single sentinel.
func (c *Client) GetUserByUsername(ctx context.Context, username string) (*user, error) {
	path := "/users?username=" + url.QueryEscape(username)
	var users []user
	err := c.api.GetJSON(ctx, path, &users, httpx.WithHeader("Accept", "application/json"))
	if err != nil {
		if errors.Is(err, httpx.ErrNotFound) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, path)
		}
		return nil, fmt.Errorf("gitlab API request for %s: %w", path, err)
	}
	if len(users) == 0 {
		return nil, fmt.Errorf("%w: /users?username=%s (no matching user)", ErrNotFound, username)
	}
	return &users[0], nil
}

// treeEntry is the subset of /api/v4/projects/{id}/repository/tree
// entries this client reads. GitLab tree entries carry Type ∈
// {"blob" (file), "tree" (directory), "commit" (submodule)}. Only
// "blob" entries inform ecosystem manifest detection — directories
// and submodules are dropped by ListRootFilenames.
//
// Field names are project-internal: the ecosystem.Source contract
// returns []string of file names, so the decoded shape stays per-forge
// rather than being lifted into a shared type.
type treeEntry struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// namespacePathFromOwnerRepo joins (owner, repoName) into the GitLab
// namespace path GetProject expects. NormalizeForgeRepoInput collapses
// gitlab inputs to two segments (owner, repo) — same shape as github/
// forgejo — so the ecosystem.Source contract takes only owner+name and
// we synthesize the namespace path here. Nested-group projects on
// gitlab.com (e.g., gitlab-org/security/foo) need a richer
// representation that the precheck path does not yet support; this is
// a known follow-up shared with NormalizeForgeRepoInput. Until then,
// the precheck addresses top-level projects only, which covers the
// common case (gitlab-org/gitlab, gitlab-org/cli, etc.).
func namespacePathFromOwnerRepo(owner, repoName string) string {
	return owner + "/" + repoName
}

// ListRootFilenames returns the names of regular files at the project
// root. Implements the ecosystem.Source contract so --network-precheck
// can detect ecosystems (go.mod, package.json, Cargo.toml, ...) on
// gitlab.com targets the same way it does on github.com / codeberg.org.
//
// Returns (nil, nil) for a missing or private project (ErrNotFound) so
// the detector can degrade to its language-only path instead of
// surfacing a hard error. Matches the github/forgejo Source shapes for
// the same reason.
//
// One API call against /repository/tree (root path by default).
// Directories and submodules are filtered out client-side; ecosystem
// detection only cares about files.
func (c *Client) ListRootFilenames(ctx context.Context, owner, repoName string) ([]string, error) {
	path := "/projects/" + projectIDPath(namespacePathFromOwnerRepo(owner, repoName)) + "/repository/tree"
	var entries []treeEntry
	err := c.api.GetJSON(ctx, path, &entries, httpx.WithHeader("Accept", "application/json"))
	if err != nil {
		if errors.Is(err, httpx.ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("gitlab API request for %s: %w", path, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.Type == "blob" {
			names = append(names, e.Name)
		}
	}
	return names, nil
}

// GetRepoLanguage returns GitLab's primary-language guess for a project
// — defined as the language with the highest percentage in
// /api/v4/projects/{id}/languages. GitLab computes per-language
// percentages from the project tree and exposes them as a JSON object
// {"Ruby": 80.5, "JavaScript": 12.3, ...}; we project to the single
// max-percentage language so the result matches github/forgejo's
// "primary language" semantic.
//
// Returns the empty string (not an error) when:
//   - The languages endpoint responds 404 (project doesn't exist or
//     is private to the unauthenticated client).
//   - The languages object is empty (docs-only / empty project).
//
// Aligns with github/forgejo's "empty when absent" handling so the
// ecosystem detector can apply the same "language=” → fall back to
// manifest" reasoning across forges.
func (c *Client) GetRepoLanguage(ctx context.Context, owner, repoName string) (string, error) {
	path := "/projects/" + projectIDPath(namespacePathFromOwnerRepo(owner, repoName)) + "/languages"
	var languages map[string]float64
	err := c.api.GetJSON(ctx, path, &languages, httpx.WithHeader("Accept", "application/json"))
	if err != nil {
		if errors.Is(err, httpx.ErrNotFound) {
			return "", nil
		}
		return "", fmt.Errorf("gitlab API request for %s: %w", path, err)
	}
	var topLang string
	var topPct float64
	for lang, pct := range languages {
		if pct > topPct {
			topPct = pct
			topLang = lang
		}
	}
	return topLang, nil
}
