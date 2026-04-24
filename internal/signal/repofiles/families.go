// Package repofiles collects presence signals for conventional
// project-hygiene files — README, SECURITY, CODEOWNERS, .mailmap,
// CHANGELOG, CONTRIBUTING, AUTHORS, MAINTAINERS, GOVERNANCE.
//
// The collector emits one compound signal of type "repo_files" whose
// value is a map keyed by family name. Absent families ship
// {"present": false}; present families ship {"present": true,
// "path": <canonical>, "alt_paths": [<others>...]}.
//
// # Detection is permissive, ranking is opinionated
//
// Detectors are case-insensitive regexes that tolerate a single
// extension suffix ([^.]+, not .+, so multi-dot names like
// README.md.bak are rejected). Ranking walks each family's Preferred
// list in declared order, first looking for an exact-case match
// against candidate basenames, then falling back to case-insensitive
// comparison. Non-Preferred matches (README.markdown, Readme-variants
// from unusual toolchains) still register as present — they become
// the chosen path when no Preferred match exists, or they surface in
// alt_paths when a Preferred match won the ranking.
//
// This split is deliberate: mechanical collection observes what's on
// disk; the provenance analyst judges meaning (stale alongside
// canonical = drift; rogue CODEOWNERS.md that GitHub ignores = setup
// error). We report; they judge.

package repofiles

import "regexp"

// Family declares one hygiene-file family — its stable key, the
// candidate directories to scan, the filename detector, and the
// preferred-canonical-name list used by the ranker.
type Family struct {
	// Name is the stable key used in the emitted signal's value map
	// (e.g. "readme", "codeowners"). Lowercase, underscore-free;
	// matches the conventional signal-value idiom.
	Name string

	// Dirs is the list of candidate directories to scan for this
	// family, relative to the clone root. "." means repo root.
	// Most families scan only the root; CODEOWNERS additionally
	// scans .github/ and docs/ per GitHub's parser rules.
	Dirs []string

	// Detector is the anchored, case-insensitive regex a candidate
	// filename must match to belong to this family.
	Detector *regexp.Regexp

	// Preferred lists canonical filenames in precedence order. The
	// first entry whose basename matches a detected file (exact case
	// preferred; case-insensitive as fallback) wins the canonical
	// slot. Non-Preferred matches are still recorded — ranking is
	// advisory, not filtering.
	Preferred []string
}

// Families returns the declared hygiene-file family list in a
// deterministic, declared order. Consumers iterate this to produce
// stable per-family output.
//
// Returns a fresh slice on each call so callers cannot mutate the
// package-level declaration.
func Families() []Family {
	out := make([]Family, len(families))
	copy(out, families)
	return out
}

// families is the canonical declaration. Order matters: it drives the
// iteration order of the emitted signal's value map and the test
// assertions that lock in coverage.
var families = []Family{
	{
		// README is the landing-page document GitHub renders on every
		// repo view. Nearly universal; absence is a meaningful cue.
		Name:      "readme",
		Dirs:      []string{"."},
		Detector:  stemWithExt("README"),
		Preferred: []string{"README.md", "README.rst", "README.txt", "README"},
	},
	{
		// SECURITY declares vulnerability-disclosure channels. GitHub
		// surfaces its contents under the repo's Security tab when
		// present at a canonical path.
		Name:      "security",
		Dirs:      []string{"."},
		Detector:  stemWithExt("SECURITY"),
		Preferred: []string{"SECURITY.md", "SECURITY.rst", "SECURITY.txt"},
	},
	{
		// CODEOWNERS gates code review. GitHub's parser requires
		// exact-case "CODEOWNERS" at one of three locations; we
		// detect case-insensitively so the analyst can flag setup
		// drift (e.g., lowercased "codeowners" that won't gate).
		Name:      "codeowners",
		Dirs:      []string{".", ".github", "docs"},
		Detector:  stemWithExt("CODEOWNERS"),
		Preferred: []string{"CODEOWNERS"},
	},
	{
		// .mailmap consolidates contributor identities. Git itself
		// only reads repo-root .mailmap; this family is scoped
		// accordingly.
		Name:      "mailmap",
		Dirs:      []string{"."},
		Detector:  exact(".mailmap"),
		Preferred: []string{".mailmap"},
	},
	{
		// CHANGELOG records version-to-version changes. The family
		// covers both the CHANGELOG.<ext> stem and the bare-CHANGES
		// convention (common in older Unix-heritage projects).
		Name:      "changelog",
		Dirs:      []string{"."},
		Detector:  changelogDetector(),
		Preferred: []string{"CHANGELOG.md", "CHANGELOG.rst", "CHANGELOG.txt", "CHANGES"},
	},
	{
		// CONTRIBUTING documents how to contribute to the project.
		Name:      "contributing",
		Dirs:      []string{"."},
		Detector:  stemWithExt("CONTRIBUTING"),
		Preferred: []string{"CONTRIBUTING.md", "CONTRIBUTING.rst"},
	},
	{
		// AUTHORS lists contributors by historical convention. Distinct
		// from CODEOWNERS (review gating) and from contributors
		// (commit-derived); represents maintainer-authored attribution.
		Name:      "authors",
		Dirs:      []string{"."},
		Detector:  stemWithExt("AUTHORS"),
		Preferred: []string{"AUTHORS.md", "AUTHORS.txt", "AUTHORS"},
	},
	{
		// MAINTAINERS declares current maintainers. Common in CNCF
		// and similar organizationally-backed projects; stronger
		// hygiene cue than AUTHORS when present because it implies
		// an explicit ongoing-responsibility statement.
		Name:      "maintainers",
		Dirs:      []string{"."},
		Detector:  stemWithExt("MAINTAINERS"),
		Preferred: []string{"MAINTAINERS.md", "MAINTAINERS.txt", "MAINTAINERS"},
	},
	{
		// GOVERNANCE documents project-level decision-making. Rare but
		// a strong hygiene cue when a project publishes one.
		Name:      "governance",
		Dirs:      []string{"."},
		Detector:  stemWithExt("GOVERNANCE"),
		Preferred: []string{"GOVERNANCE.md", "GOVERNANCE.rst"},
	},
}

// stemWithExt builds the detector for families whose filenames share
// a stem and tolerate a single extension (README, SECURITY, CHANGELOG,
// CONTRIBUTING, AUTHORS, MAINTAINERS, GOVERNANCE, CODEOWNERS). The
// extension is [^.]+ rather than .+ so multi-dot filenames (backup
// copies like README.md.bak, editor swaps like .README.md.swp) don't
// match — those aren't the hygiene file, they're artifacts.
func stemWithExt(stem string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)^` + regexp.QuoteMeta(stem) + `(\.[^.]+)?$`)
}

// exact builds the detector for families whose filename is fixed with
// no extension tolerance (.mailmap — git's parser is strict).
func exact(name string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)^` + regexp.QuoteMeta(name) + `$`)
}

// changelogDetector handles the one family with two distinct stems:
// CHANGELOG.<ext> or bare CHANGES. A single regex with alternation
// keeps the family declaration uniform with the others.
func changelogDetector() *regexp.Regexp {
	return regexp.MustCompile(`(?i)^(changelog(\.[^.]+)?|changes)$`)
}
