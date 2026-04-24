# Improve provenance signals — move mechanical work to signatory

Last updated: 2026-04-24 (post-Phase-2).

## Status

Implementation in progress. Direction and key design decisions are
settled from the 2026-04-23 dogfood session on `github.com/alecthomas/kong`;
this document is the execution plan. Work lands incrementally on `main`
(single-developer project).

**Phase progress:**

| Phase | Commit | Status |
|-------|--------|--------|
| 1. Inline Layer-1 signals into handoff body | `8bcd634` | **Shipped (measurement deferred)** |
| 2. Repo-hygiene collector (README, SECURITY, CODEOWNERS, .mailmap, CHANGELOG, CONTRIBUTING, AUTHORS, MAINTAINERS, GOVERNANCE) | pending | **Shipped (measurement deferred)** |
| 3. CI pinning collector (parse .github/workflows, SHA-vs-tag) | — | Not started |
| 4. Tool-reproducibility collector (hermit, nvmrc, tool-versions, ...) | — | Not started |
| 5. Identity cross-reference (owner ↔ commit-email domain) | — | Not started |
| 6. Orphan-tag detection (cross-reference tags with releases) | — | Not started |
| 7. Registry ↔ source SHA match (Go proxy; npm later) | — | Not started |
| 8. GHSA advisories collector | — | Not started |
| 9. Handoff rewrite — judgment-over-brief | — | Not started |

**Dogfood-measurement status.** The plan originally called for a
per-phase dogfood run on `kong` to measure token / tool-use delta
against the Phase-0 baseline (51,699 tokens / 26 tool_uses / 5-of-8
rederivations). Phase 1 proved the cadence is not currently
sustainable: subagent-dispatched tool calls (WebFetch against
`api.github.com` / `proxy.golang.org`, Read/Glob/Grep against
`filestore/clones/**`) aren't in the project's persistent allowlist,
so each dogfood run requires tens of permission clicks. Phase 1's
correctness was instead verified by unit tests (5 passing) and
payload inspection (the rendered kong provenance handoff grew
22,510 → 31,103 bytes, with the inlined signals block carrying all
four key fields the agent was re-deriving).

Rather than block each phase on permission resolution, the plan is
amended: **correctness continues to be unit-test-driven per phase;
token-delta measurement is consolidated into a single cumulative
dogfood after Phase 5 (or at any earlier natural pause).** The
permission friction itself is tracked as a separate concern in
`sync/KAIZEN.md` and not within scope of this provenance arc.

## Problem

The provenance analyst agent duplicates work signatory's mechanical
collectors already do (or could do). In the 2026-04-23 kong dogfood the
agent burned **51,699 tokens / 26 tool_uses / ~4.9 min** to produce 8
findings, of which **5 were rederivations** of facts already in
signatory's Layer-1 signal cache:

| Agent finding | Already cached under |
|--------------|---------------------|
| F001 solo maintainer (alecthomas 271 vs #2 gak 14) | `contributors.top`, `effective_maintainer_concentration.top_authors` |
| F002 last push / tag cadence / star count | `vitality.last_push.date`, `criticality.stars.count`, `publication.tags.recent` |
| F004 10/10 recent commits GPG-verified | `commit_signing: {ratio:1, signed:10, total:10}` verbatim |
| F005 three deps, two self, one indirect | `go_dependencies: {direct_count:2, indirect_count:1, total_count:3}` verbatim |
| F007 alecthomas account 17+ years old | `owner_profile.account_age_days: 6331` |

The remaining 3 findings (proxy.golang.org SHA match, CI action pinning
shape, missing repo-root hygiene files, Hermit tool pinning, no GHSA
advisories, orphan-tag defect, owner-email↔commit-email domain match)
are **all mechanically collectible** — each is a file-stat, a YAML
parse, an HTTP GET, or a cross-reference of signals the store already
has. None requires judgment; the agent was reinventing collectors.

The agent was doing what signatory should be doing.

## Principle

**If signatory can collect it mechanically, signatory should.** The
provenance agent's remaining job is:

1. **Judgment** — given this brief, what trust posture does the model
   recommend, and why?
2. **Cross-referencing** — which facts corroborate, which contradict?
3. **Follow-up** — investigate when signals surprise (Read/Glob/Grep
   the local clone, WebFetch for edge cases). Rare; not routine.

Everything else moves to the collector side. "Routine collection inside
the agent" is the antipattern this plan eliminates.

The bar for "done" is the user's statement:

> If the provenance agent only did thinking or thinking + follow up,
> I'd be happy.

## Approach — why (C), not (A) or (B)

During planning three shapes were considered:

- **(A)** Inline existing Layer-1 signals into the handoff body.
  Recovers the 5 rederivations; leaves the other 3 novel-looking
  findings as agent work.
- **(B)** Tell the agent what signatory has (without providing values).
  Weakest — agent can't cite cache values in findings.
- **(C)** Move as much mechanical work as possible to signatory; keep
  the LLM for judgment. (A) is a strict subset.

(C) is the right target. The earlier framing "defers value to v0.2"
was over-conservative: each new collector is modest (file-stat, YAML
parse, HTTP GET), the agent-facing contract doesn't change (v1 schema,
MCP ingest), and the handoff template rewrite is prose. (A)'s inlining
remains the foundation step because every subsequent collector ships
its output through the same handoff-body mechanism.

## Design decisions

### Signal taxonomy

All new collectors land under the existing top-level `hygiene` group,
alongside `hygiene.ci_cd.providers` and `hygiene.license`. New
subgroups:

- `hygiene.repo_files` — SECURITY.md, CODEOWNERS, .mailmap, CHANGELOG,
  CONTRIBUTING presence
- `hygiene.ci_pinning` — per-`uses:` SHA-vs-tag classification across
  `.github/workflows/*.yml`
- `hygiene.tool_pinning` — hermit, .nvmrc, .tool-versions,
  .python-version, etc.

Keeping everything under `hygiene` avoids bikeshedding top-level groups
and matches the existing pattern (`license`, `ci_cd.providers` are
already hygiene-flavored).

### Collector execution model

New verb: **`signatory collect --target <T> --clone <path>`**. The
skill runs it between "clone" and "handoff":

```bash
signatory handoff security "$TARGET" --clone-dir filestore/clones/ --deposit-to "$SID" ...
signatory collect "$TARGET" --clone "filestore/clones/$TARGET_NAME"
signatory handoff provenance "$TARGET" --deposit-to "$SID" ...
```

Rationale:
- Matches the skill's existing discrete-composable-verbs shape.
- Keeps `handoff` a pure render step (render inputs come from the
  store; collection is upstream).
- Keeps `analyze`'s contract clean (API-side only; doesn't sometimes
  secretly do local-clone work).

Rejected alternatives:
- Run clone-reading collectors inside `signatory handoff` just before
  render — couples collection to render; handoff becomes a meta-verb.
- Overload `signatory analyze` with `--clone <path>` — makes the verb's
  contract fuzzy (sometimes network-only, sometimes network + local).

### Work on `main` directly

Single-developer project; branch+PR ceremony is unnecessary friction.
Each phase is one or more commits directly on `main`. Pre-commit hook
(gofmt + vet + tests) is the quality gate.

## Phase plan

Ordered so each phase's infrastructure feeds the next. Each phase ends
with a dogfood run against `github.com/alecthomas/kong`; the measured
delta (tokens, tool_uses, findings that are rederivations vs novel) is
the input to "proceed to next phase."

| # | Commit | What the agent stops doing |
|---|--------|---------------------------|
| 1 | `provenance: inline Layer-1 signals into handoff body` | Re-deriving commit-signing ratio, contributor list, owner-profile age, go_dependencies, tag/star counts, last-push date. Recovers the 5/8 rederivations from kong. |
| 2 | `signal: repo-files collector (README / SECURITY / CODEOWNERS / .mailmap / CHANGELOG / CONTRIBUTING / AUTHORS / MAINTAINERS / GOVERNANCE)` | Observing "missing SECURITY.md etc." — it's a signal now. README / AUTHORS / MAINTAINERS / GOVERNANCE added during planning as free additions under the detect-vs-rank scaffold. |
| 3 | `signal: ci-pinning collector (parse .github/workflows/*.yml, SHA-vs-tag)` | Reading workflow YAML to report pinning shape. |
| 4 | `signal: tool-pinning collector (hermit / nvmrc / tool-versions / python-version)` | Noticing "Hermit bin/ pinning" as an agent observation. |
| 5 | `signal: owner↔commit-email domain cross-reference` | Cross-checking "does alec@swapoff.org align with the profile blog URL?" |
| 6 | `signal: orphan-tag detection (cross-reference tags with release metadata)` | Browsing open issues to find publish-pipeline defects. |
| 7 | `signal: registry↔source SHA match (Go proxy; npm later)` | Live-fetching proxy.golang.org to verify published-version commit match. |
| 8 | `signal: ghsa-advisories collector` | Querying the advisory DB as a positive-absence check. |
| 9 | `provenance: rewrite handoff as judgment-over-brief; document tools as follow-up-only` | Prompt now matches the bar: thinking + optional follow-up. |

### Phase 1 — Foundation (shipped `8bcd634`)

Add a `{LAYER_1_SIGNALS}` placeholder to the provenance handoff
template. `signatory handoff provenance` opens the store at render
time, assembles the cached signal block (reuse the same composer
`signatory_analyze` and `signatory analyze` use), substitutes into
the placeholder.

Template prose adds an explicit "trust these as ground truth; do
NOT use WebFetch to re-query — cite values directly in findings"
section above the signal block.

Graceful fallback: if the target has no cached signals, substitute a
clearly-marked "no cached signals — collect from scratch per
Standard Methodology" message. Agent keeps working; no hard error.

**Implementation notes** (what actually landed vs. the original plan):

- Signal-composer extraction. `buildSignalsSummary` / `signalsSummary`
  were package-private in `internal/mcp/tools/analyze.go`; extracted
  to `internal/profile/summary.go` as exported `Summarize` +
  `SignalsSummary`. The MCP tool and the handoff renderer now share
  one composer. Prerequisite no other phase has to repeat.
- Graceful-degrade broadened. Any error in signal assembly —
  store open failure, resolve failure, entity-not-found, signal
  query error, no signals cached — returns the fallback marker
  rather than erroring. The signals block is an optimization, not
  a prerequisite; handoff render is the primary obligation. This
  meant no existing tests needed store-setup updates.
- Envelope shape: `{collected_for: "<canonical URI>", signals: {...}}`.
  The URI lets the agent flag if the caller-asked target and the
  cache URI diverge (diagnostic; unusual).

Tests shipped (5 passing):
- `InlinesCachedSignals` — seeded store → all 5 signal groups in rendered block.
- `FallbackWhenNoCache` — empty store → fallback marker.
- `SignalsBlockIsValidJSON` — fenced JSON parses back into the envelope type.
- `UnresolvableTargetFallsBackCleanly` — unknown entity → fallback.
- `Security_NoSignalsBlock` — security-role handoffs unaffected.

Phase 1 correctness is verified; token-delta measurement vs. baseline
is deferred per §Status above.

### Phase 2 — Repo hygiene files (shipped)

New collector `internal/signal/repofiles/` wired into
`cmd/signatory/collectors.go`'s `collectorsFor` alongside the github
and git collectors. Emits one compound signal of type `repo_files`
under `hygiene`; the handoff-body composer (from Phase 1) picks it
up automatically.

**Deviations from the original plan**, recorded so future phases
inherit the right defaults:

1. **No new CLI verb.** Original plan called for `signatory collect
   --target <T> --clone <path>` between `handoff security` and
   `handoff provenance`. For one collector, the scaffolding cost of a
   new verb (flag surface, entity resolution, store-write path, new
   test suite) was not justified — `collectorsFor` already dispatches
   against the clone path. The new verb remains a future option if
   collector count grows and a dedicated entry point is clearly
   needed; revisit no earlier than Phase 4.

2. **Family list expanded from 5 to 9.** The original list covered
   SECURITY, CODEOWNERS, .mailmap, CHANGELOG, CONTRIBUTING. Phase 2
   shipped adds **README, AUTHORS, MAINTAINERS, GOVERNANCE**. README
   is arguably the single most important hygiene file and the design
   doc's omission was an oversight; the other three are real hygiene
   cues from past analyses (especially MAINTAINERS in
   organizationally-backed projects). The two-phase detect/rank
   architecture made additions one-struct-literal cheap.

3. **Two-phase detection architecture.** Instead of an enumerated
   list of exact filenames (original approach), Phase 2 separates:

   - **Scanner** (permissive): each family declares a case-insensitive
     anchored regex. `stemWithExt(stem)` matches `<stem>` or
     `<stem>.<ext>` with single-extension tolerance (`[^.]+`, not
     `.+`, so backups like `README.md.bak` don't match). `exact(name)`
     matches the literal name — used by `.mailmap`. Host filesystem
     case-sensitivity (Linux sensitive, macOS default insensitive)
     cannot shift whether a signal is emitted.

   - **Evaluator** (opinionated): each family declares a `Preferred`
     list of canonical filenames in precedence order. Ranking walks
     Preferred (exact case → case-insensitive), first hit wins. When
     no Preferred entry matches, first scanner-sorted entry wins the
     canonical slot. Non-canonical matches surface in `alt_paths` for
     analyst review.

   This shape is intentionally generalizable: future phases that
   detect file presence (Phase 4 tool-pinning, per-ecosystem checks)
   can extract scanner + evaluator to a shared `filematcher` package
   if the second use case materializes. **We did not generalize yet.**
   Premature abstraction before a second use case produces the wrong
   shape; refinement after two real callers is the plan.

4. **Value shape per family:**
   ```json
   { "present": true, "path": "README.md", "alt_paths": ["README"] }
   ```
   `path` / `alt_paths` omitted when `present` is false or the
   relevant slot is empty. Map key is the family name — so the
   `Family` field on the `Result` struct is tagged `json:"-"` to
   avoid double-encoding.

5. **Filtering rules:**
   - Zero-byte files → reported absent. A placeholder stub is the
     cheapest form of fake hygiene; rejecting it raises the
     adversarial bar for a minimal cost.
   - Directories matching a family regex → rejected. Families are
     file-shaped; a `SECURITY/` sub-dir is not the disclosure doc.
   - Symlinks → resolved via `filepath.EvalSymlinks`; the recorded
     path is the resolved target, not the link. Symlinks whose
     target escapes the clone root are rejected (we refuse to emit
     absolute host paths into the signal store). Broken symlinks
     are absent by construction.
   - Multi-dot filenames → rejected by the single-extension regex.

6. **ForgeryResistance: `ForgeryLowDeclining`** (matches `license`).
   These files are trivially added; presence indicates hygiene-
   consciousness, not maintainer legitimacy. Zero-byte rejection and
   symlink-escape rejection are the small anti-forgery levers;
   strong forgery resistance is not claimed.

7. **Graceful partial failure.** Missing `.github/` or `docs/`
   sub-dirs are silently skipped — the common case for most repos is
   not an anomaly. Missing clone root surfaces as `ErrNoClone`
   (consistent with `git.ErrNoClone`) for fail-loudly orchestration.

**Test coverage** (all passing):

- `families_test.go` — detector regex matrix, declaration
  stability, immutability of `Families()` return.
- `scanner_test.go` — happy path across all 9 families, case
  variants, zero-byte rejection, directory rejection, multi-dot
  rejection, missing sub-dirs, symlink target resolution, broken
  symlinks, escape symlinks, sorted-output determinism.
- `evaluator_test.go` — absent-family shape, canonical ranking,
  case-insensitive fallback, non-Preferred fallthrough, multi-family
  iteration order preservation, sub-dir path handling.
- `collector_test.go` — compound signal shape, registry coupling
  (Group + ForgeryResistance), JSON `Family` elision,
  composer (`profile.Summarize`) round-trip into `hygiene.repo_files`,
  TTL lock-in, `signal.Collector` interface satisfaction.
- `internal/signal/types_test.go` — registry coupling test in the
  same form as git / github collectors.

### Phase 3 — CI pinning

New collector `internal/signal/cipinning/`. Parses `.github/workflows/*.yml`,
walks each job's `steps`, extracts `uses:` lines, classifies each as:

- SHA-pinned (40-char hex)
- tag-pinned (`@v5`, `@v1.2.3`)
- branch-pinned (`@main`, `@master`)
- unpinned (no `@`)

Emits signal group `hygiene.ci_pinning` with per-workflow breakdowns
and a per-action-reference summary (how many `actions/checkout` calls,
what shapes).

### Phase 4 — Tool reproducibility

New collector `internal/signal/toolpinning/`. File-presence-plus-contents
for:

- `bin/hermit` (+ `bin/.package/manifest.toml` if present)
- `.nvmrc`
- `.tool-versions` (asdf)
- `.python-version`
- `.ruby-version`
- `.go-version`
- `flake.nix` / `shell.nix`
- `Brewfile`

Emits `hygiene.tool_pinning` with presence + version strings where
parseable.

### Phase 5 — Identity cross-reference

Extend the existing `identity_domain_consistency` signal (or emit a
sibling signal under `governance`). Cross-reference:

- `owner_profile.email` (if public)
- `owner_profile.blog` domain
- `owner_profile.company` → declared organization
- `identity_domain_consistency.top_domains` (commit-email domains)

Emit: "declared identity (profile / company / blog) matches dominant
commit-email domain (Y/N), with specifics."

### Phase 6 — Orphan tag detection

Existing signal `publication.tag_signing_status` already distinguishes
annotated vs lightweight tags. Extend to cross-reference with the
release list:

- Tags with a GitHub release entry
- Tags without a GitHub release entry (orphan)
- Releases without a corresponding tag (rare, but diagnostic)

Emit under `publication.tag_releases` or extend `tag_signing_status`.

### Phase 7 — Registry ↔ source SHA match

Go first (easiest — proxy.golang.org exposes the commit hash for each
published version via `{module}/@v/{version}.info`). For each recent
tag under `publication.tags.recent`, fetch the proxy's view and compare
the commit it references to the git tag's target commit.

Emit under `publication.registry_match` with per-version SHA-match
results.

npm comes in a subsequent commit (integrity-field comparison against
tarball hash; more fiddly — separate landing).

### Phase 8 — GHSA advisories

New collector `internal/signal/ghsa/`. HTTP GET against
`https://api.github.com/repos/{owner}/{repo}/security-advisories` and
`https://api.github.com/advisories?affects={package-spec}`. Emits
advisories group with open/closed counts, severities, and summary
text per advisory.

### Phase 9 — Handoff rewrite

Rewrite `templates/handoffs/provenance-review-v1.md`:

- Remove "go investigate X" collection prose.
- Frame the task as: "Here's the full brief (inlined Layer-1 signal
  block). Apply the trust model to produce conclusions, positive
  absences, observations, and a posture proposal per the v1 schema.
  Follow up with Read/Glob/Grep only if a signal surprises you."
- Narrow allowed-tools documentation: Read/Glob/Grep are
  follow-up-only. WebFetch stays for edge cases signatory's
  collectors missed, not for routine re-derivation.
- Schema precision block retained (v1-schema gotchas from the
  2026-04-23 dogfood: `line_start`/`line_end`, `confidence` enum,
  `observations: {title, body}`, `positive_absences: {pattern_checked,
  description, confidence}`, `design_intent: bool`).

## Validation

After each phase:

1. `make install` (post-commit hook does this automatically).
2. Dogfood: `/analyze github.com/alecthomas/kong` (store has prior
   analyses, so re-collect).
3. Record: provenance agent `total_tokens`, `tool_uses`, number of
   conclusions, number of conclusions that are rederivations of
   cached signals vs novel.
4. Compare against baseline and the prior phase's measurement.
5. If the measured improvement is smaller than expected (or the
   agent still rederives that class of thing), stop and diagnose
   before starting the next phase.

Baseline (2026-04-23, kong, pre-Phase-1):

- total_tokens: 51,699
- tool_uses: 26
- wall-clock: 292s (~4.9 min)
- 8 findings, 5 rederivations (62.5%), 3 novel (37.5%)

Target at Phase 9 completion:

- Novel rate: 100% (no rederivations — all mechanical facts surfaced
  through signals, so any agent-produced "finding" is judgment or
  follow-up).
- Token / tool-use count: meaningful drop. Hard target TBD; setting
  one prematurely would game the metric. The qualitative test is:
  "can you read the agent's tool-use trace and identify any call
  that's routine collection rather than judgment or follow-up?"

## Out of scope

- **Security analyst role.** The collect-vs-judge imbalance may exist
  there too, but security work is largely code reading, and the likely
  right fix is per-language skills (Python's pickle, Go's unsafe,
  JavaScript's eval patterns) rather than more mechanical collectors.
  Separate planning effort.
- **Synthesist role.** Already uses the inline-evidence pattern (M6c);
  this plan adopts the same idiom for provenance.
- **Changing the agent-facing contract (v1 schema, MCP ingest shape).**
  Unchanged throughout this arc.
- **New signal categories outside `hygiene` / `publication` /
  `governance`.** Keep taxonomy churn minimal; extend existing groups.

## References

- `design/agent-facing-contract.md` — v1 schema, MCP ingest, analyst
  contract these signals feed.
- `design/tls-trust.md` — pipeline-service trust architecture
  (unrelated; for context on recent infra work).
- Dogfood transcript 2026-04-23 — kong analysis run that motivated
  this plan (tokens, tool_uses, finding breakdown in the commit
  message for `fff4e16`).
- `internal/signal/` — existing collector package structure that new
  collectors extend.
