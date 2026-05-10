package cargo

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNormalizeDeclaredRepoURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want string
	}{
		// Happy path.
		{"plain https", "https://github.com/serde-rs/serde",
			"https://github.com/serde-rs/serde"},
		{"with .git suffix", "https://github.com/serde-rs/serde.git",
			"https://github.com/serde-rs/serde"},
		{"trailing slash", "https://github.com/serde-rs/serde/",
			"https://github.com/serde-rs/serde"},
		{"http upgraded", "http://github.com/serde-rs/serde",
			"https://github.com/serde-rs/serde"},
		{"with fragment", "https://github.com/serde-rs/serde#main",
			"https://github.com/serde-rs/serde"},

		// git+ prefix.
		{"git+https", "git+https://github.com/tokio-rs/tokio.git",
			"https://github.com/tokio-rs/tokio"},
		{"git+ssh", "git+ssh://git@github.com/tokio-rs/tokio.git",
			"https://github.com/tokio-rs/tokio"},

		// ssh:// form. CloneURL is lowercased per
		// profile.CloneURLForRepoPlatform — case-insensitive forge
		// hosts canonicalize on owner+repo, mirroring the canonical
		// URI's lowercasing rule.
		{"ssh form", "ssh://git@github.com/BurntSushi/ripgrep.git",
			"https://github.com/burntsushi/ripgrep"},

		// Multi-forge sources: codeberg and gitlab now resolve as
		// first-classed forges (CloneURLForRepoPlatform stamps the
		// canonical https URL). Rust crates declaring repository on
		// either forge resolve to a clone-able URL the downstream
		// git/repofiles collectors operate against.
		{"codeberg https", "https://codeberg.org/forgejo/forgejo",
			"https://codeberg.org/forgejo/forgejo"},
		{"codeberg with .git", "https://codeberg.org/forgejo/forgejo.git",
			"https://codeberg.org/forgejo/forgejo"},
		{"gitlab https", "https://gitlab.com/gitlab-org/gitlab",
			"https://gitlab.com/gitlab-org/gitlab"},
		{"gitlab with .git", "https://gitlab.com/gitlab-org/gitlab.git",
			"https://gitlab.com/gitlab-org/gitlab"},

		// Rejected shapes → empty.
		{"empty", "", ""},
		{"git:// insecure", "git://github.com/serde-rs/serde", ""},
		// Bitbucket and other unrecognized forges still reject — the
		// URL gate (rejectNonGitHubURL) admits only first-classed
		// forges, and CloneURLForRepoPlatform returns "" for the rest.
		{"bitbucket", "https://bitbucket.org/team/repo", ""},
		{"self-hosted", "https://git.example.com/foo/bar", ""},
		{"self-link", "https://crates.io/crates/serde", ""},
		{"garbage", "not a url", ""},
		{"owner only", "https://github.com/serde-rs", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := NormalizeDeclaredRepoURL(tc.raw)
			assert.Equal(t, tc.want, got)
		})
	}
}
