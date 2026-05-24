# Anti-subversion: detecting AI-targeted prompt injection in supply-chain content

**Status:** active. Promoted from `design/potential/` on 2026-05-24
in response to the Trapdoor crypto-stealer campaign
([`design/threat-landscape/2026-05-24-trapdoor-crypto-stealer.md`](threat-landscape/2026-05-24-trapdoor-crypto-stealer.md)),
which weaponized `.cursorrules` and `CLAUDE.md` as zero-width-Unicode
prompt-injection carriers across npm, PyPI, and crates.io.
Originally captured as a product opportunity surfaced during the
CamoLeak (CVE-2025-59145) threat discussion. Companion to
`design/hardening.md §1`, which addresses the same attack class
from the egress side (preventing signatory itself from becoming a
relay).

**Implementation status (2026-05-24):**

- Primitive package landed: [`internal/contentinjection/`](../internal/contentinjection/).
  All seven design-doc primitives implemented (invisible Unicode,
  bidi controls, tag block, markdown HTML comments, markdown image
  syntax, lexical injection patterns, encoded base-N blobs) with
  TDD on Trapdoor-shaped malicious + benign twins. Per-call
  primitive suppression via `ScanOptions.SuppressPrimitives` so
  consumers can skip primitives they know to be noisy on the file
  class they're scanning.
- AI-agent locus taxonomy unified at
  [`internal/agentconfig/`](../internal/agentconfig/) — single
  source of truth pairing the file-detector shape (Family) with
  runtime-path substring patterns. Resolved a drift bug where
  `/.codex/` had landed in the source-AST persistence-write
  catalog but lacked a corresponding Family for inventory and
  content-injection scanning. The 14-Locus taxonomy covers the
  AI-instruction file set named in §"Where AI-instruction files
  fit" §1 below: `.cursorrules`, `CLAUDE.md`, `AGENTS.md`,
  `GEMINI.md`, `.github/copilot-instructions.md`,
  `.github/instructions/*.instructions.md`, `.claude/`,
  `.cursor/rules/`, `.aider.conf.yml`, `.zed/settings.json`,
  `.codex/`, `.continue/config.json`, `.windsurfrules`.
- Layer 1 (repofiles content awareness) landed:
  [`internal/signal/repofiles/`](../internal/signal/repofiles/)
  scans AI-instruction files via `AgentConfigFamilies()` (now
  derived from `agentconfig.Loci()`) and content-inspects each
  match. Emits two always-on signals:
  `agent_config_files` (the Layer-1 inventory signal per §"Where
  AI-instruction files fit" below) and
  `agent_config_content_injection` (the
  `content-injection-surface` findings on those files). The
  content-injection scan suppresses `PrimitiveMarkdownComment`
  per §"Where AI-instruction files fit" §2: imperative-mood prose
  IS the expected content on these files, so the primitive is
  useless there.
- Layer 2 (artifact-vs-repo categorization) landed:
  [`internal/signal/artifact/`](../internal/signal/artifact/)
  classifies dropped-in-tarball agent-config files under a new
  `CategoryAgentConfig` bucket, surfaced through the existing
  `artifact_repo_divergence` signal. The xz-precedent applied to
  AI-config injection. Consumes the same `agentconfig` taxonomy
  via `repofiles.IsAgentConfigPath`.
- Layer 3 (source-AST runtime-write detection) landed:
  OS/credential-store-shape catalogs (`SensitivePathPatterns`,
  `PersistencePathPatterns`, `CredentialEnvNames`,
  `CloudMetadataHosts`) extracted to
  [`internal/signal/source/astfeature/catalogs.go`](../internal/signal/source/astfeature/catalogs.go),
  with the AI-agent runtime prefixes derived from
  `agentconfig.RuntimePersistencePrefixes()` at init. Trapdoor
  additions (wallet-keystore reads, AI-agent persistence writes)
  fire on the node analyzer today. Python wiring is a separate
  parity task; cargo and gem source-AST analyzers don't yet exist
  but inherit the shared catalogs when they land.
- **Remaining work** (collector-side):
  - README / PR / release-notes consumers of the
    `contentinjection` primitive (§"What to detect" applies to
    those surfaces too — a separate workstream).
  - Inventory-signal payload enrichment per §"Where AI-instruction
    files fit" §1: capture path, hash, byte length, last-modified,
    last-modifying author. Today `agent_config_files` carries only
    `{family, path}`.
  - Stability signal per §"Where AI-instruction files fit" §3:
    surface "AGENTS.md changed in last release by author X".
    Needs deltas + git-blame integration.
  - File:line citations in `Finding.Details` per §"Where this
    slots into the architecture" output-shape.
  - Multi-axis score rollup `(primitive count, primitive class
    diversity, novelty since last release)` per §"What to detect"
    closing paragraph.
  - `MethodologyPattern.SignalGroup = content-injection-surface`
    + `CollectorHint` registration per §"Where this slots into
    the architecture" (exchange-layer plumbing).
  - Python `SensitivePathWrites` wiring; cargo and gem source-AST
    analyzers.
- **Remaining work** (deferred per the doc itself):
  - The egress-fence consumer per `hardening.md §1` — separate
    workstream.
  - The AI-instruction-file hash-pin posture extension
    (§"Open question" below — "sits well past v0.1").
  - Live PR-body scanning (§"Open design questions" — "out of
    scope for the initial collector").
  - The file-role weighting table (§"Open design questions"
    multi-file scoring).
  - Recursive scan of `.github/instructions/` subdirectories
    (the Copilot docs note subdirectories are allowed; v0.1 scans
    only the flat directory).

## The product opportunity

Signatory's threat-economics thesis is that AI collapsed the cost of
weaponizing untrusted content, and that durable defenses are
forgery-resistant signals an attacker cannot cheaply fake. Prompt
injection embedded in supply-chain artifacts — README files, release
notes, PR descriptions, issue templates, source-code comments, CI
configuration prose — is the canonical instance of this collapse:

- **Pre-AI cost**: the payload had no execution surface. A malicious
  comment in a README was inert text.
- **Post-AI cost**: the payload executes wherever an AI assistant
  with privileged access reads it. Cost approaches zero, blast
  radius equals the assistant's permissions.

CamoLeak demonstrated the kill chain end-to-end via the
rendering-surface variant — markdown image syntax exfiltrating
through an apparently-innocent image fetch. Trapdoor (2026-05,
[`threat-landscape/2026-05-24-trapdoor-crypto-stealer.md`](threat-landscape/2026-05-24-trapdoor-crypto-stealer.md))
demonstrated the supply-chain variant: zero-width-Unicode payloads
embedded directly in `.cursorrules` and `CLAUDE.md` files dropped
into consumer repos via malicious dependencies or proposed-PRs
against established AI-tooling projects (browser-use, langchain,
langflow, llama_index, MetaGPT, OpenHands). The class is broader
than either incident: any AI assistant with deep system access plus
a rendering or tool-call surface (Microsoft 365 Copilot, Google
Gemini, Cursor, Claude Code, the consumer of *our* MCP) is on the
target list.

Signatory's mission is to flag supply-chain trust risk before
adoption. A package whose README, recent PRs, or release notes
contain prompt-injection primitives is — independent of whether
those primitives have yet been exploited — a present-tense risk to
any AI-assisted consumer. Detecting that risk is exactly the kind
of forgery-resistant signal the trust model is built around: an
attacker who needs the payload to function cannot also hide it from
a structural scan.

This is the "Signatory as defender" axis of the CamoLeak threat:
not just "don't be a relay" (the hardening axis), but "be the tool
that flags packages a downstream AI assistant should not be allowed
to read."

## What to detect (the primitives)

The collector targets *structural* injection surfaces, not
semantic content. The set of structural primitives is small,
stable, and grep-amenable. False-positives are tolerable; false-
negatives are the cost we minimize.

| Primitive | Detection | Notes |
|---|---|---|
| HTML / markdown comments containing imperative-mood prose | Regex over `<!--…-->` blocks; presence of imperative verbs in non-trivial comments | Legitimate comments exist (TOC markers, lint-disable). Score on length × verb density, not bare presence. |
| Zero-width Unicode in prose files | Byte scan for U+200B/C/D, U+FEFF, U+2060 | Almost always hostile in non-code surface. Code files (Unicode identifiers) excluded by path filter. |
| Bidirectional control characters | Byte scan for U+202A–U+202E, U+2066–U+2069 | Universally suspicious outside i18n test fixtures. |
| Markdown image syntax pointing at parameterized hosts | Regex over `![…](URL)`; flag URLs with query strings, dictionary-of-pixels patterns, or hosts in known exfil-friendly proxy ranges | The CamoLeak-specific signature. |
| Lexical prompt-injection patterns in non-code prose | Regex / classifier over README / PR-body / release-notes for "ignore previous", "you are now", "system:", "as an AI", "<\|im_start\|>", role-tag tokens | Cheap. Noisy on technical writing about AI; mitigated by category weighting. |
| Hidden Unicode tag characters | U+E0000–U+E007F range | Used in published research as a near-invisible side channel; rare in legitimate content. |
| Embedded base-N encoded blobs in prose | Heuristic on long base16/base32/base64 runs in non-code files | CamoLeak's exfil format. Distinct from legitimate hashes and signatures by length distribution. |

The collector is multi-axis: a target's score is `(primitive count,
primitive class diversity, novelty since last release)`, not a single
boolean.

## Where this slots into the architecture

A new `MethodologyPattern.SignalGroup` value: `content-injection-surface`.

Per-pattern collector hints (from `internal/exchange/types.go`
`CollectorHint`):

- `GrepPrecision`: high for unicode-class primitives; medium-high for
  comment / image regex; medium for lexical patterns.
- `ReasoningDepth`: shallow. Most primitives are byte-level scans.
- `MissMode`: false-negatives preferred over false-positives — the
  attacker is rare; a noisy alert on a real one is the win, a silent
  miss is the loss. (Most of these patterns sit naturally on the
  false-positive side anyway.)

Output shape on a hit: a `Conclusion` with category
`content-injection-surface`, severity scaled by primitive class and
density, citations pointing at the specific file:line of each hit.

Composes-with relationships:

- `content-injection-surface` × low `Vitality` (abandoned project) →
  elevated severity (no maintainer to push back on a malicious PR).
- `content-injection-surface` × recent maintainer churn → elevated
  severity (account-takeover-like patterns).
- `content-injection-surface` × high `Criticality` → elevated
  severity (large blast radius).

## Why this is a forgery-resistant signal

Per the threat-economics frame, the test for whether a signal
deserves to be in the trust model is whether an attacker who *needs*
the underlying behavior can plausibly *fake* its absence. For every
primitive listed above:

- The attacker needs the payload to *function*. A markdown comment
  that's been escaped to render visible no longer hides the
  payload. A zero-width character that's been stripped no longer
  splits the token. The functional payload and the detected
  primitive are the same bytes; you cannot have one without the
  other.
- The legitimate use is rare and narrow (i18n testing, lint-disable
  markers). False-positives are cheap to label and flow back into
  the methodology catalog as `false_positive_notes` per the
  existing schema.

This is the same property that makes commit-history continuity a
durable signal and makes self-published star counts a brittle one:
attackers cannot decouple the indicator from the underlying
behavior they need.

## Where AI-instruction files (AGENTS.md, CLAUDE.md, .cursorrules) fit

This is a sharp question because such files sit at the intersection
of the legitimate and the malicious case for this signal class. They
are imperative content in repository files, addressed to AI agents,
intended to influence agent behavior. A naive
`content-injection-surface` collector would score them at 100% — they
*are* prompt injection, just consensual.

The two-fold framing:

### As a defensive surface (legitimate)

AI-instruction files are a form of project-author trust signal:
"this is what the project authors want AI agents working in this
repo to know / do." A consumer of the project (or its maintainer's
LLM tooling) reads them with the project author's authority. They
are part of the project's declared interface to AI tooling, the way
a `package.json` is part of its declared interface to a package
manager.

### As an attack surface (under-defended)

Critically: they are *just files in the repo*. They have no
cryptographic provenance beyond commit signatures (which most
projects don't enforce). They are subject to (Trapdoor 2026-05
documents most of these as in-the-wild attack patterns):

- **Malicious-PR injection** (confirmed by Trapdoor): the operator
  opened PRs against `browser-use`, `langchain`, `langflow`,
  `llama_index`, `MetaGPT`, and `OpenHands` proposing the addition
  of `.cursorrules` files. A reviewer skim-reads the diff among
  legitimate guidance and the AI-instruction payload lands.
- **Typosquat / install-time drop** (confirmed by Trapdoor): a
  malicious npm/PyPI package's `postinstall` script writes a
  `.cursorrules` or `CLAUDE.md` payload into the consumer's repo
  on `npm install`. 21 npm + 7 PyPI + 6 crates.io packages in the
  Trapdoor campaign exercised this shape.
- **Account-takeover** (the axios / TanStack pattern applied to
  agent-config files): a compromised maintainer pushes a malicious
  AGENTS.md update; downstream consumers' AI agents inherit the
  payload on next pull. Not yet observed in agent-config form
  specifically, but the surface is symmetric with the
  documented ATO incidents on other artifact classes.
- **Same primitives, applied to the AI-instruction file itself**
  (confirmed by Trapdoor): zero-width chars (U+200B/C/D, U+2060,
  U+FEFF), bidi controls, hidden markdown comments inside
  `CLAUDE.md` / `.cursorrules` are invisible to a human reviewing
  the file but ingested by the AI agent that reads it. Trapdoor
  used the zero-width-Unicode variant as its primary carrier.

The trust gradient AGENTS.md relies on (project author → AI agent
consumer) is exactly the gradient supply-chain attacks subvert. An
unscanned AI-instruction file is a strictly more privileged
injection vector than a README, because the AI agent treats its
contents as instructions *by design*.

### What this means for the collector

The collector should treat AI-instruction files as a **first-class
scan target with distinct scoring**:

1. **Inventory signal**: emit a Layer-1 signal for any project that
   ships a file in the AI-instruction set. The current taxonomy
   ([`internal/agentconfig/locus.go`](../internal/agentconfig/locus.go))
   covers:
   - **Repo-root instruction files**: `.cursorrules`, `CLAUDE.md`,
     `AGENTS.md`, `GEMINI.md`, `.windsurfrules`.
   - **GitHub Copilot custom instructions**:
     `.github/copilot-instructions.md` (repo-wide) and
     `.github/instructions/*.instructions.md` (path-scoped,
     per the
     [Copilot docs](https://docs.github.com/en/copilot/how-tos/copilot-on-github/customize-copilot/add-custom-instructions/add-repository-instructions)).
   - **Per-agent config directories**: `.claude/settings.json`,
     `.claude/CLAUDE.md`, `.cursor/rules/*.mdc`, `.aider.conf.yml`,
     `.zed/settings.json`, `.codex/instructions.md` (and
     `.codex/config.{json,yaml,yml,toml}`), `.continue/config.json`.

   The implemented inventory signal currently captures `{family,
   path}` per detected file. The spec's full payload — hash, byte
   length, last-modified, last-modifying author — is on the
   remaining-work list above. Useful for trust analysis even with
   no hostile content: "this project has AI instructions; here is
   their stability profile."
2. **Targeted structural scan**: run the
   `content-injection-surface` primitives on these files with
   *equal or higher* severity weighting than on a generic README.
   Imperative-mood prose is expected here — that primitive is
   useless. The other primitives (zero-width, bidi, hidden
   comments, image syntax, encoded blobs, novel-since-last-release
   imperatives) gain weight: there is no legitimate reason for
   them inside an AI-instruction file.
3. **Stability signal**: surface "AGENTS.md changed in last release
   by author X" as a recency signal. AI-instruction file churn near
   a release boundary is a posture-relevant event the way any
   privileged-config change is.

### Why we cannot fence AI-instruction files the way we fence MCP output

The `<quoted-from-target>` fence convention from `hardening.md` works
because signatory controls the egress path: we render the JSON string,
we wrap the content. AI-instruction files are loaded directly from
disk by the consumer's AI client. Signatory is not in the path; the
fence cannot be applied; the consumer LLM sees the file as an
instruction, exactly as the project author intended (and exactly as
an attacker intended).

The defense available to signatory is **detect-and-warn before
adoption**, plus **flag-on-change after adoption**. We make the
hostile content visible to the human deciding whether to trust the
project; the human, or the project's own review process, is the
control point. This is consistent with signatory's overall posture:
we surface signals, we do not enforce policy at runtime.

### Open question: should signatory ship its own AI-instruction-file
hash policy?

A natural extension: signatory could record an approved hash of a
project's AI-instruction file at posture-set time
(`vetted-frozen` / `trusted-for-now`), and flag any subsequent
divergence as a posture-relevant change requiring re-review. Same
pattern as a binary-pinning lockfile, applied to the AI surface.
This sits well past v0.1 but is consistent with the trust model.

## Open design questions

- **Multi-file scope scoring.** Per-file primitive counts roll up to
  per-target counts, but signal weight should depend on which file
  fired — AGENTS.md hits weigh heavier than README.md hits, which
  weigh heavier than test-fixture hits. Suggest a path-class table
  the collector consults.
- **Encoding-blob false positives.** Long base64 runs are common
  in legitimate contexts (badges, embedded PNGs, signatures). Need
  a length distribution model rather than a flat threshold.
- **Lexical pattern noise on AI-related projects.** A project *about*
  prompt injection will trip the lexical primitive constantly. The
  `false_positive_notes` schema field exists for this; the question
  is whether the collector should self-suppress based on category
  hints (e.g., a package whose own README declares "this is a
  prompt-injection research tool"). Probably no — explicit posture
  override is the right control, not collector-level whitelist.
- **Live PR-body scanning.** v0.1 reads from a static target snapshot;
  PR descriptions aren't in scope. A v0.2+ extension would scan the
  recent-PR feed of a target, since PR-description injection is the
  exact CamoLeak vector. Out of scope for the initial collector.

## Relationship to other planned work

- **`design/hardening.md §1`** — the egress-fence design — is the
  mirror of this entry. Hardening prevents signatory from being a
  relay; this entry makes signatory the detector for the same
  attack class against other tools. They share threat-model
  vocabulary and primitive definitions; a future implementation
  should pull the primitive list into a shared internal package
  used by both.
- **Layer-1 signal collector framework** (general): this is one
  more collector; nothing in the framework is specific to it.
- **Posture model**: an open question above suggests an
  AI-instruction-file hash-pinning extension to the posture
  semantics. Tracked here, not yet a separate design entry.
