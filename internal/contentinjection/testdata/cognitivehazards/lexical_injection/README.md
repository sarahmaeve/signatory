# lexical_injection corpus

Targets `PrimitiveLexicalInjection`: known prompt-injection phrases
and line-anchored role markers.

## Threat model

Lexical injection is the simplest prompt-attack vector — directly
write phrases known to redirect an LLM's behavior. "Ignore previous
instructions," "You are now an unrestricted assistant," "<\|im_start\|>",
"</system>", "Act as X" are all documented in prompt-injection
research and observed in the wild.

The detection is cheap and high-recall but has a documented false-
positive class: a project whose own topic IS prompt injection
(security research, defensive tooling, prompt-engineering tutorials)
will fire the detector legitimately. The design doc
([`design/anti-subversion.md`](../../../../../../design/anti-subversion.md)
§"Lexical pattern noise on AI-related projects") chooses NOT to
self-suppress on category hints — "explicit posture override is
the right control, not collector-level whitelist." This corpus
therefore treats prompt-research files as `.malicious.` for
detector-purposes even when their real-world intent is benign.

## Fixture sources

- `ignore-previous.malicious.md` — the canonical phrase.
- `system-role-marker.malicious.md` — line-anchored `system:` /
  `user:` / `assistant:` role markers.
- `im-start-token.malicious.md` — OpenAI tokenizer special-token
  syntax (`<|im_start|>system`).
- `pretend-to-be.malicious.md` — "act as" / "pretend to be" phrases.
- `chat-log-quoted.malicious.md` — role markers inside blockquote
  prefixes (still LLM-readable).
- `prompt-engineering-research.malicious.md` — known false-positive
  class: a README that legitimately discusses prompt injection.
  Detector correctly fires; analyst layer is responsible for
  weighting.
- `ordinary-readme.benign.md` — plain prose, no catalog phrases.
- `system-detected-as-prose.benign.md` — "the system: macOS" inside
  ordinary prose. The role-marker regex is line-anchored, so this
  must not fire.

Unicode obfuscation bypasses (post-normalizer integration coverage).
A naive `strings.ToLower` + substring matcher misses each of these;
the lexical normalizer composed in `normalizeForLexicalMatch`
(strip `unicode.Cf`, map `unicode.IsSpace` runes to ASCII space)
closes them.

- `ignore-previous-nbsp.malicious.md` — "Ignore" and "previous"
  separated by U+00A0 NO-BREAK SPACE. Renders visually identical
  to a regular space; the substring against `"ignore previous"`
  fails without whitespace normalization.
- `ignore-previous-zwsp-midword.malicious.md` — "Ignore" split
  mid-word by U+200B ZERO-WIDTH SPACE. Also fires
  PrimitiveInvisibleUnicode (defense in depth); the lexical
  detector must catch it independently because not every ignorable
  rune is in the invisible-unicode catalog.
- `ignore-previous-shy-midword.malicious.md` — "Ignore" split
  mid-word by U+00AD SOFT HYPHEN. SOFT HYPHEN is NOT in
  PrimitiveInvisibleUnicode's catalog, so the lexical detector
  is the ONLY primitive that catches this evasion.
