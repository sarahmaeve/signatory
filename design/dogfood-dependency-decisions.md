# Signatory: Dependency Decisions (Dogfooding)

Signatory applies its own trust model to its own dependencies. This
document records posture decisions for each dependency we adopt, serving
as both a record and a test of the trust model.

## Dependency Roles

Not all dependencies carry the same risk. A project owner should assess
the **role** a dependency plays in their project, because role determines
blast radius:

| Role | Scope | Risk Profile |
|------|-------|-------------|
| **Runtime** | Compiled into the production binary, executes in deployment | Highest risk. Compromise = production compromise. |
| **Validation** | Runs during testing and CI. Not in the production binary, but executes in build environments with access to secrets, tokens, and CI credentials. | High risk. Compromise enables CI secret theft (cf. prt-scan), supply chain pivot, or silent test corruption. |
| **Build-only** | Code generation, linting, formatting. Runs at build time, output is checked in or compiled. | Medium risk. Compromise can inject code into build artifacts. |
| **Development** | Editor tooling, local-only utilities. Never runs in CI or production. | Lower risk, but can compromise developer workstations. |

A test framework like testify is a **validation dependency** — it never
reaches production, but it executes in every CI run with full access to
the build environment. The axios attack compromised runtime dependencies;
the prt-scan attack targeted CI environments. Both vectors matter.

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

## Decision: Test Framework

### testify (github.com/stretchr/testify)

**Role: Validation dependency.** Testify runs during `go test` in CI and
on developer workstations. It is never compiled into the production binary.
However, it executes in build environments with access to secrets, tokens,
and CI credentials — the same environments targeted by the prt-scan
campaign.

**Evaluated before adoption.**

| Signal | Value | Assessment |
|--------|-------|------------|
| Purpose | Test assertions, mocks, suites for Go | Validation dependency — test-only |
| Owner | Stretchr, Inc. (organization) | Corporate-backed, est. 2012 |
| Created | October 2012 | 13+ years old |
| Last commit | March 4, 2026 | Active (~5 weeks before evaluation) |
| Latest release | v1.11.1 | Regular cadence |
| Contributors | 10+ significant, top contributor has 129 commits | Deep contributor bench |
| Stars | 25,911 | Extremely high adoption |
| Forks | 1,700 | Massive ecosystem |
| Commit signing | Most recent verified | Good |
| Open issues | 377 | High, but normal at this scale |
| Temporal era | Modern AI (active through 2026) | Current |
| Org affiliation | Stretchr, Inc. | Organization-owned |
| Adoption | ~79,000 go.mod references | Near-universal in Go ecosystem |
| Governance | EMERITUS.md tracking 7 former maintainers | Formal succession process |
| Runtime deps | objx, yaml.v3 | Both active |
| CI/CD | GitHub Actions, tests against Go 1.24 | Current |

**Governance detail:**

Testify has a formal EMERITUS.md documenting 7 former maintainers. This
indicates intentional succession planning — a governance signal that
most Go projects lack. Active maintainers include @dolmen (110
contributions) and @brackendawson (111 contributions), with the original
creator (@matryer) on the emeritus list.

**Transitive dependency health:**

| Dep | Owner | Last pushed | Stars | Assessment |
|-----|-------|-------------|-------|------------|
| `objx` | stretchr | Mar 2026 | 844 | Active, same org |
| `yaml.v3` | gopkg.in | Stable | Widely used | Stable infrastructure |
| `go-spew` | davecgh | Apr 2024 | 6,386 | **2 years fallow** — individual maintainer |
| `go-difflib` | pmezard | May 2023 | 432 | **3 years fallow** — individual maintainer |

Note: `go-spew` and `go-difflib` are the same fallow-dependency pattern
we rejected in mousetrap. However, the risk is mitigated by two factors:
(1) they are test-only transitives, never in the production binary, and
(2) they are small, stable libraries with minimal attack surface. They
are worth monitoring but do not block adoption.

Note: a dependency cycle between testify and objx required a version
exclusion (`exclude github.com/stretchr/testify v1.8.4`). This is a
known, documented workaround — not a red flag, but a complexity to be
aware of.

**Trust model assessment:**

| Signal group | Assessment |
|-------------|------------|
| Vitality | Strong positive. Active development, recent releases, community PRs |
| Governance | Strong positive. Organization-owned, formal emeritus process, deep maintainer bench |
| Publication integrity | Tags with verified commits, regular release cadence |
| Hygiene | CI present, tests against current Go versions |
| Criticality | **Very high** (~26K stars, ~79K dependents). Near-universal in Go. A compromise would affect virtually every Go project's CI. Criticality amplifies both trust and vigilance requirements. |
| Forgery-resistant signals | Organization ownership (2012), 13+ year history, formal governance, massive adoption providing broad scrutiny |

**Risk assessment:**

The primary risk is not a testify compromise reaching production — it
won't, because it's test-only. The risk is a compromised testify
executing in CI environments that hold production secrets. The prt-scan
campaign demonstrated exactly this vector: code that runs in CI can
steal AWS keys, NPM tokens, and API credentials.

Mitigating factors:
- Near-universal adoption means massive scrutiny. A malicious release
  would be detected quickly (similar to axios's 3-hour window).
- Organization ownership with formal governance reduces single-point-
  of-compromise risk compared to individual-owned projects.
- We can pin to a specific version and review updates deliberately
  rather than auto-upgrading.

**Posture decision: trusted-for-now**

Testify has the strongest governance signals of any dependency we've
evaluated: organization ownership, formal succession process, deep
maintainer bench, 13-year history, and near-universal adoption. Its
role as a validation dependency (not runtime) limits blast radius to
CI/build environments.

Rationale for not vetted-frozen: we have not audited the source code
ourselves, and the two fallow transitive dependencies (go-spew,
go-difflib) have not been examined. The near-universal adoption
provides strong passive scrutiny, but that is a different thing from
our own organizational attestation.

Action items:
- Pin to v1.11.1 in go.mod (exact version, no caret/tilde)
- Monitor go-spew and go-difflib for activity or compromise signals
- Revisit posture when signatory can automate this assessment
