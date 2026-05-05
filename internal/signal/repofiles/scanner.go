package repofiles

import (
	"cmp"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Match is one detected hygiene-file path for a family.
//
// Path is rendered relative to the clone root with forward slashes
// regardless of host OS, so consumers get stable identifiers across
// macOS, Linux, and Windows dev environments. For symlinks, Path is
// the resolved target (per the v0.1 decision to record what the link
// points at, not the link itself); for regular files Path equals the
// on-disk name.
type Match struct {
	Family string
	Path   string
}

// ErrNoClone signals that Scan was called against a missing path or
// one that doesn't look like a git working-tree clone. Mirrors
// internal/signal/git.ErrNoClone so the orchestrator can surface a
// uniform "needs a clone" message for every local-clone collector.
var ErrNoClone = errors.New("repofiles scanner: no local clone at path")

// Scan walks each family's candidate directories under clonePath,
// applies the family's Detector, and returns surviving matches.
//
// Filtering rules — a candidate entry drops out of the match set when:
//
//   - the entry is a directory (families are file-shaped; a SECURITY/
//     directory is not the disclosure doc);
//   - the entry is zero-byte — treated as absent. An empty stub is
//     the cheapest possible form of fake hygiene; rejecting it costs
//     nothing and raises the bar for placeholder-drop attacks;
//   - the entry is a symlink whose target can't be resolved (broken
//     link);
//   - the entry is a symlink whose resolved target escapes the clone
//     root — suspicious and never the intent for a hygiene file.
//
// A missing candidate sub-directory (.github/, docs/) is expected for
// most repos and is silently skipped. ReadDir errors on other sub-dirs
// (EACCES, etc.) also skip — lose coverage for that sub-dir, don't
// fail the scan. A missing clone root IS fatal and surfaces as
// ErrNoClone; collection against a non-git tree is never intentional.
//
// Returned matches are sorted by (Family, Path) for deterministic
// downstream ranking.
func Scan(clonePath string, fams []Family) ([]Match, error) {
	if err := validateClone(clonePath); err != nil {
		return nil, err
	}

	// Canonicalize the clone root once. If the user passed a path
	// that traverses symlinks (e.g. /tmp → /private/tmp on macOS),
	// every EvalSymlinks result below will produce the canonical
	// prefix and filepath.Rel comparisons stay correct.
	rootCanon, err := filepath.EvalSymlinks(clonePath)
	if err != nil {
		rootCanon = clonePath
	}

	var matches []Match
	for _, fam := range fams {
		for _, dir := range fam.Dirs {
			dirAbs := filepath.Join(clonePath, dir)
			entries, err := os.ReadDir(dirAbs)
			if err != nil {
				// Missing .github/ or docs/ is the common case for
				// most repos; other ReadDir failures (permissions,
				// I/O) lose coverage for this sub-dir only.
				continue
			}
			for _, entry := range entries {
				name := entry.Name()
				if !fam.Detector.MatchString(name) {
					continue
				}
				if path, ok := resolveMatch(rootCanon, dirAbs, name); ok {
					matches = append(matches, Match{Family: fam.Name, Path: path})
				}
			}
		}
	}

	slices.SortFunc(matches, func(a, b Match) int {
		return cmp.Or(
			cmp.Compare(a.Family, b.Family),
			cmp.Compare(a.Path, b.Path),
		)
	})
	return matches, nil
}

// resolveMatch applies per-entry filtering (symlink resolve, escape
// check, zero-byte check, directory check) and returns the clone-
// root-relative, slash-separated path when the match survives.
func resolveMatch(rootCanon, dirAbs, name string) (string, bool) {
	rawAbs := filepath.Join(dirAbs, name)

	// EvalSymlinks resolves the full path (both the dir components
	// and the entry itself). Broken links return an error; we treat
	// that as absent since a dangling link is not a real hygiene file.
	resolvedAbs, err := filepath.EvalSymlinks(rawAbs)
	if err != nil {
		return "", false
	}

	// Reject paths that escape the clone root. filepath.Rel produces
	// a "../" prefix when the target is outside the base; catching
	// that before we record the Path prevents us from emitting
	// absolute host paths into the signal store.
	rel, err := filepath.Rel(rootCanon, resolvedAbs)
	if err != nil || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
		return "", false
	}

	info, err := os.Stat(resolvedAbs)
	if err != nil || info.IsDir() || info.Size() == 0 {
		return "", false
	}

	return filepath.ToSlash(rel), true
}

// validateClone confirms clonePath names a git working-tree clone.
// Cheap stat-only check for .git — matches internal/signal/git's
// validateClone behavior so both collectors agree on what counts as
// "usable clone" and one ErrNoClone message suffices for either.
func validateClone(clonePath string) error {
	if clonePath == "" {
		return ErrNoClone
	}
	info, err := os.Stat(filepath.Join(clonePath, ".git"))
	if err != nil || info == nil {
		return ErrNoClone
	}
	return nil
}
