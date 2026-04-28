package gomod

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/manifest"
)

// TestCanonicalizeGoImportPath locks in the import-path →
// canonical-URI mapping. Every row is an input a real go.mod
// could emit or that the ecosystem has demonstrably produced.
func TestCanonicalizeGoImportPath(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		// GitHub paths: stripped of subpackages, mapped to repo: scheme.
		{"github root", "github.com/alecthomas/kong", "repo:github/alecthomas/kong"},
		{"github testify", "github.com/stretchr/testify", "repo:github/stretchr/testify"},
		{"github subpackage strips to repo", "github.com/stretchr/testify/assert", "repo:github/stretchr/testify"},
		{"github deep subpackage strips to repo", "github.com/foo/bar/baz/qux", "repo:github/foo/bar"},

		// GitHub case folding: owner/name lowercased to match the
		// canonical form documented at profile.CanonicalRepoURI.
		// Surfaced by dogfood: BurntSushi/toml in go.mod was producing
		// repo:github/BurntSushi/toml while the store entity (and all
		// 4 analyses) lived at repo:github/burntsushi/toml — survey
		// reported "unexamined" because it hit a stub.
		{"github mixed-case org", "github.com/BurntSushi/toml", "repo:github/burntsushi/toml"},
		{"github all-caps", "github.com/FOO/BAR", "repo:github/foo/bar"},
		{"github mixed-case subpackage", "github.com/BurntSushi/toml/cmd/tomlv", "repo:github/burntsushi/toml"},

		// Vanity paths: preserved verbatim under pkg:go/ scheme.
		{"vanity gopkg.in", "gopkg.in/yaml.v3", "pkg:go/gopkg.in/yaml.v3"},
		{"vanity modernc.org", "modernc.org/sqlite", "pkg:go/modernc.org/sqlite"},
		{"vanity golang.org", "golang.org/x/mod", "pkg:go/golang.org/x/mod"},
		{"vanity example.com", "example.com/widget", "pkg:go/example.com/widget"},

		// Edge cases.
		{"empty string", "", ""},
		{"github prefix with no owner", "github.com/", "pkg:go/github.com/"},
		{"github with only owner", "github.com/foo", "pkg:go/github.com/foo"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, canonicalizeGoImportPath(tc.in))
		})
	}
}

func TestIsLocalPath(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		want bool
	}{
		{"./relative", true},
		{"../sibling", true},
		{"/absolute/path", true},
		{"~/home-relative", true},
		{"github.com/foo/bar", false},
		{"example.com", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, isLocalPath(tc.in))
		})
	}
}

// TestParse_Simple covers the common case: module name, go
// version, four direct deps, two indirect. Asserts the direct/
// indirect split and that canonical URIs resolve correctly for
// both github and vanity paths.
func TestParse_Simple(t *testing.T) {
	t.Parallel()

	info, deps, err := Parse(filepath.Join("testdata", "simple.go.mod"))
	require.NoError(t, err)

	// Project info.
	assert.Equal(t, "github.com/example/simple", info.Name)
	assert.Equal(t, "go", info.Ecosystem)
	assert.Equal(t, "1.25.1", info.EcoVersion)
	assert.Contains(t, info.ManifestPath, "testdata/simple.go.mod")

	// Expect 6 deps total — 4 direct + 2 indirect.
	require.Len(t, deps, 6)

	direct := filterDirect(deps)
	indirect := filterIndirect(deps)
	assert.Len(t, direct, 4, "four direct deps")
	assert.Len(t, indirect, 2, "two indirect deps")

	// Spot-check specific entries.
	byName := map[string]manifest.Dep{}
	for _, d := range deps {
		byName[d.Name] = d
	}

	kong := byName["github.com/alecthomas/kong"]
	assert.Equal(t, "repo:github/alecthomas/kong", kong.CanonicalURI)
	assert.Equal(t, "v1.15.0", kong.Version)
	assert.True(t, kong.Direct)
	assert.Equal(t, "go", kong.Ecosystem)

	yaml := byName["gopkg.in/yaml.v3"]
	assert.Equal(t, "pkg:go/gopkg.in/yaml.v3", yaml.CanonicalURI,
		"vanity paths should use pkg:go/ scheme")
	assert.True(t, yaml.Direct)

	spew := byName["github.com/davecgh/go-spew"]
	assert.Equal(t, "repo:github/davecgh/go-spew", spew.CanonicalURI)
	assert.False(t, spew.Direct, "// indirect comment should mark dep as indirect")
}

// TestParse_Replace covers both variants of `replace`: remote
// (github.com/original/thing → github.com/fork/thing) and local
// (../local/fork). Each variant has distinct semantics in survey
// output; the parser must distinguish them up front.
func TestParse_Replace(t *testing.T) {
	t.Parallel()

	_, deps, err := Parse(filepath.Join("testdata", "with-replace.go.mod"))
	require.NoError(t, err)
	require.Len(t, deps, 2)

	byName := map[string]manifest.Dep{}
	for _, d := range deps {
		byName[d.Name] = d
	}

	// Remote replace: "github.com/original/thing" is replaced with
	// "github.com/fork/thing" v1.2.3-patched. The emitted Dep should
	// carry the FORK's name, version, and canonical URI — the store
	// lookup goes against the fork, not the original.
	remote, ok := byName["github.com/fork/thing"]
	require.True(t, ok, "remote-replaced dep should use fork's name. got: %+v", byName)
	assert.Equal(t, "repo:github/fork/thing", remote.CanonicalURI)
	assert.Equal(t, "v1.2.3-patched", remote.Version)
	assert.Equal(t, "go", remote.Ecosystem)

	// Local replace: "github.com/other/dep" is replaced with
	// "../local/fork". The dep shouldn't get a canonical URI
	// because there's no remote source to analyze. Ecosystem is
	// flagged so survey can render it specially.
	var local *manifest.Dep
	for i := range deps {
		if deps[i].Ecosystem == "go-local-replace" {
			local = &deps[i]
			break
		}
	}
	require.NotNil(t, local, "expected one local-replaced dep; got: %+v", deps)
	assert.Empty(t, local.CanonicalURI, "local-replaced deps should not have canonical URI")
	assert.Contains(t, local.Name, "local-replaced", "name should signal the replacement")
	assert.Contains(t, local.Name, "../local/fork")
}

// TestParse_NoGoDirective covers older go.mods that predate the
// `go X.Y` directive. Parser must not crash; EcoVersion is empty.
func TestParse_NoGoDirective(t *testing.T) {
	t.Parallel()

	info, deps, err := Parse(filepath.Join("testdata", "no-go-directive.go.mod"))
	require.NoError(t, err)

	assert.Equal(t, "github.com/example/no-go-directive", info.Name)
	assert.Empty(t, info.EcoVersion, "missing go directive → empty EcoVersion")
	require.Len(t, deps, 1)
	assert.Equal(t, "repo:github/stretchr/testify", deps[0].CanonicalURI)
}

// TestParse_Empty covers a manifest with module + go directive
// but zero dependencies. Parser must return no error and an
// empty deps slice (not nil — callers range over it).
func TestParse_Empty(t *testing.T) {
	t.Parallel()

	info, deps, err := Parse(filepath.Join("testdata", "empty.go.mod"))
	require.NoError(t, err)

	assert.Equal(t, "github.com/example/empty", info.Name)
	assert.Equal(t, "1.25.1", info.EcoVersion)
	assert.NotNil(t, deps, "empty should return empty slice, not nil")
	assert.Empty(t, deps)
}

// TestParse_NonexistentFile fails with a clean error naming the
// path. Exact message isn't asserted (it wraps os.ReadFile's
// error which is platform-specific) but non-nil + contains-path
// is the contract.
func TestParse_NonexistentFile(t *testing.T) {
	t.Parallel()

	_, _, err := Parse(filepath.Join("testdata", "does-not-exist.go.mod"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does-not-exist.go.mod")
}

// TestParse_Malformed covers a go.mod that isn't valid. The
// underlying modfile.Parse rejects it; our wrapper should wrap
// the error with context rather than panic.
func TestParse_Malformed(t *testing.T) {
	t.Parallel()

	// Use the test's own tmp dir to write a malformed file.
	tmp := t.TempDir()
	bad := filepath.Join(tmp, "go.mod")
	require.NoError(t, writeFile(bad, "this is not a valid go.mod\n\t!!!broken"))

	_, _, err := Parse(bad)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse")
}

// ---- helpers ----

func filterDirect(deps []manifest.Dep) []manifest.Dep {
	var out []manifest.Dep
	for _, d := range deps {
		if d.Direct {
			out = append(out, d)
		}
	}
	return out
}

func filterIndirect(deps []manifest.Dep) []manifest.Dep {
	var out []manifest.Dep
	for _, d := range deps {
		if !d.Direct {
			out = append(out, d)
		}
	}
	return out
}

// writeFile is a tiny helper for the malformed-fixture test; we
// don't want to check in an intentionally-broken file.
//
//nolint:gosec // G306: test fixture permissions; not user data
func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}

// TestParseGoModGraphOutput_HappyPath exercises the pure parser
// against representative `go mod graph` output: one root module
// (no @version on the parent), several direct edges, a few
// transitive edges. Locks in the canonical-URI conversion (github
// → repo:, others → pkg:go/) and the root-detection rule
// (parent without @version).
func TestParseGoModGraphOutput_HappyPath(t *testing.T) {
	t.Parallel()
	raw := []byte(`github.com/sarahmaeve/signatory github.com/alecthomas/kong@v1.15.0
github.com/sarahmaeve/signatory gopkg.in/yaml.v3@v3.0.1
github.com/sarahmaeve/signatory modernc.org/sqlite@v1.49.1
modernc.org/sqlite@v1.49.1 github.com/dustin/go-humanize@v1.0.1
modernc.org/sqlite@v1.49.1 modernc.org/libc@v1.55.3
github.com/alecthomas/kong@v1.15.0 github.com/pkg/errors@v0.9.1
`)

	g, err := parseGoModGraphOutput(raw)
	require.NoError(t, err)

	assert.Equal(t, "repo:github/sarahmaeve/signatory", g.RootURI,
		"root module is the parent without @version")
	require.Len(t, g.Edges, 6, "all six edges should be parsed")

	// Spot-check one edge of each canonical-URI shape: github →
	// repo:, vanity domain → pkg:go/, and a transitive edge
	// where the parent itself is non-root.
	assert.Equal(t, manifest.Edge{
		Parent: "repo:github/sarahmaeve/signatory",
		Child:  "repo:github/alecthomas/kong",
	}, g.Edges[0])
	assert.Equal(t, manifest.Edge{
		Parent: "repo:github/sarahmaeve/signatory",
		Child:  "pkg:go/gopkg.in/yaml.v3",
	}, g.Edges[1])
	assert.Equal(t, manifest.Edge{
		Parent: "pkg:go/modernc.org/sqlite",
		Child:  "repo:github/dustin/go-humanize",
	}, g.Edges[3])
}

// TestParseGoModGraphOutput_TolerantOfBlankLinesAndTrailingNewline
// covers the formatting quirks `go mod graph` may emit: trailing
// newline (always), and the rare blank-line in the middle (we
// haven't observed this in practice but it costs nothing to be
// tolerant).
func TestParseGoModGraphOutput_TolerantOfBlankLinesAndTrailingNewline(t *testing.T) {
	t.Parallel()
	raw := []byte("github.com/me/proj github.com/dep/a@v1.0.0\n\ngithub.com/me/proj github.com/dep/b@v2.0.0\n")
	g, err := parseGoModGraphOutput(raw)
	require.NoError(t, err)
	assert.Len(t, g.Edges, 2, "blank lines must not produce phantom edges")
}

// TestParseGoModGraphOutput_MalformedLineRejected asserts a line
// missing the parent/child separator is treated as a parse error
// (rather than silently dropped). Catches regressions where a
// future "be more permissive" refactor loses edges to typos.
func TestParseGoModGraphOutput_MalformedLineRejected(t *testing.T) {
	t.Parallel()
	raw := []byte("github.com/me/proj github.com/dep/a@v1.0.0\nthis-line-has-no-space\n")
	_, err := parseGoModGraphOutput(raw)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "line 2",
		"error should name the offending line so the user can find it")
}

// TestParseGoModGraphOutput_EmptyInputIsError makes empty graph
// output an explicit error so survey can route through ErrGraph-
// Unavailable rather than rendering zero buckets. A real go.mod
// with zero deps still has the root module's identity to emit
// somewhere; truly empty output usually means tooling failure.
func TestParseGoModGraphOutput_EmptyInputIsError(t *testing.T) {
	t.Parallel()
	_, err := parseGoModGraphOutput(nil)
	require.Error(t, err)
}

// TestParseGraph_SubprocessFailure_WrapsErrGraphUnavailable covers
// the production seam: when runGoModGraph returns a subprocess
// error (binary not on PATH, go.sum missing modules, sandbox
// blocking exec), ParseGraph wraps with ErrGraphUnavailable so
// callers can errors.Is-detect the "no graph available, fall
// back" condition without inspecting the underlying error.
func TestParseGraph_SubprocessFailure_WrapsErrGraphUnavailable(t *testing.T) {
	orig := runGoModGraph
	t.Cleanup(func() { runGoModGraph = orig })
	runGoModGraph = func(_ context.Context, _ string) ([]byte, error) {
		return nil, fmt.Errorf("simulated `go` not found")
	}
	_, err := ParseGraph(t.Context(), "/tmp/no-such-go.mod")
	require.Error(t, err)
	assert.ErrorIs(t, err, manifest.ErrGraphUnavailable,
		"subprocess failure must surface as ErrGraphUnavailable so survey can fall back")
}

// TestParseGraph_SubprocessHappyPath runs the production
// ParseGraph wrapper end-to-end with a stubbed subprocess. Proves
// the path from runGoModGraph through parseGoModGraphOutput to
// the returned manifest.Graph is wired correctly and that the
// root URI propagates.
func TestParseGraph_SubprocessHappyPath(t *testing.T) {
	orig := runGoModGraph
	t.Cleanup(func() { runGoModGraph = orig })
	runGoModGraph = func(_ context.Context, _ string) ([]byte, error) {
		return []byte("github.com/me/proj github.com/dep/a@v1.0.0\n"), nil
	}
	g, err := ParseGraph(t.Context(), "/tmp/fake-go.mod")
	require.NoError(t, err)
	assert.Equal(t, "repo:github/me/proj", g.RootURI)
	require.Len(t, g.Edges, 1)
}
