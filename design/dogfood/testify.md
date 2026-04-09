# testify (github.com/stretchr/testify)

**Role: Validation dependency**
**Decision: Trusted-for-now**
**Date: 2026-04-09**

## Dependency Role

Testify runs during `go test` in CI and on developer workstations. It is
never compiled into the production binary. However, it executes in build
environments with access to secrets, tokens, and CI credentials — the
same environments targeted by the prt-scan campaign.

While testify is not in our shipped code, it is a critical part of how we
**validate** our code. A compromise of testify could silently corrupt test
results (making bad code appear to pass) or steal CI secrets. Its role in
our trust chain is different from a runtime dependency, but not negligible.

## Signal Table

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

## Governance Detail

Testify has a formal EMERITUS.md documenting 7 former maintainers. This
indicates intentional succession planning — a governance signal that
most Go projects lack. Active maintainers include @dolmen (110
contributions) and @brackendawson (111 contributions), with the original
creator (@matryer) on the emeritus list.

## Transitive Dependency Health

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

## Trust Model Assessment

| Signal group | Assessment |
|-------------|------------|
| Vitality | Strong positive. Active development, recent releases, community PRs |
| Governance | Strong positive. Organization-owned, formal emeritus process, deep maintainer bench |
| Publication integrity | Tags with verified commits, regular release cadence |
| Hygiene | CI present, tests against current Go versions |
| Criticality | **Very high** (~26K stars, ~79K dependents). Near-universal in Go. A compromise would affect virtually every Go project's CI. Criticality amplifies both trust and vigilance requirements. |
| Forgery-resistant signals | Organization ownership (2012), 13+ year history, formal governance, massive adoption providing broad scrutiny |

## Risk Assessment

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

## Decision

**Posture: trusted-for-now**

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

## Action Items

- Pin to v1.11.1 in go.mod (exact version, no caret/tilde)
- Monitor go-spew and go-difflib for activity or compromise signals
- Revisit posture when signatory can automate this assessment
