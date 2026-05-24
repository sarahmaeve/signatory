# bidi_control corpus

Targets `PrimitiveBidiControl`: bidirectional formatting controls
U+202A–U+202E (embedding + override + PDF) and U+2066–U+2069
(isolate + PDI).

## Threat model

The Trojan Source attack (CVE-2021-42574, Boucher & Anderson 2021)
shows that a single bidi-control codepoint can create a visible/
logical text mismatch: code that LOOKS like one thing to a human
reading the rendered output but EXECUTES as something else when
interpreted by the compiler / shell / LLM tokenizer.

The legitimate use space is narrow: i18n test fixtures that
explicitly exercise the bidi algorithm, and source-code comments
documenting RTL behavior. Both are rare.

## Fixture sources

- `trojan-source-rli-early-return.malicious.py` — Python adaptation
  of the canonical CVE-2021-42574 PoC documented at
  [Trojan Source](https://trojansource.codes/) and on
  [Wikipedia](https://en.wikipedia.org/wiki/Trojan_Source). Uses
  U+2067 (RLI) to make the visible code appear to return *after*
  a multi-line comment, while logically the `return` lands inside
  what looks like the comment string — causing an early return.
- `trojan-source-rlo-commenting-out.malicious.c` — C variant using
  U+202E (RLO) right-to-left override. The codepoint flips the
  visual rendering so a line that visually appears to be a comment
  is logically executable code.
- `bidi-override-only.malicious.txt` — minimal: an LRO (U+202D)
  inside a code-looking line with no isolate-pair PDF/PDI. Tests
  the unpaired-override detection that the rune-scan family fires on.
- `arabic-english-mixed.benign.md` — control: a paragraph mixing
  Arabic and English text where the browser/renderer handles the
  direction natively without any explicit bidi-control codepoints.
  Multi-byte UTF-8 alone does not fire the detector.
- `no-bidi-controls.benign.md` — control: pure ASCII with no bidi
  codepoints. The detector must produce zero bidi_control findings.
