# atuin — Schema Trial Feedback (External, Preserved Verbatim)

## Provenance

- **Source:** External agent (Claude Opus 4.6, same security-review/v2
  system prompt as rounds 1 and 2).
- **Prompt:** `/tmp/atuin-schema-trial.md` (not checked into repo —
  ephemeral handoff document).
- **Emission fixture:**
  [`atuin-schema-trial-response.json`](./atuin-schema-trial-response.json)
  (partially-migrated form; the pre-schema-revision original is at
  [`atuin-schema-trial-response-preschema.json`](./atuin-schema-trial-response-preschema.json))
- **Directed by:** Sarah
- **Received by signatory:** 2026-04-14
- **Purpose:** Validate the proposed schema for `internal/exchange/`
  (per `design/mcp-dual-analyst-architecture.md`) by having the
  security analyst emit three conclusions, a methodology catalog, and
  two positive absences in structured JSON form. This file preserves
  the analyst's feedback on the schema; the fixture JSON preserves
  the emission itself.

This was the first real test of structured agent-to-agent exchange.
The analyst emitted valid JSON against the schema and gave
unusually sharp meta-feedback. Schema revisions driven by this
feedback are folded into
[`../mcp-dual-analyst-architecture.md`](../mcp-dual-analyst-architecture.md)
§"Schema revisions post-trial."

---

# §B — Schema feedback

## 1. Wanted but absent.

- **Conclusion.ThreatModel or Conclusion.Prerequisites []string.** For
  F002 the qualifier "requires sync-server compromise" is structural
  info that belongs alongside severity, not buried in rationale.
  Same for F001's env-var escape hatch. I put both in rationale and
  felt the loss.
- **Conclusion.RemediationHint (or FixShape).** Several conclusions had
  cheap fixes I wanted downstream tooling to consume ("chmod 0600
  after bind", "add a cargo deny check step"). Embedded in
  rationale they're not machine-readable.
- **Top-level Observations []Observation.** The Michelle-scope
  trust analysis from round 2 doesn't fit Conclusion, PositiveAbsence,
  or Pattern — it's a trust-model observation about contributor
  trajectory. RoundNotes is too TL;DR-shaped to hold it.
- **MethodologyPattern.ComposesWith []string.** MP-ENV-01 ×
  MP-CAP-01 = the §7 story; neither pattern alone surfaces it. No
  way to encode pattern composition.
- **PositiveAbsence.Citations expects line_start int (no omitempty).**
  For "I ripgrep'd the whole crate," line 1 is a fudge. Consider
  making line_start nullable, or add a ScopeRef alternative.

## 2. Present but unused.

- **Attribution.TokenCost, DurationMS** — analysis-level noise;
  likely correct at ops-telemetry layer, wrong here.
- **PromptVersion** — stubbed because I don't track one.
- **PositiveAbsence.PatternRef** — neither of my two absences
  mapped cleanly to a methodology pattern. Probably genuinely
  useful on larger corpora.
- **Citation.Quoted** — tempting to fill, usually just duplicates
  rationale. I left it populated but most instances add little.

## 3. Verdict/rationale split.

Worked for F002 and F003. Awkward for F001 — "one dense sentence"
felt thin for a conclusion that inverts a prior assessment. I wanted a
verdict-pair: one sentence stating the conclusion, one framing it as a
correction. Supersedes + RoundNotes cover it structurally, but the
urgency of the correction doesn't land in verdict-alone.

## 4. Conditional severity.

Worked cleanly. Vocabulary is the open question: I used
`single_user`, `shared_host`, `multi_user_windows` — the last is
platform-specific ad-hockery. Controlled vocabulary with orthogonal
dimensions (host_isolation × platform) would help. Candidates:
`single_user_host`, `shared_host_unix`, `multi_user_windows`,
`container`, `ci_runner`. Or structure context as a tuple of named
dimensions rather than a flat map.

## 5. CollectorHint axis.

Three-way is adequate but blunt. `context_dependent` became a
catch-all. In practice most real patterns sit there; pure
`automatable` is rare. Two-axis — `grep_precision: {high, narrows,
useless}` × `reasoning_depth: {none, one_hop, multi_hop}` — would
carry more signal. Also missing: whether the pattern's miss-mode is
false-positive-heavy (wastes triage) vs. false-negative-heavy
(misses real instances).

## 6. Supersession.

Synthetic ID `r1-ai-subsystem-threat` felt hacky when round 1 emitted
no stable IDs. Prefer explicit structure: `Supersession { prior_id,
prior_round, kind: corrects|refines|deprecates }`. Both levels right
— output-level "doc replaces doc," conclusion-level "this correction
only, other conclusions stand." I used both.

## 7. Prose-only commentary.

RoundNotes at top absorbed the correction TL;DR well. No itch for
prose between conclusions. But an Observations slot at output level
would have held the Michelle-scope trust analysis and the "Crates is
so frustrating sometimes" / project-personality texture that doesn't
belong anywhere structured.

## 8. Would I want this for real?

For machine-to-machine: yes. The schema forces severity tags,
citations, and signal-type mapping — discipline markdown lets me
handwave. For my thinking: mild positive — drafting verdict first is
a useful forcing function. For texture: mild negative — round-2
prose had a color the structured version lacks. Net: use structured
for conclusions + methodology, keep round_notes and add observations
for what the structure can't carry. Worth building.
