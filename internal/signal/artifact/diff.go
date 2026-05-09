package artifact

import (
	"slices"
	"strings"

	"github.com/sarahmaeve/signatory/internal/artifact/stream"
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

// ComputeDiff builds a Diff from a stream-walked archive manifest
// and a git path list. Directory-typed entries are excluded from
// the comparison: git ls-tree -r --name-only emits only blob (file)
// paths, so counting tar/zip directory headers as "extra in tarball"
// would falsely flag every healthy project.
//
// The manifest's StrippedTopDir (auto-detected by the stream walker
// — "package/" for npm, "<name>-<version>/" for cargo and autotools)
// is trimmed from each entry's path before set-comparison, so an
// npm-style "package/" prefix doesn't register every file as
// divergent. The walker exposes the prefix as metadata; ComputeDiff
// applies it here so consumers don't have to.
//
// sampleCap bounds ExtrasInTarballSample. Extras are sorted by path
// for determinism (the same input always produces the same sample,
// regardless of underlying map iteration order). Order-by-size or
// order-by-category-priority is a reasonable future variant; today,
// lexical order is the cheapest thing that satisfies "deterministic
// across runs."
//
// The result's Categories map is always non-nil even when the
// sample is empty — keeps downstream JSON marshalling emitting
// `"categories": {}` rather than `"categories": null`, which
// round-trips through the synthesist more cleanly.
func ComputeDiff(manifest *stream.Manifest, gitPaths []string, sampleCap int) Diff {
	stripPrefix := manifest.StrippedTopDir

	gitSet := make(map[string]struct{}, len(gitPaths))
	for _, p := range gitPaths {
		gitSet[p] = struct{}{}
	}

	tarSet := make(map[string]stream.Entry, len(manifest.Entries))
	for _, e := range manifest.Entries {
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
		case stream.EntryFile, stream.EntrySymlink, stream.EntryHardlink:
			// include
		default:
			// EntryDir, EntryOther, EntryUnknown — skip silently.
			continue
		}

		path := strings.TrimPrefix(e.Path, stripPrefix)
		if path == "" {
			// Entry IS the wrapping directory itself (e.g. "package/"
			// as a standalone tar header). No path to compare against
			// git; drop silently.
			continue
		}
		tarSet[path] = e
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
