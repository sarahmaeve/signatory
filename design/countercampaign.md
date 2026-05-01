# Counter-campaign: owner-level burn for multi-repo attack operators

Status: **draft 2026-05-01** — written immediately after the BufferZoneCorp/grpc-client analysis surfaced the gap. Not implemented. Decisions to make are in §7; tomorrow-me wants §8 as the implementation checklist.

Supersedes: nothing. Extends: `design/entity-model-v2.md`, `design/identity-tracking-plan.md`, `design/posture-relationships.md`.

---

## 1. Problem

Today (2026-05-01) the `/analyze` pipeline rejected `repo:github/bufferzonecorp/grpc-client` as active supply-chain malware (analysis session `9134f0d7`, synthesis `699f03a6`, posture `rejected`). The `owner_profile` signal already told us the operator's account is 11 days old with **17 public repos** — a campaign-shaped fingerprint, not a one-off mistake.

The natural next move is "burn the operator." It is not currently possible. `signatory burn add` can only target one of five canonical URI schemes (`pkg:`, `repo:`, `identity:`, `org:`, `patch:`), and the only data the store keeps about the operator is a per-repo `owner_profile` signal — not an entity row, not an edge, not anything queryable as "all repos this account published." The campaign attacker pays per-account; we pay per-repo. Asymmetry in their favour.

Two distinct failure modes follow from this gap:

1. **No primitive.** A defender who has confirmed an operator is malicious must enumerate the 17 repos by hand (via `gh api` or the GitHub web UI) and run 17 individual `signatory burn add` calls. The work is mechanical, error-prone, and breaks when the operator publishes their 18th repo tomorrow.
2. **No tripwire.** Even if all 17 known repos are burned, a fresh repo published by the same account inherits no degraded posture — the trust judgment lives on the burned-leaf-repo identity, not on the operator. `signatory analyze pkg:golang/github.com/BufferZoneCorp/<new-repo>` next week will collect signals fresh and start at `unexamined`.

Both failure modes point at the same missing primitive: **the operator account as a first-class burnable entity, with cascading and forward-looking trust implications for every repo it publishes.**

## 2. Current state (cited)

Five accepted URI schemes (`internal/profile/uri.go:37-73`, scheme constants + `validURISchemes` slice):

```
pkg:<ecosystem>/<name>
repo:<platform>/<owner>/<name>
identity:<platform>/<user>
org:<platform>/<name>
patch:<platform>/<owner>/<repo>/<id>
```

The matching `EntityType` constants — `EntityProject`, `EntityPackage`, `EntityIdentity`, `EntityOrg`, `EntityPatch` — are already present and mapped from scheme to type by `entityTypeForScheme` (`cmd/signatory/posture.go:402-420`). So the data model already accommodates identity and org entities at the type level.

`identity:github/<login>` is documented in `design/entity-model-v2.md` and `design/identity-tracking-plan.md` as a contributor identity (commit author lens), but the URI shape is general — the operator-as-publisher case is the same primitive viewed from the publication side.

`signatory burn add` (`cmd/signatory/burn.go:39-129`) calls a package-local helper `ensureEntity(ctx, s, target)` defined at `cmd/signatory/posture.go:352-393` and shared with `posture set`. **`ensureEntity` already accepts `identity:` and `org:` URIs** (line 367 enumerates them in its error path), does a find-then-create dance via `Store.FindEntityByURI` + `profile.NewEntityID()` + `Store.PutEntity`, and would happily mint a fresh `identity:github/bufferzonecorp` entity row today. The reason `signatory burn add identity:github/bufferzonecorp` doesn't behave usefully is not URI rejection — it's that the resulting burn row has no propagation semantics and no edge to the operator's repos.

`Store.PutEntity` (`internal/store/sqlite.go:267-292`) is upsert-on-`id` (UUID), not upsert-on-`canonical_uri`. The find-by-canonical-URI half lives in `ensureEntity`; the github collector has no equivalent today (it doesn't talk to the store at the entity level — only `result.RecordSignal` on the repo's already-existing entity_id).

The github collector records owner data as a per-repo signal (`internal/signal/github/collector.go:211-231`):

```go
result.RecordSignal(entityID, "owner_profile", "github", now, ttl, map[string]any{
    "login": ownerUser.Login, "type": ownerUser.Type,
    "account_age_days": ..., "public_repos": ..., "followers": ...,
})
```

The `entityID` is the **repo** entity. The owner is data attached to the repo, not its own row. There is no `owners` table, no FK from `entities.repo` to `entities.identity`, and no `ownership_observations`-shaped edge.

The "related identities" feature in `signatory summary` is surfaced by `Assembler.Assemble` (`internal/summary/assemble.go:196-210`) via `Store.ListRelatedURIs` (`internal/store/analyst_output_query.go:500-531`). It walks the `analyst_outputs.collected_from_entity_id ↔ entity_id` link only — it surfaces the cross-URI hops that analysts already recorded, not a publisher→repo edge.

The `burns` table (`internal/store/migrate.go:140-147`, evolved by migration v6 with soft-delete columns at lines 821-823) FKs to `entities(id)` with `entity_id` as the primary key — single burn per entity, no version split, soft-delete via `withdrawn_at`. **The schema already supports a burn on any entity, including `identity:` rows; it's only the resolver layer that is single-target rather than cascading.**

`design/identity-tracking-plan.md §Phase 4` ("Burn propagation") proposes the cascade via `contribution_observations`, but that document's scope is broad (cross-platform identity linking, mailmap-derived identity graphs, `account_takeover_indicators`, opt-out policy) and Phase 4 is gated on Phases 1–3. None are implemented; `contribution_observations` is design-doc-only.

## 3. Proposal

Three additions, each independently small. None requires inventing a new URI scheme — we already have `identity:`.

### 3.1 Owner-as-identity entity creation

When the github collector runs against `repo:github/<owner>/<name>`, it already fetches `ownerUser`. Promote that fetch from "value goes into the repo's signal" to also "ensure an entity row exists for `identity:github/<owner>` (or `org:github/<owner>` when `ownerUser.Type == "Organization"`)."

The signal stays. We're adding an entity, not replacing a signal.

**Build decision: where does the upsert primitive live?** The CLI helper `ensureEntity` (`cmd/signatory/posture.go:352-393`) already does the right find-or-create dance, but the github collector lives in `internal/signal/github/` and can't import from `cmd/`. Two ways to bridge:

- **(a) Refactor `ensureEntity` into `internal/profile/`** as `profile.EnsureEntity(ctx, store, target) (*Entity, error)`. Both the CLI helpers and the github collector call into it. Simplest path; the helper is already general (no `cmd`-specific dependencies). Estimated 30–50 LOC of move + import-shuffle.
- **(b) Add a store-level primitive** `Store.EnsureEntityByCanonicalURI(ctx, uri, type, shortName) (*Entity, error)` that does the find-or-mint atomically inside the store package. Cleaner separation (no profile→store→profile call graph); preserves the existing shape of `PutEntity` as upsert-on-id.

Default answer: **(a)**, refactor first. The collector consumes `profile.Entity` shapes already; (b) introduces a redundant API layer when one good helper would do. If we later need transactional atomicity that (a) can't provide (concurrent collection runs racing on the same owner login), revisit (b).

A second wiring question: the github collector currently has access to `*signal.CollectionResult` but **not** to `store.Store`. Either thread the store down through the collector signature, or have the collector emit an "ensure-entity" intent the orchestrator processes alongside signal recording. Threading the store is mechanically simpler; the intent-emission shape composes more cleanly with the existing `result.RecordSignal` pattern. Decide during implementation; both work.

Sketch (using option (a) + threaded store):

```go
// in internal/signal/github/collector.go
ownerURI := ownerCanonicalURI(ownerUser)  // "identity:github/<login>" or "org:github/<login>"
ownerEntity, err := profile.EnsureEntity(ctx, c.store, ownerURI)  // refactored helper
if err != nil { /* record failure, continue */ }
result.RecordSignal(repoEntityID, "owner_profile", ...)            // unchanged
result.RecordOwnership(repoEntityID, ownerEntity.ID, "github_api", now)  // §3.2
```

Both branches of the refactor are idempotent on repeat collection runs.

### 3.2 Ownership edge

A new append-only table mirroring the **already-implemented** `dependency_observations` table (`internal/store/migrate.go:210-221`). The shape was prefigured in `design/identity-tracking-plan.md` as `contribution_observations`, but that table is design-doc-only — no migration has landed it. So `ownership_observations` is a fresh table, not a renaming.

```sql
CREATE TABLE ownership_observations (
    id            TEXT PRIMARY KEY,         -- UUID
    owner_id      TEXT NOT NULL REFERENCES entities(id),  -- identity: or org: entity
    repo_id       TEXT NOT NULL REFERENCES entities(id),  -- repo: entity
    role          TEXT NOT NULL,            -- "publisher" (today the only role; reserved for future "transferred-from", "co-publisher" etc.)
    observed_at   TEXT NOT NULL,
    source        TEXT NOT NULL             -- "github_api"
);
CREATE INDEX idx_ownership_owner ON ownership_observations(owner_id);
CREATE INDEX idx_ownership_repo  ON ownership_observations(repo_id);
```

Per migration v3's append-only-enforcement pattern (which already covers `signals`, `dependency_observations`, `signal_resolutions`, `audit_log`), `ownership_observations` should also have triggers blocking UPDATE and DELETE — this is the project's convention for any observations-shaped table.

The "current owner" is `MAX(observed_at) WHERE repo_id = ?` — a derivable view, not a stored field. Repo transfer between accounts (`/old-owner/foo` → `/new-owner/foo` redirect) appears as a fresh row with the new owner_id; the old observation stays for audit.

Lands as **migration v13** (current head is v12 per `internal/store/migrate.go`).

### 3.3 Burn cascade — read-time, not write-time

Two ways to make `signatory burn add identity:github/bufferzonecorp` propagate to all 17 repos:

- **Write-time cascade**: at burn time, walk `ownership_observations` and write 17 additional `burns` rows for each owned repo. Pros: simple downstream queries. Cons: unburn needs to find and remove all 17 cascade rows; a repo owned by both a burned operator and an OK organization has ambiguous status; the 18th repo published tomorrow gets no protection because cascade ran on yesterday's owned set.
- **Read-time cascade** (proposed): one `burns` row, on the operator. At trust-query time, anywhere we ask "is `repo:X` burned?", the resolver also asks "is `repo:X`'s current owner burned?" via `ownership_observations`. Pros: unburn is one row delete; future repos under the same operator are protected as soon as `owner_profile` is collected; auditability is intact (one ledger entry per analyst decision). Cons: every burn-aware query path picks up the join.

Read-time is the right call. It matches how `posture` lookups already walk inheritance (`design/posture-relationships.md §3.3`); it makes future repos automatically protected, which is the campaign-attack defence we actually want; and the unburn case ("we mistakenly burned this operator") is one row, not seventeen plus whatever they published since.

The query shape:

```
TrustResolver(repo_uri) =
    direct burn on repo_uri
    UNION
    burn on (current_owner_of(repo_uri))
```

`current_owner_of` walks `ownership_observations` for the latest row by `observed_at`. Returned rows in `signatory analyze` / `signatory summary` get tagged `burned (via owner identity:github/bufferzonecorp)` so the user knows which ledger entry caused it.

**Where the cascade lives.** Today the burn-check primitive is `Store.GetBurn(ctx, entityID)` (interface at `internal/store/store.go:44`, SQLite impl at `internal/store/sqlite.go:547`). Existing callers: `internal/survey/survey.go:236` and `internal/summary/assemble.go:148`. CLI rendering paths read through the same. Two integration patterns:

- **Wrap `GetBurn` in place.** Every existing caller picks up the cascade automatically; a small interface change adds an "effective via" return field (or a sibling tuple) so renderers can show the cascade reason.
- **Add `Store.EffectiveBurn(entityID) (Burn, viaOwnerURI, error)`** as a new method, leave `GetBurn` as the audit-direct lookup (returns only the row written for this entity), and migrate display callers to `EffectiveBurn` while audit/admin paths stay on `GetBurn`.

The wrap approach is one PR and fewer touch points. The split approach preserves the audit surface that "what burns are *literally* in the table for this entity" answers — which we want for `signatory burn list` and the audit log. **Default: split (option B).** `signatory burn list` should keep showing one row per literal burn; `signatory analyze` and friends should show the effective state.

Soft-delete already works at this layer: `GetBurn` filters `withdrawn_at IS NULL` (per migration v6); `EffectiveBurn` inherits that automatically since it composes `GetBurn` calls. No new mechanism needed for unburn semantics.

## 4. Surface changes

### 4.1 CLI

```bash
# burn the operator — one row, propagates to all owned repos
signatory burn add identity:github/bufferzonecorp \
  --reason-file /tmp/bufferzonecorp-rationale.md

# query the operator's campaign
signatory summary identity:github/bufferzonecorp
# → entity profile + linked repos + cumulative burn status
```

`signatory analyze repo:github/bufferzonecorp/<some-other-repo>` shows:

```
Posture:   unexamined
Burn:      yes (cascaded from identity:github/bufferzonecorp, set 2026-05-01)
Rationale: <the operator-level rationale>
```

even on a fresh refresh of a repo we've never analyzed.

### 4.2 MCP tools

`signatory_signals` and `signatory_summary` for repo entities should include `effective_burn_via_owner` in the response so an LLM consumer can see why a repo is degraded without re-issuing a query for the owner.

`signatory_show_conclusions` is unchanged — burn cascade is a posture-side concern, not a conclusions-side concern. (Conclusions are still per-analysis, per-target.)

### 4.3 Survey

When a dependency resolves to a repo whose owner is burned, the action-items block surfaces the cascade reason rather than re-stating the burn:

```
Action items
  1 transitive dep flagged via owner burn:
    pkg:golang/github.com/BufferZoneCorp/somelib → owner identity:github/bufferzonecorp burned 2026-05-01
```

## 5. The campaign signature as a future signal (deferred)

A separate but related question: can we *detect* a campaign-shaped operator without a human analyst?

The BufferZoneCorp signature is structurally distinctive: account_age_days < 30, public_repos > 10, followers = 0, all repos created within a narrow window, all commits via `users.noreply.github.com`, all tags lightweight, zero CI. None of these signals individually justifies a burn — academic-intern accounts and personal-project bursts share many of them — but the *joint distribution* is the disposable-attack-persona fingerprint our security analyst flagged independently.

A `campaign_persona_score` aggregate signal computed at owner-entity collection time would be the natural primitive. **Out of scope for this doc.** Recording the idea so we can hang an analyst-recommended posture on the operator entity later, once we have more campaign cases to calibrate against.

## 6. What this is NOT

Explicit non-goals so the scope stays tight:

- **Not** the full identity-tracking-plan. We are not implementing cross-platform identity linking, `.mailmap`-derived equivalence, `account_takeover_indicators`, or the privacy/opt-out policy that document raises. Those are correct work, but campaign-burn doesn't need them.
- **Not** an entity-relationship graph. One edge, one direction (owner → repo), one role (publisher). No `fork_of`, `competes_with`, `maintained_by`. The Option-3 design pass `posture-relationships.md` deferred is still deferred.
- **Not** automatic operator detection. `campaign_persona_score` is mentioned in §5 as a follow-on; this doc's scope is the *primitive*, not the heuristic.
- **Not** organization-aware. GitHub orgs (`org:github/<name>`) get the same treatment as user accounts (`identity:github/<login>`) — both can own repos, both can be burned. Differentiating org-vs-user trust semantics is a future refinement.
- **Not** retroactive analysis-degradation. If we burn the operator, existing analyses of their repos stay intact in the store with their original conclusions; only the trust-resolver layer sees them as burned. The audit trail is preserved.

## 7. Open questions (decide before implementing)

### 7.1 Identity vs. org URI for User-type accounts

Q: BufferZoneCorp's GitHub `type` is `User`. Should the operator entity be `identity:github/bufferzonecorp` or a new `owner:github/<login>` scheme that's neutral between User and Organization?

Default answer: reuse `identity:` for User-type accounts and `org:` for Organization-type. Both schemes already exist; the github collector inspects `ownerUser.Type` and picks. A consumer asking "burn the operator" doesn't care about User vs Organization — both are URIs, both can carry burns, both can own repos via §3.2.

Risk: `identity:` connotes "human contributor" in identity-tracking-plan.md. We're broadening it to "publisher account", which is a slight semantic stretch. Tolerable; if it becomes confusing, a v0.2 alias like `publisher:github/<login>` can be added without schema change.

### 7.2 Cascade semantics on unburn

Q: If we burn `identity:github/bufferzonecorp`, then later realize the burn was premature and unburn — what happens to repos that were `analyze`d against the cascaded burn during that window?

Default answer: nothing automatic. Conclusions and signals stay in the store unchanged. The trust resolver sees the unburn at read time and stops applying the cascade. Users who saw `burned (via owner)` during the window saw it correctly given the state of the ledger at that time; the audit trail records both the burn-add and the burn-remove with timestamps.

### 7.3 Multiple owners of one repo

Q: Repo transfers, fork-then-rename, GitHub org-level ownership changes — `ownership_observations` is append-only, so the table can hold (repo_X, owner_A, t1) and (repo_X, owner_B, t2). At trust-query time, do we use only the latest, or all historical owners?

Default answer: latest only. A repo transferred from a malicious operator to a legitimate one is no longer "their" repo; the legitimate owner now bears responsibility. Conversely, a legitimate repo recently transferred to a burned operator should pick up the cascade immediately.

Edge case: a repo's history was authored under the burned operator before transfer to a legitimate owner. The conclusions from analysts citing those commits stay; the read-time cascade does not apply (current owner is clean). The right move on these is analyst-mediated — an analyst notices the suspicious authorship in `git log`, lands a conclusion, and the synthesist proposes a posture. The cascade is a coarse trust primitive, not a substitute for analyst attention.

### 7.4 Org→user vs user→org

Q: A burn on `org:github/<name>` cascades to all repos in the org. What about org members? An identity that's a member of a burned org — is the identity also degraded?

Default answer: out of scope for v0.1 of this design. Org-membership is a different relation than repo-ownership, lives in different GitHub API surfaces, and has different revocation semantics (members come and go faster than repos transfer). Defer to identity-tracking-plan's full identity-graph work.

### 7.5 First-collection bootstrap

Q: At the moment we burn `identity:github/bufferzonecorp`, only repos we've already analyzed appear in `ownership_observations`. The other 14 (we analyzed 1 of 17) have no row. Do we proactively enumerate?

Default answer: no — but yes-eventually. v0.1 cascade applies to repos as they get collected. The 14th repo gets the cascade the first time `analyze --refresh` runs against it, because §3.1 records the ownership edge as part of every collection. A later v0.2 enhancement could add `signatory enumerate-publisher identity:github/<login>` that walks `gh api users/<login>/repos`, creates entity rows, and records ownership edges for each — populating the cascade preemptively. Not blocking.

### 7.6 Posture cascade parity

Q: This doc scopes burn cascade. Should `posture` cascade through ownership the same way?

Default answer: yes, but in a follow-up. The `posture-relationships.md` work added scope-walking for version inheritance; adding owner-walking is the natural next column. **Until we do, `posture` and `burn` ledgers will drift on the operator-level case** — burn cascades, posture doesn't. Acceptable for v0.1 (burn is the urgent primitive — it's the "fire alarm"); track as a follow-on so the two converge.

### 7.7 Direct-burn precedence

Q: A repo has both a direct burn AND a cascaded owner burn. Which wins for the rationale that gets shown?

Default answer: direct beats cascade. The direct burn carries an analyst-specific reason about *this repo*; the cascade carries the operator-level reason. `EffectiveBurn` returns the direct row as primary and the cascade as a sibling field, so renderers can show "burned: <direct reason>" with "(also burned via owner ...)". Tested in case E (§8).

## 8. Implementation checklist (when tomorrow-me comes back)

Not implemented yet. When ready:

- [ ] **Migration v13**: `ownership_observations` table + indexes; append-only triggers blocking UPDATE/DELETE per the v3 enforcement pattern (`internal/store/migrate.go:40-44`). Up/down/test cells matching the existing migration shape.
- [ ] **Refactor**: move `cmd/signatory/posture.go:352 ensureEntity` → `internal/profile/EnsureEntity(ctx, store, target)`. Update both `cmd/signatory/posture.go` and `cmd/signatory/burn.go` to call the new location. Behaviour preserved; pure rename + import shuffle.
- [ ] `internal/store/`: `RecordOwnership(repo_id, owner_id, source, observed_at)`, `CurrentOwner(repo_id) (*Entity, error)`, `OwnedRepos(owner_id) ([]Entity, error)`.
- [ ] `internal/signal/github/collector.go`: thread `store.Store` into the collector's constructor. In `collectOwnerProfile` (`:211-231`), after recording the signal, call `profile.EnsureEntity` for the owner URI and `RecordOwnership` for the edge. Idempotent on repeat runs.
- [ ] `internal/store/`: add `EffectiveBurn(entityID) (Burn, viaOwnerURI, error)` composing `GetBurn(entityID)` with `GetBurn(currentOwnerOf(entityID))`. Preserve existing `GetBurn` for audit-direct callers. Soft-delete inherited from underlying `GetBurn`.
- [ ] **Migrate display callers** (`internal/survey/survey.go:236`, `internal/summary/assemble.go:148`, plus any CLI renderers in `cmd/signatory/`) from `GetBurn` to `EffectiveBurn`. `signatory burn list` keeps `GetBurn`/`ListBurns` (audit surface).
- [ ] `signatory analyze`, `signatory summary`: render cascaded burns with the reason inline (`burned (via owner identity:github/<login>)`). Round-trip the BufferZoneCorp case end-to-end.
- [ ] `signatory survey`: surface owner-cascaded burns in action items.
- [ ] MCP `signatory_summary` / `signatory_signals`: include `effective_burn_via_owner` field.
- [ ] Tests: TDD pattern. The `EffectiveBurn` cascade is the load-bearing piece; failing test first.
  - Test case A: burn `identity:github/bufferzonecorp` after analyzing 1/17 repos. Analyze a 2nd repo — expect `burned (via owner)` from first refresh.
  - Test case B: unburn the operator (`burn remove`). `EffectiveBurn` on the previously-cascaded repo returns nothing.
  - Test case C: repo transfers from burned-operator to clean-owner via fresh `ownership_observations` row. Expect cascade no longer applies after transfer.
  - Test case D: repo transfers from clean-owner to burned-operator. Expect cascade now applies.
  - Test case E: direct burn on a repo PLUS owner burn — both apply; `EffectiveBurn` returns the direct one (precedence: direct beats cascade) with cascade as a sibling field.

Estimated scope: ~600 LOC + ~400 LOC tests. Recommended split: PR1 = migration v13 + ownership store methods + collector wiring; PR2 = `EffectiveBurn` + caller migration + rendering.

## 9. Connection to existing design

- **`design/entity-model-v2.md`** — `identity:` and `org:` URIs are already specified there AND already implemented (URI scheme constants at `internal/profile/uri.go:37-73`, entity-type mapping at `cmd/signatory/posture.go:402-420`). This doc adds an edge table and a resolver behaviour; no entity-model changes.
- **`design/identity-tracking-plan.md`** — Phase 4 ("Burn propagation") is the parent concept. This doc extracts the *publisher* slice of that work as a standalone deliverable so it doesn't wait on cross-platform identity linking, mailmap-derived graphs, or `account_takeover_indicators`. When identity-tracking-plan lands, `ownership_observations` and `contribution_observations` may merge into one table or stay separate; that's a v0.2 question.
- **`design/posture-relationships.md`** — version-scope inheritance is the model for "posture applies more broadly than the exact thing it's set on." Owner-cascade is the same pattern viewed through a different axis (publisher rather than version). The two should eventually compose: a posture on `identity:github/<login>` with `version_scope=any` cascades to all owned repos at all versions.
- **`internal/store/migrate.go`** — schema home. `dependency_observations` (v2 migration, lines 210-221) is the precedent for the table shape; v3's append-only enforcement (lines 40-44) is the precedent for the triggers. `burns` table (lines 140-147 + v6 soft-delete columns at lines 821-823) is what the cascade reads from. Migration v13 is the next free slot.
- **`design/example-litellm-attack.md`** / **`design/example-axios-attack.md`** / **`design/example-prtscan-attack.md`** — existing worked-attack documents. The BufferZoneCorp case is the next entry; once this design lands, the dogfood writeup at `design/analysis/bufferzonecorp-grpc-client.md` (TBD) is the regression test for owner-burn.
- **`design/trust-model.md`** — Principle #3 (retroactive degradation): owner-burn is the cleanest implementation of that principle for the campaign-attack case.

## 10. Decisions waiting on human review

Before starting implementation, confirm:

1. Reuse `identity:` for User-type owners and `org:` for Organization-type — no new scheme. ✓ (proposed §7.1)
2. Read-time cascade, not write-time. ✓ (proposed §3.3)
3. Append-only `ownership_observations`; "current" is derivable, not stored. ✓ (proposed §3.2)
4. v0.1 scope is publisher→repo only — no membership, no analyst-output identity extraction, no campaign-detection heuristic. ✓ (proposed §6)
5. Posture-cascade follow-up tracked but not in v0.1. ✓ (proposed §7.6)

If any of these flip, the implementation footprint grows but the test set stays roughly the same shape — the cascade resolver and the `ownership_observations` write path are the load-bearing pieces either way.
