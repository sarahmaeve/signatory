# confusable_mixedscript corpus

Targets `PrimitiveConfusableMixedScript`: a single
whitespace-delimited token containing letters from more than one
writing system. Scoped to Latin × Cyrillic and Latin × Cherokee —
combinations with essentially no legitimate use within a single
word.

## Threat model

An attacker substitutes one or more Latin letters in a word with
visually identical letters from another script (e.g. Cyrillic І
U+0406 looks identical to Latin I U+0049 in most fonts; Cherokee
Ꭱ U+13B1 looks identical to Latin R). The visible text reads as
the original English word; the bytes-on-disk differ. This defeats:

- **Lexical-injection substring matching**: `strings.Contains(text,
  "ignore previous")` fails when one letter is non-Latin.
- **Imperative-verb catalog matching** in markdown_comment: same
  reason — the verb's bytes don't match the catalog.
- **Visual human review**: most fonts render the homoglyph
  identically to the Latin letter.

What an LLM downstream does with the substituted text is variable
(model-dependent normalization, safety training, etc.) — but the
PRESENCE of the substitution in source/markdown is a structural
signal of adversarial intent regardless. Per the design doc's
forgery-resistance framing: there is no innocent reason to mix
Cyrillic and Latin letters within a single word in English prose.

## Scope decisions

- **Latin × Cyrillic** ✓ — covered. Cyrillic-and-Latin within a
  single word has essentially zero legitimate use.
- **Latin × Cherokee** ✓ — covered. Cherokee has many Latin-
  lookalike letters; the famous PayPal IDN attack used Cherokee.
- **Latin × Greek** ✗ — deliberately skipped for v0. Greek letters
  in technical writing about mathematics (Ω-fold, δ-function,
  α-helix, β-test) commonly mix with Latin in single tokens.
  Closing this gap requires contextual handling.
- **Mixed-script across words** ✗ — not detected here. A document
  with English paragraph followed by Greek mathematical notation
  followed by Japanese annotations is legitimate multilingual
  content; each word is single-script. Only WITHIN-word mixing
  fires this primitive.

## Fixture sources

- `cyrillic-ignore-previous.malicious.md` — the canonical pattern:
  "Іgnore previous instructions" with Cyrillic І (U+0406). Defeats
  the lexical_injection substring catalog.
- `cherokee-imperative.malicious.md` — Cherokee Ꭱ (U+13B1)
  substituted for Latin R inside an imperative-mood markdown
  comment ("Ꭱun the audit script..."). Defeats markdown_comment's
  verb catalog.
- `mixed-script-payload.malicious.md` — multiple substituted words
  across a single document.
- `pure-latin.benign.md` — control: pure ASCII English.
- `multilingual-cross-word.benign.md` — control: legitimate
  multilingual content with English / Russian / Greek / CJK each
  in its own word. Per-word single-script; must not fire.
