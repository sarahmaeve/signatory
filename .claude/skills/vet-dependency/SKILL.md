---
name: vet-dependency
description: Perform a supply chain trust analysis on an open-source project before adopting it. Works across ecosystems (Go, Rust, npm, PyPI, and user-facing CLIs / services). Collects signals from source forges and package registries, assesses against the signatory trust model, and produces a structured analysis document with a posture recommendation. Use when adding a new dependency, evaluating an existing one, analyzing an arbitrary open-source project, or when the user asks to vet/analyze a package.
allowed-tools: Bash Read Write Edit Glob Grep WebFetch
---

# Vet Dependency (or any open-source target)

Perform a supply chain trust analysis on an open-source project. This
skill is a prototype of `signatory analyze` — it will be superseded by
the MCP server when built.

The target is specified as $ARGUMENTS — a package coordinate (Go module
path, npm name, PyPI name, crates.io name), a GitHub / GitLab repo URL,
or a vanity module path.

## Scope

This skill was originally written for Go dependencies of signatory. It
has since been generalized to cover:

- **Go libraries** (the original dogfood path, still the most validated)
- **Rust crates** (Cargo / crates.io)
- **npm packages**
- **PyPI packages**
- **User-facing CLIs and applications** (not just libraries) — tools
  like `atuin`, `gh`, language REPLs, shell plugins, editor plugins
- **Projects hosted outside GitHub** (GitLab, Gitea, SourceHut,
  Codeberg)

If the target doesn't fit any of these cleanly, flag that up-front in
the output doc — non-obvious scope-fits are themselves a signal the
trust model needs to expand.

## Process

### 1. Identify the target

Parse $ARGUMENTS to determine:

- **Source repo** (GitHub, GitLab, or other host)
- **Package registry** if applicable (crates.io, npm, PyPI, pkg.go.dev)
- **Ecosystem** (Rust / Go / npm / Python / other)
- **Target kind** — library dependency, CLI tool, service, plugin
- **Relationship to the consumer** — direct dependency, transitive
  dependency, or "analysis only" (no consumer relationship)

**Vanity domains (Go).** Go modules often use vanity import paths
(e.g., `modernc.org/sqlite` → `gitlab.com/cznic/sqlite`). Check
`pkg.go.dev` or fetch the module's `?go-get=1` metadata to resolve the
actual repo.

**Non-GitHub hosting.** If the project is on GitLab, Gitea, SourceHut,
Codeberg, or another platform:

- Signal collection will be **less complete** — `gh api` won't work.
  Use WebFetch to scrape project pages; check if the host has a
  REST API (GitLab does, SourceHut does). Note reduced visibility in
  the output.
- The reduced tooling coverage is itself a signal: less automated
  scrutiny from the ecosystem.
- Check for a GitHub mirror and whether it's active or archived. An
  archived GitHub mirror pointing elsewhere is common (e.g.,
  `modernc.org/sqlite`).

**Archived repos.** If the GitHub repo is archived, check the
description and homepage for where development has moved. Follow the
redirect to the active host.

### 2. Determine the target's role

The blast-radius of a compromise depends on how the target runs.
Current role taxonomy (expanded beyond v0.1's library-only set):

| Role | Scope | Examples | Blast radius |
|------|-------|----------|--------------|
| Runtime | Compiled into production binary | kong, modernc/sqlite | Production compromise |
| Validation | Runs during tests / CI | testify, pytest | CI-secret theft, silent test corruption |
| Build-only | Code generation, linting, formatting | protoc, buf, goimports | Injects into build output |
| Development | Editor plugins, local-only tooling | VS Code extensions, pre-commit hooks | Dev workstation compromise |
| Shell-augment / user-input capture | Wraps a shell, REPL, or input surface | atuin, password managers, browser extensions | Per-keystroke credential harvester |
| Hosted-service coupled | Binary + SaaS; using the SaaS is a second trust decision | atuin (with hosted sync), 1password CLI | Layered — binary OR service compromise |
| Analysis only | No consumer relationship | projects we're analyzing for trust-model validation | No direct blast radius to the analyzing party |

These are not strictly mutually exclusive — a CLI can be both
"Development" and "Shell-augment". Tag accordingly. If none fits,
describe the role prose-style and flag it as a taxonomy gap in the
output doc's "Signals surfaced that didn't fit" section.

See `design/dogfood/README.md` for the original four roles and
`design/signal-storage-evolution.md` for the expanded set and its
motivation.

### 3. Collect signals from the source forge

#### 3a. GitHub-hosted projects

```
# Repo metadata
gh api repos/{owner}/{repo} --jq '{name, owner: .owner.login, owner_type: .owner.type, created_at, updated_at, pushed_at, stargazers_count, forks_count, open_issues_count, archived, description, license: .license.spdx_id, language, homepage}'

# Top contributors (ask for more than 10 — 15 gives better bus-factor view)
gh api 'repos/{owner}/{repo}/contributors?per_page=15' --jq '.[] | {login, contributions}'

# Recent commits (check signing, activity, patterns)
gh api repos/{owner}/{repo}/commits --jq '.[0:10] | .[] | {date: .commit.author.date, message: .commit.message[0:80], author: .commit.author.name, verified: .commit.verification.verified}'

# Owner/org profile
gh api users/{owner} --jq '{login, name, company, created_at, public_repos, followers, bio, blog, twitter_username}'
# OR for orgs:
gh api orgs/{owner} --jq '{login, name, description, company, created_at, public_repos, followers, blog, email}'

# Tags and releases (distinguish; many projects tag without releasing)
gh api 'repos/{owner}/{repo}/tags?per_page=10' --jq '.[] | .name'
gh api 'repos/{owner}/{repo}/releases?per_page=5' --jq '.[] | {tag_name, published_at, name, prerelease, draft}'

# Security advisories filed against this repo
gh api 'repos/{owner}/{repo}/security-advisories?per_page=10' --jq '.[] | {ghsa_id, summary, severity, state, published_at}'

# Community health profile (GitHub's composite metric)
gh api 'repos/{owner}/{repo}/community/profile' --jq '{health_percentage, files: {code_of_conduct: .files.code_of_conduct.url, contributing: .files.contributing.url, security: .files.security_policy.url, license: .files.license.spdx_id}}'

# CI workflow inventory (hygiene signal)
gh api repos/{owner}/{repo}/contents/.github/workflows --jq '.[] | .name'

# Total commit count (low count in old repos is a signal)
gh api 'repos/{owner}/{repo}/commits?per_page=1' -i 2>&1 | grep -i 'link:'
# Parse the last page number from the Link header

# Commit counts by top contributor (dominance / bus-factor)
gh api 'repos/{owner}/{repo}/commits?author={top-contributor}&per_page=1' -i 2>&1 | grep -i 'link:'
```

#### 3b. Non-GitHub source forges

- **GitLab**: `curl https://gitlab.com/api/v4/projects/{url-encoded-path}` for repo metadata. Supports contributors, tags, releases via analogous endpoints.
- **Gitea / Codeberg**: `curl https://codeberg.org/api/v1/repos/{owner}/{repo}`. API is GitHub-compatible in shape.
- **SourceHut**: HTML scraping via WebFetch; no public REST API at analysis time.

Record reduced visibility in the output. Missing signals due to
tooling gaps are distinct from missing signals due to project
properties — keep them labeled separately.

### 3c. Commit history patterns

Don't just check the last commit date — look at the **shape of
activity over the project's lifetime**:

- **Total commit count relative to age.** A 12-year-old project with
  10 commits (mousetrap) is qualitatively different from an 8-year-old
  project with 467 commits (kong) or a 5.5-year-old project with 1,751
  commits (atuin). Low total count indicates write-once code.
- **Activity distribution.** Sample cumulative commit counts at
  yearly breakpoints (`gh api 'repos/{o}/{r}/commits?until={date}&per_page=1' -i`).
  Three shapes show up:
  - **Accelerating** — later years have more commits than earlier.
    Positive vitality signal (atuin).
  - **Decelerating / flat** — declining over time. Context-dependent:
    a mature lib may genuinely need less churn (kong); a previously-
    active project winding down is a risk.
  - **Bursty with long gaps** — years of silence followed by bursts.
    Check the nature of the bursts. Feature work / genuine fixes is
    neutral-to-positive. Cosmetic cleanup (updating build tags,
    adding go.mod) in an otherwise fallow project is weaker.
- **Nature of recent commits.** Spot-check the last 10 commit messages.
  Are they fixing real bugs, adding features, or just dependency bumps?

### 3d. Adoption context

Raw adoption counts are lossy. Distinguish **direct** from
**transitive** adoption.

**For Go** — use the `go.mod` filename search as a dependents proxy:
```
gh api 'search/code?q={module_path}+filename:go.mod&per_page=1' --jq '.total_count'
```

**Refs-to-stars ratio** (Go):

| Ratio | Interpretation |
|-------|----------------|
| < 1 | Mostly direct adoption — developers actively choose this package |
| 1–5 | Mix of direct and transitive |
| > 10 | Mostly transitive — pulled in by a popular parent, rarely chosen directly |

Validated examples:
- mousetrap: 14,696 refs / 269 stars = **54:1** (almost entirely transitive via Cobra — rejected)
- kong: 2,008 refs / 3,023 stars = **0.66:1** (mostly direct, deliberately chosen — trusted-for-now)
- testify: 79,360 refs / 25,911 stars = **3:1** (strong direct adoption — trusted-for-now)

**For Rust** — use crates.io's dependents API:
```
curl "https://crates.io/api/v1/crates/{name}/reverse_dependencies?per_page=1"
```
Combine with crates.io downloads (`curl https://crates.io/api/v1/crates/{name}` → `recent_downloads`, `downloads`) and GitHub stars for a similar ratio.

**For npm** — use the registry's dependents counter and weekly
downloads:
```
curl "https://api.npmjs.org/downloads/point/last-week/{pkg}"
curl "https://registry.npmjs.org/-/v1/search?text={pkg}&size=1"
```

**For PyPI** — use the BigQuery PyPI stats (deferred — no cheap API).
For v0.1 the PyPI collector uses `pypi.org/pypi/{name}/json` for
release metadata and leaves adoption as an absence signal.

**For CLIs and apps** — there's no library-level adoption proxy.
Stars + install-count proxies (Homebrew analytics, apt popcon,
winget telemetry) are the best available. Note when the adoption
signal is weak or absent.

**Non-GitHub stars caveat.** If the project is hosted on GitLab or
another platform and has an archived GitHub mirror, the GitHub star
count is artificially low. Flag the ratio as potentially misleading
and use the active host's "stars" / "favorites" count instead.

### 4. Collect ecosystem-specific signals

#### Go

- `go.mod` contents: direct and indirect deps, Go version,
  `retract` directives (high count = past publish quality issues).
- Transitive dependency health — fallow single-maintainer transitives
  are a repeating pattern (mousetrap, go-spew, go-difflib).
- `govulncheck` config if present; `CODEOWNERS` file for governance.

#### Rust

- `Cargo.toml` workspace/package layout — workspace dependencies
  declared once, per-crate overrides.
- `deny.toml` presence — **strong hygiene signal**. Inspect:
  - `[advisories]` block: `vulnerability = "deny"`? Which advisories
    are ignored? Do ignores have written rationales?
  - `[licenses]` block: is it default-deny with an allow-list?
  - `[bans]` block: `multiple-versions` / `wildcards` stance.
- Unsafe-code posture: grep for `#![forbid(unsafe_code)]`,
  `#![deny(unsafe_code)]`, or `#![allow(unsafe_code)]` at crate roots.
- Release tooling: is it cargo-dist, cargo-release, plain `cargo
  publish`? cargo-dist produces standardized release workflows.
- crates.io trusted publishing: does the release workflow use OIDC
  to `cargo publish`, or a long-lived API token? (Inspect
  `.github/workflows/release.yml` for `id-token: write` permission
  and `rust-lang/crates-io-auth-action` or equivalent.)

#### npm

- `package.json`: dependencies, devDependencies, peerDependencies,
  optionalDependencies.
- `postinstall` hook presence in `scripts` (strong-signal: execution
  vector).
- Registry `dist.attestations` field — OIDC trusted publishing is a
  binary signal per axios case study lesson #13.
- Maintainer count and tenure; compare with committers in the repo.

#### PyPI

- `pyproject.toml` / `setup.py` — build-time code execution vectors.
- `.pth` file presence (LiteLLM case study — interpreter-time
  execution).
- Wheel-vs-sdist publish patterns.

#### Universal (all ecosystems)

- Commit signing: what fraction of recent commits are GPG/SSH
  verified? A project where the lead signs and contributors don't
  is a different profile from a project where no one signs.
- `SECURITY.md` / security disclosure policy.
- CI workflow inventory and what's actually exercised.
- Renovate / Dependabot / equivalent automation.

### 5. Assess against the trust model

Read `design/trust-model.md` for the full framework. Core lenses:

**Temporal Era Classification**:
- Pre-LLM: before 30 Nov 2022
- Early LLM: 30 Nov 2022 — 24 Nov 2025
- Modern AI: after 24 Nov 2025

**Signal Groups** (from `design/signals-v01.md`):
- Vitality: last commit, release cadence, contributor activity,
  commit-activity shape
- Governance: maintainer count/tenure, org affiliation, commit
  signing, effective-maintainer concentration
- Publication integrity: tag correspondence, tag-SHA stability,
  publish patterns, trusted-publishing status
- Hygiene: CI, linters, Renovate/Dependabot, supply-chain policy
  configs (deny.toml, etc.), unsafe-code posture
- Criticality: stars, dependents, download counts — criticality is
  a **multiplier**, not a simple positive

**Forgery Resistance** (per trust model principle #6):
- Very high: institutional affiliation, long tenure, cryptographic
  signatures, trusted publishing
- High: cross-platform identity consistency, identity-domain
  consistency (email ↔ project ↔ blog)
- Medium, declining: code hygiene, CI config
- Low, declining: code style, commit messages, PR descriptions

**Transitive dependencies.** Check what the target pulls in. Fallow
single-maintainer transitives are a recurring pattern.

**Same-author concentration.** If multiple transitive dependencies
share one author or organization, flag as **correlated compromise
risk**. Example: modernc.org/sqlite depends on 4 other modernc.org/*
packages, all from cznic — one account compromise affects the whole
stack.

**Retracted versions (Go).** `retract` directives in go.mod are
responsible behavior but a high count indicates past publish quality
issues.

**Self-documented fragility.** A project whose README warns "you
must use the exact same version of X" is self-documenting a
tight-coupling risk.

**Hosted-service coupling.** If the project offers or requires a
hosted SaaS alongside the binary, that's a **second, independent
trust decision**. Document both surfaces separately.

### 6. Produce the output document

Pick the right location for the file:

- **`design/dogfood/{package-name}.md`** — if the target is an actual
  dependency of signatory (Go libraries signatory compiles against).
  A posture decision is expected and the user confirms it.
- **`design/analysis/{package-name}.md`** — if the target is not a
  signatory dependency. This includes non-Go projects, CLIs, and
  arbitrary projects the trust model is being applied to.
  Posture may be "Analysis only — no posture recorded."

Use this structure:

```markdown
# {Project Name} ({canonical coordinate})

**Role: {role tag(s)}**
**Decision: {Rejected | Trusted-for-now | Vetted-frozen | Analysis only — no posture recorded}**
**Date: {YYYY-MM-DD}**

## Framing Notes
{Only when the target is unusual — non-Go, non-library, outside
the original dogfood scope. Flag the scope-fit up front.}

## Dependency Role
{Why this role classification, what the blast radius is.}

## Signal Table
{Full signal table with Value and Assessment columns.}

## Author/Org Provenance
{Owner details, tenure, known projects, cross-platform presence,
identity-domain consistency.}

## Commit Activity Distribution
{Yearly breakpoints; accelerating vs. decelerating vs. bursty.}

## Publication Integrity
{Tags, releases, trusted publishing status.}

## Supply Chain Hygiene
{Ecosystem-specific policy files: deny.toml, govulncheck,
package.json hooks, etc.}

## Transitive Dependency Health
{Table of transitive deps with their own health signals.
For non-library targets, substitute "top-level runtime dependency
inventory" or skip.}

## Trust Model Assessment
{Per signal group: vitality, governance, publication, hygiene,
criticality, forgery resistance.}

## Gaps and Concerns
{What's missing or worrying, stated plainly.}

## Risk Assessment
{Specific risks given the role. For elevated-blast-radius roles
like shell-augment, spell out the per-keystroke / per-request
failure modes.}

## Decision
{Posture recommendation with rationale.
Or: "Analysis only — no posture recorded" with why.
Why not a higher tier. Why not a lower tier.}

## Action Items
{Concrete next steps: version pinning, monitoring, config changes.}

## Signals Surfaced That Didn't Fit Current Schema
{Any signal types that resisted the existing scalar signal model
or the role taxonomy. Feeds into signal-storage-evolution.md and
the signal-type registry.}
```

### 7. Update the index

- If filing under `design/dogfood/`: add a row to the decisions table
  in `design/dogfood/README.md`.
- If filing under `design/analysis/`: add a row to the analyses table
  in `design/analysis/README.md`.

### 8. Present recommendation to the user

Summarize findings and present a posture recommendation with
rationale. **The decision is the user's — do not record the posture
without confirmation.** "Analysis only — no posture recorded" is a
valid terminal state and should be offered explicitly when the target
isn't a consumer dependency.

## Important Notes

- **Do not make trust decisions autonomously.** Present the analysis
  and recommendation. The user confirms.
- **Do not skip transitive dependencies** for library targets. Check
  the manifest (go.mod / Cargo.toml / package.json / pyproject.toml)
  to see what gets pulled in.
- **Criticality is a multiplier.** High adoption amplifies both trust
  and risk. Flag this explicitly.
- **Flag scope-fit problems.** If the target doesn't fit the trust
  model's current shape (non-Go, non-library, unusual role, missing
  signal types), say so up front. Those mismatches are the most
  useful feedback for the eventual `signatory analyze` MCP tool.
- **New signals are worth surfacing.** If the analysis produced
  signal types that don't exist in `design/signals-v01.md` or
  `design/signal-type-registry.md`, list them in the output's
  "Signals Surfaced That Didn't Fit" section. They inform the
  schema evolution tracked in `design/signal-storage-evolution.md`.
- **This skill is a prototype.** It precedes the `signatory analyze`
  MCP tool and will be superseded when that is built. The format and
  process validated here inform the MCP implementation.
