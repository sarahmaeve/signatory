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
