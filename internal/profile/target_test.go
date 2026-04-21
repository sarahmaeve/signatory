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
		// Other hosts entirely.
		{"pypi url", "https://pypi.org/project/requests/"},
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
