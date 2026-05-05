package gopublish

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// --- Fuzz targets for gopublish meta-tag and URL parsers ---
//
// These parsers process HTML from arbitrary vanity hosts via HTTP.
// An attacker controlling a vanity host (or performing a MITM) can
// serve crafted HTML that targets:
//
//   - ReDoS: regex patterns with [^>]* or [^"']* on adversarial input
//   - Control char injection: meta tag content flowing into downstream signals
//   - Path confusion: extractGitHubURLFromString on malformed templates
//
// The fuzz tests verify safety invariants: no panics, no excessive
// runtime (ReDoS), no control chars in output, valid UTF-8.

// --- FuzzParseMetaContent ---
//
// This is the lowest-level parser — regex-based extraction of a meta
// tag's content attribute from arbitrary HTML. Two regex patterns
// (name-first, content-first) are tried. The ReDoS surface is the
// [^>]* between attributes — an adversarial HTML page with many
// attributes or deeply nested non-'>' characters could cause
// catastrophic backtracking.

func FuzzParseMetaContent(f *testing.F) {
	// Happy path: canonical go-import tag
	f.Add([]byte(`<meta name="go-import" content="gopkg.in/yaml.v3 git https://github.com/go-yaml/yaml">`))
	// Single-quoted
	f.Add([]byte(`<meta name='go-import' content='example.com/pkg git https://github.com/x/y'>`))
	// Content-first ordering
	f.Add([]byte(`<meta content="example.com git https://github.com/x/y" name="go-import">`))
	// go-source tag (tested via go-import name since we fix the metaName)
	f.Add([]byte(`<meta name="go-import" content="gopkg.in/yaml.v3 git https://github.com/go-yaml/yaml">`))
	// Multiline tag
	f.Add([]byte("<meta\n  name=\"go-import\"\n  content=\"example.com git https://github.com/x/y\"\n>"))
	// Tag buried in large HTML
	f.Add([]byte(`<html><head><title>x</title><meta charset="utf-8"><meta name="viewport" content="width=device-width"><meta name="go-import" content="example.com git https://github.com/x/y"></head><body>lots of content here</body></html>`))
	// No match
	f.Add([]byte(`<html><head><title>no meta here</title></head></html>`))
	// Empty
	f.Add([]byte{})
	// Adversarial: many attributes between name and content
	f.Add([]byte(`<meta name="go-import" data-x="1" data-y="2" data-z="3" content="a b c">`))
	// Adversarial: very long attribute value
	f.Add([]byte(`<meta name="go-import" content="` + strings.Repeat("x", 5000) + `">`))
	// Adversarial: unclosed tag (no >)
	f.Add([]byte(`<meta name="go-import" content="a b c"`))
	// Adversarial: many unclosed angle brackets
	f.Add([]byte(strings.Repeat(`<meta name="go-import" `, 100)))
	// Adversarial: deeply nested non-closing attributes (ReDoS probe)
	f.Add([]byte(`<meta name="go-import" ` + strings.Repeat(`x="y" `, 500) + `content="a b c">`))

	f.Fuzz(func(t *testing.T, body []byte) {
		// Fix metaName to production values. The fuzzer explores
		// the body (attacker-controlled); the metaName is an internal
		// constant. Fuzzing metaName introduces regex-compilation
		// edge cases that don't exist in production.
		result, _ := parseMetaContent(body, "go-import")

		// Invariant 1: valid UTF-8
		if !utf8.ValidString(result) {
			t.Errorf("parseMetaContent returned invalid UTF-8: %q", result)
		}
		// Invariant 2: no control characters
		assertNoControlChars(t, "parseMetaContent", result)
		// Invariant 3: result should be trimmed
		if result != strings.TrimSpace(result) {
			t.Errorf("parseMetaContent returned untrimmed result: %q", result)
		}
	})
}

// --- FuzzParseGoImportMeta ---

func FuzzParseGoImportMeta(f *testing.F) {
	f.Add([]byte(`<meta name="go-import" content="gopkg.in/yaml.v3 git https://github.com/go-yaml/yaml">`))
	f.Add([]byte(`<meta name="go-import" content="example.com/pkg git https://github.com/x/y">`))
	// Two fields (missing one)
	f.Add([]byte(`<meta name="go-import" content="example.com git">`))
	// Four fields (extra)
	f.Add([]byte(`<meta name="go-import" content="a b c d">`))
	// Whitespace-heavy content
	f.Add([]byte(`<meta name="go-import" content="  example.com   git   https://github.com/x/y  ">`))
	f.Add([]byte{})
	f.Add([]byte(`<html></html>`))

	f.Fuzz(func(t *testing.T, body []byte) {
		prefix, vcs, repoRoot, ok := parseGoImportMeta(body)

		if !ok {
			// No match — all outputs should be empty
			if prefix != "" || vcs != "" || repoRoot != "" {
				t.Errorf("parseGoImportMeta returned ok=false but non-empty fields: %q %q %q", prefix, vcs, repoRoot)
			}
			return
		}

		// Invariant 1: all three fields must be non-empty on success
		if prefix == "" || vcs == "" || repoRoot == "" {
			t.Errorf("parseGoImportMeta returned ok=true but empty field: prefix=%q vcs=%q repoRoot=%q", prefix, vcs, repoRoot)
		}

		// Invariant 2: no field should contain whitespace (they're
		// split on Fields, so internal spaces mean the split is wrong)
		for _, s := range []string{prefix, vcs, repoRoot} {
			if strings.ContainsAny(s, " \t\n\r") {
				t.Errorf("parseGoImportMeta field contains whitespace: %q", s)
			}
		}

		// Invariant 3: valid UTF-8
		for _, s := range []string{prefix, vcs, repoRoot} {
			if !utf8.ValidString(s) {
				t.Errorf("parseGoImportMeta returned invalid UTF-8: %q", s)
			}
		}

		// Invariant 4: no control characters
		for _, s := range []string{prefix, vcs, repoRoot} {
			assertNoControlChars(t, "parseGoImportMeta", s)
		}
	})
}

// --- FuzzExtractGitHubURLFromString ---
//
// Extracts https://github.com/<owner>/<repo> from templated go-source
// directory/file fields. Input comes from meta tag content — which
// itself comes from an attacker-controlled vanity host.

func FuzzExtractGitHubURLFromString(f *testing.F) {
	// Normal templated fields from go-source
	f.Add("https://github.com/go-yaml/yaml/tree/v3.0.1{/dir}")
	f.Add("https://github.com/go-yaml/yaml/blob/v3.0.1{/dir}/{file}#L{line}")
	f.Add("https://github.com/user/repo")
	f.Add("https://github.com/user/repo.git")
	// No github
	f.Add("https://gitlab.com/user/repo")
	f.Add("")
	// github.com with no path
	f.Add("https://github.com/")
	f.Add("https://github.com/owner")
	f.Add("https://github.com/owner/")
	// Multiple github.com occurrences
	f.Add("see https://github.com/a/b and https://github.com/c/d")
	// Adversarial: very long path
	f.Add("https://github.com/" + strings.Repeat("a", 1000) + "/" + strings.Repeat("b", 1000))
	// Adversarial: special characters
	f.Add("https://github.com/../../../etc/passwd")
	f.Add("https://github.com/owner/repo?query=1#fragment")
	f.Add("https://github.com/\x00owner/\x00repo")

	f.Fuzz(func(t *testing.T, input string) {
		result := extractGitHubURLFromString(input)

		if result == "" {
			return
		}

		// Invariant 1: must start with canonical prefix
		const prefix = "https://github.com/"
		if !strings.HasPrefix(result, prefix) {
			t.Errorf("extractGitHubURLFromString returned non-canonical URL: %q", result)
		}

		// Invariant 2: must have exactly owner/repo after prefix (two segments)
		remainder := strings.TrimPrefix(result, prefix)
		parts := strings.Split(remainder, "/")
		if len(parts) != 2 {
			t.Errorf("extractGitHubURLFromString result has %d path segments, want 2: %q", len(parts), result)
		}

		// Invariant 3: neither owner nor repo should be empty
		if len(parts) == 2 && (parts[0] == "" || parts[1] == "") {
			t.Errorf("extractGitHubURLFromString returned empty owner or repo: %q", result)
		}

		// Invariant 4: no control characters in result
		assertNoControlChars(t, "extractGitHubURLFromString", result)

		// Invariant 5: valid UTF-8
		if !utf8.ValidString(result) {
			t.Errorf("extractGitHubURLFromString returned invalid UTF-8: %q", result)
		}

		// Invariant 6: path segments must not be literal traversal
		// components. "..0" is merely a GitHub-invalid name (and our
		// isValidPathSegment rejects pure ".." and "."), but it's not
		// a traversal vector.
		if strings.Contains(result, "/../") || strings.HasSuffix(result, "/..") {
			t.Errorf("extractGitHubURLFromString returned URL with path traversal: %q", result)
		}

		// Invariant 7: should not end in .git (we trim it)
		if strings.HasSuffix(result, ".git") {
			t.Errorf("extractGitHubURLFromString did not trim .git suffix: %q", result)
		}

		// Invariant 8: no query params or fragments leaked through
		if strings.ContainsAny(result, "?#{}") {
			t.Errorf("extractGitHubURLFromString contains query/fragment/template chars: %q", result)
		}
	})
}

// --- Helpers ---

// assertNoControlChars fails if s contains ASCII control characters
// (0x00–0x1F) except tab. These should never appear in import paths,
// VCS identifiers, or repository URLs.
func assertNoControlChars(t *testing.T, fn, s string) {
	t.Helper()
	for i, r := range s {
		if r < 0x20 && r != '\t' {
			t.Errorf("%s: control char U+%04X at byte %d in %q", fn, r, i, s)
			return
		}
	}
}
