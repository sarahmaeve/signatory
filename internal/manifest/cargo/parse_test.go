package cargo

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/manifest"
)

// TestParse_Simple covers the basic single-crate case: Cargo.toml
// with dependencies, dev-dependencies, and build-dependencies.
func TestParse_Simple(t *testing.T) {
	t.Parallel()

	info, deps, err := Parse(filepath.Join("testdata", "simple", "Cargo.toml"))
	require.NoError(t, err)

	assert.Equal(t, "simple-crate", info.Name)
	assert.Equal(t, "cargo", info.Ecosystem)
	assert.Equal(t, "1.56", info.EcoVersion)
	assert.True(t, filepath.IsAbs(info.ManifestPath))

	byName := indexByName(deps)

	// Direct runtime deps.
	serde := byName["serde"]
	require.NotEmpty(t, serde.Name, "serde should be in deps")
	assert.Equal(t, "pkg:cargo/serde", serde.CanonicalURI)
	assert.True(t, serde.Direct)
	assert.Equal(t, "1.0", serde.Version)
	assert.Equal(t, "cargo", serde.Ecosystem)

	tokio := byName["tokio"]
	assert.Equal(t, "pkg:cargo/tokio", tokio.CanonicalURI)
	assert.True(t, tokio.Direct)
	assert.Equal(t, "1", tokio.Version)

	// Dev dependency.
	tempfile := byName["tempfile"]
	assert.Equal(t, "pkg:cargo/tempfile", tempfile.CanonicalURI)
	assert.True(t, tempfile.Direct)

	// Build dependency.
	cc := byName["cc"]
	assert.Equal(t, "pkg:cargo/cc", cc.CanonicalURI)
	assert.True(t, cc.Direct)
}

// TestParse_Simple_WithLockfile verifies that when a Cargo.lock is
// present alongside Cargo.toml, transitive deps are also extracted
// and marked as Direct=false.
func TestParse_Simple_WithLockfile(t *testing.T) {
	t.Parallel()

	_, deps, err := Parse(filepath.Join("testdata", "simple", "Cargo.toml"))
	require.NoError(t, err)

	byName := indexByName(deps)

	// Transitive: serde_derive is pulled in by serde but not
	// declared in the manifest.
	sd := byName["serde_derive"]
	assert.Equal(t, "pkg:cargo/serde_derive", sd.CanonicalURI)
	assert.False(t, sd.Direct, "serde_derive is transitive")
	assert.Equal(t, "1.0.219", sd.Version)

	// proc-macro2 is a deep transitive.
	pm2 := byName["proc-macro2"]
	assert.False(t, pm2.Direct)

	// pin-project-lite comes through tokio.
	ppl := byName["pin-project-lite"]
	assert.False(t, ppl.Direct)
}

// TestParse_PlatformConditionalDeps verifies that platform-specific
// deps from [target.'cfg(...)'.dependencies] tables are included
// as direct deps regardless of the build target.
func TestParse_PlatformConditionalDeps(t *testing.T) {
	t.Parallel()

	_, deps, err := Parse(filepath.Join("testdata", "platform-deps", "Cargo.toml"))
	require.NoError(t, err)

	byName := indexByName(deps)

	// Regular deps.
	assert.Contains(t, byName, "log")
	assert.Contains(t, byName, "clap")
	assert.True(t, byName["log"].Direct)

	// Dev dep.
	assert.Contains(t, byName, "assert_cmd")
	assert.True(t, byName["assert_cmd"].Direct)

	// Build dep.
	assert.Contains(t, byName, "cc")
	assert.True(t, byName["cc"].Direct)

	// Platform-conditional deps — all should be present and direct.
	assert.Contains(t, byName, "nix", "unix dep should be included")
	assert.True(t, byName["nix"].Direct)

	assert.Contains(t, byName, "windows-sys", "windows dep should be included")
	assert.True(t, byName["windows-sys"].Direct)

	assert.Contains(t, byName, "core-foundation", "macos dep should be included")
	assert.True(t, byName["core-foundation"].Direct)
}

// TestParse_Workspace verifies that workspace manifests enumerate
// deps from all members as a flat union.
func TestParse_Workspace(t *testing.T) {
	t.Parallel()

	info, deps, err := Parse(filepath.Join("testdata", "workspace", "Cargo.toml"))
	require.NoError(t, err)

	// Workspace root has no [package].name — ProjectInfo.Name comes
	// from the first member or stays empty. Either is acceptable;
	// the key assertion is that deps from BOTH members appear.
	assert.Equal(t, "cargo", info.Ecosystem)
	assert.Equal(t, "1.61", info.EcoVersion, "rust-version from workspace.package")

	byName := indexByName(deps)

	// From serde/Cargo.toml [dependencies].
	assert.Contains(t, byName, "serde_derive")
	assert.True(t, byName["serde_derive"].Direct)

	// From serde/Cargo.toml [dev-dependencies].
	assert.Contains(t, byName, "serde_test")
	assert.True(t, byName["serde_test"].Direct)

	// From serde_derive/Cargo.toml [dependencies] via workspace inheritance.
	assert.Contains(t, byName, "proc-macro2")
	assert.True(t, byName["proc-macro2"].Direct)

	assert.Contains(t, byName, "quote")
	assert.True(t, byName["quote"].Direct)

	assert.Contains(t, byName, "syn")
	assert.True(t, byName["syn"].Direct)
}

// TestParse_NonRegistryDeps verifies that path and git deps are
// classified with ecosystem="cargo-local" and empty CanonicalURI.
func TestParse_NonRegistryDeps(t *testing.T) {
	t.Parallel()

	path := writeCargoToml(t, `[package]
name = "has-locals"
version = "0.1.0"

[dependencies]
local-lib = { path = "../local-lib" }
forked-thing = { git = "https://github.com/me/forked-thing" }
registry-dep = "1.0"
`)
	_, deps, err := Parse(path)
	require.NoError(t, err)

	byName := indexByName(deps)

	local := byName["local-lib"]
	assert.Equal(t, "cargo-local", local.Ecosystem)
	assert.Empty(t, local.CanonicalURI)
	assert.True(t, local.Direct)

	git := byName["forked-thing"]
	assert.Equal(t, "cargo-local", git.Ecosystem)
	assert.Empty(t, git.CanonicalURI)
	assert.True(t, git.Direct)

	reg := byName["registry-dep"]
	assert.Equal(t, "cargo", reg.Ecosystem)
	assert.Equal(t, "pkg:cargo/registry-dep", reg.CanonicalURI)
	assert.True(t, reg.Direct)
}

// TestParse_EmptyPath errors cleanly.
func TestParse_EmptyPath(t *testing.T) {
	t.Parallel()
	_, _, err := Parse("")
	require.Error(t, err)
}

// TestParse_NonexistentFile errors cleanly.
func TestParse_NonexistentFile(t *testing.T) {
	t.Parallel()
	_, _, err := Parse("/nonexistent/Cargo.toml")
	require.Error(t, err)
}

// TestParseGraph_Simple verifies that Cargo.lock produces a graph
// with parent-child edges.
func TestParseGraph_Simple(t *testing.T) {
	t.Parallel()

	g, err := ParseGraph(filepath.Join("testdata", "simple", "Cargo.lock"))
	require.NoError(t, err)

	assert.Equal(t, "pkg:cargo/simple-crate", g.RootURI)
	assert.NotEmpty(t, g.Edges)

	// simple-crate → serde should be an edge.
	found := false
	for _, e := range g.Edges {
		if e.Parent == "pkg:cargo/simple-crate" && e.Child == "pkg:cargo/serde" {
			found = true
			break
		}
	}
	assert.True(t, found, "expected edge simple-crate → serde")

	// serde → serde_derive should be an edge.
	found = false
	for _, e := range g.Edges {
		if e.Parent == "pkg:cargo/serde" && e.Child == "pkg:cargo/serde_derive" {
			found = true
			break
		}
	}
	assert.True(t, found, "expected edge serde → serde_derive")
}

// TestParseGraph_NonexistentFile returns ErrGraphUnavailable.
func TestParseGraph_NonexistentFile(t *testing.T) {
	t.Parallel()

	_, err := ParseGraph("/nonexistent/Cargo.lock")
	require.Error(t, err)
	assert.ErrorIs(t, err, manifest.ErrGraphUnavailable)
}

// --- helpers ---

func writeCargoToml(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "Cargo.toml")
	require.NoError(t, writeFile(p, content))
	return p
}

func indexByName(deps []manifest.Dep) map[string]manifest.Dep {
	m := make(map[string]manifest.Dep, len(deps))
	for _, d := range deps {
		m[d.Name] = d
	}
	return m
}
