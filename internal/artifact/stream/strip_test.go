package stream

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestDetectCommonTopDir_GoModuleMultiSegmentWrapper pins the
// behaviour required for Go-module zips. The Go module-proxy spec
// requires every zip entry to share the prefix
// `<module-path>@<version>/`, where module-path itself can contain
// slashes (e.g., "golang.org/x/sync@v0.20.0/"). The strip detection
// must return that ENTIRE multi-segment prefix, not the first
// slash-terminated component (which would be "golang.org/" — leaving
// "x/sync@v0.20.0/" still attached to every path and breaking the
// diff against the git tree).
//
// This is a generalisation: npm ("package/"), cargo
// ("<name>-<version>/"), and pypi ("<name>-<version>/") wrappers all
// have a single slash-terminated component, so they're a degenerate
// case of the longest-common-slash-terminated-prefix rule and remain
// unaffected.
func TestDetectCommonTopDir_GoModuleMultiSegmentWrapper(t *testing.T) {
	t.Parallel()

	entries := []Entry{
		{Path: "golang.org/x/sync@v0.20.0/go.mod", Type: EntryFile},
		{Path: "golang.org/x/sync@v0.20.0/sync.go", Type: EntryFile},
		{Path: "golang.org/x/sync@v0.20.0/errgroup/errgroup.go", Type: EntryFile},
		{Path: "golang.org/x/sync@v0.20.0/LICENSE", Type: EntryFile},
	}

	got := detectCommonTopDir(entries)
	assert.Equal(t, "golang.org/x/sync@v0.20.0/", got,
		"multi-segment wrapping prefix must be detected in full — "+
			"returning a partial prefix (e.g. 'golang.org/') leaves the "+
			"remainder attached to every path and breaks the diff")
}

// TestDetectCommonTopDir_SingleSegmentWrapperUnchanged confirms the
// generalisation doesn't regress the single-segment case used by
// npm / cargo / pypi / gem-inner.
func TestDetectCommonTopDir_SingleSegmentWrapperUnchanged(t *testing.T) {
	t.Parallel()

	entries := []Entry{
		{Path: "package/index.js", Type: EntryFile},
		{Path: "package/package.json", Type: EntryFile},
		{Path: "package/lib/util.js", Type: EntryFile},
	}

	got := detectCommonTopDir(entries)
	assert.Equal(t, "package/", got,
		"single-segment wrappers (npm-shape) must continue to detect "+
			"as before — generalisation must not regress existing ecosystems")
}

// TestDetectCommonTopDir_NoCommonPrefix returns "" when entries
// share no common slash-terminated prefix.
func TestDetectCommonTopDir_NoCommonPrefix(t *testing.T) {
	t.Parallel()

	entries := []Entry{
		{Path: "foo/bar.go", Type: EntryFile},
		{Path: "baz.go", Type: EntryFile},
	}

	got := detectCommonTopDir(entries)
	assert.Equal(t, "", got,
		"no common prefix must yield empty string")
}
