# 2026-05-24: Trapdoor Crypto Stealer — Three-Ecosystem Campaign Includes Crates.io, Plants AI-Agent Config Files

## Source

Socket Threat Research, "Trapdoor Crypto Stealer: A Coordinated
Supply Chain Attack Across npm, PyPI, and crates.io"
(`socket.dev/blog/trapdoor-crypto-stealer-npm-pypi-crates`, fetched
2026-05-24). Reports 34 weaponized packages across three ecosystems
attributed to a single operator: 21 npm, 7 PyPI, 6 crates.io.
Earliest observed publish: `eth-security-auditor@0.1.0` on PyPI at
2026-05-22T20:20:18Z. Socket reports median artifact detection of
5m27s after publication, fastest 58s.

This entry pairs with the prior cross-ecosystem operator entries
[`2026-05-02-bufferzonecorp-campaign.md`](2026-05-02-bufferzonecorp-campaign.md)
(Go + gems, single operator, webhook.site C2) and
[`2026-05-12-tanstack-mini-shai-hulud.md`](2026-05-12-tanstack-mini-shai-hulud.md)
(TeamPCP, npm + PyPI propagation). Trapdoor is the third
independent data point and the first observed campaign of this
template that includes **crates.io** as a first-class ecosystem
target.

## Why this entry exists

Three things distinguish Trapdoor from prior entries:

1. **Crates.io is a coordinated peer, not a sideshow.** Prior
   crates.io supply-chain incidents on the public record
   (`rustdecimal` 2022, `xrvrv`/`postgress` 2023) were one-off
   typo-squats. Trapdoor weaponizes `build.rs` as the
   credential-stealer payload primitive rather than as the
   typo-squat trigger, and ships it as one of three parallel payload
   primitives in a single campaign. Cargo joins npm and PyPI as a
   peer attack surface in the operator playbook
   [bufferzonecorp §"Cross-ecosystem operator correlation"](2026-05-02-bufferzonecorp-campaign.md)
   first named.
2. **AI-agent config files are a first-class persistence locus.**
   The TanStack entry §"Developer-workstation persistence locus"
   already added `.claude/`, `.vscode/`, `.cursor/` as agent/IDE
   config *directories*. Trapdoor goes one level deeper: `.cursorrules`
   and `CLAUDE.md` *files* with zero-width-Unicode payloads aimed
   at the consumer's AI coding assistant, not at the
   shell/editor/runner. A new persistence sub-locus and a new
   payload-delivery surface (LLM prompt injection in repo-level
   config files) that the v0.1 signal set covers partially but not
   yet at the file or content level.
3. **PR-against-legit-AI-project as a propagation vector.** Socket
   documents the operator opening pull requests against
   `browser-use`, `langchain`, `langflow`, `llama_index`, `MetaGPT`,
   and `OpenHands` proposing malicious `.cursorrules` files. Neither
   typo-squat nor maintainer-takeover nor registry compromise — a
   social-engineering-shaped vector that uses the legitimate PR flow
   of consumer-facing AI projects to plant config payloads that fire
   inside future agent sessions on developer workstations. No
   current signal axis covers this consumer-side surface.

## The campaign shape

Reconstructed from the Socket writeup:

1. Stand up throwaway accounts: GitHub `ddjidd564`, npm `asdxzxc`,
   PyPI `asdmini67` and `dae5411`. Domain
   `ddjidd564.github.io/defi-security-best-practices/` as a
   landing page. Campaign marker `P-2024-001` embedded in payloads.
2. Pick a developer community to target — here, the Move/Sui/DeFi
   blockchain-tooling vertical (and a parallel AI-coding-assistant
   vertical for the consumer-PR vector). Mass-publish 34 packages
   across npm/PyPI/crates.io over a weekend, with naming patterns
   matched to that community's expected dependency vocabulary:
   - **crates.io (6):** `move-analyzer-build`, `move-compiler-tools`,
     `move-project-builder`, `sui-framework-helpers`,
     `sui-move-build-helper`, `sui-sdk-build-utils`.
   - **npm (21):** `crypto-credential-scanner`, `eth-wallet-sentinel`,
     `wallet-backup-verifier`, `solidity-deploy-guard`,
     `web3-secrets-detector`, `mnemonic-safety-check`, plus
     AI-developer-tool-adjacent names (`llm-context-compressor`,
     `model-switch-router`, `prompt-engineering-toolkit`,
     `token-usage-tracker`).
   - **PyPI (7):** `cryptowallet-safety`, `defi-risk-scanner`,
     `eth-security-auditor`, `solidity-build-guard`, `git-config-sync`,
     `env-loader-cli`, `data-pipeline-check`.
3. **npm payload:** `postinstall` hook drops `trap-core.js` (1,149
   LOC, 48,485 bytes) at install time. Sophisticated encryption
   layering — Fernet + ECDH on top of XOR — and live credential
   validation against AWS / GitHub API endpoints before exfil.
4. **PyPI payload:** imports execute remote JavaScript via `node -e`,
   downloading code from attacker infrastructure and executing
   through a host-side Node runtime. A Python package shelling out
   to Node is the unusual primitive — likely chosen so a single
   payload codebase serves both npm and PyPI ecosystems.
5. **crates.io payload:** `build.rs` executes at `cargo build` time,
   sweeps local keystores (SSH, Sui/Solana/Aptos wallets, AWS, GitHub,
   browser profiles, browser crypto-extension stores), encrypts with
   XOR keyed on the literal string `cargo-build-helper-2026`, and
   exfiltrates to GitHub Gists via the public Gist API. The XOR
   layer is markedly weaker than the npm payload's Fernet/ECDH —
   reflective of either parallel-implementation-by-different-author
   or staged maturity.
6. Exfiltration via **GitHub Gists** for the cargo half (and per
   Socket, also for the broader campaign's secondary channel).
   At the network layer this is `api.github.com` — indistinguishable
   from legitimate GitHub API traffic, defeats domain-blocklist
   defenses, and runs inside the egress allow-list of nearly every
   developer workstation and CI runner.
7. **Persistence**: writes `.cursorrules` and `CLAUDE.md` files
   containing zero-width-Unicode-encoded instructions aimed at the
   developer's future AI agent sessions; also git hooks, shell rc
   files, systemd user units, cron entries, and (where SSH keys are
   harvested) lateral-movement seeding.
8. **Propagation via legit-project PRs:** open pull requests against
   established AI-tooling repos (browser-use, langchain, langflow,
   llama_index, MetaGPT, OpenHands) proposing the addition of
   `.cursorrules` files that contain zero-width-Unicode instructions
   directing the project's AI coding assistant to perform
   "security scans" that exfiltrate credentials. The repo doesn't
   need to be compromised — only one merged PR is needed; from then
   on, every contributor running an AI agent on a fresh clone fires
   the payload.

The runtime credential-target list is the same as bufferzonecorp
[§"Credential-target list is incomplete"](2026-05-02-bufferzonecorp-campaign.md)
and TanStack: SSH keys, AWS, GitHub tokens, browser login databases,
browser crypto-extension stores, with the addition of **wallet-software
data files** (Sui, Solana, Aptos keystore formats) as a new target
class consistent with the Move/Sui community framing.

## What this validates in our existing model

The cross-ecosystem operator pattern now has a **third** independent
data point. bufferzonecorp made the case (Go + gems, single
operator); TanStack confirmed it (TeamPCP, npm + PyPI in the same
campaign window); Trapdoor expands it to npm + PyPI + crates.io with
a coherent single-operator playbook across all three.
[bufferzonecorp §"Cross-ecosystem operator correlation"](2026-05-02-bufferzonecorp-campaign.md)
proposed an `operator:` or `campaign:` entity URI as the v0.2
abstraction; with Trapdoor the proposal is no longer speculative —
three data points in three weeks make the abstraction load-bearing
for any analyst trying to query "what else has this operator
shipped?" across ecosystems.

The **install-time-execution methodology pattern** named in
bufferzonecorp §"Install-time execution methodology pattern is
Go-specific" is now confirmed at full cross-ecosystem breadth in a
single campaign: npm `postinstall`, PyPI `setup.py`-as-installer,
cargo `build.rs`. Each ecosystem's source-AST analyzer is the right
place to count compile/install-time hook contents; cargo's analyzer
is the one we don't have yet (registry-side
`build_script_introduced` exists at
`internal/signal/registry/cargo/collector.go:494`, but we cannot
read *inside* `build.rs` files).

The **C2-destination-class corpus** named in bufferzonecorp
§"C2-destination-class as a corpus signal" gets a third sub-class:
GitHub Gists join `webhook.site` (request-capture-as-a-service)
from bufferzonecorp and `filev2.getsession.org` (P2P-messaging-
network / decentralized-relay) from TanStack. The new sub-class is
**legitimate-platform-as-exfil**: the destination is a well-known
service host, indistinguishable from legitimate traffic at the
network layer, and (unlike capture-as-a-service hosts) carries
genuine legitimate developer traffic.

## What this exposes as a gap

### AI-agent config files as a first-class detection surface, across three layers

[TanStack §"Developer-workstation persistence locus"](2026-05-12-tanstack-mini-shai-hulud.md)
added `.claude/`, `.vscode/`, `.cursor/`, `.zed/`, `.aider*/` as
directory-level persistence loci visible only inside the node
analyzer's `persistencePathPatterns` catalog
(`internal/signal/source/node/analyze.go:214–226`). Trapdoor
extends the surface in three ways at once — file-level paths
that the directory-substring catalog already mostly catches,
*content-level* hidden Unicode that no current signal catches at
all, and a propagation vector (the PR-against-legit-AI-project
shape) that the v0.1 signal set does not address.

The right model splits cross-ecosystem detection into three
distinct layers, ordered by ecosystem-agnosticism and per-LOC
leverage:

**Layer 1 (highest leverage, ecosystem-agnostic): repofiles
content awareness — LANDED.** Implementation pulled the broader
[`design/anti-subversion.md`](../anti-subversion.md) from
`design/potential/` to active status: the seven-primitive
content-injection-surface catalog ([`internal/contentinjection/`](../../internal/contentinjection/))
is the shared package for this signal class, and AI-agent-config
files are the first consumer.

The new code paths:

- [`internal/contentinjection/`](../../internal/contentinjection/)
  implements all seven anti-subversion primitives: invisible
  Unicode (zero-width family + bidi controls + tag block),
  markdown HTML comments with imperative-mood prose, markdown
  image syntax with exfil-shaped URLs, lexical injection phrases,
  long base-N encoded blobs. Scan / ScanFile aggregate the
  primitives over arbitrary content. False-negative-preferring
  thresholds per design doc.
- [`internal/signal/repofiles/agent_config_families.go`](../../internal/signal/repofiles/agent_config_families.go)
  declares `AgentConfigFamilies()` — the AI-instruction file
  surface: `.cursorrules`, `CLAUDE.md`, `AGENTS.md`,
  `.claude/settings.json`, `.claude/CLAUDE.md`, `.cursor/rules/*.mdc`,
  `.aider.conf.yml`, `.zed/settings.json`, `.continue/config.json`,
  `.windsurfrules`.
- [`internal/signal/repofiles/collector.go`](../../internal/signal/repofiles/collector.go)
  `detectAgentConfig` is a sibling probe to `detectProcMacro`,
  emitting two new always-on signals registered in
  [`internal/signal/types.go`](../../internal/signal/types.go):
  - `agent_config_files` (hygiene, low-declining): inventory of
    detected AI-instruction files per family. The §"Inventory
    signal" axis from [`anti-subversion.md`](../anti-subversion.md).
  - `agent_config_content_injection` (publication, high): the
    content-injection-surface findings on those files. Empty
    findings is the positive "we-scanned-and-found-nothing"
    observation.

What this catches *cross-ecosystem*:

- The dep's own repo ships `.cursorrules` with hidden Unicode —
  fires regardless of whether the package is npm/PyPI/cargo/gem.
- A consumer-side analysis (signatory on the consumer's own
  repo) flags hidden Unicode in their own `.cursorrules` —
  useful for the PR-attack vector documented later in this entry.
- README / PR / release-notes consumers are next on the same
  primitive package — same Scan call, different file roles.

**Layer 2 (cross-ecosystem at the tarball boundary): artifact
categorization — LANDED.** The artifact-vs-repo collector at
[`internal/signal/artifact/`](../../internal/signal/artifact/)
already extracts release tarballs across npm / PyPI sdist /
cargo .crate / gem / GitHub releases and surfaces
`files_extra_in_tarball` (the xz-utils-shaped "in tarball but
not in git" signal). Trapdoor added a category to that
machinery: an agent-config file shipped *in the tarball only* —
not in the source repo at that SHA — is the xz-utils shape
applied to AI-agent injection.

Implementation:

- [`internal/signal/artifact/categorize.go`](../../internal/signal/artifact/categorize.go)
  declares `CategoryAgentConfig` and inserts it in the
  `classify()` order after `suspicious_path` but before
  `generated` / `vendored` / `build_glue` / `binary_in_tests` /
  `other`. A path matching the agent-config taxonomy lands in
  the new bucket; the rest stays as before.
- The categorizer consults `repofiles.IsAgentConfigPath` (which
  itself delegates to `agentconfig.IsConfigPath` — see Layer 3
  notes on the unification refactor) so the agent-config
  taxonomy has one source of truth, not two parallel lists.
- No new signal type required: the existing
  `artifact_repo_divergence` payload's `ClassifiedEntry.category`
  field carries the new value. The signal type's caveat list in
  [`internal/signal/types.go`](../../internal/signal/types.go)
  documents the new bucket and cites Trapdoor as the motivating
  campaign.

**Layer 3 (per-language, source-AST runtime-write detection):
shared-catalog refactor — LANDED (two commits: refactor + add).**
A *runtime-write* signal — catching
`fs.writeFileSync('.cursorrules', payload)` *inside the dep's
own code* — is the source-AST layer's job, and lives in the
`PersistencePathPatterns` catalog. The catalog was previously a
node-only copy of what its own comment declared language-neutral;
the python analyzer carried an identical literal copy of
`SensitivePathPatterns`. The duplication is a structural defect
the existing code already documented and the refactor closed.

The shared catalogs now live in
[`internal/signal/source/astfeature/catalogs.go`](../../internal/signal/source/astfeature/catalogs.go):

- `SensitivePathPatterns` + `IsSensitivePath`
- `PersistencePathPatterns` + `IsPersistencePath`
- `CredentialEnvNames` + `IsCredentialEnvName`
- `CloudMetadataHosts` + `IsCloudMetadataURL`

The API-name-shape catalogs (`writeSinkCallees`,
`processExecCallees`, `networkCallees`, `base64DecodeCallees`,
`pathReadCallees`) stayed per-language because those genuinely
vary by ecosystem. The split between *what the payload targets*
(OS-shape, shared) and *how the payload calls it* (language-
shape, per-analyzer) is now reified in the codebase.

The Trapdoor-shape additions landed in the same package:

`SensitivePathPatterns` — wallet-software keystore reads
(Trapdoor's cargo payload class):

- `/.sui/`, `/.config/solana/`, `/.aptos/`,
  `/.ethereum/keystore/`, `wallet.dat` — Sui, Solana, Aptos,
  Ethereum keystore, Bitcoin.

`PersistencePathPatterns` — AI-agent loci are now derived (see
unification note below) from `agentconfig.RuntimePersistencePrefixes()`,
which currently expands to: `/.cursorrules`, `/CLAUDE.md`,
`/AGENTS.md`, `/.claude/`, `/.cursor/`, `/.aider/`, `/.zed/`,
`/.codex/`, `/.continue/`, `/.windsurfrules`, `/.windsurf/`.

Layer 3 reach is limited today: node is the only analyzer that
populates `SensitivePathWrites`. Python wiring is a separate
parity task; cargo and gem source-AST analyzers don't exist
(though both ecosystems are first-class everywhere else —
registry collectors, manifest parsers, artifact-vs-repo, and
repofiles all carry them). The shared-catalog refactor unblocks
all four when their respective wiring lands; until then,
Layers 1 and 2 carry the cross-ecosystem coverage.

**Unification refactor (follow-on, LANDED):** During Layer 3
dogfooding, an asymmetry surfaced — the new `/.codex/` entry
sat in the runtime-substring catalog but had no corresponding
Family in `repofiles.AgentConfigFamilies`. The fix introduced
[`internal/agentconfig/`](../../internal/agentconfig/) as the
single source of truth for the AI-agent locus taxonomy:

- `Locus` pairs the file-detector shape (`Dirs`, `Detector`,
  `Preferred`) with `RuntimePathPrefixes`. One declaration, two
  consumer shapes.
- `agentconfig.Loci()` returns the 11-entry canonical list
  (the 10 prior Families plus the now-properly-declared
  `codex_instructions` Locus that closes the divergence bug).
- `repofiles.AgentConfigFamilies()` maps `Loci()` to `Family`
  records for the in-repo scanner.
- `astfeature.PersistencePathPatterns` is an init-built slice
  with the non-AI patterns from the literal plus
  `agentconfig.RuntimePersistencePrefixes()` appended. Adding
  a new AI-agent toolchain is now one declaration; the prior
  dual-update bug is structurally impossible at test time
  (`TestLoci_AllFieldsDeclared` requires every Locus to declare
  `RuntimePathPrefixes`).

**Sequencing:** Landed in order — Layer 1, Layer 2, Layer 3
(refactor commit then additions commit), then the unification
refactor that consolidated the AI-agent locus taxonomy in
`agentconfig`.

### Cargo source AST is the dominant cargo-side gap

Registry-side coverage of cargo is solid: the cargo collector
already emits `build_script_present` and `build_script_introduced`
(`internal/signal/registry/cargo/collector.go:494` and
`internal/signal/types.go:840–851`), the cargo equivalents of
`postinstall_introduced`. Trapdoor's `build.rs`-introduction would
fire on these.

What's missing is **inside-the-file inspection**: the cross-language
astfeature.Counts contract
(`internal/signal/source/astfeature/counts.go`) — `EnvCredentialReads`,
`SensitivePathReads`, `SensitivePathWrites`, `CloudMetadataCalls`,
`DynamicEvalCalls`, `Base64DecodeCalls`, `XORAssignments` — is
populated by Go, Python, and Node analyzers; cargo would be the
fourth, following the leaf pattern the `project_npm_ast` memory
already named. A Rust analyzer scoped to `build.rs` files only
(small per-crate surface, single entry-point file) is the
right next ecosystem to land — and the Trapdoor cargo payload is
exactly the corpus to TDD against. The XOR-with-literal-key
primitive in particular (`XOR_KEY = "cargo-build-helper-2026"`)
would spike `XORAssignments` in the same way the campaign-shape
node analyzer already counts.

This entry does **not** propose to implement the cargo analyzer
here; it records the gap and the corpus.

### GitHub Gist as a legitimate-platform-as-exfil host class

`api.github.com/gists` POSTs are a structurally distinct exfil
channel from the host classes already named:

- **Request-capture-as-a-service** (webhook.site, requestbin) —
  bufferzonecorp's host class. Destination domain has no purpose
  but request capture; bounded-membership corpus possible.
- **P2P-messaging-network / decentralized-relay** (getsession.org
  snodes) — TanStack's host class. Destination has a legitimate
  protocol-layer purpose but the application-layer use is
  attacker-only in practice.
- **Legitimate-platform-as-exfil** (GitHub Gists, paste.ee,
  controlc.com, Pastebin, transfer.sh, file.io) — Trapdoor's host
  class. Destination is a well-known service used heavily by
  legitimate developers, and the *traffic itself* is
  indistinguishable from legitimate traffic at the network layer.

The third class needs its own corpus-membership rule. A naïve
"is this a known paste service" allowlist would be wrong because
many of these services have legitimate use inside developer
workflows. The signal-bearing observation is more specific:
**a postinstall / build.rs / import-time path containing an HTTP
POST whose destination is a paste-class host** is the shape — the
class-of-host + the timing + the verb together, not the host alone.
This is closer to a derived-signal composition than a corpus entry,
and lives at the analyst layer rather than the collector layer for
v0.1.

### PR-against-legit-AI-project consumer-side posture

Trapdoor's operator-controlled PRs against browser-use, langchain,
langflow, llama_index, MetaGPT, OpenHands open an attack class
v0.1 does not model at all: the **consumer's own repository** is
the attack surface, and the malicious content arrives via the
legitimate PR flow rather than via a dependency update.

[prt-scan §"Consumer posture signals are as important as dependency signals"](example-prtscan-attack.md)
opened the consumer-side posture surface in the
`pull_request_target` direction. The Trapdoor angle is a sibling:
**proposed-additions-of-agent-config-files by external contributors
without commit-trust history**. The detection shape would be a
consumer-side collector that, given a repo, lists PRs that
add/modify `.cursorrules`, `CLAUDE.md`, `.claude/`, `.cursor/`,
`.aider/`, `.zed/`, etc., from contributors with no prior
commit history on the repo. New collection surface; defer behind
the catalog-addition and content-inspector work above.

### Wallet-software keystore paths in the read-side catalog (LANDED)

The Layer 3 additions placed `/.sui/`, `/.config/solana/`,
`/.aptos/`, `/.ethereum/keystore/`, and `wallet.dat` in the
shared `SensitivePathPatterns` alongside the existing
SSH/AWS/PyPI/npm/GnuPG/Docker/Kube/gcloud/Azure entries. Any
analyzer that wires up `SensitivePathReads` gets the wallet
keystores automatically. The category is kept as a named
membership group in the catalog test
([`TestCatalog_SensitivePath_Membership`](../../internal/signal/source/astfeature/catalogs_test.go))
because wallet keystores are a structurally distinct credential
class — not standard developer credentials, present specifically
because of the Trapdoor cargo-payload target shape.

## Empirical: what the current signal model says

Not run against Trapdoor packages directly — at the time of
writing, npm Security has pulled most of the 21 npm packages,
PyPI has yanked the 7 PyPI packages, and the 6 crates.io
packages were removed within 48h of publication. Per the
TanStack entry §"Empirical: what the current signal model says
at T+~21h", a post-cleanup analysis would show the same
pattern: `version_publish_burst.burst_detected = false` against
a registry state that no longer contains the malicious versions,
and `version_unpublish_observed` (the post-cleanup detector
landed in the TanStack session) firing against the
unpublish/yank/removal gap.

The campaign's median 5m27s detection window per Socket reinforces
the
[`example-axios-attack.md` lesson 16](example-axios-attack.md)
framing: minutes-to-hours detection is achievable when
publish-cadence anomaly + cross-ecosystem-operator + payload
shape all fire together. The composite stays strong; the lesson
Trapdoor adds is that the composite must include **cargo** as a
peer ecosystem in the cross-correlation, not just as an
afterthought.

## What this does *not* do

### Does not add `ddjidd564`, `asdxzxc`, `asdmini67`, `dae5411`, or `ddjidd564.github.io` to a burn list

Per the discipline established in
[bufferzonecorp §"Does not maintain a per-vendor IOC list"](2026-05-02-bufferzonecorp-campaign.md)
and reinforced in
[TanStack §"Does not add `voicproducoes`, …"](2026-05-12-tanstack-mini-shai-hulud.md):
these are facts of this incident, not durable signals. The
campaign marker `P-2024-001` and the XOR key
`cargo-build-helper-2026` are recorded for reference only.

### Does not maintain a per-vendor paste-service blocklist

The legitimate-platform-as-exfil host class needs its own
detection shape (host class + timing + verb), not a vendor
blocklist that would be either too permissive (allowing real
paste-service use) or too restrictive (blocking legitimate
developer workflows). The composition lives at the analyst
layer for v0.1; the corpus discipline established in
bufferzonecorp §"C2-destination-class as a corpus signal" still
applies — typology of host classes with bounded membership, not
a vendor IOC list.

### Does not claim Trapdoor is the first crates.io supply-chain incident

`rustdecimal` (2022) typo-squat of `rust_decimal` and the 2023
`xrvrv` / `postgress` family are prior public incidents. What's
new with Trapdoor is the **coordinated three-ecosystem campaign**
that includes crates.io as a peer attack surface alongside
npm/PyPI, with `build.rs` weaponized as the credential-stealer
payload primitive rather than as a typo-squat trigger. The
framing matters because it changes what an operator-level
detection model needs to do: cargo isn't a long-tail
afterthought, it's in the rotation.

### Does not implement the cargo source AST in this entry

The cargo source-AST analyzer is the dominant gap Trapdoor
exposes, but implementation is a multi-commit effort following
the npm-ast / python-ast leaf pattern in
`internal/signal/source/<lang>/`. The `project_npm_ast` memory
named it as the next ecosystem; this entry records the
incident-shaped corpus (the 6 Trapdoor crates and their
`build.rs` files, while still observable on the wayback /
mirror surface) that should drive TDD when the work lands.

**Update 2026-05-26**: the cargo source-AST analyzer landed across
branch `cargo-rust-ast` (commits `ad580f2` … `e3a65fc`). The
per-attack-shape bucketing map and the explicit "structurally
cannot see" list live at
[`2026-05-26-cargo-ast-coverage.md`](2026-05-26-cargo-ast-coverage.md).
The synthetic clean → clean → weaponized integration fixture under
`internal/signal/source/collector_test.go` covers the Trapdoor
primitive set (named env-credential reads, sensitive-path reads,
persistence writes, IMDS contact, base64 decode, XOR obfuscation,
attacker exfil, shell exec). The
`XOR_KEY = "cargo-build-helper-2026"` primitive in particular
spikes `XORAssignments` as predicted.

The follow-on work also closed two gaps this incident exposed
that weren't named explicitly in the original write-up:

  - **Pin-source second tier (commit `c5d29bc`)**: the cargo
    pin-table emitter now upgrades the recent-window pins from
    `cargo-tag-match` to `cargo-vcs-info` by recovering the
    publisher-stamped SHA from `.cargo_vcs_info.json` inside the
    `.crate` tarball, parallel to npm's gitHead → attestation
    upgrade. Live-dogfood on anyhow + serde: zero SHA divergence
    between the two tiers.
  - **Born-malicious detection signal class (commit `e3a65fc`)**:
    the original differential `source_evolution_anomaly` was
    structurally blind to the dominant Trapdoor shape (every
    published version weaponized from v0.1.0 — no clean baseline
    to cross from). A new in-situ companion
    `source_evolution_concern` evaluates each row independently
    against the rare-on-benign Counts subset (the 9 fields where
    any non-zero value is signal-bearing without cross-version
    context). The Trapdoor six crates' v0.1.0 would now fire
    concern with `env_credential_reads`, `sensitive_path_reads`,
    `sensitive_path_writes`, `exec_calls`, `xor_assignments`,
    `cloud_metadata_calls` named — exactly the primitive set this
    incident catalogued. Dogfood-validated zero on every
    legitimate target (cargo/anyhow, cargo/serde, npm/ms,
    golang/kong, pypi/sigstore).

## Open questions

- **(Resolved 2026-05-24.)** Should the Layer 3 catalog-sharing
  extraction (move `sensitivePathPatterns`,
  `persistencePathPatterns`, `credentialEnvNames`,
  `cloudMetadataHosts` from each analyzer into
  `internal/signal/source/astfeature/`) ship as a refactor-only
  commit *before* the Trapdoor-shape additions, or as a single
  combined commit (extract + add)? **Outcome:** refactor-first
  (commit `79ae194`), Trapdoor entries followed (commit
  `a4acfc0`). Bisect-friendly and made the catalog-membership
  drift that surfaced in dogfood (`/.codex/`) easier to attribute
  and fix.
- **(Resolved 2026-05-24.)** Where does the Layer 1
  content-inspector belong —
  inside `repofiles/` (extending presence collection with
  content peek for one specific file family), or as a sibling
  collector under `internal/signal/`? **Outcome:** the
  content-inspection primitive package landed at
  [`internal/contentinjection/`](../../internal/contentinjection/)
  (top-level under `internal/`, since the design doc notes it's
  shared with the hardening egress side); `repofiles` consumes
  it via `detectAgentConfig`. The AI-agent locus taxonomy
  itself landed separately at
  [`internal/agentconfig/`](../../internal/agentconfig/) as the
  single source of truth for both the in-repo Family detectors
  and the runtime-write substring catalog.
- Does Trapdoor's PR-against-legit-AI-project vector belong
  inside the consumer-side posture surface
  [prt-scan](example-prtscan-attack.md) opened, or under a
  new "agent-config-injection" axis? The detection shape is
  consumer-side either way; the question is whether
  agent-config-PR-from-untrusted-contributor reads as a
  workflow-posture concern or as its own signal group.
- How should the `operator:` / `campaign:` entity URI
  represent a campaign that spans **three** ecosystems? The
  bufferzonecorp two-ecosystem framing implied a
  pairwise-cross-ecosystem-link model; Trapdoor's three-way
  link argues for an n-ary operator entity that grouping
  identities can join, rather than a chain of pairwise
  cross-references.

## Cross-references

- [`2026-05-02-bufferzonecorp-campaign.md`](2026-05-02-bufferzonecorp-campaign.md)
  — campaign-entry template; cross-ecosystem operator URI
  proposal (this entry is the third independent data point);
  C2-destination-class corpus (this entry adds the
  legitimate-platform-as-exfil sub-class); install-time
  execution methodology pattern.
- [`2026-05-12-tanstack-mini-shai-hulud.md`](2026-05-12-tanstack-mini-shai-hulud.md)
  — developer-workstation persistence locus (this entry adds
  the file-level and AI-agent sub-loci); P2P-messaging-network
  host class (this entry adds the legitimate-platform sibling);
  cross-ecosystem-operator framing in the TeamPCP profile.
- [`example-prtscan-attack.md`](example-prtscan-attack.md) —
  consumer-side posture surface; the
  PR-against-legit-AI-project vector this entry opens is a
  sibling consumer-side surface in the agent-config-injection
  direction.
- [`example-axios-attack.md`](example-axios-attack.md) —
  detection-cadence lesson 16; Trapdoor's 5m27s median
  reinforces the cross-correlation composition.
- [`../anti-subversion.md`](../anti-subversion.md) — promoted
  from `design/potential/` on 2026-05-24; specs the
  `content-injection-surface` signal class. AI-instruction files
  are the first consumer; README / PR / release-notes are next.
  Cites Trapdoor as the motivating campaign.
- [`../../internal/agentconfig/`](../../internal/agentconfig/)
  — single source of truth for the AI-agent locus taxonomy.
  `Locus` pairs the file-detector shape with the runtime-path
  substring patterns; `Loci()`, `IsConfigPath(p)`, and
  `RuntimePersistencePrefixes()` serve the two consumer surfaces
  without duplicated declarations.
- [`../../internal/contentinjection/`](../../internal/contentinjection/)
  — the shared primitive package (invisible Unicode, bidi
  controls, tag block, markdown HTML comments, markdown image
  syntax, lexical injection phrases, encoded base-N blobs).
  Anti-subversion §"What to detect" implementation. Top-level
  under `internal/` because the design doc notes it's shared
  with the hardening egress side.
- [`../../internal/signal/source/astfeature/catalogs.go`](../../internal/signal/source/astfeature/catalogs.go)
  — the shared OS/credential-store-shape catalogs extracted
  from the node and python analyzers:
  `SensitivePathPatterns` + `IsSensitivePath` (now includes the
  Trapdoor wallet-keystore entries), `PersistencePathPatterns`
  + `IsPersistencePath` (AI-agent loci derived from
  `agentconfig.RuntimePersistencePrefixes()`),
  `CredentialEnvNames` + `IsCredentialEnvName`,
  `CloudMetadataHosts` + `IsCloudMetadataURL`.
- [`../../internal/signal/source/astfeature/counts.go`](../../internal/signal/source/astfeature/counts.go)
  — the cross-language Counts contract. Documents the per-
  language wiring state for `SensitivePathWrites` and friends.
  Cargo and gem source-AST analyzers don't yet exist; when they
  do, they inherit the shared catalogs automatically.
- [`../../internal/signal/repofiles/agent_config_families.go`](../../internal/signal/repofiles/agent_config_families.go)
  — `AgentConfigFamilies()` derived from `agentconfig.Loci()`;
  `IsAgentConfigPath(p)` delegated to `agentconfig.IsConfigPath`.
- [`../../internal/signal/repofiles/collector.go`](../../internal/signal/repofiles/collector.go)
  `detectAgentConfig` — the sibling probe emitting the two
  always-on signals `agent_config_files` (hygiene inventory) and
  `agent_config_content_injection` (content-injection findings).
- [`../../internal/signal/artifact/categorize.go`](../../internal/signal/artifact/categorize.go)
  — `CategoryAgentConfig` bucket added to the artifact-vs-repo
  categorizer; consults `repofiles.IsAgentConfigPath` so the
  Layer 1 / Layer 2 paths share the same taxonomy via
  `agentconfig`.
- [`../../internal/signal/source/node/analyze_test.go`](../../internal/signal/source/node/analyze_test.go)
  `TestThreat_AIAgentConfigWrites` /
  `TestThreat_WalletKeystoreReads` — incident-shaped TDD for
  the Trapdoor catalog additions (true positive + benign twin
  per the project's TDD policy).
- [`../../internal/signal/registry/cargo/collector.go`](../../internal/signal/registry/cargo/collector.go)
  `recordBuildScriptIntroduced` — registry-side
  `build_script_introduced` signal that Trapdoor's cargo
  packages would fire today, even without the source-AST
  analyzer.
- [`../trust-model.md`](../trust-model.md) §"Signals must be
  weighted by forgery resistance" — install-time hook
  execution is at the **High** tier, not Very High; cargo's
  `build.rs` is at the same tier as npm's `postinstall` and
  PyPI's `setup.py`. Trapdoor exercises all three.
