package deltas

import (
	"encoding/json"
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

// TimeWindow describes the time scope of a deltas query. Exactly
// one of (All, Last, Cutoff) is meaningful at a time:
//
//   - All=true means "the full history."
//   - Last>0 means "the most recent Last observations per
//     (type, source) group."
//   - Cutoff non-zero means "observations at or after Cutoff."
//
// The CLI verb enforces mutual exclusion; renderers don't need to
// re-check.
type TimeWindow struct {
	All    bool      `json:"-"`
	Last   int       `json:"last,omitempty"`
	Cutoff time.Time `json:"cutoff,omitzero"`
}

// Kind returns a short label for the window's mode, used in the
// JSON output's "window.kind" field. Three values: "all", "last",
// "since".
func (w TimeWindow) Kind() string {
	switch {
	case w.All:
		return "all"
	case w.Last > 0:
		return "last"
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
// have to infer from which field is populated.
func (w TimeWindow) MarshalJSON() ([]byte, error) {
	type windowJSON struct {
		Kind   string    `json:"kind"`
		Last   int       `json:"last,omitempty"`
		Cutoff time.Time `json:"cutoff,omitzero"`
	}
	return json.Marshal(windowJSON{
		Kind:   w.Kind(),
		Last:   w.Last,
		Cutoff: w.Cutoff,
	})
}

// TextOpts carries the text-renderer's flags. Future fields:
// chronological vs group-by, ANSI color toggle, etc.
type TextOpts struct {
	// IncludeUnchanged surfaces signals that have no diffs in the
	// window. Default behavior (false) suppresses them — a target
	// with 40 signals and 2 changes produces a short output.
	IncludeUnchanged bool
}
