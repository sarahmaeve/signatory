package cadence

import (
	"context"
	"encoding/json"
	"time"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal"
)

const (
	sourceName = "cadence"
	signalTTL  = 24 * time.Hour

	// cadenceFallowDays is the threshold beyond which a side is
	// considered fallow. Mirrors the trust-model.md temporal
	// vocabulary: "no commits for months" is fallow; this
	// signal pins the cutoff at 60 days for both commit and
	// publish sides.
	cadenceFallowDays = 60

	// cadenceNoiseDays is the |divergence| threshold below which
	// commit and publish cadence are considered synchronized.
	// Two days absorbs run-to-run timing noise (a Friday commit
	// landing in a Monday publish) without erasing the
	// post-incident-hardening shape (TanStack: divergence_days=6).
	cadenceNoiseDays = 2
)

// Collector emits commit_publish_cadence_divergence. Stateless
// across runs; reads two prior collectors' just-emitted signals
// via the in-run accumulator, computes a 4-field value, emits.
//
// inRun is the orchestrator's accumulated CollectionResult passed
// through CollectOpts. nil-safe — when inRun is nil or missing
// either of the two input signals for this entity, Collect emits
// nothing (silent skip, not absence). The reasoning: partial data
// is "this signal doesn't apply to this entity," not "we tried and
// failed." A repo-only entity has no last_publish; a registry-only
// entity has no last_commit; neither case is a failure of this
// collector.
type Collector struct {
	inRun *signal.CollectionResult
}

// NewCollector returns a Collector with no in-run wiring. Production
// callers chain WithInRun before passing the collector to the
// orchestrator; the orchestrator's collector loop calls Collect.
func NewCollector() *Collector { return &Collector{} }

// WithInRun wires the orchestrator's accumulated CollectionResult
// into the collector. Returns the receiver for chaining; mirrors
// adoption's WithInRun shape — same pattern, same nil-safety.
func (c *Collector) WithInRun(r *signal.CollectionResult) *Collector {
	c.inRun = r
	return c
}

// Name returns the collector identifier the orchestrator's progress
// narration keys on ("[cadence] Collected N signals").
func (c *Collector) Name() string { return sourceName }

// Collect emits the commit_publish_cadence_divergence signal for an
// entity when both a commit-side signal (last_commit, falling back
// to last_push) and last_publish are visible in the in-run
// accumulator.
//
// Never returns an error. Partial inputs produce no emission (the
// signal "doesn't apply"); both inputs present produce one signal.
func (c *Collector) Collect(_ context.Context, entity *profile.Entity) (*signal.CollectionResult, error) {
	result := &signal.CollectionResult{}
	if entity == nil || c.inRun == nil {
		return result, nil
	}

	commitDate, commitOK := readCommitDate(c.inRun, entity.ID)
	publishDate, publishOK := readPublishDate(c.inRun, entity.ID)
	if !commitOK || !publishOK {
		return result, nil
	}

	now := time.Now().UTC()
	commitDaysAgo := int(now.Sub(commitDate).Hours() / 24)
	publishDaysAgo := int(now.Sub(publishDate).Hours() / 24)
	divergence := publishDaysAgo - commitDaysAgo
	shape := classifyCadenceShape(commitDaysAgo, publishDaysAgo)

	value := map[string]any{
		"commit_days_ago":  commitDaysAgo,
		"publish_days_ago": publishDaysAgo,
		"divergence_days":  divergence,
		"shape":            shape,
	}
	// Enrich with prior_version_count when the version_count sibling
	// signal is in the in-run accumulator. This is the disambiguating
	// context the analyst layer (or a human reading the deltas view
	// in isolation) needs to read a both-fallow / paused shape
	// correctly: a high-version-count package on a paused cadence is
	// operationally stable, where a low-version-count package on the
	// same cadence is more likely abandoned. The shape value alone
	// does not distinguish these cases; prior_version_count does.
	// Absent when the in-run lacks version_count — silent skip on
	// the field, mirroring the collector's posture toward absent
	// inputs throughout.
	if count, ok := readVersionCount(c.inRun, entity.ID); ok {
		value["prior_version_count"] = count
	}

	result.RecordSignal(entity.ID, "commit_publish_cadence_divergence", sourceName, now, signalTTL, value)
	return result, nil
}

// classifyCadenceShape maps a (commit_days_ago, publish_days_ago)
// pair to one of four shapes. Order matters: both-fallow is checked
// first so it absorbs the edge case of two ancient timestamps that
// happen to be within the synchronization noise floor (a 200-day
// commit + 201-day publish reports "both-fallow", not "synchronized").
func classifyCadenceShape(commitDaysAgo, publishDaysAgo int) string {
	if commitDaysAgo > cadenceFallowDays && publishDaysAgo > cadenceFallowDays {
		return "both-fallow"
	}
	diff := publishDaysAgo - commitDaysAgo
	if diff < 0 {
		diff = -diff
	}
	if diff <= cadenceNoiseDays {
		return "synchronized"
	}
	if commitDaysAgo < publishDaysAgo {
		return "active-repo-paused-publishes"
	}
	return "active-publishes-fallow-repo"
}

// readCommitDate returns the most-recent (last-write-wins)
// commit-side timestamp from the in-run accumulator. Prefers
// last_commit (github-only, per-commit precision via the commits
// API); falls back to last_push (github + forgejo + gitlab,
// repo-event-level precision). nil-safe on inRun.
func readCommitDate(inRun *signal.CollectionResult, entityID string) (time.Time, bool) {
	for _, sigType := range []string{"last_commit", "last_push"} {
		if t, ok := findDateInRun(inRun, entityID, sigType, "date"); ok {
			return t, true
		}
	}
	return time.Time{}, false
}

// readPublishDate returns the most-recent (last-write-wins)
// last_publish timestamp from the in-run accumulator. Emitted by
// every registry collector (npm/pypi/cargo/gem/maven/gopublish).
// nil-safe on inRun.
func readPublishDate(inRun *signal.CollectionResult, entityID string) (time.Time, bool) {
	return findDateInRun(inRun, entityID, "last_publish", "published_at")
}

// readVersionCount returns the count field from a version_count
// signal in the in-run accumulator. Used by Collect to enrich the
// cadence emission with prior_version_count — the disambiguating
// context that lets a reader distinguish "stable, many releases,
// publish pause" from "thin history, may be abandoned" within the
// same shape value.
//
// nil-safe on inRun; returns (0, false) when no version_count signal
// is present for the entity or its count field is malformed.
// version_count is emitted by every registry collector (npm/pypi/
// cargo/gem/maven/gopublish) with a uniform {"count": int} shape,
// so a single reader works across ecosystems. JSON's numeric type is
// float64 after unmarshal into map[string]any; the conversion to int
// truncates fractional input that shouldn't be present in practice
// (version counts are integer cardinals).
func readVersionCount(inRun *signal.CollectionResult, entityID string) (int, bool) {
	if inRun == nil {
		return 0, false
	}
	var found float64
	var ok bool
	for _, sig := range inRun.Signals() {
		if sig.EntityID != entityID || sig.Type != "version_count" {
			continue
		}
		var v map[string]any
		if err := json.Unmarshal(sig.Value, &v); err != nil {
			continue
		}
		n, isFloat := v["count"].(float64)
		if !isFloat {
			continue
		}
		found = n
		ok = true
	}
	return int(found), ok
}

// findDateInRun walks the in-run accumulator for an entity's signal
// of the given type, extracts the string value at fieldName, parses
// it as RFC3339, and returns the parsed time. Last-write-wins on
// duplicates (mirrors adoption.readStarsFromInRun's discipline —
// each emitter only emits once per entity per run, but iteration
// stays tolerant).
func findDateInRun(inRun *signal.CollectionResult, entityID, signalType, fieldName string) (time.Time, bool) {
	if inRun == nil {
		return time.Time{}, false
	}
	var found time.Time
	var ok bool
	for _, sig := range inRun.Signals() {
		if sig.EntityID != entityID || sig.Type != signalType {
			continue
		}
		var v map[string]any
		if err := json.Unmarshal(sig.Value, &v); err != nil {
			continue
		}
		dateStr, isString := v[fieldName].(string)
		if !isString {
			continue
		}
		t, err := time.Parse(time.RFC3339, dateStr)
		if err != nil {
			continue
		}
		found = t
		ok = true
	}
	return found, ok
}
