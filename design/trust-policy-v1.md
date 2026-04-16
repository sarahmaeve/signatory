# Trust Policy — v0.2 Design Sketch

**Status**: Design sketch, not yet implemented. Written 2026-04-15 during
Phase 1 dogfood wrap-up, post Finding→Conclusion rename. Deliberately
short — captures the shape of the v0.2 trust-policy evaluator without
committing to implementation details. Expand when v0.2 work starts.

**Audience**: Sarah + one reviewer. Not a user-facing doc.

## Problem

Phase 1 gave us a read-only store of trust facts:

- **Layer 1: Signals** — mechanical observations (star counts, last commit,
  maintainer count, CI presence, signing status, ecosystem-specific markers).
- **Layer 2: Conclusions** — reasoned interpretations produced by analyst
  agents or humans ("this repo is single-maintainer-risk because commit
  activity concentrates on one account spanning 3 years with no co-committers").
- **Layer 3: Decisions** — the consumer's posture or burn, attached to an
  entity. Already present in the store; set manually today.

What's missing is the path from "we have facts" to "here's what we think
you should do." That's **trust policy** — the rules that turn facts into
recommendations.

## Why not just "run an analyst again"

An analyst agent produces conclusions. But conclusions aren't decisions,
and two different consumers with the same conclusions will reach different
decisions. A hackathon project and a Fortune 500 production team can
read identical conclusions about, say, mousetrap and reasonably land at
different postures. The mapping is **values-dependent**, not
analytically determined.

Trust policy is the place where an organization's values get encoded so
the same conclusions yield consistent decisions across that organization's
dependency tree.

## Design constraints

The v0.2 evaluator should honor these up-front or accumulate technical debt:

1. **Subjectivity is first-class.** Different orgs have different weights,
   thresholds, and risk tolerances. The policy must be authorable, not
   hardcoded.
2. **Overrides exist and must be respected.** Burns short-circuit everything.
   Explicit postures override computed recommendations. These aren't soft
   weights; they're hard precedence.
3. **Criticality multiplies, not adds.** A signal of concern matters more
   when the dependency is critical. Already a principle from
   `trust-model.md`; the evaluator must honor it structurally.
4. **Forgery-resistance gates weight.** A signal from a high-forgery-resistance
   source (cryptographic signature, institutional attestation) counts more
   than a signal that's easy to fake (self-reported star count). Also from
   `trust-model.md`.
5. **Supersessions replace weight, not add.** A round-2 conclusion that
   CORRECTS a round-1 conclusion should stand alone; the round-1 weight
   must not double-count. (This is why supersession is a first-class
   schema concept, not an afterthought.)
6. **Reasoning trace is required.** An output of "trusted-for-now" without
   the ability to ask "why?" is useless. The evaluator emits the trail
   of rule firings and weighted inputs that produced the recommendation.
7. **Author-able by non-programmers.** If policy edits require Go source
   changes, adoption stalls. TOML with comments is the likely envelope.
8. **Inspectable by the MCP surface.** Once wired, an LLM client must be
   able to ask "what would the policy say about X?" and "why did it say
   that?" without opening a terminal.

## Evaluator shape (sketch)

A layered evaluator that short-circuits on higher-precedence layers:

```
Layer 0 — Burns
  If entity.is_burned:
    return Decision{tier: rejected, reasoning: [burn record]}

Layer 1 — Explicit Posture
  If entity.posture is set:
    return Decision{tier: posture.tier, reasoning: [posture record]}

Layer 2 — Computed Score
  score = 0
  for each signal s attached to entity:
    if s.superseded_by exists: skip
    weight = policy.signals[s.type].weight
           * forgery_resistance_multiplier(s.forgery_resistance)
           * (s.criticality_multiplier if s.is_criticality else 1)
    score += weight * s.severity_numeric
  for each conclusion c attached to entity:
    if c.superseded_by exists: skip
    weight = policy.conclusions[c.category].weight
    score += weight * c.severity_numeric

Layer 3 — Threshold → Tier
  tier = policy.thresholds.lookup(score)
    (e.g., score >= 0.8 → vetted-frozen;
           score >= 0.4 → trusted-for-now;
           score >= 0.0 → unexamined;
           score <  0.0 → rejected)

return Decision{
  tier: tier,
  score: score,
  reasoning: [ordered list of rule firings with their weights]
}
```

All three "weights" are policy-configurable. The function is pure — same
facts + same policy = same decision. No time-dependence, no randomness,
no LLM inference in the evaluator itself. (LLM inference can produce
inputs; the evaluator consumes facts.)

## Policy file shape (straw-man)

`~/.signatory/policy.toml` or per-project `signatory.policy.toml`:

```toml
[thresholds]
vetted_frozen = 0.8
trusted_for_now = 0.4
unexamined = 0.0
# below unexamined → rejected

[signals.single_maintainer]
weight = -0.3
# negative weight: this is a concern, not a reassurance

[signals.institutional_backing]
weight = 0.4

[signals.commit_signing]
weight = 0.2

[conclusions.supply_chain_risk]
weight = -0.5  # any conclusion in this category strongly pulls down

[forgery_resistance_multipliers]
very_high = 1.0
high = 0.8
medium_declining = 0.5
low_declining = 0.2
# multiplier applied to the signal's base weight before aggregation

[criticality]
# criticality multiplier: a concern in a critical dep matters more
low = 1.0
medium = 1.3
high = 1.7
very_high = 2.2
```

The policy ships with defaults that reflect
`design/trust-model.md`'s baseline recommendations. Orgs override.

## Entity types (forward-looking)

Current schema supports `EntityProject` and `EntityPackage`. v0.2's
policy evaluator should be designed to handle:

- **Repo-as-entity** (existing)
- **Package-as-entity** (existing)
- **Maintainer-as-entity** (new) — lets you say *"this repo is trusted IF
  maintainer X is"*. Policy can traverse: decision(repo) depends on
  decision(maintainer).
- **PR-as-entity** (new) — proposes a change to a trusted entity. Its
  trust profile inherits from the base plus the PR-specific signals
  (contributor history, code review presence, signing of the PR commits).

Compositional trust (one entity's decision depends on another's) is a
key v0.2 capability. The evaluator should accept a dependency graph,
not just one entity in isolation.

## MCP surface for v0.2

Three new tools probably suffice:

- `signatory_compute_trust(target, policy_path?)` — run the evaluator
  against one entity, return `{tier, score, reasoning_trace}`.
- `signatory_explain(target)` — expand the reasoning trace from the
  most recent computation into prose suitable for an LLM summary to
  the user.
- `signatory_policy_preview(change)` — "if I changed the weight of
  single_maintainer from -0.3 to -0.5, which dependencies would change
  tier?" Lets users see policy edits' blast radius before committing.

And a new resource:

- `signatory://policy` — the currently active policy. Like
  `signatory://config`, readable so an LLM can reason about it.

## What's explicitly OUT of scope for v0.2

- **Policy as code (Rego/OPA).** The weighted-rules model above covers
  the 80% case with 10% of the complexity. Revisit in v0.5+ if the TOML
  format hits real expressiveness limits.
- **Automated policy tuning.** "Learn weights from observed decisions"
  is tempting and dangerous — it would optimize for consistency with
  past decisions, not correctness. Human authorship only.
- **Cross-org policy sharing.** An interesting future direction (a
  community-maintained policy bundle for "defensive production
  deployments") but it's a governance question, not a v0.2 one.
- **Network-scale trust graphs.** Chaining "I trust X because they
  trust Y because Y's org trusts Z" is appealing but opens
  transitive-compromise concerns. Keep trust local to the
  consumer's policy for v0.2.

## Open questions

1. **Should signals and conclusions weight independently, or feed into
   a single aggregator?** Probably independently — signals carry
   different epistemic weight than conclusions, and the user should be
   able to say "weigh analyst conclusions heavily but discount raw
   signal counts." Two aggregation passes.

2. **How does the evaluator handle entities with no signals or
   conclusions?** Return `unknown-provenance` with score 0; don't guess.
   Analogous to how `signatory_analyze` returns NotFound rather than
   fabricating.

3. **Should policy files be signed?** Probably yes — a policy file is
   a security artifact for the consumer; tampering with it silently
   changes trust decisions. Signing deferred to v0.3 but worth naming
   as a forward concern.

4. **How does posture SUPERSEDE interact with policy?** An explicit
   posture set 6 months ago may be stale. Should policy have a "decay"
   concept where old decisions expire? Probably yes, but simplest
   implementation is "all postures are permanent unless explicitly
   overridden by the user" — warn, don't auto-expire.

## Next steps (when v0.2 work starts)

1. Prototype the evaluator as a pure function in `internal/policy/` —
   unit-testable against a handful of scenarios from `design/dogfood/`.
2. Author a default policy TOML that reproduces the posture decisions
   already recorded (signatory: trusted-for-now, mousetrap: rejected,
   etc.). If the defaults don't produce those decisions, either the
   defaults are wrong or the past decisions were off-model — either
   way, valuable learning.
3. Wire the CLI first (`signatory trust-compute <target>`), then the
   MCP tool. Separate landings; easier to review.
4. Add maintainer-as-entity and PR-as-entity schema extensions in a
   parallel track, then teach the evaluator to traverse the graph.
