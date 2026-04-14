# Signatory: Ingestion Plan for AnalystOutput

## Status

Draft — proposed plan for ingesting v1-schema `AnalystOutput`
documents into the SQLite store. Not yet implemented. Pending
discussion on the two open questions called out below
(synthesis-as-machine-step vs. human-team-product, and developer-
identity tracking).

## Motivation

We have:
- A v1 schema for analyst output (`internal/exchange/`)
- A SQLite store with an entity model (`internal/store/`,
  `design/entity-model-v2.md`)
- Two production engagements' worth of analyst output sitting on
  disk in `filestore/analysis/` as JSON

What we don't have: a way to *use* that data through the DB. The
files are queryable only by reading them; cross-target queries
("who else publishes via local-laptop?") are O(N) file reads
instead of one SQL query. The MCP architecture's "caller-chooses-
depth" model (per `design/mcp-dual-analyst-architecture.md`)
relies on agents being able to ask "have we already analyzed this?"
without re-running — which only works if past analyses are indexed.

Ingestion is the bridge between the two layers. It's also a
prerequisite for several downstream goals:

1. **MCP query endpoints** that return findings/observations by
   target, signal type, severity, analyst, freshness, etc.
2. **Burn propagation** that retroactively degrades trust signals
   tied to a compromised entity (per trust-model.md principle #3).
3. **Cross-engagement learning** — methodology patterns that hit
   on more than one target are deterministic-collector candidates;
   surfacing this requires aggregate queries.
4. **Federated trust exchange** — the entity-model-v2 design
   contemplates importing analyst outputs from peer signatory
   instances. Both peer-import and local-ingest go through the
   same code path.

## Schema tension and the chosen resolution

A `Finding` is much richer than a signal. The existing `signals`
table (atomic observation: `value` is a TEXT scalar) cannot hold
verdict + rationale + multi-citation + conditional-severity +
prerequisites + supersession without losing the structure that
makes findings queryable.

**Chosen resolution: parallel storage, joined by entity.**

Layer 1 (deterministic collectors) emits `signals` rows. Layer 2
(LLM analysts) emits `analyst_outputs` documents that decompose
into a small set of new tables. Both streams attach to the
existing `entities` table via FK. A query like "what does
signatory know about target X?" joins both streams on entity_id.

Why not collapse findings into signals (signals.value = JSON
finding)?
- Citations need to be queryable as their own rows
- Findings have polymorphic relationships (citations attach to
  findings AND positive_absences AND observations)
- Findings supersede other findings; signals supersede via a
  separate `signal_resolutions` table — different mechanics
- Severity-by-context wants its own table or JSON column

Why not collapse signals into findings (one stream)?
- Signals are atomic; findings are judgment-laden
- Layer 1 collectors emit signals as a stream of rows; doing the
  same with finding-shape would force them to fabricate
  verdict/rationale fields they don't have
- The two have different write rates (signals: high, every
  collector run; findings: low, every analyst run)

## Proposed schema (migration v4)

Lands together with the v4 changes already proposed in
`design/signal-storage-evolution.md` (`signals.details` JSON
column + `signal_evidence` table) so the migration step is one
unit.

### Tables for the analyst-output stream

```sql
-- One row per ingested AnalystOutput envelope.
CREATE TABLE analyst_outputs (
    id              TEXT PRIMARY KEY,         -- UUID, generated at ingest
    entity_id       TEXT NOT NULL REFERENCES entities(id),
    analyst_id      TEXT NOT NULL,            -- "signatory-provenance" | "external-sec-v1" | etc.
    model           TEXT NOT NULL,            -- "claude-opus-4-6"
    prompt_version  TEXT NOT NULL DEFAULT '',
    invoked_at      TEXT NOT NULL,            -- analyst's claim
    ingested_at     TEXT NOT NULL,            -- when signatory loaded it
    round           INTEGER NOT NULL DEFAULT 1,
    target_commit   TEXT NOT NULL DEFAULT '',
    round_notes     TEXT NOT NULL DEFAULT '',
    source_path     TEXT NOT NULL DEFAULT '', -- file we ingested from
    content_hash    TEXT NOT NULL UNIQUE      -- sha256 of canonical JSON
);
CREATE INDEX idx_outputs_entity ON analyst_outputs(entity_id);
CREATE INDEX idx_outputs_analyst_target ON analyst_outputs(analyst_id, entity_id, invoked_at);

-- One row per Finding.
CREATE TABLE findings (
    id                  TEXT PRIMARY KEY,         -- UUID
    output_id           TEXT NOT NULL REFERENCES analyst_outputs(id),
    finding_local_id    TEXT NOT NULL,            -- "F001" — stable within output
    verdict             TEXT NOT NULL,
    rationale           TEXT NOT NULL,
    severity_default    TEXT NOT NULL,
    design_intent       INTEGER NOT NULL DEFAULT 0,
    category            TEXT NOT NULL,
    signal_type         TEXT NOT NULL DEFAULT '', -- FK into registry; '' if absent
    answers_question    TEXT NOT NULL DEFAULT '',
    UNIQUE (output_id, finding_local_id)
);
CREATE INDEX idx_findings_output ON findings(output_id);
CREATE INDEX idx_findings_severity ON findings(severity_default);
CREATE INDEX idx_findings_signal_type ON findings(signal_type);

-- Conditional severity overrides: (host_isolation, platform) → value.
CREATE TABLE finding_severity_contexts (
    finding_id      TEXT NOT NULL REFERENCES findings(id),
    host_isolation  TEXT NOT NULL DEFAULT '',
    platform        TEXT NOT NULL DEFAULT '',
    value           TEXT NOT NULL,
    PRIMARY KEY (finding_id, host_isolation, platform)
);

-- Supersession: this finding revises one or more priors.
CREATE TABLE finding_supersedes (
    finding_id      TEXT NOT NULL REFERENCES findings(id),
    prior_id        TEXT NOT NULL,            -- string from JSON; may not match a row in this DB
    prior_round     INTEGER NOT NULL DEFAULT 0,
    kind            TEXT NOT NULL,            -- corrects | refines | deprecates
    PRIMARY KEY (finding_id, prior_id)
);

-- Per-Finding prerequisites and remediation_hints: simple lists, kept as
-- single-column join tables for queryability.
CREATE TABLE finding_prerequisites (
    finding_id  TEXT NOT NULL REFERENCES findings(id),
    seq         INTEGER NOT NULL,
    text        TEXT NOT NULL,
    PRIMARY KEY (finding_id, seq)
);
CREATE TABLE finding_remediation_hints (
    finding_id  TEXT NOT NULL REFERENCES findings(id),
    seq         INTEGER NOT NULL,
    text        TEXT NOT NULL,
    PRIMARY KEY (finding_id, seq)
);

-- Related-finding cross-references within the same output.
CREATE TABLE finding_related (
    finding_id  TEXT NOT NULL REFERENCES findings(id),
    related_id  TEXT NOT NULL,                -- finding_local_id from JSON
    PRIMARY KEY (finding_id, related_id)
);

-- Positive absences: same shape as findings but lighter, distinct semantics.
CREATE TABLE positive_absences (
    id                  TEXT PRIMARY KEY,         -- UUID
    output_id           TEXT NOT NULL REFERENCES analyst_outputs(id),
    pattern_checked     TEXT NOT NULL,
    description         TEXT NOT NULL,
    confidence          TEXT NOT NULL,            -- spot_checked | thoroughly_reviewed | exhaustive
    pattern_ref         TEXT NOT NULL DEFAULT ''  -- optional FK to a methodology pattern by id
);
CREATE INDEX idx_absences_output ON positive_absences(output_id);

-- Observations: trust-model commentary that doesn't fit the Finding shape.
CREATE TABLE observations (
    id                  TEXT PRIMARY KEY,         -- UUID
    output_id           TEXT NOT NULL REFERENCES analyst_outputs(id),
    observation_local_id TEXT NOT NULL,           -- "O1" — stable within output
    title               TEXT NOT NULL,
    body                TEXT NOT NULL,
    category            TEXT NOT NULL,
    signal_type         TEXT NOT NULL DEFAULT '',
    UNIQUE (output_id, observation_local_id)
);
CREATE INDEX idx_observations_output ON observations(output_id);

-- Methodology catalog: one per output.
CREATE TABLE methodology_catalogs (
    output_id           TEXT PRIMARY KEY REFERENCES analyst_outputs(id),
    source_analyst_id   TEXT NOT NULL,
    source_model        TEXT NOT NULL,
    source_invoked_at   TEXT NOT NULL,
    notes               TEXT NOT NULL DEFAULT ''
);

-- Methodology patterns within a catalog.
CREATE TABLE methodology_patterns (
    id                       TEXT PRIMARY KEY,    -- UUID
    output_id                TEXT NOT NULL REFERENCES analyst_outputs(id),
    pattern_local_id         TEXT NOT NULL,       -- "MP-PY-NET-01" — stable within catalog
    signal_group             TEXT NOT NULL,
    description              TEXT NOT NULL,
    pattern_text             TEXT NOT NULL DEFAULT '',
    grep_precision           TEXT NOT NULL,
    reasoning_depth          TEXT NOT NULL,
    miss_mode                TEXT NOT NULL DEFAULT '',
    false_positive_notes     TEXT NOT NULL DEFAULT '',
    hit_on_target            INTEGER,            -- nullable bool; -1=null, 0=false, 1=true
    UNIQUE (output_id, pattern_local_id)
);
CREATE INDEX idx_patterns_signal_group ON methodology_patterns(signal_group);
CREATE INDEX idx_patterns_hit ON methodology_patterns(hit_on_target);

-- Pattern composition.
CREATE TABLE methodology_pattern_composes (
    pattern_id      TEXT NOT NULL REFERENCES methodology_patterns(id),
    composes_with   TEXT NOT NULL,             -- pattern_local_id from JSON
    PRIMARY KEY (pattern_id, composes_with)
);

-- Citations: polymorphic FK via a kind+target_id pair.
CREATE TABLE citations (
    id              TEXT PRIMARY KEY,             -- UUID
    parent_kind     TEXT NOT NULL,                -- finding | positive_absence | observation | methodology_pattern
    parent_id       TEXT NOT NULL,                -- UUID of the parent row
    seq             INTEGER NOT NULL,             -- order within the parent's citations array
    path            TEXT NOT NULL DEFAULT '',
    line_start      INTEGER,                      -- nullable
    line_end        INTEGER,                      -- nullable
    scope_kind      TEXT NOT NULL DEFAULT '',
    scope_path      TEXT NOT NULL DEFAULT '',
    commit_sha      TEXT NOT NULL DEFAULT '',
    quoted          TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_citations_parent ON citations(parent_kind, parent_id);
CREATE INDEX idx_citations_path ON citations(path);

-- Top-level supersession: this output supersedes prior outputs.
CREATE TABLE output_supersedes (
    output_id       TEXT NOT NULL REFERENCES analyst_outputs(id),
    prior_id        TEXT NOT NULL,
    prior_round     INTEGER NOT NULL DEFAULT 0,
    kind            TEXT NOT NULL,
    PRIMARY KEY (output_id, prior_id)
);
```

All append-only — same trigger pattern as migration v3 applied to
each new table. Re-ingesting the same file is a no-op via the
`content_hash` UNIQUE constraint on `analyst_outputs`. Re-running
the same analyst on the same target with new findings produces a
new `analyst_outputs` row (different invoked_at, different content
hash) and the supersession metadata indicates the relationship.

### Existing-table modifications

The `signals.details` JSON column and `signal_evidence` table from
`design/signal-storage-evolution.md` land in the same migration.
They serve different purposes (Layer 1 signal evidence vs Layer 2
analyst-output structure) and don't conflict.

## Phasing

### Phase A — Schema + ingest path (1–2 days)
- Migration v4: all new tables above + the proposed
  `signal-storage-evolution.md` additions
- `internal/store/` Go API:
  - `IngestAnalystOutput(ctx, *exchange.AnalystOutput, sourcePath string) (id string, idempotent bool, err error)`
  - Returns `idempotent: true` if the file's content hash already exists
- New CLI: `signatory ingest <file>` — wraps format-check + the new store call
- Tests: round-trip the four existing JSON fixtures into a fresh DB; verify counts match

### Phase B — Read path (1 day)
- Backfill: `signatory ingest filestore/analysis/*-v1.json` to populate from existing files
- Read API in `internal/store/`:
  - `ListAnalystOutputs(filter)`, `GetAnalystOutput(id)`, `GetFindings(filter)`, etc.
- Query CLI:
  - `signatory show-analyses <target>` — list AnalystOutputs for an entity
  - `signatory show-findings [--severity=...] [--signal-type=...] [--target=...]`
  - `signatory show-methodology [--hit-on-target] [--signal-group=...]`

### Phase C — MCP integration (separate work, larger scope)
- Endpoints sketched in chat:
  - `signatory://analyses?target=...`
  - `signatory://findings?signal_type=...&severity_gte=...`
  - `signatory://analyses/freshness?target=...`
- Freshness check baked into `signatory analyze` orchestration

### Phase D — Write-back sync (later)
- When agents write findings via MCP rather than via JSON-file
  ingestion, we need a "DB → file" synchronizer to maintain the
  source-of-truth property of `filestore/analysis/`.

I'd recommend doing A and B together since they're a single
logical unit and B is the validation that A's schema isn't wrong.
Phase C is a real chunk of work and benefits from having read
patterns shaken out by B first. Phase D is post-MCP.

## Files vs. DB framing

Currently our analyst outputs live in `filestore/analysis/` as JSON.
With ingestion, we have to decide: is the DB the new source of
truth, or does it live alongside the files?

**Recommendation:** files are the source of truth, DB is the
queryable index. Justifications:

- Files are auditable in git history (verbatim emissions are
  recoverable forever)
- DB is rebuildable from files (`signatory reingest --all
  filestore/analysis/`)
- Files are the export format for sharing across signatory
  instances (federated burn-list use case from
  `entity-model-v2.md`)
- DB writes from the MCP eventually need a "sync to disk" step
  so the file representation stays canonical

The DB is not a destination — it's a runtime cache with rich
query capability. Lose the DB, no harm; lose the files, lose the
analyses.

## Idempotency

Three plausible policies:

1. **Trivial:** each ingestion creates new rows; supersession is
   the only relationship. Idempotent re-ingestion produces N
   copies. Bad.
2. **Content-hash dedupe at AnalystOutput level:** if the file's
   sha256 matches an existing `analyst_outputs.content_hash` row,
   skip. **Recommended.** Same file re-ingested = no-op. Same
   analyst re-run with new content = new row. Different content
   = different hash = different row.
3. **Per-finding semantic dedupe:** hash the finding content
   (verdict + citations + signal_type) and dedupe at row level.
   Overkill for v1 — wait until we see a real use case.

## Open questions

These came up as the user evaluated this plan. Both are larger
than the ingestion work itself and probably want their own design
notes; recording here so they don't get lost.

### 1. What does "synthesis" produce, concretely?

The `atuin.md` and `thefuck.md` synthesis documents are 1237 and
345 lines respectively, with rich architectural framing,
cross-engagement learning, design-process observations, and
voice consistency with the rest of the project documentation. A
clean Layer-3 synthesist run from cold context would not produce
documents at that level — those documents were a *human/LLM-pair
work product* of an extended interactive session, not a single
machine output.

The synthesist role is real and useful, but it produces something
narrower: an *integrated structured AnalystOutput* (joins findings,
identifies overlaps, addresses intake question, produces posture
recommendation, possibly emits the synthesis-only signal types
like `dual_analyst_self_confirmation`). That's storable in the
schema above. The narrative documents are a separate artifact.

Implication for the architecture: we should distinguish
- **Tier 1 synthesis** (automatable, schema-shaped, MCP-callable)
- **Tier 2 synthesis** (markdown narrative, human/LLM-pair work
  product, architectural framing)

The MCP can produce Tier 1. Tier 2 stays a craft activity. The
existing `atuin.md` and `thefuck.md` are Tier-2 documents that
happened to incorporate Tier-1 synthesis material plus a lot of
context-aware editorial judgment.

### 2. Developer-identity tracking

Current state: the schema has `EntityIdentity` as a type constant
(`internal/profile/entity.go:35`), but no code creates per-developer
identity entities or links them to projects. All identity data we
collect (Ellie's 6.7-year tenure, nvbn's 15-year history, mailmap
entries, cross-platform consistency) lives as unstructured prose
in observations and rationales. We can't query it.

For trust-model principle #3 ("retroactive degradation of
everything an entity touched between date X and date Y") to work,
identity has to be a first-class entity with structured signals.
Concretely:

- `identity:github/{username}` as a canonical URI
- Aliases: `identity:gitlab/{username}`, `identity:email/{address}`,
  etc., with confidence-weighted links
- Per-identity signals: account_age_days, followers_count,
  cross-platform_consistency_score, identity_graph_depth (already
  in the registry as a signal type but only emitted as prose)
- A `contribution_observations` table linking identity → project →
  role + commit_count + date_range
- A `maintainer_relationships` table for current/past co-maintainer
  status per (identity, project)

This is a substantial body of work (~2 schema migrations + a
GitHub-API collector that emits identity entities + integration
into the analyze flow). Worth its own design pass, probably as
`design/identity-tracking-plan.md`.

The connection to ingestion: when we ingest an AnalystOutput,
authors of citations, names mentioned in observations, and email
addresses in rationale fields could all be candidates for
identity-entity creation. But that's an enrichment pass we'd
add later — the v1 ingestion just stores the prose as-is.

## Cross-references

- `internal/exchange/` — the v1 schema this plan ingests from
- `design/entity-model-v2.md` — entity types, append-only
  invariants, conflict resolution
- `design/signal-storage-evolution.md` — `signals.details` and
  `signal_evidence` migrate alongside this plan
- `design/mcp-dual-analyst-architecture.md` — the consumer of the
  query API this plan enables
- `design/trust-model.md` — burn propagation and retroactive
  degradation depend on ingestion + identity tracking
- `internal/store/migrate.go` — where migration v4 lands
