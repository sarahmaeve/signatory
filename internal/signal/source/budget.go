// Package source contains the cross-version source-evolution
// collector machinery: version-budget selection, blob streaming
// from git, matrix assembly, and anomaly detection. Language-
// specific AST analysis lives in subpackages (golang/).
//
// See design/coll7.md for the full design.
package source

import (
	"slices"
	"strconv"
	"strings"
)

// BudgetOpts controls how Select picks a version subset from
// @v/list. Zero or negative values fall back to the v0.1
// defaults: LastN=12, MajorLeaves=4, HardCap=20.
type BudgetOpts struct {
	// LastN is the count of most-recent versions to include
	// unconditionally. Default 12.
	LastN int

	// MajorLeaves is the maximum number of "highest version per
	// older major" entries to add on top of LastN. Coverage
	// protects against campaigns that introduce malice in v1.0.0
	// of an existing v0.x clean line. Default 4.
	MajorLeaves int

	// HardCap bounds the total selection size. Protects against
	// pathologically long histories. Default 20.
	HardCap int
}

// defaults applies the v0.1 defaults to any non-positive fields,
// returning a fully-populated BudgetOpts.
func (b BudgetOpts) defaults() BudgetOpts {
	if b.LastN <= 0 {
		b.LastN = 12
	}
	if b.MajorLeaves <= 0 {
		b.MajorLeaves = 4
	}
	if b.HardCap <= 0 {
		b.HardCap = 20
	}
	return b
}

// Selection is the result of a budget pass: which versions were
// chosen, which were skipped, and the knob values in effect. The
// shape mirrors the matrix value's "budget" block in JSON output
// (design/coll7.md D2).
type Selection struct {
	SelectedVersions  []string `json:"selected_versions"`
	SkippedVersions   []string `json:"skipped_versions"`
	SelectionStrategy string   `json:"selection_strategy"`
	LastN             int      `json:"last_n"`
	MajorLeaves       int      `json:"major_leaves"`
}

// SelectionStrategyV1 names the v0.1 budget algorithm. Recorded in
// the matrix's budget block so a future strategy change is
// distinguishable in stored JSON without re-deriving from data.
const SelectionStrategyV1 = "last_n_plus_major_leaves"

// Select picks up to opts.HardCap versions from the input list,
// biased toward recency plus cross-major coverage:
//
//  1. Sort all versions in descending semver order. Invalid
//     semvers (non-canonical strings) cluster at the end.
//  2. Pick the first opts.LastN as the "recent core."
//  3. Walk the remainder, adding the highest version per unique
//     major NOT already represented in the core. Add up to
//     opts.MajorLeaves of these.
//  4. Cap the total at opts.HardCap, dropping from the end (oldest
//     leaves first).
//
// SelectedVersions appears in semver-descending order. Versions
// not selected populate SkippedVersions in the same sorted order.
//
// Empty input yields a Selection with empty (non-nil) slices and
// the knob values populated from defaults.
func Select(versions []string, opts BudgetOpts) Selection {
	opts = opts.defaults()
	sel := Selection{
		SelectedVersions:  []string{},
		SkippedVersions:   []string{},
		SelectionStrategy: SelectionStrategyV1,
		LastN:             opts.LastN,
		MajorLeaves:       opts.MajorLeaves,
	}
	if len(versions) == 0 {
		return sel
	}

	sorted := sortSemverDesc(versions)

	// Step 2: recent core.
	coreCount := min(opts.LastN, len(sorted))
	selected := slices.Clone(sorted[:coreCount])
	remaining := sorted[coreCount:]

	// Step 3: leaves of majors not in the core.
	coreMajors := make(map[string]struct{}, coreCount)
	for _, v := range selected {
		if m := majorOf(v); m != "" {
			coreMajors[m] = struct{}{}
		}
	}
	leafMajors := make(map[string]struct{})
	for _, v := range remaining {
		if len(leafMajors) >= opts.MajorLeaves {
			break
		}
		m := majorOf(v)
		if m == "" {
			continue
		}
		if _, inCore := coreMajors[m]; inCore {
			continue
		}
		if _, alreadyLeaf := leafMajors[m]; alreadyLeaf {
			continue
		}
		leafMajors[m] = struct{}{}
		selected = append(selected, v)
	}

	// Step 4: hard cap.
	if len(selected) > opts.HardCap {
		selected = selected[:opts.HardCap]
	}

	// Re-sort selected: leaves were appended after the core in
	// encounter order, so the combined list isn't strictly
	// descending until we sort it.
	slices.SortFunc(selected, descSemverCmp)
	sel.SelectedVersions = selected

	// Skipped is everything in `sorted` not in `selected`,
	// preserving the sorted-by-recency order.
	selectedSet := make(map[string]struct{}, len(selected))
	for _, v := range selected {
		selectedSet[v] = struct{}{}
	}
	for _, v := range sorted {
		if _, ok := selectedSet[v]; !ok {
			sel.SkippedVersions = append(sel.SkippedVersions, v)
		}
	}
	return sel
}

// ---- Hand-rolled semver subset ----
//
// We hand-roll a tiny semver implementation rather than importing
// golang.org/x/mod/semver. The needs in this package are narrow
// (parse MAJOR.MINOR.PATCH, group by major, sort descending) and
// the dependency surface is kept minimal.
//
// Behavioral choices, documented because they deviate slightly
// from strict SemVer 2.0.0:
//
//   - Pre-release ("-...") and build metadata ("+...") are stripped
//     before parsing. They don't affect ordering in this layer.
//     Strict SemVer orders v1.0.0-alpha < v1.0.0; we treat them as
//     equal. Acceptable for budget selection because @v/list rarely
//     emits pre-release tags adjacent to canonical tags in the same
//     major.minor.patch, and the spike-detection result is
//     unaffected even when ordering between such pairs shifts.
//
//   - Pseudo-versions ("v0.0.0-20260415120000-abc123") parse with
//     MAJOR=MINOR=PATCH=0 (the date+hash suffix is stripped). They
//     compare equal to one another and to a literal "v0.0.0".
//     Pseudo-versions virtually never appear in production-tag
//     @v/list responses, so this collapse is acceptable.
//
//   - Leading zeros (e.g., "v01.0.0") are tolerated (strconv.Atoi
//     accepts them). Strict SemVer rejects them as not-canonical.
//     This is a leniency over strict; @v/list responses from
//     proxy.golang.org are always canonical so the difference does
//     not matter in practice.

// parseSemver extracts MAJOR.MINOR.PATCH integers from v. Returns
// (major, minor, patch, ok). The pre-release and build-metadata
// suffixes are tolerated but ignored.
func parseSemver(v string) (major, minor, patch int, ok bool) {
	if len(v) < 2 || v[0] != 'v' {
		return 0, 0, 0, false
	}
	s := v[1:]
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	var err error
	if major, err = strconv.Atoi(parts[0]); err != nil || major < 0 {
		return 0, 0, 0, false
	}
	if minor, err = strconv.Atoi(parts[1]); err != nil || minor < 0 {
		return 0, 0, 0, false
	}
	if patch, err = strconv.Atoi(parts[2]); err != nil || patch < 0 {
		return 0, 0, 0, false
	}
	return major, minor, patch, true
}

// majorOf returns "vN" where N is the integer major component, or
// "" if v is not a valid semver. Used as a map key for grouping
// versions by major.
func majorOf(v string) string {
	major, _, _, ok := parseSemver(v)
	if !ok {
		return ""
	}
	return "v" + strconv.Itoa(major)
}

// compareSemver returns -1 if v < w, 0 if equal, +1 if v > w.
// Comparison is by integer (MAJOR, MINOR, PATCH); pre-release and
// build metadata are ignored. Invalid semvers sort to the end of
// an ascending order; two invalid semvers fall back to
// strings.Compare for stable ordering.
func compareSemver(v, w string) int {
	vMaj, vMin, vPat, vOk := parseSemver(v)
	wMaj, wMin, wPat, wOk := parseSemver(w)
	switch {
	case !vOk && !wOk:
		return strings.Compare(v, w)
	case !vOk:
		return 1
	case !wOk:
		return -1
	}
	if d := vMaj - wMaj; d != 0 {
		return clampSign(d)
	}
	if d := vMin - wMin; d != 0 {
		return clampSign(d)
	}
	return clampSign(vPat - wPat)
}

// clampSign normalizes any non-zero int to ±1. The slices.SortFunc
// cmp contract requires the result be -1/0/+1 (some implementations
// chain compares assuming -1/0/+1 specifically, not just sign).
func clampSign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	default:
		return 0
	}
}

// sortSemverDesc returns versions sorted in descending semver
// order (most-recent first). Invalid semvers cluster at the end
// regardless of direction; among themselves they sort
// lexicographically. The original input slice is not mutated.
func sortSemverDesc(versions []string) []string {
	sorted := slices.Clone(versions)
	slices.SortFunc(sorted, descSemverCmp)
	return sorted
}

// descSemverCmp is the slices.SortFunc comparator for
// descending-semver order with invalids clustered at the end.
// Same contract as compareSemver but inverted for valid-vs-valid
// comparisons; valid-vs-invalid relationship is preserved (valid
// always sorts before invalid) so invalids don't bubble to the
// front when reverse-sorting.
func descSemverCmp(a, b string) int {
	_, _, _, aOk := parseSemver(a)
	_, _, _, bOk := parseSemver(b)
	switch {
	case aOk && !bOk:
		return -1 // valid before invalid
	case !aOk && bOk:
		return 1
	case !aOk && !bOk:
		return strings.Compare(a, b)
	}
	// Both valid: descending semver order.
	return compareSemver(b, a)
}
