# Signatory: Open Questions

Questions that need resolution before or during implementation. Grouped by area.

## Data and Caching

### Freshness strategy

Querying GitHub APIs, package registries, and OpenSSF for every dependency on
every run won't scale. Options:

- Local cache with configurable TTLs per signal type
- Stale-while-revalidate pattern
- Background refresh mode
- Offline mode (cache-only, for CI or air-gapped environments)

**Decision needed:** Caching architecture and default TTLs.

### Rate limiting

GitHub API has rate limits (5,000 req/hr authenticated, 60 unauthenticated).
OpenSSF Scorecard API has its own limits. Package registries vary.

How do we handle this gracefully? Priority queuing? Parallel with backoff?
Persistent token management?

### Local storage format (resolved — see Resolved Questions)

## Signal Normalization

How do we represent signals from heterogeneous sources in a common format?
Need a schema that's flexible enough for different signal types but structured
enough for MCP consumption and dashboard rendering.

This is an implementation design question — will be addressed when building
the entity profile data model.

## Signing and Attestation (Utility, Not Core)

Signatory consumes existing signing infrastructure rather than building its
own. The remaining questions are about integration, not implementation:

### Attestation storage (if we produce organizational attestations)

Dependency posture tiers (vetted/trusted/unexamined/unknown) and burn lists
are organizational state. Where do they live?

Options:
- Local config file (simplest, portable)
- Local database alongside cached signals
- Exportable/importable format for sharing within an org

**Decision needed** once we settle on local storage format.

## Source Control Platform Support

### Multi-platform signal collection

Signatory's signal collection is currently GitHub-centric (`gh api`). Projects
hosted on other platforms have reduced signal visibility:

| Platform | API Access | Signal Coverage | Notes |
|----------|-----------|----------------|-------|
| GitHub | Full (`gh api`) | Complete | Primary platform, best tooling |
| GitLab | REST API available | Partial | No `gh` equivalent CLI; need HTTP client or WebFetch |
| Gitea | REST API available | Partial | Self-hosted instances vary |
| SourceHut | Limited API | Minimal | Email-driven workflow, different model |
| Codeberg | Gitea-based API | Partial | Community-hosted Gitea |
| Bitbucket | REST API available | Partial | Enterprise use |

**The visibility gap is itself a signal.** A project on a platform with less
automated scrutiny receives less ecosystem-wide attention. This doesn't make
the project less trustworthy, but it means less passive verification.

**Practical impact (discovered via dogfooding):** modernc.org/sqlite lives on
GitLab with an archived GitHub mirror. We could not query contributor graphs,
issue counts, or PR review patterns via `gh api`. The refs-to-stars ratio was
misleading because GitHub stars were on the archived mirror.

**Decision needed:** For v0.1, support GitHub as the primary signal source.
Design the signal collector interface to accommodate other platforms. GitLab
support is the highest priority addition after v0.1.

## Ecosystem Providers

### Package-to-repo mapping

How do we reliably map a published package back to its source repository across
ecosystems? This is straightforward for some (Go modules encode the repo URL)
and unreliable for others (PyPI `project_urls` is optional and sometimes wrong).

Additional complication: vanity import domains (e.g., `modernc.org/sqlite` →
`gitlab.com/cznic/sqlite`) require an extra resolution step. Go's `?go-get=1`
metadata endpoint can resolve these.

This is a hard problem that affects the quality of every signal we collect.

### Dependency tree resolution

Do we resolve dependency trees ourselves, or lean on ecosystem tooling
(`npm ls`, `pip-compile`, `go mod graph`)? Using ecosystem tools is more
accurate but requires those tools to be installed. Parsing lockfiles directly
is more portable but may miss edge cases.

## MCP Integration

### Server interface design (resolved — see Resolved Questions)

### Scoring extensibility

The vision is that users can plug in their own scoring via MCP. What does this
look like concretely? A prompt template that receives signals as context?
A tool that the scoring model calls back into? Need to prototype this.
Deferred until the base MCP tools are working.

## Scanner Design

### Scope of the proof-of-concept scanner

The scanner examines a specified repo, package, or PR on demand and populates
entity profiles. Remaining questions:

- Does it run as a CLI subcommand (`signatory scan <target>`) or is scanning
  implicit in `signatory analyze`?
- How does it handle rate limits for GitHub API and registry APIs during a
  full dependency tree scan?

### Future: subscription-based monitoring

Eventually, signatory should subscribe to an external service (or one we
build) for ongoing change detection — maintainer changes, ownership transfers,
unusual commit patterns — rather than relying solely on point-in-time scans.
Design deferred until the scanner PoC validates the signal model.

## Trust Model Refinements (from 2026-04-14 brainstorm)

These questions emerged from analyzing the OpenAI Trusted Access for
Cyber + GPT-5.4-Cyber announcement against the existing trust model.
Source analysis: [threat-landscape/2026-04-14-openai-tac-gpt54-cyber.md](threat-landscape/2026-04-14-openai-tac-gpt54-cyber.md).

Most are post-v0.1 unless noted otherwise.

### Signal packages as a first-class concept (post-v0.1)

The eventual federation primitive is **signal package definitions**, not
findings. A signal package is a curated, versioned bundle of signal
types and methodology — closer in shape to OWASP Top Ten than to a CVE
database. Findings stay local; methodologies are shareable.

V0.1 already ships the registry as a Go code constant (per
[signal-type-registry.md](signal-type-registry.md) §"Implementation
Notes"). The post-v0.1 questions:

- Should signal packages be modeled as separate artifacts (YAML / JSON
  / Markdown files in a known location) versioned independently of the
  signatory binary, or remain bundled?
- If separate: what's the authoring format and what mechanism loads
  external packages?
- Should every analysis record carry the package version that produced
  it, so revisits can diff "what changed because the methodology
  evolved" vs. "what changed because the world moved"?

Middle path worth considering: ship a future version with the package
*shape* (named, versioned, separate artifact in the repo) but no
loading mechanism for external packages. Loading is added later
without schema or workflow changes.

### Asymmetric federation as a first-class architectural principle

> A trust signal can be safely federated across organizational
> boundaries when its failure mode is **degradation**. It cannot be
> safely federated when its failure mode is **endorsement**.

This rule generalizes the existing burn-governance design and explains
why approve-style federation is structurally unsafe — the wider a
"trust X" list's adoption, the higher its attack value as a
concentrated supply chain target.

Open: should `trust-model.md` §"Burn Governance: Federated Model" be
promoted to a broader §"Signal Federation" section, with burns as one
application of the asymmetric rule? (Recommended before v0.2
federation work begins.)

### "Analyze peer report but don't believe it" as a distinct ingestion path (post-v0.1)

When peer signal-sharing arrives, a peer report is **not input data
to merge** — it is **an analytic target in its own right**. The
receiver verifies each claim against their own signal collection and
produces *their* analysis of *the peer's* analysis.

Architectural implication: peer reports must be machine-checkable
claim by claim, not just narrative. This shapes the structured-emission
work already underway in
[mcp-dual-analyst-architecture.md](mcp-dual-analyst-architecture.md).

Eventually warrants a distinct MCP tool — `analyze_peer_report(report)`
— separate from `ingest_signals(source)`.

### Trust circles abstraction (post-v0.1)

V0.1 is genuinely solo: just the user and the agents they spin up.
"Trusted coworkers" is the next stage and brings the question of
trust-circle primitives. Likely shape: locally-configured named circles
(no central registry), with the same dependency potentially having
different posture in different circles.

Deferred until the use case is concrete (likely V0.2+).

### Binary blob with declared rationale — data model representation (post-v0.1)

Per [threat-landscape/2026-04-14-openai-tac-gpt54-cyber.md] §"Sharpened",
binary blobs in source repos default to no-signal-to-negative. Justified
binaries (test fixtures, vendored shared libs, font assets) need a
rationale path.

Open: repo-level attestation vs. per-file annotation? Reproducibility
proof as the strongest mitigator?

### AI-attribution metadata on commits — entity profile attribute path (post-v0.1)

Codex Security and similar systems will increasingly carry attribution
in commit metadata. Recording this as an entity-profile attribute (was
this commit human-authored, AI-assisted, or AI-generated; if AI, which
vendor and tier; was the fix human-reviewed) opens compositional
analysis without making any single attribute decisive.

Open: which signal-collection path produces this? Commit message
parsing, GitHub PR metadata, both?

### LLM-as-named-tool in attestation envelopes (v0.3, when attestation lands)

When attestation production lands in v0.3, the LLM is a **named tool**
inside the human-signed envelope, not a co-attestor. The human is
director and attestor at all times. Envelope contents include LLM
model + version, signatory version, MCP tool versions called, the
analysis text verbatim, and the human's signature over all of it.

Stamps for LLM/signatory/MCP version are primarily for **internal
comparison** (your future self revisiting your past analyses), not
external trust signaling. External versioning will be expressed as
"snapshots and analysis taken using the following signal packages."

---

## Resolved Questions

### V0.1 signal set (resolved)

Finalized. Six signal groups covering project vitality, identity/governance,
publication integrity, hygiene, consumer posture, and criticality. Plus
temporal era classification. See [signals-v01.md] for the complete set.

### V0.1 scope (resolved)

1. CLI skeleton: `signatory analyze <package|repo>`, `signatory survey`
2. npm ecosystem provider (manifest parsing, registry API)
3. GitHub signal collector (repo metadata, contributor info, commit signing)
4. npm registry signal collector (publish metadata, version history)
5. OpenSSF Scorecard integration
6. Entity profile construction (project + identity + package)
7. Dependency posture tracking (vetted/trusted/unexamined/unknown tiers)
8. Burn list management (local, with import capability)
9. Temporal era classification
10. Structured JSON output
11. Local caching with configurable TTLs
12. Proof-of-concept scanner that populates profiles on demand

PyPI as the second ecosystem provider follows once the npm provider and
interfaces are proven.

### Signing systems recognized in v0.1 (resolved)

GPG and SSH commit signatures via GitHub's verified status. Trusted publisher
binding (npm provenance) as a publication integrity signal. Sigstore/gitsign
recognition deferred — consume what GitHub already surfaces.

### Fallow vs. finished code (resolved)

Not an intrinsic property of the code. Determined by the consumer org's
relationship to it. Managed via dependency posture tiers (vetted & frozen,
trusted-for-now, unexamined, unknown provenance). See trust-model.md.

### Burn governance (resolved)

Federated, local-first. Each org maintains its own burn list. Orgs can
subscribe to other orgs' lists (especially within hierarchies). Subscription
is itself a signal. Analysts can strip inherited burns as a counterintelligence
exercise. See trust-model.md.

### Analysis vs. attestation priority (resolved)

Analysis, tracking, and visibility are the product. Signing/attestation is a
supporting utility. Signatory consumes existing signing infrastructure (GPG,
SSH, Sigstore) rather than implementing its own cryptographic protocols.

### Identity model approach (resolved)

Multi-signal, compositional. No single signal is definitive. Trust is a
composite of independently verifiable signals (org membership, tenure,
maintainer role, public identity, cross-platform consistency). The same
compositional model will extend to code patches. See
example-identity-analysis-rsc.md.

### Local storage format (resolved)

SQLite, using `modernc.org/sqlite` (pure Go, no CGO dependency).

Rationale:
- Single file, zero config, no running service — ideal for CLI tool
- Relational model fits entities, signals, burns, and posture tiers naturally
- JSON columns for heterogeneous signal values (`json_extract` for queries)
- Recursive CTEs for dependency tree traversal and burn propagation
- Timestamp-based TTL/cache expiry
- Pure Go driver means `go install` works everywhere without a C compiler
- Portable — single file to copy, back up, or share

### MCP interface design (resolved)

Seven tools + four resources. See [mcp-interface.md] for full design.

Core interaction philosophy: LLM is an analyst, not a decision-maker.
- No autonomous network calls (LLM asks permission before scanning)
- Summaries by default (user prompts for drill-down)
- Recommendations with rationale, not autonomous decisions (user confirms
  all trust-modifying actions)

### Attestation production scope (resolved 2026-04-14)

Deferred to v0.3. Attestation production — signing posture decisions
into externally-verifiable envelopes — is heavyweight infrastructure
(key management, envelope schema, signing tool integration, embedded
analysis records) and is moved out of v0.1 scope to keep the initial
release focused on signal collection and personal trust journaling.
ROADMAP.md updated accordingly.

V0.1 records vetted & frozen as a local posture; v0.3 makes that
posture externally signable.

### Vetted & frozen scope in early versions (resolved 2026-04-14)

In v0.1 (and through v0.2), vetted & frozen is a posture that **can
only be produced internally** — local state in the user's own
signatory instance. Not externally signable, not federated, not
exportable as endorsement.

Primary use case: **vetting your own fork of code that you have
repaired and verified**, rather than trusting an upstream maintainer's
release. This sidesteps the upstream-trust problem entirely — you
commit to a specific snapshot of code that you (or your team) have
personally reviewed, repaired where needed, and taken ownership of.
Secondary use case: pinning a specific version of an upstream package
as the stable target ("this version of thefuck is the one we use
until we revisit").

External attestation — allowing other consumers to receive your
vetted & frozen signal as evidence — is the v0.3 attestation
production work.

### V0.1 cross-org federation (resolved 2026-04-14)

None. V0.1 ships "cloistered" — no public submission endpoint, no
anonymous ingest path, no cross-org federation of any signal type.
Local SQLite keeps its own counsel.

The asymmetric federation principle (burns federate broadly;
endorsements only within trust hierarchies) and the deeper insight
that **what federates eventually is signal package definitions, not
findings** are captured as design notes in
[threat-landscape/2026-04-14-openai-tac-gpt54-cyber.md] for v0.2+
work. Public-feed / global-registry models are rejected as
architecture for signatory's own infrastructure — signatory is a
*tool* for trust analysis, not a registry of findings.
