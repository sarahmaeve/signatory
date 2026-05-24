package contentinjection

import (
	"regexp"
	"strings"
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
// Catalog substring matching is case-insensitive and uses the same
// content buffer for every entry (no per-phrase walk; the input is
// lowercased once and ToLower-stable). Role-marker matches are
// counted in addition to substring matches.
func scanLexicalInjection(content []byte) Finding {
	out := Finding{Primitive: PrimitiveLexicalInjection}
	lower := strings.ToLower(string(content))
	seen := make(map[string]struct{})

	for _, phrase := range lexicalPhraseCatalog {
		idx := 0
		for {
			next := strings.Index(lower[idx:], phrase)
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
