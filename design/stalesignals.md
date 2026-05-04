# Stale Signal Accumulation

## Problem Statement

When `signatory analyze --refresh` runs multiple times against the same
entity, each run appends a full set of signals to the store. The store
is append-only by design (SQLite triggers ABORT on UPDATE/DELETE of
signal rows). `GetLatestSignals` uses a window function to surface only
the most recent row per `(type, source)`:

```sql
ROW_NUMBER() OVER (
  PARTITION BY type, source
  ORDER BY collected_at DESC
) AS rn
...
WHERE rn = 1
```

This works for the common case (same signal type, newer timestamp wins).
But it breaks when a signal transitions between success and absence
across runs, because `absence:X` and `X` are stored as different `type`
values and occupy **different partitions**. Both survive the window
function and appear in the output simultaneously — giving contradictory
state.

### Concrete example

1. Run 1 (no GITHUB_TOKEN): collector records `absence:adoption` from
   source `github` at T1.
2. Run 2 (with token): collector records `adoption` from source `github`
   at T2 (T2 > T1).
3. `GetLatestSignals` returns BOTH rows — each wins its own partition.
4. CLI output shows:
   ```
   adoption  [ABSENT]  GitHub API 401 — set GITHUB_TOKEN to authenticate
   adoption  [high]    adoption_type=direct, go_mod_refs=0, ...
   ```

This is confusing and incorrect. The absence is stale — the later run
superseded it.

## Existing infrastructure

- **`collected_at`** on every signal row — already used for "latest
  wins" ordering within a partition.
- **`expires_at`** on every signal row — written but **never used** as a
  filter predicate anywhere in the codebase.
- **`signal_resolutions` table** — rows that mark individual signal IDs
  as superseded. `GetLatestSignals` already excludes resolved signals
  via `id NOT IN (SELECT superseded_signal_id FROM signal_resolutions)`.
- **Analysis sessions** — exist for Layer 2 analyst outputs only. Layer
  1 signal collection has no batch/session/run concept.
- **Signal ID format** — `source:entityID:signalType:unixNano`. The
  nanos suffix guarantees uniqueness per run. There is no run UUID.

## Growth concern

Every `--refresh` appends a full new signal set (~30-40 rows per
entity). Old rows are never cleaned. At v0.1 scale (dogfooding a
handful of packages) this is noise — SQLite handles thousands of rows
trivially. But at sustained use (daily refresh of 50 packages ×
40 signals × 365 days = 730K rows/year) the DB grows without bound.

A future reaper (GC signals past `expires_at`, or retain only the last N
collections per entity) is the long-term answer. The options below solve
the immediate contradictory-display bug without adding a reaper.

---

## Option A: Query-level exclusion

Modify `GetLatestSignals` so that after the window function picks the
latest row per `(type, source)`, it filters out any `absence:X` row
when a newer non-absence `X` row from the same source exists.

### Implementation sketch

```sql
WITH latest AS (
  SELECT id, entity_id, type, signal_group, source,
         forgery_resistance, value, collected_at, expires_at
  FROM (
    SELECT *,
           ROW_NUMBER() OVER (
             PARTITION BY type, source
             ORDER BY collected_at DESC
           ) AS rn
    FROM signals
    WHERE entity_id = ?
      AND id NOT IN (
        SELECT superseded_signal_id FROM signal_resolutions
        WHERE entity_id = ?
      )
  )
  WHERE rn = 1
)
SELECT * FROM latest
WHERE NOT (
  type LIKE 'absence:%'
  AND EXISTS (
    SELECT 1 FROM latest AS successor
    WHERE successor.source = latest.source
      AND successor.type = SUBSTR(latest.type, 9)
      AND successor.collected_at > latest.collected_at
  )
)
ORDER BY signal_group, type, source
```

### Pros

- Pure read-side fix. No writes, no new rows, no schema change.
- Append-only invariant untouched.
- Stale absence rows remain as audit history — queryable directly if
  needed.
- Trivially testable with a SQL-level unit test.

### Cons

- `EXISTS` subquery adds cost per absence row. Bounded by the number of
  signal types (~50 max), negligible at any realistic scale.
- Reverse case not handled: if the latest run *fails* (produces
  `absence:X`) and an older run succeeded (`X`), the old success still
  shows. Arguably correct ("best known state"), but a token revocation
  doesn't immediately clear the old success until a successful refresh
  overwrites it.
- Old rows accumulate forever. Invisible in output but occupy disk.
  Reaper is still needed long-term.

---

## Option B: Signal-resolution at write time

When `AppendSignals` stores a successful signal `X` from source `S`, it
also checks whether a prior `absence:X` row from the same source exists
for that entity. If so, it files a `signal_resolution` row marking the
old absence as superseded. `GetLatestSignals` already excludes resolved
signals via `id NOT IN (SELECT superseded_signal_id FROM
signal_resolutions ...)`, so the stale absence disappears from output
without being deleted.

The reverse direction also applies: if a new `absence:X` is recorded
and a prior successful `X` from the same source exists with an *older*
`collected_at`, the old success is superseded.

### Implementation sketch

In the `AppendSignals` transaction (or a new `AppendSignalsWithResolution`
method), after inserting the batch:

```go
for _, sig := range signals {
    // Determine the "opposite" type to look for.
    var oppositeType string
    if strings.HasPrefix(sig.Type, "absence:") {
        oppositeType = strings.TrimPrefix(sig.Type, "absence:")
    } else {
        oppositeType = "absence:" + sig.Type
    }

    // Find the latest prior signal of the opposite type from the same source.
    row := tx.QueryRowContext(ctx, `
        SELECT id, collected_at FROM signals
        WHERE entity_id = ? AND type = ? AND source = ?
          AND id NOT IN (SELECT superseded_signal_id FROM signal_resolutions WHERE entity_id = ?)
        ORDER BY collected_at DESC
        LIMIT 1`,
        sig.EntityID, oppositeType, sig.Source, sig.EntityID)

    var priorID, priorCollectedAt string
    if err := row.Scan(&priorID, &priorCollectedAt); err == nil {
        // Prior opposite-type signal exists. Supersede it.
        resolution := &profile.SignalResolution{
            ID:                 newResolutionID(),
            EntityID:           sig.EntityID,
            SignalType:         oppositeType,
            KeptSignalID:       sig.ID,
            SupersededSignalID: priorID,
            Action:             "auto:refresh-supersede",
            ResolvedBy:         "signatory-collector",
            ResolvedAt:         time.Now().UTC(),
        }
        // INSERT into signal_resolutions within the same tx.
    }
}
```

### How it uses existing infrastructure

The `signal_resolutions` table already exists. Its schema:

```sql
CREATE TABLE signal_resolutions (
    id                   TEXT PRIMARY KEY,
    entity_id            TEXT NOT NULL REFERENCES entities(id),
    signal_type          TEXT NOT NULL,
    kept_signal_id       TEXT NOT NULL REFERENCES signals(id),
    superseded_signal_id TEXT NOT NULL REFERENCES signals(id),
    action               TEXT NOT NULL,
    resolved_by          TEXT NOT NULL,
    resolved_at          TEXT NOT NULL
);
```

`GetLatestSignals` already excludes resolved signals:
```sql
AND id NOT IN (
  SELECT superseded_signal_id FROM signal_resolutions
  WHERE entity_id = ?
)
```

So once a resolution row is filed, the stale absence (or stale success)
vanishes from all read paths — CLI display, MCP tools, summary assembly
— without any additional query changes.

### Pros

- Uses the resolution mechanism the way it was designed — explicit
  supersession with an audit trail (`action`, `resolved_by`,
  `resolved_at`).
- Append-only invariant preserved. Old signal rows stay in place. The
  resolution table is the only new write.
- The resolution itself is queryable: "when did this absence get
  superseded, and by what?" — useful for debugging auth issues.
- Both directions work: success supersedes absence, AND absence
  supersedes success (if the later run fails, the old success is
  hidden — "latest state wins" in both directions).
- No changes to `GetLatestSignals` query.

### Cons

- Write-time cost: each signal insertion triggers a lookup for its
  opposite-type sibling. Bounded by the number of signals per run
  (~40), each lookup is an indexed query on `(entity_id, type, source)`
  — sub-millisecond. Negligible.
- Adds rows to `signal_resolutions` over time. Same growth-without-
  bound concern as the signals table itself. Same future reaper handles
  both.
- More complex than Option A — writes new rows rather than just changing
  a query predicate.
- The FK constraint `REFERENCES signals(id)` on `kept_signal_id` means
  the resolution INSERT must happen AFTER the signal INSERT within the
  same transaction. This is straightforward but means the logic lives in
  the store layer, not the collector.
- If a signal type legitimately has both a successful and an absence
  variant coexisting (no current examples, but hypothetically), this
  would incorrectly suppress one. In practice, `X` and `absence:X` from
  the same source for the same entity are always contradictory — they
  represent "I tried and succeeded" vs "I tried and failed" at the same
  collection point.

---

## Recommendation

**Option A** is simpler and sufficient for v0.1. It requires no schema
interaction, no write-path changes, and handles the user-visible
symptom (contradictory display) cleanly. The stale rows remain as
audit history but are invisible.

**Option B** is architecturally cleaner for a production system —
explicit supersession with an audit trail, leveraging existing
infrastructure. Worth migrating to if resolutions become a first-class
user-facing concept (e.g., "why did this absence disappear?").

Either way, a future **reaper** is needed to GC rows past `expires_at`
or past N runs per entity. Neither option addresses unbounded growth.

