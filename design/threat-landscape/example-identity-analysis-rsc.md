# Example: Identity Signal Analysis — rsc (Russ Cox)

This document uses Russ Cox (rsc) as a case study for multi-signal,
compositional identity analysis. It illustrates both how trust signals
accumulate and how they degrade — and serves as a reference for the kind
of entity profile signatory should be able to generate.

## Positive Signals

### Institutional Affiliation (Verifiable)

- Google employee, specifically the Go team tech lead for many years
- Commits signed with @google.com / @golang.org email domains
- Verifiable through Google's org membership on GitHub, golang.org OWNERS
  files, internal Gerrit history

### Tenure and Consistency

- Contributing to Go since its inception (~2008–2009)
- 15+ years of continuous commit history on the same project
- GitHub account age and activity pattern consistent with a real, long-term
  contributor — not a recently created or purchased account

### Institutional Authority

- Named role: Go tech lead — not just a contributor, a decision-maker with
  approval authority
- Reviews on Go CLs (Gerrit change lists) gatekeeping the standard library
  for years
- Listed in OWNERS / MAINTAINERS files in the Go project

### Domain Expertise Demonstrated Publicly

- Conference talks (GopherCon, etc.) — video evidence tying a real person
  to the identity
- Academic background (MIT) with published papers
- Blog (research.swtch.com) with deep technical writing spanning years

### Supply Chain Security Work

- Designed and built Go modules, the sumdb (checksum database), the Go
  vulnerability database
- Professional work *is* supply chain verification infrastructure
- This is about as strong a domain-expertise signal as exists

### Cross-Platform Identity Consistency

- Same identity across GitHub (rsc), Go Gerrit, conference appearances,
  academic publications, Google employee directories
- Independently verifiable and mutually reinforcing
- Extremely difficult to fabricate across all platforms simultaneously

## Generalized Positive Signal Framework

| Signal | What it indicates | Forgery difficulty |
|--------|-------------------|-------------------|
| Org membership (verified) | Institutional accountability | Hard — requires compromising the org |
| Commit tenure (years) | Long-term consistent identity | Very hard — requires years of history |
| Named maintainer role | Explicit authority, not just participation | Hard — recorded in project governance |
| Public identity (talks, papers) | Real person, reputation at stake | Very hard — physical/video presence |
| Cross-platform consistency | Identity is genuine, not a sock puppet | Hard — must compromise multiple platforms |
| Domain-relevant work history | Competence signal, not just presence | Moderate — but verifiable |

**Key property:** No single signal is definitive, but the combination is
extremely hard to fake. An attacker could create a GitHub account, but cannot
fabricate 15 years of Gerrit history, conference videos, academic papers, and
organizational membership simultaneously.

## Degradation Signals

What would reduce trust in this identity?

### Institutional Departure

Russ Cox stepped back from the Go tech lead role. When this happens:
- New actions lose institutional backing
- Historical signal remains intact
- Forward-looking signal drops

**Principle:** Trust signals have a temporal direction. Past contributions
retain their signal value; future actions are evaluated under the new context.

### Account Compromise

If GitHub or Gerrit credentials are compromised, every approval under that
identity becomes suspect. The critical question: *when did the compromise
start?*

- A compromise discovered today may have been active for months
- Everything reviewed in the compromised window needs re-evaluation
- The longer the tenure, the more damage a compromise inflicts — a dark
  corollary of tenure being a positive signal

### Behavioral Anomalies

- Sudden change in review volume, timing, or pattern
- Approving large or security-sensitive patches from unfamiliar contributors
- Reviewing code outside usual areas of expertise without explanation
- Long silence followed by sudden high activity

These are the same signals intelligence agencies watch for with compromised
assets.

### Judgment Failures

- A vulnerability is found in code the identity reviewed and approved
- A vulnerability is found in infrastructure the identity designed
- One occurrence is a mistake; a pattern degrades the competence signal

### Association Contamination

- Contributors this identity vouched for or mentored turn out to be compromised
- Not guilt by association, but it degrades the signal of their judgment about
  *other people's* trustworthiness
- This is recursive — a single compromise can cascade through a web of trust

### Coercion or Pressure

- Nation-state pressure to introduce subtle vulnerabilities
- The identity is still genuine, the person is still real, but their
  independence is compromised
- Multi-signal verification cannot detect this because the person *is* who
  they claim to be
- Behavioral anomaly detection becomes the only available signal
- This is the hardest degradation to detect

## Degradation Framework

| Degradation type | What breaks | Detection method |
|---|---|---|
| Institutional departure | Institutional accountability | Public, observable |
| Account compromise | Identity authenticity | Behavioral anomaly, post-hoc discovery |
| Judgment failure | Competence signal | Vulnerability correlation |
| Association contamination | Trust-of-trust chain | Graph analysis after a burn |
| Coercion | Independence | Behavioral anomaly (hardest) |

## Design Principles Derived from This Analysis

### 1. Trust is multi-signal and compositional

Identity trust is not a binary. It is a composite of independently verifiable
signals, each with its own forgery difficulty and decay characteristics. The
identity model should evaluate and present these signals individually, not
collapse them into a single score.

### 2. Trust accumulates slowly but degrades fast

It takes 15 years to build a profile like rsc's. A single confirmed compromise
can invalidate a significant portion of it. The system must respect this
asymmetry.

### 3. Degradation is often retroactive

Compromises are discovered after the fact. The system must support re-evaluating
a window of history: "re-score everything this identity touched between date X
and date Y." This connects directly to the burn mechanism — a burn against an
identity triggers retroactive signal degradation across everything they
reviewed or approved.

### 4. Entity profiles are a first-class concept

The ability to generate and maintain entity profiles — for humans, LLM
interactions, code patches, and other actors — is a core capability, whether
produced internally by signatory or consumed from an external source. Profiles
are the substrate on which trust decisions are made.

### 5. Patch provenance will follow the same model

The multi-signal, compositional approach used for reviewer identity will
eventually extend to code patches themselves. Identity provenance is one
input to patch trust, alongside code hygiene, temporal era, review chain,
and other signals. The identity model and the patch model should share the
same compositional framework.
