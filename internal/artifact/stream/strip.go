package stream

import "strings"

// detectCommonTopDir returns the wrapping directory shared by every
// entry path, or "" if no such directory exists. Suffix is preserved
// (always ends in "/") so callers can do straightforward
// strings.TrimPrefix.
//
// Heuristic, not parser:
//   - Single-entry archives return "" (no commonality to detect with
//     n=1).
//   - The candidate prefix is the first slash-terminated component of
//     entries[0].Path. If every other entry starts with that exact
//     candidate, return it; otherwise return "".
//   - Entries with no "/" in their path immediately fail the test —
//     they're at archive root, so by definition there's no common
//     wrapper.
//
// Operates on Entry.Path verbatim. Suspicious paths (".." prefixed,
// absolute, NUL bytes) participate normally; the strip detection is
// orthogonal to path safety classification.
func detectCommonTopDir(entries []Entry) string {
	if len(entries) < 2 {
		return ""
	}
	slash := strings.IndexByte(entries[0].Path, '/')
	if slash < 0 {
		return ""
	}
	candidate := entries[0].Path[:slash+1]
	for _, e := range entries[1:] {
		if !strings.HasPrefix(e.Path, candidate) {
			return ""
		}
	}
	return candidate
}
