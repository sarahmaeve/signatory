# Signatory: Entity Model V2

## Status

Design document — pending approval before implementation. This
represents a significant evolution of the data model based on
requirements surfaced during v0.1 development.

## Motivation

The current entity model has several limitations:

1. **Entity IDs are user input** — whatever the user types becomes the
   ID. No normalization, no stability guarantee.
2. **Signals are upserted** — history is lost on every refresh.
3. **Posture is not version-aware** — vetting v1.15.0 and refreshing
   on v1.16.0 overwrites the original decision.
4. **No audit trail** — trust-modifying actions are not logged.
5. **No dependency relationships** — the graph exists only at parse
   time, not in the database.

## Entity Identity

### UUID as Internal Primary Key

Entity IDs become UUIDs — stable, fixed-width, never change. All
foreign keys (signals, postures, burns, dependencies, audit) reference
the UUID.

Benefits:
- Stable forever — renaming, merging, or re-normalizing entities
  doesn't require cascading FK updates
- Fixed-width for efficient indexes and joins
- No risk of user input in primary keys

### Canonical URI as External Identifier

Each entity has a unique `canonical_uri` field — the machine-readable,
parseable identifier. Format depends on entity type:

| Entity type | URI format | Example |
|---|---|---|
| Package (Go) | `pkg:golang/github.com/alecthomas/kong` | Standard purl |
| Package (npm) | `pkg:npm/express` | Standard purl |
| Package (PyPI) | `pkg:pypi/requests` | Standard purl |
| Repository | `repo:github/alecthomas/kong` | Custom scheme |
| Identity | `identity:github/alecthomas` | Custom scheme |
| Organization | `org:github/stretchr` | Custom scheme |
| Pull request | `pr:github/alecthomas/kong/593` | Custom scheme |

Package URIs follow the [purl spec](https://github.com/package-url/purl-spec)
for interoperability with SBOM tools (SPDX, CycloneDX, OSV). Non-package
entities use a consistent `{type}:{platform}/{path}` scheme.

### Human-Readable Fields

Each entity carries human-friendly metadata for display in dashboards,
audit logs, and CLI output:

| Field | Purpose | Example |
|---|---|---|
| `short_name` | Human-friendly label | `Kong` |
| `description` | Brief context, one line | `Go CLI argument parser` |

These can be auto-populated from API data during first analysis and
editable by the user for project-specific context (e.g., "Kong — our
CLI framework").

### Complete Entity Schema

```sql
CREATE TABLE entities (
    id             TEXT PRIMARY KEY,  -- UUID
    canonical_uri  TEXT NOT NULL UNIQUE,
    type           TEXT NOT NULL,     -- package, project, identity, patch, org
    short_name     TEXT NOT NULL,     -- human-friendly label
    description    TEXT NOT NULL DEFAULT '',
    ecosystem      TEXT NOT NULL DEFAULT '',
    url            TEXT NOT NULL DEFAULT '',
    created_at     TEXT NOT NULL,
    updated_at     TEXT NOT NULL
);

CREATE INDEX idx_entities_uri ON entities(canonical_uri);
CREATE INDEX idx_entities_type ON entities(type);
```

## Versioned Posture

### Same Entity, Versioned Decisions

A repository does not change identity when it releases a new version.
Burns apply to the entity (the maintainer was compromised). But posture
is version-specific: "we vetted v1.15.0" does not mean "we vetted
v1.16.0."

The posture primary key changes from `(entity_id)` to
`(entity_id, version)`:

```sql
CREATE TABLE postures (
    entity_id  TEXT NOT NULL REFERENCES entities(id),
    version    TEXT NOT NULL,    -- specific version, or '' for unversioned
    tier       TEXT NOT NULL,
    rationale  TEXT NOT NULL,
    set_by     TEXT NOT NULL,
    set_at     TEXT NOT NULL,
    PRIMARY KEY (entity_id, version)
);
```

This means:
- `posture set kong --version=v1.15.0 --tier=vetted-frozen` records a
  decision for v1.15.0
- When v1.16.0 appears in the dependency tree, it is automatically
  "unexamined" — the v1.15.0 decision does not transfer
- The history of posture decisions is preserved, not overwritten

## Append-Only Signals

### Events Are Immutable

Signal collection events are append-only. Each `--refresh` adds new
signal records; old ones are never modified or deleted. This provides:

- Complete history of observations over time
- Ability to answer "when did this start to go bad?"
- Audit trail of what was known and when
- No silent data loss from re-collection

### Signal ID Scheme

Signal IDs include a timestamp to prevent collisions:

```
{source}:{entity_id}:{signal_type}:{collected_at}
```

Example: `github:a1b2c3d4:stars:2026-04-09T21:00:00Z`

### Querying

- **Current state:** `SELECT * FROM signals WHERE entity_id = ? AND type = ? ORDER BY collected_at DESC LIMIT 1`
- **History:** `SELECT * FROM signals WHERE entity_id = ? AND type = ? ORDER BY collected_at`
- **As of date:** `SELECT * FROM signals WHERE entity_id = ? AND collected_at <= ? ORDER BY collected_at DESC LIMIT 1`

### Signal Expiry

Signals have an `expires_at` field. Expired signals are not deleted
(append-only) but are marked as stale in queries and display. The
`survey` command highlights dependencies with expired signals as
candidates for re-analysis.

## Dependency Relationships

### Dependencies Table

Records which entities depend on which other entities, with version
and observation history:

```sql
CREATE TABLE dependencies (
    project_id  TEXT NOT NULL REFERENCES entities(id),
    entity_id   TEXT NOT NULL REFERENCES entities(id),
    version     TEXT NOT NULL,
    direct      BOOLEAN NOT NULL,
    first_seen  TEXT NOT NULL,    -- when first observed in a survey
    last_seen   TEXT NOT NULL,    -- most recent survey that included it
    removed_at  TEXT,             -- when it disappeared from manifest
    PRIMARY KEY (project_id, entity_id)
);
```

This enables:
- `survey --timeline`: show when dependencies were added, upgraded,
  or removed
- Burn propagation: "your project depends on a burned entity"
- Blast radius: "this entity is depended on by N projects"

## Audit Logging

### In-Database Audit Log

Append-only table recording every trust-modifying action:

```sql
CREATE TABLE audit_log (
    id         TEXT PRIMARY KEY,  -- UUID
    timestamp  TEXT NOT NULL,
    actor      TEXT NOT NULL,     -- "sarah", "llm:claude", "system"
    action     TEXT NOT NULL,     -- "analyze", "set_posture", "burn", "survey"
    entity_id  TEXT,              -- may be NULL for non-entity actions
    detail     TEXT NOT NULL      -- JSON with action-specific data
);

CREATE INDEX idx_audit_timestamp ON audit_log(timestamp);
CREATE INDEX idx_audit_entity ON audit_log(entity_id);
```

### File-Based Audit Log

Append-only text file at `~/.signatory/audit.log`:
- One JSON line per event
- Survives database corruption
- Grep-able, tail-able
- External tools can consume it

Format:
```json
{"timestamp":"2026-04-09T21:00:00Z","actor":"sarah","action":"set_posture","entity":"pkg:golang/github.com/alecthomas/kong","detail":{"version":"v1.15.0","tier":"trusted-for-now"}}
{"timestamp":"2026-04-09T21:05:00Z","actor":"system","action":"analyze","entity":"pkg:golang/github.com/alecthomas/kong","detail":{"signals_collected":17,"absences":0}}
```

### What Gets Logged

| Action | Actor | Logged? |
|---|---|---|
| `analyze --refresh` | system | Yes — signals collected, absences, failures |
| `posture set` | human/LLM | Yes — entity, version, tier, rationale |
| `burn` | human/LLM | Yes — entity, reason |
| `survey` | system | Yes — dependencies found, posture summary |
| Signal collection failure | system | Yes — what failed and why (sanitized) |

## Migration Plan

This is migration v2 — the first real schema evolution using the
migration system built in #38.

### Up (v1 → v2)

1. Add `canonical_uri`, `short_name`, `description` columns to entities
2. Populate `canonical_uri` from existing `id` field (legacy migration)
3. Generate UUIDs for existing entities, update all FK references
4. Alter signals table: change ID scheme, remove upsert constraint
5. Alter postures table: change PK to `(entity_id, version)`
6. Create `dependencies` table
7. Create `audit_log` table

### Down (v2 → v1)

Reverse of the above. Note: audit log data and signal history will be
lost on rollback — this is acceptable because rollback is a recovery
mechanism, not a feature.

## Open Questions

1. **Actor identification for LLM.** When an LLM sets posture via MCP,
   how is it identified? `llm:claude-opus-4.5`? The MCP session ID?

2. **Signal deduplication.** With append-only signals, a rapid series
   of `--refresh` calls creates many near-identical records. Should we
   deduplicate within a time window (e.g., skip if identical signal
   exists within the last hour)?

3. **Dependency version tracking.** When a version changes in the
   manifest, should we create a new dependency row or update the
   existing one? Current schema updates `version` and `last_seen`.
   An alternative is append-only dependency observations too.

4. **Audit log rotation.** The file-based log will grow indefinitely.
   Should we rotate it? If so, based on size or time?
