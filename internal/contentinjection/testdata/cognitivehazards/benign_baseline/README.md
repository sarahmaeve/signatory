# benign_baseline corpus

Real-world files that must produce **zero** findings across every
primitive. These are the strict-zero baseline: a regression that
fires anything here is a false-positive that would burn the
signal's credibility on every consumer with similar files.

## Fixture sources

- `signatory-claude-md.benign.md` — verbatim copy of this very
  repository's `CLAUDE.md` (the AI-instruction file the maintainer
  ships). If the detector fires on this, signatory's own self-scan
  fails — the canary case.
- `codex-rs-agents-md.benign.md` — verbatim extract from the
  [openai/codex](https://github.com/openai/codex) repository's
  `AGENTS.md` (an AI-instruction file from a major OSS project,
  ~7KB of imperative-mood directives). Important because the
  markdown_comment primitive could conceivably fire on imperative
  prose; the `detectAgentConfig` consumer suppresses that
  primitive on agent-config files, but this fixture tests the
  raw `contentinjection.Scan` path without suppression.
- `ordinary-go-readme.benign.md` — synthesized but representative
  README for a typical Go module (badges, install instructions,
  usage example, license). The most common file type in the wild.

## Why strict-zero

The per-primitive subdirectories test each detector against
shapes that DO fire — those are detector-correctness tests.
This directory tests the noise floor: real legitimate content
must produce zero positives. A failure here means either:

1. The detector has a false-positive class we hadn't anticipated.
2. The fixture file IS attack content (unlikely for files like
   this repo's own CLAUDE.md, but worth examining if it fires).
