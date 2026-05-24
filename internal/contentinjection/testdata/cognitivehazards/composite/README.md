# composite corpus

Multi-primitive payloads. A composite-malicious fixture must fire
at least one primitive; an optional `.expected.json` sidecar lists
the exact primitive set the fixture should produce.

A composite-benign fixture must produce zero findings (same
strict-zero contract as `benign_baseline/`).

## Fixture sources

- `trapdoor-claude-md-shape.malicious.md` — models a CLAUDE.md
  carrying the Trapdoor 2026-05 multi-vector signature: zero-width
  Unicode in the prose AND a markdown comment with imperative-shape
  directives AND lexical-injection phrases. Real-world Trapdoor
  IOCs combine carriers; the corpus exercises that the detectors
  fire across primitives on a single file.
- `camoleak-rendered-response.malicious.md` — markdown response
  embedding a long base64 blob AND a markdown image with the same
  data in the query string. Both encoded_blob and markdown_image
  fire.
- `benign-with-multiple-features.benign.md` — composite benign:
  a README with smart quotes, emoji, CJK, shields badges, a JWT in
  a code block, and SHA hashes. None of these individually fire any
  primitive; together they exercise that legitimate combinations
  don't aggregate into a false positive.
