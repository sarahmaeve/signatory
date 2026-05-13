package deltas

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// RenderText writes a human-readable summary of the deltas to w.
// Default behavior suppresses signals with no changes; opts.IncludeUnchanged
// surfaces them with a "no change" annotation.
//
// Layout:
//
//	Deltas for <target> (<window-label>)
//
//	<type> (<source>, <signal_group>)
//	  T1 → T2  ◀ CHANGED
//	    <field>: <before> → <after>
//	    <field>: gained/lost N entries (per-element details when stable-key)
//	    <field>: added <value>
//	    <field>: removed <value>
//
// Determinism: groups are sorted by (signal_group, type, source);
// within each group, observations are assumed already sorted ascending
// by collected_at (the caller's responsibility).
func RenderText(w io.Writer, in RenderInput, opts TextOpts) error {
	bw := &bufWriter{w: w}

	// Header.
	bw.printf("Deltas for %s (%s)\n", in.Target, windowLabel(in.Window))

	if len(in.Groups) == 0 {
		bw.printf("\n  no observations in this window\n")
		return bw.err
	}

	// Filter clock-counter fields out of the text rendering. The
	// filtered slice is what everything below (sort, summary,
	// emit) operates on. The original `in.Groups` is unchanged,
	// so RenderJSON / MCP consumers still see the raw data.
	filtered := make([]SignalDelta, len(in.Groups))
	for i, g := range in.Groups {
		filtered[i] = filterClockFields(g)
	}

	// Partition changed groups into "meaningful" (any categorical
	// change after clock-field suppression) and "drift" (only
	// numeric scalar changes). Under default rendering, meaningful
	// groups render in full and drift collapses into a footer line.
	// --expand short-circuits the partition and renders everything
	// inline (no footer).
	groups := sortedGroups(filtered)
	var meaningful, drift []SignalDelta
	for _, g := range groups {
		if !g.HasAnyChange() {
			continue
		}
		if opts.Expand || g.HasCategoricalChange() {
			meaningful = append(meaningful, g)
		} else {
			drift = append(drift, g)
		}
	}

	// Summary line — answers "is this important?" at a glance.
	// When drift was bucketed, the meaningful count distinguishes
	// real change from drift; an additional parenthesized
	// "with N forge drift" annotates the bucketed set.
	_, runCount := summarize(filtered)
	totalChanged := len(meaningful) + len(drift)
	switch {
	case totalChanged == 0:
		bw.printf("\nNo changes in this window. (use --include-unchanged for the full view)\n")
	case len(drift) == 0 || opts.Expand:
		bw.printf("\n%s changed across %s\n",
			pluralize(len(meaningful), "signal"),
			pluralize(runCount, "run"))
	default:
		bw.printf("\n%s changed across %s (%s with forge drift only)\n",
			pluralize(len(meaningful), "signal"),
			pluralize(runCount, "run"),
			pluralize(len(drift), "signal"))
	}

	// Render IncludeUnchanged padding groups (no-change rows) on
	// the original sort order — these aren't in `meaningful` or
	// `drift` because they have no changes.
	if opts.IncludeUnchanged {
		for _, g := range groups {
			if g.HasAnyChange() {
				continue
			}
			bw.printf("\n%s (%s, %s)\n", g.Type, g.Source, g.SignalGroup)
			bw.printf("  %d observation(s), no change\n", len(g.Observations))
		}
	}

	// Render meaningful groups in full.
	for _, g := range meaningful {
		bw.printf("\n%s (%s, %s)\n", g.Type, g.Source, g.SignalGroup)
		for i, diff := range g.PairDiffs {
			if !diff.HasChanges() {
				continue
			}
			t1 := g.Observations[i].CollectedAt.UTC().Format(time.RFC3339)
			t2 := g.Observations[i+1].CollectedAt.UTC().Format(time.RFC3339)
			bw.printf("  %s → %s  ◀ CHANGED\n", t1, t2)
			renderValueDiffIndented(bw, diff, "    ")
		}
	}

	// Drift footer.
	if len(drift) > 0 {
		bw.printf("\nforge drift only (use --expand to see per-transition detail):\n")
		for _, g := range drift {
			for _, entry := range cumulativeNumericDrift(g) {
				bw.printf("  %s\n", entry)
			}
		}
	}

	return bw.err
}

// cumulativeNumericDrift summarizes a drift-only group as one line
// per numeric-scalar field that moved across the window. Format:
//
//	<signal_type>.<field> (+N)               // single transition
//	<signal_type>.<field> (+N over M runs)   // multi-transition
//
// The cumulative delta is first-observation-value vs last-
// observation-value for each field that has a numeric scalar
// entry in any Changed map across the group's PairDiffs. Fields
// only present in some observations are skipped (rare in our
// drift-shape signals).
func cumulativeNumericDrift(g SignalDelta) []string {
	if len(g.Observations) < 2 {
		return nil
	}
	first := g.Observations[0].Value
	last := g.Observations[len(g.Observations)-1].Value

	// Collect candidate field names: any key that appeared in any
	// PairDiff's Changed map. (Drift groups by definition have no
	// Added/Removed and no nested/array/opaque changes — all
	// changes are top-level numeric scalars.)
	candidates := map[string]struct{}{}
	for _, d := range g.PairDiffs {
		for k, c := range d.Changed {
			if c.Kind == ChangeKindScalar {
				candidates[k] = struct{}{}
			}
		}
	}

	keys := make([]string, 0, len(candidates))
	for k := range candidates {
		keys = append(keys, k)
	}
	sortStrings(keys)

	out := make([]string, 0, len(keys))
	for _, k := range keys {
		bf, bok := toFloat(first[k])
		af, aok := toFloat(last[k])
		if !bok || !aok {
			continue
		}
		delta := af - bf
		if delta == 0 {
			continue
		}
		ann := formatDelta(bf, af, delta)
		if len(g.Observations) > 2 {
			out = append(out, fmt.Sprintf("%s.%s (%s over %d runs)",
				g.Type, k, ann, len(g.Observations)))
		} else {
			out = append(out, fmt.Sprintf("%s.%s (%s)",
				g.Type, k, ann))
		}
	}
	return out
}

// formatDelta renders a numeric delta with explicit sign,
// integer-friendly when both endpoints are integer-valued.
// Mirrors numericDeltaAnnotation but returns the body only (no
// surrounding space or parens; the caller frames it).
func formatDelta(before, after, delta float64) string {
	if before == float64(int64(before)) && after == float64(int64(after)) {
		return fmt.Sprintf("%+d", int64(delta))
	}
	return fmt.Sprintf("%+g", delta)
}

// sortStrings is a tiny wrapper over sort.Strings so the drift-
// footer code doesn't need its own import; render.go already
// imports sort for sortedKeys.
func sortStrings(s []string) { sort.Strings(s) }

// summarize returns (changedSignals, distinctRuns) over the
// rendered set: a "changed signal" is a SignalDelta with at least
// one pair-diff that surfaces a change; a "run" is a distinct
// CollectedAt across all observations of all groups (the same
// notion the CLI's --all prompt uses).
func summarize(groups []SignalDelta) (changed int, runs int) {
	seen := make(map[time.Time]struct{})
	for _, g := range groups {
		if g.HasAnyChange() {
			changed++
		}
		for _, o := range g.Observations {
			seen[o.CollectedAt] = struct{}{}
		}
	}
	return changed, len(seen)
}

// pluralize returns "1 signal" or "N signals" — strict English
// rules: anything not exactly 1 gets the plural form.
func pluralize(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// renderValueDiffIndented renders a single ValueDiff under the given
// indent prefix. Order: added, removed, changed (alphabetical within
// each section for determinism).
func renderValueDiffIndented(bw *bufWriter, d ValueDiff, indent string) {
	addedKeys := sortedKeys(d.Added)
	removedKeys := sortedKeys(d.Removed)
	changedKeys := sortedChangeKeys(d.Changed)

	for _, k := range addedKeys {
		bw.printf("%s%s: added %s\n", indent, k, formatValue(d.Added[k]))
	}
	for _, k := range removedKeys {
		bw.printf("%s%s: removed %s\n", indent, k, formatValue(d.Removed[k]))
	}
	for _, k := range changedKeys {
		c := d.Changed[k]
		switch c.Kind {
		case ChangeKindScalar:
			bw.printf("%s%s: %s → %s%s\n", indent, k,
				formatValue(c.Before), formatValue(c.After),
				numericDeltaAnnotation(c.Before, c.After))
		case ChangeKindObject:
			bw.printf("%s%s: (object changed)\n", indent, k)
			if c.Nested != nil {
				renderValueDiffIndented(bw, *c.Nested, indent+"  ")
			}
		case ChangeKindArray:
			renderArrayChange(bw, k, c, indent)
		default: // ChangeKindOpaque
			bw.printf("%s%s: changed (opaque)\n", indent, k)
		}
	}
}

// renderArrayChange formats an array-shape Change. The summary line
// counts adds/removes/changes; per-element lines follow indented.
func renderArrayChange(bw *bufWriter, key string, c Change, indent string) {
	added, removed, changed := 0, 0, 0
	for _, el := range c.Elements {
		switch el.Kind {
		case ElementAdded:
			added++
		case ElementRemoved:
			removed++
		case ElementChanged:
			changed++
		}
	}

	var summary []string
	if added > 0 {
		summary = append(summary, fmt.Sprintf("gained %d", added))
	}
	if removed > 0 {
		summary = append(summary, fmt.Sprintf("lost %d", removed))
	}
	if changed > 0 {
		summary = append(summary, fmt.Sprintf("changed %d", changed))
	}
	if len(summary) == 0 {
		bw.printf("%s%s: (array changed)\n", indent, key)
		return
	}

	bw.printf("%s%s: %s entries\n", indent, key, strings.Join(summary, ", "))

	subIndent := indent + "  "
	for _, el := range c.Elements {
		switch el.Kind {
		case ElementAdded:
			label := identifyElement(el)
			bw.printf("%s+ %s\n", subIndent, label)
		case ElementRemoved:
			label := identifyElement(el)
			bw.printf("%s- %s\n", subIndent, label)
		case ElementChanged:
			label := identifyElement(el)
			bw.printf("%s~ %s\n", subIndent, label)
			if el.Nested != nil {
				renderValueDiffIndented(bw, *el.Nested, subIndent+"  ")
			}
		}
	}
}

// identifyElement produces a short label for an ElementChange:
// stable-key value when available, position fallback otherwise.
// The label is "<key>" or "position <N>", optionally followed by a
// formatted value snippet.
func identifyElement(el ElementChange) string {
	id := ""
	switch {
	case el.Key != "":
		id = el.Key
	case el.Position >= 0:
		id = fmt.Sprintf("position %d", el.Position)
	default:
		id = "(unkeyed)"
	}
	// Show value for added/removed primitives (no Nested).
	if el.Nested == nil {
		switch el.Kind {
		case ElementAdded:
			if el.After != nil {
				return fmt.Sprintf("%s: %s", id, formatValue(el.After))
			}
		case ElementRemoved:
			if el.Before != nil {
				return fmt.Sprintf("%s: %s", id, formatValue(el.Before))
			}
		case ElementChanged:
			if el.Before != nil && el.After != nil &&
				!isCollection(el.Before) && !isCollection(el.After) {
				return fmt.Sprintf("%s: %s → %s", id,
					formatValue(el.Before), formatValue(el.After))
			}
		}
	}
	return id
}

// formatValue renders a single JSON-decoded value as a short string
// suitable for inline display. Strings get quotes; numbers and
// booleans render verbatim; nil renders as "null"; collections
// render as a compact JSON encoding (rare on this path since
// scalar diffs take the fast path).
func formatValue(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case string:
		return fmt.Sprintf("%q", x)
	case bool, float64, int:
		return fmt.Sprintf("%v", x)
	}
	// Fall through: JSON-encode for compact rendering.
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

// isCollection reports whether v is a map or slice (i.e. won't
// format cleanly inline).
func isCollection(v any) bool {
	switch v.(type) {
	case map[string]any, []any:
		return true
	}
	return false
}

// windowLabel produces the header phrase describing the window.
// Four shapes: "since <RFC3339>", "last N", "range T1 to T2", or
// "full history."
func windowLabel(w TimeWindow) string {
	switch w.Kind() {
	case "all":
		return "full history"
	case "last":
		return fmt.Sprintf("last %d observation(s) per signal", w.Last)
	case "range":
		return fmt.Sprintf("range %s to %s",
			w.RangeStart.UTC().Format(time.RFC3339),
			w.RangeEnd.UTC().Format(time.RFC3339))
	default:
		return fmt.Sprintf("since %s", w.Cutoff.UTC().Format(time.RFC3339))
	}
}

// isClockField reports whether a top-level diff field name is a
// "clock counter" — a value derived from (now - some_stored_date).
// These fields tick with the wall clock between collection runs
// even when the underlying world (the stored date) hasn't moved,
// so their deltas are measurement artifacts, not signal. The
// text renderer suppresses them.
//
// Patterns (all match the exact key at one level of the value tree;
// no path-style matching for v1 — recursion is deferred until a
// nested clock-counter actually shows up in the corpus):
//
//   - "days_ago" exactly (e.g., last_publish.days_ago)
//   - any name ending in "_days_ago" (publish_days_ago,
//     commit_days_ago, last_days_ago)
//   - "age_days" exactly
//   - any name ending in "_age_days" (account_age_days,
//     repo_age_days)
//   - "divergence_days" exactly (a function of *_days_ago in
//     commit_publish_cadence_divergence; tracks the clock when
//     publishes pause, and the meaningful state is `shape`)
//
// JSON / MCP output is NOT filtered — those are structured
// consumers; suppression is human-text-only.
func isClockField(name string) bool {
	switch name {
	case "days_ago", "age_days", "divergence_days":
		return true
	}
	if strings.HasSuffix(name, "_days_ago") || strings.HasSuffix(name, "_age_days") {
		return true
	}
	return false
}

// filterClockFields returns a copy of g whose PairDiffs have had
// clock-counter fields removed at the top level. The original
// SignalDelta is not mutated. Used by RenderText to prune noise
// before the unchanged-suppression pass; JSON / MCP renderers
// don't call this.
func filterClockFields(g SignalDelta) SignalDelta {
	out := g
	out.PairDiffs = make([]ValueDiff, len(g.PairDiffs))
	for i, d := range g.PairDiffs {
		out.PairDiffs[i] = filterClockFieldsValueDiff(d)
	}
	return out
}

func filterClockFieldsValueDiff(d ValueDiff) ValueDiff {
	out := ValueDiff{
		Added:   map[string]any{},
		Removed: map[string]any{},
		Changed: map[string]Change{},
	}
	for k, v := range d.Added {
		if isClockField(k) {
			continue
		}
		out.Added[k] = v
	}
	for k, v := range d.Removed {
		if isClockField(k) {
			continue
		}
		out.Removed[k] = v
	}
	for k, c := range d.Changed {
		if isClockField(k) {
			continue
		}
		out.Changed[k] = c
	}
	return out
}

// numericDeltaAnnotation returns " (+N)" or " (-N)" when both
// before and after are numeric, and empty string otherwise. Used
// by the scalar-change renderer to make numeric drift legible at
// a glance — the reader sees magnitude and direction without
// subtracting in their head.
//
// JSON decoding lands all numbers as float64; we accept either
// float64 or int and format with minimal-precision: integer-valued
// deltas render without a decimal point ("+5"), fractional
// deltas use the smallest representation the value needs ("+1.2",
// "+0.05"). The leading sign is always explicit.
//
// Booleans and strings get empty string — the before → after form
// already conveys direction for those types.
func numericDeltaAnnotation(before, after any) string {
	bf, bok := toFloat(before)
	af, aok := toFloat(after)
	if !bok || !aok {
		return ""
	}
	delta := af - bf
	if delta == 0 {
		// Shouldn't happen for a Changed entry, but defensive.
		return ""
	}
	// Integer-valued deltas (where both endpoints are integer-
	// valued) render without a decimal point.
	if bf == float64(int64(bf)) && af == float64(int64(af)) {
		return fmt.Sprintf(" (%+d)", int64(delta))
	}
	// Float deltas: strip trailing zeros, keep meaningful precision.
	return fmt.Sprintf(" (%+g)", delta)
}

// toFloat normalizes any numeric value to float64. Returns (0,
// false) for non-numeric inputs. JSON-decoded values are typically
// float64 already; int / int64 are accepted for callers that
// construct test values directly.
func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	}
	return 0, false
}

// sortedGroups returns a copy sorted via SortGroups: categorical-
// change groups first, then (signal_group, type, source)
// alphabetical within each tier. See SortGroups in types.go.
func sortedGroups(groups []SignalDelta) []SignalDelta {
	return SortGroups(groups)
}

// sortedKeys returns the keys of m in lexical order. Maps in Go
// have non-deterministic iteration order; this gives reproducible
// rendering.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// sortedChangeKeys is the Change-typed variant of sortedKeys.
func sortedChangeKeys(m map[string]Change) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// RenderJSON writes the input as indented JSON to w. The output is
// deterministic for a given input — groups are sorted, Go's
// encoding/json sorts map keys alphabetically — so consumers like
// CI gates can rely on byte-equality across runs.
func RenderJSON(w io.Writer, in RenderInput) error {
	// Sort groups for deterministic output.
	in.Groups = sortedGroups(in.Groups)

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(in)
}

// bufWriter wraps an io.Writer with a sticky-error pattern so
// renderers can call printf many times without checking err each
// time. The first error sticks; subsequent printf calls become
// no-ops; the final err is returned from the top-level Render call.
type bufWriter struct {
	w   io.Writer
	err error
}

func (b *bufWriter) printf(format string, args ...any) {
	if b.err != nil {
		return
	}
	_, b.err = fmt.Fprintf(b.w, format, args...)
}
