package pypi

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestNormalizeDeclaredRepoURL covers every shape PyPI packages are
// known to emit in their info.project_urls map (and the deprecated
// info.home_page field) plus the forms that must be rejected.
//
// PyPI's project_urls is free-form — publishers type whatever they
// want into the [project.urls] table or its Poetry equivalent — so
// the URL forms encountered in the wild are essentially the
// intersection of "what humans paste" and "what github displays."
// Differences from npm: PyPI has no "github:owner/repo" shorthand
// and no "git@github.com:owner/repo.git" SCP form is conventional;
// the canonical declarations are https URLs with or without .git.
//
// Run under t.Parallel with subtests named after the input for
// debuggability.
func TestNormalizeDeclaredRepoURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   string
		want string
	}{
		// Accepted github forms → normalized https URL.
		{"https no .git", "https://github.com/psf/requests", "https://github.com/psf/requests"},
		{"https with .git", "https://github.com/psf/requests.git", "https://github.com/psf/requests"},
		{"http (downgraded)", "http://github.com/psf/requests", "https://github.com/psf/requests"},
		{"trailing slash", "https://github.com/psf/requests/", "https://github.com/psf/requests"},
		{"git+https with .git", "git+https://github.com/psf/requests.git", "https://github.com/psf/requests"},
		{"git+https no .git", "git+https://github.com/psf/requests", "https://github.com/psf/requests"},
		{"ssh with git+ prefix", "git+ssh://git@github.com/psf/requests.git", "https://github.com/psf/requests"},
		{"ssh alone", "ssh://git@github.com/psf/requests.git", "https://github.com/psf/requests"},
		{"https with branch fragment", "https://github.com/psf/requests#main", "https://github.com/psf/requests"},
		{"https with commit fragment", "https://github.com/psf/requests#abc123", "https://github.com/psf/requests"},
		{"www host", "https://www.github.com/psf/requests", "https://github.com/psf/requests"},

		// Rejected forms → empty string (caller treats as "no
		// resolvable github source").
		{"empty", "", ""},
		{"whitespace only", "   ", ""},
		{"git protocol (insecure)", "git://github.com/psf/requests", ""},
		{"git+git protocol (insecure)", "git+git://github.com/psf/requests.git", ""},
		{"gitlab host", "https://gitlab.com/foo/bar", ""},
		{"bitbucket host", "https://bitbucket.org/foo/bar", ""},
		{"docs site (Homepage typically)", "https://requests.readthedocs.io/", ""},
		{"pypi self-link", "https://pypi.org/project/requests/", ""},
		{"non-URL string", "not a url", ""},
		{"github but missing repo", "https://github.com/psf", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := NormalizeDeclaredRepoURL(tc.in)
			assert.Equal(t, tc.want, got,
				"NormalizeDeclaredRepoURL(%q) = %q; want %q", tc.in, got, tc.want)
		})
	}
}
