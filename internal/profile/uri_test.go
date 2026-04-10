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

func TestCanonicalRepoURI(t *testing.T) {
	assert.Equal(t, "repo:github/alecthomas/kong",
		CanonicalRepoURI("github", "alecthomas", "kong"))
	assert.Equal(t, "repo:github/alecthomas/kong",
		CanonicalRepoURI("GitHub", "alecthomas", "kong"),
		"platform should be lowercased")
	assert.Equal(t, "repo:gitlab/acme/secret",
		CanonicalRepoURI("gitlab", "acme", "secret"))
}

func TestCanonicalIdentityURI(t *testing.T) {
	assert.Equal(t, "identity:github/alecthomas",
		CanonicalIdentityURI("github", "alecthomas"))
}

func TestCanonicalOrgURI(t *testing.T) {
	assert.Equal(t, "org:github/stretchr",
		CanonicalOrgURI("github", "stretchr"))
}

func TestCanonicalPatchURI(t *testing.T) {
	assert.Equal(t, "patch:github/alecthomas/kong/593",
		CanonicalPatchURI("github", "alecthomas", "kong", "593"))
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
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}
