package gitlab

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal"
)

// sourceName is the value stamped on every emitted signal. "gitlab"
// names the API contract; same layering choice as github / forgejo.
const sourceName = "gitlab"

// signalTTL is the cache lifetime stamped on each emitted signal.
// 24h matches github / forgejo TTLs — same forge-API metadata shape,
// same staleness tolerance.
const signalTTL = 24 * time.Hour

// isGitLabHost reports whether rawURL points at a recognized GitLab
// deployment. v0.1 recognizes only gitlab.com; self-hosted GitLab
// instances need an explicit allow-list (same threat-model deferral
// as self-hosted Forgejo, called out in profile/target.go's
// isGitLabURL doc).
//
// Mirror of github's isGitHubHost and forgejo's isForgejoHost — the
// orchestrator wires this collector unconditionally for every
// git-hosted entity, so the gate must live here. Without it, a
// github / codeberg URL would route to gitlab.com/api/v4 and either
// 404 or hit the wrong project.
func isGitLabHost(rawURL string) bool {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return false
	}
	host := u.Host
	if host == "" {
		host, _, _ = strings.Cut(u.Path, "/")
	}
	host = strings.TrimPrefix(host, "www.")
	return host == "gitlab.com"
}

// Collector emits Tier 1 metadata signals from the GitLab API for
// gitlab.com-hosted entities. See the package doc for the signal
// catalog and Tier 2 deferral.
type Collector struct {
	client *Client
}

// NewCollector creates a GitLab collector pointed at gitlab.com's
// public API root.
func NewCollector() *Collector {
	return &Collector{client: NewClient()}
}

// NewCollectorWithClient creates a Collector with a caller-provided
// Client. Test injection point — production code uses NewCollector.
func NewCollectorWithClient(c *Client) *Collector {
	return &Collector{client: c}
}

// Name returns the collector identifier the orchestrator's progress
// narration keys on ("[gitlab] Collected N signals").
func (c *Collector) Name() string { return sourceName }

// Collect gathers Tier 1 metadata signals for a gitlab.com-hosted
// entity. Self-gates on entity.URL host: non-gitlab URLs (and empty
// URLs) return a non-nil empty CollectionResult with nil error.
// Symmetric with github / forgejo / openssf collector self-gates.
//
// Single API call: GET /api/v4/projects/{namespace_url_encoded}. Six
// signals emitted on success (stars, forks, open_issues, archived,
// repo_age, last_push). 404 surfaces as a Collect error so the
// orchestrator records the per-collector failure but continues.
//
// owner_type / owner_profile / contributors / license deferred to
// Tier 1.5 / Tier 2.
func (c *Collector) Collect(ctx context.Context, entity *profile.Entity) (*signal.CollectionResult, error) {
	if entity == nil || entity.URL == "" || !isGitLabHost(entity.URL) {
		return &signal.CollectionResult{}, nil
	}

	namespacePath, err := parseProjectPath(entity.URL)
	if err != nil {
		return nil, fmt.Errorf("gitlab collector: %w", err)
	}

	now := time.Now().UTC()
	var result signal.CollectionResult

	p, err := c.client.GetProject(ctx, namespacePath)
	if err != nil {
		return nil, fmt.Errorf("get project: %w", err)
	}

	// last_push: GitLab doesn't carry a distinct pushed_at;
	// last_activity_at is the closest analog. Same canonical signal
	// type as github's PushedAt-derived emission and forgejo's
	// UpdatedAt-derived emission.
	result.RecordSignal(entity.ID, "last_push", sourceName, now, signalTTL,
		map[string]any{
			"date": p.LastActivityAt.Format(time.RFC3339),
			"era":  string(profile.ClassifyEra(p.LastActivityAt)),
		})
	result.RecordSignal(entity.ID, "repo_age", sourceName, now, signalTTL,
		map[string]any{
			"created":  p.CreatedAt.Format(time.RFC3339),
			"age_days": int(now.Sub(p.CreatedAt).Hours() / 24),
		})
	result.RecordSignal(entity.ID, "stars", sourceName, now, signalTTL,
		map[string]any{"count": p.StarCount})
	result.RecordSignal(entity.ID, "forks", sourceName, now, signalTTL,
		map[string]any{"count": p.ForksCount})
	result.RecordSignal(entity.ID, "open_issues", sourceName, now, signalTTL,
		map[string]any{"count": p.OpenIssuesCount})
	result.RecordSignal(entity.ID, "archived", sourceName, now, signalTTL,
		map[string]any{"archived": p.Archived})

	// owner_type (Tier 1.5): namespace.kind is on the same /projects
	// response, so this is free — no extra API call. GitLab's two
	// namespace kinds ("user" / "group") map onto the
	// forge-agnostic canonical values github's collector emits
	// ("User" / "Organization") so posture rules consume the same
	// "owner_type=Organization" string regardless of which forge
	// produced the signal.
	result.RecordSignal(entity.ID, "owner_type", sourceName, now, signalTTL,
		map[string]any{
			"type":  normalizeOwnerType(p.Namespace.Kind),
			"login": p.Namespace.Path,
		})

	return &result, nil
}

// normalizeOwnerType maps gitlab's namespace.kind values onto the
// canonical owner_type.type alphabet github uses. Anything other
// than "group" maps to "User" — gitlab's documented kinds are
// "user" and "group", and any future value (e.g. "subgroup")
// would also be a non-org-shape so "User" is a safer default than
// surfacing a third value that posture rules don't recognize.
//
// "Organization" instead of "Org": matches github's literal field
// value (Owner.Type is the string "Organization", not "Org").
// Cross-forge posture rules that match on the github form keep
// working without per-forge branching.
func normalizeOwnerType(gitlabKind string) string {
	if gitlabKind == "group" {
		return "Organization"
	}
	return "User"
}

// parseProjectPath extracts the namespace+project path from a
// gitlab.com URL. Accepts every shape upstream profile resolution
// produces (https/http with trailing /, .git suffix, deeper nested
// groups). Returns the un-encoded path (gitlab-org/gitlab,
// gitlab-org/security/foo) — the caller's GetProject handles URL
// encoding via projectIDPath.
//
// Does NOT validate against GitLab's project-name grammar; upstream
// profile.NormalizeForgeRepoInput already gated valid forge shape
// before the entity reached this collector.
func parseProjectPath(rawURL string) (string, error) {
	s := strings.TrimSpace(rawURL)
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimPrefix(s, "www.")
	s = strings.TrimPrefix(s, "gitlab.com/")
	s = strings.TrimSuffix(s, ".git")
	s = strings.TrimSuffix(s, "/")

	if s == "" {
		return "", fmt.Errorf("cannot parse gitlab project path from %q", rawURL)
	}
	// Need at least owner/repo.
	if !strings.ContainsRune(s, '/') {
		return "", fmt.Errorf("cannot parse gitlab project path from %q: expected at least owner/repo", rawURL)
	}
	return s, nil
}
