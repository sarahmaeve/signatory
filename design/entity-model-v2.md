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

## Resolved Questions

### 1. Actor Identity: Signed Team Identity

Actors are identified as human-LLM team identities, analogous to PGP
identity signing. The team is the unit of trust, not the individual
human or individual LLM.

Example: `team:sarah+claude-opus-4.6`

A team identity can be:
- **Created** — team starts working together
- **Halted** — pause signing (e.g., LLM produced suspect output)
- **Rotated** — new identity created, previous trust level resets
  (trust must be re-earned under the new identity)
- **Revoked / self-burned** — everything signed under this identity
  is degraded from the revocation timestamp forward

The team identity is itself an entity in the signatory model — it can
be burned, its trust accumulates over time, and it follows the same
"trust accumulates slowly, degrades fast" principle as any other entity.

The audit log `actor` field references the team identity, not a
free-form string. This provides cryptographic accountability for
trust decisions.

```sql
CREATE TABLE team_identities (
    id          TEXT PRIMARY KEY,  -- UUID
    name        TEXT NOT NULL,     -- "sarah+claude-opus-4.6"
    created_at  TEXT NOT NULL,
    halted_at   TEXT,              -- NULL if active
    revoked_at  TEXT,              -- NULL if not revoked
    revoke_reason TEXT
);
```

### 2. Signal Conflict Resolution: Prompt, Don't Silently Decide

Signals are append-only. When a new collection produces data that
conflicts with a recent collection, the user is prompted to resolve
the conflict rather than silently deduplicating or duplicating.

#### Resolution rules:

| Scenario | Behavior |
|---|---|
| **Identical value, same source, within 1 hour** | Skip automatically, log "skipped identical" |
| **Different value, same source, within 1 hour** | Prompt user: keep new / keep previous / keep both / skip |
| **Outside time window (>1 hour)** | Append without prompting (normal case) |
| **Different source** | Always append, never deduplicate across sources |

#### CLI interaction:

```
Signal "stars" was collected 12 minutes ago (count=3023).
New collection shows: count=3025.
  [k]eep new  [p]revious  [b]oth  [s]kip?  k
```

#### MCP interaction:

The conflict surfaces as a tool response. The LLM presents it to the
user per the analyst/principal model: "These signals conflict with
data collected 12 minutes ago — which should I keep?"

#### Source priority:

Own signals (collected by this signatory instance) are privileged over
external signals. The `source` field distinguishes origins:
`github`, `npm-registry`, `peer:acme-corp`, `hierarchy:security-team`.

When displaying conflicting signals from different sources, own-source
signals are shown first with higher visual prominence.

### 3. Dependency Observations: Append-Only

Each survey produces an append-only snapshot of the dependency tree.
Dependencies are never updated in place — each observation is a new
record:

```sql
CREATE TABLE dependency_observations (
    id           TEXT PRIMARY KEY,  -- UUID
    project_id   TEXT NOT NULL REFERENCES entities(id),
    entity_id    TEXT NOT NULL REFERENCES entities(id),
    version      TEXT NOT NULL,
    direct       BOOLEAN NOT NULL,
    observed_at  TEXT NOT NULL,     -- when this survey ran
    survey_id    TEXT NOT NULL      -- groups observations from one survey
);

CREATE INDEX idx_depobs_project ON dependency_observations(project_id);
CREATE INDEX idx_depobs_survey ON dependency_observations(survey_id);
```

This enables:
- Diffing two surveys: "what changed between survey X and survey Y?"
- Timeline: "when did we start depending on v1.16.0?"
- Historical blast radius: "on date X, which projects depended on
  this burned entity?"
- No data loss: the record of every dependency tree state is preserved

### 4. Audit Log Rotation: User's Responsibility

The file-based audit log (`~/.signatory/audit.log`) grows indefinitely.
Rotation is left to the user or their system's log management. The
in-database audit log follows the same append-only model as signals
and can be pruned by the user if needed.

## External Data Ingestion

The entity model supports ingesting data from external sources — a
requirement for the federated burn list design and hierarchical trust.

### What Can Be Ingested

| Data type | Source field | Example |
|---|---|---|
| Signals | `peer:acme-corp`, `hierarchy:security-team` | A trusted peer's analysis of a package |
| Burns | `BurnSourceInherited` + `SourceOrg` | A hierarchical burn list subscription |
| Posture decisions | source annotation in audit log | "acme-corp considers this vetted-frozen" |
| Entity profiles | `source` on entity creation | Bulk import of pre-analyzed packages |

### How It Works

- External data is stored alongside local data, distinguished by source
- Local signals are privileged over external signals in display and
  scoring
- Burns from subscribed sources are layered (per the federated burn
  model in trust-model.md — analysts can strip inherited burns)
- Audit log records all ingestion events: "ingested 47 entity profiles
  from acme-corp security team at 2026-05-01"

### Trust of External Sources

An external source is itself an entity in signatory. Its trustworthiness
can be assessed, postured, and burned using the same model. Subscribing
to a hierarchical burn list is itself a trust decision that should be
recorded with posture and rationale.

## Conflict Resolution Model

### Append-Only Resolution

Conflicts are resolved without mutating any existing records. The
resolution model uses three event types:

1. **Signal event** — the original or new data (already in signals table)
2. **Conflict detection** — implicit: two signals of the same type
   within the dedup window, or conflicting claims from different sources
3. **Resolution event** — a new record documenting the decision

```sql
CREATE TABLE signal_resolutions (
    id                   TEXT PRIMARY KEY,  -- UUID
    entity_id            TEXT NOT NULL,
    signal_type          TEXT NOT NULL,
    kept_signal_id       TEXT NOT NULL REFERENCES signals(id),
    superseded_signal_id TEXT NOT NULL REFERENCES signals(id),
    action               TEXT NOT NULL,     -- see action vocabulary below
    resolved_by          TEXT NOT NULL,     -- team identity reference
    resolved_at          TEXT NOT NULL
);
```

The "current signals" query filters out superseded signals:
"latest signal per type that is not referenced as superseded in any
resolution." Superseded signals remain in the database as history.

### Pending state is implicit

Unresolved conflicts are detected by query: conflict events (two
signals of same type/source within window, or cross-source conflicting
claims) that have no corresponding resolution event. No status field
is needed — the absence of resolution IS the pending state.

`survey` surfaces unresolved conflicts: "2 signal conflicts pending
resolution — run `signatory resolve` to review."

### Action Vocabulary

| Action | Meaning |
|---|---|
| `keep_new` | Accept the newer signal, supersede the older |
| `keep_previous` | Keep the older signal, supersede the newer |
| `keep_both` | Both signals stand — no supersession (legitimate divergence) |
| `skip` | Discard the newer signal entirely |
| `pending` | Acknowledged but not yet decided — don't re-prompt until ready |
| `accept` | Accept an external/hierarchical claim (federated resolution) |
| `reject` | Reject an external claim, maintain local position |
| `investigate` | Flag for deeper analysis — may trigger an analysis workflow |

### Federated Conflict Resolution

The same model handles conflicts between local and external trust
claims. When ingesting data from a peer or hierarchical source:

```
CONFLICT: pkg:npm/xyz

  Local (you):           BURNED at v1.4
                         "maintainer account compromised"
                         by team:sarah+claude, 2026-04-15

  Hierarchy (acme-corp): VETTED-FROZEN at v1.5
                         "patched, new maintainer verified"
                         by team:security-ops, 2026-04-20

  [a]ccept hierarchy  [r]eject (keep burn)
  [p]ending           [i]nvestigate
```

This generalizes the resolution model: any two conflicting trust
claims — regardless of whether they come from two collections, two
sources, or a local decision vs. a hierarchical directive — are
resolved through the same UX pattern and stored in the same
append-only resolution table.

The resolution itself is a trust decision that gets logged in the
audit trail, with actor identification via the team identity.

### Resolution Chains

Resolutions can chain. If a conflict is resolved, and then a third
claim arrives that conflicts with the resolution, a new conflict is
detected and a new resolution is needed. The full chain is preserved:

```
signal A (v1.4, local) → conflict with signal B (v1.5, hierarchy)
  → resolution R1: pending (by sarah+claude)
  → resolution R2: accept hierarchy (by sarah+claude, after review)
signal C (v1.5.1, hierarchy) → no conflict (consistent with R2)
signal D (v1.5.1, local scan) → confirms hierarchy assessment
```

Each event is a separate, immutable record. The history tells the
complete story of how trust was established, challenged, and resolved.

## Future: Internal Identity Realms (V0.2/Enterprise)

The entity model supports internal identity registries as a natural
extension. This is deferred to v0.2 — it needs user testing and
feedback before design, and may be the differentiator between
open-source and enterprise offerings.

### Concept

Organizations can register internal identities with higher provenance
than external platform accounts:

```
identity:internal/james.park          -- verified employee
identity:internal/maria.rodriguez     -- verified employee
identity:github/random-contributor    -- external, unvetted
team:internal/platform-security       -- internal review team
team:internal/sarah.chen+claude-opus  -- internal human-LLM team
```

The `internal` realm carries a stronger provenance signal — the
company has verified these identities through their own HR/identity
systems, which is a much higher forgery resistance than a GitHub
account.

### Rapid Patch Triage

In high-volume environments, patches arrive rapidly. Internal
identity enables triage by contributor provenance:

- Patch from `identity:internal/james.park` → verified employee,
  known history, established posture → higher baseline trust
- Patch from `identity:github/unknown-user123` → external, no
  organizational verification → standard vetting required
- Patch from `identity:internal/former-employee` → halted identity
  → flagged for review

### Identity Lifecycle

Internal identities follow the same lifecycle as team identities:

- **Employee departure** → halt identity → unreviewed patches lose
  the "verified internal" signal
- **Credential compromise** → burn identity → everything they touched
  is flagged for re-review
- **Contractor vs. employee** → different identity types with different
  default trust levels
- **Team reorganization** → rotate team identities when composition
  changes

### Realms (Cross-Platform Identity Linking)

Organizations with multiple code hosting systems need to link
identities across platforms:

```
identity:corp-github/james.park      -- corporate GitHub account
identity:corp-gitlab/james.park      -- corporate GitLab account
identity:verified/james.park         -- linked corporate identity
```

A realm is a namespace for identity verification. Linking identities
across realms ("corp-github/james.park and corp-gitlab/james.park
are the same person") is the internal equivalent of the cross-platform
identity consistency signal from the rsc case study.

### Why Deferred

- No easy path to user testing or feedback for realm design currently
- Risk of building the wrong abstraction without enterprise users
- The v0.1 entity model (canonical URI with platform prefix) supports
  realms as a future extension without schema changes
- Internal identity is a superset of the open-source identity model —
  nothing in v0.1 prevents adding it later
- Prior art exists: similar systems have been designed for rapid PR
  triage in high-volume environments, marking pull requests as
  privileged or suspect based on user ID and other signals

### Compatibility with V0.1

The v0.1 canonical URI scheme (`identity:github/alecthomas`) already
uses platform prefixes. Adding `identity:internal/james.park` or
`identity:corp-github/james.park` requires no schema changes — just
new URI patterns and the organizational infrastructure to verify them.

## Remaining Open Questions

1. **Team identity key management.** What cryptographic mechanism
   backs the team identity? PGP/GPG keys? SSH keys? OIDC? Deferred
   until the attestation utility layer is built. For v0.1, team
   identity is recorded as a string without cryptographic backing.

2. **Survey snapshot sizing.** Per-survey granularity for append-only
   dependency observations. At our scale (hundreds of deps, daily
   surveys) this is manageable. Pruning can be added later if needed.
