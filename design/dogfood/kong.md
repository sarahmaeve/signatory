# Kong (github.com/alecthomas/kong)

**Role: Runtime**
**Decision: Trusted-for-now**
**Date: 2026-04-09**

## Signal Table

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
| Release cadence | v1.12 to v1.15 in reasonable timeframe | Regular, disciplined |
| CI/CD | Present, Renovate bot for dependency updates | Good hygiene |

## Author Provenance (Alec Thomas)

- GitHub account since December 2008 (17+ years)
- 175 public repositories
- Author of well-known Go projects: `participle` (parser library),
  `chroma` (syntax highlighter, used by Hugo), `kingpin` (Kong's
  predecessor)
- Long, consistent track record across the Go ecosystem
- Cross-platform identity consistency — well-known in Go community

## Trust Model Assessment

| Signal group | Assessment |
|-------------|------------|
| Vitality | Strong positive. Commits within last week, active PR review |
| Governance | Single primary maintainer with active community. Better bus factor than mousetrap but still individual-dependent |
| Publication integrity | Tags correspond to verified commits. Regular release cadence |
| Hygiene | CI present, Renovate bot, signed commits |
| Criticality | Moderate (~3K stars, ~2K dependents). Lower blast radius than Cobra — arguably a positive |
| Forgery-resistant signals | Long account tenure (2008), known ecosystem author, verified commits, zero deps |

## Gaps

- No institutional affiliation — one person's project, despite strong
  personal reputation
- Would benefit from deeper vetting if we move to vetted-frozen

## Decision

**Posture: trusted-for-now**

Strong vitality, strong author provenance, zero runtime dependencies,
verified commits. The zero-dependency property is particularly important
for a supply chain security tool — Kong adds no transitive trust surface.

Revisit for deeper vetting when signatory itself has the tooling to
automate the assessment.
