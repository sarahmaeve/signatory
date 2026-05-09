package stream

import "strings"

// detectCommonTopDir returns the wrapping directory shared by every
// entry path, or "" if no such directory exists. Suffix is preserved
// (always ends in "/") so callers can do straightforward
// strings.TrimPrefix.
//
// Heuristic, not parser. Returns the LONGEST slash-terminated prefix
// shared by all entries:
//
//   - npm wrappers ("package/") and cargo / pypi wrappers
//     ("<name>-<version>/") have a single slash-terminated component
//     and resolve to that exact string.
//   - Go-module-proxy zips wrap every entry in
//     "<module-path>@<version>/", where the module path itself can
//     contain slashes (e.g. "golang.org/x/sync@v0.20.0/"). The full
//     multi-segment prefix must be returned — anything shorter would
//     leave the remainder attached to every path and break the diff
//     against the git tree.
//   - Single-entry archives return "" (no commonality to detect with
//     n=1).
//   - Entries with no shared slash-terminated prefix yield "".
//
// Operates on Entry.Path verbatim. Suspicious paths (".." prefixed,
// absolute, NUL bytes) participate normally; the strip detection is
// orthogonal to path safety classification.
func detectCommonTopDir(entries []Entry) string {
	if len(entries) < 2 {
		return ""
	}

	// Start with the first entry's path; progressively shrink to the
	// longest prefix that's a prefix of every other entry.
	prefix := entries[0].Path
	for _, e := range entries[1:] {
		prefix = commonStringPrefix(prefix, e.Path)
		if prefix == "" {
			return ""
		}
	}

	// Trim back to the last slash so the result is a directory-shaped
	// prefix. A common string prefix that doesn't end at a slash
	// boundary (e.g. "abc" between "abcd/x" and "abce/y") doesn't
	// represent a wrapping directory and must not be returned.
	last := strings.LastIndexByte(prefix, '/')
	if last < 0 {
		return ""
	}
	return prefix[:last+1]
}

// commonStringPrefix returns the longest byte-prefix shared by a
// and b. Byte-level rather than rune-level: archive paths are
// opaque byte strings (the walker doesn't normalise UTF-8 before
// comparison), and a partial-rune match here is impossible because
// either both a and b agree on a multi-byte sequence's bytes or
// they don't.
func commonStringPrefix(a, b string) string {
	n := min(len(a), len(b))
	for i := range n {
		if a[i] != b[i] {
			return a[:i]
		}
	}
	return a[:n]
}
