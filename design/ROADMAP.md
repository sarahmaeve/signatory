# Signatory: Roadmap

Last updated: 2026-04-21, after accepting [`agent-facing-contract.md`](agent-facing-contract.md).

## V0.1 — Shipped

- **CLI skeleton + core data model.** Canonical URI resolution, target-form acceptance, tier model (including `rejected`).
- **SQLite store.** Migrations, append-only analyses, WAL + busy-timeout for safe concurrent access.
- **Survey command.** Parses `go.mod`, resolves tiers, renders human + JSON output.
- **npm ecosystem provider.** Phase A/B/C + B.6 longitudinal signals + manifest parser. Registry client, source resolution, signal collection, survey routing.
- **go.mod manifest parser.** Dependency enumeration for Go projects.
- **MCP server.** 8 read-only tools (`signatory_analyze`, `signatory_signals`, `signatory_detail`, `signatory_show_analyses`, `signatory_show_conclusions`, `signatory_show_methodology`, `signatory_survey`, `signatory_ingest_analysis`) and 6 resources. Shape differs from the original scoping — mutations are CLI-side, reads are MCP-side, reflecting what dogfood actually needed.
- **`signatory serve`.** Pipeline message service with start/stop/status/restart/logs subcommands.
- **`signatory handoff`.** Security / provenance / synthesist templates, `--network-precheck` (now npm-aware), `--clone-dir`, `--json`.
- **`signatory ingest` + `format-check`.** Analyst output validation and ingestion.
- **`/analyze` skill.** LLM-orchestrated pipeline: session → handoff → parallel analyst dispatch → verify → synthesize. Candidate for retirement in M8 (see below).
- **Git signal collector.** Identity signals (mailmap, domain, concentration), commit-signing, tag-signing, first-commit-date.
- **v0.1 architectural invariants** (Invariant 1–4) with CI enforcement.
- **109+ tests** with race detection, pre-commit hooks (gofmt + vet + race).

## V0.1 — Remaining

V0.1 blocks until every item in this section is complete. Order within each subsection is load-bearing; order across subsections is not.

### Agent-facing contract milestones

Eight milestones from [`agent-facing-contract.md`](agent-facing-contract.md). Ordered by dependency — earlier milestones unblock later ones.

- **M1 — Target grammar unification.** `@version` parsed as first-class on `pkg:` URIs; `--version` flag becomes an override with conflict detection. Per-version burns fall out for free. ~200 LOC + ~300 LOC tests.
- **M5 — File-based free-text inputs.** `--rationale-file`, `--intake-file`, `-` for stdin. Kills the heredoc-gymnastics invocation pattern. ~100 LOC + ~200 LOC tests.
- **M4 — Undo verbs + dry-run.** `posture unset`, `burn remove`, `ingest withdraw` (soft-delete as `INGEST_ERROR` status). `--dry-run` on every mutating verb. EX_USAGE exit codes. ~250 LOC + ~350 LOC tests.
- **M3 — Ecosystem resolver registry.** `pkg:<eco>/<name>` → declared source as a pluggable capability. npm bridge ported in; Go resolver added. Replaces ad-hoc branches in precheck / clone / handoff. ~400 LOC + ~500 LOC tests.
- **M2 — Identity-indexed storage.** Rows keyed under the caller's URI with `collected_from` links to resolved source. Drop-and-recollect migration (no backfill logic). ~300 LOC + ~400 LOC tests.
- **M7 — `signatory summary` verb.** CLI + MCP. One-call view: canonical URI + `collected_from` + posture + burn + analysis rollup + proposed postures. Consumed by M6 and the /analyze-skill short-circuit. ~300 LOC + ~400 LOC tests.
- **M6 — Synthesist contract.** Structured evidence in handoff body (no filestore browsing), `proposed_posture` in deposit schema, filestore markdown becomes a view. Same cross-pollination prohibition added to security + provenance templates. `signatory posture accept <synthesis-id>`. ~500 LOC + ~600 LOC tests.
- **M8 — `/analyze` retirement.** Port the 150-line bash orchestration into `signatory run analysis <target>`. Human and LLM invoke the same entry point. /analyze SKILL.md becomes a pointer. ~400 LOC + ~500 LOC tests.

### Ecosystem providers

- **PyPI ecosystem provider.** `requirements.txt`, `setup.py`, `pyproject.toml` parsers. PyPI registry client + source resolution. Second-highest-risk ecosystem after npm. Can ship in parallel with contract milestones — different file tree.

### Packaging / polish

These are nice-to-have and may slip to v0.1.1 if the contract milestones take longer than estimated:

- Signal TTL/expiry — cache works but doesn't auto-expire
- Docker packaging — `go install` works but Docker is the stated MVP target
- OpenSSF Scorecard integration — additive signal source

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
