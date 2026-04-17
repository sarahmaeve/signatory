# `internal/exchange/testdata/`

Test fixtures for the exchange package.

## `atuin-schema-trial.json`

A migration of the primary-source analyst emission at
`design/analysis/atuin-schema-trial-response.json` into the v1 schema
shape. The original was the first real test of the schema; it used
the pre-revision shapes for three fields that the analyst's
meta-feedback (preserved at
`design/analysis/atuin-schema-trial-feedback.md` (the feedback narrative document stays in design/analysis/)) specifically
flagged for revision.

This file is what the original emission would look like if the
analyst had written against the post-revision v1 schema. The
migration is lossy by necessity: the revised schema encodes
distinctions the original shapes couldn't carry, so the migration
fills in reasonable defaults where the original data lacks enough
resolution to be precise.

### Migrations applied

1. **`supersedes` at top level and at Finding.F001:**
   original `["r1"]` / `["r1-ai-subsystem-threat"]` (string arrays)
   → `[{prior_id, prior_round, kind}]` structured arrays.
   Top-level uses `kind: "refines"` because the round preserved most
   round-1 findings while adding new ones. F001 uses
   `kind: "corrects"` because it explicitly revises an incorrect
   prior assessment.

2. **F003 `severity.by_context`:** original flat map
   `{"single_user": "low", "shared_host": "medium", "multi_user_windows": "medium"}`
   → array of `{context, value}` records with structured `context`.
   The `"multi_user_windows"` key split into two dimensions
   (`host_isolation: "multi_user"` + `platform: "windows"`); the
   other two keys use only the `host_isolation` dimension.

3. **MethodologyPattern `collector_hint`:** original flat strings
   `"automatable"` / `"context_dependent"` / `"requires_reasoning"`
   → multi-axis `{grep_precision, reasoning_depth}` structs:
   - `automatable` → `{high, none}`
   - `context_dependent` → `{narrows, one_hop}`
   - `requires_reasoning` → `{useless, multi_hop}`

   `miss_mode` is omitted because the original emission didn't carry
   that information — synthesizing it from false_positive_notes would
   be guesswork.

4. **PositiveAbsence citations:** original used `line_start: 1` as a
   fudge for "I ripgrep'd the whole crate" (the analyst's own
   complaint in §5 of the trial feedback). Migrated to scope-based
   citations (`scope: {kind: "crate", path: "..."}`) with `Path`
   omitted since `Scope` now carries the path.

5. **`MethodologyPattern.hit_on_atuin` → `hit_on_target`:** post-trial
   field rename to remove engagement-specific naming. The semantics
   are unchanged — the field still records whether the pattern
   surfaced a finding on the target being analyzed. The name
   `hit_on_atuin` was a known schema quirk during the atuin and
   thefuck engagements (see commit history for the rename); this
   fixture and all other v1 fixtures now use the cleaned name.

### Why migrate rather than round-trip the original?

The exchange package implements the post-revision v1 schema. The
partially-migrated fixture at
`design/analysis/atuin-schema-trial-response.json` (and its
pre-schema-revision original at
`design/analysis/atuin-schema-trial-response-preschema.json`) is
preserved verbatim as the historical record of what the analyst
emitted — the "what shape did we actually produce on day 1" document.
This testdata file is the "what shape would we produce now, given
that schema iteration" artifact, used as the round-trip reference
for Go tests.

If the two ever diverge semantically (beyond the shape migrations
documented above), that's a bug in the migration and should be
reconciled.

## `thefuck-security-v1.json` and `thefuck-provenance-v1.json`

Real v1-schema analyst outputs produced against
`github.com/nvbn/thefuck` during a dogfooding engagement. Both target
the same entity (so ingest tests can assert entity-sharing behavior)
but were produced by different analyst roles — one security, one
provenance — with different conclusion sets.

Used by:
- `internal/store/analyst_output_test.go` — ingest round-trip, entity
  de-duplication, HTTPS→canonical-URI normalization.
- `internal/store/analyst_output_query_test.go` — filter behavior
  across a multi-output corpus (severity, design_intent, signal_type).
- `cmd/signatory/analyze_freshness_test.go` — freshness check and
  AnalysisDisplay JSON shape.

These fixtures were originally stored under `filestore/analysis/` in
an early iteration that mixed runtime scratch output with checked-in
test data. They were relocated here as part of the v0.1 invariants
work (see `design/v0.1-invariants.md` §"Invariant 3") so that tests
don't depend on the state of a scratch-pad directory.
