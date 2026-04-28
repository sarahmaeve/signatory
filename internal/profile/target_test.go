package profile

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveTarget_GitHubForms(t *testing.T) {
	t.Parallel()

	// Every one of these should resolve to the SAME canonical URI
	// and short name. The test is intentionally repetitive because
	// the v0.1 dogfood surfaced real-world CLI friction from
	// commands accepting only a subset.
	const (
		wantURI   = "repo:github/alecthomas/kong"
		wantName  = "kong"
		wantOwner = "alecthomas"
		wantClone = "https://github.com/alecthomas/kong"
	)

	inputs := []string{
		"alecthomas/kong",
		"github.com/alecthomas/kong",
		"https://github.com/alecthomas/kong",
		"https://github.com/alecthomas/kong.git",
		"http://github.com/alecthomas/kong",
		"git@github.com:alecthomas/kong.git",
		"repo:github/alecthomas/kong",
	}

	for _, in := range inputs {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			got, err := ResolveTarget(in)
			require.NoError(t, err, "ResolveTarget(%q) should succeed", in)
			assert.Equal(t, wantURI, got.CanonicalURI)
			assert.Equal(t, wantName, got.ShortName)
			assert.Equal(t, "repo", got.Scheme)
			assert.Equal(t, "github", got.Platform)
			assert.Equal(t, wantOwner, got.Owner)
			assert.Equal(t, wantClone, got.CloneURL)
		})
	}
}

// TestResolveTarget_NpmjsURLs covers the copy-paste-from-browser
// convenience: npmjs.com package URLs should resolve to pkg:npm/
// canonical URIs. Tests the six accepted shapes (with/without www,
// http/https, scoped, version page, query/fragment) and the
// lookalike-host rejection.
func TestResolveTarget_NpmjsURLs(t *testing.T) {
	t.Parallel()

	accepted := []struct {
		in      string
		wantURI string
	}{
		{"https://www.npmjs.com/package/express", "pkg:npm/express"},
		{"https://npmjs.com/package/express", "pkg:npm/express"},
		{"http://www.npmjs.com/package/express", "pkg:npm/express"},
		{"https://www.npmjs.com/package/msgpack-lite", "pkg:npm/msgpack-lite"},
		{"https://www.npmjs.com/package/@types/node", "pkg:npm/@types/node"},
		{"https://www.npmjs.com/package/@nestjs/core", "pkg:npm/@nestjs/core"},
		// Version pages: the UI adds /v/<version>; preserve as @V
		// suffix on the canonical URI so versioned identities survive
		// the copy-paste-from-browser workflow.
		{"https://www.npmjs.com/package/express/v/4.18.2", "pkg:npm/express@4.18.2"},
		{"https://www.npmjs.com/package/@types/node/v/20.0.0", "pkg:npm/@types/node@20.0.0"},
		// Query strings + fragments are UI state; strip.
		{"https://www.npmjs.com/package/express?activeTab=versions", "pkg:npm/express"},
		{"https://www.npmjs.com/package/express#readme", "pkg:npm/express"},
		// Trailing slash on the name: drop.
		{"https://www.npmjs.com/package/express/", "pkg:npm/express"},
	}
	for _, tc := range accepted {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got, err := ResolveTarget(tc.in)
			require.NoError(t, err, "ResolveTarget(%q)", tc.in)
			assert.Equal(t, tc.wantURI, got.CanonicalURI)
			assert.Equal(t, "pkg", got.Scheme)
		})
	}

	rejected := []struct {
		name string
		in   string
	}{
		// Lookalike host: must not be accepted as a "npmjs.com URL."
		// Host-anchoring rejects because "npmjs.com.attacker.com/"
		// doesn't match "npmjs.com/" exactly.
		{"lookalike host", "https://www.npmjs.com.attacker.com/package/x"},
		{"lookalike host no www", "https://npmjs.com.attacker.com/package/x"},
		// Wrong path prefix (not /package/).
		{"settings path", "https://www.npmjs.com/settings/profile"},
		{"root", "https://www.npmjs.com/"},
		// /package/ with no name.
		{"package no name", "https://www.npmjs.com/package/"},
		{"package empty scope", "https://www.npmjs.com/package/@/name"},
		// Other hosts entirely. pypi.org moved to its own accept/
		// reject coverage in TestResolveTarget_PyPIURLs once the
		// parsePyPIURL branch landed; crates.io stays rejected
		// until a crates provider ships.
		{"crates url", "https://crates.io/crates/atuin"},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := ResolveTarget(tc.in)
			require.Error(t, err, "ResolveTarget(%q) must reject", tc.in)
		})
	}
}

func TestResolveTarget_PkgURI(t *testing.T) {
	t.Parallel()

	got, err := ResolveTarget("pkg:cargo/atuin")
	require.NoError(t, err)
	assert.Equal(t, "pkg:cargo/atuin", got.CanonicalURI)
	assert.Equal(t, "atuin", got.ShortName)
	assert.Equal(t, "pkg", got.Scheme)
	assert.Empty(t, got.Platform)
	assert.Empty(t, got.CloneURL, "pkg: URIs have no clone URL")
	assert.Empty(t, got.Version, "unversioned pkg URI must have empty Version")
}

// TestResolveTarget_VersionedPkgURI covers the @version suffix grammar
// introduced by agent-facing-contract.md M1. Versioned pkg URIs are
// distinct identities from their unversioned counterparts; the URI
// preserves the @V, and Version surfaces it explicitly so commands
// don't have to re-parse.
func TestResolveTarget_VersionedPkgURI(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in          string
		wantURI     string
		wantName    string
		wantVersion string
	}{
		{"pkg:npm/express@4.18.2", "pkg:npm/express@4.18.2", "express", "4.18.2"},
		{"pkg:npm/@types/node@20.0.0", "pkg:npm/@types/node@20.0.0", "node", "20.0.0"},
		{"pkg:cargo/atuin@0.1.0", "pkg:cargo/atuin@0.1.0", "atuin", "0.1.0"},
		{"pkg:go/golang.org/x/mod@v0.35.0", "pkg:go/golang.org/x/mod@v0.35.0", "mod", "v0.35.0"},
		// Pre-release and build metadata passes through verbatim —
		// the grammar accepts whatever the ecosystem considers a
		// version string, not just strict semver.
		{"pkg:npm/foo@1.0.0-alpha.1", "pkg:npm/foo@1.0.0-alpha.1", "foo", "1.0.0-alpha.1"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got, err := ResolveTarget(tc.in)
			require.NoError(t, err, "ResolveTarget(%q)", tc.in)
			assert.Equal(t, tc.wantURI, got.CanonicalURI, "canonical URI must preserve @version")
			assert.Equal(t, tc.wantName, got.ShortName, "ShortName must strip the @version suffix")
			assert.Equal(t, tc.wantVersion, got.Version, "Version must be extracted")
		})
	}
}

// TestResolveTarget_VersionedPkgURI_Rejects covers the malformed shapes
// the parser should reject with a specific error.
func TestResolveTarget_VersionedPkgURI_Rejects(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		in     string
		errSub string
	}{
		{"trailing at", "pkg:npm/express@", "empty version"},
		{"double at", "pkg:npm/express@1.0@extra", "nested separators"},
		{"leading at in last segment", "pkg:npm/@1.0.0", "empty name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := ResolveTarget(tc.in)
			require.Error(t, err, "ResolveTarget(%q) must reject", tc.in)
			assert.Contains(t, err.Error(), tc.errSub)
		})
	}
}

// TestResolveTarget_NpmjsURL_VersionedPage verifies that npmjs.com
// version pages resolve to versioned canonical URIs — the user's
// "copy URL from my browser" workflow preserves version intent.
func TestResolveTarget_NpmjsURL_VersionedPage(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in          string
		wantURI     string
		wantVersion string
	}{
		{"https://www.npmjs.com/package/invariant/v/2.2.4", "pkg:npm/invariant@2.2.4", "2.2.4"},
		{"https://npmjs.com/package/express/v/4.18.2", "pkg:npm/express@4.18.2", "4.18.2"},
		{"https://www.npmjs.com/package/@types/node/v/20.0.0", "pkg:npm/@types/node@20.0.0", "20.0.0"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got, err := ResolveTarget(tc.in)
			require.NoError(t, err, "ResolveTarget(%q)", tc.in)
			assert.Equal(t, tc.wantURI, got.CanonicalURI)
			assert.Equal(t, tc.wantVersion, got.Version)
		})
	}
}

// TestResolveTarget_PyPIURLs covers the pypi.org URL acceptance
// shape. Structurally parallels TestResolveTarget_NpmjsURLs, with
// two PyPI-specific twists:
//
//  1. PEP 503 name normalization is applied before URI emission,
//     so `/project/Requests/` and `/project/requests/` produce the
//     same canonical URI. This is an asymmetry with npm (where
//     case is preserved) that reflects the ecosystems' native
//     identity semantics.
//  2. Versions follow the name directly in the path
//     (`/project/<name>/<version>/`) rather than via a `/v/`
//     segment like npmjs.com. The URL grammar is simpler here.
func TestResolveTarget_PyPIURLs(t *testing.T) {
	t.Parallel()

	accepted := []struct {
		in      string
		wantURI string
	}{
		// Basic shape — with/without trailing slash.
		{"https://pypi.org/project/requests/", "pkg:pypi/requests"},
		{"https://pypi.org/project/requests", "pkg:pypi/requests"},
		// With www. prefix.
		{"https://www.pypi.org/project/requests/", "pkg:pypi/requests"},
		// http:// rather than https://.
		{"http://pypi.org/project/requests/", "pkg:pypi/requests"},
		// PEP 503 normalization: case fold.
		{"https://pypi.org/project/Requests/", "pkg:pypi/requests"},
		{"https://pypi.org/project/REQUESTS/", "pkg:pypi/requests"},
		// PEP 503 normalization: separator conversion.
		{"https://pypi.org/project/python_dotenv/", "pkg:pypi/python-dotenv"},
		{"https://pypi.org/project/python.dotenv/", "pkg:pypi/python-dotenv"},
		{"https://pypi.org/project/python-dotenv/", "pkg:pypi/python-dotenv"},
		// Versioned page — version follows name directly.
		{"https://pypi.org/project/requests/2.31.0/", "pkg:pypi/requests@2.31.0"},
		{"https://pypi.org/project/requests/2.31.0", "pkg:pypi/requests@2.31.0"},
		// Normalization + version combined.
		{"https://pypi.org/project/Requests/2.31.0/", "pkg:pypi/requests@2.31.0"},
		{"https://pypi.org/project/python_dotenv/1.0.0/", "pkg:pypi/python-dotenv@1.0.0"},
		// Query strings + fragments are UI state; strip.
		{"https://pypi.org/project/requests/?activeTab=history", "pkg:pypi/requests"},
		{"https://pypi.org/project/requests/#history", "pkg:pypi/requests"},
		// PEP 440 version shapes pass through verbatim — the grammar
		// accepts whatever the ecosystem considers a version string.
		{"https://pypi.org/project/requests/2.31.0rc1/", "pkg:pypi/requests@2.31.0rc1"},
		{"https://pypi.org/project/requests/1.0.0.post1/", "pkg:pypi/requests@1.0.0.post1"},
	}
	for _, tc := range accepted {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got, err := ResolveTarget(tc.in)
			require.NoError(t, err, "ResolveTarget(%q)", tc.in)
			assert.Equal(t, tc.wantURI, got.CanonicalURI)
			assert.Equal(t, "pkg", got.Scheme)
			assert.Equal(t, "pypi", got.Ecosystem)
		})
	}

	rejected := []struct {
		name string
		in   string
	}{
		// Lookalike host: must not be accepted as a "pypi.org URL."
		// Host-anchoring rejects because "pypi.org.attacker.com/"
		// doesn't match "pypi.org/" exactly.
		{"lookalike host", "https://pypi.org.attacker.com/project/requests/"},
		{"lookalike host no path", "https://pypi.org.attacker.com/project/x"},
		{"lookalike host with www", "https://www.pypi.org.attacker.com/project/x"},
		// Wrong path prefix (not /project/).
		{"user path", "https://pypi.org/user/some-user/"},
		{"help path", "https://pypi.org/help/"},
		{"root", "https://pypi.org/"},
		// /project/ with no name.
		{"project no name", "https://pypi.org/project/"},
		{"project empty name with version", "https://pypi.org/project//2.0.0/"},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := ResolveTarget(tc.in)
			require.Error(t, err, "ResolveTarget(%q) must reject", tc.in)
		})
	}
}

// TestResolveTarget_PyPICanonicalURIs covers the normalization path
// for already-canonical `pkg:pypi/...` inputs: names that aren't in
// PEP 503 canonical form get rebuilt with the normalized name, so
// hand-typed or manifest-parsed inputs like `pkg:pypi/Requests`
// produce the same CanonicalURI as `pkg:pypi/requests`.
//
// This is the storage-fragmentation guard. Without it, a posture
// set against `pkg:pypi/Requests@2.31.0` would land under a
// different entity row than a burn recorded against
// `pkg:pypi/requests@2.31.0` — both referring to the same package
// per PyPI's own identity semantics.
func TestResolveTarget_PyPICanonicalURIs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in          string
		wantURI     string
		wantName    string
		wantVersion string
	}{
		// Already normalized: pass through.
		{"pkg:pypi/requests", "pkg:pypi/requests", "requests", ""},
		{"pkg:pypi/python-dotenv", "pkg:pypi/python-dotenv", "python-dotenv", ""},
		// Case normalization.
		{"pkg:pypi/Requests", "pkg:pypi/requests", "requests", ""},
		{"pkg:pypi/REQUESTS", "pkg:pypi/requests", "requests", ""},
		{"pkg:pypi/PyYAML", "pkg:pypi/pyyaml", "pyyaml", ""},
		// Separator normalization.
		{"pkg:pypi/python_dotenv", "pkg:pypi/python-dotenv", "python-dotenv", ""},
		{"pkg:pypi/python.dotenv", "pkg:pypi/python-dotenv", "python-dotenv", ""},
		// Normalization + version: name normalizes, version passes through.
		{"pkg:pypi/Requests@2.31.0", "pkg:pypi/requests@2.31.0", "requests", "2.31.0"},
		{"pkg:pypi/python_dotenv@1.0.0", "pkg:pypi/python-dotenv@1.0.0", "python-dotenv", "1.0.0"},
		// PEP 440 version forms pass through verbatim.
		{"pkg:pypi/requests@2.31.0rc1", "pkg:pypi/requests@2.31.0rc1", "requests", "2.31.0rc1"},
		{"pkg:pypi/requests@1.0.0.post1", "pkg:pypi/requests@1.0.0.post1", "requests", "1.0.0.post1"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got, err := ResolveTarget(tc.in)
			require.NoError(t, err, "ResolveTarget(%q)", tc.in)
			assert.Equal(t, tc.wantURI, got.CanonicalURI, "canonical URI must be PEP 503-normalized")
			assert.Equal(t, tc.wantName, got.ShortName, "ShortName must be normalized")
			assert.Equal(t, tc.wantVersion, got.Version, "Version must be preserved verbatim")
			assert.Equal(t, "pypi", got.Ecosystem)
		})
	}
}

// TestResolveTarget_NpmCasePreserved is the regression guard for
// the asymmetric normalization policy: npm is case-sensitive at
// the registry level, so `pkg:npm/Express` and `pkg:npm/express`
// are DIFFERENT packages and must produce different canonical
// URIs. This guards against a future refactor that accidentally
// applies PEP 503-style normalization across ecosystems.
func TestResolveTarget_NpmCasePreserved(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in      string
		wantURI string
	}{
		{"pkg:npm/Express", "pkg:npm/Express"},
		{"pkg:npm/EXPRESS", "pkg:npm/EXPRESS"},
		{"pkg:npm/@Types/Node", "pkg:npm/@Types/Node"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got, err := ResolveTarget(tc.in)
			require.NoError(t, err, "ResolveTarget(%q)", tc.in)
			assert.Equal(t, tc.wantURI, got.CanonicalURI,
				"npm case must be preserved; normalization is PyPI-only")
		})
	}
}

func TestResolveTarget_ScopedNpmPackage(t *testing.T) {
	// Scoped npm package names contain a slash (@types/node). The
	// ShortName should be just "node", not the whole scope.
	t.Parallel()

	got, err := ResolveTarget("pkg:npm/@types/node")
	require.NoError(t, err)
	assert.Equal(t, "pkg:npm/@types/node", got.CanonicalURI)
	assert.Equal(t, "node", got.ShortName)
}

func TestResolveTarget_IdentityURI(t *testing.T) {
	t.Parallel()

	got, err := ResolveTarget("identity:github/alecthomas")
	require.NoError(t, err)
	assert.Equal(t, "identity", got.Scheme)
	assert.Equal(t, "github", got.Platform)
	assert.Equal(t, "alecthomas", got.ShortName)
	assert.Empty(t, got.CloneURL)
}

func TestResolveTarget_OrgURI(t *testing.T) {
	t.Parallel()

	got, err := ResolveTarget("org:github/stretchr")
	require.NoError(t, err)
	assert.Equal(t, "org", got.Scheme)
	assert.Equal(t, "github", got.Platform)
	assert.Equal(t, "stretchr", got.ShortName)
}

func TestResolveTarget_PatchURI(t *testing.T) {
	t.Parallel()

	got, err := ResolveTarget("patch:github/alecthomas/kong/593")
	require.NoError(t, err)
	assert.Equal(t, "patch", got.Scheme)
	assert.Equal(t, "github", got.Platform)
	assert.Equal(t, "alecthomas", got.Owner)
	assert.Equal(t, "kong#593", got.ShortName,
		"patch URI should render as repo#id for human display")
}

// TestResolveTarget_VanityGoPaths pins the contract for non-github
// Go module paths: ResolveTarget must produce the same pkg:go/...
// canonical URI the gomod parser produces, not misclassify the
// import-path's first segment as a github owner.
//
// Surfaced by dogfood entry 1: signatory_summary sent
// "modernc.org/sqlite" through ResolveTarget and got
// "repo:github/modernc.org/sqlite" — the github-shorthand fall-
// through accepted "owner/repo"-shaped paths from any host. That's
// a NotFound for any caller using the MCP surface to ask about a
// vanity-Go-path entity, since the store rows are written by the
// gomod parser at pkg:go/<full-path> and have no row at
// repo:github/<vanity-host>/<name>.
//
// Vanity-resolution to a github form (golang.org/x/Y →
// github.com/golang/Y) is a LOOKUP-side alternate, not a
// canonicalization rule. Two reasons:
//
//  1. Most vanity hosts are terminal (gopkg.in, modernc.org, k8s.io
//     under most aliases) — the import path IS the identity; there's
//     no github "true name" to resolve to. Forcing every vanity
//     through a github transformation only works for the small subset
//     where it's defined.
//
//  2. The gomod parser, the analyst handoff template, and ResolveTarget
//     must all agree on what canonical form to write. Picking pkg:go/
//     keeps that agreement in one place; re-resolving live during
//     parse would require either a hardcoded vanity table (incomplete
//     by construction) or live HTTP fetches at parse time (rejected
//     for offline reproducibility).
//
// LookupEntity is the surface that walks alternates.
func TestResolveTarget_VanityGoPaths(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		input   string
		wantURI string
	}{
		{"modernc.org terminal vanity", "modernc.org/sqlite", "pkg:go/modernc.org/sqlite"},
		{"gopkg.in terminal vanity", "gopkg.in/yaml.v3", "pkg:go/gopkg.in/yaml.v3"},
		{"golang.org/x organizational vanity (lookup-side resolves to github)", "golang.org/x/mod", "pkg:go/golang.org/x/mod"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ResolveTarget(tc.input)
			require.NoError(t, err, "ResolveTarget(%q) should succeed", tc.input)
			assert.Equal(t, tc.wantURI, got.CanonicalURI,
				"vanity Go path must produce the same pkg:go/<path> form the gomod parser produces; mismatched canonicalization fragments lookups across writers")
		})
	}
}

// TestResolveTarget_CanonicalRepoURICaseFolded verifies that when an
// already-canonical-shaped repo: URI carries mixed-case owner/name
// segments, ResolveTarget returns the lowercased canonical form —
// matching what CanonicalRepoURI would have produced if the same
// input had come in as "owner/name" shorthand.
//
// Surfaced by dogfood: a 4-analysis BurntSushi/toml entity at the
// canonical (lowercase) URI was shadowed by a stub at the mixed-case
// URI because the analyst-output ingest path used resolveCanonicalURI,
// which validated mixed case as legal but didn't normalize it. The
// case-divergence created two entity rows for what the docs at
// CanonicalRepoURI explicitly say "must collapse to one canonical URI."
//
// Covers repo / identity / org / patch — every scheme whose
// constructor in uri.go lowercases its inputs. (pkg: case handling is
// ecosystem-specific and tested separately.)
func TestResolveTarget_CanonicalRepoURICaseFolded(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		input     string
		wantURI   string
		wantOwner string
		wantName  string
	}{
		{
			"repo mixed-case org",
			"repo:github/BurntSushi/toml",
			"repo:github/burntsushi/toml",
			"burntsushi", "toml",
		},
		{
			"repo all-caps",
			"repo:github/FOO/BAR",
			"repo:github/foo/bar",
			"foo", "bar",
		},
		{
			"repo with mixed-case owner and version suffix",
			"repo:github/BurntSushi/toml@v1.6.0",
			"repo:github/burntsushi/toml@v1.6.0",
			"burntsushi", "toml",
		},
		{
			"identity mixed-case user",
			"identity:github/AlecThomas",
			"identity:github/alecthomas",
			"", "alecthomas",
		},
		{
			"org mixed-case",
			"org:github/Stretchr",
			"org:github/stretchr",
			"", "stretchr",
		},
		{
			"patch mixed-case owner and repo",
			"patch:github/AlecThomas/Kong/593",
			"patch:github/alecthomas/kong/593",
			"alecthomas", "kong#593",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ResolveTarget(tc.input)
			require.NoError(t, err, "ResolveTarget(%q) should succeed", tc.input)
			assert.Equal(t, tc.wantURI, got.CanonicalURI,
				"CanonicalURI must be lowercased to match the constructors in uri.go")
			if tc.wantOwner != "" {
				assert.Equal(t, tc.wantOwner, got.Owner,
					"Owner field must reflect the canonical (lowercased) form")
			}
			assert.Equal(t, tc.wantName, got.ShortName,
				"ShortName field must reflect the canonical (lowercased) form")
		})
	}
}

func TestResolveTarget_NonGitHubPlatformInRepoURI(t *testing.T) {
	// Non-github platforms in canonical form must resolve but
	// carry an empty CloneURL — the CLI hasn't wired up clone for
	// those platforms yet, and callers that check for non-empty
	// CloneURL avoid hitting that gap.
	t.Parallel()

	got, err := ResolveTarget("repo:gitlab/foo/bar")
	require.NoError(t, err)
	assert.Equal(t, "gitlab", got.Platform)
	assert.Equal(t, "bar", got.ShortName)
	assert.Empty(t, got.CloneURL,
		"CloneURL should be empty until the platform is first-classed")
}

func TestResolveTarget_Rejects(t *testing.T) {
	// Each of these should surface an error with a message that
	// names the offending form, so a CLI user sees what went
	// wrong instead of a generic "invalid target."
	t.Parallel()

	cases := []struct {
		name   string
		input  string
		errSub string
	}{
		{"empty string", "", "empty target"},
		{"whitespace only", "   ", "empty target"},
		{"bare name no slash", "thefuck", "does not parse as a github repo"},
		{"repo scheme empty body", "repo:", "empty body"},
		{"pkg scheme missing name", "pkg:npm", "expected ecosystem/name"},
		{"repo missing owner/name", "repo:github/onlyone", "expected platform/owner/name"},
		{"patch missing id", "patch:github/foo/bar", "expected platform/owner/repo/id"},
		{"unknown scheme", "weird:github/x", "does not parse as a github repo"},
		// "weird:github/x" doesn't start with a KNOWN scheme, so it
		// falls through to GitHub-shorthand parsing, which correctly
		// rejects "weird:github/x" as not repo-shaped. The specific
		// error cites both failure modes.

		// Non-github URLs must reject. Previously
		// NormalizeGitHubRepoInput's prefix-strip pipeline silently
		// produced canonical URIs like "repo:github/gitlab.com/foo"
		// from "https://gitlab.com/foo/bar" — wrong and invisible
		// because no error fired. Now the URL-scheme form is gated
		// behind an explicit github.com check.
		// Non-github, non-npmjs URLs are still rejected. The wording
		// changed to mention both accepted hosts when npmjs.com
		// support was added; test substring is narrowed to the
		// invariant portion ("not yet supported").
		{"gitlab https URL", "https://gitlab.com/foo/bar", "not yet supported"},
		{"gitlab http URL", "http://gitlab.com/foo/bar", "not yet supported"},
		{"bitbucket URL", "https://bitbucket.org/team/repo", "not yet supported"},
		{"self-hosted URL", "https://git.example.com/foo/bar", "not yet supported"},
		{"gitlab SCP form", "git@gitlab.com:foo/bar.git", "not a github.com host"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ResolveTarget(tc.input)
			require.Error(t, err, "ResolveTarget(%q) should fail", tc.input)
			assert.Nil(t, got)
			assert.Contains(t, err.Error(), tc.errSub,
				"error should name the specific failure: %v", err)
		})
	}
}

func TestResolveTarget_Stable(t *testing.T) {
	// Same input → same output every call. Stable resolution is
	// a load-bearing property: ingest and query code paths both
	// call ResolveTarget, and they must agree.
	t.Parallel()

	inputs := []string{
		"alecthomas/kong",
		"repo:github/alecthomas/kong",
		"pkg:cargo/atuin",
	}
	for _, in := range inputs {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			first, err := ResolveTarget(in)
			require.NoError(t, err)
			second, err := ResolveTarget(in)
			require.NoError(t, err)
			assert.Equal(t, first, second,
				"ResolveTarget must return identical ResolvedTarget for repeated calls")
		})
	}
}

// TestResolveTarget_VersionedRepoURI extends the @version grammar
// to repo: URIs (canonical, github-shorthand, https URL forms).
// Used by signatory handoff to clone the named ref instead of
// HEAD; without this grammar, /analyze targets are forced to
// HEAD-of-default-branch regardless of what the user pinned.
//
// Mirrors the pkg: shape: CanonicalURI preserves the @V suffix
// (the URI is the request identity), Version surfaces the
// extracted suffix, ShortName carries the bare repo name without
// version. Storage canonicalization (SplitURIVersion) strips the
// suffix when looking up the entity row.
func TestResolveTarget_VersionedRepoURI(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in          string
		wantURI     string
		wantName    string
		wantVersion string
	}{
		// Canonical form.
		{"repo:github/stretchr/testify@v1.11.1",
			"repo:github/stretchr/testify@v1.11.1", "testify", "v1.11.1"},
		// GitHub shorthand.
		{"github.com/stretchr/testify@v1.11.1",
			"repo:github/stretchr/testify@v1.11.1", "testify", "v1.11.1"},
		// Raw HTTPS URL.
		{"https://github.com/stretchr/testify@v1.11.1",
			"repo:github/stretchr/testify@v1.11.1", "testify", "v1.11.1"},
		// .git suffix on URL form is stripped before @version split.
		{"https://github.com/stretchr/testify.git@v1.11.1",
			"repo:github/stretchr/testify@v1.11.1", "testify", "v1.11.1"},
		// Owner/repo shorthand.
		{"stretchr/testify@v1.11.1",
			"repo:github/stretchr/testify@v1.11.1", "testify", "v1.11.1"},
		// SemVer pre-release / build metadata pass through verbatim
		// — the grammar accepts whatever git ref the user names.
		{"stretchr/testify@v1.0.0-rc.1",
			"repo:github/stretchr/testify@v1.0.0-rc.1", "testify", "v1.0.0-rc.1"},
		// Branch name (less common but valid input).
		{"stretchr/testify@main",
			"repo:github/stretchr/testify@main", "testify", "main"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got, err := ResolveTarget(tc.in)
			require.NoError(t, err, "ResolveTarget(%q)", tc.in)
			assert.Equal(t, tc.wantURI, got.CanonicalURI,
				"canonical URI must preserve @version on repo: targets")
			assert.Equal(t, tc.wantName, got.ShortName,
				"ShortName must strip the @version suffix")
			assert.Equal(t, tc.wantVersion, got.Version,
				"Version must be extracted")
		})
	}
}

// TestResolveTarget_VersionedRepoURI_Rejects pins the
// reject-shapes for versioned repo: inputs. Mirrors the pkg: side
// — empty version, double @, etc. should fail loudly so the user
// gets a fix-your-input error instead of a silently-stripped suffix.
func TestResolveTarget_VersionedRepoURI_Rejects(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		in     string
		errSub string
	}{
		{"trailing at on canonical",
			"repo:github/stretchr/testify@", "empty version"},
		{"trailing at on shorthand",
			"stretchr/testify@", "empty version"},
		{"double at",
			"stretchr/testify@v1.0@extra", "nested"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := ResolveTarget(tc.in)
			require.Error(t, err, "ResolveTarget(%q) must reject", tc.in)
			assert.Contains(t, err.Error(), tc.errSub)
		})
	}
}

// TestResolveTarget_UnversionedRepoUnchanged is the
// regression guard: bare `repo:github/X/Y` (no @version) must
// behave EXACTLY as before this grammar extension landed.
// Storage code paths that didn't expect Version to be set on
// repo: targets continue to see Version="".
func TestResolveTarget_UnversionedRepoUnchanged(t *testing.T) {
	t.Parallel()
	cases := []string{
		"repo:github/stretchr/testify",
		"github.com/stretchr/testify",
		"https://github.com/stretchr/testify",
		"https://github.com/stretchr/testify.git",
		"stretchr/testify",
		"git@github.com:stretchr/testify.git",
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			got, err := ResolveTarget(in)
			require.NoError(t, err)
			assert.Equal(t, "repo:github/stretchr/testify", got.CanonicalURI)
			assert.Equal(t, "testify", got.ShortName)
			assert.Empty(t, got.Version,
				"unversioned repo: input must yield empty Version")
		})
	}
}

// TestSplitURIVersion_RepoURIs covers the SplitURIVersion
// extension: previously only pkg: URIs split off @version; now
// repo: URIs also split. Storage canonicalization (posture set
// / get / accept) calls SplitURIVersion to map a versioned URI
// to its unversioned entity; without the repo: extension, the
// entity for `repo:X/Y@v1.0.0` would be the literal versioned
// form (wrong — Plan A says entity is unversioned and version
// lives on the posture row).
func TestSplitURIVersion_RepoURIs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in          string
		wantBase    string
		wantVersion string
	}{
		// New: repo URIs split off @version.
		{"repo:github/stretchr/testify@v1.11.1",
			"repo:github/stretchr/testify", "v1.11.1"},
		{"repo:github/X/Y@main",
			"repo:github/X/Y", "main"},
		// Pre-existing pkg behavior unchanged.
		{"pkg:npm/express@1.2.3",
			"pkg:npm/express", "1.2.3"},
		// Unversioned passes through unchanged.
		{"repo:github/X/Y",
			"repo:github/X/Y", ""},
		{"pkg:npm/express",
			"pkg:npm/express", ""},
		// Other schemes still pass through unchanged.
		{"identity:github/alecthomas",
			"identity:github/alecthomas", ""},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			base, version := SplitURIVersion(tc.in)
			assert.Equal(t, tc.wantBase, base)
			assert.Equal(t, tc.wantVersion, version)
		})
	}
}
