package profile

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCanonicalPackageURI(t *testing.T) {
	tests := []struct {
		ecosystem string
		name      string
		want      string
	}{
		{"npm", "express", "pkg:npm/express"},
		{"pypi", "requests", "pkg:pypi/requests"},
		{"golang", "github.com/alecthomas/kong", "pkg:golang/github.com/alecthomas/kong"},
		// Ecosystem is lowercased.
		{"NPM", "express", "pkg:npm/express"},
		// Name is preserved as-is (npm is case-sensitive).
		{"npm", "Express", "pkg:npm/Express"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			assert.Equal(t, tt.want, CanonicalPackageURI(tt.ecosystem, tt.name))
		})
	}
}

// TestCanonicalPackageURI_PyPINormalizes pins that PyPI names are
// PEP 503-normalized at construction time — `Requests` and
// `python_dotenv` produce the same canonical URI as `requests` and
// `python-dotenv`. Without this, a caller that builds a pkg:pypi/
// URI without first routing through resolveCanonicalURI (the input-
// parsing path that already normalizes) would produce a non-canonical
// URI and fragment the store across identities the PyPI registry
// considers the same. Defense-in-depth: every pkg:pypi/ URI emitted
// from this package is canonical, regardless of caller hygiene.
//
// Other ecosystems are unchanged — npm preserves case (registry is
// case-sensitive), Go modules preserve verbatim path.
func TestCanonicalPackageURI_PyPINormalizes(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"already canonical", "requests", "pkg:pypi/requests"},
		{"mixed case", "Requests", "pkg:pypi/requests"},
		{"all caps", "REQUESTS", "pkg:pypi/requests"},
		{"underscore", "python_dotenv", "pkg:pypi/python-dotenv"},
		{"dot", "python.dotenv", "pkg:pypi/python-dotenv"},
		{"mixed run", "Python_-.Dotenv", "pkg:pypi/python-dotenv"},
		{"PYPI ecosystem also normalizes", "Requests", "pkg:pypi/requests"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, CanonicalPackageURI("pypi", tc.in))
		})
	}
	// Ecosystem case-fold path also triggers normalization.
	assert.Equal(t, "pkg:pypi/requests",
		CanonicalPackageURI("PYPI", "Requests"),
		"ecosystem PYPI should lowercase to pypi and trigger normalization")
}

func TestCanonicalRepoURI(t *testing.T) {
	assert.Equal(t, "repo:github/alecthomas/kong",
		CanonicalRepoURI("github", "alecthomas", "kong"))
	assert.Equal(t, "repo:github/alecthomas/kong",
		CanonicalRepoURI("GitHub", "alecthomas", "kong"),
		"platform should be lowercased")
	assert.Equal(t, "repo:gitlab/acme/secret",
		CanonicalRepoURI("gitlab", "acme", "secret"))
}

// TestCanonicalRepoURI_LowercasesOwnerAndName documents that owner
// and name are case-folded for case-insensitive platforms (GitHub
// and GitLab both treat these path components as case-insensitive
// at the API and git-clone layer). Without this, mixed-case input
// produces a different canonical URI than lowercase input even
// though they reference the same real-world entity, fragmenting
// the store. See design/dogfood-errors.md "GitHub URI case-folding
// diverges from canonical form".
func TestCanonicalRepoURI_LowercasesOwnerAndName(t *testing.T) {
	assert.Equal(t, "repo:github/burntsushi/toml",
		CanonicalRepoURI("github", "BurntSushi", "toml"),
		"owner case-folded")
	assert.Equal(t, "repo:github/burntsushi/toml",
		CanonicalRepoURI("github", "burntsushi", "TOML"),
		"name case-folded")
	assert.Equal(t, "repo:github/burntsushi/toml",
		CanonicalRepoURI("github", "BURNTSUSHI", "TOML"),
		"both case-folded")
}

func TestCanonicalIdentityURI(t *testing.T) {
	assert.Equal(t, "identity:github/alecthomas",
		CanonicalIdentityURI("github", "alecthomas"))
}

// TestCanonicalIdentityURI_LowercasesUser — same case-fold rule
// for identities. GitHub usernames are case-insensitive on the
// platform side; npm normalizes to lowercase too.
func TestCanonicalIdentityURI_LowercasesUser(t *testing.T) {
	assert.Equal(t, "identity:github/burntsushi",
		CanonicalIdentityURI("github", "BurntSushi"))
}

func TestCanonicalOrgURI(t *testing.T) {
	assert.Equal(t, "org:github/stretchr",
		CanonicalOrgURI("github", "stretchr"))
}

// TestCanonicalOrgURI_LowercasesName — same case-fold rule for
// org URIs.
func TestCanonicalOrgURI_LowercasesName(t *testing.T) {
	assert.Equal(t, "org:github/textualize",
		CanonicalOrgURI("github", "Textualize"))
}

func TestCanonicalPatchURI(t *testing.T) {
	assert.Equal(t, "patch:github/alecthomas/kong/593",
		CanonicalPatchURI("github", "alecthomas", "kong", "593"))
}

// TestCanonicalPatchURI_LowercasesOwnerAndRepo — same case-fold
// rule for patch URIs on the owner+repo segments. The id is
// preserved verbatim (typically numeric, but case-irrelevant by
// nature of being an ID rather than a path component).
func TestCanonicalPatchURI_LowercasesOwnerAndRepo(t *testing.T) {
	assert.Equal(t, "patch:github/burntsushi/toml/42",
		CanonicalPatchURI("github", "BurntSushi", "TOML", "42"))
}

func TestNormalizeGitHubRepoInput(t *testing.T) {
	tests := []struct {
		name  string
		input string
		uri   string
		owner string
		repo  string
	}{
		{
			name:  "bare owner/repo",
			input: "alecthomas/kong",
			uri:   "repo:github/alecthomas/kong",
			owner: "alecthomas",
			repo:  "kong",
		},
		{
			name:  "github.com/owner/repo",
			input: "github.com/alecthomas/kong",
			uri:   "repo:github/alecthomas/kong",
			owner: "alecthomas",
			repo:  "kong",
		},
		{
			name:  "https URL",
			input: "https://github.com/alecthomas/kong",
			uri:   "repo:github/alecthomas/kong",
			owner: "alecthomas",
			repo:  "kong",
		},
		{
			name:  "https URL with .git suffix",
			input: "https://github.com/alecthomas/kong.git",
			uri:   "repo:github/alecthomas/kong",
			owner: "alecthomas",
			repo:  "kong",
		},
		{
			name:  "https URL with trailing slash",
			input: "https://github.com/alecthomas/kong/",
			uri:   "repo:github/alecthomas/kong",
			owner: "alecthomas",
			repo:  "kong",
		},
		{
			name:  "http URL (not https)",
			input: "http://github.com/alecthomas/kong",
			uri:   "repo:github/alecthomas/kong",
			owner: "alecthomas",
			repo:  "kong",
		},
		{
			name:  "SSH URL git@github.com:owner/repo",
			input: "git@github.com:alecthomas/kong",
			uri:   "repo:github/alecthomas/kong",
			owner: "alecthomas",
			repo:  "kong",
		},
		{
			name:  "www prefix",
			input: "www.github.com/alecthomas/kong",
			uri:   "repo:github/alecthomas/kong",
			owner: "alecthomas",
			repo:  "kong",
		},
		{
			name:  "URL with extra path segments",
			input: "https://github.com/alecthomas/kong/pull/42",
			uri:   "repo:github/alecthomas/kong",
			owner: "alecthomas",
			repo:  "kong",
		},
		{
			name:  "whitespace tolerated",
			input: "  alecthomas/kong  ",
			uri:   "repo:github/alecthomas/kong",
			owner: "alecthomas",
			repo:  "kong",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uri, owner, repo, err := NormalizeGitHubRepoInput(tt.input)
			require.NoError(t, err)
			assert.Equal(t, tt.uri, uri)
			assert.Equal(t, tt.owner, owner)
			assert.Equal(t, tt.repo, repo)
		})
	}
}

// TestNormalizeGitHubRepoInput_Stable verifies that all the equivalent
// input forms collapse to the SAME canonical URI — this is the whole
// reason the function exists. Fragmenting duplicate entities across
// input variants is issue #53.
//
// Case variants are part of the equivalence set: GitHub treats owner
// and repo names as case-insensitive at the API and git-clone layer
// (`https://github.com/burntsushi/toml` and `.../BurntSushi/toml`
// resolve to the same repository). Pre-fix this function preserved
// user-typed case verbatim, producing two distinct canonical URIs
// for one real-world repo and fragmenting the store. See
// design/dogfood-errors.md.
func TestNormalizeGitHubRepoInput_Stable(t *testing.T) {
	inputs := []string{
		"alecthomas/kong",
		"github.com/alecthomas/kong",
		"https://github.com/alecthomas/kong",
		"https://github.com/alecthomas/kong.git",
		"https://github.com/alecthomas/kong/",
		"http://github.com/alecthomas/kong",
		"git@github.com:alecthomas/kong",
		"www.github.com/alecthomas/kong",
		"  alecthomas/kong  ",
		// Case variants — must collapse to the lowercase form.
		"Alecthomas/kong",
		"alecthomas/Kong",
		"ALECTHOMAS/KONG",
		"https://github.com/Alecthomas/Kong",
		"git@github.com:ALECTHOMAS/KONG",
	}
	var first string
	for i, input := range inputs {
		uri, _, _, err := NormalizeGitHubRepoInput(input)
		require.NoError(t, err, "input %d (%q) should parse", i, input)
		if i == 0 {
			first = uri
			continue
		}
		assert.Equal(t, first, uri, "input %q should collapse to the same URI as %q", input, inputs[0])
	}
}

func TestNormalizeGitHubRepoInput_Rejects(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"empty", ""},
		{"just whitespace", "   "},
		{"single segment", "justoneword"},
		{"empty owner", "/kong"},
		{"empty repo", "alecthomas/"},
		{"path traversal in owner", "../etc/passwd"},
		{"path traversal in repo", "alecthomas/../passwd"},
		{"null byte in owner", "ale\x00thomas/kong"},
		{"space in repo", "alecthomas/my repo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, _, err := NormalizeGitHubRepoInput(tt.input)
			assert.Error(t, err)
		})
	}
}

// TestValidateCanonicalURI_Accepts verifies that the canonical-form
// outputs of every Canonical*URI constructor pass validation. If this
// test breaks, the constructors and validator have drifted apart.
func TestValidateCanonicalURI_Accepts(t *testing.T) {
	tests := []string{
		CanonicalPackageURI("npm", "express"),
		CanonicalPackageURI("pypi", "requests"),
		CanonicalPackageURI("npm", "Express"), // case preserved
		CanonicalRepoURI("github", "alecthomas", "kong"),
		CanonicalRepoURI("gitlab", "acme", "secret"),
		CanonicalIdentityURI("github", "alecthomas"),
		CanonicalOrgURI("github", "stretchr"),
		CanonicalPatchURI("github", "alecthomas", "kong", "593"),
		// Real-world examples that should be accepted as-is.
		"pkg:golang/github.com/alecthomas/kong",
		// Scoped npm packages: the leading '@' is load-bearing for
		// the scoped-package convention (@types/node, @angular/core).
		// The byte-range validator admits them as a deliberate side
		// effect of the #78 refactor — mixing per-scheme semantics
		// into ValidateCanonicalURI would over-restrict legitimate
		// input. Keep these cases so a future validator change can't
		// silently regress scoped-package support.
		"pkg:npm/@types/node",
		"pkg:npm/@nestjs/core",
		"pkg:npm/@angular/core",
	}
	for _, uri := range tests {
		t.Run(uri, func(t *testing.T) {
			assert.NoError(t, ValidateCanonicalURI(uri),
				"valid canonical URI should not be rejected")
		})
	}
}

// TestValidateCanonicalURI_Rejects covers the attack vectors from
// issue #78. Each input represents a class of malformed/malicious
// data that must NOT be allowed to land in the entities table.
func TestValidateCanonicalURI_Rejects(t *testing.T) {
	tests := []struct {
		name string
		uri  string
	}{
		{"empty", ""},
		{"unknown scheme", "evil:payload"},
		{"no scheme at all", "foo/bar"},
		{"scheme only with no body", "pkg:"},
		{"control char NUL", "pkg:npm/foo\x00bar"},
		{"control char newline", "pkg:npm/foo\nbar"},
		{"control char tab", "pkg:npm/foo\tbar"},
		{"control char escape", "pkg:npm/foo\x1bbar"},
		{"DEL char", "pkg:npm/foo\x7fbar"},
		{"non-ASCII Cyrillic lookalike", "pkg:npm/lod\u0430sh"}, // Cyrillic а
		{"non-ASCII emoji", "pkg:npm/foo\U0001f4a9"},
		{"leading whitespace", " pkg:npm/express"},
		{"trailing whitespace", "pkg:npm/express "},
		{"trailing newline", "pkg:npm/express\n"},
		{"too long", "pkg:npm/" + repeatString("x", 600)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateCanonicalURI(tt.uri)
			require.Error(t, err, "malformed canonical URI must be rejected")
		})
	}
}

// repeatString is a small local helper to keep the test table readable
// without pulling strings.Repeat into the test imports.
func repeatString(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for range n {
		out = append(out, s...)
	}
	return string(out)
}

// TestSplitURIVersion covers the Plan-A canonicalization helper:
// split a canonical URI into (base, version) so posture storage can
// route `pkg:npm/X@V` writes to the unversioned entity with the
// version landing in the posture row's version column. Used by
// posture set/get/unset/accept and the summary assembler.
//
// Contract recap:
//   - pkg URI with @V in the last segment → strip it
//   - pkg URI without @V → pass through, version=""
//   - non-pkg URIs (repo:, identity:, org:, patch:) → pass through,
//     version="" (v0.1 grammar has @V only on pkg URIs)
//   - scoped npm package names (`@types/node`, `@angular/core`) have
//     an @ in the NAME but NOT in the last path segment — the split
//     must find the @ in the last segment only, not the first one.
func TestSplitURIVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		uri         string
		wantBase    string
		wantVersion string
	}{
		{
			name:        "pkg with version",
			uri:         "pkg:npm/foo@1.2.3",
			wantBase:    "pkg:npm/foo",
			wantVersion: "1.2.3",
		},
		{
			name:        "pkg without version",
			uri:         "pkg:npm/foo",
			wantBase:    "pkg:npm/foo",
			wantVersion: "",
		},
		{
			name:        "scoped npm with version",
			uri:         "pkg:npm/@types/node@22.0.0",
			wantBase:    "pkg:npm/@types/node",
			wantVersion: "22.0.0",
		},
		{
			name:        "scoped npm without version",
			uri:         "pkg:npm/@types/node",
			wantBase:    "pkg:npm/@types/node",
			wantVersion: "",
		},
		{
			name:        "pkg go with version",
			uri:         "pkg:go/github.com/foo/bar@v1.2.3",
			wantBase:    "pkg:go/github.com/foo/bar",
			wantVersion: "v1.2.3",
		},
		{
			name:        "pkg pypi with dotted version",
			uri:         "pkg:pypi/requests@2.31.0",
			wantBase:    "pkg:pypi/requests",
			wantVersion: "2.31.0",
		},
		{
			name:        "repo URI passes through",
			uri:         "repo:github/foo/bar",
			wantBase:    "repo:github/foo/bar",
			wantVersion: "",
		},
		{
			name:        "identity URI passes through",
			uri:         "identity:github/someone",
			wantBase:    "identity:github/someone",
			wantVersion: "",
		},
		{
			name:        "patch URI passes through",
			uri:         "patch:github/foo/bar/42",
			wantBase:    "patch:github/foo/bar/42",
			wantVersion: "",
		},
		{
			name:        "empty string passes through",
			uri:         "",
			wantBase:    "",
			wantVersion: "",
		},
		{
			name:        "pkg with hyphenated version",
			uri:         "pkg:npm/foo@1.2.3-rc.1",
			wantBase:    "pkg:npm/foo",
			wantVersion: "1.2.3-rc.1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotBase, gotVersion := SplitURIVersion(tt.uri)
			assert.Equal(t, tt.wantBase, gotBase, "base URI mismatch")
			assert.Equal(t, tt.wantVersion, gotVersion, "version mismatch")
		})
	}
}
