---
name: vet-dependency
description: Perform a supply chain trust analysis on a Go dependency before adopting it. Collects signals from GitHub and package registries, assesses against the signatory trust model, and produces a structured dogfood document with a posture recommendation. Use when adding a new dependency, evaluating an existing one, or when the user asks to vet/analyze a package.
allowed-tools: Bash Read Write Edit Glob Grep WebFetch
---

# Vet Dependency

Perform a supply chain trust analysis on a Go dependency. This skill is a
prototype of `signatory analyze` — it will be superseded by the MCP server
when built.

The target is specified as $ARGUMENTS (a Go module path, npm package name,
or GitHub repo URL).

## Process

### 1. Identify the target

Parse $ARGUMENTS to determine:
- The source repo (GitHub, GitLab, or other host)
- The package registry (npm, PyPI, Go modules) if applicable
- Whether this is a direct dependency or transitive

**Vanity domains:** Go modules often use vanity import paths (e.g.,
`modernc.org/sqlite` → `gitlab.com/cznic/sqlite`). Check `pkg.go.dev`
or fetch the module's `?go-get=1` metadata to resolve the actual repo.

**Non-GitHub hosting:** If the project is on GitLab, Gitea, SourceHut,
or another platform:
- Note that signal collection will be **less complete** — `gh api` won't
  work. Use WebFetch to scrape project pages for metadata.
- The reduced visibility is itself a signal: less tooling support means
  less automated scrutiny from the ecosystem.
- Check if there's a GitHub mirror and whether it's active or archived.
  An archived GitHub mirror pointing to GitLab is common (e.g.,
  modernc.org/sqlite).

**Archived repos:** If a GitHub repo is archived, check the description
and homepage for where development has moved. Follow the redirect.

### 2. Determine the dependency role

Ask the user what role this dependency plays, or infer from context:
- **Runtime** — compiled into the production binary
- **Validation** — runs during testing/CI (e.g., test frameworks)
- **Build-only** — code generation, linting, formatting
- **Development** — editor tooling, local-only

See `design/dogfood/README.md` for role definitions and risk profiles.

### 3. Collect signals from GitHub

Use `gh api` to collect:

```
# Repo metadata
gh api repos/{owner}/{repo} --jq '{name, owner: .owner.login, owner_type: .owner.type, created_at, updated_at, pushed_at, stargazers_count, forks_count, open_issues_count, archived, description, license: .license.spdx_id}'

# Top contributors
gh api 'repos/{owner}/{repo}/contributors?per_page=10' --jq '.[] | {login, contributions}'

# Recent commits (check signing, activity, patterns)
gh api repos/{owner}/{repo}/commits --jq '.[0:10] | .[] | {date: .commit.author.date, message: .commit.message[0:80], author: .commit.author.name, verified: .commit.verification.verified}'

# Owner/org profile
gh api users/{owner} --jq '{login, name, company, created_at, public_repos, followers, bio}'
# OR for orgs:
gh api orgs/{owner} --jq '{login, name, description, company, created_at, public_repos, followers}'

# Recent tags/releases
gh api repos/{owner}/{repo}/tags --jq '.[0:5] | .[] | .name'

# Adoption (approximate)
gh api 'search/code?q={module_path}+filename:go.mod&per_page=1' --jq '.total_count'

# Total commit count (low count in old repos is a signal)
gh api 'repos/{owner}/{repo}/commits?per_page=1' -i 2>&1 | grep -i 'link:' 
# Parse the last page number from the Link header, or count commits manually
```

### 3a. Analyze commit history patterns

Don't just check the last commit date — look at the **distribution** of
commits over the project's lifetime:

- **Total commit count:** A 10-year-old project with 10 commits is
  different from one with 500. Very low commit counts suggest the project
  was written once and rarely touched.
- **Activity gaps:** Large gaps (years of silence followed by a burst)
  may indicate the project was abandoned and briefly revived, or that a
  new maintainer took over. Note the gap durations and what the burst
  consisted of (feature work vs. cleanup/modernization).
- **Nature of recent commits:** Were recent commits substantive (features,
  fixes) or cosmetic (updating build tags, adding go.mod)? Cosmetic-only
  activity in an otherwise fallow project is a weaker vitality signal
  than genuine maintenance.

### 3b. Assess adoption context

When reporting adoption numbers, distinguish between **direct** and
**transitive** adoption:

- If a package has high go.mod reference counts but low stars, it is
  likely pulled in as a transitive dependency of something popular, not
  adopted directly. Note which parent dependency is driving adoption.
- Transitive-only adoption means the package has not been independently
  evaluated by most of its consumers — they inherited it without choosing
  it. This is a weaker trust signal than direct adoption where developers
  actively selected the package.

**Refs-to-stars ratio:** Calculate `go.mod references / GitHub stars` as
a heuristic for adoption type:

| Ratio | Interpretation |
|-------|---------------|
| < 1 | Mostly direct adoption — developers actively choose this package |
| 1–5 | Mix of direct and transitive |
| > 10 | Mostly transitive — dragged in by a popular parent, rarely chosen directly |

Examples from signatory's own dogfood evaluations:
- mousetrap: 14,696 refs / 269 stars = **54:1** (almost entirely transitive via Cobra)
- kong: 2,008 refs / 3,023 stars = **0.66:1** (mostly direct, deliberately chosen)
- testify: 79,360 refs / 25,911 stars = **3:1** (strong direct adoption with some transitive)

High transitive-only adoption is a risk signal: the package is in many
dependency trees but few humans have independently evaluated it.

**Note on non-GitHub stars:** If the project is hosted on GitLab or
another platform and has an archived GitHub mirror, the GitHub star
count may be artificially low. Note this in the assessment and flag
the ratio as potentially misleading.

### 4. Collect ecosystem-specific signals

For Go modules, check:
- `go.mod` contents (direct and indirect dependencies, Go version)
- Transitive dependency count and health

For npm packages, check:
- Registry metadata (weekly downloads, publish dates, maintainers)
- Whether the package has trusted publisher binding

### 5. Assess against the trust model

Read `design/trust-model.md` for the full framework. Key assessments:

**Temporal Era Classification:**
- Pre-LLM: before 30 Nov 2022
- Early LLM: 30 Nov 2022 — 24 Nov 2025
- Modern AI: after 24 Nov 2025

**Signal Groups** (from `design/signals-v01.md`):
- Vitality: last commit, release cadence, contributor activity
- Governance: maintainer count/tenure, org affiliation, commit signing
- Publication integrity: tag correspondence, publish patterns
- Hygiene: CI, linters, Renovate/Dependabot
- Criticality: stars, dependents, download counts (remember: criticality is a multiplier, not just a positive signal)

**Forgery Resistance** (from trust model principle #6):
- Very high: institutional affiliation, long tenure, cryptographic signatures
- High: cross-platform identity consistency, trusted publishing
- Medium, declining: code hygiene, CI config
- Low, declining: code style, commit messages

**Transitive Dependencies:**
Check the health of transitive deps. Fallow single-maintainer transitives
are a repeating pattern (see mousetrap, go-spew, go-difflib cases).

**Same-author concentration:** If multiple transitive dependencies share
one author or organization, flag this as **correlated risk**. A
compromise of that author's account affects all their packages
simultaneously. This is worse than the same number of dependencies from
independent authors. Example: modernc.org/sqlite depends on 4 other
modernc.org/* packages, all from cznic — a single account compromise
would affect the entire stack.

**Retracted versions:** Check if the project's go.mod contains `retract`
directives. Retraction is responsible behavior (better than leaving broken
versions live), but a high count indicates past publish quality issues
or instability.

**Self-documented fragility:** Check if the project's README or go.mod
contains warnings about version coupling or fragile dependencies. A
project that warns "you must use the exact same version of X" is
self-documenting a tight coupling risk. Note these warnings.

### 6. Produce the output document

Write a markdown file to `design/dogfood/{package-name}.md` following this
structure (see existing files in `design/dogfood/` for examples):

```markdown
# {Package Name} ({module path})

**Role: {Runtime | Validation | Build-only | Development}**
**Decision: {Rejected | Trusted-for-now | Vetted-frozen}**
**Date: {YYYY-MM-DD}**

## Dependency Role
{Why this role classification, what the blast radius is}

## Signal Table
{Full signal table with Value and Assessment columns}

## Author/Org Provenance
{Owner details, tenure, known projects, cross-platform presence}

## Transitive Dependency Health
{Table of transitive deps with their own health signals}

## Trust Model Assessment
{Per signal group: vitality, governance, publication, hygiene, criticality, forgery resistance}

## Gaps and Concerns
{What's missing or worrying, stated plainly}

## Risk Assessment
{Specific risks given the dependency role}

## Decision
{Posture recommendation with rationale}
{Why not a higher tier}

## Action Items
{Concrete next steps: version pinning, monitoring, etc.}
```

### 7. Update the index

Add a row to the decisions table in `design/dogfood/README.md`.

### 8. Present recommendation to the user

Summarize findings and present a posture recommendation with rationale.
The decision is the user's — do not record the posture without confirmation.

## Important Notes

- **Do not make trust decisions autonomously.** Present the analysis and
  recommendation. The user confirms.
- **Do not skip transitive dependencies.** Check go.mod of the target to
  see what it pulls in. Fallow transitives are a common risk pattern.
- **Criticality is a multiplier.** High adoption amplifies both trust and
  risk. Flag this explicitly.
- **This skill is a prototype.** It precedes the `signatory analyze` MCP
  tool and will be superseded when the MCP server is built. The format
  and process validated here inform the MCP implementation.
