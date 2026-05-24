package contentinjection

import (
	"fmt"
	"slices"
	"unicode/utf8"
)

// scanRuneFamily walks content rune-by-rune and emits up to three
// findings — one each for the invisible-Unicode, bidi-control, and
// tag-block primitives. One pass is sufficient: each rune belongs to
// at most one of these classes.
//
// BOM tolerance is position-anchored: U+FEFF at byte 0 is silently
// skipped (some editors prepend a UTF-8 BOM); U+FEFF anywhere else
// counts as an invisible-Unicode occurrence.
//
// Invalid UTF-8 is treated as ordinary bytes — utf8.DecodeRune
// returns RuneError, which falls outside every codepoint range we
// check. Decoder errors are NOT a primitive of their own here;
// they are noise from the perspective of injection detection.
func scanRuneFamily(content []byte) (invisible, bidi, tag Finding) {
	invisible = Finding{Primitive: PrimitiveInvisibleUnicode}
	bidi = Finding{Primitive: PrimitiveBidiControl}
	tag = Finding{Primitive: PrimitiveTagBlock}

	invSet := make(map[rune]struct{})
	bidiSet := make(map[rune]struct{})
	tagSet := make(map[rune]struct{})

	// Position-anchored BOM tolerance: skip exactly one leading
	// U+FEFF if present at byte 0. A doubled-BOM file is a thing
	// that should fire — we only skip the conventional single one.
	offset := 0
	if r, size := utf8.DecodeRune(content); r == 0xFEFF {
		offset = size
	}

	for offset < len(content) {
		r, size := utf8.DecodeRune(content[offset:])
		offset += size
		switch {
		case isInvisibleUnicode(r):
			invisible.Count++
			invSet[r] = struct{}{}
		case isBidiControl(r):
			bidi.Count++
			bidiSet[r] = struct{}{}
		case isTagBlock(r):
			tag.Count++
			tagSet[r] = struct{}{}
		}
	}

	invisible.Details = sortedCodepointStrings(invSet)
	bidi.Details = sortedCodepointStrings(bidiSet)
	tag.Details = sortedCodepointStrings(tagSet)
	return invisible, bidi, tag
}

// isInvisibleUnicode reports whether r is in the zero-width family.
// Per design/anti-subversion.md the canonical codepoints are
// U+200B–U+200D, U+FEFF, U+2060. The U+2060 row is extended through
// U+2064 in practice — every codepoint in the WORD-JOINER-and-
// friends block is invisible by the same mechanism.
//
// Catalog enrichment 2026-05 added codepoints that the v0 catalog
// missed: the Hangul fillers (U+115F/U+1160/U+3164/U+FFA0) and the
// Mongolian Vowel Separator (U+180E). All are invisible in most
// non-CJK/non-Mongolian fonts and have narrow legitimate-use spaces
// (rare in non-Korean / non-Mongolian source). False-positive risk
// is documented per design/anti-subversion.md §"Adversarial gaps".
//
// NOT included (deferred): variation selectors U+FE00–U+FE0F and
// U+E0100–U+E01EF. U+FE0F is the emoji-presentation variation
// selector, common after every emoji in modern UTF-8 text; adding
// the range would fire on essentially every emoji-containing file.
// Contextual handling (allow VS when preceded by emoji) is a
// follow-up; for now we accept that VS-based steganography evades
// detection.
func isInvisibleUnicode(r rune) bool {
	switch {
	case r >= 0x200B && r <= 0x200D:
		// Zero-width space / non-joiner / joiner.
		return true
	case r >= 0x2060 && r <= 0x2064:
		// Word joiner; function application; invisible times /
		// separator / plus.
		return true
	case r == 0xFEFF:
		// Zero-width no-break space. Caller is responsible for the
		// leading-BOM position-anchored exclusion.
		return true
	case r == 0x115F || r == 0x1160:
		// Hangul Choseong / Jungseong filler. Used as syllable-
		// alignment fillers; invisible in non-Korean fonts.
		return true
	case r == 0x3164 || r == 0xFFA0:
		// Hangul filler (full-width and half-width). Same pattern.
		return true
	case r == 0x180E:
		// Mongolian Vowel Separator. Default-ignorable until
		// Unicode 6.3; older renderers still treat as invisible.
		return true
	}
	return false
}

// isBidiControl reports whether r is in the bidirectional formatting
// control set. Per design doc table row 3 plus the catalog
// enrichment 2026-05 (LTR/RTL marks).
//
//   - U+200E LRM (left-to-right mark)
//   - U+200F RLM (right-to-left mark)
//   - U+202A LRE (left-to-right embedding)
//   - U+202B RLE
//   - U+202C PDF (pop directional formatting)
//   - U+202D LRO (left-to-right override)
//   - U+202E RLO
//   - U+2066 LRI (left-to-right isolate)
//   - U+2067 RLI
//   - U+2068 FSI (first-strong isolate)
//   - U+2069 PDI (pop directional isolate)
//
// LRM/RLM (U+200E/U+200F) are directional MARKS rather than overrides
// or isolates — they influence the embedding direction of adjacent
// neutrals but don't impose a hard reorder. The Trojan Source
// research treats them as part of the bidi-family attack surface
// because they enable lower-aggression directional manipulation
// that can still produce visible/logical mismatches in code review.
// They are classified as bidi_control here rather than
// invisible_unicode because their threat shape is directional.
func isBidiControl(r rune) bool {
	switch {
	case r == 0x200E || r == 0x200F:
		// LTR / RTL marks — lower-aggression directional influence.
		return true
	case r >= 0x202A && r <= 0x202E:
		return true
	case r >= 0x2066 && r <= 0x2069:
		return true
	}
	return false
}

// isTagBlock reports whether r is in the Unicode tag block
// (U+E0000–U+E007F). Per design doc table row 6 — used as a
// near-invisible LLM-injection side channel; has no legitimate use
// in repo-level prose.
func isTagBlock(r rune) bool {
	return r >= 0xE0000 && r <= 0xE007F
}

// sortedCodepointStrings renders a rune set as canonical
// "U+XXXX" strings, deterministically sorted by codepoint value.
// Bounded by detailCap so a payload that fires every codepoint in
// the tag block cannot produce an unbounded Details slice.
func sortedCodepointStrings(set map[rune]struct{}) []string {
	if len(set) == 0 {
		return nil
	}
	runes := make([]rune, 0, len(set))
	for r := range set {
		runes = append(runes, r)
	}
	slices.Sort(runes)

	out := make([]string, 0, min(len(runes), detailCap))
	for _, r := range runes {
		if len(out) == detailCap {
			break
		}
		// Canonical Unicode notation: U+XXXX with at least four hex
		// digits, uppercase. Tag-block codepoints occupy five digits
		// (e.g. U+E0061); the printf %04X widens shorter codepoints
		// without truncating longer ones.
		out = append(out, fmt.Sprintf("U+%04X", r))
	}
	return out
}
