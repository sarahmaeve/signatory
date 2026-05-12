# 2026-04-28: Multi-Vendor Mythos Parity and the Patchpocalypse Frame

## Source

The Verge, AI cybersecurity feature published on or about 2026-04-28
(URL not captured by the user when reproducing the article; full text
was pasted into the conversation transcript). The piece collates voices
from Trail of Bits (Dan Guido), Theori (Tim Becker), Luta Security
(Katie Moussouris), and Security Superintelligence Labs (Joshua Saxe),
framed around the DARPA AIxCC results, Anthropic's Claude Mythos
announcement, and Theori's commercial bug-finding tool Xint.

This entry pairs with the same-day
[`2026-04-28-llm-decompilation-binaries-as-source.md`](2026-04-28-llm-decompilation-binaries-as-source.md),
which addresses Russ Cox's reading of the Wiz / GitHub CVE-2026-3854
disclosure. The two entries together are signatory's 2026-04-28
threat-landscape snapshot.

## Why this entry exists

The 04-14 deferral specified the era-boundary review trigger as one of:
GA Mythos release, a second partner-scale deployment on a distinct
codebase, *or* a non-Anthropic vendor shipping equivalent capability.
The 04-22 entry added Mozilla/Firefox 150 as a partner-scale operational
datapoint and preserved the deferral one cycle.

This entry records the first credible public claim of **multi-vendor
parity** with Mythos-class capability — Theori's Xint, per Becker's
Verge interview, "found all the bugs Mythos did when scanning the same
codebases" plus 12 additional zero-days not in Anthropic's announcement.
Combined with the 04-22 datapoint, this is the strongest evidence yet
that the era-trigger condition is met without waiting for a Mythos GA
release.

The entry also captures three subordinate observations the Verge piece
uniquely articulates: the patchpocalypse / time-to-exploit collapse, the
attacker shift down the long tail, and open-weight self-hosting as a
threat-actor capability floor.

## What was claimed

1. **Multi-vendor parity (Theori Xint).** Per Becker, Theori's
   commercial tool Xint matches Mythos against the same codebases and
   found 12 additional zero-day vulnerabilities not in Anthropic's
   announcement. Theori is reporting findings to upstream maintainers
   as a community-hardening effort and capability demonstration.
2. **Bug-finding capability widely available below the frontier.**
   Guido on AIxCC: by August 2025, "10 to 20 different bug-finding
   systems… could find orders of multitude more bugs than we could
   patch." Becker: time to find a high-impact zero-day in a new
   codebase has gone from weeks/months to hours.
3. **Time-to-exploit collapse.** Moussouris: "From the time a
   vulnerability is announced to the time where there is exploit code
   available has now shrunk to pretty much zero."
4. **Patch publication as a disclosure-amplification vector.** Same
   source: a published patch makes it easier for attackers to
   reverse-engineer the bug fix and exploit unpatched downstream
   installations.
5. **Long-tail target shift.** Guido: "You can write exploits for
   software that only one company has… for software that exists in
   only one configuration that one company has. And you can do it on
   the fly." Lowered effort cost makes per-target bespoke exploitation
   economically viable.
6. **Open-weight as the actual attacker capability floor.** Becker:
   "sophisticated threat actors would be far more likely to run their
   own deployments to prevent the exploits from being exposed on
   Anthropic or OpenAI servers." Anthropic's Cyber Verification Program
   and Claude Opus 4.7 safeguards are not the ceiling on attacker
   capability; whichever open-weight model is closest to frontier sets
   that floor.
7. **Adoption-friction counter-argument (Saxe).** Joshua Saxe of
   Security Superintelligence Labs argues attacker-tooling adoption
   curves are sociotechnical, not capability-driven, and may lag
   considerably: "There's a whole human and organizational element
   here… it might be that the adoption curve is quite slow."
8. **Cox's framing (paraphrase).** "Be ready to patch your systems,
   repeatedly, for at least the next 12 months." Treated here as
   external corroboration of the patchpocalypse framing; Cox's
   load-bearing observation about LLM decompilation is in the
   companion entry.

## What this reinforces

### Era-boundary trigger condition is now met

The 04-14 deferral language listed three independent triggers; 04-22
preserved the deferral one cycle on the strength of one of them
(partner-scale operational deployment). With Xint added as a credible
non-Anthropic vendor offering equivalent capability, the second trigger
is also met. The conjunction is sufficient.

**Recommended action: promote to an era-boundary review in
`trust-model.md` §Temporal Trust Boundaries.** That review is a heavier
edit than this entry should attempt — it should land in its own commit,
informed by both 04-28 entries plus 04-22 and 04-14. This entry records
the trigger condition as met; it does not write the era change itself.

### Forgery-resistance hierarchy continues to hold (now under multi-vendor pressure)

The 04-22 reading — that the dangerous capability is *plausible-backdoor
production at speed* rather than novel vulnerability discovery — still
holds. Multi-vendor parity does not introduce new vulnerability
classes; it makes the same capability cheaper and removes any
single-vendor concentration risk. The top of the hierarchy
(institutional affiliation, long tenure, cryptographic material)
remains the durable defense. The bottom (code style, commit messages,
CI hygiene) is under increased pressure from a second offensive vector.

### Long-tail positioning sharpens further

Guido's quote on per-target bespoke exploitation is the most explicit
external articulation yet of the threat-economics shift signatory was
built around. The 04-14 strategic-positioning observation
(`vision.md`:67-69 — the maintainer "with 50 stars and 10,000
dependents") is reinforced from the attacker-economics side: lowered
effort cost makes that maintainer a viable target who was not
previously economic to attack. Worth citing in `vision.md` §Motivation
on next revision.

### Asymmetric signal federation (still correct)

Multi-vendor parity strengthens the 04-14 rejection of vendor-tier
positive trust. With multiple vendors offering frontier-grade
capability through different access regimes, no single vendor's tier
or KYC program can be a meaningful trust input. The
federated-approve-list failure mode worsens with each additional
vendor.

## New observation: time-to-patch is now a dual-edged signal

The signal model implicitly treats a maintainer's time-to-patch as a
positive signal — fast patching = responsive maintainer = good. The
Verge piece reframes patch publication as also being a
disclosure-amplification event for downstream consumers who haven't
yet updated.

For signatory's signal model this means:

- Maintainer time-to-patch remains a positive signal *for the
  maintainer*.
- Downstream consumer time-to-update becomes a first-class operational
  concern that signatory does not yet model. The window between a
  patch's publication and a consumer's adoption is now an
  exploit-recipe window, not a "should patch eventually" window.
- For projects with frequent disclosures, "advisory cadence" and
  "patch-to-adoption distribution" are interpretable as project-quality
  signals — but they are not signals about the package itself; they
  are signals about the *deployment* of the package. Out of v0.1 scope;
  worth noting for the post-v0.1 schema.

This is not a v0.1 signal change — it is a sharpening of how existing
time-to-patch evidence should be interpreted, and a flag for post-v0.1
signal-set expansion.

## New observation: open-weight self-hosted models as the threat-actor capability floor

Vendor-side abuse monitoring (Anthropic's safeguards, OpenAI's TAC
verification, Cyber Verification Programs) cannot constrain attackers
running their own open-weight deployments. The 04-14 entry's rejection
of vendor-mediated KYC as a positive signal is the consumer-side
articulation of this; the Verge piece adds the threat-actor-side
articulation: **assume attackers have access to capability roughly
equal to whichever open-weight model is closest to frontier, with no
abuse monitoring and no rate limiting**.

Practical implication: signatory should not encode any assumption that
the ceiling on attacker capability tracks vendor-published frontier
models. The attacker capability ceiling is set by the *open-weight*
frontier, which trails the closed-weight frontier but is the operative
constraint for adversary modeling. Signal definitions that depend on
"the attacker probably can't do X with current models" should be
written against the open-weight frontier, not the absolute frontier.

## New observation: empirical question — adoption-curve speed

Saxe's contrarian framing is the only falsifiable empirical claim in
the piece: do attackers actually adopt new tooling at roughly the
rate capability becomes available, or with significant sociotechnical
lag? Both ends of this distribution are present in the same piece
(Guido predicting end-of-2026 catastrophe; Saxe predicting slow
adoption). Signatory cannot resolve this from inside the trust model;
it must be tracked as an empirical question against actual incident
data over the next several quarters.

If adoption is fast (Guido is right): signal-design urgency is highest;
the 12-month horizon for v0.1 is roughly the available planning
horizon.

If adoption is slow (Saxe is right): the bounded-audit-surface posture
in `vision.md` has more runway than consensus framing suggests, and
signatory's incremental signal-set evolution can outpace operational
threat realization.

The directional bet inside signatory's model should be conservative
(assume Guido) while the operational evidence collected through
dogfooding over the next two quarters is the resolution criterion.
Worth recording as an open question.

## Adversarial parse of the article itself (per 04-17 discipline)

- The Verge piece quotes vendors with commercial interests in the
  "tidal wave" framing. Trail of Bits, Theori, Luta Security, and
  Security Superintelligence Labs all have offerings positioned for
  the expected demand. The *direction* of their claims is consistent
  across competing incentives (Theori sells the bug-finder; Luta
  sells the triage practice; Trail of Bits sells the consultancy).
  The *magnitude* of any single quote ("everything on fire by end of
  2026") is calibrated for press impact.
- Theori's Xint parity claim is a vendor capability claim, not yet
  independently verified. The 04-17 discipline applies: record the
  fact that Xint exists and that Theori publicly claims parity; do
  not ingest the parity claim as ground truth until independently
  observable. The threat-model implication of Xint *existing* and
  being publicly demonstrated against open-source codebases is
  significant regardless of whether the headline parity claim holds —
  even partial parity meets the 04-14 trigger condition.
- Saxe is the contrarian voice and is also a vendor (CTO/cofounder of
  Security Superintelligence Labs); his framing is differentiation
  marketing as much as analysis. Discount accordingly, but his
  empirical question is real.

## What this does *not* do

### Does not write the era-boundary change

Recommended in this entry, but the actual edit to `trust-model.md`
§Temporal Trust Boundaries is a separate commit informed by both
04-28 entries plus 04-22 and 04-14. This entry records the trigger
as met.

### Does not add Xint as a named capability in signal definitions

Per 04-22's discipline. Signal definitions should refer to the
underlying capability ("LLM-assisted vulnerability discovery at
human-researcher parity, deployable at scale"), not vendor product
names that will date instantly.

### Does not promote "patchpocalypse" as a v0.1 signal

The patch-tsunami / triage-bottleneck framing is a defender's
operational concern, not directly a trust signal about a target. It
informs what consumers do with signatory's output, not what signatory
collects. Worth keeping in mind when designing CLI ergonomics and
prioritization helpers, but not a new signal class.

### Does not endorse Saxe's slow-adoption framing

Records it as a falsifiable open question. Default operational posture
remains conservative.

## Open questions added to `design/open-questions.md`

- What is the specified mechanism for promoting an era boundary based
  on cumulative threshold conditions (multiple triggers met) rather
  than a single anchor event? The 04-14 entry framed era boundaries
  as anchored to single frontier releases; the multi-trigger pattern
  this entry instantiates is new.
- Should signatory model "downstream patch-adoption window" as a
  post-v0.1 signal class, distinct from maintainer time-to-patch?
- How does the signal-design horizon adjust if open-weight frontier
  capability becomes the operative attacker-capability assumption,
  rather than tracking closed-weight frontier?
- How will signatory empirically track Saxe's "adoption curve"
  question over the next two quarters of dogfooding, and what
  observation would resolve it in either direction?

## Cross-references

- [`2026-04-28-llm-decompilation-binaries-as-source.md`](2026-04-28-llm-decompilation-binaries-as-source.md)
  — same-day parallel entry; CVE-2026-3854 / Wiz / Russ Cox reading;
  closed-binary trust posture
- [`2026-04-22-mozilla-anthropic-firefox-mythos.md`](2026-04-22-mozilla-anthropic-firefox-mythos.md)
  — partner-scale operational datapoint that this entry's multi-vendor
  parity claim joins to clear the era-trigger condition
- [`2026-04-17-vendor-communication-as-signal.md`](2026-04-17-vendor-communication-as-signal.md)
  — sotto voce discipline applied to Verge sources
- [`2026-04-14-openai-tac-gpt54-cyber.md`](2026-04-14-openai-tac-gpt54-cyber.md)
  — era-deferral trigger language this entry's multi-vendor observation
  satisfies
- [`../vision.md`](../vision.md) §Motivation — long-tail positioning
  reinforced from the attacker-economics side
- [`../trust-model.md`](../trust-model.md) §"Temporal Trust Boundaries"
  — era review recommended in a follow-up commit
- [`../trust-model.md`](../trust-model.md) §"Signals must be weighted
  by forgery resistance" — hierarchy unchanged; pressure on bottom
  tier increased
- [`../open-questions.md`](../open-questions.md) — questions above
  tracked there for resolution
