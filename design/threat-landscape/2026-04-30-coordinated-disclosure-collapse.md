# 2026-04-30: Coordinated Disclosure Collapse — Kernel Confirms Duplicate-Find Inside Merge Window

## Source

oss-security mailing list thread "Coordinated Disclosure in the LLM
Age." The visible chain (as pasted into the conversation transcript
by the user on 2026-04-30) is:

- **An earlier coordinator** (unnamed in the visible excerpt; quote
  depth in the original indicates one or more earlier messages in
  the thread): *"I'm sorely tempted, both due to the increased volume
  and the risk of premature disclosure, to just assume that any
  vulnerability reported as a result of research using an LLM is
  trivially discoverable by others, and give up trying to pretend
  there's any point to working it under embargo."* The voice — "give
  up trying to pretend" — is in the register of someone who runs an
  embargo process, not a researcher.
- **Jacob Bachmeyer** (2026-04-29 05:18 +0200, message quoted by
  Lang): *"You are correct here: you should assume that any LLM will
  give a similar result to another person who asks a similar
  question. In other words, LLM-discovered vulnerabilities should be
  considered already publicly known."*
- **Clemens Lang** (2026-04-29 20:52 +0200): *"As a further data
  point backing up this theory: We're seeing duplicate reports of
  the same issue found by multiple independent groups that use LLMs,
  within the embargo period."*
- **Greg Kroah-Hartman** (2026-04-30 15:48 +0200, Message-ID
  `<2026043030-unpinned-grafted-38eb@gregkh>`): *"We (on the kernel)
  are seeing duplicate reports of the same issue from different
  groups within the time period it takes to get a fix merged (i.e.
  just within a few days)."*

The full thread is reproducible from the oss-security archive at
`lists.openwall.com`. This entry treats the four-message chain above
as the artifact of record; earlier messages in the thread are
referenced but not reproduced.

This entry pairs with the same-day
[`2026-04-30-aisi-gpt55-evaluation.md`](2026-04-30-aisi-gpt55-evaluation.md).
The two together are signatory's 2026-04-30 threat-landscape
snapshot, structurally analogous to the 04-28 pair (capability /
operational-consequence pairing on the same date).

## Why this entry exists

The 04-28 multi-vendor parity entry recorded Katie Moussouris's quote
*"from the time a vulnerability is announced to the time where there
is exploit code available has now shrunk to pretty much zero"* — and
parsed it adversarially as a Verge-piece quote from a vendor with a
commercial position in the framing.

This entry records the **operational confirmation** of the same
phenomenon, sharpened in two ways the 04-28a entry could not:

1. The voice is Greg Kroah-Hartman, reporting from inside the Linux
   kernel security workflow. He is not selling a product, not
   announcing a partnership, not running an evaluator, and not
   publishing alongside a policy moment. The post is conspicuously
   brief.
2. The claim is *stronger* than Moussouris's: not "exploit available
   faster after disclosure" but **duplicate independent finds before
   the fix can be merged**. That collapses the embargo window itself,
   not just the post-disclosure window.

The entry also confirms the 04-28a "downstream patch-adoption window"
concern from the *upstream* side: at the most-resourced OSS security
workflow on the planet, upstream maintainers are barely ahead of
duplicate finders. The downstream consumer is therefore not "ahead
of the recipe window for a few days" — they are behind it from the
moment a fix lands.

## What was claimed

1. **Embargo as a working process is under collapse pressure** (the
   earlier coordinator's posture). The proposed response — *assume
   LLM-discovered vulnerabilities are public knowledge from the
   moment they are reported* — is a structural change in coordinated
   disclosure, not an incremental tightening.
2. **Theoretical justification** (Bachmeyer): different LLM users
   asking similar questions reach the same vulnerabilities. Treat
   them as publicly known by default.
3. **Operational confirmation, coordinator side** (Lang): duplicate
   reports of the same issue from multiple independent LLM-using
   groups arrive *within the embargo period*.
4. **Operational confirmation, kernel side** (Greg KH): duplicate
   reports from different groups arrive *within the time it takes
   to merge a fix* — described as "just within a few days."

## What this reinforces

### 04-28a Moussouris quote upgraded from theory to operational practice

The 04-28a entry recorded:

> *"From the time a vulnerability is announced to the time where
> there is exploit code available has now shrunk to pretty much
> zero."*

— and explicitly discounted it as commercially-positioned framing.
Greg KH's confirmation is the same observation, made from inside the
workflow that has to operate against the change, by a maintainer
with no product to sell. The directional claim is independently
validated; the magnitude becomes credible.

The specific upgrade matters: Moussouris was talking about *post-
disclosure* exploit availability. Greg's claim covers the *pre-
disclosure* embargo window itself. The window of maintainer
advantage doesn't shrink — it inverts. By the time the maintainer
is ready to disclose, the bug is already known to other finders who
arrived independently.

### Mature-cyber audit-currency framing reinforced from the operational side

The framing established for the deferred Era 4 addition (target
durability, not authorship capability — code unexamined and
unrepaired through the maturation phase has surfaces that won't
weather modern attacks) gets a sharper active mechanism from this
entry:

**Even maintained code is being chewed through by LLM-mediated
finders at a rate that exceeds the patching cadence of the most-
resourced OSS project on the planet.**

The mature-cyber boundary was framed around "unexamined and
unrepaired" code. Greg's report sharpens that: the relevant scope
extends to *examined* code whose maintainer's review-and-merge
cadence cannot keep up with duplicate-find rates. The audit-currency
question is no longer "has anyone looked at this code recently?" but
"has anyone looked at this code recently *enough that the
intervening duplicate-find rate is bounded*?"

For long-tail projects, that bound is much tighter than for the
kernel. The kernel is at the threshold of duplicate-find-overrun;
projects below that resource tier are past the threshold by
construction.

### Long-tail asymmetry sharpens further

The 04-22 (Glasswing/Mozilla), 04-28a (multi-vendor parity), and
04-30 AISI entries each argued the gap signatory occupies widens.
This entry adds the *defensive operational* dimension: the most-
resourced OSS security workflow is at the threshold of embargo
collapse. Anything below that resource tier is past the threshold.

Strategic positioning sharpens. The 50-star, 10,000-dependent
maintainer (`vision.md`:67-69) cannot run the embargo workflow Greg
is reporting from — they don't have one to begin with. In the new
regime, that is no longer purely a maturity gap. It is an *embargo-
feasibility gap*: even if such a project wanted to coordinate
disclosure, the LLM-mediated duplicate-find rate against their
codebase exceeds whatever fix-merge cycle a solo maintainer can
maintain. The defensive posture available to them is the one Greg
proposes for the kernel — assume publicly known, ship the fix, move
on.

### High-criticality monitoring threshold tightens (per `trust-model.md` §Criticality)

`trust-model.md` §"Criticality (Dual-Edged)" specifies:

> The monitoring threshold should be inversely proportional to
> criticality. The more critical a dependency is, the smaller the
> anomaly needed to trigger investigation.

Greg's data point makes this less of an aspiration and more of an
operational constraint. For high-criticality kernel-class
dependencies, the relevant time-scale for "anomaly to investigation"
is now the duplicate-find rate, which Greg places at "a few days."
For projects whose monitoring threshold cannot be tightened to that
horizon, the criticality multiplier they nominally have is not being
delivered.

This is not a v0.1 signal change. It is a sharpening of the
operational meaning the existing principle was already pointing at.

## New observation: CVE history as a signal needs reinterpretation

A project's CVE-publication pattern was historically a positive
process signal — "they find and fix bugs; that is hygiene." Going
forward, CVE-publication patterns reflect both the underlying bug
rate *and* the embargo-feasibility ceiling. A project with falling
CVE count after 2026 may be:

- Building higher-quality code (positive)
- Outsourcing security work to upstream and not finding their own
  (neutral-to-negative; transitive trust risk)
- Moving to immediate-disclosure / fix-and-publish without CVE
  coordination (operational shift in the Greg KH direction; not a
  hygiene change)
- Unable to keep ahead of duplicate finders and quietly retiring the
  embargo process (negative; the absence of CVEs is the *evidence*
  of operational pressure, not its absence)

These are interpretively distinct and signatory's signal model
cannot disambiguate them from CVE counts alone. The *interpretation*
of CVE-history signals shifts in the new regime; the data shape does
not. No `signals-v01.md` change is implied by this entry. The
sharpening is in how synthesis prose and handoff prompts read CVE
trajectories — which is doc territory, not v0.1 collector work.

## New observation: time-to-merge for security fixes as a load-bearing signal

Vitality signals (`signals-v01.md`) already capture commit cadence
broadly. Greg's framing makes a specific subset urgently relevant:
**median time-to-merge for security-related PRs**. In the new
operational regime, that interval defines the upper bound on embargo
feasibility. A project whose security-fix time-to-merge exceeds the
LLM-mediated duplicate-find rate cannot run a useful embargo at all
— and that constraint applies before any judgment about the quality
of the fixes.

Existing vitality signals (commit cadence, last-push, last-commit)
hint at this but do not measure it. Direct measurement requires
distinguishing security PRs from other PRs, which is non-trivial
across ecosystems (label conventions vary; CVE-linked PR
identification is heuristic). Out of v0.1 scope; flag as a candidate
v0.2 signal class.

## New observation: candidate post-v0.1 signal class — disclosure-practice posture

Whether a project still operates a coordinated-embargo workflow at
all, vs has moved to immediate-disclosure / fix-and-publish, becomes
meaningful information about how the project is operating in the new
regime. Greg's post is the first dated public-record signal that
this practice is shifting at the top of OSS.

Observable artifacts that would feed such a signal:

- `SECURITY.md` content and its evolution over time
- Embargo-mention patterns in security advisories
- CVE-numbering-authority participation
- Distro security-team coordination patterns

None of these are v0.1 signals. They are flagged as a candidate
class for the post-v0.1 signal-set evolution that the 04-28 pair
also opened questions about.

## Adversarial parse (per 04-17, applied lightly)

Greg KH is the rare conspicuously-non-positioned voice in the recent
threat-landscape record. He is not selling a product, not announcing
a partnership, not running an evaluator, and not publishing
alongside a policy moment. The 04-17 sotto voce discipline applies
much less aggressively than it does to the AISI piece or the 04-28a
Verge sources.

That said, calibration still matters:

### Single-project sample

Kernel threat surface and contributor density are unique. The
duplicate-find rate Greg observes plausibly reflects a project with
an unusually wide attention surface (every serious cyber researcher
who tries an LLM does so against the kernel at some point). For
narrower projects with fewer LLM users probing them in the same week,
"duplicate within merge window" may be much rarer. The directional
claim generalizes; the rate does not.

### No magnitude numbers

Greg gives "a few days" as the merge-window benchmark and reports
duplicates within it. He does not give:

- A rate (what fraction of bug reports now arrive duplicated?)
- A baseline (what was the pre-LLM duplicate rate for comparison?)
- A trend (is the rate stable, accelerating, or seasonal?)

The qualitative observation is durable; quantitative inferences from
this single message are premature.

### Selection effect on what reaches oss-security

Embargo-breakdown is the kind of operational concern that surfaces
as a mailing-list post when it is happening; smooth coordinations
generate no posts. The signal is real. Extrapolating "this is the
new normal across all OSS" from this thread is a stronger claim than
the post itself supports — it is supported by the *combination* of
this thread with the 04-28a Verge sources and the AISI evaluation,
not by Greg's message alone.

### Bachmeyer's framing remains a step ahead of the data

Bachmeyer's claim that *"any LLM will give a similar result to
another person who asks a similar question"* is a strong theoretical
position. Lang and Greg confirm the *consequence* (duplicates
arrive) but not the *mechanism* (similar prompts → similar
findings). The mechanism is plausible and consistent, but the
threat-landscape record should treat the consequence as the
established fact and the mechanism as the working hypothesis.

## What this does *not* do

### Does not change v0.1 signal definitions

The signal model already captures CVE history and vitality cadence
as inputs. The interpretation of those signals shifts in the new
regime, but the data shape does not. No `signals-v01.md` change.

### Does not propose new collectors

Time-to-merge-for-security-PRs and disclosure-practice-posture are
flagged as candidate post-v0.1 signal classes; no collector work is
in scope for this entry.

### Does not retire embargo as a positive signal

A project that runs a working embargo workflow today still exhibits
practice quality. The signal interpretation softens going forward;
it does not invert. "Retired their embargo workflow" is a *change*
event whose interpretation depends on context (operational shift in
the Greg KH direction vs. abandonment vs. process degradation), not
a flat negative.

### Does not extrapolate kernel rates to all OSS projects

Kernel contributor density and attention surface are unique. The
directional claim generalizes; the magnitudes do not. Future
threat-landscape entries citing this entry should reference the
direction, not import "a few days" as a rate for unrelated projects.

### Does not re-litigate the era-boundary trigger

The era-boundary trigger condition is already overdetermined per
the 04-30 AISI entry. This entry adds operational-side reinforcement
to the same case; it does not change the recommendation.

### Does not commit Greg KH or any thread participant to a position

The participants are reporting observations, not endorsing
signatory's signal model. The threat-landscape record cites their
observations; it does not federate their authority.

## Open questions added to `design/open-questions.md`

- What is the right way to model "disclosure-practice posture" as a
  v0.2 signal class? Observable artifacts (`SECURITY.md` content,
  embargo-mention patterns in advisories, CVE-numbering-authority
  participation) exist but are heterogeneous across ecosystems.
- Should "median time-to-merge for security-related PRs" be a v0.2
  signal? Collection mechanics: GitHub label conventions vary,
  CVE-linked PR identification is heuristic, ecosystem coverage is
  uneven.
- Does the kernel's specific situation (high contributor density,
  broad threat surface) generalize to other top-of-tree projects, or
  is the kernel uniquely fast-saturating? Track via subsequent
  threat-landscape entries from other top-of-tree projects.
- For projects that retire coordinated embargo, what does signatory
  record about that transition, and when? Distinct from "no embargo
  workflow exists" (the long-tail default) and from "embargo
  workflow exists and is operating" (current signal).
- Does the rate Greg reports ("a few days") track frontier
  capability over time? If yes, time-to-merge thresholds will tighten
  with each capability era. If no, this is a one-time operational
  shift and the rate will stabilize.

## Cross-references

- [`2026-04-30-aisi-gpt55-evaluation.md`](2026-04-30-aisi-gpt55-evaluation.md)
  — same-day pair entry. AISI gives the capability-frontier
  measurement; this entry gives the operational-consequence
  observation. Together they are the 2026-04-30 threat-landscape
  snapshot.
- [`2026-04-28-multi-vendor-mythos-parity.md`](2026-04-28-multi-vendor-mythos-parity.md)
  — Moussouris quote whose theoretical claim this entry confirms
  operationally; "downstream patch-adoption window" concern this
  entry sharpens from the upstream side.
- [`2026-04-28-llm-decompilation-binaries-as-source.md`](2026-04-28-llm-decompilation-binaries-as-source.md)
  — Cox-on-CVE-2026-3854 reading; same family of "defensive premise
  no longer holds" arguments (closed-binary opacity → embargo
  window).
- [`2026-04-22-mozilla-anthropic-firefox-mythos.md`](2026-04-22-mozilla-anthropic-firefox-mythos.md)
  — long-tail asymmetry baseline this entry sharpens from the
  defensive operational angle.
- [`2026-04-21-vercel-contextai-incident.md`](2026-04-21-vercel-contextai-incident.md)
  — parallel threat-landscape entry; identity-surface axis
  unaffected by this entry.
- [`2026-04-17-vendor-communication-as-signal.md`](2026-04-17-vendor-communication-as-signal.md)
  — sotto voce discipline; applied lightly here because Greg KH is
  the rare non-positioned voice.
- [`2026-04-14-openai-tac-gpt54-cyber.md`](2026-04-14-openai-tac-gpt54-cyber.md)
  — original era-deferral entry; trust-accumulates-slowly /
  degrades-fast principle this entry's duplicate-find observation
  sharpens.
- [`../vision.md`](../vision.md) §"Motivation" — long-tail
  positioning sharpens from the defensive-operational angle. Worth
  citing on next revision alongside the existing 04-28a Guido
  long-tail-targeting quote.
- [`../trust-model.md`](../trust-model.md) §"Criticality
  (Dual-Edged)" — the inversely-proportional monitoring threshold
  this entry makes operationally pressing.
- [`../trust-model.md`](../trust-model.md) §"Trust accumulates
  slowly but degrades fast" — the asymmetry this entry's
  duplicate-find observation reinforces.
- [`../trust-model.md`](../trust-model.md) §"Temporal Trust
  Boundaries" — era-boundary trigger overdetermined; this entry adds
  operational-side reinforcement. The deferred Era 4 (`mature-cyber`)
  framing decision (target durability, not authorship capability)
  receives an active-mechanism datapoint from this entry.
- [`../signals-v01.md`](../signals-v01.md) — no v0.1 changes
  recommended; two candidate v0.2 signal classes flagged
  (time-to-merge-for-security-PRs; disclosure-practice posture).
- [`../ANTIPATTERNS.md`](../ANTIPATTERNS.md) — federated-approve-list
  rejection unchanged.
- [`../open-questions.md`](../open-questions.md) — questions above
  tracked there for resolution.
