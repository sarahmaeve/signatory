# Signatory: Roadmap

## V0.1 — Must-Do

These items are required before v0.1 ships. No exceptions.

### Functional

1. **Wire `survey` command**
   Parse `go.mod` (or `package.json`/`requirements.txt`), list all
   dependencies, show posture tiers, flag burned entities, highlight
   unexamined dependencies. This is the "show me my dependency tree's
   trust posture" workflow — the dashboard entry point.

2. **Entity ID normalization (#53)**
   `alecthomas/kong` and `https://github.com/alecthomas/kong` must
   resolve to the same entity. Without this, posture decisions and
   signals fragment silently across duplicate entities.

3. **npm ecosystem provider**
   Parse `package.json` and `package-lock.json`. Resolve npm packages
   to source repos. Collect npm-specific signals: publish metadata,
   install scripts, download counts, maintainer info. npm is the
   highest-risk ecosystem (axios case study).

4. **PyPI ecosystem provider**
   Parse `requirements.txt`, `setup.py`, `pyproject.toml`. Resolve
   PyPI packages to source repos (via `project_urls`). Collect
   PyPI-specific signals. PyPI is the second-highest-risk ecosystem.

5. **MCP server**
   Implement the MCP interface from `design/mcp-interface.md`:
   7 tools (`signatory_analyze`, `signatory_survey`, `signatory_compare`,
   `signatory_set_posture`, `signatory_burn`, `signatory_refresh`,
   `signatory_detail`) and 4 resources (`posture`, `burns`, `profile`,
   `unexamined`). This is how LLMs interact with signatory in coding
   workflows — a primary interface, not a nice-to-have.

### Housekeeping

6. **LICENSE file (#22)**
   Choose and add a license. Adoption blocker.

7. **`compare` command**
   Wire the stub to actually compare two entity profiles side by side.

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
- Signed attestation for vetted-frozen tier
- Dependency graph visualization
- Cross-ecosystem correlation (detect coordinated campaigns)

## V0.3+ — Future

- Additional ecosystem providers (Go modules, crates.io, Maven)
- Schema evolution (leverage migration system)
- Multiple signal source plugins
- Event-driven monitoring (subscription to change detection service)
- Kind/Kubernetes deployment
- Cloud deployment (public/private)

## Deferred Design Decisions

These require architectural discussion before implementation:

- Interface segregation for Store (#59)
- Engine constructor redesign with functional options (#6)
- Signal.Value typed structs vs. validation layer (#2)
- Authentication/authorization model for burns and posture (#3)
- Entity type inference from identifiers (#40)
- Context timeout strategy for CLI commands (#51)
