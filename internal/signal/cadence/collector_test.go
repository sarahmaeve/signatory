package cadence

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal"
)

// Compile-time interface check — pins the collector to the
// signal.Collector contract so collectorsFor can dispatch without
// per-collector type knowledge. Same pattern as adoption.
var _ signal.Collector = (*Collector)(nil)

// seedInRun builds an inRun pre-populated with last_commit (source
// github) and last_publish (source npm-registry) signals for entity
// "e1", with the given days-ago values. Tests that want different
// source signals (last_push instead of last_commit) build inRun
// manually.
func seedInRun(commitDaysAgo, publishDaysAgo int) *signal.CollectionResult {
	now := time.Now().UTC()
	inRun := &signal.CollectionResult{}
	inRun.RecordSignal("e1", "last_commit", "github", now, signalTTL,
		map[string]any{
			"date":     now.Add(-time.Duration(commitDaysAgo) * 24 * time.Hour).Format(time.RFC3339),
			"days_ago": commitDaysAgo,
		})
	inRun.RecordSignal("e1", "last_publish", "npm-registry", now, signalTTL,
		map[string]any{
			"latest_version": "1.0.0",
			"published_at":   now.Add(-time.Duration(publishDaysAgo) * 24 * time.Hour).Format(time.RFC3339),
			"days_ago":       publishDaysAgo,
		})
	return inRun
}

func entityForTest() *profile.Entity {
	return &profile.Entity{
		ID:           "e1",
		CanonicalURI: "pkg:npm/test",
		ShortName:    "test",
		Ecosystem:    "npm",
	}
}

// assertCadenceValue unmarshals the single emitted signal's value
// and runs the per-field assertions a test expresses by passing the
// expected commit / publish / divergence / shape.
func assertCadenceValue(t *testing.T, result *signal.CollectionResult,
	wantCommit, wantPublish, wantDivergence int, wantShape string) {
	t.Helper()
	sigs := result.Signals()
	require.Len(t, sigs, 1, "expected one cadence signal")
	assert.Equal(t, "commit_publish_cadence_divergence", sigs[0].Type)
	assert.Equal(t, "cadence", sigs[0].Source)
	var v map[string]any
	require.NoError(t, json.Unmarshal(sigs[0].Value, &v))
	assert.EqualValues(t, wantCommit, v["commit_days_ago"])
	assert.EqualValues(t, wantPublish, v["publish_days_ago"])
	assert.EqualValues(t, wantDivergence, v["divergence_days"])
	assert.Equal(t, wantShape, v["shape"])
}

// TestCollector_Synchronized: commit and publish within
// cadenceNoiseDays of each other → synchronized.
func TestCollector_Synchronized(t *testing.T) {
	t.Parallel()
	c := NewCollector().WithInRun(seedInRun(2, 3))
	result, err := c.Collect(context.Background(), entityForTest())
	require.NoError(t, err)
	assertCadenceValue(t, result, 2, 3, 1, "synchronized")
}

// TestCollector_ActiveRepoPausedPublishes models the TanStack
// 2026-05-12 post-incident-hardening fingerprint: commit today,
// publish 6 days ago, divergence=6, shape names the asymmetry.
func TestCollector_ActiveRepoPausedPublishes(t *testing.T) {
	t.Parallel()
	c := NewCollector().WithInRun(seedInRun(0, 6))
	result, err := c.Collect(context.Background(), entityForTest())
	require.NoError(t, err)
	assertCadenceValue(t, result, 0, 6, 6, "active-repo-paused-publishes")
}

// TestCollector_ActivePublishesFallowRepo: publish recent, commit
// stale. Rare pattern; could indicate registry-only republishing.
// Negative divergence_days encodes the asymmetry.
func TestCollector_ActivePublishesFallowRepo(t *testing.T) {
	t.Parallel()
	c := NewCollector().WithInRun(seedInRun(30, 2))
	result, err := c.Collect(context.Background(), entityForTest())
	require.NoError(t, err)
	assertCadenceValue(t, result, 30, 2, -28, "active-publishes-fallow-repo")
}

// TestCollector_BothFallow: both > cadenceFallowDays, classification
// trumps the divergence-based shapes. Pins the order-of-evaluation
// in classifyCadenceShape.
func TestCollector_BothFallow(t *testing.T) {
	t.Parallel()
	c := NewCollector().WithInRun(seedInRun(90, 95))
	result, err := c.Collect(context.Background(), entityForTest())
	require.NoError(t, err)
	assertCadenceValue(t, result, 90, 95, 5, "both-fallow")
}

// TestCollector_BothFallowOverridesSynchronization confirms a stale
// pair that LOOKS synchronized (small divergence) still reports
// both-fallow — the divergence framing requires recent activity to
// be meaningful.
func TestCollector_BothFallowOverridesSynchronization(t *testing.T) {
	t.Parallel()
	c := NewCollector().WithInRun(seedInRun(200, 201))
	result, err := c.Collect(context.Background(), entityForTest())
	require.NoError(t, err)
	// divergence_days = 1 (would be synchronized) but both > 60 → both-fallow.
	assertCadenceValue(t, result, 200, 201, 1, "both-fallow")
}

// TestCollector_FallsBackToLastPush confirms non-github forges
// (forgejo, gitlab) — which emit last_push but not last_commit —
// still produce a cadence signal.
func TestCollector_FallsBackToLastPush(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	inRun := &signal.CollectionResult{}
	inRun.RecordSignal("e1", "last_push", "forgejo", now, signalTTL,
		map[string]any{"date": now.Add(-2 * 24 * time.Hour).Format(time.RFC3339)})
	inRun.RecordSignal("e1", "last_publish", "npm-registry", now, signalTTL,
		map[string]any{"published_at": now.Add(-3 * 24 * time.Hour).Format(time.RFC3339)})

	c := NewCollector().WithInRun(inRun)
	result, err := c.Collect(context.Background(), entityForTest())
	require.NoError(t, err)
	sigs := result.Signals()
	require.Len(t, sigs, 1, "should emit signal using last_push when last_commit is absent")
}

// TestCollector_LastCommitPreferredOverLastPush: when BOTH signals
// are present (github emits both), last_commit wins. last_commit is
// per-commit precision; last_push is repo-event precision.
func TestCollector_LastCommitPreferredOverLastPush(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	inRun := &signal.CollectionResult{}
	// last_commit = 1 day ago, last_push = 10 days ago.
	// The collector should read last_commit (1) for the cadence.
	inRun.RecordSignal("e1", "last_commit", "github", now, signalTTL,
		map[string]any{"date": now.Add(-1 * 24 * time.Hour).Format(time.RFC3339)})
	inRun.RecordSignal("e1", "last_push", "github", now, signalTTL,
		map[string]any{"date": now.Add(-10 * 24 * time.Hour).Format(time.RFC3339)})
	inRun.RecordSignal("e1", "last_publish", "npm-registry", now, signalTTL,
		map[string]any{"published_at": now.Add(-3 * 24 * time.Hour).Format(time.RFC3339)})

	c := NewCollector().WithInRun(inRun)
	result, err := c.Collect(context.Background(), entityForTest())
	require.NoError(t, err)
	sigs := result.Signals()
	require.Len(t, sigs, 1)
	var v map[string]any
	require.NoError(t, json.Unmarshal(sigs[0].Value, &v))
	assert.EqualValues(t, 1, v["commit_days_ago"],
		"commit_days_ago should come from last_commit (1 day), not last_push (10 days)")
}

// TestCollector_NoCommitSignal: registry-only entity (no forge
// collector ran, or it produced no commit-side emission) → silent
// skip. No signal, no absence.
func TestCollector_NoCommitSignal(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	inRun := &signal.CollectionResult{}
	inRun.RecordSignal("e1", "last_publish", "npm-registry", now, signalTTL,
		map[string]any{"published_at": now.Add(-3 * 24 * time.Hour).Format(time.RFC3339)})

	c := NewCollector().WithInRun(inRun)
	result, err := c.Collect(context.Background(), entityForTest())
	require.NoError(t, err)
	assert.Empty(t, result.Signals(), "no commit signal → silent skip, no emission")
	assert.Zero(t, result.AbsenceCount(), "no absence either — partial inputs is not a failure")
}

// TestCollector_NoPublishSignal: repo-only entity (no registry
// collector queued) → silent skip.
func TestCollector_NoPublishSignal(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	inRun := &signal.CollectionResult{}
	inRun.RecordSignal("e1", "last_commit", "github", now, signalTTL,
		map[string]any{"date": now.Add(-2 * 24 * time.Hour).Format(time.RFC3339)})

	c := NewCollector().WithInRun(inRun)
	result, err := c.Collect(context.Background(), entityForTest())
	require.NoError(t, err)
	assert.Empty(t, result.Signals())
	assert.Zero(t, result.AbsenceCount())
}

// TestCollector_NilInRun: no inRun wired (e.g., test forgot
// WithInRun, or orchestrator hasn't accumulated yet). No panic,
// silent skip.
func TestCollector_NilInRun(t *testing.T) {
	t.Parallel()
	c := NewCollector()
	result, err := c.Collect(context.Background(), entityForTest())
	require.NoError(t, err)
	assert.Empty(t, result.Signals())
}

// TestCollector_NilEntity: defensive — orchestrator should never
// pass nil, but the collector handles it without panicking.
func TestCollector_NilEntity(t *testing.T) {
	t.Parallel()
	c := NewCollector().WithInRun(seedInRun(0, 0))
	result, err := c.Collect(context.Background(), nil)
	require.NoError(t, err)
	assert.Empty(t, result.Signals())
}

// TestCollector_Name pins the collector identifier the orchestrator's
// narration keys on.
func TestCollector_Name(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "cadence", NewCollector().Name())
}

// TestCollector_OtherEntityIgnored: inRun contains signals for a
// DIFFERENT entity. The collector must ignore them — the cadence
// signal is per-entity, not cross-entity.
func TestCollector_OtherEntityIgnored(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	inRun := &signal.CollectionResult{}
	// e2's signals, not e1's.
	inRun.RecordSignal("e2", "last_commit", "github", now, signalTTL,
		map[string]any{"date": now.Add(-2 * 24 * time.Hour).Format(time.RFC3339)})
	inRun.RecordSignal("e2", "last_publish", "npm-registry", now, signalTTL,
		map[string]any{"published_at": now.Add(-3 * 24 * time.Hour).Format(time.RFC3339)})

	c := NewCollector().WithInRun(inRun)
	result, err := c.Collect(context.Background(), entityForTest()) // asks about e1
	require.NoError(t, err)
	assert.Empty(t, result.Signals(),
		"cadence is per-entity; e2's signals must not produce an e1 emission")
}

// seedInRunWithVersionCount extends seedInRun with a version_count
// sibling signal. Tests covering the prior_version_count enrichment
// use this; tests that omit version_count use bare seedInRun and
// assert prior_version_count is absent from the emission.
func seedInRunWithVersionCount(commitDaysAgo, publishDaysAgo, versionCount int) *signal.CollectionResult {
	inRun := seedInRun(commitDaysAgo, publishDaysAgo)
	now := time.Now().UTC()
	inRun.RecordSignal("e1", "version_count", "npm-registry", now, signalTTL,
		map[string]any{
			"count": versionCount,
		})
	return inRun
}

// TestCollector_PriorVersionCountIncluded verifies that when a
// version_count sibling signal is in the in-run accumulator, the
// cadence emission carries a prior_version_count field. This is
// the disambiguating context the analyst layer (or a human reading
// the deltas view in isolation) needs to read a both-fallow shape
// correctly: a 228-version package with paused commits and publishes
// is stable; a 3-version package with the same shape is more likely
// abandoned. Same shape value, opposite trust posture.
func TestCollector_PriorVersionCountIncluded(t *testing.T) {
	t.Parallel()
	c := NewCollector().WithInRun(seedInRunWithVersionCount(0, 6, 228))
	result, err := c.Collect(context.Background(), entityForTest())
	require.NoError(t, err)
	sigs := result.Signals()
	require.Len(t, sigs, 1)
	var v map[string]any
	require.NoError(t, json.Unmarshal(sigs[0].Value, &v))
	assert.EqualValues(t, 228, v["prior_version_count"])
	// Existing fields unchanged — the enrichment is purely additive.
	assert.EqualValues(t, 0, v["commit_days_ago"])
	assert.EqualValues(t, 6, v["publish_days_ago"])
	assert.Equal(t, "active-repo-paused-publishes", v["shape"])
}

// TestCollector_PriorVersionCountOmittedWhenAbsent verifies that
// when no version_count sibling exists in the in-run accumulator,
// the cadence emission omits the prior_version_count field
// entirely — no zero, no null, no empty string. Mirrors the
// collector's existing partial-input discipline: absent context is
// silent, not falsified.
func TestCollector_PriorVersionCountOmittedWhenAbsent(t *testing.T) {
	t.Parallel()
	c := NewCollector().WithInRun(seedInRun(0, 6))
	result, err := c.Collect(context.Background(), entityForTest())
	require.NoError(t, err)
	sigs := result.Signals()
	require.Len(t, sigs, 1)
	var v map[string]any
	require.NoError(t, json.Unmarshal(sigs[0].Value, &v))
	_, present := v["prior_version_count"]
	assert.False(t, present,
		"prior_version_count should be absent (silent skip) when no version_count signal is in the in-run accumulator")
}

// TestCollector_PriorVersionCountIgnoresMalformedCount verifies that
// a version_count signal with a non-numeric count field is treated
// as if it weren't present: the cadence signal still emits its core
// observation, just without the enrichment. Mirrors findDateInRun's
// "if you can't read the field, pretend it isn't there" posture.
func TestCollector_PriorVersionCountIgnoresMalformedCount(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	inRun := seedInRun(0, 6)
	inRun.RecordSignal("e1", "version_count", "npm-registry", now, signalTTL,
		map[string]any{
			"count": "not a number", // string where a number is expected
		})
	c := NewCollector().WithInRun(inRun)
	result, err := c.Collect(context.Background(), entityForTest())
	require.NoError(t, err)
	sigs := result.Signals()
	require.Len(t, sigs, 1)
	var v map[string]any
	require.NoError(t, json.Unmarshal(sigs[0].Value, &v))
	_, present := v["prior_version_count"]
	assert.False(t, present,
		"malformed version_count.count should be ignored, not propagated")
}
