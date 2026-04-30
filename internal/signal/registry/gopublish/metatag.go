package gopublish

import (
	"regexp"
	"strings"
	"sync"
)

// parseGoImportMeta extracts the first go-import meta tag from an
// HTML body and returns its three content fields:
//
//	importPrefix vcs repoRoot
//
// per the spec at https://pkg.go.dev/cmd/go#hdr-Remote_import_paths:
//
//	<meta name="go-import" content="<importPrefix> <vcs> <repoRoot>">
//
// Returns ("", "", "", false) when no well-formed go-import meta tag
// is present.
//
// See parseMetaContent for the permissiveness contract on attribute
// ordering, casing, quoting, and multi-line tags.
func parseGoImportMeta(body []byte) (importPrefix, vcs, repoRoot string, ok bool) {
	content, ok := parseMetaContent(body, "go-import")
	if !ok {
		return "", "", "", false
	}
	parts := strings.Fields(content)
	if len(parts) != 3 {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
}

// parseGoSourceMeta extracts the first go-source meta tag from an
// HTML body and returns its four content fields:
//
//	importPrefix home directory file
//
// per https://github.com/golang/gddo/wiki/Source-Code-Links. The
// directory and file fields typically contain templated URLs with
// placeholders like {/dir}, {/file}, {line}.
//
// The go-source tag is signatory's escape hatch for vanity hosts
// that proxy git themselves (gopkg.in is the canonical example):
// go-import points at the proxy ("https://gopkg.in/yaml.v3") while
// go-source points at the canonical github mirror. Pulling the
// github URL out of go-source's directory/file fields is what lets
// signatory's github + openssf collectors fire on those modules.
//
// The "home" field can be "_" (underscore) — a documented sentinel
// meaning "no home page distinct from the directory URL." The
// parser surfaces it verbatim; the caller filters.
//
// Returns ("", "", "", "", false) when no well-formed go-source
// meta tag is present.
func parseGoSourceMeta(body []byte) (importPrefix, home, directory, file string, ok bool) {
	content, ok := parseMetaContent(body, "go-source")
	if !ok {
		return "", "", "", "", false
	}
	parts := strings.Fields(content)
	if len(parts) != 4 {
		return "", "", "", "", false
	}
	return parts[0], parts[1], parts[2], parts[3], true
}

// parseMetaContent returns the raw content attribute value of the
// first <meta> tag whose name attribute (case-insensitive) matches
// metaName. Returns ("", false) when no matching tag is present.
//
// Permissiveness vs. strictness:
//
//   - HTML attribute order is permissive: name=... content=... and
//     content=... name=... are both valid. Parser accepts both.
//   - HTML attribute names are case-insensitive per spec; "GO-IMPORT"
//     and "Go-Import" match the same as "go-import".
//   - Both single- and double-quoted attribute values are accepted.
//   - The tag may span multiple lines (a vanity host that pretty-
//     prints HTML).
//   - Multiple matching tags: first match wins. Caller policy
//     (cross-origin check, non-github rejection) further filters.
//
// What this parser does NOT do:
//
//   - Decode HTML entities (&amp;, &lt;) — module paths and repo URLs
//     don't contain those in practice; the go module system itself
//     rejects them.
//   - Honor HTML comments — <!-- <meta ... > --> would still match.
//     Tradeoff: a hostile vanity host could exploit it, but the
//     cross-origin / non-github checks in the caller catch realistic
//     exploitation paths.
//   - Validate field shapes — that's the caller's policy. Surfaces
//     what's there; whether to use it is a higher-layer decision.
//
// Implementation: regex pairs (one per attribute order) compiled
// once per metaName at first call, cached. No HTML parser
// dependency.
func parseMetaContent(body []byte, metaName string) (string, bool) {
	if len(body) == 0 {
		return "", false
	}
	patterns := metaContentPatternsFor(metaName)
	for _, p := range patterns {
		matches := p.FindAllSubmatch(body, -1)
		for _, match := range matches {
			if len(match) < 2 {
				continue
			}
			return strings.TrimSpace(string(match[1])), true
		}
	}
	return "", false
}

// metaContentPatternsFor compiles (or returns from cache) the two
// regex patterns for finding a meta tag with the given name attribute.
// Each pattern's capture group 1 is the content attribute's value.
//
// Cached because regex compilation is non-trivial — repeated calls
// for the same metaName (e.g., "go-import" parsed once per
// resolution) shouldn't pay the compile cost every time.
func metaContentPatternsFor(metaName string) []*regexp.Regexp {
	metaContentPatternsMu.Lock()
	defer metaContentPatternsMu.Unlock()
	if cached, ok := metaContentPatternsCache[metaName]; ok {
		return cached
	}
	q := regexp.QuoteMeta(metaName)
	patterns := []*regexp.Regexp{
		// name="<metaName>" precedes content="...". Other attributes
		// (charset, lang, scheme, etc.) may sit between them.
		regexp.MustCompile(
			`(?is)<meta[\s][^>]*\bname\s*=\s*["']` + q + `["'][^>]*\bcontent\s*=\s*["']([^"']*)["']`),
		// content="..." precedes name="<metaName>".
		regexp.MustCompile(
			`(?is)<meta[\s][^>]*\bcontent\s*=\s*["']([^"']*)["'][^>]*\bname\s*=\s*["']` + q + `["']`),
	}
	if metaContentPatternsCache == nil {
		metaContentPatternsCache = make(map[string][]*regexp.Regexp)
	}
	metaContentPatternsCache[metaName] = patterns
	return patterns
}

var (
	metaContentPatternsCache map[string][]*regexp.Regexp
	metaContentPatternsMu    sync.Mutex
)

// extractGitHubURLFromString returns the canonical
// https://github.com/<owner>/<repo> URL embedded anywhere in s, or
// "" if no recognizable github URL is present.
//
// Used to lift the github source URL out of go-source's templated
// directory/file fields, which typically look like:
//
//	https://github.com/go-yaml/yaml/tree/v3.0.1{/dir}
//	https://github.com/go-yaml/yaml/blob/v3.0.1{/dir}/{file}#L{line}
//
// The function locates the "github.com/" substring, reads the next
// two path segments (stopping at /, {, }, ?, #, whitespace, or end
// of string), trims a trailing .git suffix, and returns the
// canonical URL. If the segments don't form a valid owner/repo
// pair, returns "".
func extractGitHubURLFromString(s string) string {
	_, rest, ok := strings.Cut(s, "github.com/")
	if !ok {
		return ""
	}
	owner := readPathSegment(rest)
	if owner == "" {
		return ""
	}
	if len(rest) <= len(owner) || rest[len(owner)] != '/' {
		return ""
	}
	rest = rest[len(owner)+1:]
	repo := readPathSegment(rest)
	repo = strings.TrimSuffix(repo, ".git")
	if repo == "" {
		return ""
	}
	return "https://github.com/" + owner + "/" + repo
}

// readPathSegment returns s up to (but not including) the first
// path-segment terminator: /, {, }, ?, #, or whitespace. Returns
// the entire string when no terminator is present.
func readPathSegment(s string) string {
	for i, r := range s {
		switch r {
		case '/', '{', '}', '?', '#', ' ', '\t', '\n', '\r':
			return s[:i]
		}
	}
	return s
}
