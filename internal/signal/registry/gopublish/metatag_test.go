package gopublish

import (
	"strings"
	"testing"
)

// TestParseGoImportMeta covers the canonical and edge shapes for the
// go-import meta tag (https://pkg.go.dev/cmd/go#hdr-Remote_import_paths):
//
//	<meta name="go-import" content="<importPrefix> <vcs> <repoRoot>">
//
// The content attribute carries three space-separated fields. Other
// meta tags in the document (charset, description, viewport, Open
// Graph) MUST be ignored. The parser must accept both attribute
// orderings and both quote styles, but reject anything malformed.
//
// Cross-origin / non-github / non-git filtering is the caller's job
// (the parser surfaces what's there; the caller's policy decides
// what to use).
func TestParseGoImportMeta(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		body         string
		wantPrefix   string
		wantVCS      string
		wantRepoRoot string
		wantOK       bool
	}{
		{
			name: "canonical form, double-quoted",
			body: `<html><head>
				<meta name="go-import" content="gopkg.in/yaml.v3 git https://github.com/go-yaml/yaml">
			</head></html>`,
			wantPrefix:   "gopkg.in/yaml.v3",
			wantVCS:      "git",
			wantRepoRoot: "https://github.com/go-yaml/yaml",
			wantOK:       true,
		},
		{
			name:         "single-quoted attributes",
			body:         `<meta name='go-import' content='gopkg.in/yaml.v3 git https://github.com/go-yaml/yaml'>`,
			wantPrefix:   "gopkg.in/yaml.v3",
			wantVCS:      "git",
			wantRepoRoot: "https://github.com/go-yaml/yaml",
			wantOK:       true,
		},
		{
			name: "content-first attribute order",
			// HTML attribute ordering is permissive — vanity hosts in the
			// wild emit either order. Parser must accept both.
			body:         `<meta content="modernc.org/sqlite git https://gitlab.com/cznic/sqlite" name="go-import">`,
			wantPrefix:   "modernc.org/sqlite",
			wantVCS:      "git",
			wantRepoRoot: "https://gitlab.com/cznic/sqlite",
			wantOK:       true,
		},
		{
			name:         "extra attributes between name and content",
			body:         `<meta charset="utf-8" name="go-import" lang="en" content="gopkg.in/yaml.v3 git https://github.com/go-yaml/yaml">`,
			wantPrefix:   "gopkg.in/yaml.v3",
			wantVCS:      "git",
			wantRepoRoot: "https://github.com/go-yaml/yaml",
			wantOK:       true,
		},
		{
			name: "tag spans multiple lines",
			// A vanity host that pretty-prints HTML across lines must
			// still parse cleanly. The single-line regex forces us to
			// either DOTALL or pre-collapse whitespace; the parser
			// must handle one of those.
			body: `<meta
				name="go-import"
				content="gopkg.in/yaml.v3 git https://github.com/go-yaml/yaml">`,
			wantPrefix:   "gopkg.in/yaml.v3",
			wantVCS:      "git",
			wantRepoRoot: "https://github.com/go-yaml/yaml",
			wantOK:       true,
		},
		{
			name: "go-import tag among many other meta tags",
			body: `<html><head>
				<meta charset="utf-8">
				<meta name="viewport" content="width=device-width">
				<meta name="description" content="A YAML library">
				<meta name="go-import" content="gopkg.in/yaml.v3 git https://github.com/go-yaml/yaml">
				<meta name="og:title" content="yaml.v3">
			</head></html>`,
			wantPrefix:   "gopkg.in/yaml.v3",
			wantVCS:      "git",
			wantRepoRoot: "https://github.com/go-yaml/yaml",
			wantOK:       true,
		},
		{
			name:   "empty body",
			body:   "",
			wantOK: false,
		},
		{
			name:   "no meta tags at all",
			body:   "<html><body><h1>nothing here</h1></body></html>",
			wantOK: false,
		},
		{
			name:   "meta tag with name=description but no go-import",
			body:   `<meta name="description" content="something else entirely">`,
			wantOK: false,
		},
		{
			name: "go-import with malformed content (only two fields)",
			// Adversarial-ish input. content needs three fields; two
			// is not a valid go-import declaration.
			body:   `<meta name="go-import" content="gopkg.in/yaml.v3 git">`,
			wantOK: false,
		},
		{
			name:   "go-import with one field",
			body:   `<meta name="go-import" content="gopkg.in/yaml.v3">`,
			wantOK: false,
		},
		{
			name:   "go-import with empty content",
			body:   `<meta name="go-import" content="">`,
			wantOK: false,
		},
		{
			name: "case-insensitive name attribute",
			// HTML attribute names are case-insensitive per spec;
			// vanity hosts in the wild sometimes emit "Go-Import" or
			// "GO-IMPORT" — the parser must accept any case.
			body:         `<meta NAME="GO-IMPORT" CONTENT="gopkg.in/yaml.v3 git https://github.com/go-yaml/yaml">`,
			wantPrefix:   "gopkg.in/yaml.v3",
			wantVCS:      "git",
			wantRepoRoot: "https://github.com/go-yaml/yaml",
			wantOK:       true,
		},
		{
			name: "first matching tag wins on duplicates",
			// A sketchy vanity host with multiple go-import tags. The
			// caller's policy may further filter (cross-origin check,
			// non-github rejection); the parser surfaces the first.
			body: `<meta name="go-import" content="gopkg.in/yaml.v3 git https://github.com/go-yaml/yaml">
				<meta name="go-import" content="gopkg.in/yaml.v3 git https://attacker.example/evil">`,
			wantPrefix:   "gopkg.in/yaml.v3",
			wantVCS:      "git",
			wantRepoRoot: "https://github.com/go-yaml/yaml",
			wantOK:       true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotPrefix, gotVCS, gotRoot, ok := parseGoImportMeta([]byte(tc.body))
			if ok != tc.wantOK {
				t.Errorf("parseGoImportMeta(%q): ok = %v; want %v",
					strings.TrimSpace(tc.body), ok, tc.wantOK)
				return
			}
			if !ok {
				return // negative cases assert only on the bool
			}
			if gotPrefix != tc.wantPrefix {
				t.Errorf("importPrefix = %q; want %q", gotPrefix, tc.wantPrefix)
			}
			if gotVCS != tc.wantVCS {
				t.Errorf("vcs = %q; want %q", gotVCS, tc.wantVCS)
			}
			if gotRoot != tc.wantRepoRoot {
				t.Errorf("repoRoot = %q; want %q", gotRoot, tc.wantRepoRoot)
			}
		})
	}
}
