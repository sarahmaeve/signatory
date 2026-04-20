# Signatory: Roadmap

## V0.1 — Must-Do

These items are required before v0.1 ships. No exceptions.

### Functional

1. **Wire `survey` command**
   Parse `go.mod` (or `package.json`/`requirements.txt`), list all
   dependencies, show posture tiers, flag burned entities, highlight
   unexamined dependencies. This is the "show me my dependency tree's
   trust posture" workflow — the dashboard entry point.

2. **npm ecosystem provider**
   Parse `package.json` and `package-lock.json`. Resolve npm packages
   to source repos. Collect npm-specific signals: publish metadata,
   install scripts, download counts, maintainer info. npm is the
   highest-risk ecosystem (axios case study).

3. **PyPI ecosystem provider**
   Parse `requirements.txt`, `setup.py`, `pyproject.toml`. Resolve
   PyPI packages to source repos (via `project_urls`). Collect
   PyPI-specific signals. PyPI is the second-highest-risk ecosystem.

4. **MCP server**
   Implement the MCP interface from `design/mcp-interface.md`:
   6 tools (`signatory_analyze`, `signatory_survey`, `signatory_set_posture`,
   `signatory_burn`, `signatory_refresh`, `signatory_detail`) and 4
   resources (`posture`, `burns`, `profile`, `unexamined`). This is
   how LLMs interact with signatory in coding workflows — a primary
   interface, not a nice-to-have.

## V0.1 — Should-Do (if time permits)

- Signal TTL/expiry — cache works but doesn't auto-expire
- Docker packaging — `go install` works but Docker is the stated MVP target
- OpenSSF Scorecard integration — additive signal source
- Remaining medium issues from adversarial reviews

## V0.2 — Planned

- Hosted/org-wide dashboard
- Federated burn list subscription/sync protocol
- GitLab signal collector (non-GitHub platform support)
- CI pipeline integration (gate merges on posture policy)
- Dependency graph visualization
- Cross-ecosystem correlation (detect coordinated campaigns)

## V0.2/Enterprise — Internal Identity and Realms

This is a likely differentiator between open-source and enterprise
offerings. Requires user testing and feedback before design.

### Concept

Internal identity registries allow organizations to mark contributors
as verified employees, contractors, or external. Combined with the
existing signal model, this enables rapid patch triage:

- Patches from `identity:internal/james.park` (verified employee)
  get a higher baseline trust than `identity:github/unknown-user123`
- Reviewer team identities (`team:internal/platform-security`) carry
  organizational authority
- Employee departure → halt identity → unreviewed patches flagged
- Credential compromise → burn identity → all touched code re-reviewed

### Realms

A realm is a namespace for identity verification:

```
identity:corp-github/james.park    -- corporate GitHub
identity:corp-gitlab/james.park    -- corporate GitLab
identity:verified/james.park       -- linked corporate identity
```

Realms solve the cross-platform identity linking problem for
organizations with multiple internal code hosting systems.

### Why Deferred

- No easy path to user testing or feedback for realm design currently
- Risk of building the wrong abstraction without enterprise users
- The v0.1 entity model (canonical URI with platform prefix) supports
  realms as a future extension without schema changes
- Internal identity is a superset of the open-source identity model —
  nothing in v0.1 prevents adding it later

### Prior Art

Similar systems have been designed for rapid PR triage in high-volume
environments, marking pull requests as privileged or suspect based on
user ID and other signals. The realm model would support this pattern.

## V0.3+ — Future

- **Signed attestation for vetted-frozen tier** (deferred 2026-04-14
  from V0.2 — heavyweight: key management, envelope schema, signing
  tool integration, embedded analysis records. Until v0.3 ships,
  vetted & frozen is internally-produced only — see
  [open-questions.md](open-questions.md) §"Vetted & frozen scope in
  early versions" and §"LLM-as-named-tool in attestation envelopes")
- Additional ecosystem providers (Go modules, crates.io, Maven)
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
- Team identity cryptographic mechanism (PGP/GPG likely, deferred to
  attestation layer)
- Internal identity realms (enterprise, needs user testing)
