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
- **`signatory handoff`.** Security / provenance / synthesist templates, `--network-precheck` (now npm + Go aware via M3), `--clone-dir`, `--json`.
- **`signatory ingest` + `format-check`.** Analyst output validation and ingestion.
- **`/analyze` skill.** LLM-orchestrated pipeline: session → handoff → parallel analyst dispatch → verify → synthesize. Candidate for retirement in M8 (see below).
- **Git signal collector.** Identity signals (mailmap, domain, concentration), commit-signing, tag-signing, first-commit-date.
- **v0.1 architectural invariants** (Invariant 1–4) with CI enforcement.
- **109+ tests** with race detection, pre-commit hooks (gofmt + vet + race).
- **Agent-facing-contract M1** — `pkg:<eco>/<name>@<version>` first-class grammar with per-version identity; `--version` override with conflict detection. Per-version burns fall out for free.
- **Agent-facing-contract M5** — `--rationale-file`, `--reason-file`, `--intake-file` for multi-line inputs (path or `-` for stdin). Replaces bash-heredoc gymnastics for agent invocations.
- **Agent-facing-contract M4** — `posture unset`, `burn remove`, `--dry-run` on all mutators, `EX_USAGE` (64) exit codes on flag conflicts. Soft-delete semantics with audit-log preservation. (ingest withdraw deferred — separate narrower commit.)
- **Agent-facing-contract M3** — `internal/ecosystem/resolver/` registry with pluggable `Resolver` interface. npm + Go resolvers shipped; `applyNetworkPrecheck` routes every `pkg:<eco>/` target through the registry. Go uses offline path-prefix rules covering github.com/, golang.org/x/, gopkg.in/.
- **Agent-facing-contract M2** — identity-indexed storage. `analyst_outputs` gains `collected_from_entity_id` (migration v7). `IngestAnalystOutput` accepts `WithPrimaryTarget(uri)` to index under the caller's identity while capturing the analyst-stated target as collected_from. `ListAnalystOutputs` walks both URIs so `pkg:npm/X` and its resolved `repo:github/Y` find the same analysis. CLI `ingest --as`; MCP `signatory_ingest_analysis` `collected_from`.
- **Agent-facing-contract M7** — `signatory summary` verb (CLI + MCP). One-call view: canonical URI + related URIs + posture snapshot + burn snapshot + per-analyst rollup with severity-bucketed conclusion counts. Replaces the cross-tool flail (show-analyses → show-conclusions → posture get → burn list) yesterday's synthesist dogfood exposed. Composes on top of M2's cross-URI walk so related identities surface both directions.
- **Agent-facing-contract M6** — synthesist contract. Shipped 2026-04-21 as five sub-milestones (M6a-e, see [`m6-synthesis-contract.md`](m6-synthesis-contract.md)):
  - **M6a** — `SynthesisSupplement` struct on `AnalystOutput`; validator-gated to synthesist role (prefix `signatory-synthesis`); migration v8 adds `synthesis_supplement_json` + denormalized `proposed_tier` / `proposed_version_scope` columns; `GetSynthesisProposal` helper.
  - **M6b** — `internal/synthesis/` evidence assembler: sibling to summary but returns full conclusion bodies, positive absences, observations, and cross-URI hops for the synthesist handoff. D9 cross-pollination filter (no prior syntheses in the evidence) enforced at the data layer.
  - **M6c** — `signatory handoff synthesist` now inlines the full structured evidence as `{EVIDENCE_JSON}`. Synthesist template rewritten around "inputs = evidence block"; D9 independence-rule fence added to all 6 analyst/synthesist templates with a CI-enforced presence check. `signatory handoff` refuses to emit a synthesist handoff against a target with zero analyses.
  - **M6d** — `signatory posture accept <output-id>` promotes a synthesist's proposed posture into a recorded posture row, with optional `--tier` / `--version` / `--rationale` overrides. Deviations are captured in the audit detail as `proposed_*` fields (presence = deviation); `accepted_from_synthesis_id` links every accepted posture back to its source synthesis.
  - **M6e** — `signatory show-synthesis <output-id>` renders a synthesis output as markdown (the filestore markdown is now a view, not the source). `/analyze` skill Step 4 flips synthesist to land via MCP ingest; Step 5 routes through `posture accept`. Synthesist allowed-tools tightened to `WebFetch mcp__signatory__signatory_ingest_analysis` — CI-enforced.

## V0.1 — Remaining

V0.1 blocks until every item in this section is complete. Order within each subsection is load-bearing; order across subsections is not.

### Agent-facing contract milestones

Seven of eight milestones shipped (M1, M5, M4, M3, M2, M7, M6 — see Shipped section above). Remaining, ordered by dependency:

- **M8 — `/analyze` retirement.** Port the 150-line bash orchestration into `signatory run analysis <target>`. Human and LLM invoke the same entry point. /analyze SKILL.md becomes a pointer. ~400 LOC + ~500 LOC tests.
- **Follow-up: ingest withdraw.** Narrower commit on top of M4. analyst_outputs carries append-only triggers from v3, so marking an output INGEST_ERROR needs a sibling-table design (analyst_output_withdrawals) meaningfully different from the posture/burn withdrawal shape. Deferred intentionally; current needs covered.
- ~~**Follow-up: /analyze skill update to pass --as.**~~ Shipped 2026-04-21. Both analyst prompts now carry `collected_from: "{TARGET}"` when calling `signatory_ingest_analysis`; the orchestrator substitutes `$TARGET` into the prompt at dispatch time. Step 3 verification now queries under `$TARGET` to match. Closes the M2 dogfood loop until M8 retires the skill.

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
