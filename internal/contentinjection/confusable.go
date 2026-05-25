package contentinjection

import (
	"strings"
	"unicode"
)

// scanConfusableMixedScript fires PrimitiveConfusableMixedScript
// when a single whitespace-delimited token contains letters from
// more than one writing system, scoped to combinations with
// essentially no legitimate within-token use:
//
//   - Latin × Cyrillic: e.g. "Іgnore" (Cyrillic І + Latin gnore).
//     The Cyrillic capital І (U+0406) is visually indistinguishable
//     from Latin I in most fonts.
//   - Latin × Cherokee: e.g. "Ꭱun" (Cherokee Ꭱ + Latin un).
//     Cherokee Ꭱ (U+13B1) is visually indistinguishable from
//     Latin R; the famous PayPal IDN attack used Cherokee
//     homoglyphs.
//
// Greek × Latin within a token is NOT detected because mathematical
// and scientific writing legitimately mixes the two ("Ω-fold",
// "δ-function", "α-helix"). Closing that gap requires contextual
// handling and is deferred — see design/anti-subversion.md
// §"Adversarial gaps".
//
// The detection scope is intentionally per-token rather than
// per-document. Multilingual documents that mix English / Russian /
// Greek / CJK across DIFFERENT words are legitimate and must not
// fire; the structural signal is mixing WITHIN a single token,
// which has no innocent explanation.
//
// Per design/anti-subversion.md §"Why this is a forgery-resistant
// signal": the bytes-on-disk are the actionable signal regardless
// of whether any specific LLM is fooled. A human reviewer skimming
// "Іgnore previous" sees the same rendering as "Ignore previous";
// detecting the substitution is what makes the hostile content
// visible to the human deciding whether to trust the project.
func scanConfusableMixedScript(content []byte) Finding {
	out := Finding{Primitive: PrimitiveConfusableMixedScript}
	seen := make(map[string]struct{})
	for word := range strings.FieldsSeq(string(content)) {
		if !isMixedScriptConfusable(word) {
			continue
		}
		out.Count++
		if _, dup := seen[word]; !dup && len(out.Details) < detailCap {
			seen[word] = struct{}{}
			out.Details = append(out.Details, truncateWordForDetail(word))
		}
	}
	return out
}

// isMixedScriptConfusable reports whether a single token contains
// Latin letters PLUS letters from Cyrillic or Cherokee. Non-letter
// codepoints (digits, punctuation, whitespace inside the token if
// any) are ignored — only letter codepoints count toward the
// script-mix determination, so tokens like "5G" (Latin + Common)
// or "user-2024" (Latin + Common + Common) do not fire.
//
// Returns false on tokens that are entirely single-script (legit
// English, legit Russian, legit Cherokee text) — even if all-Latin
// or all-Cyrillic; the structural signal is the MIX, not the
// presence of any one script.
func isMixedScriptConfusable(word string) bool {
	var sawLatin, sawCyrillic, sawCherokee bool
	for _, r := range word {
		if !unicode.IsLetter(r) {
			continue
		}
		switch {
		case unicode.Is(unicode.Latin, r):
			sawLatin = true
		case unicode.Is(unicode.Cyrillic, r):
			sawCyrillic = true
		case unicode.Is(unicode.Cherokee, r):
			sawCherokee = true
		}
	}
	if !sawLatin {
		return false
	}
	return sawCyrillic || sawCherokee
}

// truncateWordForDetail caps a single token at 40 characters with
// an ellipsis suffix. Distinct from truncateForDetail (which
// collapses whitespace runs for multi-word comment samples); a
// confusable token has no internal whitespace by construction.
func truncateWordForDetail(word string) string {
	const limit = 40
	if len(word) <= limit {
		return word
	}
	return word[:limit] + "..."
}
