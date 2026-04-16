# Signatory: External Project Analyses

This directory holds trust analyses of open-source projects that are
**not dependencies of signatory itself**. It is distinct from
`design/dogfood/`, which records posture decisions for signatory's own
dependencies.

## Why a separate directory

Signatory is a tool for doing this kind of analysis — not just for
vetting its own deps. The `design/dogfood/` directory is scoped to
"packages signatory ships against, and the posture we hold on them."
Conflating it with "arbitrary projects we ran the model against"
muddies the audit trail.

Analyses here:

- Apply the same trust model (see `design/trust-model.md` and
  `design/signals-v01.md`) as dogfood evaluations.
- Are **not required** to end in a recorded posture. "Analysis only —
  no posture recorded" is a valid terminal state. (Posture is a
  statement about a specific consuming organization's relationship to
  the dependency; we can analyze without being a consumer.)
- Double as worked examples for the eventual `signatory analyze` MCP
  tool. Rough edges in the analysis (missing signal types, role
  categories that don't fit) are feedback for the data model and the
  `vet-dependency` skill.
- Cover non-Go ecosystems. Go is signatory's implementation language
  and therefore where the dogfood starts, but the trust model is
  ecosystem-agnostic and these analyses exercise that.

## Index

| Target | Ecosystem | Role | Posture | File |
|--------|-----------|------|---------|------|
| atuin | Rust / CLI app | Development + Shell-augment + AI-agent runtime + Hosted-service coupled | Analysis only (API + local clone + 2 rounds of external security review, 2026-04-14) | [atuin.md](atuin.md) |
| thefuck | Python / CLI app | Development tool (shell-augment / command interception) | Analysis only (dual-analyst structured emission v1, 2026-04-14) | [thefuck.md](thefuck.md) |
| photon | Java / self-hosted service | Service (self-hosted geocoder) | Analysis only (single-analyst provenance, 2026-04-15) | [photon.md](photon.md) |

### Supporting primary-source documents

| File | Kind | Notes |
|------|------|-------|
| [atuin-security-review-external.md](atuin-security-review-external.md) | External security review, round 1, verbatim | Produced by a separate Claude Opus 4.6 agent with a security-focused system prompt; integrated into atuin.md §"2026-04-14 Extended (2)" |
| [atuin-security-review-external-round2.md](atuin-security-review-external-round2.md) | External security review, round 2, verbatim | Response to signatory's follow-up handoff document; includes a material self-correction of round 1, a new medium-severity sync-censorship finding, and methodology artifacts (grep catalog + positive-absence list). Integrated into atuin.md §"2026-04-14 Extended (3)" |
| [atuin-schema-trial-response.json](../../filestore/analysis/atuin-schema-trial-response.json) | Schema-validation trial emission, JSON | First real test fixture for signatory's `internal/exchange/` package. Security analyst emitted three conclusions, 12-pattern methodology catalog, and two positive absences against the proposed schema. Used by Go round-trip tests as the reference instance. |
| [atuin-schema-trial-feedback.md](atuin-schema-trial-feedback.md) | Schema-validation trial feedback, verbatim | Security analyst's meta-feedback on the schema: which fields were absent/unused, whether the verdict-rationale split works, whether structured emission is worth the overhead. Drove schema revisions in `../mcp-dual-analyst-architecture.md` §"Schema revisions post-trial." |
| [thefuck-security-v1.json](../../filestore/analysis/thefuck-security-v1.json) | Security analyst output (v1 schema, structured), verbatim modulo a documented field rename | Fresh-agent run with the security-review handoff at `/tmp/security-review-handoff.md`. 10 conclusions, 5 positive absences, 14 methodology patterns. Passes `signatory format-check`. Integrated into `thefuck.md`. The `MethodologyPattern.hit_on_atuin` → `hit_on_target` rename was applied post-engagement; original analyst emission preserved in git history. |
| [thefuck-provenance-v1.json](../../filestore/analysis/thefuck-provenance-v1.json) | Provenance analyst output (v1 schema, structured), verbatim modulo a documented field rename | Signatory provenance methodology emitting v1 JSON in parallel to the security run. 6 conclusions, 4 positive absences, 2 observations, 14 methodology patterns. Passes `signatory format-check`. Integrated into `thefuck.md`. The `MethodologyPattern.hit_on_atuin` → `hit_on_target` rename was applied post-engagement; original analyst emission preserved in git history. |
