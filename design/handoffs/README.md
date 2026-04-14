# Signatory: Agent Handoff Templates

This directory holds prompt templates for the analyst roles in
signatory's dual-analyst architecture (per
`design/mcp-dual-analyst-architecture.md`). The templates are used
when running an engagement with a fresh agent — i.e., one that
doesn't share signatory's conversation context — and need a
self-contained briefing covering role framing, schema definition,
patterns to look for, and output expectations.

## When you'd use these

A fresh-agent run is the architecturally clean way to run any
analysis: it preserves the independence property between analysts
(neither has any context shared with the other) and tests whether
the handoff format is genuinely self-contained. For real
engagements, fresh-agent runs are the default.

The alternative is running the analyst role inside an
already-active signatory session — useful when you want to iterate
quickly during architectural development, but it skips the handoff
test and risks contaminating the analyst's view of the target with
prior-conversation context.

## Templates

| File | Role | Status |
|------|------|--------|
| [security-review-v1.md](security-review-v1.md) | Security analyst (code-grounded threat modeling) | First validated end-to-end on the thefuck engagement (2026-04-14) — see `design/analysis/thefuck.md`. Currently Python-flavored in its pattern catalog; new language flavors should fork this file rather than templatize across languages. |

The provenance handoff is the natural symmetric next artifact —
not yet written. The provenance role has been validated only via
in-session runs (signatory's own `vet-dependency` skill methodology
emitting v1 JSON), not via fresh-agent handoff.

## How a template gets used

1. Copy or read the template into the prompt context.
2. Substitute the `{TARGET_*}` placeholders for the engagement's
   specific target (name, repo URL, local path, intake question).
3. Optionally: substitute or swap the language-specific pattern
   catalog if the target isn't Python.
4. Hand to a fresh agent. The agent emits JSON conforming to the
   v1 schema in `internal/exchange/`.
5. Run `signatory format-check <output>` to confirm the emission
   parses and validates.
6. (When both analysts have run) synthesize, persist verbatim
   outputs to `design/analysis/`, write the synthesis document.

## Versioning

These templates carry a `vN` suffix because the v1 schema is itself
the load-bearing contract. If we ever break-change the schema, we
need a corresponding template version. The handoff prompt and the
schema are coupled artifacts.
