# Analyst Composability — Independently Invocable Analysts

**Status:** Scope sketch recorded 2026-04-22 from a dogfood-
surfaced question ("should we have a 'skim' version of /analyze
that works differently?"). Not yet a design decision. Records the
thinking so it doesn't drift before the follow-up conversation in
the coming days. **Pre-implementation; no code changes proposed
here.**

**Scope:** CLI and MCP surface for invoking signatory's security
and provenance analysts independently, with a scope-tagged posture
output and a compositional caching model. Out of scope: the
light-analysis economics rationale (already in
[`analysis-economics.md`](analysis-economics.md)) and the broader
v0.2 deployment shape (hosted / org-wide, in [`ROADMAP.md`](ROADMAP.md)).

Cross-references:
- [`analysis-economics.md`](analysis-economics.md) §7 for the
  market reasoning: for a lot of npm micropackages, full analysis
  is economically irrational; provenance-only is often the right
  shape.
- `internal/exchange/types.go` — the synthesis supplement storage
  shape this sketch would extend (`synthesis_supplement.scope` is
  the field introduced here).

## 1. Motivation

Signatory v0.1's `/analyze` skill dispatches two analysts in
parallel (security reads source code; provenance reads
registry/git/identity signals), then a synthesist integrates
their outputs into a trust posture. Under the economics analysis
(2026-04-22), two observations coincide:

1. **Most npm micropackages can't justify full analysis.** The
   fixed-cost floor of ~200k tokens exceeds the cost of rewriting
   a 50-LOC utility by 20-40×. Full analysis is economically
   rational only above ~1,000-1,500 LOC.
2. **The two analysts answer different questions.** Provenance
   asks "is the supply chain healthy?" (publish chain, maintainer
   identity, CI hygiene, package shape). Security asks "is the
   code behaviorally sound?" (injection, exec, IPC, etc.). These
   are not the same question, and many adoption decisions genuinely
   only need one.

The @sindresorhus/is dogfood on 2026-04-22 made this concrete:
the synthesist's load-bearing findings for that package were
provenance signals (unsigned commits, laptop-rooted publish,
no OIDC, zero-dep install-time hygiene). The code-level findings
were real but catalogued documented language-constraint side
effects, not exploitable bugs. If the user had only needed the
supply-chain question answered, they could have paid ~60-100k
tokens instead of ~190k.

**Dogfood-surfaced insight** (synthesist's own notes,
@sindresorhus/is): "a per-publisher trust profile would let
signatory amortize this kind of analysis across the family." That
amortization requires independently invocable analysts to work.

## 2. The core proposal

Expose signatory's two analysts as independently invocable CLI
verbs:

```
signatory analyze <target>      # full: both analysts + synthesis
signatory provenance <target>   # provenance analyst only + scope-tagged synthesis
signatory security <target>     # security analyst only + scope-tagged synthesis
```

Each verb answers a distinct question:

| Verb | Question |
|---|---|
| `analyze` | "Is it safe to adopt in production?" |
| `provenance` | "Is the supply chain healthy?" |
| `security` | "Does the code have defensible behavior?" |

`provenance` and `security` are not reductions of `analyze` — they
are complete answers to narrower questions. The output of
`provenance` stands on its own; a user reading it later should
not feel they are reading a deficient analyze.

## 3. Scope-tagged posture

The `SynthesisSupplement.ProposedPosture` gains a `scope` field
(new enum):

- `scope: "full"` — produced by `signatory analyze`. Synthesis
  integrates both analysts. Any tier is admissible.
- `scope: "provenance-only"` — produced by `signatory provenance`.
  Synthesis reasons from provenance evidence alone.
  `vetted-frozen` is **not** admissible at this scope; other
  tiers (`trusted-for-now`, `rejected`, `unknown-provenance`,
  `unexamined`) are.
- `scope: "security-only"` — produced by `signatory security`.
  Same restriction: `vetted-frozen` is not admissible without
  provenance evidence.

The scope value lives on the synthesis output and flows into
`posture accept`: the accepted posture row carries the scope tag
in its audit detail (`accepted_scope: "provenance-only"`). Future
readers can see whether a posture was based on full evidence or
only one side.

**Why disallow `vetted-frozen` on narrow scopes:** the tier
vocabulary in [`trust-policy-v1.md`](trust-policy-v1.md) treats
`vetted-frozen` as the strongest endorsement. That strength
requires evidence across both behavioral and provenance axes — a
supply chain can look clean while the code has exploitable bugs,
and vice versa. Scope-tagged `vetted-frozen` would be a footgun.

**Posture supersession:** a `scope: "full"` posture supersedes
any narrower-scope posture on the same target. The reverse is
not allowed: a `provenance-only` posture cannot supersede a
`full` one. M4's soft-delete semantics extend naturally — the
narrower posture gets withdrawn with reason
`superseded-by-full-analysis`.

## 4. Compositional caching

Every analyst output lands in the store independently. A future
`analyze` dispatch checks what evidence already exists:

- If both analysts have recent outputs: skip re-dispatch, run only
  the synthesist.
- If one analyst has a recent output: dispatch only the missing
  one, then synthesize.
- If neither: dispatch both, then synthesize.

**Staleness heuristic:** "recent" is tunable. A first-pass default
could be 30 days for provenance (registry/CI state changes slowly)
and 7 days for security (source changes on every release). Both
configurable per-target via `--max-age` or a signal-TTL policy.

**Per-analyst invocation caches too:**
- `signatory provenance X` checks for a recent provenance output
  under X. If present, skips dispatch and re-runs synthesis only.
  If absent, dispatches the provenance analyst.
- Same for `signatory security X`.
- `signatory analyze X` is the composition of both.

**User workflow enabled by this:**

```bash
# Skim 30 npm deps for supply-chain health.
for dep in $deps; do
    signatory provenance $dep
done

# 4 flagged concerns. Escalate those to full analysis.
for dep in $flagged; do
    signatory analyze $dep   # reuses provenance output, adds security
done
```

Neither pass re-pays the provenance tokens on the escalated
targets.

## 5. Why separate verbs, not flags

Considered and rejected: `signatory analyze --provenance-only X`.
Three reasons:

1. **A flag implies reduction; separate verbs imply different
   questions.** `--provenance-only` reads as "do less analyze."
   Separate verbs read as "answer a narrower question completely."
   The latter matches what's actually happening.
2. **Compositional caching flows naturally.** With separate verbs,
   `provenance X` and later `analyze X` have clear,
   non-overlapping semantics. With a flag, the mental model for
   flag-combinations-over-time is uglier.
3. **Output shape differs.** A provenance-only synthesis output is
   a complete artifact with its own scope tag. A flag-filtered
   analyze output invites callers to treat it as a partial analyze
   (missing fields), which it isn't.

## 6. Ecosystem fit

**npm micropackages (< 500 LOC, utility tail):** `provenance` is
often the right verb outright. The 30-LOC utility category rarely
has exploitable code-level behavior; the trust question is
supply-chain. Pay ~60-100k tokens, get a decision.

**npm larger packages / scoped SDKs / frameworks:** start with
`provenance` as a cheap first pass. Escalate to `analyze` if
provenance flags concerns OR if the code surface is large enough
to warrant behavioral review regardless.

**npm high-fanout packages (extensive direct and transitive
deps):** composition is itself the skim signal. A package with
extensive dependencies is expensive to verify, expensive to repair
(forking or replacing each transitively-trusted publisher is
generally infeasible), and expensive to re-vet on updates.
`analyze` on the root implicitly claims to cover the composition
but only covers 1/N of the actual trust surface — that's the wrong
question, not an expensive-but-correct one. The honest output
shape is `provenance` on the root, paired with mechanistic
composition signals (`direct_dep_count` now, `transitive_dep_count`
later).

**Signatory's contract for composition signals is to surface the
shape — count, OIDC attestation distribution across the resolved
graph, install-script surface, maintainer-age distribution — and
not to produce a verdict on whether the surface is acceptable.**
That is org-policy, not a signatory decision. Anything stronger is
signatory pretending to know a risk appetite it wasn't given.

**Go modules:** full `analyze` is usually the right default.
Sizes rarely fall below the economics crossover, and Go's
culture is anti-microdep, so the cheap-skim-for-tiny-utilities
case is rarer.

**Python:** bimodal. Utilities (< 500 LOC) lean provenance-only;
frameworks lean full analyze.

**Rust (when ecosystem provider lands):** TBD. Expected to
resemble npm's mixed pattern.

## 7. Open questions

These are named but not decided. Worth revisiting before
implementation.

### OQ1: Does provenance/security invoke the synthesist?

Option A: Yes, always. Synthesist runs with a scope-aware handoff
that says "reason from provenance evidence alone; declare the
posture's scope explicitly." Output is a full `AnalystOutput` with
`synthesis_supplement.scope = "provenance-only"`.

Option B: Only on demand. `signatory provenance X` produces raw
analyst output; `signatory provenance X --synthesize` adds the
scope-tagged tier. Caller pays for synthesis only when they want
a posture recommendation.

Option C: Never for scoped runs. Narrow runs are evidence-only;
only `signatory analyze` can produce a scope-tagged synthesis
(effectively: scope-tagged tiers are only `full` or absent).
Would simplify the tier vocabulary at the cost of making
`provenance` less useful.

Lean: Option B. Synthesis is valuable but not always needed — a
user running `provenance` during a quick triage pass may only
want the raw evidence for manual review. An explicit `--synthesize`
(or `--tier`) opt-in keeps costs honest.

### OQ2: Scope tag as enum or free-form string?

An enum locks the vocabulary (`full`, `provenance-only`,
`security-only`) but makes future scopes (e.g., `provenance-
limited` for targets where provenance evidence was partial)
require migration. A free-form string is flexible but opens the
door to typos and drift.

Lean: enum with a strict validator, extensible via schema
migration. Follows the same pattern as `PostureTier`.

### OQ3: Storage — per-scope posture rows?

A single target could in principle accumulate multiple postures
over time: a `provenance-only: trusted-for-now`, later superseded
by a `full: rejected`. The current posture-table schema supports
this via the existing append-only model, but consumers querying
"what's the current posture?" need rules for which scope wins.

Lean: full supersedes narrower. Narrower scopes coexist as
historical records but don't satisfy "current posture" queries
unless nothing fuller exists. The `posture get` command can show
both if `--all` is passed (analogous to per-version display
today).

### OQ4: Should `security-only` exist at all?

Provenance-only has clear v0.1-market relevance (npm
micropackages). Security-only is harder to motivate: if you have
the source and want to reason about it, you could just read it or
use a traditional SAST tool; signatory's value-add is the
combined trust model including provenance.

Lean: defer `security-only` to a later version. Ship `analyze`
and `provenance` first. Add `security` only if dogfood surfaces
a real use case for it.

### OQ5: `/analyze` skill equivalents?

The CLI verbs are the primary surface. The `/analyze` Claude Code
skill would correspondingly gain `/provenance` as a sibling skill
(with its own SKILL.md), sharing the orchestration infrastructure
but dispatching only the provenance analyst. Or: extend `/analyze`
itself with a skill-level flag.

Lean: separate skill for discoverability. `/provenance <target>`
is more memorable than `/analyze <target> --scope provenance`.

### OQ6: MCP tool surface?

Currently `signatory_ingest_analysis` accepts any analyst role.
For scope-tagged ingestion, no MCP change is required — the
synthesist's supplement carries the scope field; the store
records it unchanged. No new MCP tool needed unless we want a
`signatory_provenance_skim` read tool as a sibling to
`signatory_summary`.

Lean: no new MCP write tools. Optionally a read tool if the UX
calls for it.

## 8. Decision threshold

**Not enough data to ship this yet.** Two dogfood targets showed
the economics; one made the composability insight explicit. Before
implementation, I'd want:

- **3-5 more dogfood runs** showing the pattern — specifically,
  targets where a provenance-only pass would have been the right
  call, plus targets where full analyze was actually needed
  because of code-level concerns. This calibrates the real
  recommendation ratio.
- **One "escalation" dogfood** — start with `provenance`, hit a
  concerning signal, escalate to `analyze` that reuses the
  provenance output. Proves the compositional caching story in
  practice.
- **Naming validation** — confirm with additional users that
  `provenance` reads as "provenance assessment" and not as
  something else (e.g., a chain-of-custody assertion).

Estimated implementation cost (for when we get there): ~400-600
LOC production + ~500-700 LOC tests. Smaller than M6 because the
infrastructure (analyst dispatch, synthesis supplement, storage,
posture accept) already exists. The work is mostly: new verbs,
scope-field schema migration, staleness heuristics for
compositional caching, and the synthesist handoff variants.

## 9. What this sketch does not commit to

- **A ship-date or milestone number.** Named as a v0.1.x or v0.2
  possibility depending on when more dogfood data arrives.
- **The exact scope-tag enum values.** `provenance-only` /
  `security-only` / `full` is the starting vocabulary; naming
  bikeshed is legitimate and deferred.
- **Staleness TTL defaults.** "30 days / 7 days" is a first guess.
  Real values land when we have usage patterns.
- **The `/analyze` skill's fate.** Separate `/provenance` skill
  vs. extending `/analyze` is deferred per OQ5.
- **OSS-community signalling.** If the verb names conflict with
  established security-tooling conventions (e.g., `provenance`
  has SLSA-adjacent meaning), the naming becomes part of the
  decision rather than a detail. Worth an ecosystem-naming pass
  before implementation.

## 10. Summary for the follow-up conversation

- The idea: expose signatory's two analysts as independently
  invocable CLI verbs, with a scope-tagged posture that can't
  overreach.
- The shape: `analyze` / `provenance` / `security`, with
  compositional caching so evidence from one verb feeds later
  invocations of others.
- The motivation: economics says full analysis is often overkill;
  two data points plus the synthesist's own per-publisher
  amortization observation point toward a lighter path that's
  genuinely different, not diluted.
- The risk: shipping this prematurely could bifurcate signatory's
  UX and fragment the posture vocabulary. Decision threshold is
  3-5 more dogfood targets + an escalation test + a naming pass.

Not a v0.1-blocker. Not urgent. Worth re-examining once a few more
real analyses are in the store.
