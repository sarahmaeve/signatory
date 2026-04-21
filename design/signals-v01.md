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
| **Publish-environment fingerprint drift** | **npm / PyPI registry** | **User-agent / time-zone / publish-cadence drift on a dormant slot's first new publish. Forgery-resistant: registry-recorded server-side. Context.ai-incident-motivated (2026-04-21 threat-landscape entry).** |
| **Publisher set churn** | **npm / PyPI registry** | **npm `maintainers` list or PyPI collaborator set changed in the last N days. Registry-observable, tamper-evident. Post-takeover leading indicator.** |

### "Is their identity-surface locked down?" (Identity-Surface Exposure)

Orthogonal to "Identity and Governance" — that group verifies *who the
maintainer is*; this group observes *how attackable the maintainer's
identity surface is, independent of how verified the identity is*.
Motivated by the 2026-04-21 Vercel / Context.ai incident (see
[`threat-landscape/2026-04-21-vercel-contextai-incident.md`](threat-landscape/2026-04-21-vercel-contextai-incident.md)),
where a third-party SaaS OAuth compromise propagated through identity
to reach a position adjacent to the package-publish surface, *without*
any weakness in the identity-verification axis.

| Signal | Source | Notes |
|--------|--------|-------|
| Publish-authority structure | Registry + repo | Single-key (identity alone) vs split (identity + trusted-publisher OIDC). Determines whether identity compromise → publish compromise is one hop or two. |
| Primary commit-email domain posture | DNS + commit metadata | DMARC reject, MTA-STS, SPF strictness on the maintainer's primary commit-author email domain. Personal webmail vs. Workspace with admin revocation. |
| Commit-author email change | Git log | Primary commit-author email changed in the last N days. Registry/log-observable, tamper-evident. Post-takeover indicator. |
| Third-party workflow action exposure | `.github/workflows/*.yml` | Enumerate `uses:` entries; classify by publisher. Third-party actions with `GITHUB_TOKEN` write scope are OAuth-equivalents. Count + composition = repo identity-side attack surface. |
| Self-declared identity-surface posture | Repo (attestation file) | Project-published attestation of publish-authority structure and maintainer auth posture. Like `SECURITY.md`: presence positive, absence neutral, contradiction red-flag. |

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

### "How much depends on this working?" (Blast Radius / Network-Critical Effect)

Two nearby concepts are worth keeping separate under this heading:

- **Criticality** (ex-ante) — how much of the ecosystem, project, or
  organization *relies on this package's continued correct operation.*
  Measures position.
- **Blast radius** (ex-post) — how much damage propagates *if this
  package is compromised.* Measures consequence.

They correlate but not identically. A widely-used package with a
trivial inline replacement has high criticality and low blast radius.
A rarely-used crypto primitive baked into kernel-adjacent code has the
opposite. For the `takeover-bait` composite (see Composite Signals
below) the input we care about is *blast radius* — the attacker's ROI.
Download counts are a useful but imperfect proxy.

Signals in this group are **scope-polymorphic**. The same abstract
question ("is this load-bearing for the viewer?") has different
collectors depending on whether the viewer is the ecosystem, a single
consuming project, or an organization.

| Signal | Scope | Source | Notes |
|--------|-------|--------|-------|
| Weekly download count | registry | npm (`api.npmjs.org/downloads/point/last-week/{pkg}`) | See forgery caveat below |
| Weekly download count | registry | PyPI (pypistats.org JSON or BigQuery `the-psf.pypi.downloads`) | Rate-limited on pypistats free tier |
| Recent download count | registry | crates.io (`api/v1/crates/{name}` → `downloads`, `recent_downloads`) | Single-request clean |
| Recent download count | registry | RubyGems (`api/v1/gems/{name}.json` → `downloads`, `version_downloads`) | Clean |
| Container pull count | registry | Docker Hub (`hub.docker.com/v2/repositories/{ns}/{repo}` → `pull_count`) | |
| Release asset download count | registry | GitHub Releases API → per-asset `download_count` | For binary-distributed CLIs |
| **Go module download count** | **registry** | **No native exposure** | proxy.golang.org does not expose. deps.dev aggregates *import* counts, which is criticality, not blast radius. Collectors return `ErrUnobservable` for Go; composites must degrade gracefully. |
| Dependent package count | registry | npm / PyPI / crates.io metadata | How many registry-visible packages depend on this |
| Adoption type (refs-to-stars ratio) | registry | GitHub API + search | Distinguishes direct adoption from transitive inheritance |
| Total commit count | repo | GitHub API | Low count in old repos indicates write-once code |
| Commit activity distribution | repo | GitHub API | Gaps, bursts, cosmetic-vs-substantive recent activity |
| Presence in consumer's dep tree | consumer | Local manifest | Direct vs. transitive, depth |
| Dependency role | consumer | Local manifest + analysis | Runtime, validation, build-only, or development — determines local blast radius |
| Call-graph invocation centrality | consumer / org | *(deferred v0.2)* | Import-count × dependency-role for a consuming project; invocation tracing through service mesh / APM for org scope. Same observation name, collector shape depends on available observability. |

**Forgery resistance of download/pull counts is MEDIUM, not HIGH.**
Registry-reported download figures can be inflated by automation —
both maliciously (bot-farms gaming an attack target's perceived
adoption) and incidentally (CI pipelines pulling on every build). The
number reported by the registry is tamper-evident *at the registry*
(you can't forge npm's response), but the underlying count does not
rebuff adversarial inflation. The figure should be used as a *magnitude
indicator*, not a precise metric, and any composite that uses it
inherits MEDIUM forgery resistance from this input — see the note on
`takeover-bait` in the Composite Signals section. The error bias is
helpful (false-positive on a gamed-high-download package is lower-cost
than false-negative on a genuinely popular one), but the degradation
from HIGH should be stated explicitly, not silently absorbed.

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
- ~~Maintainer email/metadata change tracking~~ — **promoted** to the
  Identity-Surface Exposure group (commit-author email change) and
  Publication Integrity (publisher set churn) after the 2026-04-21
  Vercel / Context.ai incident established the attack axis
- Dependency seeding pattern detection (clean version published to build
  false history before weaponized version)

The provider interfaces should be designed to accommodate these signals
when they are implemented.

## Composite Signals (View Layer)

A **composite signal** is a named derived observation over base signals
— present/absent rather than quantitative. The parallel is a SQL view
over underlying tables: the view adds a name, a selection rule, and a
meaning the base rows do not individually carry. Composites do not
replace base signals; they expose recurring multi-signal patterns as
first-class consumer-subscribable objects.

Composites belong in the signal layer, not the conclusion layer,
because they are mechanically derivable from base signals and
forgery resistance is inherited from their inputs. They are the place
in the model where "burn this slot the moment anything twitches" lives
as a structured commitment, not as free-text rationale buried in a
posture decision.

### takeover-bait (v0.1-derivable)

Fires when a package simultaneously exhibits:

- `network_critical_effect` — a scope-polymorphic blast-radius signal
  from the "How much depends on this working?" group. Registry scope:
  downloads / pulls / dependents above ecosystem threshold. Consumer
  scope: load-bearing in a critical call path. Org scope: invocation
  centrality above threshold. See that group's collector matrix for
  per-ecosystem endpoints and the Go `ErrUnobservable` gap.
- `vitality_unmaintained` — last publish > N years stale (N ≥ 3 for
  npm/PyPI; adjust per ecosystem convention)
- `bus_factor_low` — single-maintainer publish authority
- `trusted_publisher_absent` — no OIDC-bound publish binding

**Forgery resistance:** MEDIUM, dominated by the
`network_critical_effect` input. Three of the four inputs
(`vitality_unmaintained`, `bus_factor_low`, `trusted_publisher_absent`)
are HIGH — registry- or repo-observable and tamper-evident. The
`network_critical_effect` input is MEDIUM when sourced from registry
download / pull counts, which are gameable by automation (see the
caveat at the end of the "How much depends on this working?" group).
Per the composition rule that composites inherit the lowest
forgery resistance of their inputs, `takeover-bait` is MEDIUM. The
error bias is helpful (false-positive on a gamed-high-download target
is lower-cost than false-negative on a genuinely popular one), but the
drop from HIGH should be explicit when the composite is presented to
consumers. A consumer-scope or org-scope collector that sources
`network_critical_effect` from call-graph centrality or invocation
tracing (v0.2) would restore HIGH for deployments where that
observability is available.

**Meaning:** the package is acceptable to install *today* and is a
high-value target for future-publish compromise. A consumer accepting
this composite is implicitly subscribing to monitor:

- Any new publish under the package slot
- Any change in the maintainer / publisher set (see Publication
  Integrity: publisher set churn)
- Any publish-environment fingerprint drift
- Any identity-surface event touching the maintainer (commit-author
  email change, account recovery activity if observable)

**Intended posture-evaluator interaction:** a target with
`takeover-bait` present should be annotated with the monitoring
commitment explicitly, independent of the chosen posture tier. This
resolves the semantic overload observed in recent dogfood analyses
where `trusted-for-now` was carrying both "safe as installed" and
"burn the slot on any twitch." See
[`threat-landscape/2026-04-21-vercel-contextai-incident.md`](threat-landscape/2026-04-21-vercel-contextai-incident.md)
§"Impact on posture-tier semantics."

### identity-surface-exposed (planned)

A composite over the Identity-Surface Exposure signal group: fires when
the identity-surface signals cluster below threshold (single-key
publish authority, weak email-domain posture, third-party workflow
action sprawl). Not specified for v0.1; specification depends on
accumulating identity-surface observations on enough targets to
calibrate threshold values.

### Composition rules

- Composites are **derived**, not ingested. An analyst does not "emit a
  takeover-bait conclusion"; the composite is computed over the
  conclusions/signals the analyst already produced.
- Composites are **stable across rounds**. Supersession applies to the
  base signals; the composite re-derives.
- Composites **inherit the lowest forgery resistance of their inputs.**
  `takeover-bait` is HIGH because every input is HIGH; a composite that
  mixed a HIGH input with a LOW input would be LOW.
- Composites are **ecosystem-aware where their inputs are.** The
  threshold for `vitality_unmaintained` is not the same for a Go stdlib
  module and an npm utility.

## Signal Composition

No single signal is definitive. Signals are presented individually and
composed into entity profiles. The axios case study demonstrated 17
independent signals converging on a single incident — any one suspicious
in isolation, damning in combination.

Scoring is explicitly not built into v0.1. Raw signals are exposed via
CLI (JSON output) and MCP. Users define their own scoring criteria.
Composite signals (see above) are the sanctioned place to name
recurring patterns without drifting into scoring.
