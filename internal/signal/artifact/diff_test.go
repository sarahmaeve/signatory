package artifact

import (
	"archive/tar"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestComputeDiff_IdenticalTrees verifies the trivially-correct case:
// when the tarball's file set exactly equals the git tree's file set,
// ComputeDiff reports zero extras on both sides and an empty sample.
//
// This is the case that will hold for the overwhelming majority of
// real targets — a healthy project's release tarball IS its git tree
// at the tag, with at most a handful of generated files. Getting this
// right is what makes the artifact_repo_divergence signal cheap to
// emit ambiently rather than only when something looks wrong.
//
// Directory-typed entries in the tarball must be excluded from the
// comparison: git ls-tree -r --name-only emits only blob (file)
// paths, never trees. If we counted tar directory headers as "extra
// in tarball," every healthy project would falsely register dozens
// of extras.
func TestComputeDiff_IdenticalTrees(t *testing.T) {
	const sampleCap = 50

	// Tarball contents: three regular files plus two directory
	// entries (the typical shape produced by `tar czf` against a
	// source tree — the parent dirs get their own headers).
	entries := []Entry{
		{Path: "src/", Size: 0, Type: tar.TypeDir},
		{Path: "src/main.go", Size: 100, Type: tar.TypeReg},
		{Path: "src/util.go", Size: 50, Type: tar.TypeReg},
		{Path: "README.md", Size: 200, Type: tar.TypeReg},
		{Path: "tests/", Size: 0, Type: tar.TypeDir},
	}

	// Git tree at the corresponding commit: the same three files,
	// no directory entries (ls-tree -r doesn't emit trees).
	gitPaths := []string{
		"README.md",
		"src/main.go",
		"src/util.go",
	}

	diff := ComputeDiff(entries, gitPaths, sampleCap)

	assert.Equal(t, 0, diff.ExtrasInTarballCount,
		"identical file sets must yield zero extras-in-tarball; "+
			"directory headers in the tar must NOT be counted as files "+
			"missing from git (ls-tree -r emits no tree entries)")
	assert.Empty(t, diff.ExtrasInTarballSample,
		"sample slice must be empty when there are no extras")
}

// TestStripCommonTopDir_NPMShape verifies the npm-flavored case
// that motivated extracting this helper: every entry is under a
// "package/" top-level directory. Stripping it yields the paths
// shape that ls-tree produces from the git side, so set-difference
// produces a meaningful answer instead of prefix-mismatch noise.
//
// Discovered as a real bug during the first dogfood run against
// pkg:npm/express: every file in the tarball appeared as
// extras_in_tarball because git stored them at "lib/foo.js" while
// the tarball had them at "package/lib/foo.js". The signal payload
// was useless until this normalization landed.
func TestStripCommonTopDir_NPMShape(t *testing.T) {
	entries := []Entry{
		{Path: "package/", Type: tar.TypeDir},
		{Path: "package/LICENSE", Type: tar.TypeReg},
		{Path: "package/index.js", Type: tar.TypeReg},
		{Path: "package/lib/", Type: tar.TypeDir},
		{Path: "package/lib/foo.js", Type: tar.TypeReg},
	}
	stripped, prefix := stripCommonTopDir(entries)

	assert.Equal(t, "package/", prefix,
		"detected prefix must be reported back so the Compare layer "+
			"can record it in the signal payload — operators reading the "+
			"divergence row need to know what was stripped")

	gotPaths := make([]string, 0, len(stripped))
	for _, e := range stripped {
		gotPaths = append(gotPaths, e.Path)
	}
	assert.ElementsMatch(t,
		[]string{"LICENSE", "index.js", "lib/foo.js"},
		gotPaths,
		"after strip, file entries match the git ls-tree shape; "+
			"directory entries are dropped (they have no git counterpart)")
}

// TestStripCommonTopDir_AutotoolsShape verifies the same heuristic
// on the autotools / PyPI sdist convention: top-level directory
// is "<project>-<version>/" rather than the npm-fixed "package/".
// The detection logic is "every entry shares a top-level dir,"
// not "the dir is named 'package'."
func TestStripCommonTopDir_AutotoolsShape(t *testing.T) {
	entries := []Entry{
		{Path: "xz-5.6.1/", Type: tar.TypeDir},
		{Path: "xz-5.6.1/configure.ac", Type: tar.TypeReg},
		{Path: "xz-5.6.1/m4/build-to-host.m4", Type: tar.TypeReg},
		{Path: "xz-5.6.1/src/main.c", Type: tar.TypeReg},
	}
	stripped, prefix := stripCommonTopDir(entries)

	assert.Equal(t, "xz-5.6.1/", prefix)
	gotPaths := make([]string, 0, len(stripped))
	for _, e := range stripped {
		gotPaths = append(gotPaths, e.Path)
	}
	assert.ElementsMatch(t,
		[]string{"configure.ac", "m4/build-to-host.m4", "src/main.c"},
		gotPaths,
		"the canonical xz attack path m4/build-to-host.m4 must "+
			"survive the strip with its category-relevant tail intact")
}

// TestStripCommonTopDir_NoCommonPrefix verifies the safety guard:
// if even one entry doesn't share the prefix candidate, we strip
// nothing. This protects tarballs that are already root-relative
// (cargo .crate format ships some metadata at the root, autotools
// dist-files-of-files variations, etc.) from getting their paths
// mangled.
func TestStripCommonTopDir_NoCommonPrefix(t *testing.T) {
	entries := []Entry{
		{Path: "package/foo.js", Type: tar.TypeReg},
		{Path: "META.json", Type: tar.TypeReg}, // root-level — disqualifies the strip
	}
	stripped, prefix := stripCommonTopDir(entries)
	assert.Empty(t, prefix,
		"a stray root-level entry must veto the strip — better to "+
			"surface a 'package/' prefix as honest divergence than to "+
			"silently rewrite paths and corrupt downstream comparison")
	assert.Equal(t, entries, stripped, "entries must pass through unchanged")
}

// TestComputeDiff_NPMShapedTarball is the integration assertion:
// when ComputeDiff is given an npm-shaped tarball and a git tree
// with the same files (root-relative), the divergence is zero
// rather than 100%. Pins the bug fix at the comparison layer.
func TestComputeDiff_NPMShapedTarball(t *testing.T) {
	entries := []Entry{
		{Path: "package/", Type: tar.TypeDir},
		{Path: "package/LICENSE", Type: tar.TypeReg},
		{Path: "package/index.js", Type: tar.TypeReg},
		{Path: "package/lib/foo.js", Type: tar.TypeReg},
	}
	gitPaths := []string{"LICENSE", "index.js", "lib/foo.js"}

	diff := ComputeDiff(entries, gitPaths, 50)

	assert.Equal(t, 0, diff.ExtrasInTarballCount,
		"after package/ strip, identical file sets yield zero "+
			"extras-in-tarball — the regression that motivated this fix")
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

	entries := []Entry{
		{Path: "src/main.go", Size: 100, Type: tar.TypeReg},
		{Path: "m4/build-to-host.m4", Size: 3208, Type: tar.TypeReg},
		{Path: "configure", Size: 80000, Type: tar.TypeReg, Mode: 0o755},
		{Path: "vendor/foo/foo.go", Size: 200, Type: tar.TypeReg},
		{Path: "tests/payload.xz", Size: 4096, Type: tar.TypeReg},
		{Path: "../escape.txt", Size: 10, Type: tar.TypeReg},
		{Path: "docs/readme.txt", Size: 50, Type: tar.TypeReg},
	}
	gitPaths := []string{"src/main.go"}

	diff := ComputeDiff(entries, gitPaths, sampleCap)

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
