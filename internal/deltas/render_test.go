package deltas

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Fixed timestamps for deterministic test output.
var (
	t1 = time.Date(2026, 5, 11, 19, 20, 39, 0, time.UTC)
	t2 = time.Date(2026, 5, 12, 15, 55, 8, 0, time.UTC)
)

// ------------------------------------------------------------------
// Text renderer tests
// ------------------------------------------------------------------

// TestRenderText_TanStackUnpublishGapAppears pins the headline
// human-readable shape against the TanStack scenario (sketch 2 in
// design/deltas.md). The output must:
//  1. Start with a clear header naming the target and window
//  2. Show the signal type + source + group on one line
//  3. Show the timestamp transition "T1 → T2 ◀ CHANGED"
//  4. Indent the per-field diff details below
//  5. Surface scalar transitions as "field: before → after"
//  6. Surface array additions as "field: gained N entries" with
//     per-element details
//  7. Surface added top-level keys with the value
func TestRenderText_TanStackUnpublishGapAppears(t *testing.T) {
	t.Parallel()
	prior := map[string]any{
		"unpublished_count":    float64(0),
		"unpublished_versions": []any{},
		"list_capped":          false,
	}
	current := map[string]any{
		"unpublished_count": float64(2),
		"unpublished_versions": []any{
			map[string]any{"version": "1.169.8", "published_at": "2026-05-11T19:26:17Z"},
			map[string]any{"version": "1.169.5", "published_at": "2026-05-11T19:20:42Z"},
		},
		"most_recent_unpublished_publish_time": "2026-05-11T19:26:17Z",
		"list_capped":                          false,
	}
	in := RenderInput{
		Target: "pkg:npm/@tanstack/react-router",
		Window: TimeWindow{Cutoff: time.Date(2026, 5, 11, 0, 0, 0, 0, time.UTC)},
		Groups: []SignalDelta{
			{
				Type:        "version_unpublish_observed",
				Source:      "npm-registry",
				SignalGroup: "publication",
				Observations: []Observation{
					{CollectedAt: t1, Value: prior},
					{CollectedAt: t2, Value: current},
				},
				PairDiffs: []ValueDiff{Diff(prior, current)},
			},
		},
	}

	var buf bytes.Buffer
	require.NoError(t, RenderText(&buf, in, TextOpts{}))
	got := buf.String()

	// Header.
	assert.Contains(t, got, "Deltas for pkg:npm/@tanstack/react-router",
		"output must name the target in a header")
	assert.Contains(t, got, "since 2026-05-11", "header must name the window")

	// Signal header line.
	assert.Contains(t, got, "version_unpublish_observed",
		"signal type must appear")
	assert.Contains(t, got, "npm-registry", "source must appear")
	assert.Contains(t, got, "publication", "signal group must appear")

	// Transition line with both timestamps and CHANGED marker.
	assert.Contains(t, got, "2026-05-11T19:20:39Z")
	assert.Contains(t, got, "2026-05-12T15:55:08Z")
	assert.Contains(t, got, "CHANGED", "must mark the changed pair")

	// Scalar diff.
	assert.Contains(t, got, "unpublished_count",
		"must surface the changed field name")
	assert.Contains(t, got, "0 → 2",
		"must show before → after for numeric scalars")

	// Added key.
	assert.Contains(t, got, "most_recent_unpublished_publish_time",
		"added top-level key must surface")

	// Array additions: the two new versions identified by stable
	// key. We don't pin exact format here (renderer choice), only
	// that the version strings appear in the output context.
	assert.Contains(t, got, "1.169.8")
	assert.Contains(t, got, "1.169.5")
}

// TestRenderText_ScalarDirectionArrow verifies that numeric scalar
// transitions get a signed delta appended in parentheses. Speeds
// the read for the most common case (incrementing counts): the
// reader sees the magnitude AND direction without subtracting in
// their head.
//
// String and boolean scalars are unchanged — the existing
// `before → after` form already conveys the direction.
func TestRenderText_ScalarDirectionArrow(t *testing.T) {
	t.Parallel()
	prior := map[string]any{
		"stars":      float64(14376),
		"forks":      float64(10),
		"divergence": float64(0.5),
		"shape":      "synchronized",
		"present":    true,
	}
	current := map[string]any{
		"stars":      float64(14381), // +5
		"forks":      float64(7),     // -3
		"divergence": float64(1.7),   // +1.2
		"shape":      "active",       // no arrow (string)
		"present":    false,          // no arrow (bool)
	}
	in := RenderInput{
		Target: "pkg:npm/example",
		Window: TimeWindow{All: true},
		Groups: []SignalDelta{{
			Type: "stars", Source: "github", SignalGroup: "criticality",
			Observations: []Observation{
				{CollectedAt: t1, Value: prior},
				{CollectedAt: t2, Value: current},
			},
			PairDiffs: []ValueDiff{Diff(prior, current)},
		}},
	}
	var buf bytes.Buffer
	require.NoError(t, RenderText(&buf, in, TextOpts{}))
	got := buf.String()

	// Positive integer delta.
	assert.Contains(t, got, "stars: 14376 → 14381 (+5)",
		"positive integer scalar must show signed delta")

	// Negative integer delta.
	assert.Contains(t, got, "forks: 10 → 7 (-3)",
		"negative integer scalar must show signed delta")

	// Float delta — accept either (+1.2) or (+1.20) etc.; pin the
	// sign and the integer-part magnitude.
	assert.Regexp(t, `divergence: 0\.5 → 1\.7 \(\+1\.?2?0?\)`, got,
		"float scalar must show signed delta")

	// String and bool scalars: no parenthesized delta.
	assert.Contains(t, got, `shape: "synchronized" → "active"`,
		"string scalar keeps existing form")
	assert.NotContains(t, got, `shape: "synchronized" → "active" (`,
		"string scalar must NOT gain a delta annotation")
	assert.Contains(t, got, "present: true → false")
	assert.NotContains(t, got, "present: true → false (",
		"bool scalar must NOT gain a delta annotation")
}

// TestRenderText_SummaryHeader verifies the at-a-glance line that
// appears between "Deltas for ..." and the first group block. The
// summary reports how many signals changed and how many distinct
// collection runs the rendered set spans — answers the reader's
// first question ("is this important?") without scrolling.
//
// Uses categorical (string) transitions so the test exercises the
// summary line itself, not the drift-bucketing path (covered
// separately in TestRenderText_BucketsForgeDrift).
func TestRenderText_SummaryHeader(t *testing.T) {
	t.Parallel()
	tA := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	tB := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	tC := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)

	mkGroup := func(typ string, p, q, r map[string]any) SignalDelta {
		return SignalDelta{
			Type: typ, Source: "github", SignalGroup: "criticality",
			Observations: []Observation{
				{CollectedAt: tA, Value: p},
				{CollectedAt: tB, Value: q},
				{CollectedAt: tC, Value: r},
			},
			PairDiffs: []ValueDiff{Diff(p, q), Diff(q, r)},
		}
	}
	in := RenderInput{
		Target: "pkg:npm/example",
		Window: TimeWindow{All: true},
		Groups: []SignalDelta{
			mkGroup("last_push_a",
				map[string]any{"date": "2026-05-10T00:00:00Z"},
				map[string]any{"date": "2026-05-11T00:00:00Z"},
				map[string]any{"date": "2026-05-12T00:00:00Z"}),
			mkGroup("last_push_b",
				map[string]any{"date": "2026-05-01T00:00:00Z"},
				map[string]any{"date": "2026-05-02T00:00:00Z"},
				map[string]any{"date": "2026-05-03T00:00:00Z"}),
		},
	}
	var buf bytes.Buffer
	require.NoError(t, RenderText(&buf, in, TextOpts{}))
	got := buf.String()

	assert.Contains(t, got, "2 signals changed across 3 runs",
		"summary line must report changed signals and run count")
}

// TestRenderText_SummaryHeader_Pluralization: 1 signal / 2 runs
// should read in the singular form ("1 signal") for the signal
// count. Uses a categorical change so the group doesn't route to
// the drift bucket.
func TestRenderText_SummaryHeader_Pluralization(t *testing.T) {
	t.Parallel()
	prior := map[string]any{"date": "2026-05-10T00:00:00Z"}
	current := map[string]any{"date": "2026-05-11T00:00:00Z"}
	in := RenderInput{
		Target: "pkg:npm/x",
		Window: TimeWindow{All: true},
		Groups: []SignalDelta{{
			Type: "last_push", Source: "github", SignalGroup: "vitality",
			Observations: []Observation{
				{CollectedAt: t1, Value: prior},
				{CollectedAt: t2, Value: current},
			},
			PairDiffs: []ValueDiff{Diff(prior, current)},
		}},
	}
	var buf bytes.Buffer
	require.NoError(t, RenderText(&buf, in, TextOpts{}))
	got := buf.String()

	assert.Contains(t, got, "1 signal changed across 2 runs",
		"singular 'signal' must appear when only one type changed")
	assert.NotContains(t, got, "1 signals changed",
		"plural 'signals changed' must not appear in singular case")
}

// TestRenderText_SummaryHeader_NoChanges replaces the prior
// per-group "no change" with a top-level "No changes in this
// window." line when nothing in the window moved AND
// IncludeUnchanged is off (default).
func TestRenderText_SummaryHeader_NoChanges(t *testing.T) {
	t.Parallel()
	value := map[string]any{"count": float64(42)}
	in := RenderInput{
		Target: "pkg:npm/stable",
		Window: TimeWindow{All: true},
		Groups: []SignalDelta{{
			Type: "stars", Source: "github", SignalGroup: "criticality",
			Observations: []Observation{
				{CollectedAt: t1, Value: value},
				{CollectedAt: t2, Value: value},
			},
			PairDiffs: []ValueDiff{Diff(value, value)},
		}},
	}
	var buf bytes.Buffer
	require.NoError(t, RenderText(&buf, in, TextOpts{}))
	got := buf.String()

	assert.Contains(t, got, "No changes in this window.",
		"unchanged window must produce an explicit summary line")
	assert.Contains(t, got, "--include-unchanged",
		"summary should hint at the flag that surfaces unchanged signals")
	assert.NotContains(t, got, "signals changed",
		"summary must not claim signals changed when none did")
}

// TestRenderText_SeverityOrdering_CategoricalFirst: a group whose
// only change is a boolean scalar (categorical) must render BEFORE
// a group whose only change is a numeric scalar (drift), even when
// alphabetical order would put the numeric group first.
func TestRenderText_SeverityOrdering_CategoricalFirst(t *testing.T) {
	t.Parallel()
	numericPrior := map[string]any{"count": float64(100)}
	numericNow := map[string]any{"count": float64(110)}
	boolPrior := map[string]any{"present": true}
	boolNow := map[string]any{"present": false}

	in := RenderInput{
		Target: "pkg:npm/x",
		Window: TimeWindow{All: true},
		Groups: []SignalDelta{
			// "a_metric" sorts BEFORE "z_state" alphabetically;
			// post-promotion, z_state (bool change) must render first.
			{
				Type: "a_metric", Source: "github", SignalGroup: "criticality",
				Observations: []Observation{
					{CollectedAt: t1, Value: numericPrior},
					{CollectedAt: t2, Value: numericNow},
				},
				PairDiffs: []ValueDiff{Diff(numericPrior, numericNow)},
			},
			{
				Type: "z_state", Source: "github", SignalGroup: "criticality",
				Observations: []Observation{
					{CollectedAt: t1, Value: boolPrior},
					{CollectedAt: t2, Value: boolNow},
				},
				PairDiffs: []ValueDiff{Diff(boolPrior, boolNow)},
			},
		},
	}

	var buf bytes.Buffer
	require.NoError(t, RenderText(&buf, in, TextOpts{}))
	got := buf.String()

	zIdx := strings.Index(got, "z_state")
	aIdx := strings.Index(got, "a_metric")
	require.Greater(t, zIdx, 0, "z_state must appear")
	require.Greater(t, aIdx, 0, "a_metric must appear")
	assert.Less(t, zIdx, aIdx,
		"categorical (bool) change must render before numeric drift")
}

// TestRenderText_SeverityOrdering_AddedRemovedPromotes: a top-level
// Added or Removed entry promotes the group above pure numeric
// drift. trusted_publishing losing its publisher fields is the
// load-bearing real-world case.
func TestRenderText_SeverityOrdering_AddedRemovedPromotes(t *testing.T) {
	t.Parallel()
	numericPrior := map[string]any{"count": float64(100)}
	numericNow := map[string]any{"count": float64(110)}
	// publisher fields disappear — Removed entries.
	prior := map[string]any{
		"present":           true,
		"publisher_kind":    "GitHub",
		"source_repository": "owner/repo",
	}
	now := map[string]any{"present": true} // present unchanged; publisher fields gone

	in := RenderInput{
		Target: "pkg:npm/x",
		Window: TimeWindow{All: true},
		Groups: []SignalDelta{
			{
				Type: "a_metric", Source: "github", SignalGroup: "criticality",
				Observations: []Observation{
					{CollectedAt: t1, Value: numericPrior},
					{CollectedAt: t2, Value: numericNow},
				},
				PairDiffs: []ValueDiff{Diff(numericPrior, numericNow)},
			},
			{
				Type: "z_publisher_drop", Source: "npm-registry", SignalGroup: "publication",
				Observations: []Observation{
					{CollectedAt: t1, Value: prior},
					{CollectedAt: t2, Value: now},
				},
				PairDiffs: []ValueDiff{Diff(prior, now)},
			},
		},
	}

	var buf bytes.Buffer
	require.NoError(t, RenderText(&buf, in, TextOpts{}))
	got := buf.String()

	zIdx := strings.Index(got, "z_publisher_drop")
	aIdx := strings.Index(got, "a_metric")
	assert.Less(t, zIdx, aIdx,
		"group with Removed entries promotes above pure-numeric group")
}

// TestRenderText_SeverityOrdering_ArrayPromotes: an Array change
// (gained/lost elements) promotes above numeric drift. Real-world
// driver: publisher_account_class gaining a service-account login.
func TestRenderText_SeverityOrdering_ArrayPromotes(t *testing.T) {
	t.Parallel()
	numericPrior := map[string]any{"count": float64(100)}
	numericNow := map[string]any{"count": float64(110)}
	arrPrior := map[string]any{
		"logins": []any{
			map[string]any{"login": "alice", "class": "human"},
		},
	}
	arrNow := map[string]any{
		"logins": []any{
			map[string]any{"login": "alice", "class": "human"},
			map[string]any{"login": "evil-bot", "class": "service-account"},
		},
	}

	in := RenderInput{
		Target: "pkg:npm/x",
		Window: TimeWindow{All: true},
		Groups: []SignalDelta{
			{
				Type: "a_metric", Source: "github", SignalGroup: "criticality",
				Observations: []Observation{
					{CollectedAt: t1, Value: numericPrior},
					{CollectedAt: t2, Value: numericNow},
				},
				PairDiffs: []ValueDiff{Diff(numericPrior, numericNow)},
			},
			{
				Type: "z_publisher_array", Source: "npm-registry", SignalGroup: "governance",
				Observations: []Observation{
					{CollectedAt: t1, Value: arrPrior},
					{CollectedAt: t2, Value: arrNow},
				},
				PairDiffs: []ValueDiff{Diff(arrPrior, arrNow)},
			},
		},
	}

	var buf bytes.Buffer
	require.NoError(t, RenderText(&buf, in, TextOpts{}))
	got := buf.String()

	zIdx := strings.Index(got, "z_publisher_array")
	aIdx := strings.Index(got, "a_metric")
	assert.Less(t, zIdx, aIdx,
		"group with array changes promotes above pure-numeric group")
}

// TestSortGroups_WithinTierAlphabetical: two groups in the same
// severity tier (both numeric-only) must sort alphabetically by
// (signal_group, type, source). Determinism within the tier is
// preserved.
func TestSortGroups_WithinTierAlphabetical(t *testing.T) {
	t.Parallel()
	numP := map[string]any{"count": float64(1)}
	numN := map[string]any{"count": float64(2)}
	mk := func(typ, src, grp string) SignalDelta {
		return SignalDelta{
			Type: typ, Source: src, SignalGroup: grp,
			Observations: []Observation{
				{CollectedAt: t1, Value: numP},
				{CollectedAt: t2, Value: numN},
			},
			PairDiffs: []ValueDiff{Diff(numP, numN)},
		}
	}
	in := []SignalDelta{
		mk("b", "github", "criticality"),
		mk("a", "github", "criticality"),
		mk("a", "github", "vitality"), // different signal_group sorts later alphabetically
	}
	out := SortGroups(in)
	assert.Equal(t, "a", out[0].Type)
	assert.Equal(t, "criticality", out[0].SignalGroup)
	assert.Equal(t, "b", out[1].Type)
	assert.Equal(t, "vitality", out[2].SignalGroup,
		"within the numeric-only tier, group ordering follows signal_group then type")
}

// TestSortGroups_CategoricalThenAlphabetical: across tiers,
// categorical-first; within each tier, the (signal_group, type,
// source) alphabetical rule continues to apply.
func TestSortGroups_CategoricalThenAlphabetical(t *testing.T) {
	t.Parallel()
	numP := map[string]any{"count": float64(1)}
	numN := map[string]any{"count": float64(2)}
	catP := map[string]any{"present": true}
	catN := map[string]any{"present": false}
	mk := func(typ string, p, q map[string]any) SignalDelta {
		return SignalDelta{
			Type: typ, Source: "s", SignalGroup: "g",
			Observations: []Observation{
				{CollectedAt: t1, Value: p},
				{CollectedAt: t2, Value: q},
			},
			PairDiffs: []ValueDiff{Diff(p, q)},
		}
	}
	in := []SignalDelta{
		mk("z_numeric", numP, numN), // numeric-only, alphabetically last
		mk("a_cat", catP, catN),     // categorical
		mk("b_numeric", numP, numN), // numeric-only
		mk("y_cat", catP, catN),     // categorical, alphabetically late
	}
	out := SortGroups(in)
	// Categorical tier first (a_cat, y_cat), then numeric (b_numeric, z_numeric).
	assert.Equal(t, "a_cat", out[0].Type)
	assert.Equal(t, "y_cat", out[1].Type)
	assert.Equal(t, "b_numeric", out[2].Type)
	assert.Equal(t, "z_numeric", out[3].Type)
}

// TestRenderText_SuppressesClockFields verifies that field
// names matching the clock-counter list (days_ago, account_age_days,
// divergence_days, etc.) are filtered out of the text rendering.
// These fields tick with the wall clock and don't reflect any
// change in the underlying world.
func TestRenderText_SuppressesClockFields(t *testing.T) {
	t.Parallel()
	// A mixed case: date changed (meaningful), days_ago changed
	// (clock-tick noise). Only the date should appear.
	prior := map[string]any{
		"date":     "2026-05-10T12:00:00Z",
		"days_ago": float64(3),
	}
	current := map[string]any{
		"date":     "2026-05-12T12:00:00Z",
		"days_ago": float64(1),
	}
	in := RenderInput{
		Target: "pkg:npm/x",
		Window: TimeWindow{All: true},
		Groups: []SignalDelta{{
			Type: "last_push", Source: "github", SignalGroup: "vitality",
			Observations: []Observation{
				{CollectedAt: t1, Value: prior},
				{CollectedAt: t2, Value: current},
			},
			PairDiffs: []ValueDiff{Diff(prior, current)},
		}},
	}
	var buf bytes.Buffer
	require.NoError(t, RenderText(&buf, in, TextOpts{}))
	got := buf.String()

	assert.Contains(t, got, `date: "2026-05-10T12:00:00Z" → "2026-05-12T12:00:00Z"`,
		"meaningful field (date) must surface")
	assert.NotContains(t, got, "days_ago:",
		"clock-counter field must be suppressed from text output")
}

// TestRenderText_SuppressesClockOnlyGroups: a group whose changes
// are entirely clock-tick noise should be treated as "unchanged"
// and suppressed under the default (--include-unchanged off).
func TestRenderText_SuppressesClockOnlyGroups(t *testing.T) {
	t.Parallel()
	// All three changed fields are clock-counters.
	prior := map[string]any{
		"commit_days_ago":  float64(0),
		"publish_days_ago": float64(6),
		"divergence_days":  float64(6),
		"shape":            "active-repo-paused-publishes",
	}
	current := map[string]any{
		"commit_days_ago":  float64(0),
		"publish_days_ago": float64(7),
		"divergence_days":  float64(7),
		"shape":            "active-repo-paused-publishes", // unchanged
	}
	in := RenderInput{
		Target: "pkg:npm/x",
		Window: TimeWindow{All: true},
		Groups: []SignalDelta{{
			Type: "commit_publish_cadence_divergence", Source: "cadence", SignalGroup: "vitality",
			Observations: []Observation{
				{CollectedAt: t1, Value: prior},
				{CollectedAt: t2, Value: current},
			},
			PairDiffs: []ValueDiff{Diff(prior, current)},
		}},
	}
	var buf bytes.Buffer
	require.NoError(t, RenderText(&buf, in, TextOpts{}))
	got := buf.String()

	assert.NotContains(t, got, "commit_publish_cadence_divergence",
		"group with only clock-tick changes must be suppressed entirely")
	assert.Contains(t, got, "No changes in this window.",
		"summary must reflect that no meaningful changes remain")
}

// TestRenderText_ClockSuppression_SummaryCount: the at-a-glance
// summary line counts only the signals that have meaningful
// changes after clock-field suppression — not the raw count.
func TestRenderText_ClockSuppression_SummaryCount(t *testing.T) {
	t.Parallel()
	// Two groups: one with a real string change, one with only
	// clock noise. Summary should report 1 signal, not 2.
	realPrior := map[string]any{"date": "2026-05-10T00:00:00Z"}
	realNow := map[string]any{"date": "2026-05-12T00:00:00Z"}
	clockPrior := map[string]any{"days_ago": float64(3)}
	clockNow := map[string]any{"days_ago": float64(1)}

	in := RenderInput{
		Target: "pkg:npm/x",
		Window: TimeWindow{All: true},
		Groups: []SignalDelta{
			{
				Type: "last_push", Source: "github", SignalGroup: "vitality",
				Observations: []Observation{
					{CollectedAt: t1, Value: realPrior},
					{CollectedAt: t2, Value: realNow},
				},
				PairDiffs: []ValueDiff{Diff(realPrior, realNow)},
			},
			{
				Type: "last_publish", Source: "npm-registry", SignalGroup: "vitality",
				Observations: []Observation{
					{CollectedAt: t1, Value: clockPrior},
					{CollectedAt: t2, Value: clockNow},
				},
				PairDiffs: []ValueDiff{Diff(clockPrior, clockNow)},
			},
		},
	}
	var buf bytes.Buffer
	require.NoError(t, RenderText(&buf, in, TextOpts{}))
	got := buf.String()

	assert.Contains(t, got, "1 signal changed",
		"summary must count only signals with non-clock changes")
	assert.NotContains(t, got, "2 signals changed")
}

// TestRenderJSON_PreservesClockFields: the JSON renderer (and
// therefore the MCP wire shape) must NOT suppress clock fields.
// Structured consumers see the raw data; suppression is a
// human-text-only presentation concern.
func TestRenderJSON_PreservesClockFields(t *testing.T) {
	t.Parallel()
	prior := map[string]any{"days_ago": float64(3), "date": "2026-05-10T00:00:00Z"}
	current := map[string]any{"days_ago": float64(1), "date": "2026-05-12T00:00:00Z"}
	in := RenderInput{
		Target: "pkg:npm/x",
		Window: TimeWindow{All: true},
		Groups: []SignalDelta{{
			Type: "last_push", Source: "github", SignalGroup: "vitality",
			Observations: []Observation{
				{CollectedAt: t1, Value: prior},
				{CollectedAt: t2, Value: current},
			},
			PairDiffs: []ValueDiff{Diff(prior, current)},
		}},
	}
	var buf bytes.Buffer
	require.NoError(t, RenderJSON(&buf, in))
	got := buf.String()

	assert.Contains(t, got, `"days_ago"`,
		"JSON output must preserve clock fields for structured consumers")
	assert.Contains(t, got, `"date"`)
}

// TestIsClockField pins the field-name pattern matcher. Any
// addition to the clock-field list needs a corresponding test
// case here.
func TestIsClockField(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		want bool
	}{
		{"days_ago", true},
		{"publish_days_ago", true},
		{"commit_days_ago", true},
		{"last_days_ago", true},
		{"account_age_days", true},
		{"age_days", true},
		{"repo_age_days", true},
		{"divergence_days", true},

		// Not clock fields.
		{"date", false},
		{"count", false},
		{"days", false}, // ambiguous; not in the list
		{"shape", false},
		{"present", false},
		{"version_checked", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, isClockField(tc.name))
		})
	}
}

// TestRenderText_BucketsForgeDrift verifies that drift-only groups
// (numeric scalar changes only, after clock-field suppression)
// collapse into a footer line. The meaningful categorical
// transition stays at the top; drift is summarized below.
func TestRenderText_BucketsForgeDrift(t *testing.T) {
	t.Parallel()
	// Meaningful: a date change (categorical string).
	mPrior := map[string]any{"date": "2026-05-10T00:00:00Z"}
	mNow := map[string]any{"date": "2026-05-12T00:00:00Z"}
	// Drift: pure numeric scalar.
	dPrior := map[string]any{"count": float64(100)}
	dNow := map[string]any{"count": float64(105)}

	in := RenderInput{
		Target: "pkg:npm/x",
		Window: TimeWindow{All: true},
		Groups: []SignalDelta{
			{
				Type: "last_push", Source: "github", SignalGroup: "vitality",
				Observations: []Observation{
					{CollectedAt: t1, Value: mPrior},
					{CollectedAt: t2, Value: mNow},
				},
				PairDiffs: []ValueDiff{Diff(mPrior, mNow)},
			},
			{
				Type: "stars", Source: "github", SignalGroup: "criticality",
				Observations: []Observation{
					{CollectedAt: t1, Value: dPrior},
					{CollectedAt: t2, Value: dNow},
				},
				PairDiffs: []ValueDiff{Diff(dPrior, dNow)},
			},
		},
	}
	var buf bytes.Buffer
	require.NoError(t, RenderText(&buf, in, TextOpts{}))
	got := buf.String()

	// Meaningful transition still renders in full.
	assert.Contains(t, got, `date: "2026-05-10T00:00:00Z" → "2026-05-12T00:00:00Z"`)
	// The CHANGED marker line for last_push is present.
	assert.Contains(t, got, "last_push (github, vitality)")

	// Drift group does NOT get the per-transition block; it's
	// summarized in a footer.
	assert.NotContains(t, got, "stars (github, criticality)",
		"drift-only group must not render its per-transition block")

	// Footer line names the drift field with its cumulative delta.
	assert.Contains(t, got, "forge drift")
	assert.Contains(t, got, "stars.count (+5)",
		"footer reports the cumulative delta for the drift field")
}

// TestRenderText_BucketsForgeDrift_AllDrift: when every changed
// group is drift-only, the meaningful count is zero and the
// summary line frames the output as drift.
func TestRenderText_BucketsForgeDrift_AllDrift(t *testing.T) {
	t.Parallel()
	mk := func(typ string, prior, now float64) SignalDelta {
		p := map[string]any{"count": prior}
		n := map[string]any{"count": now}
		return SignalDelta{
			Type: typ, Source: "github", SignalGroup: "criticality",
			Observations: []Observation{
				{CollectedAt: t1, Value: p},
				{CollectedAt: t2, Value: n},
			},
			PairDiffs: []ValueDiff{Diff(p, n)},
		}
	}
	in := RenderInput{
		Target: "pkg:npm/x",
		Window: TimeWindow{All: true},
		Groups: []SignalDelta{
			mk("forks", 100, 104),
			mk("stars", 1000, 1006),
		},
	}
	var buf bytes.Buffer
	require.NoError(t, RenderText(&buf, in, TextOpts{}))
	got := buf.String()

	assert.Contains(t, got, "0 signals changed",
		"meaningful count is zero when only drift moved")
	assert.Contains(t, got, "forge drift",
		"drift footer present")
	assert.Contains(t, got, "forks.count (+4)")
	assert.Contains(t, got, "stars.count (+6)")
	assert.NotContains(t, got, "◀ CHANGED",
		"no per-transition CHANGED markers when all groups are drift-only")
}

// TestRenderText_BucketsForgeDrift_CumulativeOverRuns: multi-
// transition drift signals report net delta plus "over N runs".
func TestRenderText_BucketsForgeDrift_CumulativeOverRuns(t *testing.T) {
	t.Parallel()
	tA := time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC)
	tB := time.Date(2026, 5, 11, 0, 0, 0, 0, time.UTC)
	tC := time.Date(2026, 5, 12, 0, 0, 0, 0, time.UTC)
	tD := time.Date(2026, 5, 13, 0, 0, 0, 0, time.UTC)

	mk := func(typ string, ts []time.Time, vals []float64) SignalDelta {
		obs := make([]Observation, len(ts))
		for i := range ts {
			obs[i] = Observation{CollectedAt: ts[i], Value: map[string]any{"count": vals[i]}}
		}
		diffs := make([]ValueDiff, 0, len(obs)-1)
		for i := 1; i < len(obs); i++ {
			diffs = append(diffs, Diff(obs[i-1].Value, obs[i].Value))
		}
		return SignalDelta{
			Type: typ, Source: "github", SignalGroup: "criticality",
			Observations: obs, PairDiffs: diffs,
		}
	}
	in := RenderInput{
		Target: "pkg:npm/x",
		Window: TimeWindow{All: true},
		Groups: []SignalDelta{
			mk("stars",
				[]time.Time{tA, tB, tC, tD},
				[]float64{1000, 1003, 1005, 1010}), // net +10 over 4 obs
		},
	}
	var buf bytes.Buffer
	require.NoError(t, RenderText(&buf, in, TextOpts{}))
	got := buf.String()

	assert.Regexp(t, `stars\.count \(\+10 over 4 runs\)`, got,
		"footer reports net delta and run count for multi-transition drift")
}

// TestRenderText_ExpandFlag: --expand restores per-transition
// rendering for drift groups (no bucketing). The footer disappears.
func TestRenderText_ExpandFlag(t *testing.T) {
	t.Parallel()
	dPrior := map[string]any{"count": float64(100)}
	dNow := map[string]any{"count": float64(105)}
	in := RenderInput{
		Target: "pkg:npm/x",
		Window: TimeWindow{All: true},
		Groups: []SignalDelta{{
			Type: "stars", Source: "github", SignalGroup: "criticality",
			Observations: []Observation{
				{CollectedAt: t1, Value: dPrior},
				{CollectedAt: t2, Value: dNow},
			},
			PairDiffs: []ValueDiff{Diff(dPrior, dNow)},
		}},
	}
	var buf bytes.Buffer
	require.NoError(t, RenderText(&buf, in, TextOpts{Expand: true}))
	got := buf.String()

	assert.Contains(t, got, "stars (github, criticality)",
		"--expand restores the per-transition rendering")
	assert.Contains(t, got, "count: 100 → 105 (+5)")
	assert.NotContains(t, got, "forge drift",
		"--expand drops the footer (drift renders inline)")
}

// TestRenderText_BucketsForgeDrift_MeaningfulOnly: when there's
// no drift at all, no footer line appears.
func TestRenderText_BucketsForgeDrift_MeaningfulOnly(t *testing.T) {
	t.Parallel()
	prior := map[string]any{"present": true}
	now := map[string]any{"present": false}
	in := RenderInput{
		Target: "pkg:npm/x",
		Window: TimeWindow{All: true},
		Groups: []SignalDelta{{
			Type: "trusted_publishing", Source: "npm-registry", SignalGroup: "publication",
			Observations: []Observation{
				{CollectedAt: t1, Value: prior},
				{CollectedAt: t2, Value: now},
			},
			PairDiffs: []ValueDiff{Diff(prior, now)},
		}},
	}
	var buf bytes.Buffer
	require.NoError(t, RenderText(&buf, in, TextOpts{}))
	got := buf.String()

	assert.Contains(t, got, "present: true → false")
	assert.NotContains(t, got, "forge drift",
		"no footer when there are no drift groups")
}

// TestRenderText_NoChanges_DefaultSuppresses confirms the
// unchanged-signal-suppression discipline: a SignalDelta with no
// per-pair changes is omitted by default. The output still names
// the target and window.
func TestRenderText_NoChanges_DefaultSuppresses(t *testing.T) {
	t.Parallel()
	value := map[string]any{"present": true, "version_checked": "1.0.0"}
	in := RenderInput{
		Target: "pkg:npm/stable",
		Window: TimeWindow{Cutoff: t1},
		Groups: []SignalDelta{
			{
				Type:        "trusted_publishing",
				Source:      "npm-registry",
				SignalGroup: "publication",
				Observations: []Observation{
					{CollectedAt: t1, Value: value},
					{CollectedAt: t2, Value: value},
				},
				PairDiffs: []ValueDiff{Diff(value, value)},
			},
		},
	}

	var buf bytes.Buffer
	require.NoError(t, RenderText(&buf, in, TextOpts{}))
	got := buf.String()

	assert.Contains(t, got, "pkg:npm/stable",
		"target still surfaces even when nothing changed")
	assert.NotContains(t, got, "CHANGED",
		"no CHANGED marker when there are no changes")
	assert.NotContains(t, got, "trusted_publishing",
		"signal must be suppressed by default when unchanged")
}

// TestRenderText_NoChanges_IncludeUnchangedShows confirms the
// IncludeUnchanged flag restores the suppressed signals — for the
// "confirm nothing else changed either" workflow.
func TestRenderText_NoChanges_IncludeUnchangedShows(t *testing.T) {
	t.Parallel()
	value := map[string]any{"present": true}
	in := RenderInput{
		Target: "pkg:npm/stable",
		Window: TimeWindow{Cutoff: t1},
		Groups: []SignalDelta{
			{
				Type:        "trusted_publishing",
				Source:      "npm-registry",
				SignalGroup: "publication",
				Observations: []Observation{
					{CollectedAt: t1, Value: value},
					{CollectedAt: t2, Value: value},
				},
				PairDiffs: []ValueDiff{Diff(value, value)},
			},
		},
	}

	var buf bytes.Buffer
	require.NoError(t, RenderText(&buf, in, TextOpts{IncludeUnchanged: true}))
	got := buf.String()

	assert.Contains(t, got, "trusted_publishing",
		"signal must appear when IncludeUnchanged=true")
	assert.Contains(t, got, "no change",
		"unchanged signal should be labeled to make it explicit")
}

// TestRenderText_EmptyInput is the no-target-data case: the store
// returned no signals for the entity in the window. The output
// should be graceful, not panic, not produce a misleading "all
// good" verdict.
func TestRenderText_EmptyInput(t *testing.T) {
	t.Parallel()
	in := RenderInput{
		Target: "pkg:npm/unknown",
		Window: TimeWindow{Cutoff: t1},
		Groups: nil,
	}

	var buf bytes.Buffer
	require.NoError(t, RenderText(&buf, in, TextOpts{}))
	got := buf.String()

	assert.Contains(t, got, "pkg:npm/unknown",
		"target must appear")
	assert.Contains(t, got, "no observations",
		"empty result needs an explicit reason, not silent zero output")
}

// TestRenderText_WindowKindLabels confirms the header label varies
// by TimeWindow.Kind(). All three kinds (since / last / all) must
// produce intelligible header text.
func TestRenderText_WindowKindLabels(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		window     TimeWindow
		wantPhrase string
	}{
		{"since", TimeWindow{Cutoff: t1}, "since 2026-05-11"},
		{"last", TimeWindow{Last: 5}, "last 5"},
		{"all", TimeWindow{All: true}, "full history"},
		{"range", TimeWindow{
			RangeStart: time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC),
			RangeEnd:   time.Date(2026, 5, 12, 23, 59, 59, 0, time.UTC),
		}, "range 2026-05-10T00:00:00Z to 2026-05-12T23:59:59Z"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := RenderInput{Target: "pkg:npm/x", Window: tc.window}
			var buf bytes.Buffer
			require.NoError(t, RenderText(&buf, in, TextOpts{}))
			assert.Contains(t, buf.String(), tc.wantPhrase)
		})
	}
}

// ------------------------------------------------------------------
// JSON renderer tests
// ------------------------------------------------------------------

// TestRenderJSON_RoundTrip confirms the JSON output decodes back
// into an equivalent RenderInput struct (modulo the TimeWindow.Kind
// derivation which is a marshal-only field).
func TestRenderJSON_RoundTrip(t *testing.T) {
	t.Parallel()
	prior := map[string]any{"unpublished_count": float64(0)}
	current := map[string]any{"unpublished_count": float64(2)}
	in := RenderInput{
		Target: "pkg:npm/@tanstack/react-router",
		Window: TimeWindow{Cutoff: t1},
		Groups: []SignalDelta{
			{
				Type:        "version_unpublish_observed",
				Source:      "npm-registry",
				SignalGroup: "publication",
				Observations: []Observation{
					{CollectedAt: t1, Value: prior},
					{CollectedAt: t2, Value: current},
				},
				PairDiffs: []ValueDiff{Diff(prior, current)},
			},
		},
	}

	var buf bytes.Buffer
	require.NoError(t, RenderJSON(&buf, in))

	var got map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &got))

	assert.Equal(t, "pkg:npm/@tanstack/react-router", got["target"])

	window, ok := got["window"].(map[string]any)
	require.True(t, ok, "window must be an object")
	assert.Equal(t, "since", window["kind"], "kind discriminator must be present")
	assert.Equal(t, "2026-05-11T19:20:39Z", window["cutoff"])

	groups, ok := got["groups"].([]any)
	require.True(t, ok)
	require.Len(t, groups, 1)
	group := groups[0].(map[string]any)
	assert.Equal(t, "version_unpublish_observed", group["type"])
	assert.Equal(t, "npm-registry", group["source"])
	assert.Equal(t, "publication", group["signal_group"])
}

// TestRenderJSON_EmptyGroups confirms the JSON shape stays valid
// when no signals were collected.
func TestRenderJSON_EmptyGroups(t *testing.T) {
	t.Parallel()
	in := RenderInput{
		Target: "pkg:npm/x",
		Window: TimeWindow{All: true},
		Groups: nil,
	}

	var buf bytes.Buffer
	require.NoError(t, RenderJSON(&buf, in))

	var got map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &got))
	assert.Equal(t, "pkg:npm/x", got["target"])
	// groups may serialize as `null` (nil slice) or `[]`; both are
	// acceptable. The contract is "decodable as a JSON value."
	_, has := got["groups"]
	assert.True(t, has, "groups key must always be present, even when nil")
}

// TestRenderJSON_DeterministicOrdering confirms that groups are
// sorted by (signal_group, type, source) before serialization, so
// two runs of the same input produce byte-identical JSON. Without
// this, scripts asserting on JSON would be flaky.
func TestRenderJSON_DeterministicOrdering(t *testing.T) {
	t.Parallel()
	in := RenderInput{
		Target: "pkg:npm/x",
		Window: TimeWindow{All: true},
		Groups: []SignalDelta{
			{Type: "b", Source: "s", SignalGroup: "publication"},
			{Type: "a", Source: "s", SignalGroup: "publication"},
			{Type: "z", Source: "s", SignalGroup: "vitality"},
		},
	}

	var buf1, buf2 bytes.Buffer
	require.NoError(t, RenderJSON(&buf1, in))
	require.NoError(t, RenderJSON(&buf2, in))
	assert.Equal(t, buf1.String(), buf2.String(),
		"identical input must produce identical bytes")

	// Ordering: vitality "z" comes before publication "a" only if
	// we sort group-then-type. The conventional ordering is
	// (signal_group, type, source) ascending — publication
	// alphabetically before vitality.
	idxA := strings.Index(buf1.String(), `"type": "a"`)
	idxB := strings.Index(buf1.String(), `"type": "b"`)
	idxZ := strings.Index(buf1.String(), `"type": "z"`)
	require.Positive(t, idxA)
	require.Positive(t, idxB)
	require.Positive(t, idxZ)
	assert.Less(t, idxA, idxB, "a must come before b within same group")
	assert.Less(t, idxB, idxZ,
		"publication group must come before vitality group alphabetically")
}
