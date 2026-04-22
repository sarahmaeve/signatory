# 2026-04-22: Mozilla × Anthropic — Mythos Preview in Firefox 150

## Source

Mozilla blog, "The Zero-Days Are Numbered" (AI & security), published
2026-04-22:
<https://blog.mozilla.org/en/privacy-security/ai-security-zero-day-vulnerabilities/>

This entry is a precis of a co-marketing post from the Mozilla Security
team describing an ongoing Mozilla × Anthropic collaboration in which
Claude models have been used to audit the Firefox codebase. The facts
below are drawn from the post as fetched on 2026-04-22.

## Why this entry exists

This is the first *operational-deployment* datapoint for Mythos-class
capability applied to a real production codebase at scale. The 04-14 and
04-17 entries deferred the next temporal-era review to "when Mythos is
released." Mozilla shipping Firefox 150 with 271 fixes identified by
Claude Mythos Preview is the strongest evidence yet that the deferral's
trigger condition is approaching — but via partner distribution, not GA.
This entry records the evidence, preserves the deferral for one more
cycle, and captures two new observations the post uniquely supports.

## What was announced

1. **Firefox 148** — Anthropic × Mozilla collaboration beginning
   February 2026 using **Claude Opus 4.6** yielded **22 security fixes**.
2. **Firefox 150** (released the week of 2026-04-22) — **Claude Mythos
   Preview** identified **271 vulnerabilities**, all fixed in the
   release.
3. **Stated capability claim**: Mythos Preview finds vulnerabilities at
   parity with elite human researchers ("no category or complexity of
   vulnerability that humans can find that this model can't"), but finds
   **no new vulnerability classes** ("we also haven't seen any bugs that
   couldn't have been found by an elite human researcher").
4. **Stated limit**: Mozilla explicitly rejects the "rewrite everything"
   posture — *"we still can't afford to stop everything to rewrite
   decades of C++ code, especially since Rust only mitigates certain
   (very common) classes of vulnerabilities."*
5. **Framing**: defensive-optimistic throughout ("defenders finally have
   a chance to win, decisively"). No discussion of the same capability
   in adversarial hands.

## What this reinforces

### Era-boundary deferral holds — for one more cycle

Mythos *Preview* deployed via partnership is a horizontal-expansion
event under the 04-17 rubric (capability deployed through curated
partner channels, not broadly). The 04-14 deferral specified "Mythos
public release" as the era-review anchor; Mozilla's use is partner
access with named distribution, not GA.

However, the operational scale (271 production fixes in a shipped
browser) means the era trigger is now closer than the 04-14 or 04-17
entries could resolve. **Recommended posture: one more deferral, with
the explicit condition that the next Mythos signal — public/GA release,
a second partner-scale deployment on a distinct codebase, or a third
capability delta against the same codebase — promotes to an era review
without further deferral.**

### Vertical expansion at production scale

22 → 271 on the same codebase between Opus 4.6 and Mythos Preview is a
**~12× delta**. This is the first public production-scale datapoint for
the 04-17 concern that "incremental improvement" no longer carries
annual-cadence reassurance. The delta is observed on real code, not a
benchmark. The capability-profile assumption behind signals designed
against 4.6-class tooling is now demonstrably stale at the upper bound.

### Vendor-communication discipline (04-17) applies in full

The Mozilla post is a co-marketing artifact from two vendors'
communications functions. Parse it adversarially:

- *"Defenders finally have a chance to win, decisively"* is a
  **reassurance framing** calibrated for a press audience.
- *"271 bugs found"* is a **competitive framing** — headline number
  without denominator (how many bugs existed in the scanned surface?),
  without baseline (what would a cyber-permissive fine-tune find
  against the same codebase?), and without adversary-simulation
  controls.
- *"No new vulnerability classes"* is a **scoping claim** that is
  convenient for both parties: Mozilla can say its historical security
  posture was not missing a capability category; Anthropic can say its
  model isn't discovering offensive primitives that didn't previously
  exist. Both are plausible; neither is independently verifiable from
  the post.

Record the facts (22, 271, Firefox 148/150, Feb 2026 start, Mythos
Preview as the current model). Do not ingest the narrative framing.

### Forgery-resistance hierarchy unchanged

"No new vulnerability classes" is consistent with the 04-14 reading: the
most dangerous capability is not novel vuln discovery, it is **plausible-
backdoor production at speed**. The hierarchy in `trust-model.md`
§"Signals must be weighted by forgery resistance" does not move. The
pressure on the bottom of the hierarchy (code style, commit messages,
CI hygiene) continues to increase.

### Long-tail asymmetry sharpens

Glasswing + Mozilla/Anthropic = another top-of-tree project with Mythos-
grade defensive auditing. The 50-star, 10,000-dependent maintainer gets
nothing from either program. The gap signatory occupies
(`vision.md`:67-69) widens with every partnership announcement of this
shape. Strategic positioning sharpens, not muddies.

## New observation: visibility + risk mitigation is the durable posture

The Mozilla quote is the load-bearing sentence of the post for
signatory's purposes:

> We still can't afford to stop everything to rewrite decades of C++
> code, especially since Rust only mitigates certain (very common)
> classes of vulnerabilities.

A top-of-tree project with Glasswing-tier partner access to frontier
cyber capability, having just shipped 271 AI-identified fixes in one
release, is simultaneously explicit that:

1. **Find-and-fix does not generalize to rewrite-wholesale** even with
   Mythos-class capability. Capability compounds the audit surface, not
   the rewrite surface.
2. **Memory-safe rewrites are not a universal remedy** — even Rust, the
   canonical rewrite target, only mitigates certain classes. Other
   classes (logic bugs, authorization flaws, supply-chain
   substitutions) persist through any language migration.

The implication is that **visibility and risk-mitigation posture remain
a first-class engineering activity through the next capability
micro-epoch**, not a transitional activity superseded by AI-assisted
audit or memory-safe rewrite. Even organizations at the top of the
dependency tree, with Anthropic on retainer, will operate in a
triage-and-visibility regime for the foreseeable future. Everyone else
will more so.

This legitimates signatory's core premise from the mouth of the
best-resourced top-of-tree project: you do not get to stop thinking
about what you depend on. The work is assessment, prioritization, and
burn-down on a bounded audit surface — not elimination.

## Sharpened (incremental)

### AI-attribution metadata: first large public reference shape

The 04-14 entry flagged AI-found-and-fixed vulnerability attribution as
post-v0.1 signal work. Firefox 148 (22 fixes) and Firefox 150 (271
fixes) will shortly be the first large, public, dated reference shape
for the *"high concentration of AI-identified, human-reviewed fixes in a
single release"* commit-attribution pattern. When signatory's entity-
profile schema grows AI-attribution attributes, Firefox 150's advisory
data is a concrete corpus to calibrate against.

### Release-delta as signal (ambiguity increases)

A minor browser release going from ~22 → 271 security fixes in
consecutive cycles would historically have been a strong red flag —
suggestive of emergency disclosure, post-breach remediation, or
historical backlog. With AI-assisted auditing normalizing, **"large CVE
delta in a minor release" is now interpretively ambiguous**: it could
be audit-backlog clearing, a new scanning tool onboarded, an emergency
response, or a combination. The signal-interpretation model should
anticipate this — a release-delta signal needs a companion signal for
*why* (disclosed audit campaign vs. unannounced spike).

## What this does *not* do

### Does not promote to an era boundary

Consistent with the 04-14 and 04-17 deferrals. Mythos Preview deployed
via partnership is a horizontal-expansion event; the era anchor remains
reserved for GA frontier-capability crossings. This entry preserves the
deferral for one more cycle with a specified promotion condition.

### Does not ingest Mozilla or Anthropic capability claims as signals

Per the 04-17 discipline. The facts (models used, release versions,
fix counts, collaboration dates) are recorded as threat-landscape
inputs. The narrative ("defenders finally win decisively," "parity with
elite human researchers," "no new vulnerability classes") is vendor
communication and must not be plumbed through as signal weighting.

### Does not add Mythos Preview as a named capability in signal definitions

Signals should not refer to specific vendor model names or preview
tiers. The underlying capability — "AI audit of C/C++ codebases at
human-researcher parity, deployable at scale" — is what matters to the
threat model. Referring to "Mythos Preview" in signal definitions would
date them instantly and couple signatory's model to vendor naming.

### Does not revise the forgery-resistance hierarchy

Mozilla's "no new vuln classes" claim, if taken at face value,
*supports* the existing hierarchy rather than revising it. No change to
`trust-model.md` §"Signals must be weighted by forgery resistance."

## Open questions added to `design/open-questions.md`

- What is the specified promotion condition for a Mythos era boundary,
  given that "GA public release" is no longer the only plausible
  anchor? (Candidates: second partner-scale deployment on a distinct
  codebase; third capability delta on the same codebase; a
  non-Anthropic vendor shipping equivalent capability; leak or
  independent replication of Mythos-class capability.)
- Should release-delta signals carry a companion "disclosed audit
  campaign" annotation, and if so, from what source? (Mozilla's post is
  an in-band disclosure; most projects will not publish one.)
- Is Firefox 150's advisory corpus (once public) worth treating as a
  first-class calibration dataset for AI-attribution schema design, or
  left as opportunistic reference material?
- At what point does partner-deployment scale (Glasswing + Mozilla +
  whatever comes next) cumulatively clear the era-boundary bar even
  without a GA release?

## Cross-references

- [`2026-04-14-openai-tac-gpt54-cyber.md`](2026-04-14-openai-tac-gpt54-cyber.md)
  — era deferral this entry preserves; AI-attribution-metadata
  observation this entry provides the first reference shape for
- [`2026-04-17-vendor-communication-as-signal.md`](2026-04-17-vendor-communication-as-signal.md)
  — sotto voce principle this entry applies; horizontal/vertical
  expansion framing this entry instantiates with a production-scale
  datapoint
- [`2026-04-21-vercel-contextai-incident.md`](2026-04-21-vercel-contextai-incident.md)
  — parallel threat-landscape entry; identity-surface exposure axis
  unaffected by this entry
- [`../vision.md`](../vision.md) §"Motivation" — already references
  Mythos Preview and Glasswing; long-tail positioning sharpens with
  each top-of-tree partnership announcement
- [`../trust-model.md`](../trust-model.md) §"Temporal Trust Boundaries"
  — era deferral preserved; §"Signals must be weighted by forgery
  resistance" — hierarchy unchanged
- [`../ANTIPATTERNS.md`](../ANTIPATTERNS.md) — no vendor-tier signal
  ingestion, no federated positive-trust lists; both hold
- [`../open-questions.md`](../open-questions.md) — questions above
  tracked there for resolution
