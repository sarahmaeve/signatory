# Signatory: Roadmap

Last updated: 2026-04-27

## V0.1 — Remaining

V0.1 blocks until every item in this section is complete. Order within each subsection is load-bearing; order across subsections is not.

### Installation and Verification

We need to create the easiest possible path for a new user to clone or fork the repo and begin using signatory through /analyze and the CLI. This needs to be documented and verified. The day one experience is critical.

### Improve economics

Our goal is to push as much as possible toward the mechanical and deterministic collection of signals. Filtering, vetting, error-correction &c. need to be the province of Go code, not the LLM analyst steps. Reducing token spends and clock time is critical. If an analyst does WebFetch just to check something it could acquire from the database, that is a bug. If an analyst does a network operation for something that is already local, that is a bug. If an analyst re-clones a repo, that *may* be a bug. If an analyst uses `curl`, that is a bug if we have surfaced the signal or pattern before -- it is a cache miss from our local db.

### Add additional signals

Add as many valuable signals as we can brainstorm to the mechanical collectors.

### Validate through dogfood

Dogfooding has already shown gaps between user expectations, data formats, knowledge and interaction flows.
We need to perform test cycles, both LLM-driven and manual, to validate our system and our assumptions, and then iterate.

### Guard against unexpected behavior

The LLM and MCP surfaces are prone to entering unexpected data. Our storage layer and ingestion layers need to refuse malformed requests, pass those errors up the stack, and present a clear error. We should *expect* that interaction may be incorrect, as the LLM acts as a fuzzer regardless of instructions. Docs are a guideline. Our code is the rule.

### Packaging / polish

These are nice-to-have and may slip to v0.1.1 if the contract milestones take longer than estimated:

- Signal TTL/expiry — cache works but doesn't auto-expire
- Docker packaging — `go install` works but Docker is the stated MVP target
- Rust and cargo support: this needs to be planned and scoped.

## V0.2 — Planned

- Hosted/org-wide dashboard
- Federated burn list subscription/sync protocol
- GitLab signal collector (non-GitHub platform support)
- CI pipeline integration (gate merges on posture policy)
- Dependency graph visualization
- Cross-ecosystem correlation (detect coordinated campaigns)
- MCP-side write tools for posture/burn (if user feedback calls for them)

## V0.2/Enterprise — Internal Identity and Realms

Likely differentiator between open-source and enterprise offerings. Requires user testing and feedback before design.

### Concept

Internal identity registries allow organizations to mark contributors as verified employees, contractors, or external. Combined with the existing signal model, this enables rapid patch triage:

- Patches from `identity:internal/james.park` (verified employee) get a higher baseline trust than `identity:github/unknown-user123`
- Reviewer team identities (`team:internal/platform-security`) carry organizational authority
- Employee departure → halt identity → unreviewed patches flagged
- Credential compromise → burn identity → all touched code re-reviewed

### Realms

A realm is a namespace for identity verification:

```
identity:corp-github/james.park    -- corporate GitHub
identity:corp-gitlab/james.park    -- corporate GitLab
identity:verified/james.park       -- linked corporate identity
```

Realms solve the cross-platform identity linking problem for organizations with multiple internal code hosting systems.

### Why Deferred

- No easy path to user testing or feedback for realm design currently
- Risk of building the wrong abstraction without enterprise users
- The v0.1 entity model (canonical URI with platform prefix) supports realms as a future extension without schema changes
- Internal identity is a superset of the open-source identity model — nothing in v0.1 prevents adding it later

### Prior Art

Similar systems have been designed for rapid PR triage in high-volume environments, marking pull requests as privileged or suspect based on user ID and other signals. The realm model would support this pattern.

## V0.3+ — Future

- **Signed attestation for vetted-frozen tier** (deferred 2026-04-14 from V0.2 — heavyweight: key management, envelope schema, signing tool integration, embedded analysis records. Until v0.3 ships, vetted & frozen is internally-produced only — see [open-questions.md](open-questions.md) §"Vetted & frozen scope in early versions" and §"LLM-as-named-tool in attestation envelopes")
- **Content-hash pinning** (BLAKE3 or similar) as a first-class version form — revisit if pressure grows; per D7 in [`agent-facing-contract.md`](agent-facing-contract.md) burns at per-version granularity cover the current need
- Additional ecosystem providers (crates.io, Maven, RubyGems)
- Schema evolution (leverage migration system)
- Multiple signal source plugins
- Event-driven monitoring (subscription to change detection service)
- Kind/Kubernetes deployment
- Cloud deployment (public/private)

## Deferred Design Decisions

These require architectural discussion before implementation:

- Interface segregation for Store (#59)
- Signal.Value typed structs vs. validation layer (#2)
- Authentication/authorization model for burns and posture (#3)
- Entity type inference from identifiers (#40)
- Context timeout strategy for CLI commands (#51)
- Team identity cryptographic mechanism (PGP/GPG likely, deferred to attestation layer)
- Internal identity realms (enterprise, needs user testing)
