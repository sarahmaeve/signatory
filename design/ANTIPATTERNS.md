# Signatory: Warning Lights and Antipatterns

A running list of things that, if seen in signatory's design, implementation, or
usage, indicate the tool is drifting away from its threat model. Some of these
are existential; some are "notice and course-correct." The common pattern is
that each one resolves an architectural or principled tension in the wrong
direction — usually toward centralization, endorsement, or laundering of
judgment.

Cross-refs:
- [`trust-model.md`](trust-model.md) — the framework these antipatterns violate
- [`vision.md`](vision.md) — design principles: analysis is the product, complement don't compete
- [`threat-landscape/2026-04-14-openai-tac-gpt54-cyber.md`](threat-landscape/2026-04-14-openai-tac-gpt54-cyber.md) — asymmetric signal federation; burns federate, approvals don't

## Architectural drift (re-centralization by another name)

- A `signatory.io` hosted anything. Enrichment API, signal sync, posture backup —
  any of them means signatory has become the trust root it warned about.
- An `api_key` field appearing anywhere in config.
- Telemetry "to improve the tool." Posture data is the single most sensitive
  dataset signatory produces — a complete map of a user's dependency hygiene.
  It must never leave the local box by default.
- A caching layer operated by anyone other than the user. "We cache GitHub API
  responses for you" is how it starts.
- The MCP server accepting network connections instead of local-only transport.
- A default burn-list subscription URL shipping in the binary. Even a
  "recommended" list violates the federation asymmetry principle.
- Any form of cross-org *approve* list, in any framing — "verified publishers,"
  "trusted maintainers," "signatory seal of approval." The threat-landscape doc
  is categorical on this and the rule is load-bearing.

## Vocabulary drift

- The word "score" appearing in user-facing output. Signatory presents
  *signals*; scoring is the consumer's job.
- "Risk: low/medium/high" bucketing replacing raw signal presentation.
- Percentage confidence figures with no defined methodology behind them. Fake
  precision is worse than qualitative language.
- Verbs like *verified*, *certified*, *approved* attached to packages. These
  are endorsement words, and signatory is not in the endorsement business.
- Layer-1 observations and Layer-2 conclusions blurring together in output.
  The distinction (analysis vs. serialization) is load-bearing — if users can't
  tell mechanical evidence from reasoned interpretation, the framework
  collapses.
- "AI-detected" anything presented without the underlying signal.

## Feature creep (violating "complement, don't compete")

- A vulnerability scanner. Even a small one. Even "just CVE lookup."
- A `signatory scan` subcommand, or any CLI primitive that runs
  `/analyze` across every dep in a manifest. Signatory is a
  *decision-moment* tool (see [`analysis-economics.md`](analysis-economics.md)
  §7 and [`v0.1-invariants.md`](v0.1-invariants.md) non-goals). Per-install /
  per-row-of-the-lockfile invocation is uneconomic at current model
  prices *and* a design mismatch: the tool produces most value when
  a human-plus-LLM pair has a specific adoption or re-vet decision
  in hand. Shipping the primitive ("but it'd be easy to add") invites
  scanner-style adoption the architecture will then bend to justify.
- Auto-remediation — "signatory can fix your go.mod."
- License compliance as a first-class feature. Scope expansion into the
  legal-tech adjacent space is a well-trodden path to becoming another
  generic supply-chain product.
- "Policy as code" that replaces human judgment with YAML.
- An enterprise tier. The moment there is a "Pro" version, the architecture
  bends to justify the price discrimination.
- SBOM generation. Other tools do this; signatory should consume SBOMs, not
  produce them.
- A CI action called `signatory-gate` that fails builds on score thresholds.
  This is users doing the wrong thing, but shipping the primitive for it is
  an antipattern on our side.

## Dogfooding failures

- A new dependency added without an entry in [`dogfood/`](dogfood/).
- Unsigned commits from any contributor, including LLM-authored ones.
- Release artifacts that aren't reproducible from source.
- Binary blobs appearing in the repo without declared rationale (the exact
  signal the threat-landscape doc flagged as negative-by-default).
- `signatory analyze signatory` producing anything embarrassing — and no one
  noticing, which is worse.
- Transitive dependency count creeping up. Zero runtime transitives is a
  statement; the moment it becomes "zero, except…" the statement is over.
- Test coverage of the attack case studies (`example-*.md`) decaying from
  live test cases into doc-ware.
- The adversarial-testing principle drifting into "we'll get to it."

## Usage antipatterns (how the ecosystem leans on it wrong)

- Running signatory across an entire dependency tree as a batch
  scan. At ~200k tokens per target floor (see
  [`analysis-economics.md`](analysis-economics.md) §1), fanning out
  over even a medium `go.mod` or `package-lock.json` is tens of
  millions of tokens of analysis to learn things mostly below the
  economic break-even. Signatory is invoked at *decision moments*
  — pre-adoption, version bumps, incident triage, shopping between
  alternatives — not as a pass over the closure. Solo-dev usage is
  the sharp case: tokens come out of the operator's own quota, so
  scanner-style use is uneconomic before it is undesirable.
- LLMs citing signatory output as authoritative in PR descriptions:
  *"Approved this dep — signatory says it's fine."* This is laundering
  judgment through a tool explicitly designed not to provide judgment.
- Agentic workflows auto-approving dependencies based on signatory output with
  no human in the loop. The tool is supposed to *inform* the human + LLM pair,
  not replace the human.
- Cyber insurance policies requiring "signatory-clean" dependency trees. Once
  underwriters key off the output, the incentive structure inverts — signals
  optimize toward whatever the insurer checks, not toward actual trust.
- Courts or auditors treating signatory reports as evidence of due diligence.
  Legal load-bearing use pulls the tool toward defensible-in-court outputs,
  which is a very different design target from useful-to-a-developer.
- Package authors tuning their repos to "look good to signatory." This is
  Goodhart on arrival.
- A `signatory-good` badge for READMEs. The badge economy is how trust signals
  get hollowed out.

## LLM-integration specific

- The MCP surface growing write operations. Read-only is load-bearing — the
  moment LLMs can `signatory_burn` or `signatory_attest`, attestation
  provenance degrades from "human decision" to "model output" without any
  architectural change flagging it.
- Signatory output appearing in LLM context as a replacement for reading the
  code, not a prompt to read it. If the agent's workflow is
  `signatory says ok → skip review`, the tool has made things worse.
- A "vibe check" output that condenses signals into a sentence the model can
  just quote. Summary outputs that preserve no path back to raw evidence are
  laundering.
- A hosted model fine-tuned on analyses ("Signatory GPT" or equivalent). That
  is the posture-data-as-training-data failure mode, and it is irreversible.

## Adversarial adaptation (Goodhart and attacker learning)

- Attackers publishing packages with deliberately signatory-friendly profiles:
  long synthetic tenure, signed commits from a fabricated identity, clean CI
  config, spotless publication metadata. The forgery-resistance hierarchy
  should hold up, but the moment a real attack is tuned to the signal set,
  every signal in that attack needs its weighting re-examined — not just the
  ones that failed.
- Evidence that projects are *removing* genuine signals (e.g., dropping a
  less-common CI for a signatory-recognized one) to improve their profile.
  Means the tool is shaping the ecosystem rather than observing it.
- Signatory becoming a recon tool for attackers — *"which high-criticality
  package has the weakest maintainer identity signals?"* The output surface
  has to assume adversarial readers.

## Governance and business drift

- Signatory incorporation as a company. Not necessarily fatal, but the moment
  there is a cap table, the architecture starts drifting toward whatever
  justifies the valuation.
- A Foundation. Foundations are where principled tools go to have their
  principles negotiated away by sponsor committees.
- A "Head of Trust Intelligence" hire at any org that consumes signatory
  output. That role's KPIs will be *compliance throughput*, not *actual
  trust*, and the tool will be pushed to serve them.
- Funding announcement. VCs will want a defensible moat, which means
  centralization, which means this whole document becomes the story of how
  it went wrong.
- A glossy marketing site before v1. Rule of thumb: the
  marketing-polish-to-code-polish ratio is a leading indicator of a project
  optimizing for the wrong reader.
- Advisory board members from package registries or AI labs. Captured inputs
  produce captured outputs.

## Data model and methodology drift

- Analyses saved without the raw evidence that produced them. If a conclusion
  can't be re-derived from signals, it is unfalsifiable and can't be audited.
- Synthesis outputs that don't version the synthesis prompt or model. A
  conclusion from a 2026-04 Opus run and a 2027-02 Sonnet run are different
  artifacts; collapsing them loses information.
- The handoff schema gaining "confidence score" fields that analysts fill in
  without a defined methodology. Human analysts and LLM analysts will both
  pattern-match if asked for confidence without a rubric.
- The entity-profile schema growing attributes that can't be independently
  verified. Every field should have an answer to "who could forge this, and
  at what cost."
- "Legacy" or "deprecated" signal types accumulating without removal. The
  signal set is load-bearing — drift in it is drift in the framework.

## Meta-warning: things other people build *on top* of signatory

Signatory can be principled and still enable antipatterns if the primitives
are too generic.

- A SaaS company offering "managed signatory" that syncs posture across teams.
  Their architecture will become what signatory refused to be, and users
  won't distinguish.
- An aggregator site showing signatory scores for the top 10k packages. Once
  it exists, packages optimize for it, and signatory's raw-signal-presentation
  stance gets routed around.
- A plugin ecosystem that adds "enrichment sources" — rapidly becomes a
  supply chain attack against signatory consumers itself.

The best defense against the meta-warning is probably that the v0.1
architecture makes the centralized versions *annoying to build* — local
SQLite, no shared schema conventions, no stable public API. Rough edges that
discourage the wrong abstractions are a feature.

## The leading indicator

If a single signal had to go on the dashboard: **the word "score" entering
user-facing output, or the word "verified" entering marketing copy.** Both
are small, both are reversible individually, and both are what every
centralized trust vendor eventually converges on. Seeing either is the cue
to re-read [`trust-model.md` §"Signals must be weighted by forgery resistance"](trust-model.md)
and figure out where the pressure came from.
