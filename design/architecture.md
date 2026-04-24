# Signatory: Architecture

## Overview

Signatory is a supply chain trust analysis tool. Its core purpose is to collect,
normalize, and present trust signals about code, projects, and the people who
produce them — enabling humans and LLMs to make informed decisions about what
to depend on.

Analysis, tracking, and visibility are the product. Cryptographic signing and
attestation are a supporting utility — signatory consumes existing signing
infrastructure (GPG, SSH, Sigstore) rather than implementing its own
cryptographic protocols.

## Core Concept: Entity Profiles

The central data structure is the **entity profile** — a compositional,
multi-signal trust representation that can describe:

- **People**: maintainers, reviewers, contributors (see
  [example-identity-analysis-rsc.md] for a worked example)
- **Projects**: repositories, organizations
- **Packages**: published modules in any ecosystem
- **Patches**: individual code changes (PRs, commits)
- **LLM interactions**: AI-generated or AI-reviewed code

All entity types share the same compositional signal framework. Each profile
is a collection of independently verifiable signals, each with its own forgery
difficulty, temporal characteristics, and decay properties.

## Component Architecture

```
signatory CLI / MCP Server
├── Analysis Engine (core)
│   ├── Entity Profile Manager
│   │   ├── Profile construction and update
│   │   ├── Signal composition
│   │   └── Retroactive re-evaluation (burn propagation)
│   ├── Signal Collectors
│   │   ├── GitHub (repo metadata, commit signing, contributor info)
│   │   ├── Package Registries (npm, PyPI, ...)
│   │   ├── Existing signing/attestation (GPG, Sigstore, etc.)
│   │   └── [Extensible: additional signal sources]
│   └── Ecosystem Providers
│       ├── npm (manifest parsing, dep resolution, registry mapping)
│       ├── PyPI (manifest parsing, dep resolution, registry mapping)
│       └── [Extensible: Go modules, crates, Maven, ...]
├── Trust Management
│   ├── Dependency Posture (vetted/trusted/unexamined/unknown tiers)
│   ├── Burn Lists (local + federated subscriptions)
│   └── Temporal Era Classification
├── Query Interface
│   ├── CLI output (human-readable, JSON)
│   ├── MCP Server (exposes signals to LLMs in coding workflows)
│   └── Export (structured data for dashboards, SBOMs)
└── Scanner (proof-of-concept)
    └── Populates profiles by examining repos, packages, or PRs on demand
```

## Analytical Paradigm

Signatory's approach to supply chain trust is modeled on intelligence analysis
and OSINT/fraud investigation tooling (cf. IBM i2 Analyst's Notebook, Palantir,
Maltego) rather than traditional developer dashboards:

- **Entity profiling** — building multi-source dossiers on maintainers,
  projects, and packages
- **Link analysis** — dependency graphs, fork relationships, reviewer chains
- **Temporal analysis** — commit history patterns, trust era classification
- **Pattern detection** — behavioral anomalies in contributor activity
- **Multi-source fusion** — composing signals from heterogeneous sources

**The key difference from pure intelligence work:** supply chain analysis can
reach ground truth. You can read the code, review it, test it, and attest to
it. The uncertainty is about what you *haven't yet examined*, not about what's
unknowable. Better tools (including AI-assisted review) make it progressively
cheaper to move things from "unexamined" to "vetted."

### The Investigation-to-Attestation Loop

1. **Monitor** — Dashboard surfaces current trust posture: what's vetted,
   what's unexamined, what's degraded, where are the gaps.
2. **Investigate** — Drill down when something looks wrong or needs vetting.
   Explore the signal graph in an analyst-style interface.
3. **Attest** — Close the loop: you've reviewed this, repaired it if needed,
   and recorded the decision. The entity moves from "unexamined" to "vetted."

This loop tightens over time. The dashboard is the entry point for v0.1; the
explorable investigation interface is a later layer that sits on the same
data model.

## Primary Workflows

These workflows serve both humans and LLMs. An LLM in a coding workflow can
query signatory via MCP before recommending a library.

1. **"Tell me about this dependency."**
   Evaluate a package, repo, or maintainer before adopting it.
   `signatory analyze <package|repo|identity>`

2. **"Show me my dependency tree's trust posture."**
   Assess the current state of everything you depend on.
   `signatory survey`

3. **"Something changed — what's the impact?"**
   A maintainer left, a new contributor appeared, a fork diverged.
   Surface changes against baseline profiles.
   `signatory diff`

4. **"This entity was burned — what's the blast radius?"**
   Trace a burn through the dependency graph.
   `signatory burn <entity> && signatory survey --burned`

## Provider Architecture

The core is ecosystem-agnostic. Ecosystem-specific logic is encapsulated in
providers that implement a common interface:

- **Ecosystem Providers** handle: parsing dependency manifests, resolving
  packages to source repositories, enumerating transitive dependencies, and
  checking distribution consistency (does the published package match the
  source repo?).

- **Signal Collectors** handle: fetching trust signals from external APIs,
  normalizing them into the entity profile format, and managing rate limits
  and caching.

### Initial Ecosystem Targets

1. **npm** — Highest risk surface. History of supply chain attacks (left-pad,
   event-stream, ua-parser-js, colors.js). Large dependency trees are the norm.
2. **PyPI** — Critical infrastructure prevalence. Typosquatting attacks are
   common. Dependency resolution is more complex than npm.

Additional ecosystems (Go modules, crates.io, Maven) to follow once the
provider interface is proven.

## Scanner (Proof of Concept)

For initial development and demonstration, signatory needs a scanner that
can examine a specified repo, package, or pull request and populate entity
profiles with collected signals. This scanner is the mechanism by which
signatory populates its data.

Refining the scanner and the signals it collects is an ongoing project —
the initial version establishes the pattern, and signal coverage improves
iteratively.

Future: subscribe to an external monitoring service (or one we build later)
that provides ongoing change detection with a high-trust signal, rather than
relying solely on point-in-time scanning.

## MCP Integration

Signatory functions as an MCP server, exposing raw signal data through a
structured interface. This enables:

- **LLM-assisted development**: An LLM generating code can query signatory
  before recommending a dependency — the defense integrated into the same
  workflow that creates the risk.
- **Custom scoring**: Users connect a model (Claude or otherwise) that
  consumes signals and produces risk assessments tailored to their context.
- **Natural language queries**: "Which of my dependencies have had a
  maintainer change in the last 90 days?"
- **Policy enforcement**: Organizations define policies as scoring rules
  and evaluate their dependency tree against them.

## Proposed Project Layout

```
signatory/
├── cmd/
│   └── signatory/              # CLI entrypoint
│       └── main.go
├── internal/
│   ├── engine/                 # Analysis engine core
│   ├── profile/                # Entity profile construction and management
│   ├── signal/                 # Signal collector interface + implementations
│   │   ├── github/
│   │   └── registry/           # Package registry metadata
│   ├── ecosystem/              # Ecosystem provider interface + implementations
│   │   ├── npm/
│   │   └── pypi/
│   ├── trust/                  # Posture tiers, burn lists, temporal eras
│   ├── scanner/                # Repo/package/PR examination
│   ├── query/                  # Query and filtering logic
│   ├── output/                 # Output formatting (table, JSON, export)
│   └── cache/                  # Local caching layer
├── pkg/
│   └── signatory/              # Public API for library consumers
├── design/                     # Design documents
├── go.mod
└── go.sum
```

## Decisions Made

| Decision | Choice | Rationale |
|----------|--------|-----------|
| Implementation language | Go | Efficient, opinionated, ecosystem alignment |
| Core product | Analysis & tracking | Signing is a utility, not the product |
| Signing approach | Consume existing (GPG, SSH, Sigstore) | Don't build crypto protocols |
| Scoring model | Raw signals, user-defined scoring via MCP | Avoid false confidence from opaque scores |
| Primary interface | CLI + MCP server | Serves humans and LLMs |
| Initial ecosystems | npm, PyPI | Highest risk, most prevalent |
| Unit of analysis | Repo/module, drill to PR | Multi-level zoom |
| Entity profiles | First-class, multi-type | Shared compositional framework across entity types |
| Data population | Scanner (PoC) + future service subscription | Start with on-demand, evolve to monitoring |
| V0.1 signal set | 6 groups + temporal eras | Validated against axios case study; see signals-v01.md |
| V0.1 ecosystems | npm first, PyPI follows | npm highest risk; PyPI once interfaces proven |

## V0.1 Scope (Finalized)

1. CLI skeleton: `signatory analyze <package|repo>`, `signatory survey`
2. npm ecosystem provider (manifest parsing, registry API)
3. GitHub signal collector (repo metadata, contributor info, commit signing)
4. npm registry signal collector (publish metadata, version history)
5. Entity profile construction (project + identity + package)
6. Dependency posture tracking (vetted/trusted/unexamined/unknown tiers)
7. Burn list management (local, with import capability)
8. Temporal era classification
9. Structured JSON output
10. Local caching with configurable TTLs
11. Proof-of-concept scanner that populates profiles on demand

Signal set details: [signals-v01.md]

## Deployment Targets

1. **Developer workstation** — Primary. CLI tool and MCP server, local
   SQLite database, on-demand queries. Target user for v0.1: a solo
   developer or small team working with an LLM.
2. **Docker** — MVP packaging target. Reproducible, self-contained,
   easy to share and run without a Go toolchain.
3. **Local Kubernetes (kind)** — Stepping stone to cloud deployment.
   Precursor to hosted/org-wide, built when needed.
4. **CI pipeline** — Gate merges on provenance policy. Future.
5. **Hosted/org-wide** — Continuous monitoring, likely runs in org's
   cloud (public or private). Future.

Deployment complexity is built gradually. The architecture does not
depend on any specific deployment target — the same Go binary runs
in all contexts.
