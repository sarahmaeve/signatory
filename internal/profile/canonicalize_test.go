package profile

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestCanonicalizeURI pins the consolidation-decision contract used
// by `signatory prune duplicates`. For each input URI, the function
// returns the One True Canonical Form per the rules in the doc-
// comment. Already-canonical inputs return verbatim.
func TestCanonicalizeURI(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		// Case-fold class.
		{
			"repo:github mixed-case folds to lowercase",
			"repo:github/BurntSushi/toml",
			"repo:github/burntsushi/toml",
		},
		{
			"repo:github all-caps folds to lowercase",
			"repo:github/FOO/BAR",
			"repo:github/foo/bar",
		},
		{
			"identity:github mixed-case folds to lowercase",
			"identity:github/AlecThomas",
			"identity:github/alecthomas",
		},
		{
			"org:github mixed-case folds to lowercase",
			"org:github/Stretchr",
			"org:github/stretchr",
		},
		{
			"patch:github mixed-case folds to lowercase",
			"patch:github/AlecThomas/Kong/593",
			"patch:github/alecthomas/kong/593",
		},

		// Ecosystem-prefix class.
		{
			"pkg:go vanity rewrites to pkg:golang",
			"pkg:go/gopkg.in/yaml.v3",
			"pkg:golang/gopkg.in/yaml.v3",
		},
		{
			"pkg:go modernc.org rewrites",
			"pkg:go/modernc.org/sqlite",
			"pkg:golang/modernc.org/sqlite",
		},
		{
			"pkg:go github embedded rewrites",
			"pkg:go/github.com/alecthomas/kong",
			"pkg:golang/github.com/alecthomas/kong",
		},

		// Versioned-entity class.
		{
			"versioned repo URI strips @V",
			"repo:github/stretchr/testify@v1.11.1",
			"repo:github/stretchr/testify",
		},
		{
			"versioned pkg URI strips @V",
			"pkg:npm/express@4.18.2",
			"pkg:npm/express",
		},
		{
			"versioned pkg:go strips @V AND rewrites prefix",
			"pkg:go/golang.org/x/mod@v0.35.0",
			"pkg:golang/golang.org/x/mod",
		},
		{
			"versioned mixed-case repo strips @V AND case-folds",
			"repo:github/BurntSushi/toml@v1.6.0",
			"repo:github/burntsushi/toml",
		},

		// Already-canonical (no-op).
		{"already-canonical lowercase repo", "repo:github/alecthomas/kong", "repo:github/alecthomas/kong"},
		{"already-canonical pkg:golang", "pkg:golang/gopkg.in/yaml.v3", "pkg:golang/gopkg.in/yaml.v3"},
		{"already-canonical npm scoped", "pkg:npm/@types/node", "pkg:npm/@types/node"},
		{"already-canonical npm scoped versioned strips @V only", "pkg:npm/@types/node@20.0.0", "pkg:npm/@types/node"},

		// Edge cases.
		{"empty input returns empty", "", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := CanonicalizeURI(tc.in)
			assert.Equal(t, tc.want, got,
				"CanonicalizeURI(%q) — the One True Canonical determines which prune-duplicates ops fire", tc.in)
		})
	}
}

// TestCanonicalizeURI_Idempotent: running CanonicalizeURI on its
// own output produces the same output. Without this, a
// consolidate-then-re-detect loop could keep finding work.
func TestCanonicalizeURI_Idempotent(t *testing.T) {
	t.Parallel()

	inputs := []string{
		"repo:github/BurntSushi/toml",
		"pkg:go/gopkg.in/yaml.v3",
		"pkg:go/golang.org/x/mod@v0.35.0",
		"identity:github/AlecThomas",
		"pkg:npm/@types/node@20.0.0",
		"repo:github/alecthomas/kong", // already-canonical
	}
	for _, in := range inputs {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			once := CanonicalizeURI(in)
			twice := CanonicalizeURI(once)
			assert.Equal(t, once, twice,
				"CanonicalizeURI must be idempotent: f(f(x)) == f(x)")
		})
	}
}
