package store

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// Migration represents a single schema migration with both forward
// (Up) and reverse (Down) SQL. Every migration must be reversible
// to protect against data corruption during upgrades.
type Migration struct {
	Version     int
	Description string
	Up          string
	Down        string
}

// migrations is the ordered list of all schema migrations.
// New migrations are appended to the end with the next version number.
// NEVER modify an existing migration — always add a new one.
var migrations = []Migration{
	{
		Version:     1,
		Description: "initial schema",
		Up:          initialSchema,
		Down:        dropInitialSchema,
	},
	{
		Version:     2,
		Description: "entity model v2: UUID PKs, canonical URI, append-only signals, versioned posture, dependency observations, audit log",
		Up:          migrationV2Up,
		Down:        migrationV2Down,
	},
	{
		Version:     3,
		Description: "append-only enforcement: triggers blocking UPDATE/DELETE on signals, dependency_observations, signal_resolutions, audit_log",
		Up:          migrationV3Up,
		Down:        migrationV3Down,
	},
	{
		Version:     4,
		Description: "analyst-output stream: tables for AnalystOutput / Finding / PositiveAbsence / Observation / MethodologyCatalog / Citation; signals.details JSON column; signal_evidence table",
		Up:          migrationV4Up,
		Down:        migrationV4Down,
	},
	{
		Version:     5,
		Description: "rename Finding → Conclusion across all analyst-output tables",
		Up:          migrationV5Up,
		Down:        migrationV5Down,
	},
	{
		Version:     6,
		Description: "soft-delete columns for postures and burns (withdrawn_at / withdrawn_by / withdrawal_reason)",
		Up:          migrationV6Up,
		Down:        migrationV6Down,
	},
	{
		Version:     7,
		Description: "analyst_outputs.collected_from_entity_id for identity-indexed storage (agent-facing-contract M2)",
		Up:          migrationV7Up,
		Down:        migrationV7Down,
	},
	{
		Version:     8,
		Description: "analyst_outputs synthesis supplement columns: synthesis_supplement_json + proposed_tier + proposed_version_scope (agent-facing-contract M6a)",
		Up:          migrationV8Up,
		Down:        migrationV8Down,
	},
	{
		Version:     9,
		Description: "citations.parent_kind CHECK constraint — pin to {conclusion, positive_absence, observation}; cleanup pre-v5 'finding' values (F002)",
		Up:          migrationV9Up,
		Down:        migrationV9Down,
	},
	{
		Version:     10,
		Description: "analyst_outputs.target + .target_version: preserve caller-supplied target identity so ingest can normalize entity URIs to the unversioned Plan-A form without losing the @V scope (closes the 2026-04-22 testify dogfood entity-fragmentation)",
		Up:          migrationV10Up,
		Down:        migrationV10Down,
	},
	{
		Version:     11,
		Description: "analysis_sessions + analyst_outputs.analysis_session_id: durable audit identity for each /analyze run so timing, attribution, and expected-vs-landed can be answered from the store; decoupled from the ephemeral pipeline_sessions relay (WebFetch-GET-only architecture — see memory/architecture_webfetch_constraint.md)",
		Up:          migrationV11Up,
		Down:        migrationV11Down,
	},
}

// initialSchema is the v1 schema, extracted from the original
// CREATE TABLE IF NOT EXISTS statements.
const initialSchema = `
CREATE TABLE IF NOT EXISTS entities (
	id         TEXT PRIMARY KEY,
	type       TEXT NOT NULL,
	name       TEXT NOT NULL,
	ecosystem  TEXT NOT NULL DEFAULT '',
	url        TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_entities_name_type ON entities(name, type);

CREATE TABLE IF NOT EXISTS signals (
	id                 TEXT PRIMARY KEY,
	entity_id          TEXT NOT NULL REFERENCES entities(id),
	type               TEXT NOT NULL,
	signal_group       TEXT NOT NULL,
	source             TEXT NOT NULL,
	forgery_resistance TEXT NOT NULL,
	value              TEXT NOT NULL,
	collected_at       TEXT NOT NULL,
	expires_at         TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_signals_entity ON signals(entity_id);
CREATE INDEX IF NOT EXISTS idx_signals_entity_group ON signals(entity_id, signal_group);

CREATE TABLE IF NOT EXISTS postures (
	entity_id TEXT PRIMARY KEY REFERENCES entities(id),
	tier      TEXT NOT NULL,
	version   TEXT NOT NULL DEFAULT '',
	rationale TEXT NOT NULL,
	set_by    TEXT NOT NULL,
	set_at    TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS burns (
	entity_id  TEXT PRIMARY KEY REFERENCES entities(id),
	reason     TEXT NOT NULL,
	source     TEXT NOT NULL,
	source_org TEXT NOT NULL DEFAULT '',
	burned_at  TEXT NOT NULL,
	burned_by  TEXT NOT NULL
);
`

const dropInitialSchema = `
DROP TABLE IF EXISTS burns;
DROP TABLE IF EXISTS postures;
DROP TABLE IF EXISTS signals;
DROP TABLE IF EXISTS entities;
`

// migrationV2Up evolves the schema for entity model v2.
// Key changes:
//   - Entities: drop v1 `name`, add canonical_uri, short_name, description.
//     Per design/entity-model-v2.md, short_name + description replace the
//     old single-purpose name column.
//   - Signals: append-only (no upsert), keep existing data.
//   - Postures: versioned PK (entity_id, version).
//   - New tables: dependency_observations, signal_resolutions, audit_log,
//     team_identities.
//
// Index sequencing note: SQLite's ALTER TABLE DROP COLUMN refuses to drop
// a column that is referenced by an index. The v1 schema has
// idx_entities_name_type ON entities(name, type), so we must drop that
// index *before* dropping the name column, otherwise the migration fails.
const migrationV2Up = `
-- Add new columns to entities (keep v1 name column for now — we copy
-- from it below before dropping).
ALTER TABLE entities ADD COLUMN canonical_uri TEXT NOT NULL DEFAULT '';
ALTER TABLE entities ADD COLUMN short_name TEXT NOT NULL DEFAULT '';
ALTER TABLE entities ADD COLUMN description TEXT NOT NULL DEFAULT '';

-- Populate canonical_uri and short_name from legacy data. Only rows with
-- empty canonical_uri are touched, so re-running is a no-op (defensive
-- even though migrations run once).
UPDATE entities SET canonical_uri = id, short_name = name WHERE canonical_uri = '';

-- Drop the v1 index that references name, then drop the name column
-- itself. Order matters: SQLite blocks DROP COLUMN if any index still
-- references the column.
DROP INDEX IF EXISTS idx_entities_name_type;
ALTER TABLE entities DROP COLUMN name;

-- V2 indexes per the entity-model-v2.md spec.
CREATE UNIQUE INDEX IF NOT EXISTS idx_entities_canonical_uri ON entities(canonical_uri);
CREATE INDEX IF NOT EXISTS idx_entities_type ON entities(type);

-- Postures: recreate with composite PK (entity_id, version).
-- SQLite cannot alter primary keys, so we recreate the table.
CREATE TABLE postures_v2 (
	entity_id TEXT NOT NULL REFERENCES entities(id),
	version   TEXT NOT NULL DEFAULT '',
	tier      TEXT NOT NULL,
	rationale TEXT NOT NULL,
	set_by    TEXT NOT NULL,
	set_at    TEXT NOT NULL,
	PRIMARY KEY (entity_id, version)
);
INSERT INTO postures_v2 (entity_id, version, tier, rationale, set_by, set_at)
	SELECT entity_id, version, tier, rationale, set_by, set_at FROM postures;
DROP TABLE postures;
ALTER TABLE postures_v2 RENAME TO postures;

-- Dependency observations (append-only).
CREATE TABLE IF NOT EXISTS dependency_observations (
	id          TEXT PRIMARY KEY,
	project_id  TEXT NOT NULL REFERENCES entities(id),
	entity_id   TEXT NOT NULL REFERENCES entities(id),
	version     TEXT NOT NULL,
	direct      INTEGER NOT NULL,
	observed_at TEXT NOT NULL,
	survey_id   TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_depobs_project ON dependency_observations(project_id);
CREATE INDEX IF NOT EXISTS idx_depobs_survey ON dependency_observations(survey_id);

-- Signal resolutions (append-only conflict resolution).
CREATE TABLE IF NOT EXISTS signal_resolutions (
	id                   TEXT PRIMARY KEY,
	entity_id            TEXT NOT NULL,
	signal_type          TEXT NOT NULL,
	kept_signal_id       TEXT NOT NULL REFERENCES signals(id),
	superseded_signal_id TEXT NOT NULL REFERENCES signals(id),
	action               TEXT NOT NULL,
	resolved_by          TEXT NOT NULL,
	resolved_at          TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_resolutions_entity ON signal_resolutions(entity_id);

-- Audit log (append-only).
CREATE TABLE IF NOT EXISTS audit_log (
	id         TEXT PRIMARY KEY,
	timestamp  TEXT NOT NULL,
	actor      TEXT NOT NULL,
	action     TEXT NOT NULL,
	entity_id  TEXT,
	detail     TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_audit_timestamp ON audit_log(timestamp);
CREATE INDEX IF NOT EXISTS idx_audit_entity ON audit_log(entity_id);

-- Team identities.
CREATE TABLE IF NOT EXISTS team_identities (
	id            TEXT PRIMARY KEY,
	name          TEXT NOT NULL,
	created_at    TEXT NOT NULL,
	halted_at     TEXT,
	revoked_at    TEXT,
	revoke_reason TEXT
);
`

// migrationV2Down rolls back migration v2 to a readable v1 state.
//
// Rollback semantics per design/entity-model-v2.md:246 — rollback is a
// recovery mechanism, not a feature. Data only present in v2 (audit log,
// dependency observations, signal resolutions, team identities, posture
// version history) is lost on rollback. The v1 `entities` and `signals`
// tables are fully restored, and `postures` collapses to one row per
// entity by keeping whichever row SQLite returns last (order-insensitive
// rollback — the user gets a working v1 schema, not a time machine).
//
// Order matters here too. To restore the v1 `name` column we must:
//  1. Drop the v2 indexes that reference canonical_uri / type
//  2. Re-add the `name` column
//  3. Copy short_name → name so v1 code can read it
//  4. Drop the v2 columns
//  5. Recreate the v1 index on (name, type)
//
// Dropping v2 indexes before v2 columns avoids the same DROP COLUMN
// "column in use by index" error that bit us on the up-path.
const migrationV2Down = `
-- Drop v2-only tables (their data is lost by design).
DROP TABLE IF EXISTS team_identities;
DROP TABLE IF EXISTS audit_log;
DROP TABLE IF EXISTS signal_resolutions;
DROP TABLE IF EXISTS dependency_observations;

-- Recreate postures with original PK (entity_id only). Version history
-- is collapsed — we keep one row per entity, whichever the SELECT
-- yields last. This is acceptable loss for a recovery rollback.
CREATE TABLE postures_v1 (
	entity_id TEXT PRIMARY KEY REFERENCES entities(id),
	tier      TEXT NOT NULL,
	version   TEXT NOT NULL DEFAULT '',
	rationale TEXT NOT NULL,
	set_by    TEXT NOT NULL,
	set_at    TEXT NOT NULL
);
INSERT OR REPLACE INTO postures_v1 (entity_id, version, tier, rationale, set_by, set_at)
	SELECT entity_id, version, tier, rationale, set_by, set_at FROM postures;
DROP TABLE postures;
ALTER TABLE postures_v1 RENAME TO postures;

-- Drop v2 entity indexes before dropping v2 columns — SQLite DROP
-- COLUMN fails if the column is referenced by an index.
DROP INDEX IF EXISTS idx_entities_type;
DROP INDEX IF EXISTS idx_entities_canonical_uri;

-- Restore v1 name column and populate it from short_name so v1 code
-- can still read these rows.
ALTER TABLE entities ADD COLUMN name TEXT NOT NULL DEFAULT '';
UPDATE entities SET name = short_name;

-- Drop the v2-only entity columns.
-- SQLite 3.35.0+ supports ALTER TABLE DROP COLUMN. The modernc driver
-- ships SQLite 3.51+, so this is safe.
ALTER TABLE entities DROP COLUMN canonical_uri;
ALTER TABLE entities DROP COLUMN short_name;
ALTER TABLE entities DROP COLUMN description;

-- Recreate the v1 index on the restored name column.
CREATE INDEX IF NOT EXISTS idx_entities_name_type ON entities(name, type);
`

// migrationV3Up adds BEFORE UPDATE and BEFORE DELETE triggers to the
// four append-only tables. This converts the convention documented at
// sqlite.go:8 (signals, dependency observations, signal resolutions,
// and audit entries are append-only) into a schema-enforced invariant.
//
// The triggers fire on any UPDATE or DELETE statement, regardless of
// source — Go application code, raw SQL via the sqlite3 shell, or
// anything else that goes through the SQLite parser. RAISE(ABORT, ...)
// aborts the offending statement and rolls back any pending changes
// from that statement; the surrounding transaction is not affected
// unless the caller chooses to roll it back.
//
// Tables that are NOT append-only — entities, postures, burns,
// team_identities — are intentionally left mutable. Their methods
// (PutEntity, SetPosture, SetBurn, PutTeamIdentity) upsert.
const migrationV3Up = `
CREATE TRIGGER signals_no_update BEFORE UPDATE ON signals
    BEGIN SELECT RAISE(ABORT, 'signals are append-only'); END;
CREATE TRIGGER signals_no_delete BEFORE DELETE ON signals
    BEGIN SELECT RAISE(ABORT, 'signals are append-only'); END;

CREATE TRIGGER dependency_observations_no_update BEFORE UPDATE ON dependency_observations
    BEGIN SELECT RAISE(ABORT, 'dependency_observations are append-only'); END;
CREATE TRIGGER dependency_observations_no_delete BEFORE DELETE ON dependency_observations
    BEGIN SELECT RAISE(ABORT, 'dependency_observations are append-only'); END;

CREATE TRIGGER signal_resolutions_no_update BEFORE UPDATE ON signal_resolutions
    BEGIN SELECT RAISE(ABORT, 'signal_resolutions are append-only'); END;
CREATE TRIGGER signal_resolutions_no_delete BEFORE DELETE ON signal_resolutions
    BEGIN SELECT RAISE(ABORT, 'signal_resolutions are append-only'); END;

CREATE TRIGGER audit_log_no_update BEFORE UPDATE ON audit_log
    BEGIN SELECT RAISE(ABORT, 'audit_log is append-only'); END;
CREATE TRIGGER audit_log_no_delete BEFORE DELETE ON audit_log
    BEGIN SELECT RAISE(ABORT, 'audit_log is append-only'); END;
`

// migrationV3Down drops the append-only triggers, restoring the
// pre-v3 behavior where these tables can be mutated freely. This is
// a recovery path only — running it on a populated database removes
// the schema-level append-only enforcement and reverts to convention.
const migrationV3Down = `
DROP TRIGGER IF EXISTS audit_log_no_delete;
DROP TRIGGER IF EXISTS audit_log_no_update;
DROP TRIGGER IF EXISTS signal_resolutions_no_delete;
DROP TRIGGER IF EXISTS signal_resolutions_no_update;
DROP TRIGGER IF EXISTS dependency_observations_no_delete;
DROP TRIGGER IF EXISTS dependency_observations_no_update;
DROP TRIGGER IF EXISTS signals_no_delete;
DROP TRIGGER IF EXISTS signals_no_update;
`

// migrationV4Up adds the analyst-output stream — the tables that
// hold structured `exchange.AnalystOutput` documents per
// design/ingestion-plan.md — and the proposed signals.details
// JSON column + signal_evidence table from
// design/signal-storage-evolution.md. Lands together because they
// constitute one logical schema update (richer storage for both
// streams: signals get JSON details + raw evidence; AnalystOutput
// gets a parallel structured stream).
//
// All new tables are append-only (triggers below). They use
// foreign keys into the existing entities table; identity entities
// are referenced by canonical_uri-driven lookup at insert time.
//
// Re-ingestion idempotency is enforced by analyst_outputs.content_hash
// being UNIQUE.
const migrationV4Up = `
-- ===== signals enrichment =====
ALTER TABLE signals ADD COLUMN details TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS signal_evidence (
    id           TEXT PRIMARY KEY,
    signal_id    TEXT NOT NULL REFERENCES signals(id),
    kind         TEXT NOT NULL,
    origin       TEXT NOT NULL,
    content_hash TEXT NOT NULL,
    content      BLOB NOT NULL,
    captured_at  TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_evidence_signal ON signal_evidence(signal_id);
CREATE INDEX IF NOT EXISTS idx_evidence_kind ON signal_evidence(kind);

-- ===== analyst-output stream =====

-- Top-level envelope. One row per ingested AnalystOutput.
CREATE TABLE IF NOT EXISTS analyst_outputs (
    id              TEXT PRIMARY KEY,
    entity_id       TEXT NOT NULL REFERENCES entities(id),
    analyst_id      TEXT NOT NULL,
    model           TEXT NOT NULL,
    prompt_version  TEXT NOT NULL DEFAULT '',
    invoked_at      TEXT NOT NULL,
    ingested_at     TEXT NOT NULL,
    round           INTEGER NOT NULL DEFAULT 1,
    target_commit   TEXT NOT NULL DEFAULT '',
    round_notes     TEXT NOT NULL DEFAULT '',
    source_path     TEXT NOT NULL DEFAULT '',
    content_hash    TEXT NOT NULL UNIQUE
);
CREATE INDEX IF NOT EXISTS idx_outputs_entity ON analyst_outputs(entity_id);
CREATE INDEX IF NOT EXISTS idx_outputs_analyst_target ON analyst_outputs(analyst_id, entity_id, invoked_at);

-- One row per Finding within an output.
CREATE TABLE IF NOT EXISTS findings (
    id                  TEXT PRIMARY KEY,
    output_id           TEXT NOT NULL REFERENCES analyst_outputs(id),
    finding_local_id    TEXT NOT NULL,
    verdict             TEXT NOT NULL,
    rationale           TEXT NOT NULL,
    severity_default    TEXT NOT NULL,
    design_intent       INTEGER NOT NULL DEFAULT 0,
    category            TEXT NOT NULL,
    signal_type         TEXT NOT NULL DEFAULT '',
    answers_question    TEXT NOT NULL DEFAULT '',
    UNIQUE (output_id, finding_local_id)
);
CREATE INDEX IF NOT EXISTS idx_findings_output ON findings(output_id);
CREATE INDEX IF NOT EXISTS idx_findings_severity ON findings(severity_default);
CREATE INDEX IF NOT EXISTS idx_findings_signal_type ON findings(signal_type);

-- Conditional severity overrides per (host_isolation, platform).
CREATE TABLE IF NOT EXISTS finding_severity_contexts (
    finding_id      TEXT NOT NULL REFERENCES findings(id),
    host_isolation  TEXT NOT NULL DEFAULT '',
    platform        TEXT NOT NULL DEFAULT '',
    value           TEXT NOT NULL,
    PRIMARY KEY (finding_id, host_isolation, platform)
);

-- Supersession: this finding revises one or more priors.
CREATE TABLE IF NOT EXISTS finding_supersedes (
    finding_id      TEXT NOT NULL REFERENCES findings(id),
    prior_id        TEXT NOT NULL,
    prior_round     INTEGER NOT NULL DEFAULT 0,
    kind            TEXT NOT NULL,
    PRIMARY KEY (finding_id, prior_id)
);

-- Per-Finding prerequisites (ordered list).
CREATE TABLE IF NOT EXISTS finding_prerequisites (
    finding_id  TEXT NOT NULL REFERENCES findings(id),
    seq         INTEGER NOT NULL,
    text        TEXT NOT NULL,
    PRIMARY KEY (finding_id, seq)
);

-- Per-Finding remediation hints (ordered list).
CREATE TABLE IF NOT EXISTS finding_remediation_hints (
    finding_id  TEXT NOT NULL REFERENCES findings(id),
    seq         INTEGER NOT NULL,
    text        TEXT NOT NULL,
    PRIMARY KEY (finding_id, seq)
);

-- Cross-references between findings within the same output.
CREATE TABLE IF NOT EXISTS finding_related (
    finding_id  TEXT NOT NULL REFERENCES findings(id),
    related_id  TEXT NOT NULL,
    PRIMARY KEY (finding_id, related_id)
);

-- Positive absences (pattern checked, not found).
CREATE TABLE IF NOT EXISTS positive_absences (
    id                  TEXT PRIMARY KEY,
    output_id           TEXT NOT NULL REFERENCES analyst_outputs(id),
    pattern_checked     TEXT NOT NULL,
    description         TEXT NOT NULL,
    confidence          TEXT NOT NULL,
    pattern_ref         TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_absences_output ON positive_absences(output_id);

-- Observations: trust-model commentary that doesn't fit Finding shape.
CREATE TABLE IF NOT EXISTS observations (
    id                      TEXT PRIMARY KEY,
    output_id               TEXT NOT NULL REFERENCES analyst_outputs(id),
    observation_local_id    TEXT NOT NULL,
    title                   TEXT NOT NULL,
    body                    TEXT NOT NULL,
    category                TEXT NOT NULL,
    signal_type             TEXT NOT NULL DEFAULT '',
    UNIQUE (output_id, observation_local_id)
);
CREATE INDEX IF NOT EXISTS idx_observations_output ON observations(output_id);

-- Methodology catalog: one per output.
CREATE TABLE IF NOT EXISTS methodology_catalogs (
    output_id           TEXT PRIMARY KEY REFERENCES analyst_outputs(id),
    source_analyst_id   TEXT NOT NULL,
    source_model        TEXT NOT NULL,
    source_invoked_at   TEXT NOT NULL,
    notes               TEXT NOT NULL DEFAULT ''
);

-- Methodology patterns within a catalog.
CREATE TABLE IF NOT EXISTS methodology_patterns (
    id                       TEXT PRIMARY KEY,
    output_id                TEXT NOT NULL REFERENCES analyst_outputs(id),
    pattern_local_id         TEXT NOT NULL,
    signal_group             TEXT NOT NULL,
    description              TEXT NOT NULL,
    pattern_text             TEXT NOT NULL DEFAULT '',
    grep_precision           TEXT NOT NULL,
    reasoning_depth          TEXT NOT NULL,
    miss_mode                TEXT NOT NULL DEFAULT '',
    false_positive_notes     TEXT NOT NULL DEFAULT '',
    -- hit_on_target is nullable via convention: -1=null, 0=false, 1=true
    -- (SQLite doesn't enforce a real BOOLEAN type; we use INTEGER)
    hit_on_target            INTEGER NOT NULL DEFAULT -1,
    UNIQUE (output_id, pattern_local_id)
);
CREATE INDEX IF NOT EXISTS idx_patterns_signal_group ON methodology_patterns(signal_group);
CREATE INDEX IF NOT EXISTS idx_patterns_hit ON methodology_patterns(hit_on_target);

-- Pattern composition.
CREATE TABLE IF NOT EXISTS methodology_pattern_composes (
    pattern_id      TEXT NOT NULL REFERENCES methodology_patterns(id),
    composes_with   TEXT NOT NULL,
    PRIMARY KEY (pattern_id, composes_with)
);

-- Citations: polymorphic FK via parent_kind + parent_id.
-- parent_kind in {conclusion, positive_absence, observation} (CHECK added in v9).
-- The v4 comment here historically named 'finding' (renamed to 'conclusion' by
-- v5) and 'methodology_pattern' (never inserted — MethodologyPattern has no
-- Citations field). v9's CHECK constraint is the authoritative list.
CREATE TABLE IF NOT EXISTS citations (
    id              TEXT PRIMARY KEY,
    parent_kind     TEXT NOT NULL,
    parent_id       TEXT NOT NULL,
    seq             INTEGER NOT NULL,
    path            TEXT NOT NULL DEFAULT '',
    -- line_start / line_end as INTEGER nullables: -1 means null (Citation
    -- uses Scope alternative when line_start is unset).
    line_start      INTEGER NOT NULL DEFAULT -1,
    line_end        INTEGER NOT NULL DEFAULT -1,
    scope_kind      TEXT NOT NULL DEFAULT '',
    scope_path      TEXT NOT NULL DEFAULT '',
    commit_sha      TEXT NOT NULL DEFAULT '',
    quoted          TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_citations_parent ON citations(parent_kind, parent_id);
CREATE INDEX IF NOT EXISTS idx_citations_path ON citations(path);

-- Top-level supersession: output supersedes prior outputs.
CREATE TABLE IF NOT EXISTS output_supersedes (
    output_id       TEXT NOT NULL REFERENCES analyst_outputs(id),
    prior_id        TEXT NOT NULL,
    prior_round     INTEGER NOT NULL DEFAULT 0,
    kind            TEXT NOT NULL,
    PRIMARY KEY (output_id, prior_id)
);

-- Output reframes_from: free-text notes on cross-analyst engagement.
CREATE TABLE IF NOT EXISTS output_reframes_from (
    output_id   TEXT NOT NULL REFERENCES analyst_outputs(id),
    seq         INTEGER NOT NULL,
    text        TEXT NOT NULL,
    PRIMARY KEY (output_id, seq)
);

-- ===== append-only triggers =====
-- Same pattern as migration v3. Every new table is append-only;
-- query layer filters to current state.

CREATE TRIGGER signal_evidence_no_update BEFORE UPDATE ON signal_evidence
    BEGIN SELECT RAISE(ABORT, 'signal_evidence is append-only'); END;
CREATE TRIGGER signal_evidence_no_delete BEFORE DELETE ON signal_evidence
    BEGIN SELECT RAISE(ABORT, 'signal_evidence is append-only'); END;

CREATE TRIGGER analyst_outputs_no_update BEFORE UPDATE ON analyst_outputs
    BEGIN SELECT RAISE(ABORT, 'analyst_outputs are append-only'); END;
CREATE TRIGGER analyst_outputs_no_delete BEFORE DELETE ON analyst_outputs
    BEGIN SELECT RAISE(ABORT, 'analyst_outputs are append-only'); END;

CREATE TRIGGER findings_no_update BEFORE UPDATE ON findings
    BEGIN SELECT RAISE(ABORT, 'findings are append-only'); END;
CREATE TRIGGER findings_no_delete BEFORE DELETE ON findings
    BEGIN SELECT RAISE(ABORT, 'findings are append-only'); END;

CREATE TRIGGER finding_severity_contexts_no_update BEFORE UPDATE ON finding_severity_contexts
    BEGIN SELECT RAISE(ABORT, 'finding_severity_contexts are append-only'); END;
CREATE TRIGGER finding_severity_contexts_no_delete BEFORE DELETE ON finding_severity_contexts
    BEGIN SELECT RAISE(ABORT, 'finding_severity_contexts are append-only'); END;

CREATE TRIGGER finding_supersedes_no_update BEFORE UPDATE ON finding_supersedes
    BEGIN SELECT RAISE(ABORT, 'finding_supersedes are append-only'); END;
CREATE TRIGGER finding_supersedes_no_delete BEFORE DELETE ON finding_supersedes
    BEGIN SELECT RAISE(ABORT, 'finding_supersedes are append-only'); END;

CREATE TRIGGER finding_prerequisites_no_update BEFORE UPDATE ON finding_prerequisites
    BEGIN SELECT RAISE(ABORT, 'finding_prerequisites are append-only'); END;
CREATE TRIGGER finding_prerequisites_no_delete BEFORE DELETE ON finding_prerequisites
    BEGIN SELECT RAISE(ABORT, 'finding_prerequisites are append-only'); END;

CREATE TRIGGER finding_remediation_hints_no_update BEFORE UPDATE ON finding_remediation_hints
    BEGIN SELECT RAISE(ABORT, 'finding_remediation_hints are append-only'); END;
CREATE TRIGGER finding_remediation_hints_no_delete BEFORE DELETE ON finding_remediation_hints
    BEGIN SELECT RAISE(ABORT, 'finding_remediation_hints are append-only'); END;

CREATE TRIGGER finding_related_no_update BEFORE UPDATE ON finding_related
    BEGIN SELECT RAISE(ABORT, 'finding_related are append-only'); END;
CREATE TRIGGER finding_related_no_delete BEFORE DELETE ON finding_related
    BEGIN SELECT RAISE(ABORT, 'finding_related are append-only'); END;

CREATE TRIGGER positive_absences_no_update BEFORE UPDATE ON positive_absences
    BEGIN SELECT RAISE(ABORT, 'positive_absences are append-only'); END;
CREATE TRIGGER positive_absences_no_delete BEFORE DELETE ON positive_absences
    BEGIN SELECT RAISE(ABORT, 'positive_absences are append-only'); END;

CREATE TRIGGER observations_no_update BEFORE UPDATE ON observations
    BEGIN SELECT RAISE(ABORT, 'observations are append-only'); END;
CREATE TRIGGER observations_no_delete BEFORE DELETE ON observations
    BEGIN SELECT RAISE(ABORT, 'observations are append-only'); END;

CREATE TRIGGER methodology_catalogs_no_update BEFORE UPDATE ON methodology_catalogs
    BEGIN SELECT RAISE(ABORT, 'methodology_catalogs are append-only'); END;
CREATE TRIGGER methodology_catalogs_no_delete BEFORE DELETE ON methodology_catalogs
    BEGIN SELECT RAISE(ABORT, 'methodology_catalogs are append-only'); END;

CREATE TRIGGER methodology_patterns_no_update BEFORE UPDATE ON methodology_patterns
    BEGIN SELECT RAISE(ABORT, 'methodology_patterns are append-only'); END;
CREATE TRIGGER methodology_patterns_no_delete BEFORE DELETE ON methodology_patterns
    BEGIN SELECT RAISE(ABORT, 'methodology_patterns are append-only'); END;

CREATE TRIGGER methodology_pattern_composes_no_update BEFORE UPDATE ON methodology_pattern_composes
    BEGIN SELECT RAISE(ABORT, 'methodology_pattern_composes are append-only'); END;
CREATE TRIGGER methodology_pattern_composes_no_delete BEFORE DELETE ON methodology_pattern_composes
    BEGIN SELECT RAISE(ABORT, 'methodology_pattern_composes are append-only'); END;

CREATE TRIGGER citations_no_update BEFORE UPDATE ON citations
    BEGIN SELECT RAISE(ABORT, 'citations are append-only'); END;
CREATE TRIGGER citations_no_delete BEFORE DELETE ON citations
    BEGIN SELECT RAISE(ABORT, 'citations are append-only'); END;

CREATE TRIGGER output_supersedes_no_update BEFORE UPDATE ON output_supersedes
    BEGIN SELECT RAISE(ABORT, 'output_supersedes are append-only'); END;
CREATE TRIGGER output_supersedes_no_delete BEFORE DELETE ON output_supersedes
    BEGIN SELECT RAISE(ABORT, 'output_supersedes are append-only'); END;

CREATE TRIGGER output_reframes_from_no_update BEFORE UPDATE ON output_reframes_from
    BEGIN SELECT RAISE(ABORT, 'output_reframes_from are append-only'); END;
CREATE TRIGGER output_reframes_from_no_delete BEFORE DELETE ON output_reframes_from
    BEGIN SELECT RAISE(ABORT, 'output_reframes_from are append-only'); END;
`

// migrationV4Down rolls back the analyst-output stream and the
// signal enrichment additions. Data in the dropped tables is lost
// — they're net-new in v4 with no v3 precedent to roll back into.
//
// The signals.details column drop is the only data-loss in the
// existing-table modification; existing signals rows survive with
// their original columns intact.
const migrationV4Down = `
-- Drop triggers first so DROP TABLE works.
DROP TRIGGER IF EXISTS output_reframes_from_no_delete;
DROP TRIGGER IF EXISTS output_reframes_from_no_update;
DROP TRIGGER IF EXISTS output_supersedes_no_delete;
DROP TRIGGER IF EXISTS output_supersedes_no_update;
DROP TRIGGER IF EXISTS citations_no_delete;
DROP TRIGGER IF EXISTS citations_no_update;
DROP TRIGGER IF EXISTS methodology_pattern_composes_no_delete;
DROP TRIGGER IF EXISTS methodology_pattern_composes_no_update;
DROP TRIGGER IF EXISTS methodology_patterns_no_delete;
DROP TRIGGER IF EXISTS methodology_patterns_no_update;
DROP TRIGGER IF EXISTS methodology_catalogs_no_delete;
DROP TRIGGER IF EXISTS methodology_catalogs_no_update;
DROP TRIGGER IF EXISTS observations_no_delete;
DROP TRIGGER IF EXISTS observations_no_update;
DROP TRIGGER IF EXISTS positive_absences_no_delete;
DROP TRIGGER IF EXISTS positive_absences_no_update;
DROP TRIGGER IF EXISTS finding_related_no_delete;
DROP TRIGGER IF EXISTS finding_related_no_update;
DROP TRIGGER IF EXISTS finding_remediation_hints_no_delete;
DROP TRIGGER IF EXISTS finding_remediation_hints_no_update;
DROP TRIGGER IF EXISTS finding_prerequisites_no_delete;
DROP TRIGGER IF EXISTS finding_prerequisites_no_update;
DROP TRIGGER IF EXISTS finding_supersedes_no_delete;
DROP TRIGGER IF EXISTS finding_supersedes_no_update;
DROP TRIGGER IF EXISTS finding_severity_contexts_no_delete;
DROP TRIGGER IF EXISTS finding_severity_contexts_no_update;
DROP TRIGGER IF EXISTS findings_no_delete;
DROP TRIGGER IF EXISTS findings_no_update;
DROP TRIGGER IF EXISTS analyst_outputs_no_delete;
DROP TRIGGER IF EXISTS analyst_outputs_no_update;
DROP TRIGGER IF EXISTS signal_evidence_no_delete;
DROP TRIGGER IF EXISTS signal_evidence_no_update;

-- Drop tables in reverse-FK order.
DROP TABLE IF EXISTS output_reframes_from;
DROP TABLE IF EXISTS output_supersedes;
DROP TABLE IF EXISTS citations;
DROP TABLE IF EXISTS methodology_pattern_composes;
DROP TABLE IF EXISTS methodology_patterns;
DROP TABLE IF EXISTS methodology_catalogs;
DROP TABLE IF EXISTS observations;
DROP TABLE IF EXISTS positive_absences;
DROP TABLE IF EXISTS finding_related;
DROP TABLE IF EXISTS finding_remediation_hints;
DROP TABLE IF EXISTS finding_prerequisites;
DROP TABLE IF EXISTS finding_supersedes;
DROP TABLE IF EXISTS finding_severity_contexts;
DROP TABLE IF EXISTS findings;
DROP TABLE IF EXISTS analyst_outputs;
DROP TABLE IF EXISTS signal_evidence;

-- Drop the v4 column from signals. Existing signal rows are
-- preserved; the details column is removed.
ALTER TABLE signals DROP COLUMN details;
`

// migrationV5Up renames the six finding-prefixed tables to their
// conclusion-prefixed equivalents, and renames the finding_id column
// in each child table to conclusion_id. The findings table itself
// also gets its finding_local_id column renamed to conclusion_local_id.
//
// SQLite supports ALTER TABLE … RENAME (table and column) since 3.25.0
// (2018). The modernc.org/sqlite driver ships SQLite 3.51+, so all
// renames are safe.
//
// Each RENAME is reversible; migrationV5Down reverses the sequence.
// Triggers and indexes are NOT renamed here — SQLite carries them with
// the table on RENAME TABLE and does not support ALTER INDEX RENAME;
// the trigger names stay findings_no_update etc. They still fire on the
// renamed table because SQLite binds triggers to table OIDs, not names.
// This is intentional: the triggers were created on the v4 table and
// remain correct on the renamed table.
const migrationV5Up = `
-- Rename the main table and its internal column.
ALTER TABLE findings RENAME TO conclusions;
ALTER TABLE conclusions RENAME COLUMN finding_local_id TO conclusion_local_id;

-- Rename child tables and their FK columns.
ALTER TABLE finding_severity_contexts RENAME TO conclusion_severity_contexts;
ALTER TABLE conclusion_severity_contexts RENAME COLUMN finding_id TO conclusion_id;

ALTER TABLE finding_supersedes RENAME TO conclusion_supersedes;
ALTER TABLE conclusion_supersedes RENAME COLUMN finding_id TO conclusion_id;

ALTER TABLE finding_prerequisites RENAME TO conclusion_prerequisites;
ALTER TABLE conclusion_prerequisites RENAME COLUMN finding_id TO conclusion_id;

ALTER TABLE finding_remediation_hints RENAME TO conclusion_remediation_hints;
ALTER TABLE conclusion_remediation_hints RENAME COLUMN finding_id TO conclusion_id;

ALTER TABLE finding_related RENAME TO conclusion_related;
ALTER TABLE conclusion_related RENAME COLUMN finding_id TO conclusion_id;
`

// migrationV5Down reverses migrationV5Up: renames conclusion-prefixed
// tables and columns back to their finding-prefixed originals.
// This is a recovery path only — running it on a populated database
// that has been used via the v5 code reverts to the v4 naming.
const migrationV5Down = `
ALTER TABLE conclusion_related RENAME COLUMN conclusion_id TO finding_id;
ALTER TABLE conclusion_related RENAME TO finding_related;

ALTER TABLE conclusion_remediation_hints RENAME COLUMN conclusion_id TO finding_id;
ALTER TABLE conclusion_remediation_hints RENAME TO finding_remediation_hints;

ALTER TABLE conclusion_prerequisites RENAME COLUMN conclusion_id TO finding_id;
ALTER TABLE conclusion_prerequisites RENAME TO finding_prerequisites;

ALTER TABLE conclusion_supersedes RENAME COLUMN conclusion_id TO finding_id;
ALTER TABLE conclusion_supersedes RENAME TO finding_supersedes;

ALTER TABLE conclusion_severity_contexts RENAME COLUMN conclusion_id TO finding_id;
ALTER TABLE conclusion_severity_contexts RENAME TO finding_severity_contexts;

ALTER TABLE conclusions RENAME COLUMN conclusion_local_id TO finding_local_id;
ALTER TABLE conclusions RENAME TO findings;
`

// migrationV6Up adds soft-delete columns to postures and burns so the
// M4 undo verbs (posture unset, burn remove) can mark a row withdrawn
// without dropping it. Reads filter WHERE withdrawn_at IS NULL by
// default. A re-set after unset clears these fields via the existing
// SetPosture/SetBurn upsert paths (the UPDATE clause in ON CONFLICT
// is extended to reset them).
//
// withdrawal_reason is optional context the caller can supply —
// "author left the org", "reassessment pending", etc. The audit_log
// remains the canonical event stream; these columns are for fast
// "is this row active?" lookup at read time.
//
// ingest withdraw (the third undo verb from agent-facing-contract
// M4) is NOT implemented by this migration. analyst_outputs carries
// append-only triggers from v3; marking an output INGEST_ERROR needs
// a sibling-table design that's meaningfully different from the
// posture/burn shape and lands in its own commit.
const migrationV6Up = `
ALTER TABLE postures ADD COLUMN withdrawn_at TEXT NOT NULL DEFAULT '';
ALTER TABLE postures ADD COLUMN withdrawn_by TEXT NOT NULL DEFAULT '';
ALTER TABLE postures ADD COLUMN withdrawal_reason TEXT NOT NULL DEFAULT '';

ALTER TABLE burns ADD COLUMN withdrawn_at TEXT NOT NULL DEFAULT '';
ALTER TABLE burns ADD COLUMN withdrawn_by TEXT NOT NULL DEFAULT '';
ALTER TABLE burns ADD COLUMN withdrawal_reason TEXT NOT NULL DEFAULT '';
`

// migrationV6Down removes the soft-delete columns. Running it on a
// database that has withdrawn rows drops that metadata — but the
// audit_log retains the unset / remove events so the history isn't
// wholly lost on rollback.
const migrationV6Down = `
ALTER TABLE postures DROP COLUMN withdrawal_reason;
ALTER TABLE postures DROP COLUMN withdrawn_by;
ALTER TABLE postures DROP COLUMN withdrawn_at;

ALTER TABLE burns DROP COLUMN withdrawal_reason;
ALTER TABLE burns DROP COLUMN withdrawn_by;
ALTER TABLE burns DROP COLUMN withdrawn_at;
`

// migrationV7Up adds collected_from_entity_id to analyst_outputs so
// the store can index an analysis under the caller's URI while
// retaining a link to the identity the work was actually performed
// against. This is the schema leg of agent-facing-contract §3.2
// (transparent-with-citation resolution): an analyst run against
// repo:github/X by a caller asking about pkg:npm/Y is stored with
// entity_id → pkg:npm/Y's entity and collected_from_entity_id →
// repo:github/X's entity, so queries under either URI surface the
// record.
//
// Nullable by default — pre-M2 rows and rows where the caller's
// URI matches the analyst's target have NULL, treated semantically
// as "no resolution hop." The index speeds reverse lookups when
// queries come in under the resolved-source URI.
//
// Per D2: no backfill. Existing rows stay as they are; users re-run
// /analyze on anything they care about.
const migrationV7Up = `
ALTER TABLE analyst_outputs ADD COLUMN collected_from_entity_id TEXT REFERENCES entities(id);
CREATE INDEX IF NOT EXISTS idx_outputs_collected_from ON analyst_outputs(collected_from_entity_id);
`

// migrationV7Down drops the M2 column and its index.
const migrationV7Down = `
DROP INDEX IF EXISTS idx_outputs_collected_from;
ALTER TABLE analyst_outputs DROP COLUMN collected_from_entity_id;
`

// migrationV8Up adds the synthesis-supplement columns to
// analyst_outputs for agent-facing-contract M6a. Three nullable
// columns:
//
//   - synthesis_supplement_json: opaque JSON blob carrying the full
//     SynthesisSupplement (reasoning, summary, concordance,
//     contradictions, conclusion refs, gaps, action_items, notes).
//     Display-only — never queried at the SQL layer.
//
//   - proposed_tier: denormalized from synthesis_supplement.
//     proposed_posture.tier. Read by `signatory posture accept`
//     (M6d) to avoid JSON unmarshaling, and by Summary (M7) to
//     surface pending-recommendation state without unpacking the
//     blob.
//
//   - proposed_version_scope: denormalized from synthesis_supplement.
//     proposed_posture.version_scope. Feeds the posture row's
//     version on accept.
//
// All three are NULL on every existing row (security/provenance
// outputs never carry a supplement) and on non-synthesist rows going
// forward (validator-enforced). Non-null iff attribution.analyst_id
// starts with "signatory-synthesis". The invariant is enforced at the
// Go validator layer; no CHECK constraint here (SQLite CHECK on
// cross-column JSON introspection is clumsy and the validator runs
// on every write path).
//
// Per D2 (agent-facing-contract.md): no backfill. The current
// filestore synthesis markdowns are historical artifacts; new
// syntheses land in the store via M6e's skill update.
//
// Indexing rationale: no index on proposed_tier/proposed_version_scope
// for v0.1. The expected read pattern is `posture accept <output-id>`
// (primary-key lookup on analyst_outputs.id, the JSON-adjacent columns
// come along for the ride) and Summary rollups (already bounded by
// entity_id). Add indexes when a "find all syntheses proposing tier X"
// query surfaces.
const migrationV8Up = `
ALTER TABLE analyst_outputs ADD COLUMN synthesis_supplement_json TEXT;
ALTER TABLE analyst_outputs ADD COLUMN proposed_tier TEXT;
ALTER TABLE analyst_outputs ADD COLUMN proposed_version_scope TEXT;
`

// migrationV8Down drops the three M6a columns.
const migrationV8Down = `
ALTER TABLE analyst_outputs DROP COLUMN proposed_version_scope;
ALTER TABLE analyst_outputs DROP COLUMN proposed_tier;
ALTER TABLE analyst_outputs DROP COLUMN synthesis_supplement_json;
`

// migrationV9Up adds a CHECK constraint to citations.parent_kind
// pinning it to the three production values {conclusion,
// positive_absence, observation}. Closes F002 from
// design/analysis/signatory-security-v1.json: the v4 schema
// comment named four valid values but the table imposed zero
// constraint, so a stray tx.Exec with a typo'd parent_kind would
// land silently and produce rows invisible to every later query.
//
// The MethodologyPattern type has no Citations field (see
// internal/exchange/types.go), so 'methodology_pattern' — named
// in the v4 schema comment as if it were a legitimate parent_kind —
// was always a stale-comment fiction. It is not in the CHECK list.
//
// Pre-v5 data cleanup: any citations row with parent_kind='finding'
// (inserted before v5's Finding→Conclusion rename) is rewritten to
// 'conclusion' before the rebuild, so the CHECK doesn't fail on
// legacy data. No-op on clean databases. Not reversible by v9Down
// — once cleaned, the rows stay consistent with post-v5 code, which
// is what you want.
//
// SQLite has no ALTER TABLE ADD CONSTRAINT, so the CHECK is
// installed via the standard rebuild-through-new-table ceremony:
// create _new with the constraint, copy rows, drop old, rename.
// Indexes drop with the table and are recreated afterward. No
// FKs in or out of citations, so no foreign_keys PRAGMA gymnastics.
//
// Append-only trigger ceremony: citations carries the v3 append-
// only triggers (citations_no_update, citations_no_delete) — see
// migrationV3Up. Both the legacy 'finding'→'conclusion' UPDATE and
// the rebuild's DROP TABLE would be blocked by these triggers
// (the UPDATE directly; DROP via per-row delete in some SQLite
// versions). v9Up explicitly drops the triggers at the start, runs
// the cleanup + rebuild, then reinstalls them on the rebuilt
// table. The triggers cannot survive the table rebuild even if we
// left them in place, since SQLite binds them to the table's OID
// and DROP TABLE invalidates the OID.
//
// If the rebuild INSERT fails on a row whose parent_kind isn't in
// the CHECK list (and isn't the 'finding' value we cleaned up),
// the migration aborts its transaction and the operator keeps the
// v8 schema — intended behavior. They'd need to manually identify
// and clean the anomalous row before re-running. For v0.1 solo-
// dogfood scope, this is acceptable; no automated remediation is
// warranted for an edge case that requires someone to have
// inserted a non-production value into the store.
const migrationV9Up = `
-- Step 1: Drop the v3-installed append-only triggers temporarily.
-- They would block the legacy 'finding' UPDATE below and would not
-- survive the table rebuild regardless (SQLite binds triggers to
-- table OIDs; DROP TABLE invalidates the binding).
DROP TRIGGER IF EXISTS citations_no_update;
DROP TRIGGER IF EXISTS citations_no_delete;

-- Step 2: Defensive cleanup — pre-v5 'finding' → post-v5 'conclusion'.
-- Safe no-op on databases that never held pre-v5 data.
UPDATE citations SET parent_kind = 'conclusion' WHERE parent_kind = 'finding';

-- Step 3: Rebuild citations with the CHECK constraint (SQLite can't
-- ALTER TABLE ADD CONSTRAINT, so we copy through a new table).
CREATE TABLE citations_new (
    id              TEXT PRIMARY KEY,
    parent_kind     TEXT NOT NULL CHECK (parent_kind IN ('conclusion', 'positive_absence', 'observation')),
    parent_id       TEXT NOT NULL,
    seq             INTEGER NOT NULL,
    path            TEXT NOT NULL DEFAULT '',
    line_start      INTEGER NOT NULL DEFAULT -1,
    line_end        INTEGER NOT NULL DEFAULT -1,
    scope_kind      TEXT NOT NULL DEFAULT '',
    scope_path      TEXT NOT NULL DEFAULT '',
    commit_sha      TEXT NOT NULL DEFAULT '',
    quoted          TEXT NOT NULL DEFAULT ''
);

INSERT INTO citations_new
    (id, parent_kind, parent_id, seq, path, line_start, line_end,
     scope_kind, scope_path, commit_sha, quoted)
SELECT id, parent_kind, parent_id, seq, path, line_start, line_end,
       scope_kind, scope_path, commit_sha, quoted
FROM citations;

DROP TABLE citations;
ALTER TABLE citations_new RENAME TO citations;

-- Step 4: Recreate indexes (they were attached to the old table).
CREATE INDEX IF NOT EXISTS idx_citations_parent ON citations(parent_kind, parent_id);
CREATE INDEX IF NOT EXISTS idx_citations_path ON citations(path);

-- Step 5: Reinstall the append-only triggers on the rebuilt table.
-- Body matches the v3 originals exactly; without these the
-- citations table would silently lose its append-only invariant
-- after v9 lands.
CREATE TRIGGER citations_no_update BEFORE UPDATE ON citations
    BEGIN SELECT RAISE(ABORT, 'citations are append-only'); END;
CREATE TRIGGER citations_no_delete BEFORE DELETE ON citations
    BEGIN SELECT RAISE(ABORT, 'citations are append-only'); END;
`

// migrationV9Down reverses the CHECK by rebuilding the table
// without it. The pre-v5 'finding' → 'conclusion' cleanup is
// intentionally NOT reversed — rewriting consistent rows back to
// a legacy value the rest of the code no longer understands would
// be a foot-gun masquerading as a recovery path. Down is for
// reverting the CHECK specifically, not for time-traveling the
// table's contents back to before v5.
//
// Same trigger ceremony as v9Up: drop the append-only triggers
// before the rebuild, reinstall on the rebuilt table afterward.
// Without this, DROP TABLE could trip the no-delete trigger and
// the post-rebuild table would silently lose its append-only
// invariant.
const migrationV9Down = `
DROP TRIGGER IF EXISTS citations_no_update;
DROP TRIGGER IF EXISTS citations_no_delete;

CREATE TABLE citations_old (
    id              TEXT PRIMARY KEY,
    parent_kind     TEXT NOT NULL,
    parent_id       TEXT NOT NULL,
    seq             INTEGER NOT NULL,
    path            TEXT NOT NULL DEFAULT '',
    line_start      INTEGER NOT NULL DEFAULT -1,
    line_end        INTEGER NOT NULL DEFAULT -1,
    scope_kind      TEXT NOT NULL DEFAULT '',
    scope_path      TEXT NOT NULL DEFAULT '',
    commit_sha      TEXT NOT NULL DEFAULT '',
    quoted          TEXT NOT NULL DEFAULT ''
);

INSERT INTO citations_old
    (id, parent_kind, parent_id, seq, path, line_start, line_end,
     scope_kind, scope_path, commit_sha, quoted)
SELECT id, parent_kind, parent_id, seq, path, line_start, line_end,
       scope_kind, scope_path, commit_sha, quoted
FROM citations;

DROP TABLE citations;
ALTER TABLE citations_old RENAME TO citations;

CREATE INDEX IF NOT EXISTS idx_citations_parent ON citations(parent_kind, parent_id);
CREATE INDEX IF NOT EXISTS idx_citations_path ON citations(path);

CREATE TRIGGER citations_no_update BEFORE UPDATE ON citations
    BEGIN SELECT RAISE(ABORT, 'citations are append-only'); END;
CREATE TRIGGER citations_no_delete BEFORE DELETE ON citations
    BEGIN SELECT RAISE(ABORT, 'citations are append-only'); END;
`

// migrationV10Up adds two columns to analyst_outputs so the row can
// carry its own identity independent of the entity it's indexed
// under. Plan-A canonicalization wants entities at the unversioned
// URI (pkg:X not pkg:X@V); without a per-row target field we'd lose
// the original scope information and be unable to tell which version
// an analysis covered.
//
// Columns:
//
//   - target: the full caller-supplied canonical URI, including any
//     @V suffix. Empty-string default on existing rows — callers
//     route through GetAnalystOutput, which falls back to the
//     joined entity's canonical_uri when target is empty. Lossless
//     under the pre-v10 model where entity URI WAS the caller's
//     target verbatim.
//
//   - target_version: the @V suffix extracted from target, or empty.
//     Empty-string default; readers needing the version for pre-v10
//     rows derive it via profile.SplitURIVersion on the fallback
//     target. New rows carry both fields explicitly.
//
// NOT NULL with DEFAULT ” so existing append-only queries and JOINs
// don't need rewrites for the new columns, and so the column can be
// safely added without a separate NOT NULL CHECK rebuild. The empty
// default matches the "no version specified" wire shape.
//
// No UPDATE backfill: the append-only trigger on analyst_outputs
// (installed in v3) would RAISE on an UPDATE inside the migration
// transaction. Dropping and recreating the trigger around the
// backfill is possible but buys little — empty-string target on old
// rows is handled cleanly by the read-path fallback, so the
// complexity would be work without payoff. The 2026-04-22 dogfood
// caught this on the live DB; tests had empty analyst_outputs and
// never exercised the backfill's trigger interaction.
const migrationV10Up = `
ALTER TABLE analyst_outputs ADD COLUMN target         TEXT NOT NULL DEFAULT '';
ALTER TABLE analyst_outputs ADD COLUMN target_version TEXT NOT NULL DEFAULT '';
`

// migrationV10Down drops the two columns. SQLite's modern ALTER
// TABLE DROP COLUMN handles the rewrite cleanly — we don't need the
// rebuild-through-new-table ceremony v9 used for CHECK constraints.
const migrationV10Down = `
ALTER TABLE analyst_outputs DROP COLUMN target_version;
ALTER TABLE analyst_outputs DROP COLUMN target;
`

// migrationV11Up creates analysis_sessions (durable audit identity
// for /analyze runs) and adds analyst_outputs.analysis_session_id.
// Design rationale: design/phase3-plan.md.
//
// Schema notes:
//
//   - status: CHECK-constrained to the four AnalysisSessionStatus
//     values. Terminal transitions are one-way, enforced in Go by
//     CloseAnalysisSession (SQLite CHECKs can't reference OLD).
//
//   - ended_at: empty-string sentinel until close, then RFC3339.
//     House convention for optional time-like columns; matches the
//     domain layer's *time.Time representation.
//
//   - synthesis_output_id, analyst_outputs.analysis_session_id:
//     both are nullable FKs with ON DELETE SET NULL. Pruning either
//     side of the relationship leaves the other row intact with a
//     dangling-nullified pointer. The FK cascade only fires through
//     prune (append-only triggers otherwise block deletes).
const migrationV11Up = `
CREATE TABLE analysis_sessions (
    id                    TEXT PRIMARY KEY,
    entity_id             TEXT NOT NULL REFERENCES entities(id),
    target_uri            TEXT NOT NULL,
    target_version        TEXT NOT NULL DEFAULT '',
    invoked_by            TEXT NOT NULL,
    pipeline_session_id   TEXT NOT NULL DEFAULT '',
    expected_analysts     TEXT NOT NULL DEFAULT '',
    started_at            TEXT NOT NULL,
    ended_at              TEXT NOT NULL DEFAULT '',
    status                TEXT NOT NULL CHECK (status IN ('in_progress', 'completed', 'failed', 'partial')),
    synthesis_output_id   TEXT REFERENCES analyst_outputs(id) ON DELETE SET NULL,
    notes                 TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_analysis_sessions_entity ON analysis_sessions(entity_id, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_analysis_sessions_status ON analysis_sessions(status);
CREATE INDEX IF NOT EXISTS idx_analysis_sessions_pipeline ON analysis_sessions(pipeline_session_id);

ALTER TABLE analyst_outputs ADD COLUMN analysis_session_id TEXT REFERENCES analysis_sessions(id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS idx_analyst_outputs_session ON analyst_outputs(analysis_session_id);
`

// migrationV11Down drops the FK column and the table. Order matters:
// drop the index that names the column BEFORE DROP COLUMN — SQLite
// rejects DROP COLUMN when an index references it with an
// "error in index" complaint. Then drop the column, the session
// table's own indexes, and finally the table itself.
const migrationV11Down = `
DROP INDEX IF EXISTS idx_analyst_outputs_session;
ALTER TABLE analyst_outputs DROP COLUMN analysis_session_id;
DROP INDEX IF EXISTS idx_analysis_sessions_entity;
DROP INDEX IF EXISTS idx_analysis_sessions_status;
DROP INDEX IF EXISTS idx_analysis_sessions_pipeline;
DROP TABLE IF EXISTS analysis_sessions;
`

// migrate runs all pending migrations on the database. It:
// 1. Creates the schema_version table if it doesn't exist
// 2. Detects legacy databases (tables exist but no version) and marks them as v1
// 3. Backs up the database file before each migration
// 4. Applies each migration in a transaction
// 5. Refuses to open if the database is newer than the code supports
//
// ctx cancellation propagates to every SQL operation. Cancelling
// mid-migration is safe because each version's Up-plus-record-version
// is a single transaction: a cancelled transaction rolls back and the
// schema_version table reflects the last COMMITTED version. A restart
// with the same ctx (or a fresh one) resumes from that version.
func migrate(ctx context.Context, db *sql.DB, dbPath string) error {
	// Create the version tracking table.
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_version (
			version    INTEGER NOT NULL,
			applied_at TEXT NOT NULL
		)
	`); err != nil {
		return fmt.Errorf("create schema_version table: %w", err)
	}

	currentVersion, err := getCurrentVersion(ctx, db)
	if err != nil {
		return err
	}

	// Detect legacy database: tables exist but no version recorded.
	if currentVersion == 0 {
		hasLegacyTables, err := detectLegacyTables(ctx, db)
		if err != nil {
			return err
		}
		if hasLegacyTables {
			// Mark as v1 — the initial schema is already applied.
			if err := recordVersion(ctx, db, 1); err != nil {
				return fmt.Errorf("record legacy version: %w", err)
			}
			currentVersion = 1
		}
	}

	latestVersion := len(migrations)

	// Refuse to open a database newer than this code supports.
	if currentVersion > latestVersion {
		return fmt.Errorf(
			"database schema version %d is newer than this version of signatory supports (max %d); "+
				"upgrade signatory or use the database with a newer version",
			currentVersion, latestVersion)
	}

	// Apply pending migrations.
	for i := currentVersion; i < latestVersion; i++ {
		m := migrations[i]

		// Backup before migration.
		if dbPath != "" {
			if err := backupDatabase(ctx, db, dbPath, i); err != nil {
				return fmt.Errorf("backup before migration %d: %w", m.Version, err)
			}
		}

		// Apply migration and record version atomically in one transaction.
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", m.Version, err)
		}

		if _, err := tx.ExecContext(ctx, m.Up); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %d (%s) failed: %w", m.Version, m.Description, err)
		}

		if _, err := tx.ExecContext(ctx,
			"INSERT INTO schema_version (version, applied_at) VALUES (?, ?)",
			m.Version, time.Now().UTC().Format(time.RFC3339)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record version %d: %w", m.Version, err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", m.Version, err)
		}
	}

	return nil
}

// migrateDown rolls back the most recent migration. It backs up the
// database before rolling back.
func migrateDown(ctx context.Context, db *sql.DB, dbPath string) error {
	currentVersion, err := getCurrentVersion(ctx, db)
	if err != nil {
		return err
	}

	if currentVersion == 0 {
		return fmt.Errorf("database is at version 0, nothing to roll back")
	}

	if currentVersion > len(migrations) {
		return fmt.Errorf("database version %d is newer than code supports (max %d)", currentVersion, len(migrations))
	}

	m := migrations[currentVersion-1]

	// Backup before rollback.
	if dbPath != "" {
		if err := backupDatabase(ctx, db, dbPath, currentVersion); err != nil {
			return fmt.Errorf("backup before rollback from %d: %w", m.Version, err)
		}
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin rollback %d: %w", m.Version, err)
	}

	if _, err := tx.ExecContext(ctx, m.Down); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("rollback %d (%s) failed: %w", m.Version, m.Description, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit rollback %d: %w", m.Version, err)
	}

	// Update version: delete the rolled-back version entry.
	if _, err := db.ExecContext(ctx, "DELETE FROM schema_version WHERE version = ?", m.Version); err != nil {
		return fmt.Errorf("delete version %d: %w", m.Version, err)
	}

	return nil
}

// getCurrentVersion returns the highest applied migration version,
// or 0 if no migrations have been recorded.
func getCurrentVersion(ctx context.Context, db *sql.DB) (int, error) {
	var version int
	err := db.QueryRowContext(ctx, "SELECT COALESCE(MAX(version), 0) FROM schema_version").Scan(&version)
	if err != nil {
		return 0, fmt.Errorf("get current version: %w", err)
	}
	return version, nil
}

// detectLegacyTables checks if the database has the original schema
// tables but no version tracking.
func detectLegacyTables(ctx context.Context, db *sql.DB) (bool, error) {
	var count int
	err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='entities'",
	).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("detect legacy tables: %w", err)
	}
	return count > 0, nil
}

// recordVersion inserts a version record into schema_version.
func recordVersion(ctx context.Context, db *sql.DB, version int) error {
	_, err := db.ExecContext(ctx,
		"INSERT INTO schema_version (version, applied_at) VALUES (?, ?)",
		version, time.Now().UTC().Format(time.RFC3339))
	return err
}

// backupDatabase checkpoints the WAL and copies the database file to a
// timestamped backup. The checkpoint ensures all committed transactions
// are flushed to the main database file before copying.
//
// Format: signatory.db.backup-v{version}-{timestamp}-{random}
//
// The {random} suffix is generated by os.CreateTemp and serves three
// distinct security purposes (issue #82):
//
//  1. O_EXCL atomic creation prevents clobbering an existing file at
//     the predicted path. The previous implementation used O_CREATE|
//     O_WRONLY without O_EXCL, so any pre-existing file at the path
//     was opened for writing and partially overwritten by the database
//     bytes (and without O_TRUNC, the trailing bytes survived,
//     producing a corrupt backup).
//  2. The unguessable random suffix prevents symlink-race attacks. An
//     attacker who could predict the backup path could plant a symlink
//     pointing at e.g. /etc/cron.d/something and redirect the database
//     bytes to that target. With a random suffix the attacker has no
//     path to plant the symlink at.
//  3. The random suffix prevents within-second collisions when two
//     backups happen in the same second (the timestamp is only second-
//     precision).
//
// CreateTemp opens with O_RDWR|O_CREATE|O_EXCL, mode 0600 — same
// permission as the previous explicit OpenFile call.
func backupDatabase(ctx context.Context, db *sql.DB, dbPath string, fromVersion int) error {
	// Checkpoint WAL to ensure all committed data is in the main file.
	// TRUNCATE mode flushes the WAL and truncates it to zero bytes,
	// ensuring the backup of the main file is complete.
	if db != nil {
		if _, err := db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
			return fmt.Errorf("checkpoint WAL before backup: %w", err)
		}
	}

	// G304: dbPath is the caller-supplied DB path (same one that
	// OpenSQLite opened); backing it up IS the function's job.
	src, err := os.Open(dbPath) //nolint:gosec // G304: caller-supplied DB path; backing it up is this function's purpose
	if err != nil {
		if os.IsNotExist(err) {
			return nil // Nothing to back up for a new database.
		}
		return err
	}
	defer src.Close() //nolint:errcheck // read-only file; close errors are not actionable after the copy above

	dir := filepath.Dir(dbPath)
	pattern := fmt.Sprintf("%s.backup-v%d-%s-*",
		filepath.Base(dbPath), fromVersion,
		time.Now().UTC().Format("20060102-150405"))

	dst, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return fmt.Errorf("create backup: %w", err)
	}

	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close() // already in error path; the real error is the copy failure
		// Clean up the partial backup file on copy failure so we don't
		// leave a truncated backup masquerading as a valid one.
		_ = os.Remove(dst.Name()) // best-effort cleanup; the copy failure is the primary error
		return fmt.Errorf("copy database to backup: %w", err)
	}

	// Explicit close to catch flush errors (M1 from review).
	if err := dst.Close(); err != nil {
		_ = os.Remove(dst.Name()) // best-effort cleanup; the finalize error is the primary error
		return fmt.Errorf("finalize backup: %w", err)
	}

	return nil
}
