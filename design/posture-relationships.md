# Posture relationships: version succession + cross-package successors

Status: **draft 2026-04-20** — brainstorm-to-design transcription. Not implemented. Tomorrow-me or future reader: decisions to make are in §8, then §9 is the implementation checklist.

Supersedes: nothing. Extends: `design/trust-model.md`, `design/entity-model-v2.md`.

---

## 1. Problem

During the msgpack-lite dogfood analysis (2026-04-20, synthesis at `filestore/analysis/msgpack-lite-synthesis.md`), the synthesist recommended migrating to `@msgpack/msgpack`. The recommendation currently lives only in the posture's rationale prose. That's three distinct failure modes:

1. `signatory survey` against an npm project that still has `msgpack-lite` shows tier=`rejected` with a rationale snippet, but nothing about *where to go instead*. The migration guidance is invisible to the workflow that would actually benefit from it.
2. `signatory_detail` (the MCP surface) returns the rationale but doesn't structurally link the two entities. An LLM consumer can read "migrate to @msgpack/msgpack" in the text, but has no queryable way to hop to that entity's profile.
3. Programmatic consumers (CI gates, dashboards) can't act on the recommendation without regex-parsing rationale strings.

Parallel observation (surfaced in the same conversation): postures are today **version-exact**. Setting `vetted-frozen` on `v4.1.4` says nothing about `v4.1.5` — the latter shows as `unexamined`. For a typical patch-release cadence, re-running the full analyst pipeline on every bump is wasteful. Most patches don't change the trust model; some do (the axios-2026 pattern). The model today forces re-analysis on every version, and has no shape for "inherited unless challenged."

Both observations point at the same missing layer: **relationships between postures**. Not relationships between entities (that's a different, harder problem — §7). Between postures.

## 2. Three relationship kinds

Separating them is the key design move. Conflating them produces a worse model — an "entity graph" that tries to serve three use cases and serves none well.

### 2.1 Cross-package supersession

"Package X is retired; consult package Y instead."

- Different URIs: `pkg:npm/msgpack-lite` → `pkg:npm/@msgpack/msgpack`.
- Different entities: different authors, different code, different signal shapes.
- Semantic, not structural: we're not claiming the packages are equivalent — we're recording an analyst's judgment about where users should go.
- Typically recorded alongside a rejected-tier posture on the predecessor.
- **Not transitive by default** (see §8.2).

### 2.2 Intra-entity version succession

"v4.1.4 is vetted-frozen, and v4.1.5 inherits that posture unless a signal says otherwise."

- Same URI. Different version strings.
- Inheritance is *declared* by the analyst (via a `scope` label on the posture) rather than inferred from version-number arithmetic. The analyst is the one who understood whether the patch-to-patch change is routine or risky.
- Lookup walks narrowest-first (exact → patch-scope → minor-scope → major-scope → any).
- Interacts with Phase B.6 cross-version signals (§5): when `postinstall_introduced` or `publish_origin_consistency` fires, inheritance should be advisory-flagged (v0.1) or auto-suspended (v0.2).

### 2.3 Aliasing

"These two URIs are actually the same entity."

- Cross-platform duplicates (if gitlab-hosted npm packages ever land).
- Lookalike-URI attempts (handled at validation time today — #78 byte-range check rejects the adversarial forms).
- For npm specifically, scoped vs unscoped name collisions don't exist: the registry enforces uniqueness.

**Not modeled in v0.1.** Current `ValidateCanonicalURI` + `NormalizeGitHubRepoInput` + `ResolveTarget` collapse the known alias forms at input time. A proper alias model would be v0.2+ work when non-GitHub platforms land.

## 3. Proposed model

Two small additions to the `postures` table. No new tables, no entity-graph machinery.

### 3.1 `version_scope` column

```
version_scope TEXT NOT NULL DEFAULT 'exact'
  CHECK (version_scope IN ('exact', 'patch', 'minor', 'major', 'any'))
```

Semantics:

- `exact` — current behavior. Posture applies only when the queried version matches exactly. This stays the default so the upgrade is backwards-compatible.
- `patch` — posture applies to any version that differs only in the patch component (e.g., `v4.1.4 patch` covers `v4.1.0`–`v4.1.∞`).
- `minor` — posture applies to any version in the same major line (e.g., `v4.1.4 minor` covers `v4.0.0`–`v4.∞.∞`).
- `major` — semver-style compatibility semantics don't really exist beyond the major line; kept for completeness (e.g., `v4.1.4 major` covers `v4.*.*`). Functionally equivalent to `minor` under standard semver, but split out in case of pre-1.0-quirks where major==minor semantics matter.
- `any` — "trust this package regardless of version." High-trust label; use sparingly.

### 3.2 `successor_uri` column

```
successor_uri TEXT NULL
  CHECK (successor_uri IS NULL OR length(successor_uri) <= 512)
```

- Nullable. Most postures leave it empty.
- When set, must pass `ValidateCanonicalURI` (printable ASCII, known scheme, ≤512 bytes).
- No referential integrity check — the successor entity doesn't have to exist yet in the store. That's desirable: analysts may record "migrate to X" before X has been analyzed. When X does get analyzed, the pointer resolves.

### 3.3 Lookup algorithm

Given `(uri, version)`, return the best-matching posture:

1. **Exact match.** Any posture with matching URI, matching version, and `version_scope='exact'`. If found, return it directly. This is today's behavior.
2. **Scope walk.** Otherwise, load all postures for URI. For each posture, check whether `(its_version, its_scope)` covers the queried version (see §3.4). Among matches, prefer the narrowest scope (patch beats minor beats major beats any). Return the winner, tagged as *inherited from X.Y.Z via patch-scope*.
3. **No match.** Return nothing; tier lookup proceeds to TierNotInStore / TierUnexamined as today.

The "inherited from X.Y.Z" tagging is surfaced in output so readers know they're one hop removed from the explicit analyst judgment.

### 3.4 Scope coverage semantics

Given a posture at version `P` with scope `S`, does it cover a queried version `Q`?

- `S=exact` → `P == Q`.
- `S=patch` → `major(P) == major(Q)` and `minor(P) == minor(Q)`. Q's patch can be anything.
- `S=minor` → `major(P) == major(Q)`. Q's minor and patch can be anything.
- `S=major` → same as minor in practice; reserved for the 0.x edge case where major-bump semantics are weaker.
- `S=any` → always covers.

Version parsing is strict semver: `v` prefix stripped, three-component `major.minor.patch` required, pre-release suffixes (`-alpha.1`) ignored for scope matching but kept for exact-match. Non-semver version strings (Go's `v1.2.3-20230101000000-abcdef`, calendar versions like `2026.01.15`) only match `exact` and `any`.

## 4. Surface changes

### 4.1 CLI

**`signatory posture set`** gains two flags:

```
--scope=exact|patch|minor|major|any    (default: exact)
--successor=<canonical-uri>              (default: none)
```

**`signatory posture get`** output includes scope and successor when set:

```
Posture:   rejected (version 0.1.26, scope=exact)
Rationale: ... see pkg:npm/@msgpack/msgpack for current-generation alternative
Successor: pkg:npm/@msgpack/msgpack
```

**`signatory analyze <target>`** human-readable view renders the successor as a trailing line:

```
Posture:   rejected (version 0.1.26)
Rationale: ...
           → migrate to pkg:npm/@msgpack/msgpack
```

**`signatory analyze <target>` JSON view** includes `successor_uri` as a top-level field on the posture record, making it trivial for CI gates to consume.

### 4.2 Survey

When a dep resolves to a rejected/burned tier AND has a `successor_uri`, the action-items block pivots from "analyze this" to "migrate":

```
Action items
  1 direct dependency rejected with recommended successor:
    pkg:npm/msgpack-lite @ 0.1.26 → migrate to pkg:npm/@msgpack/msgpack
```

When a dep version inherits a posture via scope, the icon reflects inheritance:

```
  [✓~] vitest   ^4.1.0   vetted-frozen   (inherited from v4.1.4 patch-scope)
```

Icon proposal: `[✓~]` for inherited (swap the existing `[✓]` when scope-inherited). Subject to UX review.

### 4.3 MCP tools

**`signatory_detail`** JSON response includes the new fields on the posture object. LLM consumers reading a detail can follow the successor pointer with a fresh `signatory_detail` call.

**`signatory_show_conclusions`** — no change. Conclusions are about analyst findings, not posture relationships.

**New field, no new tool.**

## 5. Interaction with Phase B.6 cross-version signals

The cross-version signals shipped in commit `5f5519b`:

- `postinstall_introduced` — flags when a version adds a postinstall script where older recent versions had none.
- `publish_origin_consistency` — tracks attestation-presence transitions and unique-publisher churn across recent versions.

These are the exact forensic gates that detect the axios-2026 compromise pattern. When they fire on a version whose posture was about to be *inherited* from a predecessor, inheritance without re-examination is unsafe.

**v0.1 behavior (this doc's scope):** advisory. The signal appears in the profile; the inherited posture still renders with its original tier, but `analyze` and `survey` display a warning:

```
Posture:   vetted-frozen (inherited from v4.1.4 patch-scope)
           ⚠ postinstall_introduced=true on v4.1.5 — inheritance may be stale; consider re-analyzing
```

**v0.2+ behavior (future work):** automatic inheritance suspension. When any of a specified signal list fires with "risky" values, the scope-inherited posture is returned as `unexamined` with a reason pointing at the triggering signal. This requires:

- A declared "suspension-trigger" signal set. Minimum candidates: `postinstall_introduced=true`, `publish_origin_consistency.attestation_transitions > 0`, `trusted_publishing` flipped from true to false.
- A clear UI for "why is this version unexamined despite inheritance?" — probably a reason string appended to the posture result.
- Decision: do we auto-suspend for ANY cross-version signal firing, or only for the enumerated severe ones? The latter is safer against over-triggering; the former is more conservative against attacks.

Not in v0.1 — the signals themselves are v0.1, but hooking them to posture validity is a trust-policy decision, not a schema decision, and worth its own design pass once we have real data on how often they fire.

## 6. Migration

SQLite migration:

```sql
ALTER TABLE postures ADD COLUMN version_scope TEXT NOT NULL DEFAULT 'exact'
    CHECK (version_scope IN ('exact', 'patch', 'minor', 'major', 'any'));

ALTER TABLE postures ADD COLUMN successor_uri TEXT NULL;
```

Existing rows get `version_scope='exact'`, `successor_uri=NULL` by default — zero behavior change for existing postures.

The check constraint for `successor_uri` (≤512 bytes, passes `ValidateCanonicalURI`) is enforced at the application layer rather than in SQLite, matching the existing `canonical_uri` validation pattern in `store.PutEntity`. Consistency with #78's approach.

## 7. What this is NOT

Explicit non-goals, so the scope stays tight:

- **Not** a general entity-relationship graph. We're not modeling `fork_of`, `competes_with`, `maintained_by`, etc. Those are the Option-3 design pass this doc's Option-2 exists to avoid.
- **Not** a fork-tracking mechanism. `vitest` and `vitest-fork` would each get their own posture; a `successor_uri` would only make sense if the fork had supplanted the original.
- **Not** a package-alias system. Canonical URI normalization handles the aliases that exist today (`pkg:npm/express` always resolves the same way). Non-GitHub platform aliasing is v0.2+.
- **Not** an auto-migration recommender. The tool records analyst judgment about where to migrate; it doesn't generate the recommendation itself. That's what `/analyze` produces during synthesis.
- **Not** a posture-chain. There's no "v4.1.5's posture supersedes v4.1.4's"; each posture is standalone, with an optional scope label controlling which versions it applies to.

## 8. Open questions (decide before implementing)

### 8.1 Semver strictness

Q: How strict is the semver parser? Does `v1.2.3-alpha.1` match a scope-patch posture on `v1.2.3`? Does `2026.01.15` (calendar versioning) work at all?

Default answer: strict semver (three numeric components, optional `v` prefix, optional pre-release). Non-conformant versions degrade to `exact` and `any` scope matching only. `patch`/`minor`/`major` scopes require all parties to be parseable semver.

Risk: some ecosystems (Go modules with pseudo-versions like `v0.0.0-20230101000000-abcdef`) won't benefit from scope inheritance. Acceptable tradeoff — Go is go-modules' problem; Phase-A+B targets npm where semver is ubiquitous.

### 8.2 Successor transitivity

Q: If A's posture names B as successor, and B's posture names C as successor, does a consumer querying A see C?

Default answer: **no, one-hop only.** Reasons:

- Loop prevention is easier.
- Analyst intent is "here's where to go NOW" — not "here's where to go eventually."
- If chained migration is real (A was retired in favor of B, which was itself later retired in favor of C), the right move is to update A's posture to point at C. Humans stay in charge of the chain.

Revisit if the one-hop model feels genuinely limiting after six months of use.

### 8.3 Schema location

Q: Additive columns on `postures`, or a separate `posture_relationships` table?

Default answer: **additive columns**. Two reasons:

- Additive is migration-cheaper (one ALTER vs. new table + foreign keys + join queries).
- Separate table only earns its keep if we need multiple successors per posture or first-class relationship metadata. Neither applies today.

Revisit if we land Option-3 (full entity-relationship graph), at which point the relationships move to their own table and `postures.successor_uri` becomes a view.

### 8.4 Inheritance rendering

Q: Icon? Text label? Both? Does `survey`'s tier count show inherited postures in the same bucket as explicit ones, or separate?

Default answer: icon-modified (`[✓~]`) plus parenthetical note. Counts merge into the explicit bucket (inherited `vetted-frozen` counts as `vetted-frozen`) since the trust judgment IS the same tier — just scope-applied.

### 8.5 Multi-version line scoping

Q: If v4 line is vetted-frozen (minor scope) but v5 line is unexamined, and someone sets a NEW posture on v4.2.0 with patch scope — does the minor-scope v4.x posture still cover v4.1.x, with the patch-scope v4.2.x posture as an override?

Default answer: yes. Narrowest scope wins. The lookup walk in §3.3 ensures this naturally.

Risk: posture history becomes layered and harder to reason about. Mitigation: `signatory posture get --all` shows every posture for an entity; the rendering makes the scope explicit so the "why does this version match THAT posture" question is always answerable from the output.

### 8.6 Cross-version signal trigger set

Q: For the v0.2 auto-suspension feature, which signals qualify as "invalidation triggers"?

Deferred. Current lean: start with `postinstall_introduced`, `publish_origin_consistency.attestation_transitions > 0`, and `trusted_publishing` becoming false after being true. Expand only with evidence. This is a trust-policy question, not a schema question, so it can be iterated without schema changes.

## 9. Implementation checklist (when tomorrow-me comes back)

Not implemented yet. When ready:

- [ ] Migration: ALTER `postures` with two columns, defaults preserve current behavior.
- [ ] `profile.Posture` struct: `Scope string`, `SuccessorURI string`. Store handles marshal/unmarshal.
- [ ] `store.SetPosture`: persist new fields. `store.GetPosture` / `GetPostures`: return them.
- [ ] Version-matching helper: new `internal/profile/version_scope.go` with `CoversVersion(postureVersion, scope, queried string) bool`. Strict semver. Extensive table-driven tests.
- [ ] `ResolvePosture(uri, version)` — the scope-walk lookup (§3.3). Unit tests with mixed exact/patch/minor postures.
- [ ] CLI: `--scope` and `--successor` on `posture set`; render both on `posture get` and `analyze`.
- [ ] Survey: successor rendering in action items; inherited-scope rendering in dep table.
- [ ] MCP: `signatory_detail` includes new fields.
- [ ] Cross-version signal interaction (v0.1): advisory warning when inherited posture + risky signal value.
- [ ] Tests: TDD pattern on each piece. The version-scope helper and the scope-walk lookup are the two places correctness matters most.
- [ ] Design doc updates: this doc moves from `draft` to `implemented` once landed; v0.2 trigger-set decision gets its own follow-up doc.

Estimated scope: ~400 LOC + ~400 LOC tests. One focused commit or two (schema+core + rendering).

## 10. Connection to existing design

- **`design/trust-model.md`** — this extends the tier system without changing it. Tiers stay the same; scope is new metadata about WHICH versions get the tier.
- **`design/trust-policy-v1.md`** — the `signatory survey` tier ordering is unaffected. Inherited postures collapse into the same tier bucket.
- **`design/entity-model-v2.md`** — no changes. Entities stay the same. Postures get new columns.
- **`design/npm-plan.txt`** — Phase B.6's cross-version signals are the forensic gate described in §5. This doc makes their consumer-facing purpose explicit.
- **`design/example-axios-attack.md`** — the attack pattern this design defends against via the signal/inheritance interaction.

## 11. Decisions waiting on human review

Before starting implementation, confirm:

1. Default scope is `exact` — no silent behavior change. ✓ (proposed)
2. Successor is one-hop, no transitive chaining. ✓ (proposed)
3. Schema is additive columns. ✓ (proposed)
4. Inherited postures render differently than explicit. ✓ (proposed icon + text)
5. v0.1 signal interaction is advisory. ✓ (proposed)
6. Strict semver only for scope matching. ✓ (proposed)

If any of these need to flip, the downstream implementation changes are small but the test set grows. Flag them in the review pass before coding starts.
