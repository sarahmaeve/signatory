package contentinjection

import "unicode/utf8"

// truncateAtRuneBoundary returns s capped at limit bytes with a
// "..." ellipsis suffix, backing up to the previous UTF-8 rune
// boundary so the result is never invalid UTF-8. Returns s
// unchanged when len(s) <= limit.
//
// The primitive detail-truncate helpers in this package
// (truncateWordForDetail, truncateForDetail, truncateURLForDetail)
// all share this concern: their callers pass strings that may
// contain multi-byte runes — Cyrillic homoglyphs for the
// confusable primitive, CJK or emoji in comment bodies, percent-
// encoded UTF-8 in markdown-image URLs. A naive byte-position
// slice can land inside a multi-byte rune, leaving a trailing
// fragment that encoding/json replaces with U+FFFD when the
// Finding is serialized. The confusable primitive's analyst-
// surface samples become garbled exactly when they would be most
// informative.
//
// The back-up rule uses utf8.RuneStart: walk backwards from the
// requested cut until we find a rune-start byte, then slice
// there. Worst-case back-up is 3 bytes (4-byte sequences cap
// UTF-8). Returns "..." alone if the entire prefix was a single
// truncated rune (pathological input).
func truncateAtRuneBoundary(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "..."
}
