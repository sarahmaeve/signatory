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

func TestResolveTarget_PkgURI(t *testing.T) {
	t.Parallel()

	got, err := ResolveTarget("pkg:cargo/atuin")
	require.NoError(t, err)
	assert.Equal(t, "pkg:cargo/atuin", got.CanonicalURI)
	assert.Equal(t, "atuin", got.ShortName)
	assert.Equal(t, "pkg", got.Scheme)
	assert.Empty(t, got.Platform)
	assert.Empty(t, got.CloneURL, "pkg: URIs have no clone URL")
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
		{"gitlab https URL", "https://gitlab.com/foo/bar", "not a github.com URL"},
		{"gitlab http URL", "http://gitlab.com/foo/bar", "not a github.com URL"},
		{"bitbucket URL", "https://bitbucket.org/team/repo", "not a github.com URL"},
		{"self-hosted URL", "https://git.example.com/foo/bar", "not a github.com URL"},
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
