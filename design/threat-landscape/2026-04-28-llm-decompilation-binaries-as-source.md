# 2026-04-28: LLM Decompilation — Closed Binaries as Source

## Source

Two artifacts:

1. GitHub Security Lab disclosure post for **CVE-2026-3854**, a
   critical RCE in github.com / GitHub Enterprise Cloud / GHES via
   crafted git push options (delimiter injection into internal
   metadata, escaping push-hook sandbox). Reported by Wiz researchers
   via GitHub's Bug Bounty program on 2026-03-04, validated and patched
   on github.com within ~75 minutes, GHES patches issued across all
   supported releases. Full disclosure text reproduced into the
   conversation transcript by the user on 2026-04-28.
2. Russ Cox's commentary on Bluesky on 2026-04-28
   (<https://bsky.app/profile/swtch.com/post/3mklihkliyc2p>), reading
   the disclosure as evidence of LLM-assisted binary decompilation
   (paraphrasing): *"The GitHub remote execution bug is fine and all,
   but the bigger story is the LLM (with IDA) quickly decompiling
   GH's binaries to enable the analysis. A lot of companies shipping
   'closed' binaries are going to learn the hard way that they might
   as well be shipping source."* Bluesky pages do not render
   server-side for WebFetch; full text was reproduced into the
   conversation transcript by the user.

This entry pairs with the same-day
[`2026-04-28-multi-vendor-mythos-parity.md`](2026-04-28-multi-vendor-mythos-parity.md);
the two together are signatory's 2026-04-28 threat-landscape snapshot.

## Provenance gap (record explicitly)

The GitHub disclosure post does not describe Wiz's methodology — it
addresses response, root cause, and remediation. Cox's claim that an
LLM (with IDA) was used to decompile GitHub's binaries is *attributed
to Cox*, presumably from a Wiz technical writeup or other public
material this entry has not directly verified. The threat-model
analysis below treats Cox's reading as a credible-but-unverified
account; the architectural conclusions stand even if the specific
methodology was different, because the *capability* of LLM-assisted
binary decompilation at this fidelity is well-established (see e.g.
the 04-14 entry's coverage of GPT-5.4-Cyber's binary RE capability).

If a Wiz technical writeup is later located, this entry should be
updated to cite it directly and confirm the methodology claim.

## Why this entry exists

The 04-14 entry's "Binary blobs in source repos: explicit no-signal
to negative signal" sharpening was framed at the granularity of
*files committed to a repository*. Cox's reading generalizes the
concern: it is not just binary blobs in source trees that lose
defensive opacity — it is **closed-source binary distribution as a
category**. Vendor SaaS internals shipped in container images,
proprietary CLIs, embedded firmware, and any system that
historically depended on binary-distribution opacity as a defense
layer is now in the same risk class as source distribution, with
respect to vulnerability discovery.

This is the first explicit, dated public articulation of the
"closed-binary opacity is no longer load-bearing" position by a
recognized voice with no commercial stake in selling decompilation
tooling (Cox is a Go core maintainer, not a vendor). It belongs in
the threat-landscape record on that basis alone.

The entry also captures one secondary observation from the GitHub
disclosure that is independently useful for signatory's signal model:
**deployment drift / unintended-code-path-on-disk** as a signal class.

## What happened (CVE-2026-3854)

Facts from the GitHub disclosure post:

- Vulnerability class: delimiter injection. User-supplied git push
  option values were incorporated into internal inter-service
  metadata using a delimiter character that could appear in user
  input. By embedding the delimiter, an attacker could inject
  additional fields the downstream service interpreted as trusted
  internal values.
- Exploit chain: chained injected values overrode the environment
  the push was processed in, bypassed sandbox protections normally
  constraining hook execution, and achieved arbitrary command
  execution on the GitHub server handling the push.
- Affected surface: github.com, GitHub Enterprise Cloud (all
  variants), GitHub Enterprise Server (multiple supported releases).
- Authentication requirement: any user with push access to any
  repository, including a self-created repository.
- Detection telemetry: the exploit forced execution of a code path
  that does not run during normal operation; GitHub's audit
  telemetry for that code path showed only Wiz researcher activity.
  No customer-data impact reported.
- Defense-in-depth observation from GitHub: the exploitable code
  path was present on disk in a deployment configuration where it
  was not intended to be reachable. An older deployment method had
  excluded it; when the deployment model changed, the exclusion was
  not carried forward. GitHub has now removed the unnecessary code
  path from environments where it should not exist.

Signatory's threat model is not directly concerned with web-service
RCE bugs. The relevance of CVE-2026-3854 is the *discovery context*
(Wiz's methodology, per Cox) and the *defense-in-depth observation*
(deployment drift).

## What this reinforces

### Forgery-resistance hierarchy: closed-binary opacity drops further

`trust-model.md` §"Signals must be weighted by forgery resistance"
ranks signals by attacker-cost-to-forge. The 04-14 entry observed
that the bottom of that hierarchy (code style, commit messages, CI
hygiene) is "actively misleading in suspicious contexts" because
cyber-tuned LLMs can produce idiomatic plausibly-clean output
cheaply.

Cox's reading extends the same erosion to a different signal class:
**binary-distribution-as-opacity is no longer a defensive posture**.
Concretely:

- A project that ships only closed binaries to defenders is providing
  no more attacker-cost asymmetry than one shipping source.
  Attackers with LLM+IDA capability decompile to readable IR cheaply;
  defenders without that tooling cannot reproduce the analysis.
- This is *worse* than parity. It is a defender-disadvantage:
  attackers can read the source-equivalent of the binary; legitimate
  reviewers, package signatories, and downstream auditors operate
  blind unless they invest in the same decompilation tooling.
- "Reproducibility from source" — already cited in 04-14 as the
  strongest mitigator for binary-blob risk — becomes the **operative
  positive signal** for this entire signal class. Closed binaries
  shipped without reproducible-from-source provenance are not just
  "no-signal to negative"; they are now actively negative.

### Defense-in-depth: deployment drift as a signal class

The GitHub root-cause observation — *a code path was present in an
environment it was not meant to be reachable from, because a
deployment-model change failed to carry forward an earlier
exclusion* — is independently interesting for signatory's signal
model. Generalizing:

- Container images / artifacts that contain code unused in their
  target deployment are a latent risk surface even absent any
  identified vulnerability. The cost-to-exploit for a future bug in
  that code is the bug-finding cost; the cost-to-exclude was paid
  once historically and silently dropped.
- For supply-chain trust, this maps to a class of observations
  signatory does not currently capture: *artifact-content scope
  vs. declared functionality*. A package whose distributed artifact
  contains substantially more executable code than its documented
  functionality requires has undeclared latent surface.
- This is not a v0.1 signal — collection requires artifact-level
  static analysis tooling outside v0.1 scope. It is worth recording
  as a post-v0.1 signal-class candidate.

### Vendor-communication discipline (04-17) applies

The GitHub disclosure post is a vendor incident-response artifact.
Parse it adversarially:

- *"In less than two hours we had validated… deployed a fix… and
  begun a forensic investigation that concluded there was no
  exploitation"* is a **response-quality framing** calibrated for
  trust restoration. The facts (timestamps, no-impact telemetry,
  patches across supported releases) are observable and recordable;
  the *speed-as-virtue* narrative is communications work.
- *"No customer data was accessed, modified, or exfiltrated"* is
  bounded by the telemetry GitHub has, against the threat-model
  GitHub considered. It is consistent with the disclosed evidence;
  it is not an absolute claim against unknown threat actors with
  different methodologies. Treat as "no evidence of exploitation at
  the observability surface available to GitHub", which is what the
  post actually supports.

These notes do not diminish GitHub's response — by the disclosed
facts, the response was prompt and competent. They are calibration
for how signatory should ingest disclosure-post content if such
posts are ever a signal input.

### Asymmetric signal federation (still correct)

Independent observation: incident-response posts of this kind cannot
federate as positive trust signals about the *vendor's* entity
profile, for the same federation-asymmetry reasons articulated in
04-14. They can be recorded as facts (CVE issued, patches available,
response window) and contribute to incident-history calibration;
they cannot be plumbed through as "this vendor is trustworthy
because they responded quickly to this one bug."

## New architectural principle: reproducibility-from-source as a first-class positive signal

Sharpening of 04-14's mitigation observation, promoted by this entry:

> **Reproducible-from-source builds are a first-class positive
> signal, independent of their build-provenance attestation. They
> are the operative defender-side counterweight to LLM-assisted
> binary decompilation, which has collapsed closed-binary opacity as
> a defensive posture.**

Concretely, signatory's signal set should:

1. Distinguish "reproducible-from-source build" from "signed binary
   with no source verifiability" as separate, positively-weighted
   categories — not lump both into "vendor-distributed artifact."
2. Treat closed-binary-only distribution as a **negative** signal in
   the absence of a declared rationale (firmware, embedded-vendor
   licensing, etc.), elevating it from 04-14's "no-signal to
   negative" framing.
3. Keep the 04-14 distinction between "binary present" and "binary
   justified" — but the bar for "justified" rises: licensing
   restrictions or distribution convenience are no longer sufficient
   rationale, because the closed-binary defensive premise no longer
   holds.

This is a v0.1 signal-set change candidate. The collection mechanics
(detecting whether a project has a documented reproducible build)
are tractable for the v0.1 signal scope. Belongs in `signals-v01.md`
on next pass.

## Sharpened (incremental)

### "Binary justified" rationale tightens

The 04-14 entry allowed declared rationale (test fixtures, font
assets, embedded vendor libraries with disclosed source) as a
mitigator for binary content. That list still holds, but the
*absence* of a rationale moves from "leans suspicious" to "negative
signal" given the 04-28 reading. The threshold has shifted; the
rationale categories have not.

### Patch-as-disclosure-amplifier (cross-ref to parity entry)

The 04-28 multi-vendor parity entry observes that patch publication
is now a 1-day-exploit-recipe vector. CVE-2026-3854 is a worked
example: GHES customers who delay patching are exposed to anyone
who can apply LLM-assisted reverse-engineering to the published
patch diff, which is a strictly easier task than the original
discovery (narrowed scope, known vulnerable code path). The general
principle is in the parity entry; this CVE is a concrete instance.

## What this does *not* do

### Does not promote "closed-source = untrustworthy" as a flat rule

The signal change recommended above weights closed-binary-only
distribution negatively *in the absence of declared rationale*. It
does not categorically reject closed-source projects. Many
legitimate distributions ship closed binaries for licensing or
commercial reasons. The signal change pressures those projects
toward declaring rationale and providing reproducible-from-source
build options where possible — which is the architecturally correct
outcome — without treating commercial software as inherently
untrustworthy.

### Does not ingest the GitHub disclosure post as a positive signal about GitHub

Per the federation-asymmetry reasoning above. Disclosure posts
cannot federate as vendor-trust signals. They are facts on the
threat-landscape record, not entries in a vendor-approval list.

### Does not propose deployment-drift as a v0.1 signal

Worth recording as a post-v0.1 candidate; collection mechanics are
out of v0.1 scope.

### Does not commit to Cox's methodology claim as fact

Recorded as Cox's reading, not as independently verified. The
architectural conclusions hold under either interpretation because
the *capability* of LLM-assisted binary decompilation is established
(04-14 entry); whether this specific bug was found that way changes
nothing for the trust model.

## Open questions added to `design/open-questions.md`

- Should `signals-v01.md` add a "reproducible-from-source build
  available" positive signal and a "closed-binary-only distribution
  without declared rationale" negative signal in this v0.1 cycle?
- How does signatory detect / declare "reproducible build" status
  for packages whose ecosystems do not have first-class
  reproducibility metadata (npm, PyPI without explicit pinned-build
  manifests, Go modules without verified-builder attestations)?
- Is "artifact-content scope vs. declared functionality"
  (deployment-drift / latent-code-on-disk) a candidate post-v0.1
  signal class, and what tooling would collect it?
- Should disclosure-post content (incident-response artifacts) be
  modeled as a structured input class, or remain narrative
  threat-landscape material? Volume so far suggests the latter;
  revisit if disclosure-post density grows.

## Cross-references

- [`2026-04-28-multi-vendor-mythos-parity.md`](2026-04-28-multi-vendor-mythos-parity.md)
  — same-day parallel entry; the parity-and-patchpocalypse half of
  the 2026-04-28 snapshot
- [`2026-04-22-mozilla-anthropic-firefox-mythos.md`](2026-04-22-mozilla-anthropic-firefox-mythos.md)
  — forgery-resistance hierarchy reading this entry extends to
  closed-binary distribution
- [`2026-04-17-vendor-communication-as-signal.md`](2026-04-17-vendor-communication-as-signal.md)
  — sotto voce discipline applied to the GitHub disclosure post
- [`2026-04-14-openai-tac-gpt54-cyber.md`](2026-04-14-openai-tac-gpt54-cyber.md)
  — "Binary blobs in source repos" sharpening this entry generalizes;
  GPT-5.4-Cyber's binary RE capability as the established
  capability that makes Cox's methodology claim plausible
- [`../trust-model.md`](../trust-model.md) §"Signals must be weighted
  by forgery resistance" — hierarchy unchanged; closed-binary signal
  class repositioned
- [`../signals-v01.md`](../signals-v01.md) — reproducible-from-source
  positive signal recommended for this cycle; closed-binary-only
  negative signal recommended for this cycle
- [`../ANTIPATTERNS.md`](../ANTIPATTERNS.md) — federated-approve-list
  rejection unchanged; vendor incident-response posts as
  non-federate-able
- [`../open-questions.md`](../open-questions.md) — questions above
  tracked there for resolution
