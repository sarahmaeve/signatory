package deltas

import (
	"reflect"
	"sort"
)

// maxDepth bounds the recursion depth for nested object diffs.
// Signal values are generally shallow (≤ 3 levels in practice);
// 5 is defensive against pathological inputs without producing
// truncated diffs on real data.
const maxDepth = 5

// stableKeyFields are the field names that, when present on the
// first element of an array of objects, license stable-key
// alignment for array diffing. Priority order: when multiple
// candidates appear, the first match wins.
//
// Sourced from real signal-value shapes:
//
//   - "version"   — unpublished_versions, version-bundle arrays
//   - "login"     — publisher / contributor lists
//   - "tag_name"  — tag arrays (forward-looking for tag_sha_mapping)
//   - "path"      — file arrays (artifact_repo_divergence extras)
//   - "name"      — dep arrays (git_url_deps_in_latest)
var stableKeyFields = []string{"version", "login", "tag_name", "path", "name"}

// ChangeKind discriminates the shape of a Change so renderers
// can dispatch without re-inspecting Before/After.
type ChangeKind string

const (
	// ChangeKindScalar: Before and After are non-collection values
	// (string, number, bool, nil) or values of mismatched type.
	ChangeKindScalar ChangeKind = "scalar"
	// ChangeKindObject: Before and After are both maps. Nested
	// carries the recursive ValueDiff.
	ChangeKindObject ChangeKind = "object"
	// ChangeKindArray: Before and After are both arrays.
	// Elements carries per-element changes when stable-key alignment
	// or same-length positional alignment succeeded.
	ChangeKindArray ChangeKind = "array"
	// ChangeKindOpaque: collections we can't structurally align.
	// Render shows Before/After as opaque blobs with count summaries.
	ChangeKindOpaque ChangeKind = "opaque"
)

// ElementChangeKind discriminates the shape of an ElementChange.
type ElementChangeKind string

const (
	// ElementAdded: the element is in After but not Before.
	ElementAdded ElementChangeKind = "added"
	// ElementRemoved: the element is in Before but not After.
	ElementRemoved ElementChangeKind = "removed"
	// ElementChanged: the element is in both, with differing value.
	ElementChanged ElementChangeKind = "changed"
)

// ValueDiff carries the structural difference between two signal
// values. A zero ValueDiff (no Added / Removed / Changed entries)
// signals "no changes."
type ValueDiff struct {
	// Added: keys present in current, absent in prior. Values are
	// the current value verbatim.
	Added map[string]any
	// Removed: keys present in prior, absent in current. Values
	// are the prior value verbatim.
	Removed map[string]any
	// Changed: keys present in both with differing values.
	Changed map[string]Change
}

// HasChanges reports whether the diff is non-empty.
func (d ValueDiff) HasChanges() bool {
	return len(d.Added) > 0 || len(d.Removed) > 0 || len(d.Changed) > 0
}

// Change carries the before/after pair for a single changed key,
// plus a Kind discriminator so renderers can dispatch on shape.
//
// For ChangeKindObject, Nested holds the recursive ValueDiff and
// Before/After hold the raw map values.
//
// For ChangeKindArray, Elements holds the per-element changes
// (stable-key-aligned or positionally aligned).
//
// For ChangeKindScalar and ChangeKindOpaque, only Before/After
// are populated; Nested and Elements are nil.
type Change struct {
	Kind     ChangeKind
	Before   any
	After    any
	Nested   *ValueDiff
	Elements []ElementChange
}

// ElementChange describes one entry's transition in an array diff.
//
// For stable-key alignment, Key holds the field value (e.g.,
// "1.169.5" for a version) and Position is the index in the
// current array, or -1 for removed elements with no current index.
//
// For positional alignment (same-length primitive arrays), Position
// holds the index and Key is empty.
//
// For object elements that changed, Nested holds the recursive
// ValueDiff describing the field-level change within that element.
type ElementChange struct {
	Kind     ElementChangeKind
	Position int
	Key      string
	Before   any
	After    any
	Nested   *ValueDiff
}

// Diff computes the per-field difference between two signal
// values. Pure function: same input produces same output, no I/O.
//
// Both maps are treated as JSON-decoded values; numeric ambiguity
// (everything is float64 after json.Unmarshal) is handled by
// reflect.DeepEqual at each comparison point.
func Diff(prior, current map[string]any) ValueDiff {
	return diffAtDepth(prior, current, 0)
}

// diffAtDepth is the recursive workhorse. Computes Added / Removed /
// Changed for the given (prior, current) pair; recurses into nested
// objects and arrays via classifyChange. Bounded by maxDepth.
func diffAtDepth(prior, current map[string]any, depth int) ValueDiff {
	out := ValueDiff{
		Added:   map[string]any{},
		Removed: map[string]any{},
		Changed: map[string]Change{},
	}

	// Keys in current only → added.
	for k, v := range current {
		if _, ok := prior[k]; !ok {
			out.Added[k] = v
		}
	}

	// Keys in prior only → removed.
	for k, v := range prior {
		if _, ok := current[k]; !ok {
			out.Removed[k] = v
		}
	}

	// Keys in both → check for changes via classifyChange.
	for k, currentV := range current {
		priorV, ok := prior[k]
		if !ok {
			continue
		}
		if reflect.DeepEqual(priorV, currentV) {
			continue
		}
		out.Changed[k] = classifyChange(priorV, currentV, depth+1)
	}

	return out
}

// classifyChange dispatches on the (prior, current) value types to
// produce the appropriate Change shape: nested object diff for two
// objects, array diff (positional / keyed / opaque) for two arrays,
// scalar before/after for primitives or type-mismatched values.
// Beyond maxDepth, everything degrades to opaque to bound the work.
func classifyChange(prior, current any, depth int) Change {
	if depth >= maxDepth {
		return Change{Kind: ChangeKindOpaque, Before: prior, After: current}
	}

	// Both objects → recurse.
	pm, priorIsObj := prior.(map[string]any)
	cm, currentIsObj := current.(map[string]any)
	if priorIsObj && currentIsObj {
		nested := diffAtDepth(pm, cm, depth)
		return Change{
			Kind:   ChangeKindObject,
			Before: prior,
			After:  current,
			Nested: &nested,
		}
	}

	// Both arrays → array-specific diffing.
	pa, priorIsArr := prior.([]any)
	ca, currentIsArr := current.([]any)
	if priorIsArr && currentIsArr {
		return diffArrays(pa, ca, depth)
	}

	// Scalar or type-mismatched values → opaque before/after.
	return Change{Kind: ChangeKindScalar, Before: prior, After: current}
}

// diffArrays applies the array-diffing heuristics in priority order:
//
//  1. Stable-key alignment for arrays of objects with a recognized
//     key field. Best for arrays where elements have stable identity
//     (versions, logins, tags).
//  2. Same-length primitive arrays → per-position diff.
//  3. Different-length primitive arrays → set-diff (added /
//     removed elements by value-equality).
//  4. Otherwise → opaque.
//
// Elements are sorted by Position then Key for deterministic output
// across runs (Go map iteration is non-deterministic; without
// sorting, the diff output would vary between runs of the same input).
func diffArrays(prior, current []any, depth int) Change {
	if stableKey, ok := findStableKey(prior, current); ok {
		return diffArraysByKey(prior, current, stableKey, depth)
	}
	priorPrim := allPrimitives(prior)
	currentPrim := allPrimitives(current)
	if priorPrim && currentPrim {
		if len(prior) == len(current) {
			return diffArraysByPosition(prior, current)
		}
		return diffArraysAsSet(prior, current)
	}
	return Change{Kind: ChangeKindOpaque, Before: prior, After: current}
}

// findStableKey examines the first non-empty array's first element
// for a recognized stable-key field. Returns (key, true) when found,
// ("", false) otherwise. The check is non-strict: only the first
// element is inspected; mixed-shape arrays trigger fallback paths
// downstream during the actual diff.
func findStableKey(prior, current []any) (string, bool) {
	var sample map[string]any
	if len(current) > 0 {
		if m, ok := current[0].(map[string]any); ok {
			sample = m
		}
	} else if len(prior) > 0 {
		if m, ok := prior[0].(map[string]any); ok {
			sample = m
		}
	}
	if sample == nil {
		return "", false
	}
	for _, candidate := range stableKeyFields {
		if _, ok := sample[candidate]; ok {
			return candidate, true
		}
	}
	return "", false
}

// diffArraysByKey aligns two arrays-of-objects by a stable-key
// field, emitting one ElementChange per add/remove/change. Elements
// without the key field (or non-string key values) are silently
// dropped from alignment; v1 trades some completeness for predictable
// behavior on malformed inputs.
func diffArraysByKey(prior, current []any, key string, depth int) Change {
	priorByKey, _ := indexByKey(prior, key)
	currentByKey, currentPos := indexByKey(current, key)

	var elements []ElementChange

	for kval, cur := range currentByKey {
		p, ok := priorByKey[kval]
		if !ok {
			elements = append(elements, ElementChange{
				Kind:     ElementAdded,
				Key:      kval,
				Position: currentPos[kval],
				After:    cur,
			})
			continue
		}
		if reflect.DeepEqual(p, cur) {
			continue
		}
		nested := diffAtDepth(p, cur, depth)
		elements = append(elements, ElementChange{
			Kind:     ElementChanged,
			Key:      kval,
			Position: currentPos[kval],
			Before:   p,
			After:    cur,
			Nested:   &nested,
		})
	}

	for kval, p := range priorByKey {
		if _, ok := currentByKey[kval]; !ok {
			elements = append(elements, ElementChange{
				Kind:     ElementRemoved,
				Key:      kval,
				Position: -1,
				Before:   p,
			})
		}
	}

	sortElements(elements)
	return Change{
		Kind:     ChangeKindArray,
		Before:   prior,
		After:    current,
		Elements: elements,
	}
}

// indexByKey builds two maps: arr[stable-key] → map element, and
// arr[stable-key] → position. Elements whose key value isn't a
// string are skipped — stable-key alignment assumes string-typed
// identifiers (versions, logins, tag names, paths, dep names).
func indexByKey(arr []any, key string) (map[string]map[string]any, map[string]int) {
	byKey := make(map[string]map[string]any, len(arr))
	positions := make(map[string]int, len(arr))
	for i, el := range arr {
		m, ok := el.(map[string]any)
		if !ok {
			continue
		}
		kval, ok := m[key].(string)
		if !ok {
			continue
		}
		byKey[kval] = m
		positions[kval] = i
	}
	return byKey, positions
}

// diffArraysByPosition handles same-length primitive arrays. Each
// position with a differing value becomes an ElementChanged entry;
// unchanged positions are silently omitted.
func diffArraysByPosition(prior, current []any) Change {
	var elements []ElementChange
	for i := range prior {
		if reflect.DeepEqual(prior[i], current[i]) {
			continue
		}
		elements = append(elements, ElementChange{
			Kind:     ElementChanged,
			Position: i,
			Before:   prior[i],
			After:    current[i],
		})
	}
	sortElements(elements)
	return Change{
		Kind:     ChangeKindArray,
		Before:   prior,
		After:    current,
		Elements: elements,
	}
}

// diffArraysAsSet handles different-length primitive arrays via
// value-equality: each element only in current is "added"; each
// element only in prior is "removed". O(N×M); acceptable because
// signal-value arrays are small (logins, maintainer lists, tag
// samples — usually < 100 elements).
func diffArraysAsSet(prior, current []any) Change {
	var elements []ElementChange

	for i, cur := range current {
		if !containsValue(prior, cur) {
			elements = append(elements, ElementChange{
				Kind:     ElementAdded,
				Position: i,
				After:    cur,
			})
		}
	}
	for _, p := range prior {
		if !containsValue(current, p) {
			elements = append(elements, ElementChange{
				Kind:     ElementRemoved,
				Position: -1,
				Before:   p,
			})
		}
	}
	sortElements(elements)
	return Change{
		Kind:     ChangeKindArray,
		Before:   prior,
		After:    current,
		Elements: elements,
	}
}

// containsValue reports whether arr contains an element that is
// reflect.DeepEqual to target. Linear scan; arrays are small.
func containsValue(arr []any, target any) bool {
	for _, el := range arr {
		if reflect.DeepEqual(el, target) {
			return true
		}
	}
	return false
}

// allPrimitives reports whether every element of arr is a non-
// collection value. JSON decoding produces map[string]any for
// objects and []any for arrays; anything else is a primitive
// (string, float64, bool, nil).
func allPrimitives(arr []any) bool {
	for _, el := range arr {
		switch el.(type) {
		case map[string]any, []any:
			return false
		}
	}
	return true
}

// sortElements imposes a stable ordering on ElementChange slices for
// deterministic output. Sort precedence:
//
//  1. Position ascending (with -1 ordering last; "removed without
//     current position" trails the in-place changes)
//  2. Key ascending for equal positions
//
// Go's map iteration order is non-deterministic; without sorting
// here, the diff output would vary between runs of the same input.
func sortElements(elements []ElementChange) {
	sort.SliceStable(elements, func(i, j int) bool {
		pi, pj := elements[i].Position, elements[j].Position
		// -1 trails (sentinel for "removed without current position").
		if pi == -1 && pj != -1 {
			return false
		}
		if pj == -1 && pi != -1 {
			return true
		}
		if pi != pj {
			return pi < pj
		}
		return elements[i].Key < elements[j].Key
	})
}
