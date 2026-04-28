package profile

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestAlternateURIs covers the lookup-side equivalence map: when an
// exact-URI lookup against the store misses, callers walk the
// alternates list to find an entity stored under a different but
// equivalent URI form.
//
// Equivalences encoded:
//
//   - Version stripping: foo@v1.2.3 ↔ foo. Plan-A says entities live
//     at the unversioned base; pre-v10 stores fragment by version.
//     The base form catches v10+ writes, the versioned form catches
//     historical leftovers (e.g. testify before its M1 fix).
//
//   - Cross-scheme github: pkg:go/github.com/X ↔ pkg:golang/github.com/X
//     ↔ repo:github/X. Three writers (gomod parser, analyst handoff
//     using purl-spec ecosystem name "golang", direct repo URI input)
//     fragment github-hosted Go modules across three URI shapes;
//     equivalents must be tried so a survey lookup finds the entity
//     wherever the analyst chose to write it.
//
//   - Pkg ecosystem prefix: pkg:go/X ↔ pkg:golang/X for non-github
//     paths (vanity hosts). gomod parser writes pkg:go/, the purl spec
//     uses pkg:golang/. Analysts following the spec produce pkg:golang/.
//
//   - golang.org/x organizational vanity: pkg:go/golang.org/x/Y ↔
//     repo:github/golang/Y (and pkg:golang/golang.org/x/Y likewise).
//     Real-world signal-collection pipelines vanity-resolve organisational
//     hosts to their github mirrors at ingest time, so the github form
//     is what often actually exists in the store.
//
// Other vanity hosts (gopkg.in, modernc.org, k8s.io) are deliberately
// NOT in this list — they're terminal-vanity (the import path IS the
// identity, no github mirror exists or matters), so the pkg:go/ form
// is the canonical form and there's no equivalent github URI to try.
func TestAlternateURIs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input string
		// wantContains lists URI forms that MUST appear in the
		// alternates output. Order doesn't matter for inclusion
		// checks; the helper's contract is just "try these."
		wantContains []string
		// wantNotContains lists forms that must NOT appear (used
		// to guard against over-broad alternates that would match
		// unrelated entities, e.g. asserting a non-github vanity
		// path doesn't accidentally produce a repo:github form).
		wantNotContains []string
	}{
		{
			name:  "github repo URI",
			input: "repo:github/stretchr/testify",
			wantContains: []string{
				"repo:github/stretchr/testify",
				"pkg:go/github.com/stretchr/testify",
				"pkg:golang/github.com/stretchr/testify",
			},
		},
		{
			name:  "github repo URI versioned",
			input: "repo:github/stretchr/testify@v1.11.1",
			wantContains: []string{
				"repo:github/stretchr/testify@v1.11.1", // exact
				"repo:github/stretchr/testify",         // version-strip
				"pkg:go/github.com/stretchr/testify",
				"pkg:golang/github.com/stretchr/testify",
				// Cross-scheme + versioned variants — pre-v10
				// fragmentation may have written versioned entity
				// rows for any of these forms.
				"pkg:go/github.com/stretchr/testify@v1.11.1",
				"pkg:golang/github.com/stretchr/testify@v1.11.1",
			},
		},
		{
			name:  "pkg:go/github form cross-resolves to repo:github",
			input: "pkg:go/github.com/alecthomas/kong",
			wantContains: []string{
				"pkg:go/github.com/alecthomas/kong",
				"repo:github/alecthomas/kong",
				"pkg:golang/github.com/alecthomas/kong",
			},
		},
		{
			name:  "pkg:golang/github form cross-resolves to repo:github",
			input: "pkg:golang/github.com/alecthomas/kong",
			wantContains: []string{
				"pkg:golang/github.com/alecthomas/kong",
				"repo:github/alecthomas/kong",
				"pkg:go/github.com/alecthomas/kong",
			},
		},
		{
			name:  "pkg:go vanity (non-github) cross-resolves to pkg:golang",
			input: "pkg:go/gopkg.in/yaml.v3",
			wantContains: []string{
				"pkg:go/gopkg.in/yaml.v3",
				"pkg:golang/gopkg.in/yaml.v3",
			},
			wantNotContains: []string{
				// gopkg.in is a terminal vanity host with no github
				// equivalent — alternates must NOT invent one.
				"repo:github/gopkg.in/yaml.v3",
			},
		},
		{
			name:  "pkg:go/golang.org/x vanity-resolves to repo:github/golang",
			input: "pkg:go/golang.org/x/mod",
			wantContains: []string{
				"pkg:go/golang.org/x/mod",
				"pkg:golang/golang.org/x/mod",
				"repo:github/golang/mod",
			},
		},
		{
			name:  "pkg:golang/golang.org/x vanity-resolves to repo:github/golang",
			input: "pkg:golang/golang.org/x/sync",
			wantContains: []string{
				"pkg:golang/golang.org/x/sync",
				"pkg:go/golang.org/x/sync",
				"repo:github/golang/sync",
			},
		},
		{
			name:  "pkg:go/golang.org/x with version preserves @V on each alternate",
			input: "pkg:go/golang.org/x/mod@v0.35.0",
			wantContains: []string{
				"pkg:go/golang.org/x/mod@v0.35.0",
				"pkg:go/golang.org/x/mod",
				"repo:github/golang/mod@v0.35.0",
				"repo:github/golang/mod",
			},
		},
		{
			name:  "pkg:npm scoped — no equivalents to swap",
			input: "pkg:npm/@types/node",
			wantContains: []string{
				"pkg:npm/@types/node",
			},
			wantNotContains: []string{
				"repo:github/@types/node",
				"pkg:go/@types/node",
			},
		},
		{
			name:         "empty input returns nil",
			input:        "",
			wantContains: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := AlternateURIs(tc.input)

			if len(tc.wantContains) == 0 && tc.input == "" {
				assert.Nil(t, got, "empty input must return nil, not an empty slice")
				return
			}

			// First element must be the input itself — preserves
			// caller intent and avoids surprise when iterating.
			if len(got) > 0 {
				assert.Equal(t, tc.input, got[0],
					"alternates list must lead with the input URI; callers iterate in order and the input is the strongest match")
			}

			for _, want := range tc.wantContains {
				assert.Contains(t, got, want,
					"alternate %q must be present for input %q", want, tc.input)
			}
			for _, unwanted := range tc.wantNotContains {
				assert.NotContains(t, got, unwanted,
					"alternate %q must NOT be present for input %q (would route lookups to an unrelated entity)", unwanted, tc.input)
			}

			// Determinism: alternates have no duplicates. Callers
			// iterate to lookup; duplicates waste round-trips and
			// can mask ordering bugs.
			seen := map[string]bool{}
			for _, uri := range got {
				assert.False(t, seen[uri], "duplicate alternate %q in output: %v", uri, got)
				seen[uri] = true
			}
		})
	}
}
