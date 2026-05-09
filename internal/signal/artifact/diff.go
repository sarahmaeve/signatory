package artifact

import (
	"archive/tar"
	"slices"
	"strings"
)

// ClassifiedEntry is one entry in the extras-in-tarball sample,
// carrying enough context for the synthesist to reason about it
// without re-fetching the archive.
//
// Category is the result of the cheap-heuristic classifier
// (categorize.go); it is descriptive, not adjudicative — "this
// path looks like build glue" is a fact about the path, not a
// claim about intent.
type ClassifiedEntry struct {
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	Category string `json:"category"`
}

// Diff is the result of comparing a tarball's file set against a
// git tree's file set. Counts are full populations; samples are
// capped to the caller-supplied bound.
//
// The diff is INTENTIONALLY ONE-SIDED: only files present in the
// tarball but absent from the repo are surfaced. The reverse
// direction (files in repo but not in tarball) was tried in
// dogfooding and produced ~200-entry samples on every healthy npm
// package — npm's `.npmignore` / `files` field, PyPI's MANIFEST.in,
// and similar publishing filters legitimately exclude tests,
// docs, .github/, examples/, etc. Surfacing those as "extras" was
// pure noise drowning out the actual divergence signal. Removed
// 2026-05-09 after the express dogfood run.
//
// If a future attack class shipped LESS than the repo declared
// (e.g., stripping a SECURITY.md to suppress disclosure
// instructions), reintroduce a directional version as its own
// signal type rather than re-bundling here.
type Diff struct {
	ExtrasInTarballCount  int               `json:"files_extra_in_tarball"`
	ExtrasInTarballSample []ClassifiedEntry `json:"extras_in_tarball_sample"`
	Categories            map[string]int    `json:"categories"`
}

// stripCommonTopDir detects and removes a single shared top-level
// directory across every entry. This handles the universal "tarball
// wraps everything in a folder" convention: npm uses "package/",
// PyPI sdists use "<name>-<version>/", autotools dist-tarballs use
// "<project>-<version>/", GitHub release zips use "<repo>-<sha>/".
//
// The detection is "every entry shares a candidate top-level dir,"
// not "the dir is named one of these specific things." Heuristic
// stays universal across ecosystems and (more importantly) refuses
// to strip when it can't be sure: a single root-level entry
// disqualifies the whole strip, preserving a tarball that's already
// root-relative as-is.
//
// Returns the (possibly rewritten) entries and the prefix that
// was stripped (empty string when no strip happened). Callers can
// surface the prefix into the signal payload so operators reading
// the divergence row see what was normalized away.
//
// Directory-typed entries that match the prefix exactly (the
// "package/" entry itself, with no tail) are dropped — they have
// no path to rewrite to. Non-directory entries inside the prefix
// keep their tails ("package/lib/foo.js" → "lib/foo.js").
func stripCommonTopDir(entries []Entry) ([]Entry, string) {
	if len(entries) == 0 {
		return entries, ""
	}

	// Candidate prefix is the first path component of the first
	// entry that has one. An entry with no slash anywhere (i.e.,
	// already a root-level file) means there is no common top-dir
	// candidate at all.
	candidate := topComponent(entries[0].Path)
	if candidate == "" {
		return entries, ""
	}

	// Verify EVERY entry starts with the candidate. A single
	// outlier vetoes the strip — better to surface the prefix as
	// honest divergence than to silently rewrite paths and corrupt
	// a downstream comparison.
	for _, e := range entries[1:] {
		if !strings.HasPrefix(e.Path, candidate) {
			return entries, ""
		}
	}

	stripped := make([]Entry, 0, len(entries))
	for _, e := range entries {
		// Drop directory-typed entries entirely. They have no git
		// ls-tree counterpart (ls-tree -r emits only blobs), and
		// callers that consume strip output directly expect a
		// clean file-only list.
		if e.Type == tar.TypeDir {
			continue
		}
		newPath := strings.TrimPrefix(e.Path, candidate)
		if newPath == "" {
			continue
		}
		clone := e
		clone.Path = newPath
		stripped = append(stripped, clone)
	}
	return stripped, candidate
}

// topComponent returns the leading path component plus its trailing
// slash, or "" if the path has no embedded slash. We keep the slash
// on the prefix so HasPrefix in the verification loop matches at a
// directory boundary rather than a string-prefix coincidence
// ("package" wrongly matching "package-extra").
func topComponent(p string) string {
	i := strings.IndexByte(p, '/')
	if i < 0 {
		return ""
	}
	return p[:i+1]
}

// ComputeDiff builds a Diff from a tarball entry list and a git
// path list. Directory-typed tarball entries are excluded from the
// comparison: git ls-tree -r --name-only emits only blob (file)
// paths, so counting tar directory headers as "extra in tarball"
// would falsely flag every healthy project.
//
// sampleCap bounds both ExtrasInTarballSample and ExtrasInRepoSample.
// Extras are sorted by path for determinism (the same input always
// produces the same sample, regardless of underlying map iteration
// order). Order-by-size or order-by-category-priority is a
// reasonable future variant; today, lexical order is the cheapest
// thing that satisfies "deterministic across runs."
//
// The result's Categories map is always non-nil even when both
// sample slices are empty — keeps downstream JSON marshalling
// emitting `"categories": {}` rather than `"categories": null`,
// which round-trips through the synthesist more cleanly.
func ComputeDiff(entries []Entry, gitPaths []string, sampleCap int) Diff {
	// Normalize away the tarball's wrapping top-level directory
	// before set-comparison, so an npm-style "package/" prefix
	// or an autotools-style "<project>-<version>/" prefix doesn't
	// register every file as divergent. See stripCommonTopDir
	// for the detection heuristic and its safety guard.
	entries, _ = stripCommonTopDir(entries)

	gitSet := make(map[string]struct{}, len(gitPaths))
	for _, p := range gitPaths {
		gitSet[p] = struct{}{}
	}

	tarSet := make(map[string]Entry, len(entries))
	for _, e := range entries {
		// Filter to entries the comparison can meaningfully match
		// against ls-tree output. Directories and other non-file
		// types (block/char devices, fifos) are excluded: they have
		// no git-tree counterpart and surfacing them as "extras"
		// would be noise on every healthy project.
		//
		// Symlinks and hardlinks ARE included — they appear in git
		// trees with mode 120000, and a symlink in a tarball that's
		// absent from the git tree is itself a signal worth surfacing.
		switch e.Type {
		case tar.TypeReg, tar.TypeSymlink, tar.TypeLink:
			tarSet[e.Path] = e
		default:
			// Directory, device, fifo, xglobal, xheader, ...
			// Skip silently — not part of the file-set comparison.
		}
	}

	var extrasInTarball []ClassifiedEntry
	for path, e := range tarSet {
		if _, ok := gitSet[path]; ok {
			continue
		}
		extrasInTarball = append(extrasInTarball, ClassifiedEntry{
			Path:     path,
			Size:     e.Size,
			Category: classify(path),
		})
	}
	slices.SortFunc(extrasInTarball, func(a, b ClassifiedEntry) int {
		if a.Path < b.Path {
			return -1
		}
		if a.Path > b.Path {
			return 1
		}
		return 0
	})

	categories := map[string]int{}
	for _, e := range extrasInTarball {
		categories[e.Category]++
	}

	return Diff{
		ExtrasInTarballCount:  len(extrasInTarball),
		ExtrasInTarballSample: capSlice(extrasInTarball, sampleCap),
		Categories:            categories,
	}
}

// capSlice returns s truncated to at most n elements. A zero or
// negative cap returns the full slice unchanged — useful for tests
// that want to assert on the whole population without a cap dance.
//
// Returns nil (not the zero-length slice) when s itself is nil so
// the caller's "empty?" assertion sees the expected zero value.
func capSlice[T any](s []T, n int) []T {
	if s == nil {
		return nil
	}
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n]
}
