# atuin (github.com/atuinsh/atuin)

**Role: Development (elevated blast radius — captures every shell command)**
**Decision: Analysis only — no posture recorded**
**Recommended posture if adopted: Trusted-for-now**
**Date: 2026-04-14**

## Framing Notes

Two things that make this analysis unusual, flagged so they don't get
lost in the signal noise:

1. **Atuin is not a library and not a signatory dependency.** It's a
   Rust-built user-facing CLI (plus optional hosted sync service).
   Analysis was done as an exercise in applying the trust model to
   non-Go, non-library targets. It surfaced gaps in the role taxonomy
   and the signal set — those gaps are captured in
   `design/signal-storage-evolution.md` and the `vet-dependency` skill
   update that landed alongside this file.
2. **The "Development" role doesn't really fit.** The skill's four
   roles (runtime / validation / build-only / development) were written
   for library dependencies. Atuin is installed on developer
   workstations and reads **every interactive shell command** —
   including credentials and tokens accidentally typed into prompts.
   That's a blast radius somewhere between "editor plugin" and
   "authentication agent." "Development (elevated)" is the closest
   existing bucket; a proper taxonomy is a to-do.

## Dependency Role

Atuin replaces the shell's builtin history. It runs a background daemon
(tonic/gRPC), writes every command into a local SQLite database, and
optionally syncs client-side-encrypted records (PASETO V4) to either
the hosted atuin.sh service or a self-hosted server.

Failure mode if compromised at the supply-chain level:

- **Per-keystroke credential harvester.** Shell history routinely
  contains passwords, tokens, `export API_KEY=...`, and secrets typed
  into prompts that accept them on stdin. All of this would pass
  through a compromised atuin.
- **Outbound network access by design.** Sync mode phones home
  (configurable endpoint). A malicious release can exfiltrate without
  needing to add new network capability.
- **Deployed on every dev machine in an org that adopts it.** Supply
  chain compromise = fleet-wide exposure.

Using hosted atuin.sh sync is a **second, independent trust decision**
on top of installing the binary: you're trusting the `atuinsh` company
to run a service that holds (client-encrypted) command histories.
PASETO V4 local encryption protects content at rest on their servers,
but metadata, uptime, and custody remain theirs.

## Signal Table

| Signal | Value | Assessment |
|--------|-------|------------|
| Purpose | Shell history database + sync | User-facing tool, not a library |
| Language / ecosystem | Rust (Cargo workspace, 11 crates) | Outside skill's original Go scope |
| Owner | atuinsh (organization) | Org created Jan 2023, wraps creator's work |
| Creator / lead | Ellie Huxtable (@ellie) | Public identity since Jul 2019 (6.7yr) |
| Repo created | Oct 2020 | 5.5 years old |
| Last commit | 2026-04-14 (today) | Extremely active |
| Latest release | v18.14.1 (2026-04-13) | Released yesterday |
| Prior release cadence | v18.13.6 → v18.14.0 in ~17 days | Regular, non-bursty |
| Total commits | 1,751 | Healthy volume for age |
| Commit distribution | ~151 by end y1, ~315 by end y2, 1,751 now | Accelerating, not fallow |
| Top contributors | ellie 698, conradludgate 81, BinaryMuse 74, akinomyoga 47, arcuru 29 | Concentrated lead; real bench |
| Commit signing | All recent commits GPG-verified | Strong |
| Stars | 29,152 | Very high adoption |
| Forks | 827 | Healthy fork ecosystem |
| Open issues | 478 | High absolute, expected at this scale |
| License | MIT | Permissive, standard |
| Temporal era span | Pre-LLM → Modern AI | Active development ongoing |
| Supply chain policy | `deny.toml` (cargo-deny) present and configured | **Strong positive** |
| Advisory suppressions | 2 ignores, each with written rationale | Conscientious, not rubber-stamped |
| License allow-list | MIT/Apache/BSD/ISC/MPL/OpenSSL/Unicode-DFS; `default = "deny"` | Restrictive, explicit |
| CI workflows | 8: rust, release, docker, installer, nix, shellcheck, codespell, update-nix-deps | Thorough |
| Release tooling | cargo-dist (axodotdev) | Standardized, reproducible |
| Dependabot | Active (283 bot commits) | Automated dep updates |
| Unsafe-code policy | `#![deny(unsafe_code)]` client/common, `#![forbid(unsafe_code)]` server | Above baseline |
| Clippy config | pedantic + nursery, `-D warnings` enforced in CI | Above baseline |
| Security disclosure | **No SECURITY.md** | Gap — no documented reporting channel |
| Community health % | 75% (GitHub community profile) | Adequate, not complete |
| AGENTS.md | Present | AI-tooling awareness (modern era signal) |
| crates.io trusted publishing | **Not verified in this pass** | Follow-up signal |

## Author/Org Provenance

**Ellie Huxtable** — cross-platform-consistent, high-forgery-resistance
identity:

- GitHub: @ellie, 1,604 followers, 44 public repos, account from
  Jul 2019 (6.7 years before this analysis)
- Blog: ellie.wtf (distinct domain, publicly linked from GitHub)
- Twitter: @ellie_huxtable
- Email (in Cargo.toml): `ellie@atuin.sh` — domain-consistent with the
  project
- Company: @atuinsh org, created Jan 2023 — effectively her company

The identity signal set is strong: domain ownership, multi-year tenure,
cross-platform consistency, verified commits. These are precisely the
"high to very-high forgery resistance" signals from
`design/trust-model.md`.

The `atuinsh` org (286 followers, 14 repos) provides the
institutional-affiliation signal without the governance depth of a
broader organization (compare testify's stretchr-inc with its formal
EMERITUS.md succession roster).

## Commit Activity Distribution

| Window | Cumulative commits | Annualized rate |
|--------|-------------------|-----------------|
| Oct 2020 – Oct 2021 (year 1) | ~151 | 151/yr |
| Oct 2021 – Oct 2022 (year 2) | ~315 | ~164/yr |
| Oct 2022 – Apr 2026 (years 3–5.5) | 1,751 | ~410/yr |

Activity **accelerated** over project lifetime — the opposite of the
fallow-then-revived pattern that characterized rejected dependencies
(cf. mousetrap). There are no concerning gaps in the visible window
and the nature of recent commits is substantive (features, refactors,
fixes — not cosmetic cleanup).

## Publication Integrity

- Tags follow strict semver (`v18.14.1`); prereleases explicitly marked
  (`-beta.1` suffix)
- Release CI is `cargo-dist`-generated (axodotdev/cargo-dist), which
  produces reproducible artifacts with workflow-provenance via GitHub
  Actions
- A `weekly` floating tag exists alongside semver tags — used for
  nightly-style installers, not a red flag
- crates.io publishing path: uses `GITHUB_TOKEN` in the release
  workflow; **whether `cargo publish` uses OIDC trusted publishing or
  a long-lived crates.io API token stored as a secret is not verified
  in this pass**. That's the single most useful follow-up signal for
  publication integrity on a Rust project — the Rust analog of the
  npm trusted-publishing binary-signal pattern from the axios case
  study.

## Supply Chain Hygiene

The `deny.toml` is the strongest single hygiene artifact here. Its
contents:

- **Advisories block**: pulls from rustsec advisory DB,
  `vulnerability = "deny"`, two explicit ignores (RUSTSEC-2022-0093,
  RUSTSEC-2021-0041) each with a written rationale explaining why the
  suppression is safe for this project's usage. Rationale-attached
  suppressions are a conscientious-maintenance signal, not rubber
  stamps.
- **Licenses block**: default-deny, explicit allow-list, copyleft
  warns. Restrictive license posture.
- **Bans block**: `multiple-versions = "allow"` is lenient — many
  projects flip this to `warn`. Not a red flag, but a choice worth
  noting.

Other hygiene signals: Dependabot active, 8 CI workflows including
codespell and shellcheck, strict clippy, forbid(unsafe_code) on the
server crate. Above baseline for the Rust ecosystem.

The top-level dependency set is mainstream Rust: tokio, sqlx, reqwest,
rustls, clap, ratatui, tonic, serde, rusty-paseto. No exotic or
visibly-unmaintained crates at the top level. A full `cargo-deny
check` transitive-audit pass is a follow-up — not run in this
analysis.

## Trust Model Assessment

| Signal group | Assessment |
|--------------|------------|
| Vitality | **Strong positive.** Commits daily, release yesterday, accelerating contribution rate over 5.5 years |
| Governance | **Mixed.** Org-owned on paper, but effectively one lead maintainer (ellie at 40% of commits). Real secondary contributors exist. No formal succession artifact (no EMERITUS.md analog). Bus-factor concern, not a forgery-resistance concern |
| Publication integrity | **Positive, with a gap.** Signed commits, cargo-dist releases, semver discipline. crates.io trusted-publishing status unverified |
| Hygiene | **Strong positive.** cargo-deny with reasoned advisory ignores, dependabot, clippy pedantic, forbid(unsafe_code) on server, 8 CI workflows. Notable miss: no SECURITY.md |
| Criticality | **Very high.** 29K stars; widely installed. Criticality-as-multiplier applies both ways — lots of passive scrutiny (positive) and a priority supply-chain target (risk). Per trust-model.md, monitoring threshold should be inversely proportional to criticality |
| Forgery resistance | **High.** Cross-platform identity consistency for ellie, 6+yr tenure, GPG-verified commits, domain ownership matching project + email. These are the durable signals |

## Gaps and Concerns

1. **Bus factor.** ellie dominates commits ~10:1 over the next
   contributor. The `atuinsh` org is effectively her company. There is
   no positive succession signal (like testify's EMERITUS.md). This is
   not a forgery-risk — her identity is strong — but it's a
   vitality-continuity risk.
2. **No SECURITY.md / no documented disclosure policy.** For a tool
   with this blast radius (29K+ developers' shell input), the absence
   of a documented vulnerability-reporting channel is the most obvious
   gap to raise upstream.
3. **Hosted service is a parallel trust decision.** If you use
   atuin.sh hosted sync, you're extending trust to `atuinsh` the
   company for service operation. Client-side PASETO V4 encryption
   narrows but doesn't eliminate this. Self-hosting the sync server or
   disabling sync entirely collapses that surface.
4. **crates.io publishing path unverified.** Does `cargo publish` run
   via OIDC trusted publishing, or does it use a stored API token?
   The answer materially changes the forgery resistance of a release.
5. **No transitive audit in this pass.** A full `cargo-deny check` /
   `cargo audit` would give a better picture of the 400+ transitive
   crates typical for a project of this shape.

## Risk Assessment (for a developer or org considering atuin)

| Risk | Likelihood | Impact | Mitigation |
|------|-----------|--------|-----------|
| Supply-chain compromise | Low (strong signals, high scrutiny) | Very high (credential harvester per shell command) | Pin specific version; monitor ellie's GitHub; watch for release anomalies |
| Hosted service compromise | Low-medium | Medium (client-encrypted; metadata exposure remains) | Self-host sync server, or disable sync |
| Maintainer burnout / staleness | Medium over multi-year horizon | Low for local-only use, medium for sync dependency | Pinned version continues working; follow-on migration if abandonment sustained |
| Single-account (ellie) compromise | Low | Could cut a malicious release | GPG-verified commits raise the bar; not eliminated |

## Decision

**Recorded: Analysis only — no posture recorded.**

Atuin is not a dependency of signatory, so there is no organizational
consumer relationship here to express a posture about. This file is
a worked example of the trust model applied to a non-Go, non-library
target.

**Recommended posture if a consuming organization were adopting
atuin**: Trusted-for-now.

Rationale (if recorded):

- Forgery-resistant signals are strong: cross-platform identity,
  6+-year tenure, GPG-verified commits, domain ownership. These are
  the signals that don't collapse under AI-generated content (per
  trust-model.md principle #6).
- Hygiene above ecosystem baseline: cargo-deny with reasoned advisory
  suppressions, strict clippy, forbid(unsafe_code) on server,
  standardized release pipeline.
- Criticality very high → amplifies both trust (passive scrutiny) and
  risk (attractive target). Fits the trusted-for-now tier rather than
  vetted-frozen.
- Why not vetted-frozen: no source audit performed, no transitive
  audit run, no organizational attestation recorded. Good candidate
  to promote after pinning a specific version and running cargo-deny.
- Why not rejected: none of the signals that would drive a rejection
  (fallow, unknown maintainer, unsigned commits, hygiene absence)
  are present.

## Action Items (if someone adopts it)

- Pin to `v18.14.1` explicitly (no caret/tilde ranges) rather than
  tracking `main` or `weekly`.
- Decide on sync: self-host the server, or disable sync entirely,
  unless comfortable with hosted atuin.sh retaining client-encrypted
  history metadata.
- File an issue upstream requesting a `SECURITY.md` with a disclosure
  contact.
- Run `cargo-deny check` against the pinned version for a transitive
  sanity pass.
- Verify whether `cargo publish` uses OIDC trusted publishing by
  inspecting the release workflow permissions.
- Monitor ellie's GitHub account activity and any incident reports
  as a primary compromise signal.

## Signals Surfaced That Didn't Fit Current Schema

This analysis generated several signal types that don't cleanly fit
the existing signatory signal set or the current `signals.value`
scalar-TEXT storage. They are captured in
`design/signal-storage-evolution.md` as the motivation for a proposed
schema evolution:

- Supply-chain-policy config (cargo-deny, govulncheck config)
- Advisory suppressions with written rationale (structured list,
  not scalar)
- Unsafe-code language-level posture
  (`forbid(unsafe_code)` / `go:build safety` analogs)
- Release-tooling kind (cargo-dist / goreleaser / semantic-release)
- Commit-activity shape (acceleration / deceleration / bursty)
  beyond last-commit timestamp
- Community-health composite (GitHub community profile %)
- Identity-domain consistency (email-domain ↔ project-domain ↔
  blog-domain linkage)
- Hosted-service coupling (binary + parallel SaaS as two trust
  decisions)
- Effective-maintainer concentration (org-owned-on-paper vs. one
  person actually shipping)

---

## 2026-04-14 Extended: Deep Dive on Local Clone

This section was added after inspecting a local clone of atuin at
`~/git/atuin` on the same day as the initial analysis. It corrects
and extends the GitHub-API-based conclusions above. Where a later
conclusion contradicts an earlier claim, the correction is called out
explicitly so the revision history is legible.

No additional GitHub API calls were made for this section —
everything below comes from files and `git` history on the local
clone.

### Corrections to the initial analysis

#### Commit signing is weaker than the GitHub API suggested

- **Initial claim:** "All recent commits GPG-verified." (from
  `gh api .../commits .verified: true`)
- **Local-repo conclusion:** `git log --format='%G?' -100` returns `N`
  (no signature) for 100 of 100 recent commits. The commits carry no
  per-author GPG or SSH signatures.
- **Interpretation:** GitHub's `verified: true` flag reflects
  **web-flow signing** — when a PR is merged via the GitHub UI, the
  merge commit is signed by GitHub's internal key, not by the
  committing human. This is a weaker trust signal than per-developer
  signing because it attests "this went through GitHub's web UI," not
  "the committing identity controls a signing key."
- **Revised forgery resistance:** Medium (web-flow signing only), not
  Strong (per-developer signing). A GitHub-account compromise of the
  release branch pusher is a direct path to a "verified" malicious
  commit under this model.

#### Tags are unsigned lightweight commit refs, not annotated/signed tags

- **New conclusion:** All 79 tags in the repo are lightweight commit-type
  refs. `git for-each-ref refs/tags --format='%(objecttype)'` returns
  79 occurrences of `commit`, zero of `tag`. `git tag -v v18.14.1`
  returns "cannot verify a non-tag object of type commit."
- **Impact:** For a Rust project shipping binaries and library crates
  to ~29K users, the absence of signed tags is a real
  publication-integrity gap. A signed annotated tag is the canonical
  point of cryptographic attestation for a Rust release. Its absence
  here means an attacker who could push to the repo would not need to
  forge a tag signature.

#### deny.toml is declared but not CI-enforced

- **Initial claim implied** deny.toml presence = strong hygiene
  positive.
- **Local-repo conclusion:** `.github/workflows/rust.yml` runs
  `cargo build`, `cargo nextest run`, `cargo check` (with several
  feature matrices), `cargo clippy -- -D warnings -D clippy::redundant_clone`,
  and `cargo fmt --check`. It does **not** invoke `cargo-deny check`
  or `cargo-audit`. No other workflow runs them either.
- **Revised:** The deny.toml is still a positive signal — it documents
  the policy — but enforcement depends on someone manually running
  `cargo-deny check`. The declared policy is not gated. This is a
  common pattern and still better than having no policy file at all,
  but it's weaker than the initial analysis implied.

### New conclusions: build provenance attestations exist (positive)

- `dist-workspace.toml` sets `github-attestations = true`.
- `release.yml` build-local-artifacts job has `id-token: write`
  permission (line 118) and runs `actions/attest-build-provenance@v3`
  (line 151–154) against the built artifacts.
- This produces Sigstore-signed provenance binding each release binary
  to the workflow that built it — the Rust binary analog of npm
  trusted publishing. An attacker would have to compromise the GitHub
  Actions environment to produce a malicious binary with matching
  attestation.
- **However:** this attests the **binaries**, not the **crates.io
  publishes**. I found no `cargo publish` step in any workflow. The
  v18.14.1 CHANGELOG entry "Ensure we can publish to crates (#3403)"
  confirms they do publish to crates.io, but the publish path is not
  in CI — it likely runs from the maintainer's local machine. That's
  the single largest remaining forgery-surface on the publication
  side.

### New conclusions: atuin is becoming an AI-agent runtime

This substantially changes the role classification. As of v18.14.0
(released the day before this analysis, 2026-04-13):

- A new crate **`atuin-ai`** exists, not visible in the
  `[workspace.dependencies]` table (it's picked up via
  `members = ["crates/*"]`). It contains:
  - `permissions/` — a full permission system: `shell.rs`, `rule.rs`,
    `resolver.rs`, `walker.rs`, `check.rs`, `writer.rs`, `file.rs`
  - Tree-sitter-based shell command parsing (bash + fish grammars) to
    analyze commands for permission matching
  - `eventsource-stream` for streaming LLM API responses (SSE)
  - `pulldown-cmark` for markdown rendering of LLM output
  - `stream.rs`, `context.rs`, `commands/`, `tools/`, `tui/`
- A new crate **`atuin-hex`** — "a terminal emulator for atuin" —
  `portable-pty`, `vt100`, `signal-hook`. Likely for sandboxed
  rendering of AI-tool terminal output.
- `install.sh` auto-detects and installs atuin hooks into:
  - Claude Code (detects `~/.claude` or `claude` command)
  - OpenAI Codex (detects `~/.codex` or `codex` command)
  - "pi" (detects `~/.config/pi` or `pi` command)
- Feature work in v18.14.0 explicitly: "Client-tool execution +
  permission system" (#3370), "Track coding agent shell usage"
  (#3388), "Autoinstall ai shell history hooks" (#3399), "Opt-in to
  sharing last command with ai" (#3367).

**Implication for role classification:** atuin in 2026 is
simultaneously:
- **Shell-augment / user-input capture** (unchanged from initial)
- **AI-agent runtime** (new — can execute LLM-generated commands
  under a permissions model)
- **Hosted-service coupled** (unchanged; likely includes AI API
  integrations in addition to sync)
- **Development tool** (it's installed on dev machines)

Blast radius expands accordingly: a compromised atuin can not only
log your shell input, it can issue LLM requests on your behalf,
interpret their output, and execute commands under its permission
model. The permissions subsystem becomes a security-critical module
in its own right.

### New conclusions: self-updating binaries

- `dist-workspace.toml` sets `install-updater = true`.
- The installer ships a self-update mechanism.
- Amplifier signal: every auto-update is a fresh opportunity for a
  compromised release to reach users. Mitigations present (build
  provenance attestations, cargo-dist's release pipeline); mitigation
  absent (no visible version-pin discipline in the updater UX).

### New conclusions: install.sh blast-radius details

Beyond "modifies shell rc files":

- `install.sh` writes to `~/.zshrc`, `~/.bashrc`, `~/.config/fish/config.fish`.
- On bash, it pulls `rcaloras/bash-preexec` from GitHub directly each
  install:
  `curl -LsSf https://raw.githubusercontent.com/rcaloras/bash-preexec/master/bash-preexec.sh -o ~/.bash-preexec.sh`
  Then sources it from `.bashrc`. That's a **separate supply-chain
  input** outside the cargo/crates.io path — not covered by deny.toml,
  not cryptographically verified, pulled from `master` rather than a
  pinned commit.
- On interactive runs it prompts the user to sign up for Atuin Cloud
  sync (opt-in, but it's the recommended flow — the prompt's default
  is `y`).

### New conclusions: bus factor trend is improving

Year-by-year author distribution (main branch, human commits):

| Year | Ellie | #2 human | Notes |
|------|-------|----------|-------|
| 2024 | 225 | Koichi Murase (36) | Ellie dominates (~65% of human commits) |
| 2025 | 47 | Lucas Trzesniewski (14), Michelle Tilley (11) | Slow year — Ellie's activity dropped roughly 5x |
| 2026 (Jan–Apr) | 124 | **Michelle Tilley (63)** | Resurgence with a real #2 |

In 3.5 months of 2026, Michelle Tilley has authored 63 commits — more
than any single human contributor managed in all of 2025. She is not
captured in the initial analysis's contributor snapshot because the
API returned the 15 all-time leaders, and Michelle's all-time count
(74) only just crossed that threshold.

**Revised bus factor:** ~2 as of 2026, up from ~1 through 2024. This
is the single clearest governance improvement visible in the repo.

### New conclusions: supply-chain composition is clean

From `Cargo.lock`:

- 659 total crates
- 642 from `registry+https://github.com/rust-lang/crates.io-index`
  (97.4%)
- **0** from git sources (matches `deny.toml`'s `allow-git = []`
  policy)
- Remainder: 17 path-based local workspace crates (the 15 atuin-*
  crates + the two-level nucleo workspace)

No unusual registry sources, no git-pinned deps, no vendored snapshots.
A compromise would have to land via crates.io itself.

### New conclusions: identity-graph signals from .mailmap

`.mailmap` (maintained, 804 bytes) is a rich identity-consistency
source not available through the GitHub API:

- **Ellie** has three emails across project history: `ellie@atuin.sh`
  (current canonical), `ellie@elliehuxtable.com` (personal-domain),
  `e@elm.sh`. All map to the canonical identity — three-way
  cross-domain confirmation.
- **Corporate → personal email migrations** across contributors
  (multi-year identity continuity):
  - Conrad Ludgate: truelayer.com → personal
  - Frank Hamand: coinbase.com → personal
  - Jakob Schrettenbrunner: telekom.de → personal
  - Sandro Jaeckel: sap.com → personal
  - Dennis Trautwein: posteo.de → personal
  - Cristian Le: mpsd.mpg.de (Max Planck) → personal
  - Chris Rose: offbyone GitHub → personal

Each migration is independent evidence of identity continuity across
corporate-trust boundaries. This is a **high forgery-resistance**
signal set that would be very expensive to fabricate across 6+
contributors.

### New conclusions: security policy gaps (confirmed and detailed)

- No `SECURITY.md` file in the repo (confirmed via filesystem search).
- Issue #1867 "Add security contact" exists in CHANGELOG history but
  never produced a file.
- Only **2 security-tagged entries** across 2,142 commits worth of
  CHANGELOG history:
  - #2126 "Add audit config, ignore RUSTSEC-2023-0071" (when
    `deny.toml` was introduced)
  - #1867 "Add security contact"

For a project at this scale and age, either (a) they have not had
many security incidents (plausible — narrow scope), (b) security
reports don't get tagged in commits (plausible), or (c) there's no
public incident history because there's no disclosure channel. The
absence of a disclosure channel is the repeatable concern.

### New conclusions: crypto design quality is above baseline

`crates/atuin-client/src/record/encryption.rs` shows the PASETO V4
implementation has **documented design rationale** in code comments:

- References the OWASP Cryptographic Storage Cheat Sheet
- References AWS / Azure / GCP envelope-encryption docs
- Discusses HSM integration path for enterprise customers
- Uses `rusty_paseto` (PASETO V4 Local = XChaCha20-Poly1305 + Blake2b)
  plus `rusty_paserk` for PASERK key wrapping
- Per-record random content-encryption key (CEK); master key wraps
  CEKs, not payloads
- Implicit assertions carry metadata authentication (record id, idx,
  version, tag, host)

This is above-baseline crypto engineering — principled choices with
documented rationale, no "roll your own" smell.

### New conclusions: CI action pinning tightness

- `actions/checkout@v6`, `actions/cache@v5`, `actions/upload-artifact@v6`,
  `actions/download-artifact@v7` — all major-version pinned.
- `dtolnay/rust-toolchain@master` — pinned to master (weakest).
- `taiki-e/install-action@v2` — major-version pinned.

Typical posture but not SHA-pinned. Major-version pinning accepts that
the action author's latest vN is trusted; master-pinning accepts
whatever is at HEAD at workflow run time. Neither is SHA-pinned.

### Revised signal table (additions)

Append to the signal table above:

| Signal | Value | Assessment |
|--------|-------|------------|
| Per-developer commit signing | **None** (100/100 recent commits unsigned locally; GitHub's `verified: true` = web-flow only) | Forgery-resistance: Medium, not Strong |
| Tag signing | **None** (79/79 tags are lightweight commit refs, 0 annotated/signed) | Gap |
| CI-enforced supply-chain policy | `deny.toml` configured; no CI job invokes `cargo-deny check` or `cargo-audit` | Declared, unenforced |
| Build provenance attestations | `actions/attest-build-provenance@v3` + `id-token: write` in release.yml, `github-attestations = true` | Positive — Sigstore/SLSA-style |
| crates.io publish path | No `cargo publish` step in any workflow | Publishes happen outside CI — higher forgery surface than OIDC-trusted-publishing |
| Lockfile composition | 642/659 from crates.io, 0 git-sourced, 17 path-local | Strong positive |
| CI action pinning | Major-version pinned for most; `@master` for dtolnay/rust-toolchain | Typical, not SHA-pinned |
| Self-updating binaries | `install-updater = true` | Continuous attack-surface amplifier |
| Third-party install inputs | `install.sh` pulls `rcaloras/bash-preexec` from GitHub each install | Separate supply chain, not deny.toml-covered |
| AI-agent runtime capability | `atuin-ai` crate with tree-sitter shell parsing + permissions system | New blast-radius role as of v18.14.0 |
| Self-updater + attestations together | Yes + Yes | Partial mitigation of updater surface |
| Bus factor trend (3-year window) | 2024: 1, 2025: ~0.5 (slow year), 2026: 2 (Michelle Tilley as co-lead) | Improving |
| Identity-graph depth (mailmap) | 3 emails for ellie; 6+ documented corporate→personal migrations | High forgery-resistance |
| Crypto design rationale present | Yes (PASETO V4 + PASERK + envelope encryption with OWASP-referenced comments) | Above baseline |
| Security disclosure policy | **None** (no SECURITY.md; issue #1867 unresolved) | Gap confirmed |
| CHANGELOG security mentions | 2 entries across 2,142 commits | Thin — interpretation ambiguous |

### Revised role tagging

- **Original:** Development (elevated blast radius — captures every
  shell command)
- **Revised:** Development + Shell-augment/user-input capture +
  **AI-agent runtime** + Hosted-service coupled

The AI-agent-runtime tag is the one the initial analysis missed
entirely — it was not visible in the workspace dependency table I
pulled via API, because the atuin-ai crate is picked up by the
`crates/*` glob rather than listed explicitly.

### Revised decision

**Recommendation if adopted (revised):** Trusted-for-now, with
tighter caveats than the initial assessment.

The extended conclusions pull in opposite directions:

**Weaker than the initial analysis implied:**
- Tags unsigned
- Commits web-flow-signed, not per-developer GPG
- deny.toml not CI-enforced
- crates.io publish path outside CI (likely maintainer-local)
- Self-updater adds continuous attack surface
- atuin-ai adds AI-agent-runtime blast radius
- bash-preexec pulled from GitHub master on install

**Stronger than the initial analysis implied:**
- Build provenance attestations via Sigstore
- Clean lockfile composition (0 git deps, all crates.io)
- Multi-year cross-platform identity graph in `.mailmap`
- Documented principled crypto design
- Bus factor improving (Michelle Tilley as real co-lead in 2026)

Net effect: the overall tier recommendation stays at trusted-for-now,
but the specific reasons for not moving to **vetted-frozen** are now
concrete rather than generic:

1. Tags should be signed (annotated+signed) before a vetted-frozen
   attestation is meaningful.
2. `cargo-deny check` and/or `cargo-audit` should run in CI, not just
   exist on disk.
3. crates.io publish should run under OIDC trusted publishing in CI,
   not from a maintainer's local machine.
4. The atuin-ai permissions subsystem is a new security-critical
   surface and deserves its own review before relying on it in an
   agent workflow.

### Additional action items (extending the original list)

On top of the original action items:

- **Tags**: Ask upstream to ship signed annotated tags (`git tag -s`
  instead of `git tag`). Low friction, high trust gain.
- **CI supply-chain gate**: Ask upstream to add a `cargo-deny check`
  (or at minimum `cargo-audit`) job to rust.yml.
- **crates.io publishing**: Ask upstream to move publishing into a
  GitHub Actions workflow using crates.io's OIDC trusted publishing.
  Highest single trust-surface improvement available.
- **Self-updater pinning**: If adopting atuin, consider disabling the
  updater and pinning releases via your own package manager.
- **atuin-ai permissions review**: Treat the atuin-ai permissions
  subsystem as security-critical. Independent review desirable
  before relying on it.
- **bash-preexec dependency (bash users only)**: Separately vet
  `rcaloras/bash-preexec` or vendor it locally. It's a third-party
  input that landed via install-time curl rather than crates.io.

### Additional signals surfaced that didn't fit — to add to signal-storage-evolution.md

The deep dive surfaced signal distinctions not captured in the
initial set:

- `per_developer_commit_signing_ratio` vs. `web_flow_signing_ratio` —
  these are different trust signals often conflated by GitHub's
  single `verified` flag. Separate signal types needed.
- `tag_signing_status` — annotated-and-signed vs. annotated-unsigned
  vs. lightweight. Three-valued, not boolean.
- `ci_supply_chain_gate` — whether a declared supply-chain policy
  (deny.toml, audit config) is actually invoked in CI.
- `build_provenance_attestation` — Sigstore/SLSA-style artifact
  attestations, distinct from both tag signing and trusted publishing.
- `identity_graph_depth` — `.mailmap`-derived count of confirmed
  identity mappings across a project, with corporate→personal
  email migrations as a sub-signal.
- `self_updater_present` — amplifier signal (continuous attack
  surface).
- `third_party_install_inputs` — external scripts/binaries pulled
  during install beyond the package manager (e.g., bash-preexec).
- `ai_agent_runtime_capability` — the project can execute
  LLM-generated commands under a permissions model.
- `registry_publish_origin` — OIDC-in-CI vs. long-lived-token-in-CI
  vs. local-maintainer-machine.
- `ci_action_pin_tightness` — SHA-pinned / major-version-pinned /
  master-pinned, per action.

These are now listed in the revised
`design/signal-storage-evolution.md` "New signal types to register"
table.

---

## 2026-04-14 Extended (2): Security-focused Code Review

This section integrates conclusions from an **external security review**
of the same commit, preserved verbatim in
[atuin-security-review-external.md](atuin-security-review-external.md).
That review was performed by a separate Claude Opus 4.6 agent running
under a security-focused system prompt, distinct from signatory's
trust-model skill. Its framing was "what does this code actually do,
and what can go wrong?" — complementary to our provenance-and-signal
framing.

The raw review is the primary source. This section is signatory's
integration: what agreed, what the security reviewer caught that our
analysis missed, and what new signal types the review surfaces.

### What both analyses agreed on

- atuin decomposes into distinct subsystems with different risk
  profiles (shell capture, E2E sync, local daemon, AI subsystem).
- `install.sh` does too much in one curl-pipe — modifies shell RCs,
  AI-agent configs, pulls third-party `bash-preexec` from GitHub
  master.
- Crypto is principled — PASETO V4 envelope encryption, master key
  never leaves client, server cannot decrypt.
- `atuin-ai` is where the new risk concentrates.
- Sync server is fully self-hostable; the AI subsystem is not.

The security reviewer added a clarification we missed: **`atuin hook
install` for Claude Code / Codex / pi is benign** — those hooks only
record commands into the local atuin database, they do not phone
home. Our earlier framing ("atuin is becoming an AI-agent runtime")
was directionally right but overstated the telemetry implication of
the agent hooks specifically. The agent-runtime risk lives in
`atuin-ai`, not in the agent hooks.

### What the security review caught that our analysis missed

These are code-path-specific conclusions discoverable only by reading
the source. They represent a real gap in what provenance-model
analysis alone can surface.

#### A. Unbypassable phone-home endpoints (most important)

Two endpoints are hardcoded and fire regardless of self-hosting
configuration:

- **`update_check`** is hardcoded to `https://api.atuin.sh` in
  `crates/atuin-client/src/api_client.rs:143-161` (cited by the
  security reviewer). Self-hosters still ping atuin.sh unless they
  set `update_check = false`.
- **`ensure_hub_session`** is called on login even when sync is
  self-hosted — see
  `crates/atuin/src/command/client/account/login.rs:118` and PR #3301
  ("Call ensure_hub_session even if primary sync endpoint is
  self-hosted"). This is **deliberate architectural intent**, not an
  oversight — the maintainers merged it with that description.

This is stronger and more specific than our "hosted-service coupled"
framing. It's not "there's an available hosted service" — it's
"these endpoints are hardcoded and documented as intentional even
for self-hosters." That's a distinct, worse, signal.

#### B. Windows daemon: unauthenticated TCP on localhost

`crates/atuin-daemon/src/server.rs` binds `127.0.0.1:8889` with **no
authentication** on Windows. Any process on the machine can call the
gRPC services including history add/end, search, and **shutdown**.
On multi-user Windows hosts this is a real local-privilege-boundary
issue. On Unix the daemon uses a UDS whose permissions inherit from
umask (not explicitly chmod'd).

#### C. Default-on risky features

- `atuin setup` defaults `ai.enabled = Y` and `daemon.enabled = Y`.
- `ai.capabilities.enable_history_search` defaults to **true** —
  the server can request the full history search tool. One "Allow"
  away from a natural-language-queryable copy of command history
  leaving the machine.
- `send_cwd` and `send_last_command` default to false (positive).

Our analysis noted the setup flow but didn't catch that the defaults
bias toward capability rather than least-privilege.

#### D. Encryption key file permission hygiene

`~/.local/share/atuin/key` inherits umask on creation — **not
explicitly hardened to `0o600`**. Only `meta.rs:69-70` sets `0o600`
on the meta DB. For a key that unlocks every encrypted record on
every synced device, explicit permission hardening is what you'd
expect.

#### E. `secrets_filter` is best-effort

The regex set in `crates/atuin-client/src/secrets.rs` catches AWS
keys, GitHub PATs, Slack / Stripe / Netlify / npm / Pulumi tokens.
It **misses**:
- `DATABASE_URL=postgres://user:pw@...`
- Generic bearer tokens
- `-p <password>` CLI flags
- Anything behind an unrecognized env-var name

Our analysis didn't examine this surface. "Treat it as best-effort,
not a guarantee" is the correct framing — a user should assume some
secrets *will* end up encrypted-but-synced.

#### F. Remote-instructed tool surface is enumerable

From `crates/atuin-ai/src/tools/mod.rs:321-332`:
- `read_file` — any file on the user's system
- `create_file` — enum variant exists; execution currently NYI (line 379)
- `execute_shell_command` — fully implemented via
  `execute_shell_command_streaming` at line 664+
- `atuin_history` — search the encrypted history

The permission model (`.atuin/permissions.ai.toml`, walked cwd→root,
priorities `ask > deny > allow`, default `ask`) is decent but the
capability gating at `stream.rs:292-311` is client-enforced — the
client rejects server-advertised tool calls it doesn't recognize.
That's a **positive** signal worth noting: the client doesn't blindly
trust the server's tool advertisement.

Concrete exfiltration vector named in the review: a hostile Hub
prompt can request `read_file` on `~/.ssh/`, `~/.aws/credentials`,
or `~/.local/share/atuin/key`. The permission prompt is the only
defense, and prompt fatigue is a known failure mode.

#### G. Shell globs are not a sandbox

Named exploit: `Shell(git *)` allow-rule permits
`git config --global alias.x='!bash -c "curl ... | bash"'; git x`.
The allow-list composes with git's own extension mechanisms into
arbitrary-command execution. This is a pattern we should generalize:
**any "allowlist X's subcommands" rule is only as tight as X's own
plugin/extension surface.** git, kubectl, brew, npm, cargo all have
this property.

#### H. External plugin model

`atuin foo` spawns `atuin-foo` from PATH (`crates/atuin/src/command/external.rs`).
git-style. A typo → execution of whatever binary sits at `atuin-<typo>`
in PATH. Standard pattern but worth flagging.

#### I. Predictable temp-file path

`crates/atuin/src/command/client/hook.rs:187-189` uses
`std::env::temp_dir().join(format!("atuin-hook-{tool_use_id}"))`. On
Linux with shared `/tmp` and a predictable `tool_use_id`, a local
attacker could race the file. Low severity; macOS `temp_dir()` is
user-private.

### Revised role tagging (again)

- **From extended §1:** Development + Shell-augment + AI-agent
  runtime + Hosted-service coupled
- **Correction:** the "AI-agent runtime" role was accurate, but the
  hook-install behavior (Claude Code / Codex / pi) is a separate
  concern from the agent-runtime concern. The hooks just log into
  the local atuin DB. Agent-runtime risk lives in the
  `hub.atuin.sh`-backed chat tool, not in the IDE-agent hooks.

The tag set stands but the *internal composition* of the
AI-agent-runtime role is: hub.atuin.sh chat + client-side tools
(read_file / execute_shell_command / atuin_history) + permissions
model. That's the audit target, not the agent hooks.

### Revised decision (again)

Still **Trusted-for-now** if adopted, with the caveats from the
security review added to the action items. The security-review
conclusions don't change the tier but they do change what "adopted
carefully" looks like.

### Revised action items (merged)

Combining the original list, the extended deep-dive list, and the
security-review conclusions. **New items are marked ★.**

**Before installing / at install time:**
- Do **not** run `curl … | sh`. Install the binary manually (brew /
  apt / cargo / release download). ★
- Skip `atuin setup`, or answer `n` to the AI and daemon prompts
  (both default to Y). ★
- Pin to `v18.14.1` explicitly.

**Configuration hardening:**
- Set `update_check = false` to stop the `api.atuin.sh` phone-home. ★
- If not using Atuin AI, set `ai.enabled = false`. ★
- If using Atuin AI, write a
  `~/.config/atuin/permissions.ai.toml` with explicit `deny` rules
  for `~/.ssh/**`, `~/.aws/**`, `~/.atuin/**`,
  `~/.local/share/atuin/**`, `**/.env`, `**/credentials*`. Keep
  `Shell` scoped narrowly, and remember that shell globs compose
  with the target tool's extension mechanisms (git aliases, etc.). ★
- `chmod 0600 ~/.local/share/atuin/key` after install. ★
- Self-host the sync server, or disable sync entirely. Even with
  self-hosted sync, login triggers an outbound to
  `hub.atuin.sh` via `ensure_hub_session` — documented as
  intentional per PR #3301. ★
- Consider disabling the self-updater and pinning releases via your
  package manager.
- On multi-user Windows hosts, avoid running the atuin daemon (it
  binds unauthenticated `127.0.0.1:8889`). ★
- Assume `secrets_filter` misses some secrets — don't rely on it
  for generic bearer tokens, `DATABASE_URL`, or custom env-var
  names. ★

**Ongoing:**
- Run `cargo-deny check` against the pinned version.
- Monitor ellie's GitHub account activity as a primary compromise
  signal.
- Re-audit the atuin-ai permissions subsystem on each minor release
  — it's new and moving fast. ★

**Upstream asks (file issues):**
- Ship signed annotated tags (`git tag -s`).
- Add a `cargo-deny check` CI job.
- Move crates.io publishing into GitHub Actions with OIDC trusted
  publishing.
- Add a `SECURITY.md` with a disclosure contact.
- Make `update_check` honor `sync_address` instead of hardcoding
  `api.atuin.sh`. ★
- Explicitly chmod `~/.local/share/atuin/key` to `0o600` on
  creation. ★
- Windows daemon: add an auth mechanism (token in env var + TLS,
  or a named pipe with ACL). ★

### Signals surfaced that didn't fit — from the security review

Ten new signal types, now proposed for the registry in
`design/signal-storage-evolution.md`:

- `unbypassable_hosted_callback` — hardcoded phone-home endpoints
  that fire regardless of user config
- `documented_unbypassable_callbacks` — such callbacks documented in
  merged PRs as intentional (worse than accidental)
- `default_on_risky_features` — risky features that default to Y in
  setup flows
- `secret_file_permission_hygiene` — explicit `chmod 0600` on
  sensitive files vs. umask-inherit
- `local_ipc_auth_mechanism` — UDS permissions / named-pipe ACL /
  TCP-with-auth / TCP-unauthenticated
- `remote_tool_call_surface` — for AI-integrated projects: what
  tools exist, permission model, default stance
- `capability_allowlist_enforcement` — client-side rejection of
  server-advertised capabilities it doesn't know (positive when
  present)
- `data_minimization_policy` — redaction coverage, enumerable via
  regex list
- `plugin_discovery_by_path` — `cmd-foo`-from-PATH extension model
- `temp_file_predictability` — predictable-by-attacker vs.
  randomized temp file paths

### Note on analysis methodology (as of extended pass 2)

Three passes on the same target in one session surfaced distinctly
different signal classes:

| Pass | Grounding | Reasoning | Output |
|------|-----------|-----------|--------|
| API-metadata (signatory skill, pass 1) | GitHub API, workflow YAML | Fixed trust-signal framework | Signal table, role, posture recommendation |
| Local-clone deep dive (signatory skill, pass 2) | Filesystem + git history | Same framework, extended data | Corrections + new signal types |
| Security code review (external agent) | Crate source code | Generative-skeptical threat modeling | Code-path-cited conclusions + defensive actions |

Each pass produced signals the others missed. The
`design/signal-storage-evolution.md` note on collector design —
*when upstream sources collapse multiple signals, de-collapse them* —
generalizes: each analysis perspective surfaces signals the others
systematically miss. This has direct implications for the MCP
architecture; see
[`design/mcp-dual-analyst-architecture.md`](../mcp-dual-analyst-architecture.md)
for the formal proposal (two analyst roles, cheap deterministic
collectors, optional synthesist, caller-chosen depth).

A fourth pass followed (below). It tested whether the dual-analyst
handoff format — the prose `/tmp/atuin-provenance-followup.md` —
could carry structured questions between agents, and whether the
security analyst could materially correct itself when given specific
code-verification prompts. It could, it did, and the result
substantially changed one of our prior assessments.

---

## 2026-04-14 Extended (3): Security Agent Round 2

Source: external security agent's response to signatory's
follow-up prompt, preserved verbatim in
[`atuin-security-review-external-round2.md`](atuin-security-review-external-round2.md).
Round 1 by the same agent is in
[`atuin-security-review-external.md`](atuin-security-review-external.md).

This round produced a material self-correction on the atuin-ai threat
assessment, resolved the minor mysteries left from extended pass 2,
and added one new medium-severity conclusion. It also produced two
methodology outputs (a 60-pattern grep catalog and a "checked and
chose not to flag" list) that directly inform signatory's Layer 1
collector design per
`design/mcp-dual-analyst-architecture.md`.

### §7 Material self-correction: AI capability gating downgrades threat

**Round 1 claimed:** atuin-ai is a remote LLM that can "instruct
your CLI to run shell commands, read files, write files, or search
your history, defended primarily by a permission prompt."

**Round 2 correction:** that characterization was only accurate for
`atuin_history`. For `execute_shell_command`, `read_file`, and
`create_file`, there is a **second line of defense ahead of the
permission prompt**: a client-hardcoded capability allowlist that,
in any normal configuration, does not advertise those capabilities.

Layer-by-layer trace from round 2 (file:line citations from the
review):

1. **Tool-call parsing** (`tools/mod.rs:320-333`): `ClientToolCall::try_from`
   accepts a hard-coded 4-name enum by `match`. Anything else returns
   `Err("Unknown tool call")`. This is a compile-time bound on the
   tool namespace.
2. **Capability gating** (`stream.rs:289-311`): tool calls whose
   required capability wasn't advertised never reach permission
   check, never reach execution; server gets a canned "capability not
   advertised" error.
3. **Capability source** (`stream.rs:62-87`, `settings.rs:680-684`):
   `AiCapabilities` struct has exactly one field —
   `enable_history_search`. No config surface exists for
   shell/read/write. The only path to widen capability is the
   undocumented env var `ATUIN_AI__ADDITIONAL_CAPS`.

**Effect on our assessment:**

- AI-subsystem severity revised from medium to **low-medium in a
  default configuration**.
- The realistic default-install threat is "hostile Hub requests
  `atuin_history` under permission prompts" — meaningfully narrower
  than "hostile Hub can request arbitrary shell execution under
  permission prompts."
- The architecture will age well *provided* the default-true pattern
  on `enable_history_search` isn't copied for shell/write caps in
  future.
- Practical recommendation changes: setting
  `ai.capabilities.enable_history_search = false` is a cleaner
  hardening than writing deny rules, because it closes the entire
  reachable tool surface rather than rule-by-rule.

**New trust-model observation:** self-correction across analysis
rounds is itself a trust signal about the analyst. An analyst that
revises its own prior assessment in response to deeper source
reading is producing higher-quality output than one that defends
its prior position. Worth encoding as analyst metadata — see
`design/mcp-dual-analyst-architecture.md` for the schema
implications.

### §8 New conclusion: sync protocol lacks monotonicity check

**Medium severity.** Not surfaced in any prior pass.

The data model (`atuin-common/src/record.rs:45-74`) uses
`(host, tag, idx)` with `idx: u64` monotonic per `(host, tag)`.
PASETO V4 implicit assertions bind `id`, `idx`, `tag`, `host`,
`version` into each record's AEAD
(`atuin-client/src/record/encryption.rs:170-196`), so per-record
content cannot be tampered by the server.

But the sync loop
(`atuin-client/src/record/sync.rs:218-273`) increments
`progress += page.len()` — counting *records received*, not *idx
values*. A compromised server returning idx `[0,1,2]` then
`[7,8,9]` (gap at 3–6) brings `progress` to 6 and makes the client
consider the range complete. On the next sync, `status()` returns
local-max-idx (9), so the client never re-fetches the gap even if
the server later starts serving records 3–6.

Net: **a compromised sync server can silently censor history
entries** without detection. PASETO prevents fabrication and
tampering; it doesn't prevent censorship. A Merkle chain or hash
pointer per record would close this; it doesn't exist today.

The README claim "your secrets are safe" is accurate about
confidentiality but doesn't cover integrity/availability. Worth
surfacing as a distinct signal:

- `sync_confidentiality_protection` — atuin: **yes** (PASETO v4)
- `sync_integrity_protection` — atuin: **partial** (per-record
  integrity via implicit assertions, no sequence integrity)
- `sync_availability_protection` — atuin: **no** (server can censor)

This splits what the existing model conflates into a single "E2E
encrypted sync" signal.

### Michelle Tilley trajectory: confirmed clean, inverted trust signal

Full `git log --author="Michelle Tilley" --stat` scan. Her commits
are strictly in-lane: atuin-ai + one cross-cutting `atuin config
set/get` feature + mechanical release-prep + docs + two small fixes.
**Zero touches** to `atuin-client/src/encryption.rs`,
`record/encryption.rs`, `auth.rs`, `api_client.rs`, `sync.rs`,
`record/sync.rs`, `atuin-server/**`, or any database migration.

The security reviewer's observation worth preserving verbatim:

> The capability gating that materially downgrades my threat
> assessment in §7 is her work. From a trust-model standpoint,
> that's the opposite of what you'd expect an adversarial actor
> to contribute.

This is a **positive trajectory** signal for the new-co-lead trust
concern. Our extended pass 1 flagged her as "scope worth watching";
round 2 confirmed scope is appropriate and her owned code is
actively hardening the atuin-ai surface rather than weakening it.

Revised recommendation: no particular action needed on Michelle as
a trust concern. The ramp-up shape was textbook "new contributor
earns ownership of a subsystem, ships defense-in-depth there."

### Minor resolutions

| Item | Resolved to |
|------|-------------|
| `pi` | `@mariozechner/pi-coding-agent`, open-source coding agent. Hook writes a TypeScript extension at `~/.pi/agent/extensions/atuin.ts`. Bundled extension, fully local, doesn't phone home. Most invasive of the three hook integrations (file vs. JSON config) but benign. |
| `atuin-hex` | Pty-based terminal emulator for the search popup, *not* an AI-tool sandbox. Feature-gated off by default (`hex = ["atuin-hex"]` in `crates/atuin/Cargo.toml:41`). **Not on the AI execution path** — `execute_shell_command_streaming` spawns its own child. My hypothesis that atuin-hex was an AI-tool sandbox was wrong. |
| `include_str!` scan | 11 usages, all shell init scripts, help text, and TOML examples. No credentials, no compiled-in endpoints. Endpoint defaults (`api.atuin.sh`, `hub.atuin.sh`) are string constants in `settings.rs`, not embedded files. Clean. |
| `create_file` NYI behavior | Hard error with explicit message (`tools/mod.rs:374-381`). Not a panic, not a silent skip, no fallthrough to accidental write. Informational severity only. |
| Daemon peer-credential check | **Absent** on Unix. No `SO_PEERCRED` / `getpeereid` in any tonic interceptor. Socket inherits umask; defense-in-depth reduces to filesystem perms. Cheap upstream fix: chmod 0600 + peer-UID check. |
| Hub auth request payload | Nearly nothing. `User-Agent: atuin/$VERSION`, version header, empty body (except `link_account` which sends a local CLI token for account linking). No device fingerprint, no OS/hostname, no session correlator beyond IP + TLS fingerprint + User-Agent. Unavoidable for CLI auth. |
| `Command::env_clear` for AI shell tool | Deliberately not called (`tools/mod.rs:672-681`). Shell tool inherits full environment including API tokens, SSH agent, etc. Flagged by reviewer as "design choice, not bug" — the user obviously wants `git status` to work against their private SSH agent. Worth tracking as a design invariant. |
| Credential-path denylist in permissions | **Absent.** No hardcoded deny-below-user-rules for `~/.ssh/**`, `~/.aws/credentials`, `~/.local/share/atuin/key`, `~/.gnupg/**`. Currently moot given §7 (read_file is capability-gated off). Would become a cheap high-value fix if the capability gate is lifted in future. |

### Revised severity inventory (final for atuin as of 2026-04-14)

Merging all three passes plus round 2:

| Conclusion | Severity |
|------------|----------|
| AI capability gating limits default-install attack surface | **positive** (reduces prior risk) |
| Sync server can censor records (no monotonicity check) | **medium** |
| Daemon Unix socket no peer-cred check | **medium** on shared hosts, **low** single-user |
| Windows daemon unauthenticated TCP `127.0.0.1:8889` | **medium** on multi-user Windows |
| crates.io publish from maintainer laptop | **medium** (supply chain) |
| Update-check hardcoded to `api.atuin.sh` | **low** (documented phone-home) |
| `ensure_hub_session` called for self-hosters | **low** (documented per PR #3301) |
| No hardcoded credential-path denylist | **low** (informational given capability gate) |
| Commits unsigned by omission (web-flow only) | **low** |
| Tags unsigned (lightweight, not annotated) | **low** |
| `deny.toml` unenforced in CI | **low** |
| Key file umask-inherited perms | **low** |
| Auto-install of AI agent hooks during `install.sh` | **low** (invasive UX, not exploit) |
| `create_file` NYI returns hard error | **informational** |
| Hub auth request minimal fingerprint | **informational** |
| AI shell tool inherits full env on spawn | **informational** (by design) |
| `ATUIN_AI__ADDITIONAL_CAPS` env-var escape hatch | **informational** (silent cap widening if set) |

### New signal types from this round

Added to `design/signal-storage-evolution.md`:

- `ai_capability_gating_model` — structured: `hardcoded_enum` /
  `config_driven` / `server_advertised` / `none`. atuin is
  `hardcoded_enum` (positive). The **positive** value of this
  signal was not obvious from metadata alone; it took a three-layer
  source trace.
- `sync_integrity_protection` — per-record integrity (atuin: yes)
  vs. sequence integrity (atuin: no). Splits the existing
  "E2E encrypted" signal into confidentiality /
  per-record-integrity / sequence-integrity components.
- `sync_availability_protection` — can the server drop records
  without client detection? atuin: yes, silently.
- `silent_privilege_escalation_via_env_var` — structured list of
  env vars that silently widen trust surface.
  atuin has one: `ATUIN_AI__ADDITIONAL_CAPS`.
- `env_inheritance_policy_on_spawn` — for projects that spawn
  subprocesses (AI tools, shell execution, build steps),
  whether `env_clear` is called before the spawn. atuin: no,
  by design.

### Revised action items (final)

**Before installing:**
- Don't run `curl … | sh`. Install binary manually.
- Skip `atuin setup` or answer `n` to AI and daemon prompts.
- Pin to `v18.14.1` explicitly.

**Configuration hardening (in priority order, updated per round 2):**
- Set `update_check = false` (stop `api.atuin.sh` phone-home).
- If not using Atuin AI, set `ai.enabled = false`.
- **If using Atuin AI, set
  `ai.capabilities.enable_history_search = false`** unless you
  actively want the remote LLM to query your history. This is
  **the highest-leverage single hardening** — with it disabled,
  no client-side tool is reachable from the server under default
  configuration. (Previously I recommended deny-rules as the
  primary hardening; round 2 showed capability disable is both
  simpler and more complete.)
- Confirm `ATUIN_AI__ADDITIONAL_CAPS` is not set in your shell env
  (`env | grep ATUIN_AI`).
- `chmod 0600 ~/.local/share/atuin/key` after install.
- Self-host the sync server, or disable sync entirely. Even with
  self-hosted sync, login triggers outbound to `hub.atuin.sh`.
- Consider disabling self-updater; pin releases via your package
  manager.
- On multi-user Windows, avoid the daemon (unauthenticated
  `127.0.0.1:8889`).
- If using hosted sync, be aware the server can silently drop
  history records (§8). Self-host eliminates this trust.
- Assume `secrets_filter` misses some secrets.

**Ongoing:**
- Run `cargo-deny check` against the pinned version.
- Re-audit the atuin-ai permissions subsystem on each minor
  release, *especially* if new capabilities appear in
  `AiCapabilities`.
- Monitor ellie's and michelle's GitHub account activity as
  primary compromise signals.

**Upstream asks (file issues):**
- Signed annotated tags.
- `cargo-deny check` CI job.
- OIDC trusted publishing for crates.io via GitHub Actions.
- `SECURITY.md` with disclosure contact.
- Make `update_check` honor `sync_address`.
- Explicit `chmod 0600` on key-file creation.
- Windows daemon: auth mechanism (named-pipe ACL or token).
- Merkle chain / hash pointer in sync protocol (closes §8).
- Document `ATUIN_AI__ADDITIONAL_CAPS` or remove it.
- Hardcoded credential-path denylist beneath user rules (for
  future-proofing against capability-gate lifting).

### Analyst methodology artifacts

Round 2 produced two methodology outputs that are broadly useful
beyond atuin and feed directly into Layer 1 collector design:

1. **Grep-catalog of security-relevant source patterns** — ~60
   patterns organized by category (network endpoints, local
   listeners, file/credential handling, default-on capabilities,
   unsafe-context patterns, env-var fallbacks, telemetry,
   auth/crypto smells, dependency hygiene, build provenance). Each
   is a candidate deterministic collector. See the round-2 file §B
   for the full list. This is the direct input to
   `internal/signal/source/` collector design.
2. **"Checked and chose not to flag" positive-absence signals** —
   unsafe blocks absent in client crates, panic paths absent in
   network deserialization, SQL injection shape absent in sqlx
   usage, etc. Distinct from "not examined." Gives signatory a way
   to distinguish known-good from never-examined in signal
   bookkeeping.

Both outputs are preserved in the round-2 file. Extracting them
into a collector catalog and a positive-absence signal taxonomy is
the next concrete implementation step — it moves the highest-value
portion of an Opus-grade security pass into cheap deterministic
code.

### Methodology: dual-analyst handoff worked

The round-2 response validates that a prose handoff format between
analysts can:
- Carry structured questions with priority ordering
- Elicit file:line-grounded answers
- Prompt analyst self-correction (§7 above)
- Produce methodology catalogs usable as collector specs
- Sustain cross-analyst engagement (the round-2 "Net assessment"
  picks up the provenance analyst's "social-engineering surface
  realer than false-flag" framing and confirms it from code
  trajectory)

What it does *not* yet do well (schema implications for
`design/mcp-dual-analyst-architecture.md`):
- Severity conditional on deployment context (single-user vs.
  shared host) needs structured representation
- "Positive" severity (conclusions that reduce prior risk) needs to
  be a first-class enum value
- Supersession between analysis rounds needs to be tracked
- Verdict-vs-rationale split is real (every round-2 section
  opened with a bolded one-sentence verdict)
- Methodology catalog should be a distinct output type, not a
  prose sidebar

These schema learnings are folded into the architecture doc.
