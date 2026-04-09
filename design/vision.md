# Signatory: Vision

## Motivation

Frontier AI models have reached a capability threshold where they can autonomously
discover zero-day vulnerabilities in production software and develop working
proof-of-concept exploits. Anthropic's Claude Mythos Preview demonstrated this
concretely: thousands of zero-days found across major OSes, browsers, and critical
infrastructure, with an 83.1% score on the CyberGym vulnerability reproduction
benchmark.

Anthropic's response (Project Glasswing) channels these capabilities toward defense,
partnering with major infrastructure maintainers (AWS, Apple, Microsoft, Linux
Foundation, and others) to harden their systems. But Glasswing covers the top of the
dependency tree. The long tail — solo-maintained packages, transitive dependencies,
ecosystem plumbing — remains exposed.

When the cost of crafting a subtle, exploitable patch drops to near zero, the defense
can't rely solely on human code review. The questions shift to:

- **Provenance**: Can I cryptographically verify who wrote this code, when, and that
  it hasn't been altered?
- **Visibility**: Across my entire dependency tree, which components have strong
  trust signals and which are opaque?

Signatory addresses both.

## What Signatory Is

A supply chain trust analysis tool. Signatory collects, normalizes, and presents
trust signals about code, projects, and the people who produce them — enabling
humans and LLMs to make informed decisions about what to depend on.

**Analysis, tracking, and visibility are the product.** Cryptographic signing
and attestation are a supporting utility. Signatory consumes existing signing
infrastructure (GPG, SSH, Sigstore) rather than implementing its own
cryptographic protocols. The existence and quality of signatures is one signal
among many.

In a "suddenly needs to be hardened" scenario, the first problem isn't "how do
I sign things" — developers can already do that imperfectly. The first problem
is "I have hundreds of transitive dependencies and no idea which ones are
trustworthy." Signatory answers that question.

**Key properties:**
- Language/ecosystem agnostic. Not tied to Go, npm, or any single ecosystem.
- Aggregator, not scanner. We don't run vulnerability checks ourselves.
- Raw signals, not opinionated scores. Users can define their own risk thresholds
  and scoring mechanisms, ideally via MCP integration.
- Unit of analysis: repository or code module, with drill-down to individual PRs
  eventually.
- Serves both humans and LLMs. An LLM in a coding workflow can query signatory
  via MCP before recommending a dependency.

The goal is to allow humans and LLMs to navigate existing open-source projects
and codebases in a more rational framework — replacing the current default of
trusting whatever is top-of-stack or appears first in a search engine.

## Design Principles

- **Analysis is the product.** Signal collection, entity profiling, and trust
  visibility are the core. Signing and attestation are utilities that feed
  signals into the core.
- **Complement, don't compete.** Signatory is not a vulnerability scanner, not a
  replacement for Sigstore, not a CI system. It aggregates and presents what
  already exists.
- **Long-tail first.** The major infrastructure projects have Glasswing. Signatory
  should be useful to the maintainer who has 50 stars and 10,000 dependents.
- **CLI and MCP first.** The primary interfaces are a CLI for humans and an MCP
  server for LLMs. Hosted/dashboard views come later.
- **Extensible scoring via MCP.** Signatory exposes raw signal data. Scoring logic
  is decoupled — users can consume signals via MCP and apply their own risk models.

- **Validate against real attacks.** Real-world supply chain incidents are test
  cases for the signal model. Each new attack should be analyzed: "would signatory
  have surfaced this?" If yes, the model is working. If no, we learn what signal
  is missing. See the `design/example-*.md` files for worked examples.

## Implementation Language

Go. Chosen for:
- Efficiency of generated code and runtime performance
- Strong opinions and built-in safety features
- Alignment with the existing ecosystem (Sigstore, git tooling, CLI tools)
- Not Python or TypeScript — those are explicitly out of scope
