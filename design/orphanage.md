# Orphan-prevention audit: data-integrity guardrails for v0.1

Status: Phase 0 in progress (started 2026-04-24). Plan captured
from the 2026-04-24 burndown scoping conversation.

Cross-references:
- `design/v0.1-invariants.md` §Invariant 3 — SQLite as canonical
  store. The invariant is load-bearing only if the store is
  actually consistent.
- `MEMORY.md` §"TDD for security fixes" — write failing test
  BEFORE fixing; proven method for integrity bugs. The audit
  follows this pattern explicitly.
- `design/agent-facing-contract.md` §"M2" — identity-indexed
  storage; introduces `analyst_outputs.collected_from_entity_id`
  which is one of the FK-shaped columns in scope.

## Frame

This is a **data-integrity audit followed by a fix**, not a single
implementation task. The audit is the load-bearing part: we don't
know the shape of the fix until we know what's broken.

Root cause (per user scoping, 2026-04-24): "lack of FK references
in the database and our process." Two layers of defense missing:

1. **Schema layer** — foreign key constraints that SQLite should
   reject at write time.
2. **Code layer** — ingest-time validation that rejects before
   reaching the write.

A proper fix adds both. The audit has to inventory both sides
before touching anything.

## Why this is P0 for v0.1

- v0.1 Invariant 3 names SQLite as the canonical store. Trust in
  that canonicalization erodes the moment `postures` and `burns`
  can reference entities that never existed or have been silently
  replaced.
- Agent-dispatched ingest runs against URIs whose existence isn't
  always pre-verified. `signatory_ingest_analysis` is the
  write-side boundary between "analyst says this is about X" and
  "store records this is about X." A silent orphan here means a
  trust decision gets recorded against nothing.
- Cheapest to fix before v0.1 flip. After the flip, orphan-
  carrying stores proliferate into users' hands; migrations get
  socially expensive.

## SQLite pitfalls to flag up front

Before anything else, two SQLite-specific facts shape the work:

### FK enforcement is off by default

**SQLite does not enforce foreign keys by default.** Every
connection has to run `PRAGMA foreign_keys = ON` or FK
constraints in the schema are advisory comments. This is the
single most important diagnostic question, and the answer changes
the audit's expected scope.

- Pragma on: the audit finds FK-shaped columns without
  `REFERENCES`. Closable individually.
- Pragma off: every FK we think we have is already not enforced.
  Audit scope expands to "every write path is currently capable
  of orphan production."

### ALTER TABLE cannot add FKs in-place

SQLite's `ALTER TABLE` can add columns but cannot add FK
constraints to existing ones. Adding a FK requires the rename-
recreate-copy-drop pattern (or PRAGMA `writable_schema=ON` for
the brave). This means **every schema-level fix is a new
migration**, not an in-place edit. Migration count grows with FK
count.

## Phase 0: Baseline (~30 min)

Confirmation before scoping. Three questions:

1. Is `PRAGMA foreign_keys = ON` set? Per-connection? At open only?
2. How many `REFERENCES` clauses exist across current migrations?
3. How many `_id`-suffixed columns exist across tables?

Delta between (2) and (3) is the naïve upper bound on missing
FK constraints. The ratio tells us whether we're in "a few holes
to close" territory or "FKs are universally unenforced"
territory.

**Deliverable:** a one-paragraph baseline in this doc under
§"Phase 0 findings" below.

## Phase 1 findings (2026-04-24)

Delegated to a Sonnet agent; results reviewed and integrated by
the orchestrator. Agent produced the column-by-column
classification below after reading every `CREATE TABLE` and
`ALTER TABLE` in `internal/store/migrate.go` end-to-end.

### Full FK-shaped column catalog

| Table | Column | Type | NULL? | Default | REFERENCES | ON DELETE | Classification | Notes |
|---|---|---|---|---|---|---|---|---|
| `entities` | `id` | TEXT PK | NO | — | — | — | — | PK; referenced by others, not itself a FK |
| `signals` | `entity_id` | TEXT | NO | — | `entities(id)` | — | FK-enforced | Original v1 schema |
| `postures` | `entity_id` | TEXT PK | NO | — | `entities(id)` | — | FK-enforced | v1 PK; re-declared in v2 rebuild with same FK |
| `burns` | `entity_id` | TEXT PK | NO | — | `entities(id)` | — | FK-enforced | v1 PK |
| `dependency_observations` | `project_id` | TEXT | NO | — | `entities(id)` | — | FK-enforced | v2 |
| `dependency_observations` | `entity_id` | TEXT | NO | — | `entities(id)` | — | FK-enforced | v2 |
| `dependency_observations` | `survey_id` | TEXT | NO | — | — | — | FK-missing | Index exists (`idx_depobs_survey`), NOT NULL, no REFERENCES; surveys table doesn't exist in schema — likely a free-form external survey token. Phase 2 must resolve target table. ORPHAN RISK if surveys are ever persisted. |
| `signal_resolutions` | `entity_id` | TEXT | NO | — | — | — | FK-missing | NOT NULL; every other `entity_id` in the schema has `REFERENCES entities(id)`; index exists. Out-of-pattern. ORPHAN RISK. |
| `signal_resolutions` | `kept_signal_id` | TEXT | NO | — | `signals(id)` | — | FK-enforced | v2 |
| `signal_resolutions` | `superseded_signal_id` | TEXT | NO | — | `signals(id)` | — | FK-enforced | v2 |
| `audit_log` | `entity_id` | TEXT | YES | NULL | — | — | legit-nullable-audit | Nullable by design; auditing events that don't touch an entity (e.g. session start) is legitimate. Missing FK is a deliberate choice, not an oversight. |
| `signal_evidence` | `signal_id` | TEXT | NO | — | `signals(id)` | — | FK-enforced | v4 |
| `analyst_outputs` | `entity_id` | TEXT | NO | — | `entities(id)` | — | FK-enforced | v4 |
| `analyst_outputs` | `analyst_id` | TEXT | NO | — | — | — | legit-freeform | Free-form analyst name string (e.g. `signatory-security-v1`), not a row ID in any table. |
| `analyst_outputs` | `collected_from_entity_id` | TEXT | YES | NULL | `entities(id)` | — | FK-enforced | v7; nullable (NULL = "no resolution hop"). |
| `analyst_outputs` | `analysis_session_id` | TEXT | YES | NULL | `analysis_sessions(id)` | SET NULL | FK-enforced | v11 ALTER TABLE; nullable FK with ON DELETE SET NULL. |
| `conclusions` | `output_id` | TEXT | NO | — | `analyst_outputs(id)` | — | FK-enforced | v4 (as `findings`), renamed v5 |
| `conclusions` | `conclusion_local_id` | TEXT | NO | — | — | — | legit-composite | Local position identifier within `(output_id, conclusion_local_id)` composite key. |
| `conclusion_severity_contexts` | `conclusion_id` | TEXT | NO | — | `conclusions(id)` | — | FK-enforced | v4 (renamed v5) |
| `conclusion_supersedes` | `conclusion_id` | TEXT | NO | — | `conclusions(id)` | — | FK-enforced | v4 (renamed v5) |
| `conclusion_supersedes` | `prior_id` | TEXT | NO | — | — | — | legit-freeform | Cross-round reference string ("prior round N conclusion ID"); intentionally loose — the prior may not exist in this store. Not a same-table self-FK. |
| `conclusion_prerequisites` | `conclusion_id` | TEXT | NO | — | `conclusions(id)` | — | FK-enforced | v4 (renamed v5) |
| `conclusion_remediation_hints` | `conclusion_id` | TEXT | NO | — | `conclusions(id)` | — | FK-enforced | v4 (renamed v5) |
| `conclusion_related` | `conclusion_id` | TEXT | NO | — | `conclusions(id)` | — | FK-enforced | v4 (renamed v5) |
| `conclusion_related` | `related_id` | TEXT | NO | — | — | — | legit-freeform | Cross-reference string to another conclusion in the same output; `UNIQUE(conclusion_id, related_id)` pair but no FK — same-output pairing is enforced logically, not relationally. Deliberate. |
| `positive_absences` | `output_id` | TEXT | NO | — | `analyst_outputs(id)` | — | FK-enforced | v4 |
| `observations` | `output_id` | TEXT | NO | — | `analyst_outputs(id)` | — | FK-enforced | v4 |
| `observations` | `observation_local_id` | TEXT | NO | — | — | — | legit-composite | Local position identifier within `(output_id, observation_local_id)` composite key. |
| `methodology_catalogs` | `output_id` | TEXT PK | — | — | `analyst_outputs(id)` | — | FK-enforced | v4; PK is the FK. |
| `methodology_catalogs` | `source_analyst_id` | TEXT | NO | — | — | — | legit-freeform | Free-form analyst name string, same pattern as `analyst_outputs.analyst_id`. |
| `methodology_patterns` | `output_id` | TEXT | NO | — | `analyst_outputs(id)` | — | FK-enforced | v4 |
| `methodology_patterns` | `pattern_local_id` | TEXT | NO | — | — | — | legit-composite | Local position identifier within `(output_id, pattern_local_id)` composite key. |
| `methodology_pattern_composes` | `pattern_id` | TEXT | NO | — | `methodology_patterns(id)` | — | FK-enforced | v4 |
| `methodology_pattern_composes` | `composes_with` | TEXT | NO | — | — | — | legit-freeform | References a pattern by local string identity; no FK because the target pattern may be in a different output. Intentional cross-output loose coupling. |
| `citations` | `parent_id` | TEXT | NO | — | — | — | needs-investigation | Polymorphic FK: routes to `conclusions`, `positive_absences`, or `observations` depending on `parent_kind`. SQLite cannot express polymorphic `REFERENCES`; the v9 CHECK on `parent_kind` is the partial guard. Write-path validation needed. |
| `output_supersedes` | `output_id` | TEXT | NO | — | `analyst_outputs(id)` | — | FK-enforced | v4 |
| `output_supersedes` | `prior_id` | TEXT | NO | — | — | — | legit-freeform | References a prior output by round-scoped string; deliberately loose — prior outputs may not be in this store (cross-store, imported analyses). |
| `output_reframes_from` | `output_id` | TEXT | NO | — | `analyst_outputs(id)` | — | FK-enforced | v4 |
| `analysis_sessions` | `entity_id` | TEXT | NO | — | `entities(id)` | — | FK-enforced | v11 |
| `analysis_sessions` | `pipeline_session_id` | TEXT | NO | `''` | — | — | FK-missing-sentinel | NOT NULL with `DEFAULT ''`. The empty-string default admits rows that look like they name a pipeline session but match nothing. No `pipeline_sessions` table exists in the main store schema; the pipeline store is separate. If this is truly a cross-store ID then FK is impossible by design and the sentinel is appropriate; if the column is meant to reference a row it needs a schema note. ORPHAN RISK (sentinel shape). |
| `analysis_sessions` | `synthesis_output_id` | TEXT | YES | NULL | `analyst_outputs(id)` | SET NULL | FK-enforced | v11; nullable with ON DELETE SET NULL. |

### Summary

**Total rows in table: 40** (40 unique columns; one column,
`analyst_outputs.analysis_session_id`, belongs to both the
analyst_outputs table and the v11 ALTER group — listed once).

| Classification | Count |
|---|---|
| FK-enforced | 27 |
| FK-missing | 2 |
| FK-missing-sentinel | 1 |
| legit-composite | 4 |
| legit-freeform | 6 |
| legit-nullable-audit | 1 |
| needs-investigation | 1 |

**ORPHAN RISK columns (Phase 3 targets):**

1. `signal_resolutions.entity_id` — NOT NULL, no REFERENCES,
   out of pattern with every other `entity_id`.
2. `dependency_observations.survey_id` — NOT NULL, indexed, no
   REFERENCES; target table unknown from schema alone.
3. `analysis_sessions.pipeline_session_id` — NOT NULL DEFAULT
   '', cross-store ID, empty-string sentinel admits
   unverifiable values.
4. `citations.parent_id` — polymorphic; no REFERENCES possible
   in SQLite, but write-path may insert mismatched
   `parent_id`/`parent_kind` pairs silently.

### Flagged observations

**1. `signal_resolutions.entity_id` is the clearest oversight.**
Every other `entity_id` in the schema — `signals`, `postures`,
`burns`, `dependency_observations` (×2), `analyst_outputs`,
`analysis_sessions` — carries `REFERENCES entities(id)`.
`signal_resolutions.entity_id` is the sole exception, with an
index but no FK. Given this table is also append-only (trigger
from v3), an orphan row here can never be corrected after
insert. Highest-confidence bug.

**2. `dependency_observations.survey_id` was flagged in Phase 0
and remains unresolved.** No `surveys` table exists anywhere in
the main store schema. The column is NOT NULL with an index,
suggesting it's used as a lookup key — but what it references
is not declared. Two possibilities: (a) it's a free-form
external token (e.g. a git SHA or an opaque survey identifier
from a calling tool), in which case it's `legit-freeform` and
the classification is conservative; (b) surveys are expected to
eventually be persisted, in which case inserting without a
parent already produces orphan-shaped rows. Phase 2 write-path
inspection will resolve this.

**3. `analysis_sessions.pipeline_session_id` DEFAULT '' is a
cross-store sentinel.** The v11 migration comment explicitly
documents that `analysis_sessions` is "decoupled from the
ephemeral pipeline_sessions relay." The pipeline store is a
separate SQLite file; cross-file FK is not possible in SQLite.
So no `REFERENCES` is technically correct here. What is worth
flagging: the DEFAULT '' means any row created without a
pipeline session gets an empty string rather than NULL, making
"was this session pipeline-dispatched?" ambiguous at the SQL
layer. A NULL default would be cleaner semantics and matches
how `collected_from_entity_id` handles the optional-reference
case. This is a style/clarity issue rather than an integrity
gap, but it's the same sentinel pattern that caused the F002
citation bug (v9 fixed a different silent-bypass via empty
values).

**4. `citations.parent_id` is a known polymorphic-FK
limitation.** The v4 comment acknowledges this: "polymorphic FK
via parent_kind + parent_id." The v9 CHECK on `parent_kind` is
the half-guard — it ensures the discriminator is valid but
cannot guarantee `parent_id` resolves. This is a SQLite
architectural limitation, not an oversight; the correct fix is
write-path validation (Phase 2). It's flagged
`needs-investigation` because the severity depends on whether
the ingest path validates `parent_id` existence against the
declared `parent_kind` table before inserting.

**5. Columns added via `ALTER TABLE` with `REFERENCES` vs.
without:**

- v7 `ALTER TABLE analyst_outputs ADD COLUMN
  collected_from_entity_id TEXT REFERENCES entities(id)` — FK
  added correctly in the ALTER.
- v11 `ALTER TABLE analyst_outputs ADD COLUMN
  analysis_session_id TEXT REFERENCES analysis_sessions(id) ON
  DELETE SET NULL` — FK added correctly.
- No ALTER TABLE in the schema adds a `_id` column without a
  `REFERENCES` clause (the non-FK `_id` columns were all added
  in original `CREATE TABLE` blocks). The ALTER TABLE
  discipline is clean; the gaps are all in original CREATE
  TABLE definitions.

**6. The `prior_id` pattern is consistent but worth
documenting.** `conclusion_supersedes.prior_id` and
`output_supersedes.prior_id` both intentionally omit
`REFERENCES` because prior-round artifacts may not be present
in the current store (analyst re-runs across sessions, imported
analyses). This is a deliberate loose-coupling choice, but it
means the supersession chain is unverifiable at the schema
layer. For v0.1 dogfood it's acceptable; at v1.0 scale, a
within-store supersession should probably be enforced.

### Orchestrator notes

- Phase 1 confirms Phase 0's hypothesis: the schema-layer
  picture is cleaner than feared. The FK-enforced majority
  (27 of 40) plus three genuinely-unverifiable but
  deliberately-loose columns (the `prior_id`/`related_id`
  family) means the audit's real work is concentrated on the 4
  ORPHAN RISK columns, not a schema-wide rewrite.
- The polymorphic-FK case at `citations.parent_id` is the most
  interesting. Phase 2 needs to specifically trace the citation
  ingest path — if `parent_id` + `parent_kind` are validated as
  a pair before insert, the integrity property holds despite
  no `REFERENCES`. If not, this is a distinct bug class from
  the other three.
- The `pipeline_session_id` empty-string sentinel isn't an
  orphan in the traditional sense (it's an optional cross-store
  reference) but it IS a style inconsistency with how the rest
  of the schema handles optional refs (via NULL). Phase 4 can
  decide whether to fold a "sentinel → NULL" fix into the
  audit's fix commits or leave as v0.2 cleanup.
- The `dependency_observations.survey_id` case is the one that
  genuinely needs Phase 2 to resolve. If the write-path
  inspection shows `survey_id` is never cross-referenced as a
  lookup key, it may actually belong in the `legit-freeform`
  bucket and the audit drops to 3 risk columns.

## Phase 1: Schema inventory (~1 session)

Walk every migration and produce a catalog:

| Table | Column | Type | NULL? | REFERENCES? | ON DELETE | Notes |
|---|---|---|---|---|---|---|
| `postures` | `entity_id` | TEXT | ??? | ??? | ??? | |
| `burns` | `entity_id` | TEXT | ??? | ??? | ??? | |
| `signals` | `entity_id` | TEXT | ??? | ??? | ??? | |
| `analyst_outputs` | `primary_target_entity_id` | TEXT | ??? | ??? | ??? | M2 |
| `analyst_outputs` | `collected_from_entity_id` | TEXT | ??? | ??? | ??? | M2 |
| `dependency_observations` | `project_id`, `entity_id`, `survey_id` | ... | | | | |
| … (every `_id` column across the schema) | | | | | | |

Tables known to exist (from prior code reading): `entities`,
`postures`, `burns`, `signals`, `analyst_outputs`,
`dependency_observations`, `projects`, `surveys`, `audit_log`,
`analyst_output_withdrawals` (if shipped), pipeline-session
tables.

**Deliverable:** this catalog, committed into this doc or a
sibling migration-reference doc. Every subsequent phase refers
back to it.

## Phase 2 findings (2026-04-24)

Delegated to four Sonnet agents in parallel, one per Phase-1
ORPHAN RISK column. Results reviewed and integrated by the
orchestrator.

### Executive summary

**The orphan-risk set collapses from 4 columns to 1.** Three of
Phase 1's suspicious columns turn out to be legitimate on write-
path inspection, and only one — `signal_resolutions.entity_id`
— is a confirmed bug. Phase 3's target set is one test, not
four.

| Phase 1 classification | Phase 2 reality | Column |
|---|---|---|
| FK-missing | CONFIRMED bug | `signal_resolutions.entity_id` |
| FK-missing | **Reclassify** → `legit-freeform` | `dependency_observations.survey_id` |
| FK-missing-sentinel | **Reclassify** → `legit-freeform` (+ style cleanup) | `analysis_sessions.pipeline_session_id` |
| needs-investigation | **Reclassify** → `legit-polymorphic` | `citations.parent_id` |

### Column 1: `signal_resolutions.entity_id` — CONFIRMED bug

Single write path: `store.(*SQLite).AppendResolution` at
`internal/store/sqlite.go:700`. Takes `r.EntityID` from the
caller-supplied struct, performs only a non-empty-string check,
does no `SELECT` against `entities`, does no `PutEntity`.
Classification: **unchecked**.

Reachability: the `store.Store` interface is public, so any
caller with a store handle can land an orphan. Currently, the
ONLY production code path calling `AppendResolution` is… none
— the function is implemented and tested but not yet wired to
any CLI command or MCP handler. Test fixtures are the sole
callers today. This means:

- **No real dogfood store has an orphan here yet.** The bug is
  a latent gap — the landmine is in place, no one has stepped
  on it.
- **Fix should land before the function is wired.** Adding the
  FK now is zero-risk to production data; adding it after
  wiring means a migration against real rows.

Secondary finding: `AppendResolution` also doesn't validate
cross-entity consistency among `entity_id`, `kept_signal_id`,
and `superseded_signal_id`. That's a documented limitation
(`sqlite_security_test.go:136`) and a separate integrity gap,
not in scope for this audit but worth noting.

**Phase 3 test recipe** (directly from agent):

1. `s := newTestDB(t)` (helper in `internal/store/sqlite_test.go`).
2. Insert two signals with a real, seeded entity — satisfies
   `kept_signal_id` / `superseded_signal_id` FKs which ARE
   enforced.
3. Call `s.AppendResolution(ctx, &profile.SignalResolution{
   ID: "orphan-res-1", EntityID: "ghost-entity-id-99999", ... })`
   where `ghost-entity-id-99999` is never in `entities`.
4. Assert `require.NoError(t, err)` — documents the current bug.
   Test inverts to `require.Error` + `ErrorIs` once the fix
   lands.
5. Assert orphan is observable:
   `SELECT COUNT(*) FROM signal_resolutions WHERE entity_id =
   'ghost-entity-id-99999'` returns 1.
6. Assert parent is absent: same count for `entities` returns 0.

Name: `TestOrphan_AppendResolution_EntityID`.

### Column 2: `dependency_observations.survey_id` — reclassify as `legit-freeform`

Agent traced every call path and found:

- `AppendDependencyObservations` has **zero non-test callers**
  in the codebase.
- `internal/survey` (the CLI `signatory survey` command) is
  explicitly read-only; it resolves tiers but never writes
  observations.
- `signatory_survey` MCP tool is the stub documented in
  `design/potential-survey-mcp.md` — returns `CodeNotFound`
  before any store call.
- No `surveys` table is declared or referenced anywhere.
- `GetLatestDependencies` uses `survey_id` as a within-table
  deduplication cursor: "pick the most-recent survey_id for a
  project, then fetch all rows for that token." A self-
  referential grouping pattern, not a cross-table FK.

Semantically identical to `analyst_outputs.analyst_id` (already
classified `legit-freeform`): an opaque caller-assigned token,
no parent table, no lookup intended.

Phase 1 correctly flagged the index as suspicious, but the
index exists for the within-table `WHERE survey_id = ?`
deduplication lookup, not as evidence of an external parent.

**Action:** update Phase 1 classification for this column to
`legit-freeform`; drop from ORPHAN RISK set; no Phase 3 test.

### Column 3: `analysis_sessions.pipeline_session_id` — reclassify as `legit-freeform` (+ style cleanup recommendation)

Agent found a single write path
(`CreateAnalysisSession`) that takes the value directly from a
CLI flag (`--pipeline-session-id`) without validation. But the
load-bearing finding is about READ paths:

- **No code queries `WHERE pipeline_session_id = ?` anywhere.**
- The value is read back only as part of full-row `SELECT *`
  projections and displayed in the `analysis show` renderer.
- The `idx_analysis_sessions_pipeline` index exists in the
  schema but is never exercised.

So the column is **write-and-display-only**. It's a forensic
audit label, not a relational key. A stale/typo'd value causes
no integrity violation because nothing resolves it.

Integrity verdict: **not a bug.**

Style verdict: **still worth a small cleanup.** The `NOT NULL
DEFAULT ''` sentinel is out of pattern with every other
optional-reference column in the schema (`collected_from_entity_id`,
`analysis_session_id`, `synthesis_output_id` all use `NULL`).
The empty-string makes "intentionally no relay" vs. "relay ID
not supplied" ambiguous at the SQL layer.

**Action:** update Phase 1 classification to `legit-freeform`;
drop from ORPHAN RISK set. Document the NULL-vs-empty-string
style fix as a sidecar v12 migration — feasible to fold into the
orphan-audit fix commits, or defer as v0.2 cleanup. Orchestrator
lean: fold in, since we're touching migrations anyway.

### Column 4: `citations.parent_id` — reclassify as `legit-polymorphic`

Agent traced the polymorphic citation ingest flow and found
the structural guarantee Phase 1 couldn't infer from schema
alone:

- Three call sites into `insertCitations`: from
  `insertConclusions`, `insertPositiveAbsences`,
  `insertObservations` (all in
  `internal/store/analyst_output.go`).
- Each site: `parent_kind` is a **hardcoded string literal**
  matching the parent table; `parent_id` is a **freshly-
  generated UUID** from the immediately-prior parent-row
  INSERT in the same transaction.
- **No call path takes `parent_id` from analyst-supplied
  JSON.** The `Citation` struct in `exchange/types.go` has
  neither `ParentID` nor `ParentKind` fields — those are
  computed entirely by the store layer.

So the polymorphic FK property holds without enforcement: the
parent row is guaranteed to exist because we just inserted it
one transaction step earlier with an ID we control. A floating
citation would require a code bug (refactoring the ingest to
split the transaction or rebind the variable) — a future-code
risk, not a current-code one.

**Optional hardening (not required for v0.1):** a SQL trigger
on `INSERT INTO citations` that checks `parent_id` against the
correct table per `parent_kind`. This would catch future
refactors that break the in-transaction sequencing. Feasible
but adds migration complexity; defer to v0.2.

**Action:** update Phase 1 classification to `legit-polymorphic`;
drop from ORPHAN RISK set; no Phase 3 test needed.

### Revised ORPHAN RISK set

Down to **one** column:

1. `signal_resolutions.entity_id` — confirmed bug, unchecked
   write path, not yet reachable via CLI/MCP but directly
   reachable via the public `store.Store` interface.

### Revised sizing

Phase 3 shrinks from 3–5 tests to **1 test**
(`TestOrphan_AppendResolution_EntityID`). Phase 4 becomes a
one-class design decision (Class A auto-create or Class B
reject-on-write; lean Class B because `AppendResolution` has no
legitimate "auto-materialize entity" story). Phase 5 becomes
one migration adding `REFERENCES entities(id)` to the
`signal_resolutions.entity_id` column via table-rebuild.

Optional sidecar: v12 schema-only migration flipping
`analysis_sessions.pipeline_session_id` from `NOT NULL DEFAULT
''` to `DEFAULT NULL`. Small enough to ride along in the same
commit series.

Optional sidecar (v0.2): SQL trigger for citation parent
validation. Deferred.

**Total revised estimate: 1–2 focused sessions**, down from
3–5 post-Phase-1 and 5–10 pre-audit. The audit has done the
work it set out to do: the actual fix is small and bounded.

### Orchestrator notes

- The dramatic collapse (4 → 1) is a testament to Phase 1 +
  Phase 2's complementarity. Schema alone showed 4 suspicious
  columns; write-path reality cleared three as legitimate
  patterns. A schema-only audit would have produced three
  unnecessary migrations.
- The "function implemented but not wired" status of
  `AppendResolution` is the cleanest possible fix situation: no
  existing orphan rows to migrate, no existing behavior to
  preserve, just a correctness property to enforce before the
  code gets used.
- The style cleanup for `pipeline_session_id` is a judgement
  call for the operator. I lean fold-in because we're touching
  migrations anyway and the diff is small; you may prefer defer
  so the orphan-audit commit stays narrowly scoped.
- Phase 3 is now small enough that delegation overhead would
  exceed doing it directly. Same for Phase 4's one-class design
  decision. Phase 5's migration is worth careful writing
  (SQLite's rebuild-the-table dance) but also fast — one
  session.

## Phase 2: Write-path inventory (~1 session)

For each FK-shaped column from Phase 1, enumerate the code paths
that write to it:

- Every `INSERT INTO <table>` or SQL builder populating the
  column.
- Every Go helper (`PutEntity`, `SetPosture`, `AppendSignals`,
  `IngestAnalystOutput`, etc.) that the SQL sits behind.
- Every CLI command or MCP tool handler that calls those helpers.

For each write path, classify:

- **Validated** — pre-write existence check against the parent
  table.
- **Auto-creates** — materializes the parent row as part of the
  same operation (legitimate, e.g., `posture set` on a new URI).
- **Unchecked** — writes without validation or auto-create.
  Orphan-production site.

**Deliverable:** a write-path × FK-column matrix with
validated/auto-creates/unchecked per cell. The unchecked cells
are the bug sites.

**Expected finding:** the ingest path is likelier to have holes
than the CLI path — analyst agents pass URIs to MCP
`signatory_ingest_analysis` that might reference entities that
don't yet exist. The handler has to choose auto-create vs.
reject; silent-bypass is not a defensible third option.

## Phase 3 findings (2026-04-24)

Phase 3's deliverable is the single reproducing test for the one
confirmed ORPHAN RISK column
(`signal_resolutions.entity_id`), plus evidence captured from a
local test run that the bug is real.

### Test landed

`internal/store/orphan_test.go` →
`TestOrphan_AppendResolution_EntityID`, guarded by `t.Skip`
pending Phase 5. The test body asserts post-fix behavior:
`AppendResolution` with an orphan `entity_id` must return a
typed `ErrOrphanedEntity` error, and no row must be observable
in `signal_resolutions` afterward.

Sibling change: `internal/store/errors.go` now defines
`ErrOrphanedEntity` ahead of its first production use in Phase
5. Declaring the sentinel in Phase 3 lets the test reference it
via `errors.Is` at compile time, independent of when the
validation logic lands.

### Captured red state (pre-fix, 2026-04-24)

TDD step 2 — prove the bug is real. The `t.Skip` was
temporarily removed and the test run in isolation. Output:

```
$ go test -run TestOrphan_AppendResolution_EntityID -v ./internal/store/...
=== RUN   TestOrphan_AppendResolution_EntityID
=== PAUSE TestOrphan_AppendResolution_EntityID
=== CONT  TestOrphan_AppendResolution_EntityID
    orphan_test.go:107:
        	Error Trace:	/Users/sarah/git/signatory/internal/store/orphan_test.go:107
        	Error:      	An error is expected but got nil.
        	Test:       	TestOrphan_AppendResolution_EntityID
        	Messages:   	AppendResolution must reject a resolution whose entity_id
        	                does not exist in the entities table — the schema's
        	                missing REFERENCES entities(id) plus the absent Go-side
        	                validation produces a silently-landed orphan today; the
        	                Phase 5 fix closes both halves.
--- FAIL: TestOrphan_AppendResolution_EntityID (0.07s)
FAIL
FAIL	github.com/sarahmaeve/signatory/internal/store	0.500s
FAIL
```

Failure location: the first assertion in the test
(`require.Error(t, err)`). Mechanism: `AppendResolution`
returned `nil` when called with `entity_id =
"ghost-entity-id-99999"`, a value never inserted into
`entities`. The INSERT succeeded at the SQL layer because
`signal_resolutions.entity_id` has no `REFERENCES
entities(id)` constraint and SQLite's FK enforcement (which IS
on globally) had nothing to check. The Go helper performed
only a non-empty-string check on `r.EntityID` and did no
pre-INSERT lookup against `entities`.

This is the exact bug shape Phase 2 predicted: unchecked write
path + unenforced schema = silent orphan at permanent
append-only rest.

### Phase 3 commit

Lands the test + error sentinel + this findings update in one
atomic diff:

- `internal/store/errors.go` — add `ErrOrphanedEntity` sentinel
- `internal/store/orphan_test.go` — new file,
  `TestOrphan_AppendResolution_EntityID` guarded by `t.Skip`
- `design/orphanage.md` — Phase 3 findings section (this one)

Commit message documents the red-state capture and names Phase
5 as the unskip point.

### What Phase 4 + Phase 5 will do

The test's post-fix assertions dictate the fix's shape. To make
this test pass, Phase 5 must:

1. **Schema (migration):** add `REFERENCES entities(id)` to
   `signal_resolutions.entity_id` via SQLite's table-rebuild
   pattern. This is the defense-in-depth layer — even if
   someone bypasses the Go helper, the SQL rejects.
2. **Code (`AppendResolution`):** add a pre-INSERT existence
   check against `entities` by `entity_id`. On miss, return
   `fmt.Errorf("%w: entity_id=%q", ErrOrphanedEntity,
   r.EntityID)` or similar. This is the UX-clean layer — the
   caller gets a typed error with useful context, not a raw
   SQL constraint violation.
3. **Remove the `t.Skip`** in the same commit. The test
   transitions from "documents the bug" to "guards the fix."

Phase 4's one design decision (Class A/B/C per §Phase 4): this
is **Class B** — `AppendResolution` has no legitimate
"auto-materialize entity" story (resolutions describe
conflicts among signals that can only exist if an entity
exists). Reject-on-write is correct.

Optional sidecar in the same commit series: v12 migration
flipping `analysis_sessions.pipeline_session_id` from `NOT NULL
DEFAULT ''` to `DEFAULT NULL`. Per Phase 2's lean: fold in
because we're touching migrations anyway.

## Phase 4 + 5 findings (2026-04-24)

Phase 4 was a one-line design call (Class B: reject-on-write + FK)
already made in the Phase 3 commit's message. Phase 5 is the
actual fix. Landing together in one commit because the test
transitions red → green through this same commit, and splitting
would leave a mid-state where the test was live but the fix
partial.

### Class B decision, confirmed

`AppendResolution` describes a resolution between conflicting
signals for a specific entity. There's no legitimate
"auto-materialize entity from a resolution" story — if the entity
doesn't exist, neither do its signals, so there's nothing to
resolve. Reject-on-write is correct. Class A (auto-create
parent) was never viable.

### What landed

**Schema (v12 migration, `internal/store/migrate.go`):** adds
`REFERENCES entities(id)` to `signal_resolutions.entity_id` via
the v9-style rebuild ceremony:

1. Drop the v3 append-only triggers on signal_resolutions.
2. Create `signal_resolutions_new` with the FK.
3. Copy rows via `INSERT … SELECT … WHERE entity_id IN (SELECT
   id FROM entities)` — orphan-filter in the WHERE clause; any
   orphan rows silently dropped.
4. DROP old, RENAME new.
5. Recreate `idx_resolutions_entity`.
6. Reinstall the append-only triggers on the rebuilt table.

Down migration reverses by rebuilding without the FK, same
trigger ceremony.

**Why silent-drop for orphans:** per Phase 2, `AppendResolution`
has no production callers; any orphan rows in any store would be
test-leftover or manual-poke data, not legitimate application
state. The alternative (abort migration on orphan) would block
operators with no recourse beyond manual SQL surgery — a bad
trade for a pattern that shouldn't arise in practice. A
pre-migration operator can count orphans themselves with
`SELECT COUNT(*) FROM signal_resolutions sr WHERE sr.entity_id
NOT IN (SELECT id FROM entities)` if they want visibility before
running.

**FK enforcement and transactions:** `migrate.go` runs each Up
inside a transaction, and SQLite silently ignores `PRAGMA
foreign_keys` toggles inside a transaction — we cannot disable
FK enforcement during the rebuild. The orphan-filter WHERE
clause is the mechanism that makes the INSERT succeed without
needing PRAGMA manipulation.

**Code (`internal/store/sqlite.go`):** `AppendResolution` gains
a pre-INSERT entity existence check. On miss, returns
`fmt.Errorf("%w: entity_id=%q", ErrOrphanedEntity, r.EntityID)`.
This is the UX-clean layer — callers get a typed error with
readable context, not a raw SQL constraint violation.

**Prune side-effect fix (`internal/store/prune.go`):** adds
`{"signal_resolutions", "entity_id"}` to the directChildren
sweep at Level 2. Without this, pruning an entity X whose
entity_id appears in a signal_resolutions row — but whose
signal_ids don't belong to X — would fail the new FK on the
subsequent `DELETE FROM entities`. The cross-entity case is
theoretical today (the cross-entity-consistency gap is
documented in sqlite_security_test.go but not exercised), but
the fix is one line and closes the edge case at the same time.

**Test transition:** `t.Skip` removed from
`TestOrphan_AppendResolution_EntityID`. Same test body that
was captured red in Phase 3 now passes — both assertions
(`require.Error` + `assert.ErrorIs(ErrOrphanedEntity)`) fire,
and the observable-row COUNT == 0 check confirms the FK
rejected the INSERT even if the code check somehow didn't.

**Migration-test updates:** `TestMigration_RollbackDown` and
`TestMigration_V2DataRoundTrip` each walk migrations down
step-by-step and expected the downward journey to start at
v11. Added one more rollback step at the top (v12 → v11)
plus a matching numbering correction for the existing
follow-on comments.

### Captured green state (post-fix, 2026-04-24)

```
$ go test -count=1 -run "TestOrphan_AppendResolution_EntityID|TestMigration_RollbackDown|TestMigration_V2DataRoundTrip" -v ./internal/store/...
=== RUN   TestMigration_RollbackDown
--- PASS: TestMigration_RollbackDown (0.18s)
=== RUN   TestMigration_V2DataRoundTrip
--- PASS: TestMigration_V2DataRoundTrip (0.25s)
=== RUN   TestOrphan_AppendResolution_EntityID
=== PAUSE TestOrphan_AppendResolution_EntityID
=== CONT  TestOrphan_AppendResolution_EntityID
--- PASS: TestOrphan_AppendResolution_EntityID (0.07s)
PASS
ok  	github.com/sarahmaeve/signatory/internal/store	0.911s
```

Full tree (`go test -p 1 ./...`) passes in ~55s, every package
green.

### Audit status after this commit

| Column | Audit status |
|---|---|
| `signal_resolutions.entity_id` | **CLOSED** — schema FK + Go validation + prune fix |
| `dependency_observations.survey_id` | Closed in Phase 2 (reclassified `legit-freeform`) |
| `analysis_sessions.pipeline_session_id` | Closed in Phase 2 (reclassified `legit-freeform`); style-cleanup sidecar deferred |
| `citations.parent_id` | Closed in Phase 2 (reclassified `legit-polymorphic`) |

All 4 originally-flagged columns resolved. The audit is
complete for v0.1.

### Deferred follow-ups (post-audit)

1. **`analysis_sessions.pipeline_session_id` NULL-vs-empty-string
   migration.** Phase 2 recommended folding this in; on closer
   inspection, it requires Go-side `sql.NullString` handling in
   `scanAnalysisSession` and wherever the field is read, which
   grows the commit meaningfully. Deferred to a separate narrow
   commit. Audit-independent — pure style consistency.
2. **Cross-entity-consistency validation in `AppendResolution`**
   (entity_id's signals match the kept/superseded signal_ids'
   entity_id). Documented limitation in
   `sqlite_security_test.go`; distinct integrity gap from this
   audit's scope.
3. **SQL trigger on citation INSERT validating
   `(parent_kind, parent_id)` pair.** Belt-and-suspenders
   hardening against a future refactor that splits citation
   ingest across transactions. Currently structurally safe;
   trigger makes it safe even under refactoring. Defer to v0.2.

### Lessons from the audit

- **Schema alone is misleading.** Phase 1's 4 suspicious
  columns collapsed to 1 in Phase 2 once write-paths were
  traced. A schema-only audit would have produced three
  unnecessary migrations — specifically one for
  `pipeline_session_id` that would have broken `scanAnalysisSession`
  until follow-up code landed.
- **Parallel investigation pays off when subjects are
  independent.** Phase 2's four agents finished in ~80s each
  vs. the ~4 hours a single-threaded investigation would have
  taken, because the four columns had no overlap in their
  write-path surfaces.
- **TDD ordering works under pre-commit constraints.** The
  `t.Skip` + prose-evidence pattern preserved the red → green
  TDD cycle without breaking CI between commits. The captured
  failure output in Phase 3's doc became the audit trail
  confirming the bug was real.
- **Append-only plus missing FK is a trap.** A normal
  (mutable) table's orphan is correctable post-hoc. An
  append-only table's orphan is permanent. Future audits
  should treat append-only tables with missing FKs as
  higher-severity than mutable ones.

## Phase 3: Reproduce with failing tests (~1 session)

Per `MEMORY.md` §"TDD for security fixes": write the failing
test before the fix. For each "unchecked" cell from Phase 2:

1. Open a fresh TempDir store.
2. Use the smallest possible call path to create the orphan
   (preferring the real Go helper over direct SQL).
3. Assert the orphan lands — no error, no rejection, row
   observable in the table.
4. Test inverts the moment the fix lands. That's the gate.

**Naming:** `TestOrphan_<writepath>_<fkcolumn>` so the test
catalog mirrors the Phase 2 matrix.

**Style:** each test today passes (documents the bug). After
the fix ships, flip to `require.Error` + `ErrorIs` against a
typed error. Alternative: `t.Skip("pending orphan-audit fix")`
— cleaner but hides the diagnostic detail. Preferred: the
explicit `t.Log("currently passing; inverts on fix")` pattern.

**Deliverable:** N failing-on-paper tests living under
`internal/store/orphan_test.go` or distributed per table. Fix
PRs invert them to passing-under-enforcement.

## Phase 4: Design fixes per class (~half session)

Group Phase 3 failures by fix shape:

- **Class A — Add FK + keep auto-create.** Legitimate auto-
  materialization (posture-set on new URI is a user gesture).
  Add FK; ensure parent creation precedes child write.
- **Class B — Add FK + reject at code layer.** Write path
  shouldn't auto-create (ingest of analyst_output against a
  mismatched entity is suspicious). Add FK, add pre-write
  existence check, return typed error.
- **Class C — Add NOT NULL + stricter lookup.** Column was
  nullable and got NULLs when lookup silently failed. Make NOT
  NULL; force lookup to error on miss instead of returning NULL.

**Deliverable:** per orphan class, a one-line "Class A/B/C +
rationale" assignment. Drives migration and code work.

## Phase 5: Migrations (~per-class)

Because SQLite can't add FK in place, each fix is a new migration
(`migrations/v9_<name>.sql` onward). Group related fixes where
they share a parent table — avoid one migration per column if
one table has multiple holes.

**Two considerations per migration:**

1. **Pragma on.** Confirm `PRAGMA foreign_keys = ON` runs on
   every migrated connection.
2. **Existing-orphan handling.** Recreated-with-FK tables
   surface existing violations. Options:
   - **Abort migration** — safest for pristine stores, brutal
     for dogfood stores.
   - **Delete orphans with logged count** — automated recovery.
     Operator sees "migration vN dropped 3 orphaned posture
     rows, IDs logged."
   - **Quarantine to sidecar table** — heaviest; lets operator
     inspect before deciding.

**Recommendation:** delete-with-log for v0.1. The audience is
the dogfood operator; a lost posture is re-creatable; the
logged count preserves observability. Document the "drop
orphans" behavior visibly in the migration comment and in the
commit message.

## Phase 6: CI guard (~half session)

Once fixes land, prevent regression:

- Migration-time lint flagging new `_id` columns without
  `REFERENCES`. Hard to implement generically; may reduce to a
  per-migration review checklist.
- Code-layer lint flagging new write paths against store tables
  without going through canonical helpers.

Minimum viable guard: a single test in `internal/store/` that
attempts to violate each constraint class and asserts the
violations reject. Inverts Phase 3's tests from "documents bug"
to "guards fix."

## Phase 7: Deferred

Out of scope for this audit but on the follow-up list:

1. **Ingest withdraw.** Separately-scoped concern. Depends on
   orphan-audit fixes landing first — withdraw semantics
   (`analyst_output_withdrawals` table) assume the referenced
   `analyst_output` is legitimately resolvable.
2. **MCP-write-tool orphan tests.** The MCP
   `signatory_ingest_analysis` path is a write site and belongs
   in Phase 3. But the broader MCP surface is deprioritized
   (see `design/potential-survey-mcp.md`). Either exercise the
   MCP ingest path through its handler directly (no transport),
   or skip and document the gap.

## Deliverable shape

At audit completion:

1. **Phase 0 baseline** — paragraph in this doc (§"Phase 0
   findings").
2. **Phase 1 catalog** — markdown table in this doc or sibling
   migration-reference doc.
3. **Phase 2 matrix** — markdown table in this doc.
4. **Phase 3 failing tests** — in-tree, `internal/store/orphan_test.go`
   or distributed per table.
5. **Phase 4–5 fix design** — per-class plan in this doc; per-
   migration rationale in commit messages.
6. **Phase 6 CI guards** — code + test.

## Rough sizing

- Phase 0–1: investigation-heavy, ~1.5 sessions.
- Phase 2: ~1 session, more investigation.
- Phase 3: ~1 session. Parallelizable across orphan classes.
- Phase 4: ~half session of design work.
- Phase 5–6: per-migration, hard to estimate without Phase 1–2
  output. Estimate 2–5 migrations, each a self-contained commit.

**Total:** ~5–10 focused sessions, skewed toward investigation
and test-writing.

---

## Phase 0 findings (2026-04-24)

**Executive summary: better news than expected.** FK enforcement
is on, and most FK-shaped columns already have `REFERENCES`.
The audit scope narrows to "a small set of suspicious columns
and the write paths that might bypass them," not "every write
is orphan-capable."

### 1. PRAGMA `foreign_keys` is ON

Both SQLite stores turn it on at open:

- `internal/store/sqlite.go:141` — main store
- `internal/pipeline/store.go:59` — pipeline service store

Durability: the main store uses `db.SetMaxOpenConns(1)`
(`sqlite.go:125`). A single connection means per-connection
pragmas apply globally to every query — no pool-induced silent
drift. Good discipline, and the v0.1 single-user shape makes
this a correct choice.

**Conclusion:** declared FK constraints are actually enforced.
Adding a `REFERENCES` to the schema will do real work.

### 2. FK coverage count

- **41** `_id`-suffixed columns across all `CREATE TABLE` and
  `ALTER TABLE ADD COLUMN` sites in `internal/store/migrate.go`.
- **28** have a `REFERENCES` clause.
- **Delta: 13** columns without declared FKs.

Grep source: `grep -iEc '^\s*[a-z_]+_id\s' internal/store/migrate.go`
and `grep -c REFERENCES internal/store/migrate.go`.

The delta includes both legitimately-non-FK columns and
suspicious ones; Phase 1 is the classification work.

### 3. First-pass classification of the 13

Legitimately **not** FKs (local-composite or free-form ID
columns):

- `finding_local_id`, `observation_local_id`, `pattern_local_id`
  — local identifiers within a parent's composite key.
- `analyst_id`, `source_analyst_id` — free-form analyst-name
  strings (e.g. `signatory-security-v1`), not row IDs.
- `prior_id`, `related_id` — cross-reference strings whose
  semantics in Phase 1 still need confirmation, but by name
  look external-purposed.

Suspicious — FK-shaped and likely missing enforcement:

- **`dependency_observations.survey_id`** (line 211) — NOT
  NULL, no `REFERENCES`. There's an index on it, suggesting
  it IS meant as a row identifier, so a missing FK is the
  most likely explanation.
- **`signal_resolutions.entity_id`** (line 220) — NOT NULL,
  no `REFERENCES`. Out of pattern with every other
  `entity_id` column, which DOES have `REFERENCES entities(id)`.
- **`analysis_sessions.pipeline_session_id`** (line 1132) —
  NOT NULL with `DEFAULT ''`. The empty-string sentinel is a
  house convention for "optional but never-NULL" — but it
  also means the column accepts a value that would never
  satisfy a `REFERENCES` constraint. Worth a Phase-1 decision
  on whether the empty-string sentinel stays (and FK is not
  added) or gets replaced with NULL (and FK is added).
- **`audit_log.entity_id`** (line 237) — nullable, no
  `REFERENCES`. Auditing non-entity events is legitimate
  (a posture set against a never-resolved target should still
  audit), so missing-FK here is probably correct. Document
  and move on.

Three `parent_id` sites (lines 549, 974, 1029) — probably
self-references inside hierarchical structures; Phase 1
confirms.

### 4. Pipeline store: clean

`internal/pipeline/migrate.go` has two tables:

- `pipeline_sessions` (id + target + status + timestamps)
- `pipeline_messages` (session_id REFERENCES pipeline_sessions(id))

Pragma on, single FK declared and enforced. No orphan risk.
The pipeline side is out of scope for the audit; all further
phases focus on the main store.

### 5. Orphan-shape: insert-time, not delete-time

The v11 migration comment at `migrate.go:1120-1124` observes
that the main store relies on append-only triggers (blocks
`DELETE` outside the prune path) plus `ON DELETE SET NULL`
for the two pruning-eligible FKs. This means:

> Orphan risk in this store is almost entirely **insert-time**:
> a row is inserted with an FK value that doesn't resolve to a
> parent that existed at insert time. "Parent was deleted later,
> leaving orphans" is prevented structurally by the append-only
> discipline.

This matches the user's original framing ("preventing orphaned
data from being ingested into the db") and significantly
narrows the audit: we're looking for write paths that insert
with unvalidated FK values, not for cleanup after deletes.

### 6. Revised scope

Based on Phase 0:

- Phase 1 (schema inventory) is smaller than initially sized —
  maybe 2–4 hours, not a full session. The classification is
  mostly inspection of ~13 columns already enumerated.
- Phase 2 (write-path inventory) is the real work. Every
  `INSERT INTO <table>` site needs to be classified as
  validated/auto-creates/unchecked, and the universe is larger
  than the FK count. This is the part where orphan risk
  actually lives.
- Phase 3 (reproducing tests) has a narrower target set than
  initially assumed — probably 3–5 orphan classes based on the
  suspicious columns above plus insert-side bypasses yet to
  surface in Phase 2.

**Total revised estimate: 3–5 focused sessions**, down from
5–10.

### 7. Next action

Phase 1: inventory. Confirm the classification of the 13
non-REFERENCES columns by reading each declaration in context,
produce the schema-inventory table (§Phase 1), and flag the
specific orphan-suspect columns.

Phase 1 can be delegated to a Sonnet agent with the schema
file and this findings section as input, because it's pattern-
matching over `migrate.go` — no judgment calls about v0.1
scope. The orchestrator reviews the returned table before
Phase 2 kicks off.
