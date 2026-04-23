# Signatory

![Otis the hedgehog](images/otis-cyberpunk.png)

## **A supply-chain analysis tool for the AI age**

Are you pairing with an AI to build software?

Your LLM will suggest open-source packages to solve a problem.
Why? Because they've been used in published code. We once viewed
open-source software as safe, because so many people interacted
with it, and we assumed that would surface bugs and security
problems, and the fixes would roll in from the community.

Now, the *older* a package is, the larger its blast radius. The more
users who have adopted it without review. The longer it has lain fallow
without a security update. And, counterintuitively, the more prevalent
it is in example code and the LLM cache.

We've seen established modules turn into launching pads for swift
and effective cyber attacks. Capabilities that were formerly the
purview of nations are now available via a monthly subscription.

Most tools assume packages are trustworthy and flag the ones with
known CVEs. Signatory doesn't. It borrows from counterintelligence
and fraud investigation: gather evidence on projects and the teams
behind them, weigh each signal by how hard it is to forge, and decide
whether something is worth trusting, worth forking, or worth replacing.

Signatory aggregates trust signals from GitHub, git history, and
package registries. High-quality LLM-generated code and comments are
now cheap, so signatory emphasizes more durable signals: institutional
tenure, cryptographic signatures, and your own organization's signed
attestations.

Your *own* decisions are the highest-value signals in the system.

It answers:

- **"Is anyone home?"**: vitality, release cadence, contributor activity
- **"Who's responsible?"**: maintainer tenure, org affiliation, commit signing
- **"How was this published?"**: publication metadata, trusted publisher binding, inter-version dependency changes
- **"Does it look like they care?"**: CI presence, commit signing, repo hygiene
- **"Is this a likely target?"**: install base, dependents, infrastructure criticality

## Key Concepts

- **Explicit postures.** Your organization records where each
  dependency sits: *vetted-frozen*, *trusted-for-now*, *unexamined*,
  *unknown-provenance*, or *rejected*. Decisions are explicit, based on
  recorded data, so they persist across sessions and are available to
  both you and your models when needed.

- **Burns.** When a package or identity is compromised, burn it.
  Signatory keeps the ledger and surfaces it alongside every signal.

## How it works

Signatory is a CLI paired with a Claude Code skill. The CLI collects
deterministic signals and stores them locally; the `/analyze` skill
dispatches analyst agents over those signals and suggests a verdict.

Every analysis is linked to the source signals, so you can drill down
later or use the same data to run a further analysis with your own
agent criteria.

Here's the primary workflow, in a Claude Code session:

    /analyze                # dispatch analyst agents, produce
                            # verdict + posture proposal

(or you can ask: "Can I trust XYZ?" if the LLM has the skill in context)

    signatory survey        # aggregate view of your dependency tree

Supporting subcommands:

    signatory posture set express --tier=trusted-for-now \
      --rationale="Strong vitality, no anomalies, pending deep review"
    signatory burn add compromised-pkg --reason="Maintainer account compromised"

## Design

Signatory's trust model draws from intelligence analysis and
open-source intelligence (OSINT) methodology rather than developer
dashboards. See [`design/`](design/) for the full model.
Start with [vision.md](design/vision.md).

## Building

    go install github.com/sarahmaeve/signatory/cmd/signatory@latest

Requires Go 1.25+, plus a Claude Code (or MCP-capable) client to run
`/analyze`. Pre-v0.1; see [ROADMAP](design/ROADMAP.md).
