package forgejo

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal"
)

// sourceName is the value stamped on every emitted signal. "forgejo"
// names the API contract (Gitea / Forgejo), not the deployment
// (codeberg.org). Same layering choice as the github collector — when
// support for additional Forgejo deployments lands, they'll fold
// under this same source.
const sourceName = "forgejo"

// signalTTL is the cache lifetime stamped on each emitted signal.
// 24h matches the github collector's TTL — same forge-API metadata
// shape, same staleness tolerance.
const signalTTL = 24 * time.Hour

// isForgejoHost reports whether rawURL points at a recognized Forgejo
// deployment. v0.1 recognizes only codeberg.org; self-hosted Forgejo
// instances need an explicit allow-list mechanism (same threat-model
// deferral as self-hosted GitLab in profile/target.go's isGitLabURL).
//
// Mirror of github's isGitHubHost — the orchestrator wires this
// collector unconditionally for every git-hosted entity, so the gate
// must live here. Without it, a github URL would route to
// codeberg.org/api/v1, fail with 404, and break the run.
func isForgejoHost(rawURL string) bool {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return false
	}
	host := u.Host
	if host == "" {
		host, _, _ = strings.Cut(u.Path, "/")
	}
	host = strings.TrimPrefix(host, "www.")
	return host == "codeberg.org"
}

// Collector emits Tier 1 metadata signals from the Forgejo API for
// codeberg-hosted entities. See the package doc for the signal
// catalog and the design's deferred Tier 2 set.
type Collector struct {
	client *Client
}

// NewCollector creates a Forgejo collector pointed at codeberg.org's
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
// narration keys on ("[forgejo] Collected N signals").
func (c *Collector) Name() string { return sourceName }

// Collect gathers Tier 1 metadata signals for a codeberg-hosted
// entity. Self-gates on entity.URL host: non-codeberg URLs (and empty
// URLs) return a non-nil empty CollectionResult with nil error. The
// gate is symmetric with github's isGitHubHost and openssf's
// extractOwnerRepo — it admits the orchestrator's pattern of wiring
// every git-hosted collector unconditionally and letting each
// collector own the host check for its own forge.
//
// Single API call: GET /api/v1/repos/{owner}/{repo}. Six signals
// emitted on success (stars, forks, open_issues, archived, repo_age,
// last_push). 404 surfaces as a Collect error so the orchestrator
// records the per-collector failure but continues with the rest of
// the dispatch — matches github's GetRepo failure handling.
//
// owner_type / owner_profile / contributors / license are deferred
// to Tier 2 (need a second API call). See the package doc.
func (c *Collector) Collect(ctx context.Context, entity *profile.Entity) (*signal.CollectionResult, error) {
	if entity == nil || entity.URL == "" || !isForgejoHost(entity.URL) {
		return &signal.CollectionResult{}, nil
	}

	owner, repoName, err := parseRepoURL(entity.URL)
	if err != nil {
		return nil, fmt.Errorf("forgejo collector: %w", err)
	}

	now := time.Now().UTC()
	var result signal.CollectionResult

	r, err := c.client.GetRepo(ctx, owner, repoName)
	if err != nil {
		return nil, fmt.Errorf("get repo: %w", err)
	}

	// last_push: Forgejo doesn't carry a distinct pushed_at; updated_at
	// is the closest analog and advances on push. Same canonical
	// signal type as github's PushedAt-derived emission.
	result.RecordSignal(entity.ID, "last_push", sourceName, now, signalTTL,
		map[string]any{
			"date": r.UpdatedAt.Format(time.RFC3339),
			"era":  string(profile.ClassifyEra(r.UpdatedAt)),
		})
	result.RecordSignal(entity.ID, "repo_age", sourceName, now, signalTTL,
		map[string]any{
			"created":  r.CreatedAt.Format(time.RFC3339),
			"age_days": int(now.Sub(r.CreatedAt).Hours() / 24),
		})
	result.RecordSignal(entity.ID, "stars", sourceName, now, signalTTL,
		map[string]any{"count": r.StarsCount})
	result.RecordSignal(entity.ID, "forks", sourceName, now, signalTTL,
		map[string]any{"count": r.ForksCount})
	result.RecordSignal(entity.ID, "open_issues", sourceName, now, signalTTL,
		map[string]any{"count": r.OpenIssuesCount})
	result.RecordSignal(entity.ID, "archived", sourceName, now, signalTTL,
		map[string]any{"archived": r.Archived})

	// owner_type (Tier 1.5): Forgejo's repo response doesn't carry
	// the User/Organization discriminator, so probe /orgs/{name}.
	// 200 → Organization; 404 → User; anything else (5xx, timeout,
	// transport error) records a per-signal failure and leaves the
	// Tier 1 signals above intact. Isolation property is load-
	// bearing — pinned by
	// TestCollector_OwnerType_ProbeFailure_DoesNotBreakOtherSignals.
	c.emitOwnerType(ctx, &result, entity.ID, r.Owner.Login, now)

	return &result, nil
}

// emitOwnerType performs the /orgs/{name} probe AND the
// /users/{name} fetch and records owner_type + owner_profile
// signals (or per-signal failures on probe / fetch errors).
//
// Two API calls, intentionally paired:
//
//   - /orgs/{name} is the User/Organization discriminator. Body
//     ignored — only the status code matters.
//   - /users/{name} carries the owner_profile metadata. Works for
//     BOTH user accounts and organization owners in Forgejo's
//     data model (orgs are users-with-type-org and the same
//     endpoint serves both record kinds).
//
// Isolation properties pinned by tests:
//
//   - Tier 1 emissions above this call are unaffected by anything
//     here (pinned by
//     TestCollector_OwnerType_ProbeFailure_DoesNotBreakOtherSignals).
//   - owner_type and owner_profile fail independently: a
//     /users/{name} fetch failure records as an owner_profile
//     failure without disturbing the owner_type signal (which
//     succeeded from the /orgs/{name} probe).
//
// "Organization" / "User" match github's canonical value alphabet
// (Owner.Type is the literal string "Organization") so cross-forge
// posture rules consume the same value regardless of which forge
// produced the signal. See gitlab's normalizeOwnerType for the
// same mapping going the other direction.
func (c *Collector) emitOwnerType(ctx context.Context, result *signal.CollectionResult,
	entityID, login string, now time.Time) {
	isOrg, err := c.client.IsOrg(ctx, login)
	if err != nil {
		// Non-404 error on the probe — both owner_type and
		// owner_profile fail (we don't know the discriminator and
		// can't classify the owner). 5xx / transport-error class is
		// retryable; next refresh might succeed.
		reason := sanitizeOwnerEndpointError(err)
		result.RecordFailure(entityID, "owner_type", sourceName, reason, true, now)
		result.RecordFailure(entityID, "owner_profile", sourceName, reason, true, now)
		return
	}
	ownerType := "User"
	if isOrg {
		ownerType = "Organization"
	}
	result.RecordSignal(entityID, "owner_type", sourceName, now, signalTTL,
		map[string]any{"type": ownerType, "login": login})

	// Fetch metadata for owner_profile. owner_type was already
	// emitted above, so a /users/{name} failure here only affects
	// owner_profile. Independent failure paths.
	u, err := c.client.GetUser(ctx, login)
	if err != nil {
		result.RecordFailure(entityID, "owner_profile", sourceName,
			sanitizeOwnerEndpointError(err), true, now)
		return
	}
	result.RecordSignal(entityID, "owner_profile", sourceName, now, signalTTL,
		map[string]any{
			"login":            u.Username,
			"name":             u.FullName,
			"created":          u.Created.Format(time.RFC3339),
			"account_age_days": int(now.Sub(u.Created).Hours() / 24),
			"followers":        u.FollowersCount,
			"type":             ownerType,
		})
}

// sanitizeOwnerEndpointError trims an IsOrg or GetUser error to a
// short reason safe for the persisted CollectionError.Reason field.
// Forgejo's Client.get builds error messages with the path included
// (e.g. "forgejo API returned status 503"); the path is
// operator-known and contains no secret, so passing the raw string
// is acceptable. Bounded to avoid blowing up the persisted record
// on a pathological wrap chain.
func sanitizeOwnerEndpointError(err error) string {
	s := err.Error()
	const maxLen = 200
	if len(s) > maxLen {
		s = s[:maxLen] + "…"
	}
	return s
}

// parseRepoURL extracts owner and repo name from a codeberg.org URL.
// Permissive prefix-strip (https/http/www) followed by path-segment
// extraction — same shape as github's ParseRepoURL but anchored on
// codeberg.org. Does NOT validate against the Forgejo name grammar;
// upstream profile.NormalizeForgeRepoInput already gated valid forge
// shape before the entity reached this collector.
func parseRepoURL(rawURL string) (owner, repoName string, err error) {
	s := strings.TrimSpace(rawURL)
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimPrefix(s, "www.")
	s = strings.TrimPrefix(s, "codeberg.org/")
	s = strings.TrimSuffix(s, ".git")
	s = strings.TrimSuffix(s, "/")

	parts := strings.SplitN(s, "/", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("cannot parse codeberg owner/repo from %q", rawURL)
	}
	return parts[0], parts[1], nil
}
