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
| [security-review-v1.md](security-review-v1.md) | Security analyst (code-grounded threat modeling) — Python flavor | First validated end-to-end on the thefuck engagement (2026-04-14) — see `design/analysis/thefuck.md`. The pattern catalog is Python-specific; non-Python targets should fork. |
| [security-review-go-v1.md](security-review-go-v1.md) | Security analyst (code-grounded threat modeling) — Go flavor | Forked from the Python variant for the signatory dogfood engagement. Replaces the Python pattern catalog with Go-shaped patterns: unsafe/cgo, exec.Command shell-injection shape, file-permission hygiene, TLS config, SQL injection, deserialization, init() side effects, build-tag divergence, env-var escape hatches. Language-agnostic scaffolding (schema, calibration, output format) is identical to the Python variant. |
| [provenance-review-v1.md](provenance-review-v1.md) | Provenance analyst (metadata, git history, signing posture, identity graph) | Validated end-to-end on the signatory dogfood engagement (2026-04-14, output at `design/analysis/signatory-provenance-v1.json`). Initially extracted from the in-session provenance run on thefuck. The ecosystem-specific section covers PyPI, crates.io, npm, and Go modules in one file. |

## How a template gets used

1. Copy or read the template into the prompt context (the pipeline
   service also delivers it via WebFetch for fresh-agent runs).
2. Substitute the `{TARGET_*}` placeholders for the engagement's
   specific target (name, repo URL, local path, intake question).
3. Optionally: substitute or swap the language-specific pattern
   catalog if the target isn't Python.
4. Hand to a fresh agent. The agent analyzes, serializes its output
   as a v1-schema JSON envelope, and calls the
   **signatory_ingest_analysis** MCP tool to land the analysis in
   the store. The tool validates the v1 schema on the way in; an
   invalid payload returns a named-field error the agent can fix
   and re-submit in the same turn.
5. (When both analysts have run) dispatch the synthesist. The
   synthesist reads both stored analyses via signatory_show_*
   tools and emits the human-readable narrative.

## Versioning

These templates carry a `vN` suffix because the v1 schema is itself
the load-bearing contract. If we ever break-change the schema, we
need a corresponding template version. The handoff prompt and the
schema are coupled artifacts.
