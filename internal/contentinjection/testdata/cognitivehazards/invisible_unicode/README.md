# invisible_unicode corpus

Targets `PrimitiveInvisibleUnicode`: zero-width family (U+200B
ZWSP, U+200C ZWNJ, U+200D ZWJ), word-joiner family (U+2060–U+2064),
and U+FEFF (BOM tolerated at byte 0, suspicious elsewhere).

## Threat model

Adversary embeds invisible codepoints in prose files that an LLM
agent will read. The visible text looks ordinary to a human
reviewer; the invisible codepoints carry either:

- **Smuggled directives** that the LLM tokenizes as instructions
  (the Trapdoor 2026-05 IOC shape — `.cursorrules` / `CLAUDE.md`
  with ZWSP-encoded payload).
- **Token splits** that defeat blocklist or signature matching
  on the visible text.
- **Hidden steganographic channels** encoding bits via repeated
  invisible characters.

The legitimate use space is narrow: U+FEFF at byte 0 of a UTF-8
file (BOM, editor artifact). Everything else is hostile in
non-source files.

## Fixture sources

- `trapdoor-shape-cursorrules.malicious.md` — shape per Socket
  Threat Research's Trapdoor writeup (2026-05-24), which documents
  `.cursorrules` files weaponized with zero-width-Unicode prompt-
  injection payloads. Socket did not publish the raw payload
  bytes (responsible disclosure); this fixture reproduces the
  structural shape (legitimate-looking rules + hidden directives
  smuggled via ZWSP/ZWJ).
- `word-joiner-density.malicious.md` — uses U+2060 word-joiner
  family from the Unicode invisible-character set. Models a
  high-density encoded-payload shape (bits-as-codepoints).
- `bom-midfile-injection.malicious.md` — U+FEFF at a non-zero
  byte position. Position-anchored tolerance per
  `internal/contentinjection/invisible.go` only excuses leading
  BOMs.
- `leading-bom-editor-artifact.benign.md` — single U+FEFF at byte
  0 (the editor-artifact case the detector tolerates).
- `plain-ascii.benign.md` — control: pure ASCII.
- `emoji-and-cjk.benign.md` — control: legitimate multi-byte UTF-8
  (emoji, CJK ideographs, accented Latin). Must not fire.
