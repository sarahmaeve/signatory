package pypi

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestExtractPyPILogins covers the conservative login-shape filter
// the collector applies before minting identity:pypi/<login>
// entities. PyPI's info.maintainer / info.author are legacy
// publisher-supplied free-text fields whose contents in the wild
// span:
//
//   - actual registry logins ("ofek", "konstin")
//   - display names ("Saurabh Kumar", "John Smith")
//   - "Name <email>" wrappers ("Some Person <some@example.com>")
//   - comma-separated lists ("alice, bob, charlie")
//   - empty or whitespace-only values
//
// The extractor accepts only login-shaped values — alphanumeric plus
// hyphen/underscore/dot, modest length — and rejects free-text
// display names. Minting identity:pypi/<display name> would pollute
// the store with non-existent identities and create false-positive
// cascades; refusing to mint when the value is ambiguous is the safer
// failure mode.
//
// Why conservative rather than permissive: PyPI's JSON API does NOT
// expose the actual upload-account login (only the
// publisher-declared metadata strings). v0.1 mints best-effort
// publisher entities from the data we have; future enhancements
// (HTML scrape of pypi.org/owners/<name>, or PyPI API expansion)
// would give us the actual registry account login and let us
// loosen this filter.
func TestExtractPyPILogins(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		info *Info
		want []string
	}{
		// ----- accepted shapes -----
		{
			name: "single login in maintainer field",
			info: &Info{Maintainer: "ofek"},
			want: []string{"ofek"},
		},
		{
			name: "single login in author field (no maintainer)",
			info: &Info{Author: "konstin"},
			want: []string{"konstin"},
		},
		{
			name: "Name <email> wrapper — LHS extracted",
			info: &Info{Maintainer: "ofek <ofek@example.com>"},
			want: []string{"ofek"},
		},
		{
			name: "comma-separated list of logins",
			info: &Info{Maintainer: "alice, bob, charlie"},
			want: []string{"alice", "bob", "charlie"},
		},
		{
			name: "PEP 639 maintainers list",
			info: &Info{Maintainers: []Person{{Name: "alice"}, {Name: "bob"}}},
			want: []string{"alice", "bob"},
		},
		{
			name: "Maintainer string AND PEP 639 list — both folded in, deduped",
			info: &Info{
				Maintainer:  "alice",
				Maintainers: []Person{{Name: "bob"}, {Name: "alice"}},
			},
			want: []string{"alice", "bob"},
		},
		{
			name: "uppercase login canonicalised lowercase",
			info: &Info{Maintainer: "Konstin"},
			want: []string{"konstin"},
		},
		{
			name: "underscore and hyphen and dot all accepted",
			info: &Info{Maintainer: "alice_bob, charlie-dan, ev.an"},
			want: []string{"alice_bob", "charlie-dan", "ev.an"},
		},
		{
			name: "Maintainer wins over Author when both set",
			info: &Info{
				Maintainer: "real-maintainer",
				Author:     "original-author",
			},
			want: []string{"real-maintainer"},
		},

		// ----- rejected shapes -----
		{
			name: "free-text display name with space — REJECTED",
			info: &Info{Maintainer: "Saurabh Kumar"},
			want: nil,
		},
		{
			name: "empty fields produce empty result",
			info: &Info{},
			want: nil,
		},
		{
			name: "whitespace-only field rejected",
			info: &Info{Maintainer: "   "},
			want: nil,
		},
		{
			name: "comma list with mixed login + display name skips the display name",
			info: &Info{Maintainer: "alice, John Smith, bob"},
			want: []string{"alice", "bob"},
		},
		{
			name: "extremely long string rejected (length cap)",
			info: &Info{Maintainer: longString(60)},
			want: nil,
		},
		{
			name: "punctuation that isn't login-shape rejected",
			info: &Info{Maintainer: "alice@bob"},
			want: nil,
		},
		{
			name: "leading hyphen rejected — not a valid login start",
			info: &Info{Maintainer: "-leading-hyphen"},
			want: nil,
		},
		{
			name: "PEP 639 entry with empty Name skipped",
			info: &Info{Maintainers: []Person{{Name: ""}, {Name: "bob"}}},
			want: []string{"bob"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := extractPyPILogins(tc.info)
			assert.Equal(t, tc.want, got,
				"extractPyPILogins(%+v) = %v; want %v", tc.info, got, tc.want)
		})
	}
}

// TestExtractPyPILogins_NilSafe pins the nil-input contract: passing
// a nil Info pointer must not panic. The collector reaches the
// extractor only after a successful GetProjectInfo call (which
// returns a non-nil Info on the happy path), but defending the
// boundary keeps a future caller's bug from surfacing as a panic.
func TestExtractPyPILogins_NilSafe(t *testing.T) {
	t.Parallel()
	got := extractPyPILogins(nil)
	assert.Empty(t, got, "nil Info must produce empty slice, not panic")
}

// longString returns a string of n 'a' bytes — used to exercise the
// length-cap branch of extractPyPILogins. n=60 is past the 50-char
// upper bound of pypiLoginPattern.
func longString(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a'
	}
	return string(b)
}
