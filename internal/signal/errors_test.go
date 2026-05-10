package signal

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/sarahmaeve/signatory/internal/profile"
)

// TestCollectionResult_WorthNarrating_EmptyResultIsSilent pins the
// narration-suppression rule: a CollectionResult with zero collected
// entries and zero failures is the unambiguous "this collector
// decided not to apply" signature (every forge-self-gated collector
// produces this shape: it returns &signal.CollectionResult{} early).
// The dispatcher's per-collector "[name] Collected 0 signals"
// narration is noise in that case — predictable, scales with the
// per-forge collector count, and tells the operator nothing they
// don't already know about their target.
//
// Inversion: anything in either Collected or Failures means the
// collector did real work — even if all of it was negative
// (absences) or unsuccessful (failures). Those still narrate.
// Operators benefit from "github API call failed: 503" or
// "exfil-host check found nothing"; they don't benefit from
// "[github] Collected 0 signals" for a codeberg target.
func TestCollectionResult_WorthNarrating_EmptyResultIsSilent(t *testing.T) {
	t.Parallel()

	var r CollectionResult
	assert.False(t, r.WorthNarrating(),
		"empty + no failures = self-gated no-op; narration must be suppressed so multi-forge dispatch output stays readable")
}

// TestCollectionResult_WorthNarrating_WithSignalsIsNarrated pins
// the positive case: any successful emission keeps the narration
// line. Forge metadata collectors (github / forgejo / gitlab) that
// produced signals must surface their counts so the operator can
// see what was collected.
func TestCollectionResult_WorthNarrating_WithSignalsIsNarrated(t *testing.T) {
	t.Parallel()

	var r CollectionResult
	r.RecordSignal("e1", "stars", "github", time.Now().UTC(), time.Hour,
		map[string]any{"count": 100})
	assert.True(t, r.WorthNarrating(),
		"successful emissions must narrate; suppressing them would hide the actual work done")
}

// TestCollectionResult_WorthNarrating_WithAbsenceIsNarrated pins a
// trickier case: an absence record (e.g. "no per-developer signed
// commits in window") is a real observation, not a no-op. The
// collector ran, queried the data, and produced a definitive
// negative — that's worth surfacing. Absences land in Collected
// (not Failures), so the empty-Collected check naturally captures
// them.
func TestCollectionResult_WorthNarrating_WithAbsenceIsNarrated(t *testing.T) {
	t.Parallel()

	var r CollectionResult
	r.RecordAbsence("e1", "commit_signing_keys", "git",
		"no signed commits in window", false, time.Now().UTC())
	assert.True(t, r.WorthNarrating(),
		"absences are real observations (collector ran, produced a definitive negative) and must narrate")
}

// TestCollectionResult_WorthNarrating_WithFailuresIsNarrated pins
// the failure case: a collector that hit a 5xx or rate limit
// recorded the failure but emitted no signals. The line MUST
// narrate so the operator knows their analyze run had a hiccup.
// Suppressing failures would hide real problems behind a clean-
// looking output.
//
// Edge case: RecordFailure also calls RecordAbsence internally,
// so a failure populates BOTH Failures and Collected. The
// narrate-on-non-empty test would catch this via Collected alone;
// this test pins the invariant from the Failures side so a future
// refactor that decouples the two paths can't silently hide
// failure-only results.
func TestCollectionResult_WorthNarrating_WithFailuresIsNarrated(t *testing.T) {
	t.Parallel()

	var r CollectionResult
	r.RecordFailure("e1", "scorecard-check", "openssf-scorecard",
		"network timeout", true, time.Now().UTC())
	assert.True(t, r.WorthNarrating(),
		"failures must narrate so operators see real problems; a clean-looking output that hides 503s would be operator-hostile")

	// Verify the invariant we're depending on: RecordFailure populates
	// both fields. If a future RecordFailure refactor splits them,
	// this assertion fails loudly and the WorthNarrating logic
	// needs revisiting.
	assert.NotEmpty(t, r.Collected, "RecordFailure populates Collected (via internal RecordAbsence)")
	assert.NotEmpty(t, r.Failures, "RecordFailure populates Failures")
}

// TestCollectionResult_WorthNarrating_NilReceiverIsSilent guards
// against the nil-CollectionResult corner — a future collector that
// returns nil instead of &CollectionResult{} on its self-gate
// branch must not panic the dispatcher's narration step. Silent on
// nil is the right answer: nothing to say, nothing to crash on.
//
// Today every collector returns &CollectionResult{} on self-gate
// (asserted by their package tests), but the dispatcher's
// post-collector loop calls Methods on the result and would panic
// on nil. Adding the receiver-nil branch makes this method safe
// to call regardless.
func TestCollectionResult_WorthNarrating_NilReceiverIsSilent(t *testing.T) {
	t.Parallel()

	var r *CollectionResult // nil
	assert.False(t, r.WorthNarrating(),
		"nil receiver must be silent (not panic); guards against a future collector returning nil on self-gate")
}

// Compile-time confirmation that the test's seed signal type is
// real. Without this, a typo in "stars" / "commit_signing_keys" /
// "scorecard-check" would be a silent test failure (RecordSignal
// panics on unregistered types, which fails the test loudly, but
// having the dependency declared here documents the expected
// registry shape).
var _ = profile.SignalGroup("vitality")
