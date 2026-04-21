# 2026-04-21: Vercel / Context.ai Incident — Identity-Surface Exposure as a Signal Axis

## Source

Vercel Security Team bulletin, dated 2026-04-21 (last updated), reporting
an incident originating in Context.ai — a third-party AI tool used by a
Vercel employee — whose Google Workspace OAuth app was compromised. The
attacker used that access to take over the employee's Vercel Google
Workspace account, gaining access to some Vercel environments and to
environment variables not marked as "sensitive." Vercel's assessment,
corroborated with GitHub, Microsoft, npm, and Socket, is that
Vercel-published npm packages were *not* compromised.

## Why this entry exists

The incident exercises an attack path the v0.1 signal set does not
currently model. Compromise of a third-party SaaS integration with broad
Google Workspace OAuth consent propagated through identity to reach
Vercel's internal environments and was credibly positioned to reach the
package-publish surface. npm packages being clean is an outcome of
Vercel's containment work, not of the compromise shape being bounded at
a lower layer.

This entry names the orthogonal axis the attack traveled on — *identity-
surface exposure* — proposes the signals that would observe it, and
records the implication for posture-tier semantics that dogfood analyses
have been converging on anyway.

## The attack shape

Canonical four-hop:

1. Third-party SaaS (Context.ai) holds broad Google Workspace OAuth
   consent granted by one or more Vercel Workspace users.
2. Context.ai is itself compromised; the attacker holds consented-token
   access to its Workspace integrations.
3. Token → full account takeover of a Vercel employee's Workspace
   account.
4. Workspace account → Vercel internal systems and non-sensitive
   environment variables.

The supply-chain layer (npm) was *adjacent* to the traversed path, not
traversed. The bulletin notes Vercel *validated* npm packages were clean
in collaboration with GitHub, Microsoft, npm, and Socket — meaning the
attacker was in position to attempt it and the integrity conclusion is
the output of active containment work, not a structural bound.

## What this reinforces

### Identity verification is necessary but not sufficient

The v0.1 "Identity and Governance" signal group scores a maintainer via
forgery-resistant signals: tenure, commit signing, cross-surface
consistency, org affiliation. These answer "is the maintainer who they
say?" They do not answer "how many attackable third-party consent grants
does the maintainer's identity intersect?"

A verified identity with a sprawling OAuth consent graph is lower-trust
than its identity score alone implies. The Vercel employee whose account
was taken over was presumably a real, long-tenured engineer whose
identity would have scored high on every forgery-resistance axis we
currently measure.

### Forgery-resistance hierarchy gains a new top-level axis

The 04-17 entry noted pressure on the *bottom* of the forgery-resistance
hierarchy as capability expands. This entry observes a *new axis at the
top*: identity-surface exposure is orthogonal to identity verification
and deserves its own signal group.

Hardware keys + DMARC reject + Workspace-admin-revocable OAuth consents
is a materially different posture from personal Gmail + SMS 2FA + opaque
OAuth consent log, *independent of* any identity forgery-resistance
score. Both maintainers may pass every identity check; only one survives
a Context.ai-class SaaS compromise.

### Supply-chain compromise and identity compromise are converging

Package takeover patterns (`event-stream`, `ua-parser-js`, `colors.js`)
historically centered on direct maintainer phishing or social-engineered
handover. The Context.ai shape is categorically different: the
maintainer is not phished, does nothing wrong, is never tricked, and may
never notice. The compromise arrives through a third-party SaaS that a
majority of users in a Workspace may not even know is integrated.

The defensive implication is that supply-chain observability has to
extend past the publish surface into the publisher's identity
integration surface. Signatory is well-positioned to do this — most
raw inputs are DNS-observable, registry-observable, or repo-observable —
but the v0.1 signal set does not yet collect them.

## New axis: identity-surface exposure

Signals in this axis observe *how attackable the maintainer's identity
surface is, independent of how verified the identity is.* Core
questions:

- **Publish-authority structure.** Is publish authority bound to the
  identity alone (single-key; the Context.ai failure mode) or split
  across an identity + a separate surface (trusted publisher via repo
  OIDC; requires compromising two independent surfaces)?
- **Identity-provider strength.** What is the maintainer's primary
  commit-email domain? Personal webmail vs. corporate Workspace with
  admin revocation has a 10× difference in OAuth-consent-phishing
  survivability.
- **Account-change observability.** Has the publisher set,
  commit-author email, or maintainer list changed recently?
  Post-takeover churn is the leading indicator; all three are
  registry-observable and tamper-evident.
- **Publish-environment fingerprint.** Does the publish metadata for a
  new version match the historical fingerprint of prior publishes, or
  has user-agent / time-zone / IP-class drifted?
- **Third-party automation surface on the repo.** Workflow files
  reference third-party GitHub Actions with `GITHUB_TOKEN` write scope.
  Each is an OAuth-equivalent; the composition is a measure of the
  repo's identity-side attack surface.
- **Self-declared identity-surface posture.** A project publishing an
  attestation — *"we publish only via trusted publisher; maintainers
  use Workspace + hardware keys; no personal-account publish paths
  exist"* — is directly addressing this axis. Like `SECURITY.md`:
  presence positive, absence neutral, contradiction red-flag.

Detailed signal rows land in `signals-v01.md`. This entry records *why
the axis exists*; the signal registry records *what to collect*.

## Impact on posture-tier semantics

Recent dogfood analyses have been drifting toward a distinction the
current tier vocabulary does not name.

`invariant@2.2.4` (2026-04-21, npm) landed at `trusted-for-now` with a
rationale that read, concretely, as *"safe as installed; burn the slot
the moment anything twitches."* Earlier npm fallow-package analyses
(`escape-html`, `msgpack-lite`) surfaced the same ambiguity and the
consumer chose `rejected` to discharge it, accepting the cost of
migration rather than the ambiguity of subscribing to an unnamed
monitoring commitment.

Either outcome is defensible. The problem is that the posture tier does
not encode the monitoring commitment a `trusted-for-now` on a fallow
high-traffic package requires. The Context.ai incident is the
externality that makes this encoding matter: the monitoring-subscription
signal class (account churn, publisher set change, new publish under a
dormant slot, OAuth-app incident disclosures) is exactly what a
downstream consumer needs to observe, and current posture language does
not route that observation to anyone.

The remedy is **not** a new tier. Adding `trusted-with-monitoring`
between `trusted-for-now` and `vetted-frozen` would overload the tier
axis with a separate concept (monitoring commitment) that is orthogonal
to trust level. A package can be `vetted-frozen` and still warrant
twitch-monitoring; a package can be `trusted-for-now` and not warrant
any monitoring beyond the standard cadence.

The remedy is a **composite-signal / view layer over base signals** —
a named derived concept (e.g., `takeover-bait`) that explicitly surfaces
the monitoring commitment, parallel to a SQL view over underlying
tables. Base signals stay as base data; composites name the patterns
consumers subscribe to. `signals-v01.md` grows a "Composite Signals
(View Layer)" section to record the shape.

This framing also resolves the phrasing problem in the dogfood
analyses: the synthesist isn't asked to encode a monitoring commitment
inside a free-text rationale. The composite is present-or-absent, the
monitoring commitment is attached to the composite definition, and the
posture tier goes back to answering the single question it was designed
for: should we install this?

## What this does *not* do

### Does not promote to an era boundary

Consistent with the 04-14 and 04-17 deferrals. This entry documents a
threat-landscape signal class, not a capability inflection. No change
to the temporal era boundaries.

### Does not ingest Vercel or Context.ai post-incident claims as signals

The bulletin's facts (attack shape, four-hop structure, "non-sensitive"
default, npm validation workflow) are recorded as threat-landscape
inputs. Vercel's "we validated npm is clean" and Context.ai's eventual
postmortem are outputs from the same communications function the 04-17
entry discipline applies to. Record, do not weight.

### Does not add Context.ai specifically to any burn list or IOC registry

Per `ANTIPATTERNS.md` §"Architectural drift" and §"Usage antipatterns,"
signatory does not maintain per-vendor IOC lists or curated
positive-trust sets. Context.ai is the illustrative instance of a
pattern. Maintaining a vendor-specific burn list would instantiate
precisely the federated-positive-trust failure mode the 04-14 entry
rejected, applied in the negative direction.

## Open questions added to `design/open-questions.md`

- Should "identity-surface exposure" be a top-level signal group
  alongside "Identity and Governance," or a subsection within it? The
  axes are orthogonal; making it separate improves discoverability but
  adds a group for a small number of signals.
- Should composite signals live in `signals-v01.md` with a
  `composition:` field on the signal row, or in a separate
  `design/composite-signals.md` that cross-references base signals?
- What is the right observability surface for the "has this dormant
  slot twitched?" monitoring commitment — a signatory subscription, a
  Renovate rule, a notification hook in the store, or left to the
  consumer to wire? The answer determines whether composites emit
  events or are polled.

## Cross-references

- [`2026-04-17-vendor-communication-as-signal.md`](2026-04-17-vendor-communication-as-signal.md)
  — parallel "treat vendor framing as signal-to-be-interpreted";
  Context.ai post-incident communication falls under the same
  discipline
- [`2026-04-14-openai-tac-gpt54-cyber.md`](2026-04-14-openai-tac-gpt54-cyber.md)
  — forgery-resistance hierarchy foundation; this entry adds a new
  top-level axis
- [`../signals-v01.md`](../signals-v01.md) — signal-set updates derived
  from this entry (identity-surface exposure group + composite-signals
  section)
- [`../trust-model.md`](../trust-model.md) §"Signals must be weighted
  by forgery resistance" — the hierarchy gaining a new axis
- [`../trust-policy-v1.md`](../trust-policy-v1.md) — v0.2 posture
  evaluator, which will need to know about composite signals
- [`../ANTIPATTERNS.md`](../ANTIPATTERNS.md) — no per-vendor IOC list,
  no federated approve-list, apply in both directions
- `design/analysis/` and `filestore/analysis/` — the dogfood analyses
  that surfaced the posture-tier overloading observation
  (`invariant-2.2.4`, `escape-html`, `msgpack-lite`)
