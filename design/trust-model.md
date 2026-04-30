# Signatory: Trust Model

## Framing

The signal model operates under a limited-trust, counterintelligence lens.
Trust is provisional, degradable, and temporally bounded. The goal is not to
prove code is safe — it's to surface reasons for suspicion and make trust
decisions explicit and auditable.

## Temporal Trust Boundaries

Code provenance is interpreted differently depending on when it was committed.
Three key dates define four eras. The first three boundaries anchor
*authorship-capability* claims (could the author have written or reviewed
this code with AI assistance at this date?). The fourth — at 30 April 2026
— is a *target-durability* claim; see Era 4 for the framing shift.

### Era 1: Pre-LLM (before 30 November 2022)

- **ChatGPT 3.5 release date** marks the beginning of meaningful LLM interaction
  with code.
- Code from this era is guaranteed to be human-authored.
- However, it has never been reviewed by modern AI methods — vulnerabilities that
  a Mythos-class model would find instantly may be hiding in plain sight.
- Fallow code from this era is a particularly negative signal: old, unreviewed,
  and unmaintained.

### Era 2: Early LLM (30 November 2022 — 24 November 2025)

- LLMs could assist with code generation and review, but capabilities were
  limited.
- Code may or may not have AI involvement — the uncertainty zone.
- Trust signals from this era require more contextual interpretation.

### Era 3: Modern AI (24 November 2025 — 30 April 2026)

- **Claude Opus 4.5 release date** marks the threshold of current code generation
  abilities.
- Assume AI could have authored or reviewed any code from this era.
- Provenance of the *author* matters more than provenance of the *code* itself.
- Sophisticated, subtle backdoors become plausible at scale.

### Era 4: Mature Cyber (after 30 April 2026)

- **Anchor: 2026-04-30** — UK AISI's independent evaluation of GPT-5.5
  cyber capabilities (the first non-vendor measurement of multi-vendor
  frontier parity). Greg Kroah-Hartman on oss-security the same day
  reports duplicate independent vulnerability finds within the Linux
  kernel's fix-merge window. See
  [threat-landscape/2026-04-30-aisi-gpt55-evaluation.md] and
  [threat-landscape/2026-04-30-coordinated-disclosure-collapse.md].
- **The boundary's meaning shifts here.** Earlier era boundaries anchor
  authorship-capability claims. The mature-cyber boundary is a
  **target-durability** claim — code unexamined and unrepaired through
  the maturation phase has surfaces that will not weather current attacks
  well.
- A `mature-cyber` stamp on `last_push` or `last_commit` is a positive
  durability signal — the project has been touched since multi-vendor
  frontier offensive cyber capability went public. A `modern-ai` (or
  earlier) stamp on `last_push` for a still-depended-on project is the
  audit-debt concern.
- Coordinated-disclosure embargo as a defensive posture is collapsing
  under LLM-mediated duplicate-find rates. Even the Linux kernel barely
  outruns the merge window; long-tail projects are past that threshold
  by construction. The signal model reads CVE history, vitality
  cadence, and disclosure-practice posture against this operational
  reality.

**Note:** These dates are defaults. Whether they should be configurable
per-deployment is an open question.

## Signal Principles

### Code Hygiene

Code that shows zero safety tooling usage — no linters, no format checkers,
no CI — is suspect regardless of when or how it was generated. Absence of
basic hygiene is a negative signal independent of era.

### Reviewer Provenance

- Code approved by known-good human reviewers (industry figures, maintainers
  at major organizations) has higher-valued provenance.
- This trust does **not** transfer through forks. A fork of well-reviewed code
  by an unvetted party is *less* secure than the original, not equivalent —
  because it inherits reputation while potentially introducing unreviewed changes.

### Fallow Code

A project that is entirely static for an extended period is a negative signal,
particularly if the code predates 2024. This indicates:
- No one is looking for vulnerabilities
- No one is applying patches
- Dependencies may be outdated

However, whether fallow code is a problem is **not an intrinsic property of the
code — it's determined by the consumer's relationship to it.** The same
unmaintained library is "vetted and frozen" for one org and "unknown provenance"
for another. See Dependency Posture Tiers below.

### Criticality (Dual-Edged)

Criticality — measured by install base, dependents, and breadth of adoption —
is not a simple positive or negative signal. It is a **multiplier** that
amplifies both trust and risk:

- **High criticality + stable signals** = high confidence, but also a
  high-priority monitoring target
- **High criticality + any anomaly** = immediate alert, regardless of how
  small the anomaly appears in isolation
- **High criticality + degradation** = organizational emergency

The monitoring threshold should be **inversely proportional to criticality**.
The more critical a dependency is, the smaller the anomaly needed to trigger
investigation.

Wide install base and active contributors are initially positive signals.
However, they also mean the project is a higher-value target for
sophisticated attackers (including nation-state actors — see
[example-axios-attack.md]), and as AI capabilities increase, AI-generated
contributions may be indistinguishable from legitimate ones.

### Adoption Type: Direct vs. Transitive

Raw adoption counts (go.mod references, npm downloads) don't distinguish
between developers who *chose* a package and those who *inherited* it as
a transitive dependency. The **refs-to-stars ratio** provides a heuristic:

| Ratio (refs/stars) | Interpretation |
|--------------------|---------------|
| < 1 | Mostly direct adoption — developers actively evaluate and select this package |
| 1–5 | Mix of direct and transitive |
| > 10 | Mostly transitive — pulled in by a popular parent, rarely chosen directly |

Transitive-only adoption is a weaker trust signal than direct adoption.
A package with a 50:1 ratio is in many dependency trees but few humans
have independently evaluated it — they inherited it without choosing it.

Validated against signatory's own dependency evaluations:
- mousetrap: 54:1 (transitive via Cobra — rejected)
- kong: 0.66:1 (direct adoption — trusted-for-now)
- testify: 3:1 (strong direct adoption — trusted-for-now)

### Commit Activity Patterns

Last commit date alone is insufficient. Two additional signals refine
the vitality assessment:

- **Total commit count relative to age.** A 12-year-old project with 10
  commits (mousetrap) is qualitatively different from an 8-year-old
  project with 467 commits (kong). Low total count indicates write-once
  code.
- **Activity distribution.** Years of silence followed by a burst may
  indicate abandonment and brief revival. The nature of the burst matters:
  substantive work (features, fixes) is a stronger signal than cosmetic
  cleanup (updating build tags, adding go.mod).

See [example-axios-attack.md] for a case study of how criticality amplified
the impact of the axios npm supply chain attack (March 2026).

## Dependency Posture Tiers

Organizations need to record deliberate trust decisions about their
dependencies. Signatory tracks these as **organizational attestations** —
part of the visibility layer, not deferred to the attestation layer.

| Tier | Description | Investment | Signal strength |
|------|-------------|------------|-----------------|
| **Vetted & frozen** | Org has reviewed a specific version, repaired if necessary, and explicitly locked it with a signed attestation. "Version 1.5 is what we use until we update." | Review + repair + cryptographic signature | Strongest signal in the system. Unfakeable without compromising the org's signing keys. |
| **Trusted-for-now** | Well-known codebase, trusted over alternatives until vetted or replaced. "We will trust this for now, more than other options." | Judgment call, no deep review | Provisional positive. Acknowledged risk, pending deeper review. |
| **Unexamined** | Old code, no organizational decision has been made. "Hasn't been added to in 7 years — needs examination at minimum." | None | Negative. Default state for legacy dependencies. |
| **Unknown provenance** | Old fork of old code. "No idea who made this or why. No value over what we can repair or generate ourselves." | None | Strongly negative. Candidate for replacement. |

These tiers represent organizational state that signatory manages. They
cannot be derived from external signals alone — they require an explicit
decision by the consuming organization.

### Signed Attestation as the Highest-Value Signal

The most expensive and highest-quality signal in signatory's model is a
**signed attestation from a vetted source** — preferably from your own
organization, or from an individual or team's own work to review, repair,
and attest their code.

This is the signal that closes the investigation-to-attestation loop:

1. Analysis surfaces unexamined or degraded dependencies
2. A human or human-supervised team reviews and repairs the code
3. They sign an attestation recording that work
4. The dependency moves to "vetted & frozen" with the strongest possible
   trust signal

This signal is the most forgery-resistant in the system because it requires:
- Your organization's own cryptographic material
- Actual review work by a vetted human or team
- A deliberate decision recorded with accountability

As AI reduces the cost of producing cheaper signals (code style, commit
messages, PR descriptions), signed organizational attestation becomes
*relatively more valuable* over time. It is the one signal whose
production cost cannot be collapsed by AI — it represents genuine human
judgment and institutional commitment.

### Purpose

These vetting, provenance, and scoring methods are designed to allow humans
and LLMs to navigate existing open-source projects and codebases in a more
rational framework — replacing the current default of trusting whatever is
top-of-stack or appears first in a search engine.

## Trust Revocation ("Burning")

Signatory must support the ability to **burn** projects and user signatures
when they become compromised. Burning degrades all trust signals and provenance
associated with the burned entity — analogous to:

- An auction house found trafficking in forgeries
- An intelligence operative confirmed as a fabricator
- A certificate authority whose root key is compromised

Burning should:
- Retroactively degrade signals for the burned entity
- Propagate to dependents (anything that depends on a burned project inherits
  degraded trust)
- Be auditable (who burned what, when, and why)

### Burn Governance: Federated Model

Burning is **local-first and federated**, not centralized:

- **Each organization maintains its own burn list** with its own criteria.
- **Organizations can subscribe to another org's burn list**, particularly
  within a hierarchy (e.g., a subsidiary subscribing to a parent company's
  list, or an open-source project subscribing to a foundation's list).
- **Subscription is itself a signal.** Accepting a hierarchical organization's
  burn list is a positive attestation — it means you trust their judgment.
- **Subscription is optional and revocable.** A contrary analyst or thorough
  security inspector might deliberately ignore inherited burns as a
  counterintelligence exercise — testing whether the hierarchy's assessments
  are accurate or whether they create blind spots.

### Architectural Consequences of Federated Burns

- **Burns and signals are separate layers in the data model.** A burn overlays
  signal data; it does not replace or modify the underlying signals. Raw
  signals remain accessible regardless of burn state.
- **Each burn entry tracks its source** — local decision vs. inherited from
  a subscribed organization.
- **The query layer supports filtering by burn source:**
  - All burns applied (default view)
  - Raw signals only, ignoring all burns
  - Only burns from a specific source
  - Only locally-originated burns, stripping inherited ones

**Remaining open questions:**
- How does burning propagate through dependency trees?
- Is there an appeal or reinstatement mechanism?
- Should burning be graduated (warning, degraded, revoked) or binary?
- What is the subscription/sync protocol for inherited burn lists?

## Core Principles

1. **Trust is multi-signal and compositional.** No single signal is definitive.
   Trust is a composite of independently verifiable signals, each with its own
   forgery difficulty and decay characteristics. This applies to identities,
   code patches, and projects alike. See [example-identity-analysis-rsc.md]
   for a worked example.

2. **Trust accumulates slowly but degrades fast.** It takes years to build a
   strong trust profile. A single confirmed compromise can invalidate a
   significant portion of it. The system must respect this asymmetry.

3. **Degradation is often retroactive.** Compromises are discovered after the
   fact. The system must support re-evaluating a window of history: "re-score
   everything this entity touched between date X and date Y." Burns against
   an identity trigger retroactive signal degradation across everything they
   reviewed or approved.

4. **Entity profiles are a first-class concept.** The ability to generate and
   maintain profiles — for humans, LLM interactions, code patches, and other
   actors — is a core capability, whether produced internally by signatory or
   consumed from an external source. Profiles are the substrate on which trust
   decisions are made.

5. **The compositional model extends to patches.** Identity provenance will be
   one input to patch trust, alongside code hygiene, temporal era, review
   chain, and other signals. The identity model and the patch model should
   share the same compositional framework.

6. **Signals must be weighted by forgery resistance, not just
   informativeness.** AI has dramatically reduced the cost of producing
   previously expensive signals. Code that looks idiomatic, well-written
   commit messages, plausible PR descriptions, consistent coding style —
   these used to be reliable because faking them required genuine expertise.
   AI makes them cheap to produce. The prt-scan campaign already generates
   language-aware payloads matching repository conventions.

   Signals that remain durable are those that require real time, real
   institutions, or real cryptographic material to produce:

   | Forgery resistance | Signal type | Why |
   |--------------------|-------------|-----|
   | Very high | Institutional affiliation (verified) | Requires compromising the org |
   | Very high | Long tenure (years of commit history) | Requires years of real time |
   | Very high | Cryptographic signatures (GPG/SSH/OIDC) | Requires key compromise |
   | High | Cross-platform identity consistency | Requires compromising multiple platforms |
   | High | Publication metadata / trusted publishing | Requires CI pipeline compromise |
   | Medium, declining | Code hygiene (linters, CI config) | AI can generate this |
   | Low, declining | Code style, commit messages, PR descriptions | Trivially generated by AI |

   The bottom of this table used to be useful — it is becoming noise.
   Signatory should still collect these signals (they contribute to the
   composite picture), but the weight of trust must shift toward
   forgery-resistant signals as AI capabilities increase. This shift
   will accelerate.

## Implications for Architecture

1. **Timestamps are first-class signals**, not just metadata. The interpretation
   of every other signal depends on which temporal era the code falls in.

2. **Trust is mutable state.** The data model must support retroactive
   degradation of trust signals. This is not a static scorecard.

3. **Fork genealogy matters.** Signatory must track relationships between
   repos (upstream, fork, vendored copy) and not assume trust transfers
   through those relationships.

4. **Identity is load-bearing.** Identity trust is multi-signal and
   compositional. See [example-identity-analysis-rsc.md] for the signal
   framework and degradation model.

5. **Entity profiles are a core data type.** The data model must support
   profiles for multiple entity types (human, LLM, patch, project) with
   a shared compositional signal framework.
