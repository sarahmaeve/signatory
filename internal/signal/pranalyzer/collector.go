package pranalyzer

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/sarahmaeve/pr-analyzer/analyzer"
	"github.com/sarahmaeve/pr-analyzer/codeshape"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal"
	"github.com/sarahmaeve/signatory/internal/signal/github"
)

const (
	// signalSource is the Source field stamped on every signal this
	// collector emits.
	signalSource = "pr-analyzer"

	// defaultSampleLimit bounds how many of the most-recent open PRs are
	// fetched-and-analyzed. The listing (and thus open_pr_count) is not
	// bounded; only the per-PR detail/files fetches — the expensive,
	// rate-limited calls (~2 per PR) — are capped.
	defaultSampleLimit = 30

	// defaultMaxLOC is the large-PR threshold passed to pr-analyzer's
	// code-shape collector. Without a non-zero threshold ExceedsMaxLOC is
	// always false and pr_oversized_share is uniformly zero; signatory's
	// collection path has no org config, so a built-in default applies.
	defaultMaxLOC = 1000
)

// prRateTypes are the open-PR-queue signal types other than
// open_pr_count: the rate / distribution rollups plus the detail
// carrier. Over an empty (or all-failed) sample these are recorded as
// absences rather than misleading zeros.
var prRateTypes = []string{
	"pr_author_association_distribution",
	"pr_first_time_contributor_share",
	"pr_test_touch_rate",
	"pr_dependency_manifest_touch_rate",
	"pr_agent_config_touch_rate",
	"pr_oversized_share",
	"pr_language_distribution",
	"pr_queue_samples",
}

// firstTimeAssociations are the GitHub author_association values that
// mark an author as first-time or unaffiliated with the repository.
var firstTimeAssociations = map[string]struct{}{
	"FIRST_TIME_CONTRIBUTOR": {},
	"FIRST_TIMER":            {},
	"NONE":                   {},
}

// Collector adapts the pr-analyzer mechanistic PR analyzer into a
// signatory signal collector. It characterizes a repository's open-PR
// queue: it lists open PRs, analyzes a bounded sample of the most recent
// ones through pr-analyzer, and folds the per-PR Code Shape / Engineer
// Profile signals into entity-level aggregate signals.
type Collector struct {
	src           analyzer.PRSource
	limit         int
	maxLOC        int
	authenticated bool
}

// NewCollector builds a Collector that fetches PRs through signatory's
// hardened github.Client (one auth / rate-limit / redirect / token-
// redaction path). An empty token disables the collector: it is still
// dispatched (so the orchestrator's collector list stays deterministic,
// independent of the environment) but Collect self-gates to an empty
// result — a 30-PR sample at ~2 calls each would exhaust GitHub's 60-
// req/hour anonymous budget and poison sibling collectors' rate budget.
func NewCollector(token string) *Collector {
	return &Collector{
		src:           newGitHubSource(github.NewClient(token)),
		limit:         defaultSampleLimit,
		maxLOC:        defaultMaxLOC,
		authenticated: token != "",
	}
}

// Name returns the collector identifier.
func (c *Collector) Name() string { return signalSource }

// Collect implements signal.Collector. It returns a non-nil error only
// when it cannot proceed at all (the target can't be parsed as a repo).
// A failed listing or per-PR fetch is recorded in the result, not
// returned as an error — the PR-queue signals are supplementary and must
// not nuke an otherwise-successful analyze run.
func (c *Collector) Collect(ctx context.Context, entity *profile.Entity) (*signal.CollectionResult, error) {
	// Self-gate: without an authenticated token the open-PR sample would
	// blow the anonymous rate budget, so do nothing. Empty result →
	// WorthNarrating suppresses the "0 signals" line.
	if !c.authenticated {
		return &signal.CollectionResult{}, nil
	}

	// Self-gate: a non-empty URL whose host isn't github.com is a
	// non-GitHub entity (gitlab, codeberg, …). Return empty before any
	// HTTP. Mirrors the github collector's gate.
	if entity.URL != "" && !github.IsGitHubHost(entity.URL) {
		return &signal.CollectionResult{}, nil
	}

	target := entity.URL
	if target == "" {
		target = entity.ShortName
	}
	owner, repoName, err := github.ParseRepoURL(target)
	if err != nil {
		return nil, fmt.Errorf("pr-analyzer collector: %w", err)
	}

	now := time.Now().UTC()
	ttl := 24 * time.Hour

	refs, err := c.src.ListOpenPRs(ctx, owner, repoName)
	if err != nil {
		// Listing is the foundational call; without it no PR signal can
		// be produced. Record every type as a failure (absence + run
		// diagnostic) but return nil so the analyze run continues.
		res := &signal.CollectionResult{}
		res.RecordFailure(entity.ID, "open_pr_count", signalSource, "failed to list open pull requests", true, now)
		for _, st := range prRateTypes {
			res.RecordFailure(entity.ID, st, signalSource, "failed to list open pull requests", true, now)
		}
		return res, nil
	}

	totalOpen := len(refs)
	sample := refs
	truncated := false
	if len(sample) > c.limit {
		sample = sample[:c.limit]
		truncated = true
	}

	cfg := analyzer.Config{CodeShape: codeshape.Config{MaxLOC: c.maxLOC}}
	analyses := make([]analyzer.Analysis, 0, len(sample))
	fetchFailures := 0
	for _, ref := range sample {
		a, err := analyzer.Analyze(ctx, c.src, ref, analyzer.WithConfig(cfg))
		if err != nil {
			fetchFailures++
			continue
		}
		analyses = append(analyses, a)
	}

	return aggregate(aggInput{
		entityID:      entity.ID,
		analyses:      analyses,
		totalOpen:     totalOpen,
		truncated:     truncated,
		fetchFailures: fetchFailures,
		maxLOC:        c.maxLOC,
	}, now, ttl), nil
}

// aggInput carries everything aggregate needs to fold a sample of
// analyzed PRs into entity-level signals.
type aggInput struct {
	entityID      string
	analyses      []analyzer.Analysis
	totalOpen     int
	truncated     bool
	fetchFailures int
	maxLOC        int
}

// aggregate folds the per-PR analyses into the entity-level open-PR-queue
// signals. open_pr_count is always emitted (the true total). When the
// effective sample is empty — an empty queue, or every sampled PR failed
// to fetch — the rate / distribution / detail signals are recorded as
// absences rather than misleading zeros.
func aggregate(in aggInput, now time.Time, ttl time.Duration) *signal.CollectionResult {
	res := &signal.CollectionResult{}
	res.RecordSignal(in.entityID, "open_pr_count", signalSource, now, ttl,
		map[string]any{"count": in.totalOpen})

	n := len(in.analyses)
	if n == 0 {
		reason := "no open pull requests to sample"
		retryable := false
		if in.totalOpen > 0 { // listed PRs, but every sampled fetch failed
			reason = "all sampled pull requests failed to fetch"
			retryable = true
		}
		for _, st := range prRateTypes {
			res.RecordAbsence(in.entityID, st, signalSource, reason, retryable, now)
		}
		return res
	}

	assocDist := map[string]int{}
	langDist := map[string]int{}
	manifestUnion := map[string]struct{}{}
	agentUnion := map[string]struct{}{}
	var firstTime, testTouched, manifestTouched, agentTouched, oversized int
	samples := make([]map[string]any, 0, n)

	for _, a := range in.analyses {
		assoc := a.EngineerProfile.AuthorAssociation
		if assoc == "" {
			assoc = "UNKNOWN"
		}
		assocDist[assoc]++
		if _, ok := firstTimeAssociations[assoc]; ok {
			firstTime++
		}

		cs := a.CodeShape
		if cs.TestsTouched {
			testTouched++
		}
		if len(cs.ManifestsTouched) > 0 {
			manifestTouched++
			for _, m := range cs.ManifestsTouched {
				manifestUnion[m] = struct{}{}
			}
		}
		if len(cs.AgentConfigPathsTouched) > 0 {
			agentTouched++
			for _, p := range cs.AgentConfigPathsTouched {
				agentUnion[p] = struct{}{}
			}
		}
		if cs.ExceedsMaxLOC {
			oversized++
		}
		for _, l := range cs.Languages {
			langDist[l]++
		}

		samples = append(samples, map[string]any{
			"number":                     a.PR.Ref.Number,
			"author":                     a.PR.Author,
			"author_association":         assoc,
			"additions":                  cs.LOC.Additions,
			"deletions":                  cs.LOC.Deletions,
			"total":                      cs.LOC.Total,
			"tests_touched":              cs.TestsTouched,
			"manifests_touched":          cs.ManifestsTouched,
			"agent_config_paths_touched": cs.AgentConfigPathsTouched,
			"languages":                  cs.Languages,
			"exceeds_max_loc":            cs.ExceedsMaxLOC,
		})
	}

	share := func(k int) float64 { return float64(k) / float64(n) }

	res.RecordSignal(in.entityID, "pr_author_association_distribution", signalSource, now, ttl,
		map[string]any{"sampled": n, "distribution": assocDist})
	res.RecordSignal(in.entityID, "pr_first_time_contributor_share", signalSource, now, ttl,
		map[string]any{"sampled": n, "first_time": firstTime, "share": share(firstTime)})
	res.RecordSignal(in.entityID, "pr_test_touch_rate", signalSource, now, ttl,
		map[string]any{"sampled": n, "touched": testTouched, "rate": share(testTouched)})
	res.RecordSignal(in.entityID, "pr_dependency_manifest_touch_rate", signalSource, now, ttl,
		map[string]any{"sampled": n, "touched": manifestTouched, "rate": share(manifestTouched),
			"manifests": slices.Sorted(maps.Keys(manifestUnion))})
	res.RecordSignal(in.entityID, "pr_agent_config_touch_rate", signalSource, now, ttl,
		map[string]any{"sampled": n, "touched": agentTouched, "rate": share(agentTouched),
			"paths": slices.Sorted(maps.Keys(agentUnion))})
	res.RecordSignal(in.entityID, "pr_oversized_share", signalSource, now, ttl,
		map[string]any{"sampled": n, "oversized": oversized, "share": share(oversized), "threshold": in.maxLOC})
	res.RecordSignal(in.entityID, "pr_language_distribution", signalSource, now, ttl,
		map[string]any{"sampled": n, "distribution": langDist})
	res.RecordSignal(in.entityID, "pr_queue_samples", signalSource, now, ttl,
		map[string]any{
			"sampled":        n,
			"total_open":     in.totalOpen,
			"truncated":      in.truncated,
			"fetch_failures": in.fetchFailures,
			"samples":        samples,
		})

	return res
}
