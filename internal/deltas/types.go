package deltas

import (
	"encoding/json"
	"sort"
	"time"
)

// Observation is one signal collection: when it happened and what
// value was recorded. Renderers consume Observations and the diffs
// between them; the CLI verb produces Observations by walking
// store.GetSignals output and grouping by (type, source).
type Observation struct {
	CollectedAt time.Time      `json:"collected_at"`
	Value       map[string]any `json:"value"`
}

// SignalDelta carries the chronological observations for one
// (type, source) pair plus the per-pair diffs between them.
//
// Observations are sorted by CollectedAt ascending (oldest first).
// PairDiffs has len(Observations)-1 entries: PairDiffs[i] is the
// diff between Observations[i] and Observations[i+1].
//
// A SignalDelta with no diffs (len(PairDiffs)==0 or every diff
// reports !HasChanges()) is "unchanged" — the renderer's
// IncludeUnchanged flag controls whether it surfaces.
type SignalDelta struct {
	Type         string        `json:"type"`
	Source       string        `json:"source"`
	SignalGroup  string        `json:"signal_group"`
	Observations []Observation `json:"observations"`
	PairDiffs    []ValueDiff   `json:"pair_diffs"`
}

// HasAnyChange reports whether any pair-diff in this delta surfaces
// a change. Cheap pre-check for the renderer's unchanged-suppression
// path.
func (s SignalDelta) HasAnyChange() bool {
	for _, d := range s.PairDiffs {
		if d.HasChanges() {
			return true
		}
	}
	return false
}

// HasCategoricalChange reports whether any pair-diff in this delta
// surfaces a "categorical" change — one that signals a discrete
// state transition rather than numeric drift. The rule:
//
//   - Added or Removed top-level fields → categorical
//   - Scalar change where either endpoint is non-numeric (bool /
//     string) → categorical
//   - Array change (any add / remove / change of elements) →
//     categorical (arrays in our signal model carry meaningful
//     entries: publishers, versions, workflow_refs, maintainers)
//   - Object change recurses into the same rule
//   - Opaque change → categorical (we know something changed but
//     couldn't decompose it; structurally significant)
//
// What's NOT categorical: a Scalar change between two numeric
// endpoints (drift). Used by SortGroups to promote interesting
// transitions above noisy numeric drift in the rendered output.
func (s SignalDelta) HasCategoricalChange() bool {
	for _, d := range s.PairDiffs {
		if diffHasCategorical(d) {
			return true
		}
	}
	return false
}

func diffHasCategorical(d ValueDiff) bool {
	if len(d.Added) > 0 || len(d.Removed) > 0 {
		return true
	}
	for _, c := range d.Changed {
		switch c.Kind {
		case ChangeKindScalar:
			if !isNumericValue(c.Before) || !isNumericValue(c.After) {
				return true
			}
		case ChangeKindArray:
			return true
		case ChangeKindObject:
			if c.Nested != nil && diffHasCategorical(*c.Nested) {
				return true
			}
		case ChangeKindOpaque:
			return true
		}
	}
	return false
}

func isNumericValue(v any) bool {
	switch v.(type) {
	case float64, float32, int, int64, int32:
		return true
	}
	return false
}

// SortGroups returns a sorted copy of the input where categorical-
// change groups appear before pure-numeric-drift groups. Within
// each tier, groups are ordered by (signal_group, type, source)
// alphabetically.
//
// "Interesting stuff first" is the principle: a reader scanning
// the output wants discrete state transitions (a trusted_publisher
// disappeared, a service-account publisher appeared, a workflow
// ref changed) before they wade through stars/forks drift. The
// MCP truncation cap uses the same ordering so when groups must
// be dropped, the noisy-drift groups go first.
//
// Used by both renderers and by the MCP truncation path. Groups
// with no changes at all (HasAnyChange()==false) sort as numeric-
// tier (no promotion) — they have nothing categorical to surface.
func SortGroups(groups []SignalDelta) []SignalDelta {
	out := make([]SignalDelta, len(groups))
	copy(out, groups)
	sort.SliceStable(out, func(i, j int) bool {
		ci := out[i].HasCategoricalChange()
		cj := out[j].HasCategoricalChange()
		if ci != cj {
			return ci // true (categorical) sorts before false
		}
		if out[i].SignalGroup != out[j].SignalGroup {
			return out[i].SignalGroup < out[j].SignalGroup
		}
		if out[i].Type != out[j].Type {
			return out[i].Type < out[j].Type
		}
		return out[i].Source < out[j].Source
	})
	return out
}

// TimeWindow describes the time scope of a deltas query. Exactly
// one of (All, Last, Range, Cutoff) is meaningful at a time:
//
//   - All=true means "the full history."
//   - Last>0 means "the most recent Last observations per
//     (type, source) group."
//   - RangeStart/RangeEnd both non-zero means "observations
//     between RangeStart and RangeEnd, inclusive on both ends."
//   - Cutoff non-zero means "observations at or after Cutoff."
//
// The CLI verb enforces mutual exclusion; renderers don't need to
// re-check. Kind() returns the discriminator in stable precedence
// order (All > Last > Range > Cutoff/since).
type TimeWindow struct {
	All        bool      `json:"-"`
	Last       int       `json:"last,omitempty"`
	RangeStart time.Time `json:"-"`
	RangeEnd   time.Time `json:"-"`
	Cutoff     time.Time `json:"cutoff,omitzero"`
}

// Kind returns a short label for the window's mode, used in the
// JSON output's "window.kind" field. Values: "all", "last",
// "range", "since".
func (w TimeWindow) Kind() string {
	switch {
	case w.All:
		return "all"
	case w.Last > 0:
		return "last"
	case !w.RangeStart.IsZero() && !w.RangeEnd.IsZero():
		return "range"
	default:
		return "since"
	}
}

// RenderInput is the complete payload the renderers consume.
// Groups is sorted by (SignalGroup, Type, Source) for
// deterministic output.
type RenderInput struct {
	Target string        `json:"target"`
	Window TimeWindow    `json:"window"`
	Groups []SignalDelta `json:"groups"`
}

// MarshalJSON for TimeWindow emits the Kind() field alongside the
// active mode's value. The plain struct serialization omits the
// kind discriminator, which downstream consumers would otherwise
// have to infer from which field is populated. range_start and
// range_end appear only when the window is in "range" mode; cutoff
// only when in "since" mode; last only when in "last" mode.
func (w TimeWindow) MarshalJSON() ([]byte, error) {
	type windowJSON struct {
		Kind       string    `json:"kind"`
		Last       int       `json:"last,omitempty"`
		Cutoff     time.Time `json:"cutoff,omitzero"`
		RangeStart time.Time `json:"range_start,omitzero"`
		RangeEnd   time.Time `json:"range_end,omitzero"`
	}
	return json.Marshal(windowJSON{
		Kind:       w.Kind(),
		Last:       w.Last,
		Cutoff:     w.Cutoff,
		RangeStart: w.RangeStart,
		RangeEnd:   w.RangeEnd,
	})
}

// TextOpts carries the text-renderer's flags. Future fields:
// chronological vs group-by, ANSI color toggle, etc.
type TextOpts struct {
	// IncludeUnchanged surfaces signals that have no diffs in the
	// window. Default behavior (false) suppresses them — a target
	// with 40 signals and 2 changes produces a short output.
	IncludeUnchanged bool

	// Expand restores per-transition rendering for "drift-only"
	// groups (signals whose only changes are numeric scalar drift,
	// after clock-field suppression — typically stars/forks/
	// open_issues/followers counters). Default behavior (false)
	// collapses these into a single footer line with cumulative
	// deltas, so the meaningful categorical transitions aren't
	// buried under boilerplate.
	Expand bool

	// Color enables ANSI color/style codes in the text output.
	// Default behavior (false) emits plain text. The CLI verb
	// auto-detects whether stdout is a terminal and sets this
	// accordingly (with NO_COLOR env and --no-color flag honored).
	// Renderer-internal: bold for signal-type names, dim for
	// timestamps, yellow for the CHANGED marker.
	Color bool
}
