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

## Ecosystem Providers

### Package-to-repo mapping

How do we reliably map a published package back to its source repository across
ecosystems? This is straightforward for some (Go modules encode the repo URL)
and unreliable for others (PyPI `project_urls` is optional and sometimes wrong).

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
