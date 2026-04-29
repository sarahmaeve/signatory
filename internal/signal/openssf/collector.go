package openssf

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal"
)

// source is the value that lands in profile.Signal.Source for
// every emission from this collector. Constant so the string
// appears in exactly one place — and matches the Source value the
// signal-type-registry fixture in profile/signal_test.go pinned.
const source = "openssf-scorecard"

// signalType is the only type this collector emits. Registered in
// internal/signal/types.go (Hygiene group, ForgeryVeryHigh).
const signalType = "scorecard-check"

// defaultTTL matches the schema fixture in profile/signal_test.go
// and reflects Scorecard's roughly-weekly per-project refresh
// cadence — longer TTL would surface stale data, shorter would
// re-fetch unnecessarily often.
const defaultTTL = 7 * 24 * time.Hour

// notFoundReason is the absence-record reason when Scorecard's
// API returns 404. Distinct from network/5xx (RecordFailure with
// retryable=true) and "no GitHub source" (no record at all);
// "not in scorecards index" is a definitive negative answer
// from the upstream that the project hasn't been indexed.
const notFoundReason = "not in scorecards index"

// Collector fetches OpenSSF Scorecard data for GitHub-hosted
// entities and emits a single scorecard-check signal per entity.
//
// Eligibility filter: entity.URL must be parseable as a github
// repo (via profile.NormalizeGitHubRepoInput). Non-github entities
// receive an empty CollectionResult with nil error so the
// orchestrator can include the collector unconditionally in its
// dispatch list — symmetric with gopublish's behavior for
// non-Go entities.
type Collector struct {
	client *Client
}

// NewCollector returns a Collector bound to the public Scorecard
// API.
func NewCollector() *Collector {
	return &Collector{client: NewClient()}
}

// NewCollectorWithClient returns a Collector using the supplied
// Client. Tests pass a Client wired to an httptest server; production
// uses NewCollector.
func NewCollectorWithClient(c *Client) *Collector {
	return &Collector{client: c}
}

// Name identifies the collector — flows into source-tracking
// columns and the dogfood-metrics report.
func (c *Collector) Name() string { return source }

// Collect fetches the Scorecard for entity and emits one
// scorecard-check signal, one absence (on 404), or one failure
// (on network/5xx). Returns a non-nil CollectionResult in every
// case so callers can iterate without nil-guards.
//
// Per signal.Collector's contract, returning a non-nil error is
// reserved for "collection cannot proceed at all" — entity-shape
// mismatches and per-signal failures are recorded inside the
// CollectionResult, not surfaced as the error.
func (c *Collector) Collect(ctx context.Context, entity *profile.Entity) (*signal.CollectionResult, error) {
	owner, repo, ok := extractOwnerRepo(entity)
	if !ok {
		// Not a github-hosted entity — empty (non-nil) result.
		// Mirror of gopublish's non-Go-entity branch.
		return &signal.CollectionResult{}, nil
	}

	result := &signal.CollectionResult{}
	collectedAt := time.Now().UTC()

	sc, err := c.client.GetScorecard(ctx, owner, repo)
	switch {
	case err == nil:
		result.RecordSignal(entity.ID, signalType, source, collectedAt, defaultTTL, scorecardValue(sc))
	case errors.Is(err, ErrNotFound):
		// Definitive negative — Scorecard's crawler hasn't indexed
		// this project. Not retryable; re-fetching would yield
		// the same 404 until Scorecard's index catches up.
		result.RecordAbsence(entity.ID, signalType, source, notFoundReason, false, collectedAt)
	default:
		// Network / 5xx / decode / cap-exceeded. All retryable —
		// next refresh might succeed.
		result.RecordFailure(entity.ID, signalType, source, sanitizeFetchReason(err), true, collectedAt)
	}

	return result, nil
}

// extractOwnerRepo derives the github owner+repo pair from an
// entity. The entity's URL is the load-bearing field — populated
// for repo:github/* entities at creation time and for pkg:* entities
// after source resolution lands a github URL. Empty URL or non-github
// host → skip.
//
// Host-gating is explicit because profile.NormalizeGitHubRepoInput
// is intentionally permissive: it strips known prefixes and takes
// the first two path segments without verifying the host. Passing
// it `https://gitlab.com/foo/bar` yields owner=gitlab.com, name=foo
// — which would land as a wrong-project Scorecard fetch. The
// host check below short-circuits that.
func extractOwnerRepo(entity *profile.Entity) (owner, repo string, ok bool) {
	if entity == nil || entity.URL == "" {
		return "", "", false
	}
	if !isGitHubHost(entity.URL) {
		return "", "", false
	}
	_, owner, repo, err := profile.NormalizeGitHubRepoInput(entity.URL)
	if err != nil {
		return "", "", false
	}
	return owner, repo, true
}

// isGitHubHost reports whether rawURL points at github.com (with
// or without scheme; with or without www. prefix). Cheap host
// check sitting in front of NormalizeGitHubRepoInput so the
// permissive parser can't be tricked into emitting non-github
// owner/repo pairs from gitlab/bitbucket-style URLs.
func isGitHubHost(rawURL string) bool {
	// url.Parse handles the scheme'd forms cleanly; for bare
	// "github.com/owner/repo" inputs (no scheme), it parses the
	// whole string as Path. Handle both shapes uniformly.
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return false
	}
	host := u.Host
	if host == "" {
		// Bare "github.com/owner/repo" path-only form.
		host, _, _ = strings.Cut(u.Path, "/")
	}
	host = strings.TrimPrefix(host, "www.")
	return host == "github.com"
}

// scorecardValue builds the JSON-serializable value the signal
// stores. Field set is intentionally narrow — what an analyst or
// downstream tool needs to reason about the score — without
// embedding Scorecard's full per-check `details` and
// `documentation` blocks (high-volume, low-marginal-information).
//
// Per-check map is keyed by check name for fast lookup
// ("did Branch-Protection score above N?") rather than the wire
// shape's array. Renderers that want array order can sort the
// keys; the lookup form is the more common access pattern.
func scorecardValue(sc *Scorecard) map[string]any {
	checks := make(map[string]any, len(sc.Checks))
	for _, ck := range sc.Checks {
		checks[ck.Name] = map[string]any{
			"score":  ck.Score,
			"reason": ck.Reason,
		}
	}
	return map[string]any{
		"score":             sc.AggregateScore,
		"as_of":             sc.AsOf,
		"repo_commit":       sc.Repo.Commit,
		"scorecard_version": sc.ScorecardVersion.Version,
		"checks":            checks,
	}
}

// sanitizeFetchReason converts a fetch error into a reason string
// safe to persist in the signal row. Mirrors gopublish's helper
// — same shape so cross-collector failure diagnostics stay
// uniform in the dogfood report and posture audit.
func sanitizeFetchReason(err error) string {
	if errors.Is(err, ErrNotFound) {
		// Defensive — Collect's switch handles this branch
		// directly, but keeping the case here means a future
		// caller using sanitizeFetchReason on its own gets a
		// useful reason string instead of leaking the wrapped
		// "openssf: scorecard not found: kjd/idna" form.
		return notFoundReason
	}
	if errors.Is(err, context.Canceled) {
		return "collection canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "collection timed out"
	}
	msg := err.Error()
	const maxLen = 200
	if len(msg) > maxLen {
		msg = msg[:maxLen] + "…"
	}
	return msg
}

// compile-time interface assertion. Catches a future signature
// drift on signal.Collector at build time rather than at runtime
// when the orchestrator tries to dispatch us.
var _ signal.Collector = (*Collector)(nil)
