package repofiles

import (
	"path/filepath"
	"strings"
)

// Result is the per-family verdict surfaced to the collector.
//
// Family is intentionally tagged json:"-" because the enclosing
// signal value is a map keyed by family name — encoding the family
// both as the key and as a struct field would be redundant.
type Result struct {
	Family   string   `json:"-"`
	Present  bool     `json:"present"`
	Path     string   `json:"path,omitempty"`
	AltPaths []string `json:"alt_paths,omitempty"`
}

// Evaluate picks a canonical representative per family from a set of
// matches and returns one Result per declared family in declaration
// order. Families with no matches get Present=false; families with at
// least one match get Present=true, the ranked-canonical entry's path
// in Path, and any remaining matches in AltPaths (sorted as they
// arrived from the scanner).
//
// Ranking (see rank()): exact-case Preferred match wins; if none,
// case-insensitive Preferred match; if still none, the first entry
// in scanner-sorted order. This preserves canonical-spelling
// preference on mixed-case repos while never discarding a detection.
//
// Iteration order of the returned slice mirrors the fams argument,
// so callers that emit a map-valued signal get stable field order
// across runs.
func Evaluate(fams []Family, matches []Match) []Result {
	byFamily := make(map[string][]Match, len(fams))
	for _, m := range matches {
		byFamily[m.Family] = append(byFamily[m.Family], m)
	}

	results := make([]Result, 0, len(fams))
	for _, fam := range fams {
		ms := byFamily[fam.Name]
		if len(ms) == 0 {
			results = append(results, Result{Family: fam.Name, Present: false})
			continue
		}
		chosenIdx := rank(fam.Preferred, ms)
		chosen := ms[chosenIdx]

		var alts []string
		for i, m := range ms {
			if i == chosenIdx {
				continue
			}
			alts = append(alts, m.Path)
		}

		results = append(results, Result{
			Family:   fam.Name,
			Present:  true,
			Path:     chosen.Path,
			AltPaths: alts,
		})
	}
	return results
}

// rank returns the index of the match to promote as canonical.
//
// Three-phase selection:
//
//  1. Walk Preferred in declared order; for each, find a match whose
//     basename equals the Preferred entry exactly (case-sensitive).
//     First hit wins. This is the "README.md is THE readme" rule on
//     a repo that has both README.md and readme.txt.
//
//  2. If no exact-case hit, repeat with case-insensitive comparison.
//     Catches the common case of a repo whose canonical file is
//     lowercased (readme.md) — still ranks it above non-Preferred
//     variants (README.markdown).
//
//  3. If still no hit — every match is outside the Preferred list
//     (README.markdown alone; or a symlink that resolved to an
//     unrelated name) — return the first match. Matches are already
//     sorted by the scanner, so this is deterministic.
//
// The fall-through case matters for forward compatibility: new
// filename variants we haven't thought of still register as present
// without requiring a code change, which is the architectural goal
// of the detect-vs-rank split.
func rank(preferred []string, matches []Match) int {
	for _, pref := range preferred {
		for i, m := range matches {
			if filepath.Base(m.Path) == pref {
				return i
			}
		}
	}
	for _, pref := range preferred {
		for i, m := range matches {
			if strings.EqualFold(filepath.Base(m.Path), pref) {
				return i
			}
		}
	}
	return 0
}
