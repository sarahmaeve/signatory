# modernc.org/sqlite (gitlab.com/cznic/sqlite)

**Role: Runtime**
**Decision: Trusted-for-now**
**Date: 2026-04-09**

## Dependency Role

Runtime dependency — compiled into the production binary as the
persistence layer (SQLite store implementation). A compromise of this
package would mean arbitrary code execution in signatory itself.

Selected over the CGO alternative (`mattn/go-sqlite3`) specifically
because the pure Go property means `go install` works everywhere
without a C compiler — critical for CLI tool adoption.

## Signal Table

| Signal | Value | Assessment |
|--------|-------|------------|
| Purpose | Pure Go (CGO-free) SQLite implementation | Runtime persistence layer |
| Owner | cznic (individual) | GitHub since 2011, GitLab since 2018, 994 followers |
| Primary repo | gitlab.com/cznic/sqlite | GitHub mirror is archived |
| Created | November 2018 | 7.5 years old |
| Last commit | April 8, 2026 | 1 day before evaluation — very active |
| Total commits | 643 | Healthy for age |
| Latest release | v1.48.2 (April 8, 2026) | Rapid cadence |
| Release cadence | 5 releases in 3 weeks (v1.46.2–v1.48.2) | Very active |
| Contributors | cznic (primary), Josh Bleecher Snyder (active) | Bus factor ~2 |
| Stars | ~345 (GitHub, archived) | Moderate (GitLab stars not queryable) |
| Commit signing | **None** | No verified commits |
| Temporal era | Modern AI (active through 2026) | Current |
| Org affiliation | Individual (@modernc_org on X) | No corporate backing |
| Adoption | 9,872 go.mod refs, 3,518+ known importers | Strong |
| Refs-to-stars ratio | 28.6:1 | High transitive adoption (partially misleading — GitHub stars on archived mirror) |
| Notable importers | FerretDB, Service Weaver, SigNoz, Syft, Gatus | Production infrastructure tools |
| License | BSD 3-Clause | Permissive |
| Retracted versions | 7 | Past publish quality issues; retraction is responsible |
| Self-documented fragility | `modernc.org/libc` must version-match exactly | Project warns about its own tight coupling |

## Author Provenance (cznic)

- GitHub account since April 2011 (15 years), 58 public repos
- GitLab since June 2018, @modernc_org on X
- 994 GitHub followers — significant for an individual
- Author of the entire modernc.org ecosystem (libc, memory, mathutil,
  fileutil) — this is a deep, long-running technical project: transpiling
  C to Go to provide pure Go implementations
- Known in the Go community as the primary CGO-free SQLite provider
- Cross-platform presence: GitHub + GitLab + X

## Transitive Dependency Health

| Dep | Type | Owner | Status |
|-----|------|-------|--------|
| `modernc.org/libc` v1.70.0 | Direct | cznic | Active, **fragile** (exact version match required) |
| `modernc.org/mathutil` v1.7.1 | Direct | cznic | Active |
| `modernc.org/fileutil` v1.4.0 | Direct | cznic | Active |
| `modernc.org/memory` v1.11.0 | Indirect | cznic | Active |
| `golang.org/x/sys` v0.42.0 | Direct | Go team | Go team maintained |
| `github.com/google/pprof` | Direct | Google | Google maintained |
| `github.com/dustin/go-humanize` | Indirect | Individual | 4K stars, last pushed 2024 |
| `github.com/google/uuid` | Indirect | Google | Google maintained |
| `github.com/mattn/go-isatty` | Indirect | Individual | Active |
| `github.com/remyoudompheng/bigfft` | Indirect | Individual | **3 years fallow** |
| `github.com/ncruces/go-strftime` | Indirect | Individual | Niche |

**Concentration risk:** 4 of 5 direct dependencies and 1 indirect are
from cznic. A compromise of cznic's GitLab account would affect the
entire modernc.org stack simultaneously. This is correlated risk, not
independent risk.

## Trust Model Assessment

| Signal group | Assessment |
|---|---|
| Vitality | Strong positive. Commits yesterday, 5 releases in 3 weeks, active bug fixes from multiple contributors |
| Governance | **Weak.** Single primary maintainer, one active contributor. No org, no governance docs, no succession plan. All modernc.org/* deps are the same person. |
| Publication integrity | Tags with regular cadence. **No signed commits.** 7 retracted versions (responsible behavior but indicates past quality issues). |
| Hygiene | CHANGELOG maintained, active bug fixes. CI visibility limited (GitLab). |
| Criticality | High. 9,872 go.mod refs, used by FerretDB, SigNoz, Service Weaver. |
| Forgery resistance | Long account tenure (2011) is very high. **No commit signing is a gap.** Cross-platform presence is positive. |

## Gaps and Concerns

1. **Single maintainer with no succession plan.** cznic owns the entire
   modernc.org ecosystem. If unavailable, the entire pure Go SQLite
   stack is unmaintained.
2. **No commit signing.** Zero verified commits across entire history.
3. **GitLab hosting limits signal collection.** Less visibility into
   contributor graphs, issue counts, PR review patterns than GitHub.
   Less visibility means less scrutiny.
4. **Large transitive dependency tree** concentrated in one author's
   packages. Correlated failure/compromise risk.
5. **`modernc.org/libc` is self-flagged as "fragile"** — version must
   match exactly. Tight coupling and maintenance burden.
6. **7 retracted versions** — responsible behavior, but indicates past
   publish instability.
7. **Refs-to-stars ratio of 28.6:1** suggests significant transitive
   adoption where consumers haven't independently evaluated this package.

## Risk Assessment

This is signatory's highest-risk dependency by several measures:
- Runtime (in the production binary)
- Deep transitive dependency tree concentrated in one person
- No commit signing
- Hosted on a platform where signal collection is weaker
- Single maintainer with no succession plan

Mitigating factors:
- Adoption by serious projects (FerretDB, SigNoz, Service Weaver)
  provides some confidence — these teams did their own evaluations
- 15-year account tenure is a very strong forgery-resistant signal
- The Store interface insulates signatory from this dependency — an
  alternative backend (mattn/go-sqlite3 with CGO, or even S3) could
  replace it without touching the rest of the codebase
- The pure Go property is a hard requirement for our distribution model;
  no CGO alternative satisfies this constraint

## Comparison with Alternative

| | modernc.org/sqlite | mattn/go-sqlite3 |
|---|---|---|
| Stars | ~345 | 9,041 |
| go.mod refs | 9,872 | 26,304 |
| Requires CGO | **No** | Yes |
| Cross-compile | Easy | Hard |
| `go install` works | Yes | Requires C compiler |
| Maintainer depth | 1-2 | Broader |

We chose modernc specifically to avoid CGO. The trade-off is real:
fewer eyes, deeper dependency tree, single maintainer.

## Decision

**Posture: trusted-for-now**

The pure Go property is a hard requirement. No alternative exists that
satisfies this constraint. The project is actively maintained with rapid
release cadence, and adoption by production infrastructure tools provides
passive scrutiny. But the governance gap is the most significant we've
seen in any dependency.

Rationale for not vetted-frozen: single maintainer with no succession
plan, no commit signing, concentrated transitive dependencies, limited
signal visibility on GitLab, and we have not audited the source.

## Action Items

- Pin to v1.48.2 in go.mod (exact version)
- Monitor cznic's GitLab activity and modernc.org ecosystem health
- The Store interface already insulates us — if the project shows signs
  of abandonment, evaluate mattn/go-sqlite3 with CGO as a fallback
- Watch for community governance improvements (succession planning,
  commit signing adoption)
- Consider periodic `go mod verify` to check module integrity
