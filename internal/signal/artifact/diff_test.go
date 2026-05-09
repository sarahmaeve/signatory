package artifact

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/artifact/stream"
)

// TestComputeDiff_IdenticalTrees verifies the trivially-correct case:
// when the tarball's file set exactly equals the git tree's file set,
// ComputeDiff reports zero extras and an empty sample.
//
// This is the case that will hold for the overwhelming majority of
// real targets — a healthy project's release tarball IS its git tree
// at the tag, with at most a handful of generated files. Getting
// this right is what makes the artifact_repo_divergence signal
// cheap to emit ambiently rather than only when something looks
// wrong.
//
// Directory-typed entries in the tarball must be excluded from the
// comparison: git ls-tree -r --name-only emits only blob (file)
// paths, never trees. If we counted tar/zip directory headers as
// "extra in tarball," every healthy project would falsely register
// dozens of extras.
func TestComputeDiff_IdenticalTrees(t *testing.T) {
	const sampleCap = 50

	manifest := &stream.Manifest{
		Entries: []stream.Entry{
			{Path: "src/", Type: stream.EntryDir},
			{Path: "src/main.go", Size: 100, Type: stream.EntryFile},
			{Path: "src/util.go", Size: 50, Type: stream.EntryFile},
			{Path: "README.md", Size: 200, Type: stream.EntryFile},
			{Path: "tests/", Type: stream.EntryDir},
		},
	}
	gitPaths := []string{
		"README.md",
		"src/main.go",
		"src/util.go",
	}

	diff := ComputeDiff(manifest, gitPaths, sampleCap)

	assert.Equal(t, 0, diff.ExtrasInTarballCount,
		"identical file sets must yield zero extras-in-tarball; "+
			"directory headers in the tar must NOT be counted as files "+
			"missing from git (ls-tree -r emits no tree entries)")
	assert.Empty(t, diff.ExtrasInTarballSample,
		"sample slice must be empty when there are no extras")
}

// TestComputeDiff_NPMShapedTarball is the integration assertion:
// when ComputeDiff is given an npm-shaped manifest (every entry
// under "package/", and the walker's StrippedTopDir set accordingly)
// alongside a git tree with the same files (root-relative), the
// divergence is zero rather than 100%. Pins the bug fix at the
// comparison layer.
//
// The strip detection itself is the stream walker's job and is
// exercised in stream/tar_walker_test.go. Here we exercise the
// integration: ComputeDiff trims StrippedTopDir from each entry
// before set-comparison, so prefix-mismatch noise doesn't drown
// out the actual signal.
func TestComputeDiff_NPMShapedTarball(t *testing.T) {
	manifest := &stream.Manifest{
		StrippedTopDir: "package/",
		Entries: []stream.Entry{
			{Path: "package/", Type: stream.EntryDir},
			{Path: "package/LICENSE", Type: stream.EntryFile},
			{Path: "package/index.js", Type: stream.EntryFile},
			{Path: "package/lib/foo.js", Type: stream.EntryFile},
		},
	}
	gitPaths := []string{"LICENSE", "index.js", "lib/foo.js"}

	diff := ComputeDiff(manifest, gitPaths, 50)

	assert.Equal(t, 0, diff.ExtrasInTarballCount,
		"after package/ strip, identical file sets yield zero "+
			"extras-in-tarball — the regression that motivated this fix")
}

// TestComputeDiff_AutotoolsShape verifies the same on the autotools /
// PyPI sdist convention: top-level directory is "<project>-<version>/"
// rather than the npm-fixed "package/". The stream walker detects
// the prefix uniformly; ComputeDiff trims it without caring what
// the prefix is named.
func TestComputeDiff_AutotoolsShape(t *testing.T) {
	manifest := &stream.Manifest{
		StrippedTopDir: "xz-5.6.1/",
		Entries: []stream.Entry{
			{Path: "xz-5.6.1/", Type: stream.EntryDir},
			{Path: "xz-5.6.1/configure.ac", Type: stream.EntryFile},
			{Path: "xz-5.6.1/m4/build-to-host.m4", Type: stream.EntryFile},
			{Path: "xz-5.6.1/src/main.c", Type: stream.EntryFile},
		},
	}
	gitPaths := []string{"configure.ac", "src/main.c"}

	diff := ComputeDiff(manifest, gitPaths, 50)

	assert.Equal(t, 1, diff.ExtrasInTarballCount,
		"after xz-5.6.1/ strip, m4/build-to-host.m4 is the one "+
			"tarball-only entry (the canonical CVE-2024-3094 shape)")
	require.Len(t, diff.ExtrasInTarballSample, 1)
	assert.Equal(t, "m4/build-to-host.m4", diff.ExtrasInTarballSample[0].Path,
		"the canonical xz attack path must survive the strip with its "+
			"category-relevant tail intact")
}

// TestComputeDiff_ExtraInTarballClassified verifies the inverse of
// the identical-trees case: when the tarball ships files the git
// tree doesn't, the diff surfaces them with the right Category and
// updates the Categories histogram.
//
// Each fixture entry pins a category the classifier must emit:
//
//   - m4/build-to-host.m4         → build_glue   (the xz shape)
//   - configure                   → generated    (autoconf output)
//   - vendor/foo/foo.go           → vendored
//   - tests/payload.xz            → binary_in_tests
//   - ../escape.txt               → suspicious_path
//   - docs/readme.txt             → other
//
// The Categories histogram must sum to ExtrasInTarballCount and
// must include exactly the categories represented in the sample —
// no spurious zero-valued buckets.
func TestComputeDiff_ExtraInTarballClassified(t *testing.T) {
	const sampleCap = 50

	manifest := &stream.Manifest{
		Entries: []stream.Entry{
			{Path: "src/main.go", Size: 100, Type: stream.EntryFile},
			{Path: "m4/build-to-host.m4", Size: 3208, Type: stream.EntryFile},
			{Path: "configure", Size: 80000, Type: stream.EntryFile, Mode: 0o755},
			{Path: "vendor/foo/foo.go", Size: 200, Type: stream.EntryFile},
			{Path: "tests/payload.xz", Size: 4096, Type: stream.EntryFile},
			{Path: "../escape.txt", Size: 10, Type: stream.EntryFile},
			{Path: "docs/readme.txt", Size: 50, Type: stream.EntryFile},
		},
	}
	gitPaths := []string{"src/main.go"}

	diff := ComputeDiff(manifest, gitPaths, sampleCap)

	assert.Equal(t, 6, diff.ExtrasInTarballCount,
		"all six tarball-only entries must surface as extras")

	// Index sample by path so we can assert per-entry without
	// depending on the (deterministic, lexical) sort order.
	byPath := map[string]ClassifiedEntry{}
	for _, e := range diff.ExtrasInTarballSample {
		byPath[e.Path] = e
	}

	cases := map[string]string{
		"m4/build-to-host.m4": "build_glue",
		"configure":           "generated",
		"vendor/foo/foo.go":   "vendored",
		"tests/payload.xz":    "binary_in_tests",
		"../escape.txt":       "suspicious_path",
		"docs/readme.txt":     "other",
	}
	for path, wantCat := range cases {
		got, ok := byPath[path]
		if assert.True(t, ok, "extra %q must appear in sample", path) {
			assert.Equal(t, wantCat, got.Category,
				"extra %q must be categorised as %q", path, wantCat)
		}
	}

	// Histogram invariants: sum equals count, no zero-valued
	// buckets (categories present iff at least one entry uses them).
	sum := 0
	for cat, n := range diff.Categories {
		assert.Positive(t, n, "category %q in histogram must have nonzero count", cat)
		sum += n
	}
	assert.Equal(t, diff.ExtrasInTarballCount, sum,
		"category histogram must sum to extras count")
}
