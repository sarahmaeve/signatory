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
func TestRenderText_SummaryHeader(t *testing.T) {
	t.Parallel()
	tA := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	tB := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	tC := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)

	// Two signals, each with two transitions; three distinct runs total.
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
			mkGroup("stars",
				map[string]any{"count": float64(10)},
				map[string]any{"count": float64(20)},
				map[string]any{"count": float64(30)}),
			mkGroup("forks",
				map[string]any{"count": float64(1)},
				map[string]any{"count": float64(2)},
				map[string]any{"count": float64(3)}),
		},
	}
	var buf bytes.Buffer
	require.NoError(t, RenderText(&buf, in, TextOpts{}))
	got := buf.String()

	assert.Contains(t, got, "2 signals changed across 3 runs",
		"summary line must report changed signals and run count")
}

// TestRenderText_SummaryHeader_Pluralization: 1 signal / 1 run
// should read in the singular form. The CLI verb's most common
// case is "I just ran analyze twice; what's different?" — that's
// exactly N=1 run-pair, N=1 signal in many scenarios.
func TestRenderText_SummaryHeader_Pluralization(t *testing.T) {
	t.Parallel()
	prior := map[string]any{"count": float64(10)}
	current := map[string]any{"count": float64(11)}
	in := RenderInput{
		Target: "pkg:npm/x",
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

	assert.Contains(t, got, "1 signal changed across 2 runs",
		"singular 'signal' must appear when only one type changed")
	assert.NotContains(t, got, "1 signals",
		"plural 'signals' must not appear in singular case")
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
