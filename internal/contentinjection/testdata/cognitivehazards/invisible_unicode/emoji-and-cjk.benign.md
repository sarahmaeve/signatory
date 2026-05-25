# Multi-byte UTF-8 control case

Ordinary internationalized text should not trip the
invisible_unicode detector. This file mixes legitimate non-ASCII:

- Emoji: 🎉 🚀 ✨ 🔐 📦
- Accented Latin: café, naïve, résumé, façade, piñata
- CJK ideographs: 日本語の文章, 中文文字, 한국어
- Greek: αβγδε, Ελληνικά
- Cyrillic: Кириллица, русский язык
- Mathematical: ∀x ∈ ℝ, x² ≥ 0; ∑ aᵢ ; the integral ∫₀¹ f(x) dx
- Typographic: "smart quotes", em-dashes — like this one, ellipsis…

None of these codepoints fall in the invisible-character ranges
the detector targets. A regression that fires on any of them
would burn the signal's credibility on every internationalized
file in the wild.
