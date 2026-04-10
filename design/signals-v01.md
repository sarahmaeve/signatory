# Signatory: V0.1 Signal Set

Finalized signal set for the v0.1 proof of concept. These signals were
validated against the axios supply chain attack case study (March 2026)
and derived from the trust model's core principles.

## Signal Groups

### "Is anyone home?" (Project Vitality)

| Signal | Source | Notes |
|--------|--------|-------|
| Last commit date | GitHub API | Temporal era classification applied |
| Release cadence / last publish date | npm registry | Gap detection vs. historical pattern |
| Contributor count and trend | GitHub API | Bus factor signal |
| Open issue/PR responsiveness | GitHub API | Mean time to first response |

### "Who's responsible?" (Identity and Governance)

| Signal | Source | Notes |
|--------|--------|-------|
| Maintainer count and tenure | GitHub API + npm registry | Cross-referenced |
| Org affiliation | GitHub API | Verified org membership |
| Commit signing presence | GitHub API | GPG/SSH verified commits |
| Maintainer account age | GitHub API | Young accounts on critical packages are anomalous |
| Source control platform | Repo URL | GitHub, GitLab, Gitea, SourceHut, etc. — platform affects signal collection depth |
| Dependency author concentration | go.mod / manifest | Multiple deps from same author = correlated compromise risk |

### "How was this published?" (Publication Integrity)

Derived primarily from the axios case study. These signals would have
caught the attack at the publication layer, before any code executed.

| Signal | Source | Notes |
|--------|--------|-------|
| Publication metadata consistency | npm registry | Compare publish patterns across versions |
| Git tag correspondence | GitHub API + npm registry | Published version has no matching tag/commit trail |
| **Git tag SHA stability** | **GitHub API** | **Track tag → SHA over time. SHA change on existing tag = force-push attack (Trivy case study)** |
| New/changed dependencies between versions | npm registry | Unexpected new dependency = high-priority anomaly |
| Dormant version branch activity | npm registry | Publication to a long-inactive version line |
| Trusted publisher binding | npm registry | OIDC-based provenance presence/absence |
| **`.pth` file presence (PyPI)** | **PyPI registry** | **Python interpreter-time execution vector (LiteLLM case study)** |
| **Source code divergence vs git tag** | **Registry + GitHub** | **Published package contents differ from the git tag they claim to be** |

### "Does it look like they care?" (Hygiene)

| Signal | Source | Notes |
|--------|--------|-------|
| CI configuration present | GitHub API | Actions, Travis, CircleCI, etc. |
| OpenSSF Scorecard | Scorecard API | Consumed as a composite signal source |
| Linter/formatter config present | GitHub API | File existence check in repo root |

### "What's the consumer's posture?" (Self-Assessment)

Signals about the consuming project, not the dependency.

| Signal | Source | Notes |
|--------|--------|-------|
| Version pinning discipline | Local manifest | Exact versions vs. `^`/`~` ranges |
| Lockfile presence and consistency | Local filesystem | package-lock.json exists, is committed |

### "How critical is this?" (Criticality as Multiplier)

Criticality amplifies all other signals. High criticality + any anomaly =
immediate investigation. See trust-model.md for the multiplier framework.

| Signal | Source | Notes |
|--------|--------|-------|
| Weekly download count | npm registry | Raw adoption metric |
| Dependent package count | npm registry | How many packages depend on this |
| Presence in consumer's dep tree | Local manifest | Direct vs. transitive, depth |
| Dependency role | Local manifest + analysis | Runtime, validation, build-only, or development — determines blast radius |
| Adoption type (refs-to-stars ratio) | GitHub API + search | Distinguishes direct adoption from transitive inheritance |
| Total commit count | GitHub API | Low count in old repos indicates write-once code |
| Commit activity distribution | GitHub API | Gaps, bursts, cosmetic-vs-substantive recent activity |

## Temporal Era Classification

Applied automatically to all entities based on commit/publish timestamps:

| Era | Date Range | Interpretation |
|-----|-----------|----------------|
| Pre-LLM | Before 30 Nov 2022 | Human-authored, never AI-reviewed |
| Early LLM | 30 Nov 2022 — 24 Nov 2025 | Possible AI involvement, limited capability |
| Modern AI | After 24 Nov 2025 | Assume AI could have authored or reviewed |

## Deferred Signals (Post-V0.1)

These are high-value but require deeper implementation work:

- Install script analysis (postinstall hook detection and change tracking)
- Distribution integrity (published package vs. source repo comparison)
- Anti-forensic behavior detection (self-modifying packages)
- Cross-ecosystem correlation (detecting coordinated campaigns)
- Maintainer email/metadata change tracking
- Dependency seeding pattern detection (clean version published to build
  false history before weaponized version)

The provider interfaces should be designed to accommodate these signals
when they are implemented.

## Signal Composition

No single signal is definitive. Signals are presented individually and
composed into entity profiles. The axios case study demonstrated 17
independent signals converging on a single incident — any one suspicious
in isolation, damning in combination.

Scoring is explicitly not built into v0.1. Raw signals are exposed via
CLI (JSON output) and MCP. Users define their own scoring criteria.
