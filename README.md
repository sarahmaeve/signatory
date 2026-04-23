# Signatory

![Otis the hedgehog](images/otis-cyberpunk.png)

## **a supply-chain analysis tool for the age of AI**

Are you pairing with an AI to build software?

Your LLM will suggest open-source packages to solve a problem.
Why? Because they've been used before in previous code. We viewed
open-source software as safe, because so many people interacted
with it, and we assumed that would surface bugs and security
problems, and the fixes would roll in from the community. Not so.

Now the *older* a package is, the larger its blast-radius. The more
users who have blithely installed it. The longer it has laid fallow
without a security update. And, counterintuitively, the more prevalent
it is in example code and the LLM cache.

We've seen some established code provide tasty targets for malicious
actors of all types, and now what were formerly nation-state level
capabilities are available on a monthly subscription.

Most tools assume packages are trustworthy and flag the ones with
known CVEs. Signatory doesn't. It borrows from counterintelligence
and fraud investigation: gather evidence on projects and the people
behind them, weigh each signal by how hard it is to forge, and decide
— with evidence in hand — whether something is worth trusting, worth
forking, or not worth depending on.

Signatory aggregates trust signals from GitHub, git history, and
package registries. Each signal is evaluated by how hard it is to forge.
As AI makes cheap signals — code style, commit messages — easier to
fake, signatory emphasizes the durable ones: institutional tenure,
cryptographic signatures, and your own organization's signed
attestations. Your own attestation is the highest-value signal in
the system.

It answers:

- **"Is anyone home?"** — vitality, release cadence, contributor activity
- **"Who's responsible?"** — maintainer tenure, org affiliation, commit signing
- **"How was this published?"** — publication metadata, trusted publisher binding, inter-version dependency changes
- **"Does it look like they care?"** — CI presence, commit signing, repo hygiene
- **"How critical is this?"** — downloads, dependents, blast radius

## Key Concepts

- **Explicit postures.** Your organization records where each
  dependency sits: *vetted-frozen*, *trusted-for-now*, *unexamined*,
  *unknown-provenance*, or *rejected*. Decisions are first-class data,
  not tribal knowledge.
- **Burns.** When a package or identity is compromised, burn it.
  Signatory keeps the ledger and surfaces it alongside every signal.

## How it works

Signatory is a CLI paired with a Claude Code skill. The CLI collects
deterministic signals and stores them locally; the `/analyze` skill
dispatches analyst agents over those signals and records a verdict.

Multiple LLM agents analyze provenance signals, code, and then provide
a human-friendly output that you can view any time.

Every analysis is linked to the original signal, so you can drill down
later or use the same data to run a further analysis with your own
agent criteria.

Here's the primary workflow, in a Claude Code session with the signatory
message server active:

    /analyze                # dispatch analyst agents, produce
                            # verdict + posture proposal

(or you can ask: "Can I trust XYZ?" if you've prepped the LLM)

    signatory survey        # aggregate view of your dependency tree

Supporting subcommands:

    signatory posture set express --tier=trusted-for-now \
      --rationale="Strong vitality, no anomalies, pending deep review"
    signatory burn add compromised-pkg --reason="Maintainer account compromised"

## Design

Signatory's trust model draws from intelligence analysis and
open-source intelligence (OSINT) methodology rather than developer
dashboards. See [`design/`](design/)
for the full model — start with [vision.md](design/vision.md).

## Building

    go install github.com/sarahmaeve/signatory/cmd/signatory@latest

Requires Go 1.25+, plus a Claude Code (or MCP-capable) client to run
`/analyze`. Pre-v0.1; see [ROADMAP](design/ROADMAP.md).
