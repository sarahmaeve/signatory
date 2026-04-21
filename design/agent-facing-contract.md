# Agent-Facing Contract

**Status:** Accepted — 2026-04-21. Six decisions locked (§7). Implementation sketch in §5 binds ordering for the six milestones; each is its own PR. Prior draft was opened the same day after three consecutive dogfood runs (escape-html, invariant@2.2.4, kong, golang.org/x/mod) surfaced the same class of friction at different CLI/MCP/filestore seams.

**Scope:** How signatory presents itself to the two kinds of agents that drive it — LLM pipeline agents (the `/analyze` skill, the security/provenance/synthesist subagents) and MCP-connected assistants in coding workflows. Out of scope: the Layer 1 / Layer 2 trust model (see [`trust-model.md`](trust-model.md)), the agent-output JSON contract (see [`agent-output-contract.md`](agent-output-contract.md)), v0.1 scope boundaries (see [`v0.1-invariants.md`](v0.1-invariants.md)).

## 1. Motivation

Signatory has grown four distinct agent-facing surfaces — the `signatory` CLI (22 subcommands), the MCP tool registry (8 tools), the MCP resource tree (6 resources), and the filestore directory conventions — plus the canonical URI scheme that nominally ties them together. Each surface was designed locally. The seams between them are where the dogfood failures live.

Concrete failures from the last 48 hours, each previously treated as a local bug:

- **`posture set` defeated Opus on first invocation** when told "just do it." Agent produced a wrong-row and had no way to delete it. Root cause: flag density + mutating verb with no undo.
- **`pkg:npm/X` analyses recorded under `repo:github/...`**. User's stated identity silently replaced with the resolved identity; `signatory_analyze pkg:npm/X` returns NotFound the next day despite the work being done.
- **`pkg:go/golang.org/x/mod` failed precheck and clone**, fell back to hand-mapping, succeeded under the github URI. Same pattern as npm, different ecosystem, same silent identity drift.
- **Synthesist browsed `filestore/analysis/` to read prior syntheses before writing its own**. Anchoring / calibration drift hazard. Cross-pollinates judgment across independent assessments.
- **Synthesist spent 16+ minutes and 50+ tool calls** trying multiple CLI flag shapes, MCP tool variants, and filestore reads to gather its inputs. No single canonical source of "the conclusions for this target."

These are not four bugs. They are four views of one underlying problem: **the agent-facing surface of signatory is a patchwork, and agents cannot route through it without trial and error.** Humans tolerate trial and error because they can read docs and remember last time. Agents cannot. Every inconsistency costs tokens, produces bad rows, or anchors decisions incorrectly.

## 2. Principles

The contract is governed by six principles. Every entry point should satisfy all six; violations should be documented and justified.

### P1. One target grammar, everywhere.

Every target-shaped argument — CLI positional, MCP tool input, handoff placeholder — accepts the same forms and resolves through the same function (`profile.ResolveTarget`). The response names the canonical URI the input resolved to. If the caller's URI resolves into a *different* identity where data lives (e.g., `pkg:npm/X` → `repo:github/Y`), the response includes the successor URI explicitly — never a silent rewrite.

### P2. Recorded work is indexed under the identity the caller used.

If the caller asked about `pkg:npm/X`, analyses and postures are recorded under `pkg:npm/X`, with a `collected_from` link to whatever identity the work was actually performed against. Next-time lookups under the original identity succeed. Identity drift between request and storage is eliminated.

### P3. Every mutating verb has a paired undo.

`posture set` → `posture unset`. `burn add` → `burn remove`. `ingest` → `withdraw`. A surface that produces bad rows (see §1) and provides no recovery turns every agent mistake into a manual sqlite edit. Undo is contract-level, not feature-level.

### P4. Multi-line free text goes through files, not shell quoting.

`--rationale-file <path>` is the canonical form. `--rationale <oneliner>` is a convenience for one-line inputs. Agents write rationale/intake/notes to a file and pass the path. Heredocs and escape-quote gymnastics stop being the expected invocation path.

### P5. Analyst inputs are structured and scoped; analyst outputs land in the store.

The synthesist's handoff body contains the target's rolled-up conclusions, positive absences, and observations in a canonical JSON structure — plus the exact output skeleton. The synthesist does not browse `filestore/analysis/`. It cannot read other targets' work. Its output is a first-class analysis record in the store (new analyst type: `synthesist`), with a structured `proposed_posture` field; the markdown file is a *rendered view*, not the source of truth.

### P6. Ecosystem resolution is a named, pluggable capability.

`pkg:<eco>/<name>` → "declared source" lookup is one abstraction, not N branches scattered across precheck / clone / analyze. Each ecosystem (npm, go, pypi, cargo) registers a resolver; every target-accepting verb consults the registry when it sees a `pkg:` URI it needs to trace to a source. The npm bridge shipped 2026-04-20 is the first instance, not the canonical one.

## 3. The contract (concrete rules)

### 3.1. Target input

Every target-accepting entry point:

- Accepts any form `profile.ResolveTarget` accepts.
- Emits the canonical URI in structured responses (JSON, MCP return values, log lines).
- If the input carries a version suffix (`pkg:npm/X@V`, `pkg:go/...@V`), the version parses into a dedicated field on the resolved target. `--version` is an override and errors loudly on conflict with the URI-embedded version.
- Never silently rewrites the caller's identity. If resolution hops from `pkg:X` to `repo:Y`, the response cites both: `{"target": "pkg:npm/X", "resolved_source": "repo:github/Y"}`.

### 3.2. Recorded identity

When the pipeline analyzes `pkg:<eco>/X` via its resolved source `repo:<platform>/Y`:

- The store row is indexed under `pkg:<eco>/X` (caller's identity).
- A structured `collected_from: repo:<platform>/Y` field links to the source used.
- Queries under either URI return the same record, with the linkage made visible in the response.
- Versioned identities (`pkg:npm/X@V`) are first-class: a record under `pkg:npm/X@2.2.4` does not satisfy a query for `pkg:npm/X` unversioned; posture scope resolution ([`posture-relationships.md`](posture-relationships.md)) handles the narrowing/widening. Unversioned queries *may* return "we have records for: 2.2.4, 2.2.3, …" to preserve the trail.

### 3.3. Mutating verbs

Every mutating verb provides:

- A `--dry-run` flag that prints what would change without writing.
- A paired undo subcommand: `posture unset`, `burn remove`, `ingest withdraw`, etc.
- Exit code 64 (EX_USAGE) for "caller supplied a nonsense combination" (e.g., `--version 1.0` with `pkg:npm/X@2.0` on the URI).
- Exit code 1 for "operation failed after validation passed" (DB locked, IO error, etc.).

Undo verbs accept the same target forms as their sibling `set`/`add`. A row inserted via `posture set pkg:npm/X --tier trusted-for-now` is removable via `posture unset pkg:npm/X`.

### 3.4. Free-text inputs

For any flag that commonly carries multi-line content (rationale, intake question, synthesis body):

- Primary form: `--<name>-file <path>`. Reads the file, strips trailing newline, stores verbatim.
- Convenience form: `--<name> <string>`. For single-line inputs. Rejects strings containing newlines.
- When both are provided, `--<name>-file` wins and `--<name>` is an error.
- `-` as the file path reads from stdin.

This is how an agent supplies a rationale: write to a temp file, pass `--rationale-file /tmp/rat`. No heredoc, no bash quoting, no escape-character math.

### 3.5. Synthesist inputs

The synthesist handoff (deposited via `signatory handoff synthesist <target>`) carries:

- Target canonical URI.
- Rolled-up conclusions for that target, across all prior analyst records (security, provenance): verdict, severity, rationale, citations, forgery-resistance, design-intent flag.
- Rolled-up positive absences (what was looked for and not found).
- Rolled-up observations (informational context).
- The output skeleton (markdown template) — inlined, not fetched from filestore.
- An explicit prohibition: "Your inputs are the conclusions listed above. Do not read other analyses in `filestore/analysis/`. Do not browse prior syntheses. Your judgment is derived from evidence, not precedent."

The synthesist handoff template lives at `templates/handoffs/synthesist-v1.md`. The format skeleton and the evidence envelope are both in the handoff body — one source, one delivery.

### 3.6. Synthesist output

The synthesist deposits a structured analysis record with `analyst_id: signatory-synthesis-v1`. The record contains:

- The full synthesis prose (as with other analyst records).
- A `proposed_posture: {tier, version_scope, rationale_summary}` field.
- Optional `contradictions_detected: []` and `concordance_strengths: []` arrays.

`filestore/analysis/<target>-synthesis.md` is generated by the CLI as a *view* of this record — not the source of truth. A future "accept synthesis" verb (`signatory posture accept <synthesis-record-id>`) promotes the proposed posture into a real posture row with two commands total.

### 3.7. Ecosystem resolution

A registry of ecosystem resolvers, keyed by the `pkg:<eco>/` prefix:

```go
type EcosystemResolver interface {
    ResolveSource(ctx context.Context, name, version string) (DeclaredSource, error)
}

type DeclaredSource struct {
    URI         string  // repo:github/... or repo:gitlab/... or ""
    SelfReported bool   // always true for current ecosystems; left extensible
    VerifiedBy   string // "", "sigstore", "slsa-l3", etc. — room to grow
}
```

Registered resolvers per ecosystem (v0.1: npm + go; v0.2+: pypi, cargo). Every target-accepting verb that needs a source repo consults `ecosystem.Resolve(uri)` — one call site, one abstraction, consistent failure modes.

Network-precheck, clone-dir, handoff, and the `/analyze` skill all route through this registry. The npm bridge shipped 2026-04-20 is ported to be the first registered resolver.

## 4. How the three known problems collapse

| Symptom | Principle(s) violated | Fix |
| --- | --- | --- |
| Synthesist reads prior syntheses from filestore | P5 | §3.5 (structured input) + §3.6 (output-as-store-record) |
| `pkg:X` analyses recorded under `repo:Y` | P2 | §3.2 (caller-identity-indexed records) |
| `--network-precheck` rejects `pkg:go/...` | P6 | §3.7 (ecosystem resolver registry) |
| `posture set` defeats Opus on one-shot invocation | P3, P4 | §3.3 (undo verb) + §3.4 (rationale-file) + synthesist-proposes-posture pattern in §3.6 |
| `pkg:npm/X@V` silently drops `@V` without `--version` | P1 | §3.1 (URI version is first-class) |

Five dogfood observations. Six principles. They are the same problem.

## 5. Implementation sketch

This is shape, not a commitment. Each milestone lands as its own PR with its own tests; milestones are orderable.

**M1 — Target grammar unification.**
- `ResolveTarget` learns the `@version` suffix on `pkg:` URIs.
- Every mutating verb validates `--version` against URI-embedded version and errors on conflict.
- Per-version burns fall out for free: `burn add pkg:npm/invariant@2.2.4` works the same as any other target form.
- ~200 LOC + ~300 LOC tests.

**M2 — Identity-indexed storage.**
- Analysis and posture rows gain a `collected_from` column.
- Write-path keys on caller-identity; read-path walks `collected_from` for "show me everything related to this identity."
- Migration: backfill existing rows with `collected_from = canonical_uri` (self-link).
- ~300 LOC + ~400 LOC tests + migration.

**M3 — Ecosystem resolver registry.**
- Extract npm bridge into `internal/ecosystem/resolver/`.
- Register npm; add go resolver (`proxy.golang.org` → `repository.url`).
- Every `pkg:` consumer (precheck, clone, handoff target rewriting) goes through the registry.
- ~400 LOC + ~500 LOC tests.

**M4 — Undo verbs + dry-run.**
- `posture unset`, `burn remove`, `ingest withdraw`.
- `--dry-run` on every mutating verb.
- Exit codes aligned with the contract.
- ~250 LOC + ~350 LOC tests.

**M5 — File-based free-text inputs.**
- `--rationale-file` added to `posture set`; `--rationale` keeps one-line semantics.
- `--intake-file` on handoff.
- `-` reads stdin.
- ~100 LOC + ~200 LOC tests.

**M6 — Synthesist contract.**
- `signatory handoff synthesist` embeds structured evidence (reusing M7's rollup) + output skeleton + prohibition.
- Synthesist deposit schema extended with `proposed_posture`.
- Filestore synthesis markdown becomes a render of the store record, not a primary artifact.
- `signatory posture accept <synthesis-id>` promotes proposal to posture.
- Same cross-pollination prohibition is extended to the security and provenance analyst handoffs (cheap addition to `templates/handoffs/security-review-v1.md` and `templates/handoffs/provenance-review-v1.md`).
- ~500 LOC + ~600 LOC tests.

**M7 — `signatory summary` verb.**
- CLI: `signatory summary <target>`. MCP: `signatory_summary`.
- Returns canonical URI + `collected_from` links + current posture (tier/scope/rationale) + burn status + analysis rollup (conclusion count by severity, positive absences, observations, proposed postures).
- Sits on top of M2 (needs identity-linked reads) and feeds M6 (synthesist's structured input is `summary` output + extra evidence fields).
- Also consumed by the /analyze skill pre-dispatch: check summary first, short-circuit if prior work exists.
- ~300 LOC + ~400 LOC tests.

**M8 — /analyze retirement into `signatory run analysis`.**
- Port the /analyze skill's orchestration (session create → handoff → deposit → dispatch analysts → verify ingestion → dispatch synthesist → present proposed posture) into a first-class CLI verb.
- Human or LLM invokes the same entry point.
- /analyze SKILL.md becomes a thin "use `signatory run analysis`" pointer.
- ~400 LOC + ~500 LOC tests + skill file update.

Order: M1 → M5 → M4 → M3 → M2 → M7 → M6 → M8. Earlier milestones unblock immediate agent workflow; M6 wants M7 real (for structured synthesist input); M8 wants everything real.

## 6. Non-goals for this pass

- **Web/GUI surface.** Contract is for CLI + MCP + handoff agents only. A GUI is v0.3+ and will adapt to the contract, not the reverse.
- **Multi-user authorization model.** v0.1 is single-user; the contract leaves room for per-identity audit fields but doesn't define auth.
- **Cross-host sync / federation.** Burn-list federation and posture sharing across orgs are named in [`trust-model.md`](trust-model.md) but out of scope here.
- **Agent scheduling / autonomy.** When and how often `/analyze` runs is a policy layer on top of the contract, not the contract itself.

## 7. Decisions (locked 2026-04-21)

**D1. Resolution-hop behavior.** Transparent-with-citation. Never invisibly merge identities; always show the resolution hop so duplicates are hard to create and provenance is easy to follow. Optional `--no-resolve` flag exists for audit mode but is not the default.

**D2. Historical records migration.** Drop and re-collect. V0.1 has ~10 records and we can be rough with our own DB in pre-alpha. No backfill logic; release notes note "re-run `/analyze` on anything you care about after M2 lands."

**D3. Synthesist output location.** Store-first. Structured record in SQLite is the source of truth; markdown/HTML rendering is a view generated at read time. Eliminates the browsable-filestore cross-pollination temptation.

**D4. Versioned vs unversioned queries.** Exact-match; unversioned URI is its own identity. A query for `pkg:npm/X@2.2.4` returns the 2.2.4 record or nothing — never 2.2.5 silently. When data exists under other versions of the same root name, the response includes a "data on other versions available" hint listing the known versions. An `--all` flag flattens to show every record under the unversioned root.

**D5. Undo semantics.** Soft delete for `posture unset` and `burn remove` — preserves audit trail (who set what, who withdrew when). For ingest withdraw: records are never hard-deleted; they are marked with an `INGEST_ERROR` status that excludes them from normal queries but keeps the row and its error context for post-hoc analysis. Same append-only invariant as analyses, different status values: `active | withdrawn | ingest_error`.

**D6. Scope for v0.1.** All six milestones are v0.1-blocking. The contract is foundational enough that shipping v0.1 with the current seams means first external eyeballs meet the friction we just spent two days documenting. Target ordering: M1 → M5 → M4 → M3 → M2 → M7 → M6 → M8. Each milestone lands as its own PR.

**D7. Git SHA / content-hash in the version grammar.** Not for v0.1. Version grammar is semver-ish: `pkg:<eco>/<name>@<version>` where version is a string the ecosystem recognizes (semver for npm/cargo, Go module version for Go, etc.). If a specific commit is known-bad (kong's `v1.14.0` case), the correct user action is `burn add pkg:<eco>/<name>@<version>` on that version — per-version burns fall out for free once M1 lands. Future: content-hash pinning (BLAKE3 or similar) is a v0.2+ topic if pressure for it grows.

**D8. Name of the unified "what do we know about X" verb: `signatory summary`.** Composes posture + burns + analyses rollup + `collected_from` links into one response. Matches the word a user or agent would reach for naturally. New milestone M7 (see §5), sits after M2 (needs identity-linked reads to be real) and before M6 (the synthesist's structured input is `summary` output with extra evidence fields).

**D9. Analyst fence scope: all three analyst types.** Security and provenance analysts, not just the synthesist, carry the "do not read other analyses in `filestore/analysis/`; do not browse prior analyses of any target to calibrate" prohibition in their handoffs. Cross-pollination risk is lower for them (their primary inputs are source code) but not zero, and the prohibition is cheap to add.

**D10. /analyze skill retirement.** Yes — as a downstream goal. Once M1–M7 land, most of /analyze's 150-line bash orchestration is redundant. Replace with a single `signatory run analysis <target>` (or similar) CLI verb that a human or LLM invokes directly. Named here as M8; requires M1–M7 to be real before it can work. Worth pursuing for human and agent ergonomics equally.

## 8. Cross-references

- [`posture-relationships.md`](posture-relationships.md) — version scope + successor URI (the storage-side counterpart to §3.2 and §3.7).
- [`agent-output-contract.md`](agent-output-contract.md) — JSON-vs-prose for analyst output (orthogonal; this contract covers *what agents see*, that contract covers *what agents produce*).
- [`v0.1-invariants.md`](v0.1-invariants.md) — scope boundaries and the Layer 1 / Layer 2 discipline this contract enforces.
- [`ANTIPATTERNS.md`](ANTIPATTERNS.md) — past mistakes this contract is shaped by.
- MEMORY `project_dogfood_posture_gaps.md` — the 2026-04-20 observations that kicked this off.
