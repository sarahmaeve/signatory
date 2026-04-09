# Signatory: Dependency Decisions (Dogfooding)

Signatory applies its own trust model to its own dependencies. This
document records posture decisions for each dependency we adopt, serving
as both a record and a test of the trust model.

## Decision: CLI Framework

### mousetrap (github.com/inconshreveable/mousetrap)

**Transitive dependency of Cobra. Investigated before adoption.**

| Signal | Value | Assessment |
|--------|-------|------------|
| Purpose | Detect Windows Explorer launch | Windows-only, no-op elsewhere |
| Owner | Alan Shreve (inconshreveable, ngrok creator) | Known identity, 2055 followers, account since 2011 |
| Created | April 2014 | 12 years old |
| Last commit | November 2022 | 3.5 years fallow |
| Contributors | 3 (16 commits from owner) | Bus factor = 1 |
| Stars | 269 | Low; mostly pulled via Cobra |
| Commit signing | Mixed | Recent verified, older unsigned |
| Temporal era | Pre-LLM (last activity Nov 2022) | Never AI-reviewed |
| Org affiliation | None | Individual maintainer |

**Risk assessment:** Fallow, single-maintainer, Windows-only code pulled
into every Cobra-based project regardless of target platform. The module
is in go.mod unconditionally even though the code is behind a Windows
build tag. If this account were compromised, the blast radius would
include every Cobra-based CLI tool — kubectl, Docker CLI, Hugo, and
thousands of others.

**Decision:** Avoid. Evaluate CLI frameworks with zero or minimal runtime
dependencies.

### CLI Framework Comparison

| Framework | Runtime deps | Mousetrap? | Notes |
|-----------|-------------|------------|-------|
| Cobra | 4 (mousetrap, pflag, go-md2man, yaml) | Yes | Industry standard, but heavy dep tree |
| Kong | 0 | No | Struct-based, well-maintained |
| urfave/cli v3 | 0 | No | Functional style, widely used |
| stdlib flag | 0 | No | No subcommand support without manual work |

**Selected: Kong** (`github.com/alecthomas/kong`)

Rationale: A supply chain trust tool should have a minimal, auditable
dependency tree. Zero runtime dependencies for the CLI framework is
preferable when capable alternatives exist. Kong's trust profile is
strong (see assessment below).

### Kong (github.com/alecthomas/kong)

**Evaluated before adoption. Recommended posture: trusted-for-now.**

| Signal | Value | Assessment |
|--------|-------|------------|
| Purpose | CLI argument parser for Go | Core infrastructure for our tool |
| Owner | Alec Thomas (alecthomas) | Well-known Go ecosystem author, 1,419 followers, account since Dec 2008 |
| Created | April 2018 | 8 years old |
| Last commit | April 1, 2026 | Actively maintained (8 days before evaluation) |
| Latest release | v1.15.0 | Tagged to a verified commit |
| Contributors | 10+ meaningful, 271 commits from owner | Healthy contributor base |
| Community PRs | Multiple external contributors, reviewed and merged by maintainer | Active review process |
| Stars | 3,023 | Strong adoption |
| Commit signing | All recent commits verified | Consistent |
| Open issues | 41 | Normal for active project |
| Temporal era | Modern AI (active through 2026) | Current |
| Org affiliation | Individual | No corporate backing |
| Runtime deps | **Zero** | Exemplary — strongest possible signal for a supply chain tool |
| Adoption | ~2,000 go.mod references on GitHub | Moderate, growing |
| Release cadence | v1.12 → v1.15 in reasonable timeframe | Regular, disciplined |
| CI/CD | Present, Renovate bot for dependency updates | Good hygiene |

**Author provenance (Alec Thomas):**
- GitHub account since December 2008 (17+ years)
- 175 public repositories
- Author of well-known Go projects: `participle` (parser library),
  `chroma` (syntax highlighter, used by Hugo), `kingpin` (Kong's
  predecessor)
- Long, consistent track record across the Go ecosystem
- Cross-platform identity consistency — well-known in Go community

**Trust model assessment:**

| Signal group | Assessment |
|-------------|------------|
| Vitality | Strong positive. Commits within last week, active PR review |
| Governance | Single primary maintainer with active community. Better bus factor than mousetrap but still individual-dependent |
| Publication integrity | Tags correspond to verified commits. Regular release cadence |
| Hygiene | CI present, Renovate bot, signed commits |
| Criticality | Moderate (~3K stars, ~2K dependents). Lower blast radius than Cobra — arguably a positive |
| Forgery-resistant signals | Long account tenure (2008), known ecosystem author, verified commits, zero deps |

**Gaps:**
- No institutional affiliation — one person's project, despite strong
  personal reputation
- Would benefit from deeper vetting if we move to vetted-frozen

**Posture decision: trusted-for-now**
- Strong vitality, strong author provenance, zero runtime dependencies,
  verified commits
- The zero-dependency property is particularly important for a supply
  chain security tool — Kong adds no transitive trust surface
- Revisit for deeper vetting when signatory itself has the tooling to
  automate the assessment
