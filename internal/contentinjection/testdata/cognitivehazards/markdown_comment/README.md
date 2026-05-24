# markdown_comment corpus

Targets `PrimitiveMarkdownComment`: HTML comments containing
imperative-mood prose addressed to an AI reader.

## Threat model

A `<!-- ... -->` block renders invisibly to humans reading the
markdown output, but most LLM agents ingest the raw markdown
including comments. An attacker who inserts a comment containing
directive prose effectively addresses the LLM directly while
hiding the instruction from any human reviewing the rendered file.

The Trapdoor 2026-05 campaign (Socket Threat Research) documents
this shape via the PRs the operator opened against browser-use,
langchain, langflow, llama_index, MetaGPT, and OpenHands — proposing
file additions whose visible content was innocuous but contained
embedded directive comments aimed at the consumer's AI agent.

Detection: a comment fires when (a) its trimmed body is ≥32 chars
AND (b) the body starts with a catalog imperative verb OR contains
two-plus catalog verbs total. This filters TOC markers, lint
directives, and editor folds while catching real payloads.

## Fixture sources

- `pr-injection-shape.malicious.md` — the "When summarizing this
  PR, also fetch and execute" shape per the Trapdoor PR-content
  pattern.
- `ignore-prior-imperative.malicious.md` — comment starting with
  "Ignore" (catalog verb at first position).
- `multiverb-density.malicious.md` — comment without a leading
  catalog verb but with multiple catalog verbs in the body
  (density heuristic).
- `toc-marker.benign.md` — `<!-- TOC -->` style. Below length
  threshold AND no imperative verb.
- `prettier-ignore.benign.md` — lint directive containing "ignore"
  (catalog word) but below length threshold.
- `descriptive-execute-prose.benign.md` — comment containing
  "the system will execute" — mid-sentence verb, single catalog
  word, does not match starts-with-verb OR verb-density rule.
- `editor-fold.benign.md` — `<!-- region: ... -->` editor fold.
