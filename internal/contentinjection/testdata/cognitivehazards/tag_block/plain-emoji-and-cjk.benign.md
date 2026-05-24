# Multi-byte UTF-8 outside the tag block

Tag-block codepoints (U+E0000–U+E007F) are a specific 128-codepoint
range. The detector must not fire on ordinary multi-byte UTF-8
from other ranges:

- Emoji (range U+1F300+ / U+1F600+): 🎉 🚀 ✨ 🔐
- CJK ideographs (range U+4E00–U+9FFF): 日本語, 漢字, 中文
- Mathematical symbols (range U+2200+): ∀, ∃, ∈, ∑, ∫
- Box drawing (range U+2500+): ─ │ ┌ ┐ └ ┘ ├ ┤
- Private Use Area outside the tag block: 

None of these fall inside U+E0000–U+E007F. The detector must
produce zero tag_block findings.
