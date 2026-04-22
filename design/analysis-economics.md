# Analysis Economics — When is a signatory run worth the tokens?

**Status:** Draft recorded 2026-04-22 from dogfood data (three
targets, updated 2026-04-22 with @sindresorhus/is). Not a design
decision — a market/usage observation. Answers the question "at
what target size does signatory analysis start to be cheaper than
rewriting the target?" and what that means for where signatory is
economically rational.

**Scope:** Token-cost economics of `/analyze` against real targets,
circa Claude Opus 4.7 pricing. Out of scope: Claude API pricing
math, Max-subscription amortization, multi-user org amortization
(those are v0.2+ deployment-shape concerns).

Cross-references:
- [`vision.md`](vision.md) and [`trust-model.md`](trust-model.md) for
  what signatory is trying to produce.
- [`v0.1-invariants.md`](v0.1-invariants.md) Invariant 1 (no direct
  API) and Invariant 2 (mechanical work in Go) for the shape that
  fixes the analyst-side token cost.
- `sync/KAIZEN.md` 2026-04-22 entry for per-run token-cost data
  (gitignored; local log).

## 1. Observed data

Three `/analyze` runs, recorded in the KAIZEN log:

| Target | LOC (roughly) | Tokens | Think level |
|---|---|---|---|
| pkg:npm/postcss-place@11.0.0 | ~55 (logic) | ~203k | high |
| pkg:npm/@sindresorhus/is | ~500-1,000 | ~190k | high |
| pkg:npm/@stripe/stripe-react-native | ~50,000-100,000 | ~360k (~300k excluding a 60k re-dispatch) | x-high |

**Key observation:** analysis token cost is roughly **bounded, not
linear** in target size. 1000× the code produces ~1.5-2× the tokens.

Why: analysts sample strategically and grep for patterns rather than
reading every line. Most of the token budget is spent on the fixed
overhead of handoff ingestion, schema conformance, and cross-
analyst reasoning. The code volume contributes modestly to
provenance's registry/CI survey and to security's target-area
sampling, but not proportionally.

**Floor:** ~190-210k tokens per target at high think level, across
tiny (~55 LOC) and small (~500-1000 LOC) targets. Confirms the
floor is a **pipeline fixed cost**, not think-level-specific — the
high-think @sindresorhus/is run landed in the same range as the
high-think postcss-place run.

**Think-level effect:** x-high buys reasoning depth that pays off
on larger/harder targets. The tiny and small high-think runs
produced synthesis quality comparable to the large x-high run
(multi-paragraph tier justification, forgery-resistance weighting,
calibration hedges) — suggesting x-high matters most when there's
genuine disambiguation work (contradictions, complex trade-offs,
competing signals). For targets with clean evidence and no
contradictions, high think is sufficient and cheaper.

**Caveat:** three data points, all npm. A Go module or Python
package run would test whether the floor is npm-pipeline-specific
or genuinely universal.

## 2. Cost model: rewrite scales linearly

For an LLM to rewrite a package, cost scales with size:

- Reading + writing code: ~40-80 tokens per LOC (formatting- and
  language-dependent).
- Reasoning / planning overhead: ~2-3× the raw read/write.
- Context for tests, types, integration points: additional ~20-40
  tokens per LOC.

**Total: ~100-250 tokens per LOC** for a reasonably-tested rewrite.

## 3. The crossover

```
target LOC     rewrite cost      analysis cost      winner
─────────────  ────────────────  ─────────────      ────────
    55         ~10,000 tokens    ~200,000 tokens    rewrite is 20× cheaper
   500         ~75,000           ~200,000           rewrite is 2-3× cheaper
 1,500         ~225,000          ~250,000           roughly equal
 5,000         ~750,000          ~300,000           analysis is 2-3× cheaper
50,000         ~7,500,000        ~350,000           analysis is 20× cheaper
```

**Break-even is around 1,000-1,500 LOC.** Below that, rewriting is
token-cheaper. Above that, analyzing is token-cheaper, and the gap
widens quickly.

## 4. What pure-token math misses

Token cost is necessary but not sufficient. Real adoption decisions
weight additional dimensions:

### Analysis buys provenance signals rewriting can't produce

Maintainer identity, publish chain integrity, bus factor, CI
hygiene, organizational control — none of these live in the source.
For ecosystems with takeover history (npm especially), those
signals are often more load-bearing than the code audit itself.

The Stripe dogfood (§1) made this explicit: the medium-severity
finding that dominated the tier decision was **provenance F004**
(laptop-rooted publish chain, no OIDC trusted publishing), not any
code-level issue. A rewrite that eliminated the code-level findings
would gain nothing on the provenance side — the consumer still
doesn't know who published the version they adopted. Rewriting
replaces upstream trust with downstream ownership, which is a
different risk surface, not zero risk.

### Rewrite transfers ongoing maintenance

- **Analysis**: pay once, re-vet at version bumps (cheap re-runs),
  inherit upstream evolution including bug fixes and security
  patches.
- **Rewrite**: pay once, own the code forever. Every bug the
  upstream fixes has to be re-discovered or ported. Every feature
  added upstream is either missing downstream or ported at cost.

For a tiny utility that doesn't evolve (think `left-pad`), the
"forever" cost is ~zero. For anything under active development,
it's significant and usually dwarfs the original rewrite cost
within 6-12 months.

### Amortization across projects

Analyzing a 500-LOC dep consumed by 30 projects across an org
amortizes the tokens 30×. Rewriting distributes to N owners.
Signatory's v0.1 model is single-user local, so this multiplier
isn't live yet — but a hosted/org-wide model (v0.2+, see
[`ROADMAP.md`](ROADMAP.md)) changes the per-project math
dramatically.

### Risk asymmetry

- Analysis cost is spent whether the answer is good or bad. The
  tokens bought the decision, not the outcome.
- Rewrite cost is spent regardless, and you own the result either
  way. If the rewrite has bugs, you carry the liability. Upstream
  bugs are shared.

## 5. Ecosystem patterns diverge sharply

### npm

Lots of tiny (< 100 LOC) packages in the dependency tree. Many of
these are literally below the token-break-even threshold for
analysis. `left-pad`, `is-odd`, `ms`, `one-liner-utility-of-the-week`
are 20-80 LOC. The supply-chain-risk reputation of npm lives here:
small packages with enormous fanout, minimal individual oversight,
and frequent maintainer churn or takeover attempts.

**For npm, a two-tier analysis makes sense**: full analysis for
anything > ~500 LOC or with a published API surface; provenance-only
(or decline-and-replace) for the micropackage tail.

### Go

Culture is anti-microdep. Even "small" Go packages are 500-2000
LOC; standard-library-style conventions discourage single-function
packages. Analysis wins more often in the per-target economics.

### Python

Bimodal: tiny (`six`, `pytz`) vs. large frameworks (`requests`,
`sqlalchemy`). Mixed outcomes. `requests` (~5k LOC) is well above
break-even; `six` (~900 LOC) is near it.

### Rust (when ecosystem provider lands)

Probably closer to npm's pattern: crates.io has both significant
crates and micro-utilities. TBD until dogfood.

## 6. Practical guideline

| Target shape | Default recommendation |
|---|---|
| npm package < 200 LOC, single-purpose utility | **Rewrite or inline.** Analysis rarely worth 200k tokens. |
| npm package 200-1000 LOC | Analyze if already vetting ecosystem neighbors; otherwise inline. |
| npm package > 1000 LOC, or any published API surface | **Analyze.** |
| Any Go module | **Analyze** — sizes rarely fall below break-even. |
| Python utility < 500 LOC | Usually rewrite/inline. |
| Python framework/library > 2000 LOC | **Analyze.** |

These are defaults. Override upward (analyze more aggressively)
when:
- The target has a history of takeover attempts or maintainer
  instability.
- Adoption is in a security-critical context (CI runners, prod,
  anything that touches credentials).
- The dependency is consumed by code you ship to customers (as
  opposed to dev tooling).

Override downward (rewrite-or-inline more aggressively) when:
- The target is a single-purpose utility with clear behavior.
- You control the consuming codebase and can afford the maintenance.
- The ecosystem has high churn and adoption-via-analysis would need
  to be re-run frequently.

## 7. The uncomfortable conclusion (and why it doesn't undermine signatory)

For a lot of npm — maybe the majority of micropackages by count,
though not by fanout — **signatory's full analysis is economically
irrational at current model prices**. A code-level review of a
30-LOC `is-odd`-shape package is ~20-40× the cost of rewriting it.

This is not a criticism of signatory. It's a market observation
about **what's worth vetting**. Signatory is optimized for targets
where:
- The decision is high-stakes (you're going to ship this in prod).
- The maintainer's identity matters (provenance dominates the
  trust decision).
- The publish chain is the attack surface.
- The code is too large to audit by hand or replace.

For those targets, 200-400k tokens is cheap relative to the
blast radius of a bad adoption decision. For a 30-LOC utility that
turns booleans into strings, it's not cheap, and the market will
notice.

**The implication for scope:** signatory's target user is not "every
`npm install` line" — it's the set of dependencies where the
decision matters enough to pay the analysis cost. Solo devs and
small teams vetting ~10-100 critical deps per project. Enterprise
security teams vetting dependencies that enter production. Not
per-line npm automation.

### Signatory is a decision-moment tool, not a scanner

Stated positively rather than by negation: signatory is invoked
when a human-plus-LLM pair has a *specific* adoption or re-vet
decision to make, and is willing to spend minutes and dollars of
tokens to inform it. The load-bearing trait of the target user is
not "senior engineer" or "security team" — it's **has a decision
to make right now**.

The solo-dev case makes the economics sharp. In v0.1's
single-user shape (see [`v0.1-invariants.md`](v0.1-invariants.md)
Invariant 1), the tokens come straight out of the operator's Max
quota or API spend. Per-install scanner-style use is not just
discouraged — at ~200k tokens per target, it is uneconomic. One
person running `/analyze` against every row of a `package-lock.json`
is burning weeks of budget to learn things mostly below the
economic break-even (§3). The single-user shape forces decision-
moment use; shared/org deployment (v0.2+) changes the math but
doesn't reintroduce "scan everything" — it just lets multiple
consumers amortize one decision.

Contexts where invocation is economically rational:

1. **Pre-adoption gate** — "should I add this dep?" The canonical
   case and the biggest value: provenance findings (e.g. Stripe
   F004) that a PR review would miss.
2. **Version-bump re-vet** — Dependabot opened a PR, a major
   jumped, or a maintainer changed. Cheaper on the second run if
   the per-publisher cache (§8) lands.
3. **Incident-adjacent triage** — "X got flagged in advisory feed
   Y, do we use it and does it matter?" Highest urgency, least
   forgiving of pipeline slowness.
4. **Shopping between alternatives** — choosing among N candidate
   libraries. Here the economics bite: analyzing 3 candidates at
   200k each can exceed just picking one and rewriting.
5. **Backlog audit** — "we inherited this repo, what's on the
   trust budget?" Batch-shaped, but the batch is human-curated
   ("these 10 deps I'm worried about"), not dep-tree-wide.

What signatory is *not* designed for, and what the economics
actively reject:

- `signatory scan-everything` over a lockfile or `go.mod` closure.
- A CI action that gates PRs on signatory output for every
  changed dep. (Also an antipattern for a second reason — see
  [`ANTIPATTERNS.md`](ANTIPATTERNS.md) §"Usage antipatterns".)
- Scheduled re-analysis without a user-visible decision moment
  on the other side. (Also an Invariant-1 non-goal.)
- "Run signatory on the top 1000 npm packages so users can look
  them up." The hosted-aggregator shape signatory explicitly
  refuses.

## 8. Future work

Named as possibilities, not commitments. All post-v0.1.

### Light-analysis tier

A provenance-only analysis path that skips the code-level review:
publish-chain signals, maintainer identity, CI hygiene, install-time
hooks, ecosystem metadata. Would land in ~50-100k tokens and would
be economically sensible for small utilities where the code is
trivial but the provenance signals still matter (scope takeover,
maintainer account compromise, etc.).

Technical shape: a new `handoff provenance-only` role that skips
the synthesis step, or a new `/analyze --quick` flag that runs only
the provenance half of the pipeline.

### Size-aware dispatch

Before dispatching full analysts, signatory could survey the target
(registry metadata + tarball size) and route:
- < ~300 LOC: skip analysis, return a "below economical threshold"
  recommendation pointing to inline/rewrite.
- 300-1000 LOC: dispatch provenance-only.
- > 1000 LOC: full analysis.

Would require the CLI to know target size before dispatching, which
means a pre-fetch step. Moderately invasive.

### Per-analyst cost caps

Instead of running x-high think across the board, budget per
analyst based on target size. A 50-LOC target might get 20k tokens
for security and 80k for provenance (since provenance's work is
mostly size-independent). Would require more telemetry than we
currently capture (see the KAIZEN entry on token-cost recording
being suspiciously absent from the store).

### Amortization model

Multi-user / org-wide deployment (v0.2+) lets one analysis serve
N projects. The economics shift dramatically — a $0.50 analysis
amortized across 50 projects is $0.01/project, at which point even
small utilities become worth analyzing. Named in
[`ROADMAP.md`](ROADMAP.md) V0.2 — worth returning to this doc when
those amortization assumptions become concrete.

### Per-publisher trust profile

Surfaced by the @sindresorhus/is synthesist on 2026-04-22: a lot
of the provenance evidence for one sindresorhus package
(maintainer identity, npm publish chain shape, commit-signing
distribution, CI pinning patterns) would be structurally identical
for every other sindresorhus package. Redoing the provenance
assessment per-package re-pays the same tokens across a family.

A per-publisher cache — "what do we know about this npm
maintainer's publish posture?" — would let the provenance analyst
short-circuit the shared signals and focus on package-specific
evidence. Most applicable to the long tail of single-maintainer
utility packages that cluster under the same npm account
(sindresorhus, substack, feross, etc.); less applicable to
corporate-maintained packages where the publisher is an org with
wide membership.

Shape (sketch): a new `publishers` table keyed on (ecosystem,
maintainer_id) carrying provenance snapshot + timestamp. The
provenance analyst's handoff pre-loads the current snapshot as
context; the analyst's output updates it. Amortization then
applies within a maintainer-namespace in addition to across
projects (§Amortization model above).

Not scoped for v0.1. Would want more dogfood data (5-10 packages
from the same maintainer) before committing to the abstraction.

## 9. Data gaps worth closing

Would sharpen this analysis with:
- **Third data point: a Go module run.** Is the token-cost floor
  npm-specific or pipeline-specific?
- **Third data point: a Python module run.** Same question.
- **x-high vs high on the same target.** Isolates effort-level
  variance from target-complexity variance.
- **A tiny-target x-high run.** Tells us whether the ~200k floor
  is the true fixed cost or whether think-level nudges it.
- **Token-cost persisted in the store.** Currently only visible via
  Claude Code's transcript; a store-side record would let us
  graph cost-per-analysis over time and detect pipeline regressions
  or model-upgrade wins automatically.
