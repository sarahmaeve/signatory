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

	// Summary line — answers "is this important?" at a glance.
	// Counts only groups with actual changes (not IncludeUnchanged
	// padding); runs are distinct CollectedAt across the rendered
	// set. The unchanged-only case gets a "No changes" line plus
	// a hint at --include-unchanged.
	changedCount, runCount := summarize(in.Groups)
	switch {
	case changedCount > 0:
		bw.printf("\n%s changed across %s\n",
			pluralize(changedCount, "signal"),
			pluralize(runCount, "run"))
	default:
		bw.printf("\nNo changes in this window. (use --include-unchanged for the full view)\n")
	}

	groups := sortedGroups(in.Groups)

	wrote := false
	for _, g := range groups {
		if !g.HasAnyChange() && !opts.IncludeUnchanged {
			continue
		}
		wrote = true
		bw.printf("\n%s (%s, %s)\n", g.Type, g.Source, g.SignalGroup)

		if !g.HasAnyChange() {
			bw.printf("  %d observation(s), no change\n", len(g.Observations))
			continue
		}

		// Render each pair-diff with the transition timestamps.
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

	// The top-level summary already owned the "no changes" message;
	// nothing further to emit when wrote==false.
	_ = wrote

	return bw.err
}

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

// sortedGroups returns a copy of the input sorted by (signal_group,
// type, source). Independent of input order; same input produces
// same output.
func sortedGroups(groups []SignalDelta) []SignalDelta {
	out := make([]SignalDelta, len(groups))
	copy(out, groups)
	sort.SliceStable(out, func(i, j int) bool {
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
