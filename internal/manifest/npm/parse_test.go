package npm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/manifest"
)

// TestIsNonRegistrySpec locks in the classification of every
// documented dep-spec prefix — if this test fails, the survey will
// either treat a local path as a registry package (trying to
// analyze it remotely) or treat a registry package as local
// (skipping its analysis entirely).
func TestIsNonRegistrySpec(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		spec string
		want bool
	}{
		// Registry specs: ranges, exact versions, dist-tags.
		{"caret range", "^4.18.2", false},
		{"tilde range", "~4.18.2", false},
		{"exact version", "4.18.2", false},
		{"any", "*", false},
		{"dist-tag latest", "latest", false},
		{"dist-tag next", "next", false},
		{"x-range", "4.x", false},

		// Non-registry: filesystem.
		{"file:", "file:../local-fork", true},
		{"file: uppercase", "FILE:../x", true},

		// Non-registry: git sources.
		{"git:", "git://github.com/foo/bar.git", true},
		{"git+https", "git+https://github.com/foo/bar.git", true},
		{"git+ssh", "git+ssh://git@github.com/foo/bar.git", true},

		// Non-registry: shortform hosts.
		{"github:", "github:foo/bar", true},
		{"gitlab:", "gitlab:foo/bar", true},
		{"bitbucket:", "bitbucket:foo/bar", true},

		// Non-registry: tarballs by URL.
		{"http tarball", "http://example.com/foo.tgz", true},
		{"https tarball", "https://example.com/foo.tgz", true},

		// Non-registry: aliases.
		{"npm: alias", "npm:lodash@^4.17.0", true},

		// Non-registry: workspace protocols.
		{"workspace:", "workspace:*", true},
		{"portal:", "portal:./sibling", true},
		{"link:", "link:../sibling", true},

		{"empty", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, isNonRegistrySpec(tc.spec))
		})
	}
}

// TestIsRootLevelLockPath verifies the flat-vs-nested distinction
// used to skip deeply-nested lockfile entries. A nested entry
// means npm couldn't dedupe; emitting both the nested and the
// root version as separate deps would confuse the survey's flat
// view.
func TestIsRootLevelLockPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		want bool
	}{
		{"node_modules/express", true},
		{"node_modules/@types/node", true},
		{"node_modules/@nestjs/core", true},
		{"node_modules/express/node_modules/body-parser", false},
		{"node_modules/@types/node/node_modules/x", false},
		{"", false},
		{"some-other-path/x", false},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, isRootLevelLockPath(tc.in))
		})
	}
}

// TestParse_Simple covers the common case: package.json with two
// direct deps plus a lockfile with a handful of transitive deps.
// Asserts ProjectInfo fields, the direct/indirect split, and
// canonical URI resolution.
func TestParse_Simple(t *testing.T) {
	t.Parallel()

	info, deps, err := Parse(filepath.Join("testdata", "simple", "package.json"))
	require.NoError(t, err)

	assert.Equal(t, "simple-project", info.Name)
	assert.Equal(t, "npm", info.Ecosystem)
	assert.Equal(t, ">=18.0.0", info.EcoVersion)
	assert.Contains(t, info.ManifestPath, "testdata/simple/package.json")

	byName := indexByName(deps)

	// Direct deps: express + lodash. Version from lockfile
	// supersedes the range spec.
	express := byName["express"]
	require.NotEmpty(t, express.Name)
	assert.Equal(t, "pkg:npm/express", express.CanonicalURI)
	assert.True(t, express.Direct)
	assert.Equal(t, "4.18.2", express.Version, "locked version supersedes ^4.18.2 range")
	assert.Equal(t, "npm", express.Ecosystem)

	lodash := byName["lodash"]
	assert.Equal(t, "pkg:npm/lodash", lodash.CanonicalURI)
	assert.True(t, lodash.Direct)
	assert.Equal(t, "4.17.21", lodash.Version)

	// Transitive deps from lockfile: accepts, negotiator.
	accepts := byName["accepts"]
	assert.Equal(t, "pkg:npm/accepts", accepts.CanonicalURI)
	assert.False(t, accepts.Direct, "accepts is transitive — not in package.json")
	assert.Equal(t, "1.3.8", accepts.Version)

	negotiator := byName["negotiator"]
	assert.False(t, negotiator.Direct)
}

// TestParse_Scoped verifies that @scope/name packages keep their
// scope prefix both in the Name field and in the canonical URI.
func TestParse_Scoped(t *testing.T) {
	t.Parallel()

	_, deps, err := Parse(filepath.Join("testdata", "scoped", "package.json"))
	require.NoError(t, err)

	byName := indexByName(deps)

	types := byName["@types/node"]
	require.NotEmpty(t, types.Name, "scoped package should appear as @types/node")
	assert.Equal(t, "pkg:npm/@types/node", types.CanonicalURI,
		"canonical URI must preserve the @scope/ prefix")
	assert.Equal(t, "npm", types.Ecosystem)

	nestjs := byName["@nestjs/core"]
	assert.Equal(t, "pkg:npm/@nestjs/core", nestjs.CanonicalURI)

	// Unscoped package alongside scoped ones.
	express := byName["express"]
	assert.Equal(t, "pkg:npm/express", express.CanonicalURI)
}

// TestParse_MixedDependencyClasses confirms that all four
// dependency classes in package.json (dependencies, dev, peer,
// optional) contribute to the Direct=true dep set.
func TestParse_MixedDependencyClasses(t *testing.T) {
	t.Parallel()

	_, deps, err := Parse(filepath.Join("testdata", "mixed", "package.json"))
	require.NoError(t, err)

	byName := indexByName(deps)

	// Each class contributes at least one representative.
	assert.Contains(t, byName, "express", "dependencies")
	assert.Contains(t, byName, "typescript", "devDependencies")
	assert.Contains(t, byName, "vitest", "devDependencies")
	assert.Contains(t, byName, "react", "peerDependencies")
	assert.Contains(t, byName, "fsevents", "optionalDependencies")

	// Every one is Direct — v0.1 flattens the four classes together.
	for _, d := range deps {
		assert.True(t, d.Direct,
			"all mixed fixture deps should be direct (no lockfile)")
	}
}

// TestParse_NoLockfile covers the case where package.json is present
// but no package-lock.json lives alongside. Only direct deps land,
// and non-registry specs (file:, npm:alias) get ecosystem="npm-local"
// with no canonical URI.
func TestParse_NoLockfile(t *testing.T) {
	t.Parallel()

	_, deps, err := Parse(filepath.Join("testdata", "no-lockfile", "package.json"))
	require.NoError(t, err)

	byName := indexByName(deps)

	// Registry dep: canonicalizes to pkg:npm/*.
	express := byName["express"]
	assert.Equal(t, "pkg:npm/express", express.CanonicalURI)
	assert.Equal(t, "npm", express.Ecosystem)
	assert.Equal(t, "^4.18.2", express.Version,
		"without a lockfile, direct deps retain their range spec")

	// file: dep: flagged as npm-local.
	localFork := byName["local-fork"]
	assert.Equal(t, "npm-local", localFork.Ecosystem,
		"file: spec should flag the dep as non-registry")
	assert.Empty(t, localFork.CanonicalURI,
		"non-registry deps must not get a canonical URI")

	// npm:alias dep: also flagged as npm-local. The alias target
	// (lodash in our case) is intentionally NOT resolved — the
	// alias name is what the project's code imports.
	alias := byName["private-alias"]
	assert.Equal(t, "npm-local", alias.Ecosystem)
	assert.Empty(t, alias.CanonicalURI)

	// All deps from package.json are direct (no lockfile means no
	// transitive resolution).
	for _, d := range deps {
		assert.True(t, d.Direct)
	}
}

// TestParse_NonexistentFile fails with a clean error naming the
// path.
func TestParse_NonexistentFile(t *testing.T) {
	t.Parallel()

	_, _, err := Parse(filepath.Join("testdata", "does-not-exist", "package.json"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does-not-exist")
}

// TestParse_Malformed covers a package.json that isn't valid JSON.
// Parser should wrap the error with context rather than panic.
func TestParse_Malformed(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	bad := filepath.Join(tmp, "package.json")
	require.NoError(t, writeFile(bad, `{not valid json`))

	_, _, err := Parse(bad)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse")
}

// TestParse_CorruptLockfile verifies that a malformed lockfile
// surfaces as a parse error rather than silently falling back to
// direct-only. Silent fallback on a corrupted lockfile would
// silently degrade signal coverage — a failure mode worth making
// loud.
func TestParse_CorruptLockfile(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	require.NoError(t, writeFile(filepath.Join(tmp, "package.json"),
		`{"name":"x","dependencies":{"express":"^4"}}`))
	require.NoError(t, writeFile(filepath.Join(tmp, "package-lock.json"),
		`{ malformed`))

	_, _, err := Parse(filepath.Join(tmp, "package.json"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "lockfile")
}

// TestParse_UnsupportedLockfileVersion covers lockfileVersion 1,
// which predates the `packages` map. We don't support it in v0.1;
// surface as an error with the version number so the operator
// knows what's wrong.
func TestParse_UnsupportedLockfileVersion(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	require.NoError(t, writeFile(filepath.Join(tmp, "package.json"),
		`{"name":"x","dependencies":{"express":"^4"}}`))
	require.NoError(t, writeFile(filepath.Join(tmp, "package-lock.json"),
		`{"name":"x","lockfileVersion":1,"dependencies":{}}`))

	_, _, err := Parse(filepath.Join(tmp, "package.json"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "lockfileVersion 1")
}

// TestParse_MalformedLockfileName_NoCanonicalURIStamped verifies
// the Security L7 fix: a lockfile packages-map key whose trimmed
// form isn't a valid npm package name (e.g., from a hostile or
// malformed lockfile) must NOT result in a bad canonical URI
// being stamped and later persisted. The dep should still appear
// in the output — operators seeing garbage in their lockfile is
// useful — but no pkg:npm/... URI should be associated with it.
func TestParse_MalformedLockfileName_NoCanonicalURIStamped(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	require.NoError(t, writeFile(filepath.Join(tmp, "package.json"),
		`{"name":"x","dependencies":{"express":"^4"}}`))
	// The lockfile key "node_modules/../../etc" parses as root-
	// level (exactly one "node_modules/" occurrence) and
	// TrimPrefix yields "../../etc" — a name no npm registry
	// would accept. The parser must refuse to stamp a URI for it.
	require.NoError(t, writeFile(filepath.Join(tmp, "package-lock.json"), `{
	  "name": "x",
	  "lockfileVersion": 3,
	  "packages": {
	    "": {"name": "x"},
	    "node_modules/express": {"version": "4.18.2"},
	    "node_modules/../../etc": {"version": "1.0.0"}
	  }
	}`))

	_, deps, err := Parse(filepath.Join(tmp, "package.json"))
	require.NoError(t, err)

	byName := indexByName(deps)

	// express: normal, gets a canonical URI.
	assert.Equal(t, "pkg:npm/express", byName["express"].CanonicalURI)

	// Malformed name: still present in the dep list (operators
	// should see the garbage), but WITHOUT a canonical URI. No
	// pkg:npm/../../etc lands anywhere downstream.
	malformed := byName["../../etc"]
	require.NotEmpty(t, malformed.Name,
		"malformed dep should still appear in the output for operator visibility")
	assert.Empty(t, malformed.CanonicalURI,
		"malformed name must not get a CanonicalURI stamped — prevents persisting pkg:npm/../../etc into the store")
	assert.Equal(t, "npm", malformed.Ecosystem)
}

// TestIsValidPackageName_Accepts locks in the grammar npm accepts
// for published names. The fixture set deliberately overlaps with
// the validator in internal/signal/registry/npm/client_test.go;
// drift between the two validators would mean the manifest parser
// could stamp URIs the registry collector would refuse, breaking
// the analyze path silently.
func TestIsValidPackageName_Accepts(t *testing.T) {
	t.Parallel()

	for _, name := range []string{
		"express",
		"lodash",
		"a",
		"vitest",
		"camelCase",
		"kebab-case",
		"snake_case",
		"dot.name",
		"@types/node",
		"@nestjs/core",
		"@angular/core",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.True(t, isValidPackageName(name), "%q should be accepted", name)
		})
	}
}

func TestIsValidPackageName_Rejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		pkg  string
	}{
		{"empty", ""},
		{"starts with dot", ".hidden"},
		{"starts with hyphen", "-leading"},
		{"contains slash unscoped", "foo/bar"},
		{"path traversal", "../../etc"},
		{"contains space", "foo bar"},
		{"contains null", "foo\x00bar"},
		{"scope missing name", "@scope/"},
		{"scope with no slash", "@scope"},
		{"too long", strings.Repeat("a", 215)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.False(t, isValidPackageName(tc.pkg), "%q should be rejected", tc.pkg)
		})
	}
}

// TestParse_NestedLockfileEntriesSkipped verifies that deeply-nested
// lockfile entries (version conflicts that npm couldn't dedupe)
// are skipped rather than emitted as separate deps. The survey's
// flat view would otherwise show the same package twice with
// different versions.
func TestParse_NestedLockfileEntriesSkipped(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	require.NoError(t, writeFile(filepath.Join(tmp, "package.json"),
		`{"name":"x","dependencies":{"express":"^4"}}`))
	require.NoError(t, writeFile(filepath.Join(tmp, "package-lock.json"), `{
	  "name": "x",
	  "lockfileVersion": 3,
	  "packages": {
	    "": {"name": "x"},
	    "node_modules/express": {"version": "4.18.2"},
	    "node_modules/lodash": {"version": "4.17.21"},
	    "node_modules/express/node_modules/lodash": {"version": "3.10.1"}
	  }
	}`))

	_, deps, err := Parse(filepath.Join(tmp, "package.json"))
	require.NoError(t, err)

	byName := indexByName(deps)
	// Only one lodash, and it's the root-level 4.17.21.
	assert.Equal(t, "4.17.21", byName["lodash"].Version,
		"nested lodash@3.10.1 should be ignored; root-level wins")
}

// ---- helpers ----

func indexByName(deps []manifest.Dep) map[string]manifest.Dep {
	out := make(map[string]manifest.Dep, len(deps))
	for _, d := range deps {
		out[d.Name] = d
	}
	return out
}

//nolint:gosec // G306: test fixture permissions; not user data
func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}
