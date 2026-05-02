# Entity-burn v0.1 — `ownership_observations` + cascade-on-owner

Status: **shipped 2026-05-02** — see "Status update" below for what landed and what diverged from the original sketch. The original design is preserved in §1–§12 as history.

Implements: the v0.1 slice of `design/countercampaign.md`.
Defers: identity-equivalence (multiple identities for one human/org) to v0.2 — see §11.

---

## Status update (2026-05-02): shipped

The work this doc sketches is implemented end-to-end. The BufferZoneCorp use case the parent `countercampaign.md` raised works through every CLI surface: one `signatory burn add identity:github/bufferzonecorp` propagates to every repo that operator publishes (and any future repos), surfacing as `*** BURNED: ... (via publisher identity:github/bufferzonecorp, ...) ***` in `signatory analyze`, `summary`, `survey`, and `show-analyses`; `signatory analyze --refresh` refuses to collect against a burned operator's targets by default; `burn list` stays literal.

### What landed, commit-by-commit

| Commit | What |
|---|---|
| `586524b` | `profile`: shared `EntityTypeFor*` helpers + fix analyst_output type hardcode (PR0 — producer-side prerequisite) |
| `c6c4849` | `cmd/signatory`: drop Type check from npm/pypi resolver gates (Option A — fixed legacy-data fragility) |
| `4ff4c81` | `cmd/signatory`: scheme guard in analyze + Path D smoke tests (identity/org URI surface validated) |
| `264c21c` | **Path A**: github collector mints `identity:github/<login>` / `org:github/<name>` for repo owners |
| `fad5738` | **Path C**: npm collector mints `identity:npm/<login>` for maintainers + per-version publishers |
| `c3a7d5a` | **Path B**: cascade resolver `EffectiveBurn` via signal lookup, display callers migrated |
| `57dcd7f` | `analyze`: pre-collection burn gate + `--ignore-burn` override (default-deny) |
| `af123ef` | `show-analyses`: surface effective burn banner before the listing |
| `ff06108` | `cmd/signatory`: extract shared `formatBurnLine` renderer (DRY for the three burn surfaces) |

### Key divergence from the original sketch

**The cascade is signal-derived, not table-backed.** §2's `ownership_observations` table never landed. Instead, Path B's `Store.EffectiveBurn(entityID)` and the sibling `EffectiveBurnByURI(uri)` walk the *existing* signals — `owner_profile` (github), `maintainer_count` and `publish_origin_consistency` (npm) — at read time, deriving owner URIs from the JSON values and probing each for a burn.

Why this changed:
- **No new schema cost.** No migration to write, review, or roll back. The cascade is a pure-derivation feature that ships behind a Go-level Store method.
- **Same end behaviour.** "Burn the operator → all their repos cascade-burn at display" works identically. Soft-delete of the owner burn naturally clears the cascade because `EffectiveBurn` composes `GetBurn` calls.
- **`EffectiveBurnByURI` covers the brand-new-entity case.** For `repo:github/X/Y`, it derives both `identity:github/X` and `org:github/X` candidates from the URI structure alone — no entity row required. That's what makes the pre-collection gate work on a brand-new BufferZoneCorp repo we've never analyzed.
- **Adding new producer ecosystems is purely additive.** PyPI publisher entities, git committer entities, etc., extend the `cascadeCandidates` switch by one case each. No table to modify.
- **Shape B intent emission was overkill** for the v0.1 surface. Github + npm collectors only mint *entities*, not edges; threading a narrow `EntityStore` interface via a `WithEntityStore` setter (consumer-side interface in each collector package) was simpler than the orchestrator-level intent flush. The Shape-B alternative remains valid if a future collector needs to emit edge-shaped data alongside signals.

What this means for the §2/§3/§4 design notes below: read them as the option-A path that *would have worked*, not the path we took. §11 (identity equivalence) is unchanged — that's still v0.2 work, with its own table.

### Surface area shipped beyond this doc's scope

These weren't in the original §1 "In" but emerged naturally from the work:

- **Path C (npm publisher entities)** — listed as "Out" in §1 but came at parity cost once Path A's pattern was established. The lodash-shape case (three historical publishers, only one current maintainer) wanted entity rows.
- **Pre-collection burn gate + `--ignore-burn` override** — display-time cascade in §3.3 was the original scope, but a burned operator collecting fresh signals on first contact was the actual v0.1 shipping bar. Default-deny on `analyze --refresh` with explicit override.
- **`show-analyses` burn banner** — `/analyze` skill's Step 0 calls this; surfacing the burn there gives humans and LLM consumers the lede before the analysis listing.
- **Renderer extraction** — three commands (`analyze`, `summary`, `show-analyses`) print the BURNED line; `formatBurnLine` consolidates the format.

---

## Pending work (post v0.1)

Each item is its own focused unit; none blocks the others.

### 1. PyPI publisher entities

Mechanical Path A/C parallel for the pypi collector. Same `EntityStore` setter pattern, same minting branch in the per-version walk. The PyPI registry exposes `info.maintainer` (single string) and a `maintainers`-shape extension on newer responses; minting `identity:pypi/<login>` and adding `pypi` to `EffectiveBurn`'s cascade candidates closes the third major ecosystem.

### 2. Git committer / signer entities

`identity:email/<addr>` and `identity:gpg/<keyid>` via the git collector. The git collector's `collectAuthorshipSignals` already parses `--format=%aN\x1f%aE` (mailmap-canonical) and `collectCommitSigning` reads `%GK` (GPG key ID — currently discarded after ratio computation). Promoting either to entity rows is small.

The catch: the existing privacy stance (`internal/signal/git/identity.go:108-111`) deliberately discards the per-mapping detail because mailmap entries can encode personal email addresses. Minting `identity:email/<addr>` rows reverses that decision; needs a privacy review before landing. GPG fingerprints don't carry the same PII concern.

This work also bumps directly into v0.2's identity-equivalence question — Alec at swapoff.org and Alec at block.xyz become two separate entities under v0.1's model, and burning one doesn't burn the other. Worth doing only after the equivalence model is settled or accepting the limitation explicitly.

### 3. Posture cascade

Same shape as burn cascade for posture rows. `Store.EffectivePosture(entityID)` would return the direct posture if any, or walk to owner candidates and return the most-restrictive cascaded posture. Display callers migrate from `GetPostures` to `EffectivePosture` for the rendered "current" view; `signatory posture get --all` keeps the literal-rows surface.

The interesting wrinkle is policy — direct-beats-cascade is obvious for burns ("the analyst said so"), but for postures the right rule is less clear (most-restrictive? most-recent? direct-with-rationale-overriding-cascade?). Worth one design pass before implementing.

### 4. Identity equivalence (v0.2)

The full work outlined in §11 below. Sibling table `identity_equivalences`, populated from `.mailmap` per-line mappings, GPG-UID multi-email keys, npm-username-equals-github-username co-occurrence, and analyst attestations. `EffectiveBurn` extended to walk equivalence edges with depth-bounded transitive closure and confidence-thresholded edges.

This unblocks the "Alec at swapoff.org and Alec at block.xyz are the same person" case in production data. It also re-enables Pending Work #2 (git committer entities) cleanly.

### 5. Smaller follow-ons

- **Skill-side updates** — `/analyze` could check `show-analyses` output for BURNED at Step 0 and short-circuit before pipeline session creation. Skipped per direction; the binary-side gate at Step 1b is the load-bearing enforcement.
- **`signatory show-conclusions` / `show-methodology` burn banner** — same surfacing pattern as `show-analyses`. Easy follow-up if any user feedback indicates that's where they look first.
- **Burn-cascade for transitive deps in `survey`** — survey already marks the directly-burned dep as `TierBurned`; if a dep's *dependency* is burned, that's not currently propagated. May or may not be desirable (creates noise on common-ancestor burns); needs design.

---

# Original design sketch (preserved as history)

Sections §1 through §12 below are the implementation sketch as drafted on 2026-05-01, before the work was carried out. They describe the table-backed cascade option — see "Key divergence from the original sketch" above for what we built instead. Read this section as the design discussion that produced the implementation, not as a description of the current code.

## 1. Scope

**In:** github publisher edges, owner-cascade burn resolution, the BufferZone case end-to-end.

**Out:**
- Git committer / email-domain entity emission (deferred — opens identity-equivalence questions that v0.2 settles first).
- Identity equivalences ("two emails for one human", "github login + GPG key on one person") — separate observations table, see §11.
- Posture cascade (countercampaign.md §7.6).
- GPG-key-as-entity (`identity:gpg/<fp>`) — needs `git verify-commit` plumbing.
- Retroactive backfill of already-analyzed repos. Documented behavior: re-running `analyze --refresh` populates ownership rows for previously-analyzed targets.
- npm publisher edges. The npm collector already extracts `_npmUser` and `Maintainers` (`internal/signal/registry/npm/collector.go:317, 197-208`) but the wiring is mechanical follow-on once Shape-B (§3) lands; not on the BufferZone critical path.

## 2. Migration v13 — `ownership_observations`

```sql
CREATE TABLE ownership_observations (
    id           TEXT PRIMARY KEY,                          -- UUID
    owner_id     TEXT NOT NULL REFERENCES entities(id),     -- identity: or org: row
    target_id    TEXT NOT NULL REFERENCES entities(id),     -- repo: or pkg: row
    role         TEXT NOT NULL CHECK (role IN (
                     'publisher',                           -- github owner / npm publisher / pypi uploader
                     'committer',                           -- git author (deferred emission, v0.2)
                     'maintainer',                          -- npm/pypi maintainers list (deferred)
                     'signer'                               -- GPG/SSH key (deferred)
                 )),
    source       TEXT NOT NULL,                             -- "github_api" | "git_log" | "npm_registry" | "analyst"
    observed_at  TEXT NOT NULL                              -- RFC3339
);
CREATE INDEX idx_ownership_owner       ON ownership_observations(owner_id);
CREATE INDEX idx_ownership_target_role ON ownership_observations(target_id, role);

-- append-only enforcement, per migration v3 pattern (internal/store/migrate.go:40-44)
CREATE TRIGGER ownership_observations_no_update BEFORE UPDATE ON ownership_observations
    BEGIN SELECT RAISE(ABORT, 'ownership_observations are append-only'); END;
CREATE TRIGGER ownership_observations_no_delete BEFORE DELETE ON ownership_observations
    BEGIN SELECT RAISE(ABORT, 'ownership_observations are append-only'); END;
```

Notes:
- `role` taxonomy locked by CHECK from day one. Only `publisher` emits in v0.1; the others are reserved so adding committer/signer/maintainer collectors later is additive, not a migration.
- "Current owner" = `MAX(observed_at) WHERE target_id=? AND role=?` — derivable, not stored. Repo transfer between accounts surfaces as a fresh row; the previous row stays for audit.
- No `confidence` column. Source string is sufficient discriminator for ownership ("`github_api` told us"). Confidence belongs on the future `identity_equivalences` table where weak/strong claims actually diverge (see §11).
- No backfill from existing `owner_profile` signals. `analyze --refresh` repopulates as repos are touched.

Lands as **migration v13** (current head is v12 per `internal/store/migrate.go:94`).

## 3. `CollectionResult` extension — Shape B (intent emission)

```go
// internal/signal/errors.go
type CollectionResult struct {
    Collected     []SignalOrAbsence
    Failures      []CollectionError
    EntityIntents []EntityIntent      // NEW
    EdgeIntents   []OwnershipIntent   // NEW
}

type EntityIntent struct {
    CanonicalURI string
    Type         profile.EntityType
    ShortName    string
    Source       string
    ObservedAt   time.Time
}

type OwnershipIntent struct {
    OwnerURI   string  // resolves to entity_id at flush time via the EntityIntent map
    TargetID   string  // entity.ID passed into Collect — already known
    Role       string  // "publisher" in v0.1
    Source     string
    ObservedAt time.Time
}

// internal/signal/make.go
func (r *CollectionResult) RecordEntity(uri string, t profile.EntityType,
    shortName, source string, at time.Time) {
    r.EntityIntents = append(r.EntityIntents, EntityIntent{
        CanonicalURI: uri, Type: t, ShortName: shortName,
        Source: source, ObservedAt: at,
    })
}

func (r *CollectionResult) RecordOwnership(ownerURI, targetID, role, source string,
    at time.Time) {
    r.EdgeIntents = append(r.EdgeIntents, OwnershipIntent{
        OwnerURI: ownerURI, TargetID: targetID, Role: role,
        Source: source, ObservedAt: at,
    })
}
```

Why Shape B over threading the store into collectors:
- **Existing pattern matches.** The orchestrator already accumulates `inRunResult` across collectors (`cmd/signatory/analyze.go:573`). Adding intent slices to that accumulator is the natural shape.
- **Atomicity falls out.** Today `AppendSignals` is a single batched flush. Adding entity/edge flushes alongside (one transaction) preserves the "collector outputs are buffered, store is the single mutator" invariant.
- **Multi-collector composition.** github, git, npm, pypi all emit intents in the same shape. The orchestrator flushes once.
- **Testing.** `Collect()` stays pure; tests assert on the slices without a store fixture.

## 4. Store interface additions

```go
// internal/store/store.go
type Store interface {
    // ... existing methods ...

    // EnsureEntityByCanonicalURI returns the existing row or mints one.
    // The bool is true if a new row was created (useful for audit logging).
    EnsureEntityByCanonicalURI(ctx context.Context, uri string,
        t profile.EntityType, shortName string) (*profile.Entity, bool, error)

    // RecordOwnership appends one observation. Append-only.
    RecordOwnership(ctx context.Context, ownerID, targetID, role, source string,
        observedAt time.Time) error

    // CurrentOwner returns the latest-observed owner of target for the given role.
    // ErrNotFound if no observation exists.
    CurrentOwner(ctx context.Context, targetID, role string) (*profile.Entity, error)

    // OwnedTargets returns all targets currently owned by ownerID for the given role.
    // Used by `signatory summary identity:github/<login>` to list "what does this
    // operator publish?".
    OwnedTargets(ctx context.Context, ownerID, role string) ([]*profile.Entity, error)

    // EffectiveBurn returns the burn that applies to entityID, walking ownership.
    // Direct burn beats cascade (countercampaign.md §7.7).
    // ErrNotFound if neither the entity nor its current publisher-role owner is burned.
    EffectiveBurn(ctx context.Context, entityID string) (*profile.Burn, *EffectiveBurnContext, error)

    // FlushIntents resolves entity intents (find-or-mint) and ownership intents
    // (insert observation rows) in one transaction. Called by the orchestrator
    // between the collector loop and AppendSignals.
    FlushIntents(ctx context.Context, entities []signal.EntityIntent,
        edges []signal.OwnershipIntent) error
}

type EffectiveBurnContext struct {
    Direct   bool             // true: Burn is on entityID itself
    ViaOwner *profile.Entity  // populated when cascade fired
    ViaRole  string           // which role-edge cascaded (always "publisher" in v0.1)
}
```

`GetBurn` and `ListBurns` stay unchanged — audit surface (`signatory burn list`) reads literal rows. Only display callers move to `EffectiveBurn`.

## 5. GitHub collector wiring (the only emitter in v0.1)

Six-line addition in `internal/signal/github/collector.go:collectOwnerProfile` (after the existing `result.RecordSignal(entityID, "owner_profile", ...)` at `:220-230`):

```go
ownerURI := ownerCanonicalURI(ownerUser)   // helper: identity:github/<login> | org:github/<login>
ownerType := profile.EntityIdentity
if ownerUser.Type == "Organization" {
    ownerType = profile.EntityOrg
}
result.RecordEntity(ownerURI, ownerType, ownerUser.Login, sourceName, now)
result.RecordOwnership(ownerURI, entityID, "publisher", "github_api", now)
```

`ownerCanonicalURI` is a small unexported helper next to the existing collector code; it picks the URI scheme by `ownerUser.Type` ("User" → `identity:github/<login>`, "Organization" → `org:github/<login>`).

## 6. Orchestrator integration

Three lines in `cmd/signatory/analyze.go`, between the collector loop (ends at `:576`) and `AppendSignals` (`:578`):

```go
if err := s.FlushIntents(ctx, inRunResult.EntityIntents, inRunResult.EdgeIntents); err != nil {
    return fmt.Errorf("flush intents: %w", err)
}
```

`FlushIntents` runs entity-creation, edge-insertion, and signal append inside one SQLite transaction.

**Open question (decide before coding)**: does `FlushIntents` swallow `AppendSignals` (one call, three steps), or stay separate with the orchestrator opening the transaction and passing a tx-aware store down? Both work. Default: fold them — simpler API, one transaction boundary, no leaky tx interface.

## 7. Display-caller migration

| Caller | File:line | Stays on `GetBurn`? |
|---|---|---|
| `signatory burn list` | `cmd/signatory/burn.go` (list path) | **yes** — audit surface |
| `signatory analyze` (rendering) | `cmd/signatory/analyze.go` (render path) | no — moves to `EffectiveBurn` |
| `signatory summary` | `internal/summary/assemble.go:148` | no |
| `signatory survey` | `internal/survey/survey.go:236` | no |
| MCP `signatory_summary` / `signatory_signals` | response builders | no — adds `effective_burn_via_owner` field |

Display callers use `EffectiveBurnContext` to render `burned (via owner identity:github/<login>)` when cascade fired.

Soft-delete handled automatically: `EffectiveBurn` composes `GetBurn` calls, which already filter `withdrawn_at IS NULL` (per migration v6).

## 8. PR split + test sequence (TDD-ordered)

| | PR1 — data plumbing | PR2 — cascade behavior |
|---|---|---|
| Migration v13 | ✓ | |
| `EnsureEntityByCanonicalURI`, `RecordOwnership`, `CurrentOwner`, `OwnedTargets`, `FlushIntents` | ✓ | |
| `CollectionResult` extension + `RecordEntity`/`RecordOwnership` | ✓ | |
| GitHub collector emits intents | ✓ | |
| Orchestrator `FlushIntents` call | ✓ | |
| `Store.EffectiveBurn` | | ✓ |
| Display-caller migration | | ✓ |
| Rendering changes (CLI + MCP) | | ✓ |

PR1 is observable via direct DB inspection (rows appear in `entities` and `ownership_observations`) but invisible in product UX. PR2 is where cascade fires and BufferZone shows `burned (via owner …)`. If PR2 stalls or reverts, PR1's data is harmless — extra rows nobody queries.

**TDD-ordered test sequence** (each is a failing test first, then implement to green):

PR1:

1. **Migration v13 append-only**: insert one row, attempt UPDATE → expect error; attempt DELETE → expect error. Pattern: `internal/store/append_only_security_test.go`.
2. **`EnsureEntityByCanonicalURI`**: first call mints + returns `created=true`; second call returns existing + `created=false`. ErrNotFound is never the right answer.
3. **`RecordOwnership` + `CurrentOwner`**: round-trip; later `observed_at` wins. Two roles on same target stay independent.
4. **`OwnedTargets`**: returns all targets for an owner+role; empty slice (not nil) when none.
5. **`CollectionResult.RecordEntity` / `RecordOwnership`**: methods append, slices grow, no dedupe at this layer (dedupe is FlushIntents' job).
6. **`FlushIntents` happy path**: 1 entity intent + 1 edge intent → 1 entity row + 1 observation row + signals appended; all-or-nothing.
7. **`FlushIntents` idempotent on repeat**: same intents twice → entities deduped, edge rows append (append-only is correct here).
8. **GitHub collector emits intents**: collect against a fixture for `repo:github/bufferzonecorp/grpc-client` → 1 EntityIntent for `identity:github/bufferzonecorp` (type=Identity), 1 EdgeIntent (publisher).
9. **Orchestrator end-to-end**: `signatory analyze --refresh repo:github/bufferzonecorp/grpc-client` against a recorded API → `entities WHERE canonical_uri='identity:github/bufferzonecorp'` returns 1 row; `ownership_observations` returns 1 publisher row.

PR2 (the §8 A–E cases from `countercampaign.md`, restated as test order):

10. **Cascade fires (case A)**: with bufferzone owner row + edge in place, `signatory burn add identity:github/bufferzonecorp` then analyze a *different* repo published by that owner → `EffectiveBurn` returns the cascade with `ViaOwner` populated.
11. **Unburn (case B)**: `signatory burn remove identity:github/bufferzonecorp` → `EffectiveBurn` on the previously-cascaded repo returns `ErrNotFound`. Verifies soft-delete inheritance through `GetBurn`.
12. **Transfer away (case C)**: append a fresh `ownership_observation` row pointing to a clean owner → cascade no longer fires for that target.
13. **Transfer in (case D)**: append a fresh row pointing to a burned owner → cascade fires for that target.
14. **Direct beats cascade (case E)**: direct burn on repo + owner burn → `EffectiveBurn` returns the *direct* burn as primary, `ViaOwner` populated as secondary context.

## 9. Open implementation questions to pin before code

1. ✅ Reuse `identity:` for User-type, `org:` for Organization-type — no new scheme.
2. ✅ Read-time cascade, not write-time.
3. ✅ Append-only `ownership_observations`; "current" derivable.
4. ✅ Shape-B intent emission (vs threading store into collectors).
5. ✅ `role` column locked from day one (CHECK constraint), only `publisher` emits in v0.1.
6. ✅ `confidence` deferred to `identity_equivalences` (v0.2).
7. ⚠ **Decide before coding**: does `FlushIntents` swallow `AppendSignals` (one orchestrator call, three internal steps), or do they stay separate calls under one orchestrator-managed transaction? Default: fold them.
8. ⚠ **Decide before coding**: does `EnsureEntityByCanonicalURI` audit-log every mint? Existing pattern in `analyze.go:323/341/367/614` audit-logs entity creation. Default: yes, mint emits an `audit_log` row with `action="ensure_entity"` and the source collector recorded.

## 10. Documentation deltas

- `design/countercampaign.md` — amend §3 to lock Shape B, role taxonomy, and the no-confidence-column choice. Add a §11 cross-ref to identity-equivalence as the explicit v0.2 successor.
- `design/entity-model-v2.md` — one paragraph: "`identity:<platform>/<login>` is reused for publisher accounts of User type; `org:` mirrors it for Organization type. Cross-platform identity primitives (`identity:email/...`, `identity:gpg/...`) are reserved for v0.2."
- New `design/analysis/bufferzonecorp-grpc-client.md` writeup — the dogfood case anchored to PR2 test #10 above.

## 11. What this v0.1 explicitly does NOT solve — identity equivalence

The proposed `ownership_observations` table is **owner→target**, directional. It does not express **identity↔identity** equivalence ("Alec at swapoff.org is the same person as Alec at block.xyz", "`identity:github/colinhacks` is the same person as `identity:npm/colinhacks`").

The data already tells us we'll need this, even from the small store we have today:

- Kong's authorship log shows two `Alec Thomas` entries with different emails (`alec@swapoff.org`, `aat@block.xyz`). Mailmap canonicalised the *display name* to "Alec Thomas" for both, but the emails are independent and would mint as two separate `identity:email/...` rows in v0.2. Without an equivalence relation, burning one does not affect the other.
- Cross-platform anchor cases (`identity:github/<login>` ≡ `identity:npm/<login>` ≡ `identity:gpg/<fp>` ≡ `identity:email/<addr>`) are the rule, not the exception.

For v0.2, the proposed shape is a sibling table — same append-only/source/observed_at semantics but a different relation:

```sql
CREATE TABLE identity_equivalences (
    id           TEXT PRIMARY KEY,
    a_id         TEXT NOT NULL REFERENCES entities(id),
    b_id         TEXT NOT NULL REFERENCES entities(id),
    relation     TEXT NOT NULL,    -- "same-person" | "controls" | "delegated-to"
    confidence   TEXT NOT NULL,    -- "asserted" | "observed" | "cryptographic"
    source       TEXT NOT NULL,    -- "mailmap" | "gpg-uid" | "analyst-attest" | "openid-claim" | "github-bound-key"
    observed_at  TEXT NOT NULL
);
```

Why a sibling table and not an overloaded `role` value on `ownership_observations`:

- Different cardinality semantics (many owners per target ↔ many identities per person).
- Different cascade semantics (owner→target is one-way; equivalence is symmetric, transitively closed).
- Different confidence model (ownership has clear source attribution; equivalence has weak-to-strong sources that genuinely diverge).
- Conflating "X commits to Y" with "X is the same person as Y" in one column is a category error.

**Why no `person:<uuid>` URI scheme**: equivalence is a graph edge, not a fabricated parent identifier. We don't actually know who the human is — we observe identity co-occurrence. "Person" is a synthesis, not a primitive. Burning is concrete at the identity level.

The cascade resolver in v0.2 walks both `ownership_observations` and `identity_equivalences`, with depth-bounded transitive closure and confidence-thresholded edges. Existing data sources we'd populate from on day one: `.mailmap` (per-line mappings — currently the collector counts entries but discards specifics for PII reasons; that decision needs revisiting), GPG-key UIDs, npm-username-equals-github-username co-occurrence, analyst attestations.

Until v0.2 lands: **burning Alec at `swapoff.org` does not burn Alec at `block.xyz`** even though humans know it's the same person. This is a known limitation, documented at point of cascade so users see it.

## 12. Connection to existing design

- **`design/countercampaign.md`** — this doc is the v0.1 implementation slice. Decisions in `countercampaign.md §10` are presupposed.
- **`design/entity-model-v2.md`** — `identity:` and `org:` URIs already specified and implemented (`internal/profile/uri.go:37-73`). This doc adds an edge table and a resolver behaviour; no entity-model changes.
- **`design/identity-tracking-plan.md`** — Phase 4 ("Burn propagation") is the parent concept. v0.1 extracts the *publisher* slice; v0.2 (§11) extracts the *equivalence* slice; the cross-platform-graph and `account_takeover_indicators` work stays deferred.
- **`design/posture-relationships.md`** — version-scope inheritance is the precedent for "posture applies more broadly than the exact thing it's set on." Owner-cascade is the same pattern viewed through the publisher axis. Posture-cascade-via-ownership is countercampaign.md §7.6 follow-on; not in v0.1.
- **`internal/store/migrate.go`** — schema home. `dependency_observations` (v2 migration, lines 210-221) is the precedent for the table shape; v3's append-only enforcement (lines 40-44) is the precedent for the triggers.

---

Estimated diff: ~700 LOC + ~500 LOC tests across PR1+PR2. The heaviest single piece is `FlushIntents` (transaction coordination); everything else is mechanical.
