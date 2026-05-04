# Signatory: Roadmap

Last updated: 2026-05-04

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

## V0.2 — Planned

- Federated burn list subscription/sync protocol
- GitLab signal collector (non-GitHub platform support)
- Dependency graph visualization
- Cross-ecosystem correlation (detect coordinated campaigns)
- Scan repos / directories for policy violations (what's already in your local db)

## Future

- **Signed attestation for vetted-frozen tier** (deferred 2026-04-14 from V0.2 — heavyweight: key management, envelope schema, signing tool integration, embedded analysis records. Until v0.3 ships, vetted & frozen is internally-produced only — see [open-questions.md](open-questions.md) §"Vetted & frozen scope in early versions" and §"LLM-as-named-tool in attestation envelopes")
- **Content-hash pinning** (BLAKE3 or similar) as a first-class version form — revisit if pressure grows; per D7 in [`agent-facing-contract.md`](agent-facing-contract.md) burns at per-version granularity cover the current need
- Multiple signal source plugins
- Kind/Kubernetes deployment
- Cloud deployment (public/private)
- Connect to local inference provider
- Multiple AI provider support (including run without claude-skill usage)