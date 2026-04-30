# 2026-04-30: AISI Independent Evaluation of GPT-5.5 Cyber Capabilities

## Source

UK AI Security Institute (AISI), part of the Department for Science,
Innovation and Technology (DSIT), blog post published 2026-04-30:
<https://www.aisi.gov.uk/blog/our-evaluation-of-openais-gpt-5-5-cyber-capabilities>

WebFetch retrieved the article text on 2026-04-30; the structured
summary (numeric results, methodology, caveats, and direct quotes) is
captured in the conversation transcript and forms the basis of this
entry. The original URL is publicly readable; refetch if verbatim
quotes need verification.

The AISI publication coincides with a same-day UK government cyber-
resilience announcement (43% of UK businesses reporting a breach in
the past 12 months; £90M new resilience funding). That co-timing is
relevant to the adversarial parse below.

## Why this entry exists

The 04-22 Firefox/Mythos entry preserved the era-boundary deferral
"for one more cycle" and named four candidate promotion conditions:

> Second partner-scale deployment on a distinct codebase; third
> capability delta on the same codebase; a non-Anthropic vendor
> shipping equivalent capability; leak or independent replication of
> Mythos-class capability.

The 04-28 multi-vendor parity entry argued the third condition was
met by Theori's Xint claim, while explicitly recording that the
parity claim was *vendor-asserted, not independently verified*, and
deferred ingestion of the headline number until independent
observation existed.

This entry records that independent observation. AISI is a UK
government evaluator with no commercial stake in selling cyber
tooling, and their published numbers place GPT-5.5 at statistical
parity with Mythos Preview on a 95-task expert-tier CTF battery.
**Independent third-party verification of multi-vendor frontier
parity is now on the record.** Combined with the 04-22 partner-scale
operational datapoint and the 04-28a vendor parity claim it
corroborates, the era-boundary trigger condition is now
overdetermined.

The entry also captures the first dollar-denominated threat-economics
data point in the threat-landscape record, and a 6-hour universal-
jailbreak finding that quantifies the operational fragility of
vendor-side abuse monitoring.

## What was published

1. **Methodology.** AISI evaluated four models — OpenAI's GPT-5.5,
   Anthropic's Claude Mythos Preview, OpenAI's GPT-5.4, and
   Anthropic's Claude Opus 4.7 — on:
   - 95 capture-the-flag challenges across four difficulty tiers
     (vulnerability research, reverse engineering, web exploitation,
     cryptography).
   - Two cyber ranges: "The Last Ones" (TLO) — a 32-step corporate-
     network attack simulation across four subnets and ~20 hosts; and
     "Cooling Tower" — a 7-step industrial control system attack.
2. **Expert-tier CTF results** (mean ± 1 SE):

   | Model           | Pass rate | ±1 SE |
   |-----------------|-----------|-------|
   | GPT-5.5         | 71.4%     | 8.0   |
   | Mythos Preview  | 68.6%     | 8.7   |
   | GPT-5.4         | 52.4%     | 9.8   |
   | Opus 4.7        | 48.6%     | 10.0  |

3. **TLO (corporate-network range).** GPT-5.5 completed end-to-end
   in 2/10 attempts at a 100M-token budget; Mythos Preview achieved
   3/10. AISI describes GPT-5.5 as "the second model" to complete the
   range end-to-end at any rate.
4. **Cooling Tower (ICS range).** No model — including GPT-5.5 —
   solved the range.
5. **Notable single-task result.** GPT-5.5 solved the `rust_vm`
   reverse-engineering challenge (custom virtual machine, recovered
   instruction set, built disassembler, analyzed authentication
   logic, solved constraint satisfaction) in **10 minutes 22 seconds
   at $1.73 in API spend**, against an estimated 12-hour human-expert
   solution time.
6. **Red-teaming finding.** Six hours of expert red-teaming produced
   "a universal jailbreak that elicited violative content across all
   malicious cyber queries." OpenAI subsequently updated safeguards;
   AISI reports "a configuration issue in the version provided meant
   UK AISI were unable to verify the effectiveness of the final
   configuration."
7. **Stated caveats.** Testing in a controlled research setting; not
   reflective of public-user access; agent already had network access
   to vulnerable targets; AISI explicitly states "we cannot say from
   these results whether GPT-5.5 would succeed against a well-defended
   target."
8. **AISI's framing claim.** "If cyber-offensive skill is emerging as
   a byproduct of more general improvements in long-horizon autonomy,
   reasoning, and coding, we should expect further increases in cyber
   capability from models in the near future, potentially in quick
   succession."
9. **Policy co-timing.** Same-day HMG announcement: 43% of UK
   businesses suffered a cyber breach or attack in the past 12
   months; £90M new funding to boost cyber resilience; AI capability
   evaluations positioned as part of HMG's response strategy.

## What this reinforces

### Era-boundary trigger condition: now triply satisfied with independent verification

The 04-22 promotion conditions list four candidates; the cumulative
evidence as of 2026-04-30 satisfies three of them:

| Condition (per 04-22)                                        | Evidence                                              | Status                              |
|--------------------------------------------------------------|-------------------------------------------------------|-------------------------------------|
| Second partner-scale deployment on a distinct codebase       | Firefox 150 (271 fixes via Mythos Preview)            | 04-22 — *vendor-asserted, public*   |
| Third capability delta on the same codebase                  | —                                                     | not yet                             |
| Non-Anthropic vendor shipping equivalent capability          | Theori Xint (parity + 12 additional zero-days)        | 04-28a — *vendor-asserted*          |
| Leak or independent replication of Mythos-class capability   | AISI evaluation: GPT-5.5 ≈ Mythos Preview at expert tier | 04-30 — *independently measured* |

The AISI publication is the strongest of the three on epistemic
grounds: it is the first claim of multi-vendor frontier parity
*measured by a non-commercial third party*. The 04-28a entry treated
the era-boundary trigger as met. This entry treats it as
overdetermined and recommends the era-boundary edit be promoted from
"indicated" to "scheduled" in the next planning cycle.

The era-boundary edit itself remains a separate commit (see "What
this does not do" below).

### Statistical reading: frontier parity, two-tier capability landscape

The headline framing ("GPT-5.5 leads") is media calibration. The
honest statistical reading is that the two pairs (GPT-5.5, Mythos
Preview) and (GPT-5.4, Opus 4.7) heavily overlap within ±1 SE
internally, while the gap between pairs is well outside ±1 SE. The
operative finding is therefore:

> **Two frontier-tier models at statistical parity (GPT-5.5,
> Mythos Preview); two sub-frontier models at statistical parity
> (GPT-5.4, Opus 4.7), ~20 percentage points behind.**

This is a stronger and more useful statement than a linear ranking,
and it sharpens the 04-28a finding rather than complicating it. The
threat is *two frontier capability sources*, not "OpenAI overtook
Anthropic." The federation-asymmetry argument (`ANTIPATTERNS.md`,
04-14) tightens with each additional vendor at parity.

### Forgery-resistance hierarchy: vendor-side abuse monitoring is empirically fragile

The 04-28a entry argued, on the basis of Becker's quote, that
sophisticated threat actors would self-host open-weight models
rather than route through vendor-monitored APIs. AISI's 6-hour
universal-jailbreak finding is the closed-weight-side empirical
companion: *the vendor-side ceiling fails on a one-business-day
timescale to a competent third party with controlled access.*

This does not move the forgery-resistance hierarchy itself
(`trust-model.md` §"Signals must be weighted by forgery resistance"
is unchanged). It does sharpen the 04-14 rejection of TAC-tier
verification as a positive trust signal: vendor safeguard programs
have measurable failure rates, and "verified-via-vendor-program" is
not a meaningful identity signal even when the vendor is acting in
good faith. The structural failure is the federation asymmetry; the
6-hour jailbreak just supplies the timescale.

### Long-tail asymmetry: now with unit economics

The 04-28a Guido quote — *"You can write exploits for software that
only one company has… for software that exists in only one
configuration that one company has. And you can do it on the fly"* —
established the qualitative shift in attacker economics. The
`rust_vm` data point quantifies it at one observation:

- $1.73 / ~10 minutes / one custom-VM reverse-engineering task that
  AISI estimates at 12 human-expert hours.
- At $50/hour expert rate, that is ~$600 of expert work for $1.73
  of compute. ~720× speedup; ~1/350 the cost.

For signatory's signal model the implication is qualitative
already: the cost-to-attack arguments embedded in older threat
modeling assume per-target attacker effort that no longer holds at
this price point. Signal definitions that depend on "this attack is
not economic to mount against the long tail" must be re-examined
against current dollar-per-expert-hour-equivalent figures, not
generic "AI is cheaper" framing.

This is one data point. Recording the methodology of capturing
dollar/time/expert-equivalent triples — when threat-landscape
sources publish them — is the durable contribution; the specific
$1.73 number will move.

Worth citing in `vision.md` §Motivation alongside the existing
CyberGym 83.1% number on the next revision; worth recording in
`analysis-economics.md` if a recurring class of such data points
emerges.

### Cooling Tower (ICS) as the next-likely capability-frontier marker

Cooling Tower is the only AISI range no model has solved. AISI's own
"potentially in quick succession" framing implies the next era's
capability surface includes industrial control systems, and the
next era boundary is plausibly anchored around the first model that
clears Cooling Tower (or a comparable ICS benchmark from another
evaluator). Worth flagging now as a falsifiable forecast: if no
model has solved an ICS range by mid-2026, the rate of capability
progress is observably slower than current framing suggests; if a
model clears it before then, the era-boundary cadence is
accelerating.

This is not a v0.1 signal change. It is a marker for the next
threat-landscape review cycle.

## New observation: third-party government evaluator as a distinct input class

Earlier entries treated capability claims as either:

- **Vendor announcements** (04-14 OpenAI TAC, 04-17 Opus 4.7
  release, 04-22 Mozilla/Anthropic co-marketing). Adversarial parse
  required; framing discounted heavily; facts (release dates,
  programmatic access boundaries, fix counts) recordable.
- **Vendor-product capability demonstrations** (04-28a Theori Xint).
  Vendor-asserted parity claims; treated as "exists and is publicly
  demonstrated" without ingesting headline numbers as ground truth.

AISI introduces a third class: a non-commercial government evaluator
with controlled access to vendor-supplied models, publishing numeric
comparisons. The epistemic status sits between the two prior
classes:

- Higher trust than vendor self-reports because AISI does not sell
  the products being evaluated and runs its own benchmark battery.
- Lower trust than independent academic replication (which does not
  yet exist for this class of evaluation) because AISI tests what
  the vendor provides, not what the vendor ships at GA, and cannot
  always verify post-test configuration changes (see configuration-
  verification gap below).
- Institutional alignment with "evaluations are load-bearing for
  policy" introduces a different framing bias than commercial
  framing — not absent, just oriented differently.

Threat-landscape entries should record AISI publications when they
arrive, with framing parsed adversarially per 04-17. The volume so
far is one entry; the structured-input question is deferred until a
recurring volume materializes.

## New observation: dollar-denominated threat economics enter the record

The 04-28a entry's threat-economics framing was qualitative ("per-
target bespoke exploitation is now economically viable"). AISI's
`rust_vm` data point introduces the first quantitative anchor:

- **One task; one model; one configuration; published cost.** Not
  generalizable to all tasks; sufficient as a calibration anchor.
- For threat-landscape entries that involve cost-to-attack
  arguments, the v0.1-era assumption "AI compute is the rounding
  error in attacker economics" should be cited concretely against
  AISI-class numbers, not as folklore.

The architectural implication is small (no signal-set change), and
the reportorial implication is large: when future threat-landscape
sources publish dollar/time/expert-equivalent triples, they should
be recorded as such, with the source, the task, and the model clearly
identified. The triple is the unit; the headline ratio is the
shorthand.

## Adversarial parse of the AISI piece (per 04-17 discipline)

### AISI institutional incentives

AISI is not commercial, but is not neutral. Its remit, funding case,
and ongoing relevance depend on AI capability evaluations being seen
as load-bearing for HMG cyber policy. The same-day publication
schedule (alongside the 43% breach statistic and the £90M resilience
announcement) makes the framing — "potentially in quick succession",
the rust_vm anecdote leading the prose, the policy-context section —
calibrated for a policy moment, not for signatory's signal model.

The numeric measurements are durable artifacts. The urgency framing
is communications work. Discount the rhetoric; keep the numbers.

### CTF benchmarks ≠ deployment realism

AISI is explicit, and we should be too:

- Tests run in a controlled research setting.
- The agent is directed at specific vulnerable targets where it
  already has network access.
- Public deployments include additional safeguards, monitoring, and
  access controls.
- "We cannot say from these results whether GPT-5.5 would succeed
  against a well-defended target."

The numbers measure a capability ceiling under contrived conditions,
not an exploitation rate against real defenders. The same caveat the
04-22 entry applied to Mozilla's "271 fixes" headline applies here:
record the fact, do not ingest the deployment-realism framing.

### Configuration-verification gap

AISI was unable to verify the final safeguard configuration after
OpenAI's update following the universal-jailbreak finding. By the
04-17 sotto voce discipline:

- **Recordable fact:** a universal jailbreak existed in the version
  AISI tested.
- **Vendor-asserted fact:** OpenAI patched it.
- **Currently unverifiable claim:** the patch is effective in
  shipped configurations.

This is the same epistemic structure as the Theori Xint parity claim
*before* this AISI entry corroborated it: vendor-asserted, not
independently verified. The OpenAI safeguard claim is in that bucket
until an evaluator can re-test the shipped configuration. Worth
tracking; not currently dispositive.

### Headline parity vs. statistical parity

The "GPT-5.5 leads Mythos Preview" reading is media framing; ±1 SE
intervals overlap heavily and the defensible reading is parity.
Future threat-landscape entries citing this evaluation should use
the statistical reading; the headline number is shorthand for
"frontier-tier," not for a vendor ranking.

### The Cooling Tower null result is not vendor-favorable evidence

It is tempting to read "no model solved Cooling Tower" as evidence
that vendor safeguards or capability ceilings constrain offensive
ICS deployment. The honest reading is **the frontier hasn't reached
ICS yet, and AISI's framing implies it will.** The null result is a
falsifiable forecast for the next cycle, not a reassurance about
present-day exposure.

## What this does *not* do

### Does not write the era-boundary change itself

Recommended (overdetermined now), but the actual edit to
`trust-model.md` §"Temporal Trust Boundaries" is a separate commit
informed by 04-22, the 04-28 pair, and this entry. The era-anchor
question — should the new era anchor on 2026-04-22 (Firefox 150 ship
date), 2026-04-28 (Theori claim and same-day Cox reading), or
2026-04-30 (independent third-party verification)? — is itself open
and deserves discussion in the era-boundary commit, not here.

### Does not treat AISI evaluation results as direct signal inputs

A package's trustworthiness is not a function of which frontier
model can or cannot solve a CTF battery. AISI's measurements inform
the threat-landscape posture (which informs era-boundary
interpretation, which informs signal weighting); they do not
themselves become signals about specific packages. No
`signals-v01.md` change is implied.

### Does not promote vendor-tier or evaluator-tier as a positive trust signal

AISI's evaluation does not constitute an endorsement of OpenAI; it
constitutes evidence about capability availability. The 04-14 and
04-17 rejections of vendor-mediated KYC and federated approve lists
extend by analogy: evaluator-mediated capability tiers are not
positive trust signals about the entities that hold them either.
Same federation-asymmetry reasoning.

### Does not commit to specific cost numbers as durable parameters

$1.73 for `rust_vm` at 10:22 is *one* data point on *one* task by
*one* model. The unit economics will move. The methodology — record
dollar/time/expert-equivalent triples when sources publish them — is
the durable contribution. Future threat-landscape entries should
not cite "$1.73" as the rate; they should cite the methodology of
capturing the triple.

### Does not propose AISI as a structured recurring input class

Threat-landscape volume so far includes one government-evaluator
publication. The schema does not need to grow to ingest evaluator
outputs as a structured class. Revisit if AISI (or peer evaluators
elsewhere — e.g., a US AISI counterpart, EU evaluator, JP NICT)
ship a comparable evaluation in the next two quarters and the
content of those evaluations begins to repeat.

## Open questions added to `design/open-questions.md`

- The era-boundary trigger is now overdetermined (three of 04-22's
  four conditions met, with the 04-30 confirmation being the
  strongest epistemically). What is the new era's anchor date —
  2026-04-22 (operational deployment), 2026-04-28 (multi-vendor
  capability claim), or 2026-04-30 (independent third-party
  verification)? The existing era model anchors on single capability-
  release dates; the multi-trigger pattern this cycle instantiates
  has no specified anchor convention.
- Should `vision.md` §"Motivation" or `analysis-economics.md` grow a
  citable threat-economics calibration table — recording
  dollar/time/expert-equivalent triples from threat-landscape sources
  as they accrue — so that threat-economics arguments in v0.1
  documentation are anchored to dated measurements rather than
  folklore?
- What is the standing discount rate for government-evaluator
  publications relative to vendor self-reports? Higher than vendor
  self-reports, lower than independent academic replication; track
  the actual variance over the next several entries.
- Should signatory record "next watched capability threshold"
  annotations on threat-landscape entries (here: ICS / Cooling Tower
  class) so that capability-progress forecasts are falsifiable in
  retrospect? Currently captured only in narrative prose.
- Has the Firefox 150 advisory corpus (flagged in 04-22 as a
  potential calibration dataset for AI-attribution schema design)
  become public? Cross-reference review when the schema work begins.

## Cross-references

- [`2026-04-28-multi-vendor-mythos-parity.md`](2026-04-28-multi-vendor-mythos-parity.md)
  — Theori Xint vendor-asserted parity claim independently
  corroborated by this entry; era-boundary trigger reading
  reinforced
- [`2026-04-28-llm-decompilation-binaries-as-source.md`](2026-04-28-llm-decompilation-binaries-as-source.md)
  — same-day pair entry to 04-28a; reproducible-from-source signal
  recommendation gains weight from this entry's capability-frontier
  evidence
- [`2026-04-22-mozilla-anthropic-firefox-mythos.md`](2026-04-22-mozilla-anthropic-firefox-mythos.md)
  — partner-scale operational datapoint that this entry's
  independent verification joins to overdetermine the era trigger;
  promotion-condition list this entry references directly
- [`2026-04-21-vercel-contextai-incident.md`](2026-04-21-vercel-contextai-incident.md)
  — parallel threat-landscape entry; identity-surface axis
  unaffected by this entry
- [`2026-04-17-vendor-communication-as-signal.md`](2026-04-17-vendor-communication-as-signal.md)
  — sotto voce discipline applied to AISI's policy framing and to
  OpenAI's "patched safeguards" claim; horizontal/vertical expansion
  framing
- [`2026-04-14-openai-tac-gpt54-cyber.md`](2026-04-14-openai-tac-gpt54-cyber.md)
  — direct predecessor on the OpenAI vendor lineage; GPT-5.4
  measured here as the sub-frontier tier; vendor-tier KYC rejection
  reinforced by AISI's 6-hour-jailbreak finding
- [`../vision.md`](../vision.md) §"Motivation" — long-tail
  positioning sharpens with dollar-denominated threat economics;
  CyberGym 83.1% number gains complementary AISI parity
  calibration; consider citing on next revision
- [`../trust-model.md`](../trust-model.md) §"Temporal Trust
  Boundaries" — era-boundary edit overdetermined; specific anchor
  question added to open-questions
- [`../trust-model.md`](../trust-model.md) §"Signals must be
  weighted by forgery resistance" — hierarchy unchanged; vendor-
  side abuse-monitoring fragility quantified at one-business-day
  timescale
- [`../ANTIPATTERNS.md`](../ANTIPATTERNS.md) — federated-approve-
  list rejection unchanged; vendor safeguard programs and evaluator
  tiers as non-federate-able positive signals
- [`../analysis-economics.md`](../analysis-economics.md) — dollar-
  per-expert-hour-equivalent calibration worth citing on next
  revision
- [`../open-questions.md`](../open-questions.md) — questions above
  tracked there for resolution
