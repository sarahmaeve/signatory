# Platform Mutability: What's Actually Trustworthy

## Premise

Signatory collects trust signals from platforms — primarily GitHub
today, eventually GitLab, npm registry, PyPI, and others. We treat
these signals as evidence about the trustworthiness of code, projects,
and people.

But how trustworthy is the evidence itself?

The honest answer: **almost nothing on these platforms is
cryptographically immutable except commit SHAs and tree contents.**
Everything else is mutable, often silently, often without an
accessible audit trail. The Trivy attack (March 2026) demonstrated
this concretely: TeamPCP rewrote the git tags of 76 of 77 versions
of `aquasecurity/trivy-action`, defeating version pinning entirely.

This document categorizes platform data by mutability and explains
what it means for signatory's signal model.

## Mutability Tiers

### Tier 1: Cryptographically Immutable

These are the only platform artifacts that cannot be silently
modified. Their integrity is guaranteed by mathematics, not by the
platform.

| Artifact | Why immutable |
|----------|---------------|
| Commit SHA | Hash of the commit content; changing content changes the SHA |
| Tree SHA | Same — hash of the directory structure |
| Blob SHA | Same — hash of the file content |
| Signed commit signature | Cryptographic signature over the commit hash |

**Implication for signatory:** When we record a SHA, we are recording
a cryptographic claim that cannot be changed. When we record anything
else, we are recording an observation of mutable state.

### Tier 2: Mutable but with Implicit History

These can be changed, and the change leaves *some* trace, but the
trace is often inaccessible, incomplete, or temporary.

| Artifact | What can change | Where the history lives |
|----------|----------------|------------------------|
| Branches | Force-push, delete, rename | Events API for ~90 days |
| Tags | Force-push, delete, recreate | Events API for ~90 days |
| README | Editable | Git history of the file |
| LICENSE field | Derived from LICENSE file in repo | Git history of the file |
| Repository name | Renames leave a redirect | Partial; original name reusable |
| Issue/PR title | Editable | UI shows edit history, API exposure inconsistent |
| Issue/PR body | Editable | Same |
| Comments | Editable | Same |
| Review comments | Editable | Same |

**Implication for signatory:** History exists but is fragile.
Treating any of these as authoritative without recording our own
observation is a mistake.

### Tier 3: Mutable with No Reliable History

These can be changed silently, and there is no public audit trail.
Treating them as authoritative is a category error.

| Artifact | What can change |
|----------|----------------|
| Star count | Mass star/unstar; no historical data via API |
| Fork count | Same |
| Watcher count | Same |
| Follower count | Same |
| Repository description | Free-form edit |
| Repository topics | Free-form edit |
| Release notes | Editable after publish |
| Release binary attachments | Re-uploadable |
| Repository visibility | Toggleable public/private |
| Default branch | Changeable |
| Branch protection rules | Editable |
| Repository ownership | Transferable |
| User display name, bio, email, company | Free-form edit |
| Organization membership | Add/remove silently |
| Reactions on issues/PRs | Add/remove silently |
| Labels on issues/PRs | Editable |
| Milestones | Editable |

**Implication for signatory:** These are observations of state at
a moment in time, not facts about the entity. The signal we record
is "we observed X stars on date Y," not "this project has X stars."

### Tier 4: Decoupled From Identity Entirely

These look like identity signals but are not actually tied to the
person they appear to represent.

| Artifact | Why it's not what it looks like |
|----------|-------------------------------|
| Commit author email | Git does not enforce identity — anyone can commit as anyone, just by setting `git config user.email` to that address |
| Commit author name | Same |
| Author timestamp | Forgeable — git accepts any timestamp, including past or future dates |
| Committer name/email/timestamp | Same |
| Reclaimed usernames | After account deletion, GitHub usernames can be reclaimed by a different person, inheriting the visible identity |
| Organization affiliation displayed on commits | Cached at commit time, may not reflect current affiliation |

**Implication for signatory:** Identity signals from commit metadata
alone are unreliable. Cross-referencing against signed commits, account
tenure, and external identity providers is necessary for any
identity-based trust decision.

## The Special Case: Signed Commits

Signed commits are an interesting middle ground. A GPG or SSH signature
on a commit is cryptographically valid at signing time. But:

- **Key revocation invalidates past signatures.** If a maintainer
  revokes their GPG key (because it was compromised), commits signed
  with that key become unverified — even if they were legitimately
  signed before the revocation.
- **Key expiration has the same effect.** A signed commit with an
  expired key shows as unverified in GitHub's UI.
- **GitHub recomputes verification on every API request.** The
  `verified` field is not stored — it is re-evaluated against current
  key state.

This means **the forgery resistance of "commit signing" depends on
when you observed it.** A commit that was verified yesterday may be
unverified today, even though no one tampered with the commit itself.

For signatory, this argues for recording the observed verification
status with a timestamp, not assuming current state matches past state.

## Star and Follower Manipulation

We currently treat star count, fork count, and follower count as
positive criticality signals. The reality is more nuanced:

- **Star manipulation is a known phenomenon.** "Star jacking" services
  sell mass stars; coordinated unstar campaigns have been documented.
- **GitHub's API does not expose historical star counts.** A repo
  showing 10,000 stars today might have had 100 stars yesterday — we
  cannot tell from a single observation.
- **Followers can be created en masse via bot accounts.** A user
  with 5,000 followers may have 5,000 real fans or 5,000 sock puppets.
- **Forks are even noisier.** A fork count includes abandoned forks,
  bot mirrors, and personal copies that have never been used.

**Forgery resistance ranking adjustment:**

| Signal | Currently | Should be |
|--------|-----------|-----------|
| Stars | Medium-declining | **Low-declining** |
| Forks | Medium-declining | **Low-declining** |
| Followers | Medium-declining | **Low-declining** |
| Watchers | (not collected) | Low-declining |

These are still useful as broad criticality indicators, but should
not be treated as forgery-resistant signals.

## What Actually Provides Forgery Resistance

If almost everything is mutable, what can signatory rely on?

### Cryptographic chains

- Commit SHAs in git history
- Signed commits (with key validity caveats)
- Trusted publisher binding (OIDC-backed publishing)
- npm package provenance attestations
- PEP 740 attestations (PyPI)
- Sigstore / cosign signatures

### Long-term observable patterns

- Tenure (account age, repo age) — measured in years, expensive to fake
- Activity distribution over time — bursts and gaps tell stories
- Cross-platform identity consistency — harder to fake across multiple platforms

### Our own observations

- Append-only signal collection — what we saw, when we saw it
- Tag SHA tracking — relabeling becomes detectable
- Source code divergence checks — published artifacts vs. claimed source

### External verifiable claims

- Conference talks (video evidence tying real people to identities)
- Academic publications
- Verified employment / org membership (when verified by the org itself)
- News coverage and ecosystem reputation

## Implications for the Signal Model

### 1. The append-only model is not optional

Without append-only signal collection, signatory has no defense
against silent platform mutation. Our database must become the
historical record that platforms do not keep.

This was already on the v0.1 must-do list. This document reinforces
that it is the foundation, not a nice-to-have.

### 2. Signals are observations, not facts

A signal record means "signatory observed X about entity Y at
time T from source S." It does not mean "Y has property X." This
distinction matters in the data model and in how we present signals
to users.

The current `Signal` type already captures `CollectedAt` and `Source`.
The framing in CLI output, MCP responses, and dashboards should
emphasize the observation framing.

### 3. SHA tracking is the strongest claim we can persist

When signatory records "tag v1.0.0 of repo X pointed to SHA abc123 on
date Y," that record is cryptographically anchored. If the tag later
points to a different SHA, signatory can detect it without trusting
GitHub. The SHA tracking signal added to v0.1 (from the Trivy attack)
is the highest-confidence signal we can collect.

### 4. Mutation detection is itself a signal

If a previously-observed value changes — star count drops by 90%,
description suddenly mentions a different package, repository goes
private, default branch changes — that change is data. Even if the
change is legitimate, it is worth surfacing to a human reviewer.

This argues for a future feature: a "diff" view that compares the
current observation against the previous one and highlights all
changes, not just signal-by-signal updates.

### 5. Forgery resistance ratings need adjustment

The current ratings treat star count and fork count as
"medium-declining" forgery resistance. Given the documented manipulation
patterns and the absence of historical data, these should be
"low-declining" — still collected, still useful as broad indicators,
but not treated as evidence of trustworthiness.

The signals that should be elevated:

| Signal | Current | Should be |
|--------|---------|-----------|
| Account tenure | High | **Very high** |
| Repository age | Very high | Very high (correct) |
| Commit signing (signed at observation) | Very high | **High** (because of key revocation risk) |
| Tag SHA stability | (new) | **Very high** |
| Published source matches git tag | (new) | **Very high** |
| Trusted publisher binding | High | **Very high** |

### 6. Multi-source verification is the durable defense

When a single platform's signal is mutable, the defense is to
cross-reference against other sources that would have to be
mutated in concert.

- Account tenure on GitHub × public conference appearances
- Commit signing × employer attestation × news coverage
- Trusted publisher × git tag × source code comparison
- Star count × dependent count × actual download count

This is the multi-signal compositional model we already designed,
applied with awareness that no single signal is reliable.

## The Bigger Picture

Signatory's role is not to trust GitHub. Signatory's role is to be
a third-party witness that records what platforms claimed, when they
claimed it, and provides a stable historical record that platforms
themselves do not maintain.

In this framing, signatory is more like an archive than a query tool.
The query capability matters, but the archival capability is what
makes the queries trustworthy.

This is also the answer to the question "why do we need this when
GitHub has all the data?" Because GitHub has the data *now* — it
doesn't have the data *as of last Tuesday*, and it cannot prove that
its current data hasn't been silently mutated.

Signatory can.
