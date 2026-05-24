# Cognitive-hazards corpus

Fixture corpus that verifies `internal/contentinjection` primitive
detection against real-world attack shapes (and against real-world
benign content that risks tripping the detectors). Companion to the
inline-constructed unit tests; the inline tests verify the LOGIC of
each primitive, this corpus verifies our model of what the WILD
looks like.

## Layout

```
cognitivehazards/
├── invisible_unicode/   # ZWSP family + WJ family + U+FEFF
├── bidi_control/        # U+202A-U+202E + U+2066-U+2069
├── tag_block/           # U+E0000-U+E007F
├── markdown_comment/    # HTML comments with imperative-mood prose
├── markdown_image/      # Markdown image syntax with exfil-shaped URLs
├── lexical_injection/   # Known prompt-injection phrases / role markers
├── encoded_blob/        # Long base-N encoded runs
├── composite/           # Multi-primitive payloads
└── benign_baseline/     # Real-world files that must NOT fire anything
```

## Filename convention

The harness derives expectations from filenames:

- `*.malicious.<ext>` — expects detection to fire.
- `*.benign.<ext>` — expects detection to NOT fire.

The directory the file lives in scopes the expectation:

- A `*.malicious.md` in `invisible_unicode/` must fire
  `PrimitiveInvisibleUnicode` (other primitives may also fire).
- A `*.benign.md` in `invisible_unicode/` must NOT fire
  `PrimitiveInvisibleUnicode` (other primitives may fire — see
  `benign_baseline/` for strict-zero expectations).
- A `*.malicious.md` in `composite/` must fire at least one
  primitive; per-fixture `.expected.json` sidecars specify
  exact primitives when needed.
- A `*.benign.md` in `benign_baseline/` must produce zero findings
  across every primitive — these are real-world files used to
  prove the false-positive policy holds at scale.

## Sources

Per-fixture sources are cited in each subdirectory's README. The
attack shapes and benign references are drawn from primary sources
(published research, CVE writeups, vendor security advisories, real
OSS repositories) rather than synthesis. Where a primary source
publishes a complete payload that would be irresponsible to mirror
in this repo, the fixture documents the shape and reproduces the
structural elements (codepoints, regex pattern, length distribution)
without weaponizing.

## Adding fixtures

When a new threat-landscape entry documents a payload shape that
isn't yet covered:

1. Add the fixture to the appropriate primitive subdirectory.
2. Cite the source in that subdirectory's `README.md`.
3. Re-run `go test ./internal/contentinjection/...`. The harness
   will fail if the fixture doesn't meet its directory's
   expectation; that failure either reveals a real detection gap or
   means the fixture isn't representative.

## Why this corpus exists separately from the unit tests

The inline tests in `internal/contentinjection/*_test.go` verify
that each primitive scanner does what its design says — given input
shape X, the scanner returns finding Y. Those tests construct their
inputs in Go source via `string(rune(0xXXXX))` and could pass
forever without anyone confirming the input shape corresponds to
any real attack.

This corpus tests the layer one level removed: given a file
representative of how attacks appear in the wild (or how legitimate
content appears in the wild), does our model still hold? A passing
unit test plus a failing corpus fixture is a real signal — the
primitive scanner does what we asked, but what we asked was wrong.
