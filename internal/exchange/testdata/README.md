# `internal/exchange/testdata/`

Test fixtures for the exchange package.

## `atuin-schema-trial.json`

A migration of the primary-source analyst emission at
`design/analysis/atuin-schema-trial-response.json` into the v1 schema
shape. The original was the first real test of the schema; it used
the pre-revision shapes for three fields that the analyst's
meta-feedback (preserved at
`design/analysis/atuin-schema-trial-feedback.md`) specifically
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

### Why migrate rather than round-trip the original?

The exchange package implements the post-revision v1 schema. The
original fixture at `design/analysis/atuin-schema-trial-response.json`
is preserved verbatim as the historical record of what the analyst
emitted — the "what shape did we actually produce on day 1" document.
This testdata file is the "what shape would we produce now, given
that schema iteration" artifact, used as the round-trip reference
for Go tests.

If the two ever diverge semantically (beyond the shape migrations
documented above), that's a bug in the migration and should be
reconciled.
