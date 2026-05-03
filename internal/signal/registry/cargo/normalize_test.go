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

		// ssh:// form.
		{"ssh form", "ssh://git@github.com/BurntSushi/ripgrep.git",
			"https://github.com/BurntSushi/ripgrep"},

		// Rejected shapes → empty.
		{"empty", "", ""},
		{"git:// insecure", "git://github.com/serde-rs/serde", ""},
		{"non-github", "https://gitlab.com/foo/bar", ""},
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
