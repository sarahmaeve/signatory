# Signatory

**Supply chain trust analysis for an AI-native world.**

Signatory collects, normalizes, and presents trust signals about open-source code, projects, and the people who maintain them — enabling developers and LLMs to make informed decisions about what to depend on.

## The Problem

The cost of a convincing exploit is constantly declining. AI models can now autonomously discover zero-day vulnerabilities, generate working exploits, and craft subtle, plausible-looking supply chain attacks. The [axios npm compromise](design/example-axios-attack.md) (March 2026) demonstrated the impact: 3 hours, 100M+ weekly downloads, thousands of environments compromised. Every major vendor's response — YARA rules, Defender signatures, EDR detections — operates *after* the malicious code has already executed.

Signatory operates upstream. Before `npm install`. Before the code runs.

## What It Does

Signatory aggregates trust signals from existing sources — GitHub, package registries, OpenSSF Scorecard — and presents them through a compositional trust model. It doesn't scan for vulnerabilities or assign opinionated risk scores. Instead, it answers questions:

- **"Is anyone home?"** — Project vitality, release cadence, contributor activity
- **"Who's responsible?"** — Maintainer tenure, org affiliation, commit signing
- **"How was this published?"** — Publication metadata consistency, trusted publisher binding, dependency changes between versions
- **"Does it look like they care?"** — CI configuration, linters, OpenSSF Scorecard
- **"How critical is this?"** — Download counts, dependent packages, blast radius

Every signal is tagged with its **forgery resistance** — how hard it is to fake. As AI makes cheap signals (code style, commit messages) trivial to produce, signatory emphasizes the signals that remain durable: institutional affiliation, long tenure, cryptographic signatures, and your own organization's signed attestations.

## Key Concepts

**Temporal Era Classification.** Code is classified by when it was produced: pre-LLM (before Nov 2022), early LLM (2022–2025), or modern AI (after Nov 2025). The era affects how every other signal is interpreted.

**Dependency Posture Tiers.** Your organization records explicit trust decisions about dependencies: *vetted & frozen* (reviewed, signed, locked), *trusted-for-now* (accepted, pending deeper review), *unexamined* (no decision made), or *unknown provenance* (no value over what you can build yourself).

**Federated Burn Lists.** When a project or identity is compromised, burn it. Burns degrade trust signals retroactively and propagate through your dependency tree. Organizations maintain their own burn lists and can subscribe to others'.

**The highest-value signal is your own attestation.** External signals degrade as AI makes them cheaper to fake. A signed attestation from your organization — representing actual review, repair, and cryptographic commitment — is the most forgery-resistant signal in the system.

## Usage

```
signatory analyze express          # Trust profile for a package
signatory survey                   # Posture overview of your dependency tree
signatory compare lodash underscore  # Side-by-side signal comparison
signatory posture set express \
  --tier=trusted-for-now \
  --rationale="Strong vitality, no anomalies, pending deep review"
signatory burn compromised-pkg \
  --reason="Maintainer account compromised"
```

Signatory also runs as an MCP server, enabling LLMs in coding workflows to query trust signals before recommending dependencies.

## Design

Signatory's trust model is informed by intelligence analysis and OSINT methodology rather than traditional developer dashboards. The full design — including the trust model, signal framework, and case studies from real attacks — is in [`design/`](design/):

- [Vision](design/vision.md) — Why this exists
- [Architecture](design/architecture.md) — How it's built
- [Trust Model](design/trust-model.md) — How trust works
- [V0.1 Signal Set](design/signals-v01.md) — What we measure
- [MCP Interface](design/mcp-interface.md) — How LLMs interact
- [Axios Attack Analysis](design/example-axios-attack.md) — Case study: precision nation-state attack
- [prt-scan Analysis](design/example-prtscan-attack.md) — Case study: AI-augmented volume attack

## Status

Early development. The CLI skeleton, core data model, and 109 tests are in place. Signal collectors, the SQLite store, and MCP server are next. See the [open issues](../../issues) for the roadmap.

## Building

```
go install github.com/sarahmaeve/signatory/cmd/signatory@latest
```

Requires Go 1.25+. One runtime dependency ([Kong](https://github.com/alecthomas/kong), zero transitive deps — [we vetted it](design/dogfood-dependency-decisions.md)).

## Contributing

After cloning, activate the pre-commit hooks (gofmt, vet, `go test -race`):

```
make install-hooks
```

This sets `core.hooksPath` to `.githooks/` for this clone. One-time per clone, idempotent to re-run. CI enforces the same checks; running the hook locally catches them before push.
