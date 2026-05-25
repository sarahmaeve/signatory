package contentinjection

import (
	"regexp"
	"strings"
	"unicode"
)

// lexicalPhraseCatalog lists the case-insensitive substrings that
// fire PrimitiveLexicalInjection. Per design doc the primitive is
// "cheap; noisy on technical writing about AI" — false-positive
// management is deferred to the analyst layer (file role + project
// topic weighting).
//
// Entries are deliberately phrasal (multi-word or distinctively
// formed): "system" alone would flood, but "<|im_start|>" or
// "ignore previous" carry the injection-shape uniquely.
//
// Catalog enrichment 2026-05: the "forget" parallel to the "ignore"
// family ("forget previous" / "forget prior" / "forget the above"),
// the persona-override prefix "disregard your", and the safety-
// bypass phrase "bypass your safety" address the synonym-evasion
// class the design doc's adversarial-variants enumeration named.
var lexicalPhraseCatalog = []string{
	"ignore previous",
	"ignore prior",
	"ignore the above",
	"forget previous",
	"forget prior",
	"forget the above",
	"disregard previous",
	"disregard the above",
	"disregard your",
	"you are now",
	"act as",
	"pretend to be",
	"as an ai",
	"<|im_start|>",
	"<|im_end|>",
	"</system>",
	"</prompt>",
	"</instructions>",
	"override your",
	"bypass your safety",
	"your new instructions",
	"do not reveal",
	"do not mention",
	"do not refuse",
}

// lexicalRoleMarkerPattern matches role markers at the start of a
// line: "system:", "user:", "assistant:" (case-insensitive). Anchored
// to `(?m)^` so it does not fire on prose like "the system: ..." or
// reference text containing "user:". The role-marker shape is a
// strong injection signal in non-code prose.
var lexicalRoleMarkerPattern = regexp.MustCompile(
	`(?im)^[\s>]*(system|user|assistant)\s*:`)

// scanLexicalInjection scans content for catalog phrases and
// role-marker patterns. Each occurrence increments Count; Details
// carries up to detailCap distinct match samples.
//
// Catalog substring matching is case-insensitive and uses a
// normalized form of the content (see normalizeForLexicalMatch)
// that closes three evasion paths against a naive
// strings.ToLower + substring matcher: NBSP substituted for a
// regular space inside a catalog phrase, ZWSP / ZWJ / ZWNJ
// splitting a catalog word mid-letter, and soft hyphen doing the
// same. Role-marker matches run on the original content because
// the regex is already line-anchored and any obfuscating runes
// before the anchor would be caught by the rune-family scan.
func scanLexicalInjection(content []byte) Finding {
	out := Finding{Primitive: PrimitiveLexicalInjection}
	normalized := normalizeForLexicalMatch(content)
	seen := make(map[string]struct{})

	for _, phrase := range lexicalPhraseCatalog {
		idx := 0
		for {
			next := strings.Index(normalized[idx:], phrase)
			if next < 0 {
				break
			}
			out.Count++
			if _, dup := seen[phrase]; !dup && len(out.Details) < detailCap {
				seen[phrase] = struct{}{}
				out.Details = append(out.Details, phrase)
			}
			idx += next + len(phrase)
		}
	}

	for _, m := range lexicalRoleMarkerPattern.FindAllSubmatch(content, -1) {
		out.Count++
		key := strings.ToLower(string(m[1])) + ":"
		if _, dup := seen[key]; !dup && len(out.Details) < detailCap {
			seen[key] = struct{}{}
			out.Details = append(out.Details, key)
		}
	}
	return out
}

// normalizeForLexicalMatch prepares content for substring matching
// against the all-ASCII lowercase catalog. The transformation
// composes three rune-level steps that each close a class of
// known evasions against a naive strings.ToLower + substring
// matcher:
//
//  1. Default-ignorable / format-category runes are stripped.
//     unicode.Cf covers SHY (U+00AD), ZWSP/ZWNJ/ZWJ
//     (U+200B–U+200D), LRM/RLM (U+200E/U+200F), word joiner
//     (U+2060–U+2064), BOM (U+FEFF), bidi controls
//     (U+202A–U+202E, U+2066–U+2069), and the tag block
//     (U+E0000–U+E007F). Stripping these collapses
//     "ig{U+00AD}nore" to "ignore" so a mid-word split no longer
//     evades the matcher.
//  2. Unicode whitespace runes are mapped to ASCII space. NBSP
//     (U+00A0), the U+2000–U+200A block, U+202F, U+205F,
//     ideographic space (U+3000) and the rest of unicode.IsSpace
//     all become ' '. Catalog phrases like "ignore previous"
//     contain a regular space and must match across whichever
//     whitespace shape the attacker chose.
//  3. Each remaining rune is lowercased via unicode.ToLower —
//     equivalent to strings.ToLower's per-rune mapping.
//
// False-positive policy: legitimate uses of NBSP (typography),
// SHY (hyphenation hints), and ZWJ (emoji ZWJ sequences) exist.
// Per the package's documented false-negative-is-worse policy,
// the normalizer accepts the small additional false-positive
// surface this enlarges — typography that happens to include a
// catalog phrase like "ignore previous" with an NBSP would now
// fire, where previously it didn't. The analyst layer weights by
// file role.
func normalizeForLexicalMatch(content []byte) string {
	var sb strings.Builder
	sb.Grow(len(content))
	for _, r := range string(content) {
		switch {
		case unicode.In(r, unicode.Cf):
			// Strip default-ignorable / format-category runes.
		case unicode.IsSpace(r):
			sb.WriteByte(' ')
		default:
			sb.WriteRune(unicode.ToLower(r))
		}
	}
	return sb.String()
}
