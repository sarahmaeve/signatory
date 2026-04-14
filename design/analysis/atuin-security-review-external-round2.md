# atuin — Round 2, Source-Grounded Answers (External, Preserved Verbatim)

## Provenance

- **Source:** External agent (Claude Opus 4.6 running under the same
  security-focused system prompt that produced round 1).
- **Prompt:** Response to signatory's follow-up request at
  `/tmp/atuin-provenance-followup.md` (not checked into repo —
  ephemeral handoff file).
- **Directed by:** Sarah
- **Received by signatory:** 2026-04-14
- **Preserved for the signatory analysis record.** See
  [`atuin.md`](atuin.md) §"2026-04-14 Extended (3): Security Agent
  Round 2" for signatory's integration. This file is the
  primary-source artifact; the integration is synthesis.

Notable about this round: the analyst **materially corrected their
own round-1 finding** (see §7 below — the AI subsystem's capability
gating is more robust than they originally characterized).
Self-correction across rounds is a useful signal in its own right.

---

# atuin — Round 2, source-grounded answers

Response to `atuin-provenance-followup.md`. File:line citations where
available. **Heads up at the top:** one finding in my original review
was materially wrong in the user's favor — see §7 below.

## TL;DR — the correction that matters

**My round-1 threat model for atuin-ai overstated the attack surface.** I
characterized the AI feature as a remote LLM that can "instruct your
CLI to run shell commands, read files, write files, or search your
history, defended primarily by a permission prompt."

Source reading for §7 shows that's only accurate for `atuin_history`.
For `execute_shell_command`, `read_file`, and `create_file`, there is a
second line of defense *ahead of* the permission prompt: a **client-
hardcoded capability allowlist** that, in any normal configuration,
does not advertise any of those capabilities. The server literally
cannot cause shell/read/write to execute unless:

1. The user sets an undocumented environment variable
   `ATUIN_AI__ADDITIONAL_CAPS=client_v1_shell,...`
   (`crates/atuin-ai/src/stream.rs:72-78`), **or**
2. A future release extends `AiCapabilities`
   (`crates/atuin-client/src/settings.rs:680-684`) to add flags for
   those tools.

As of 18.14.1, the only capability flag is `enable_history_search`
(default `true`). So the realistic attack surface on a default install
is: a hostile Hub can request `atuin_history` queries — and the
permission prompt is the only defense against those. Everything else
is gated off at the transport layer before the permission check runs.

This changes the severity of the AI subsystem from **medium** to
**low-medium** in a default configuration. Still not nothing, but the
defense-in-depth is real.

Revised posture recommendation: the permission-file hardening I
suggested in round 1 is still useful but lower-priority than I
implied.

---

## §4 — Hardcoded denylist for sensitive paths

**Verdict: absent. Informational severity.**

`crates/atuin-ai/src/permissions/check.rs:30-73` runs user rules only.
The walker (`permissions/walker.rs:100-121`) collects
`.atuin/permissions.ai.toml` files from cwd up to `/`, plus the global
`~/.config/atuin/permissions.ai.toml`. If no rule matches, the default
is `Ask` (`check.rs:72`), which is reasonable.

There is **no code path** that pre-emptively denies access to
`~/.ssh/**`, `~/.aws/credentials`, `~/.local/share/atuin/key`,
`~/.gnupg/**`, `/etc/shadow`, or similar credential paths. The
`RuleFilePermissions` struct (`permissions/file.rs:19-26`) has only
user-supplied `allow`, `deny`, `ask` vectors; no built-in baseline.

**Why this is less scary than it sounds** (see TL;DR): `read_file` is
unreachable without `ATUIN_AI__ADDITIONAL_CAPS=client_v1_read`. If
that gate is lifted in future, a hardcoded denylist under the user
layer would be a cheap, high-value defense. Worth filing upstream.

---

## §5 — Daemon peer-credential check on Unix

**Verdict: absent. Medium severity (low on a single-user box, higher on
shared hosts).**

`crates/atuin-daemon/src/server.rs:67-70`:

```rust
tracing::info!("listening on unix socket {socket_path:?}");
(UnixListener::bind(socket_path.clone())?, true)
```

No `set_permissions` / `chmod` call before or after the bind. No
`SO_PEERCRED` / `getpeereid` read in any tonic `Interceptor` or tower
middleware. No authentication layer on any of the three gRPC services
(`HistoryServer`, `SearchServer`, `ControlServer`).

Trust boundary: whoever can `connect(2)` the socket can call any gRPC
method, including the control service's shutdown RPC. Defence reduces
to filesystem permissions on the socket path
(`~/.local/share/atuin/atuin.sock` by default), which inherit from
umask. On a stock macOS/Linux single-user setup with umask 022, the
socket is world-readable/writable (actually depends on the parent
directory — the data dir is typically 700, which saves you).

Cheap upstream fix: `chmod 0600` after bind, plus SO_PEERCRED check in
a tonic interceptor to reject cross-UID connections. The Windows
fallback TCP listener at `127.0.0.1:8889`
(`server.rs:128-134`) is also unauthenticated — same concerns on
multi-user Windows machines, which are rare.

---

## §6 — `create_file` NYI behavior

**Verdict: hard error with explicit message. Informational severity.**

`crates/atuin-ai/src/tools/mod.rs:374-381`:

```rust
pub async fn execute(&self, db: &atuin_client::database::Sqlite) -> ToolOutcome {
    match self {
        ClientToolCall::Read(tool) => tool.execute(),
        ClientToolCall::AtuinHistory(tool) => tool.execute(db).await,
        _ => ToolOutcome::Error("Client-side tool execution not yet implemented".to_string()),
    }
}
```

The `_ =>` arm catches `ClientToolCall::Write` and any other
unimplemented variant. It returns a `ToolOutcome::Error` with the
literal string `"Client-side tool execution not yet implemented"`.

- Not a panic
- Not a silent skip (the error message propagates back to the server as
  a `tool_result` with `is_error: true`)
- No fallthrough to an accidental default write

Note: `Shell` does **not** hit this arm because
`crates/atuin-ai/src/tui/dispatch.rs:164-172` routes it separately
through `execute_shell_command_streaming`. So shell execution is fully
wired; only write tools are NYI.

Side note found during the trace: there's a minor naming inconsistency
that's worth knowing about. `tools/mod.rs:323` matches the wire name
`"create_file"` to `ClientToolCall::Write`, but the `WRITE` descriptor
(`tools/descriptor.rs:33-40`) lists canonical_names as
`["str_replace", "file_create", "file_insert"]` — no `"create_file"`.
The server-side name `"create_file"` would parse as a tool call
successfully but looking it up by name via `descriptor::by_name` would
miss it. No exploit, but suggests the write-tool story is mid-
refactor.

---

## §7 — Capability allowlist source (the big one)

**Verdict: client-hardcoded enum, one capability auto-advertised, env-var
escape hatch. Upgrades the AI safety story significantly.**

Layer by layer:

**Layer 1 — tool call parsing** (`tools/mod.rs:320-333`):
`ClientToolCall::try_from((name, input))` accepts exactly these names
by `match`: `"read_file"`, `"create_file"`, `"execute_shell_command"`,
`"atuin_history"`. Anything else returns `Err("Unknown tool call")`.
This is a hard, enum-bounded set in the compiled binary.

**Layer 2 — capability gating** (`stream.rs:289-311`):

```rust
if let Ok(tool) = ClientToolCall::try_from((name.as_str(), &input)) {
    if let Some(required_cap) = tool.descriptor().capability
        && !capabilities.iter().any(|c| c == required_cap)
    {
        // reject with an error tool_result; do NOT add to tracker;
        // do NOT trigger CheckToolCallPermission
        return;
    }
    // accept — add to tracker, enqueue permission check
    ...
}
```

The rejected tool never reaches the permission prompt. It never
reaches `execute_tool`. The server gets a canned `tool_result`
explaining the capability wasn't advertised.

**Layer 3 — where capabilities come from** (`stream.rs:62-87`):

```rust
pub(crate) fn new(
    messages: Vec<serde_json::Value>,
    session_id: Option<String>,
    capabilities: &AiCapabilities,
) -> Self {
    let mut caps = vec![];
    if capabilities.enable_history_search.unwrap_or(true) {
        caps.push("client_v1_atuin_history".to_string());
    }
    if let Ok(extra) = std::env::var("ATUIN_AI__ADDITIONAL_CAPS") {
        caps.extend(extra.split(',').map(|s| s.trim().to_string())
                         .filter(|s| !s.is_empty()));
    }
    ...
}
```

The `AiCapabilities` struct (`atuin-client/src/settings.rs:680-684`)
has exactly one field: `enable_history_search`. There is **no config
surface** for enabling shell/read/write — only the env-var escape
hatch.

**Net effect:** on a stock install, the only tool the server can
successfully invoke is `atuin_history`. Permission prompts still apply
on top of that. The wider tool surface is feature-flag-off by default
and requires an undocumented env var to unlock.

Caveat: the env var exists (presumably for atuinsh's internal testing
or future staged rollout). Anything in the user's shell environment
that sets it would silently widen the surface. Worth a `ps aux` /
`env` check if you're suspicious.

---

## §8 — Sync monotonicity check

**Verdict: absent. Medium severity — server can censor but not tamper.**

The data model (`atuin-common/src/record.rs:45-74`): records are
`(host, tag, idx)` with `idx: u64` monotonic per `(host, tag)`.
PASETO-v4 implicit assertions bind `id`, `idx`, `tag`, `host`,
`version` into every record's AEAD
(`atuin-client/src/record/encryption.rs:170-196`), so the *content* of
a given idx cannot be tampered with by the server.

Sync flow (`atuin-client/src/record/sync.rs:218-273`):

```rust
loop {
    let page = client.next_records(host, tag.clone(),
                                   local + progress, page_size).await?;
    if page.is_empty() {
        break;  // <-- no assertion that progress == expected
    }
    store.push_batch(page.iter()).await?;
    progress += page.len() as u64;  // counts records, not idx deltas
    if progress >= expected {
        break;
    }
}
```

Three observations:

1. `progress += page.len()` counts records received, not idx values. A
   compromised server returning `idx=[0,1,2]` then `idx=[7,8,9]` (gap
   at 3-6) would bring `progress` to 6 and make the client think the
   range is complete, even though records 3-6 were censored.
2. The sqlite-level `push_batch` (`sqlite_store.rs:73-94`) uses
   `insert or ignore` and has no constraint that `idx` be contiguous
   per `(host, tag)`. The gap persists in local storage.
3. On next sync, `status()` returns `max(idx) per (host, tag)`
   (`sqlite_store.rs:264`+). If local has 0,1,2,7,8,9 the local max is
   9 — so the client thinks it's fully synced and will never re-fetch
   records 3-6 even if the server later starts serving them.

**What this means:** a compromised sync server (hosted or self-hosted)
can silently censor specific history entries. It cannot fabricate or
modify entries due to the PASETO binding. The attack requires server-
side cooperation; it's not reachable from a passive network attacker.

The README claim ("your secrets are safe") is technically accurate
about confidentiality, but it does not cover availability/integrity.
Users who run `atuin sync` expecting an authoritative history mirror
should know the server can remove records. A Merkle chain or hash
pointer per record would close this; it doesn't exist today.

---

## §9 — What's in an `ensure_hub_session` request

**Verdict: nearly nothing. Informational severity.**

Three outbound calls in the Hub auth flow
(`atuin-client/src/hub.rs:266-304`):

1. **`request_code`** — `POST /auth/cli/code`. Headers:
   `User-Agent: atuin/$VERSION` and `ATUIN_HEADER_VERSION: $VERSION`.
   **Empty body.** No JSON, no query params.

2. **`verify_code`** — `POST /auth/cli/verify?code=$CODE`. Same two
   headers. **Empty body.** The code is a URL query parameter.

3. **`link_account`** (only called after successful login, if a legacy
   CLI token exists locally) — `POST /api/v0/account/link`. Headers
   plus `Authorization: Bearer $hub_token`. Body:
   `{"token": $cli_token}`. Nothing else.

No device fingerprint is generated. No session identifier beyond the
code itself is sent. No environment metadata. No hostname, OS,
distribution. `save_hub_session` writes to local SQLite only
(`atuin-client/src/meta.rs:199+`, with `0o600` set at lines 69-70).

Correlation surface is therefore: source IP, TLS client fingerprint,
`atuin/$VERSION` User-Agent, and the short-lived auth code. All of
those are unavoidable for a CLI auth flow. The Atuin Hub cannot
passively identify a self-hoster's machine or network beyond those
observables.

One thing the follow-up asked and is *not* in this flow but is worth
noting from round 1: the separate update-check ping
(`atuin-client/src/api_client.rs:143-161`) goes to `api.atuin.sh`
unconditionally (not hub), sends the same two headers, and reads the
returned index JSON. Same minimal fingerprint profile.

---

## §§1-3 — Signing, cargo-deny, crates.io publish

### §1 Commit signing

**Verdict: unsigned by omission. Low severity — supply-chain adjacent.**

- `git log --format='%G?' -100`: every commit on main returns `N`
  (no signature).
- No `signingkey` references anywhere in the repo.
- No `.pre-commit-config.yaml`, no Makefile, no justfile, no
  `scripts/` content beyond `span-table.ts`.
- No docs/CONTRIBUTING guidance on signing.

"Unsigned by omission" — the tooling to add signing isn't present and
nobody's turning it on locally. GitHub's API reporting `verified:
true` for web-flow merges is from GitHub's PR-merge web signing, not
developer-side signing.

Pragmatic note: the release workflow uses
`actions/attest-build-provenance@v3` (`.github/workflows/release.yml:152`)
for the published binaries, so the attack window on official GitHub-
release artifacts is well-covered. The weakness is git-level
verification for people who build from source — they can't
cryptographically verify a specific commit came from the claimed
author.

### §2 cargo-deny enforcement

**Verdict: declared but unenforced. Low severity.**

- `deny.toml` exists at repo root (3.3 KB, covers advisories/licenses/
  bans/sources).
- `rg "cargo-deny|cargo deny|cargo-audit"` across `.github/workflows/`,
  `.depot/workflows/`, `scripts/`, `CONTRIBUTING.md`, root manifests:
  **zero invocations.**
- No external GitHub App status check that would run it (visible to
  me, anyway — branch protection could be configured without leaving
  a trace in-repo, but there's no hint of it).
- The `.depot/workflows/` directory mirrors `.github/workflows/`
  exactly in content; I diffed the rust.yml variants by eye. No extra
  jobs hiding there.

So the deny config is unenforced. This is a common pattern ("written,
never run") and the realistic fix is a two-line `cargo install
cargo-deny && cargo deny check` step in rust.yml. Low cost.

### §3 crates.io publish

**Verdict: local-maintainer publish, not CI-gated. Medium severity.**

- No `cargo publish` in any workflow (`.github/`, `.depot/`).
- No `release.toml` (cargo-release config), no `publish.sh`, no
  `Makefile`/`justfile`.
- `dist-workspace.toml` (the cargo-dist config) publishes GitHub
  release artifacts only — cargo-dist doesn't publish to crates.io.
- PR #3403 ("ensure we can publish to crates", commit `59645a8d`) is a
  file-move: `contrib/pi/atuin.ts` → `crates/atuin/contrib/pi/atuin.ts`,
  so `include_str!("../../../contrib/pi/atuin.ts")` in hook.rs
  resolves inside the published crate tarball. The commit message
  ("Crates is so frustrating sometimes") confirms this: someone was
  manually `cargo publish`-ing and hit an include_str resolution
  error. Local publish.

So: the crates.io API token lives on at least one maintainer's
laptop — almost certainly Ellie's, since she authored #3403. That's a
long-lived credential outside the attested CI pipeline. Relative to
the GitHub-release path (which is SLSA-attested), the crates.io path
is the weaker link. Users who `cargo install atuin` are more exposed
than users who download a GitHub release.

Cheap fix is `cargo-release` + a `workflow_dispatch`-gated publish job
using OIDC-auth'd `cargo-publish-action`, but that's maintainer's
call.

---

## Context items

### pi

`pi` is `@mariozechner/pi-coding-agent`, an open-source coding agent.
Confirmed at `crates/atuin/contrib/pi/atuin.ts:12-13`:

```typescript
import type { BashOperations, ExtensionAPI } from "@mariozechner/pi-coding-agent";
import { createBashTool, createLocalBashOperations } from "@mariozechner/pi-coding-agent";
```

Hook install path is `~/.pi/agent/extensions/atuin.ts`
(`atuin/src/command/client/hook.rs:52-58`). The installer writes a
bundled TypeScript extension that wraps pi's bash tool and reports
start/end events to `atuin history`. It's the most invasive of the
three hook integrations (writes a file rather than JSON config), but
it's fully local and doesn't phone home.

One note that strengthens the project's posture: PR #3410 (the most
recent commit, by Michelle) removes `opencode` from the auto-detect
list in `install.sh`. Suggests there's a review bar for which agents
they'll auto-hook. Pi, Claude Code, Codex are the three that survived.

### atuin-hex

Not a sandbox. Not on the AI shell-execution path.

`atuin-hex` (`crates/atuin-hex/`) is a pty-based terminal emulator
that hosts atuin's search popup alongside the shell, so the popup
doesn't collide with the prompt. It exposes a Unix socket at the path
in `ATUIN_HEX_SOCKET` (`atuin-hex/src/lib.rs:252`). The atuin search
UI connects to that socket (`atuin/src/command/client/search/interactive.rs:1393-1446`)
to read the saved screen state and restore it when the popup closes.

Feature-gated: `hex = ["atuin-hex"]` in `crates/atuin/Cargo.toml:41`.
Off by default.

Relevant to the AI path? No — `execute_shell_command_streaming`
(`atuin-ai/src/tools/mod.rs:664-`) spawns its own child via
`tokio::process::Command::new(&shell_call.shell).arg("-c").arg(...)`.
It uses `vt100::Parser` for the live-preview display (same library
atuin-hex uses, hence the follow-up's hypothesis), but runs in-process
in the atuin binary, not via atuin-hex's pty multiplexer. So
atuin-hex is not a security boundary for AI shell execution and
doesn't need evaluation from that angle.

The atuin-hex socket has the same peer-cred gap as the main daemon
socket (§5). Same severity, same fix.

### Michelle Tilley's scope trajectory

`git log --author="Michelle Tilley" --stat --since=2025-01-01`, full
scan:

- `feat: Client-tool execution + permission system` (#3370, 5182+
  lines): created `crates/atuin-ai/src/permissions/*` and
  `crates/atuin-ai/src/tools/*` entirely. **This is the capability
  gating I rely on in §7.**
- `feat: Use eye-declare for more performant and flexible AI TUI`
  (#3343): TUI refactor, atuin-ai only.
- `chore: Update to eye-declare 0.2.0`, `0.3.0` (#3355, #3365): same.
- `feat: Add 'atuin config' subcommand` (#3368): adds `atuin config
  set/get` — touches `settings.rs` additively (config getter/setter
  wiring; does not touch crypto or sync).
- Release-prep commits (#3393, #3356): Cargo.toml version bumps
  workspace-wide. Mechanical.
- `docs: Add Tools & Permissions doc section` (#3402): docs only.
- `fix: Thread remote and content_length ...` (#3404): atuin-ai
  stream.rs, 54 lines.
- `fix: install script incorrectly tries to install opencode hooks`
  (#3410): 1 line in install.sh.

**No commits touch** `atuin-client/src/encryption.rs`,
`atuin-client/src/record/encryption.rs`, `atuin-client/src/auth.rs`,
`atuin-client/src/api_client.rs`, `atuin-client/src/sync.rs`,
`atuin-client/src/record/sync.rs`, `atuin-server/**`, or any
database migration.

Strict in-lane trajectory. Scope is atuin-ai + one cross-cutting
config feature + mechanical release work. No drift into crypto, sync,
or auth-critical code.

Against signatory's "new-co-lead" heuristic: this is a clean signal.
A contributor going from small TUI refactors to owning the permission
model and capability gating is a promotion, but it's a promotion
*within her area*, not across trust domains. Her permission code also
reads well (priority-ordered rules, filesystem walking with sensible
merge semantics, no weird `unsafe`, no network-touching surprises).

If anything, the noteworthy inversion is that the **capability gating
that materially downgrades my threat assessment in §7 is her work.**
From a trust-model standpoint, that's the opposite of what you'd
expect an adversarial actor to contribute.

### include_str! usage

Full list (`rg "include_str!"`):

- `atuin-client/src/settings.rs:20` → `../config.toml` (example config)
- `atuin/src/command/client/init.rs:55` → `../../shell/atuin.nu`
- `atuin/src/command/client/init/bash.rs:15` → `atuin.bash`
- `atuin/src/command/client/init/zsh.rs:15` → `atuin.zsh`
- `atuin/src/command/client/init/fish.rs:43` → `atuin.fish`
- `atuin/src/command/client/init/powershell.rs:5` → `atuin.ps1`
- `atuin/src/command/client/init/xonsh.rs:6` → `atuin.xsh`
- `atuin/src/command/client/hook.rs:13` → `contrib/pi/atuin.ts`
- `atuin/src/command/contributors.rs:1` → `CONTRIBUTORS`
- `atuin-ai/src/tui/state.rs:349` → `content/help.md`
- `atuin-server/src/settings.rs:9` → `../server.toml`

All shell init scripts, help text, and a TOML example. No credential
material, no compiled-in endpoint URLs. The endpoint defaults
(`api.atuin.sh`, `hub.atuin.sh`) are string constants
(`atuin-client/src/settings.rs:35,38`), not bundled files.

Nothing surprising here.

---

## Methodology asks (§B expanded)

The grep-grade catalog I run through on every project, tagged by
signal type so it can feed a deterministic collector. This is written
out so it's codable, not prescriptive — individual patterns fire false
positives routinely.

### Network endpoints

- Hardcoded URLs in code paths that should be configurable
  (`rg "https?://[^\"' ]+"` scoped to non-docs, non-Cargo.lock; then
  filter to where they appear inside function bodies vs. in comments/
  tests).
- URLs constructed via `format!("{}/...", host)` where `host` comes
  from a setting — these are the "self-hosters expected this" ones.
- Check `reqwest::Client::new()` / `hyper::Client::new()` instantiation
  sites; map each to a specific URL source.
- Any fetch that happens during startup/auth/config-load rather than
  on explicit user action → potential background call.

### Local listeners

- `TcpListener::bind(...)` / `UnixListener::bind(...)` sites.
- Cross-check each with an authentication layer (tower/tonic
  Interceptor, middleware, `peer_addr`/`SO_PEERCRED` calls).
- For Unix sockets: is there a `set_permissions` or explicit `chmod`
  after bind?
- For TCP: is it bound to `127.0.0.1` / `::1`, or a wildcard?
- Windows-only TCP fallback paths — often unauthenticated because the
  Unix peer-cred story doesn't translate.

### File/credential handling

- `File::create(...)` / `OpenOptions` followed by writing key/token
  material — check for `0o600` / `from_mode(0o600)` nearby.
- `env::temp_dir()` + predictable filenames (uuid? incremental?
  attacker-controlled suffix? cross-user readable `/tmp` on Linux?).
- `File::open(key_path)` before a `set_permissions` — key file may be
  created with umask-default perms and never tightened.
- Directory creation (`create_dir_all`) preceding sensitive file
  writes — parent perms matter.

### Default-on capabilities

- `.unwrap_or(true)` / `.unwrap_or_default()` on feature flags — biases
  toward enabled.
- Interactive prompts that default to `y` on empty input — `atuin
  setup` does this for AI and daemon.
- `Option<bool>` fields where `None` → enabled; check the unwrap sites.

### Dangerous patterns in unsafe contexts

- `unwrap()` / `expect()` on network/deserialization boundaries →
  DoS via malformed input.
- `panic!` reachable from an untrusted frame parser (SSE, JSON, RPC).
- `Command::new` with interpolated strings that could carry user
  input; `Command::new(shell).arg("-c").arg(user_string)` is the canonical
  shell-injection shape.
- `include_str!` paths outside the crate (packaging-level issue, not
  security, but sometimes indicates something is being embedded that
  shouldn't be).

### Env var fallbacks

- `env::var("X").unwrap_or(...)` that changes behavior → undocumented
  power. `ATUIN_AI__ADDITIONAL_CAPS` is this pattern.
- Env vars that silently modify security-sensitive defaults (disable
  TLS, bypass auth, add capabilities).
- Env vars that leak into process spawns (`Command::env_clear` is the
  opt-in safe default — its absence means children inherit
  everything).

### Telemetry and auto-network

- Update checks — scheduled or triggered, what URL, configurable host?
- "Report errors" / "send diagnostics" prompts.
- Background tasks (`tokio::spawn`, `std::thread::spawn`) that make
  network calls.
- Sync loops — how often, to where, conditional on what?

### Auth + crypto smells

- Key generation: `OsRng`? Something custom? KDF used?
- Session tokens stored: file perms, in memory, rotation?
- `bcrypt` cost, `argon2` params, any homegrown hashing.
- Nonce reuse, deterministic IV, "authenticated" claims without
  implicit-assertion / additional-data binding.

### Dependency hygiene

- Git-pinned deps (`git = "..."` in Cargo.toml) → non-crates.io
  supply chain.
- `version = "*"` → unpinned.
- Alt registries (`registry = "..."`).
- `deny.toml` / `audit.toml` present but unenforced.
- Workflow files that skip caches of sensitive artifacts.

### Build/release provenance

- Release workflow: who signs? SLSA attestations?
- `cargo publish` location — CI or local?
- Binary reproducibility claims vs. reality.
- `rust-toolchain.toml` — pinned or floating?

For atuin, the catalog surfaced: hardcoded `api.atuin.sh` (network
endpoints), Windows 127.0.0.1:8889 unauthenticated (local listeners),
unhardened key-file perms (file handling), `atuin setup` default-y
(default-on capabilities), `ATUIN_AI__ADDITIONAL_CAPS` (env-var power),
unconditional update check (auto-network), and unenforced deny.toml
(dependency hygiene).

Each of those could be written as a specific ripgrep-plus-filter
collector. The atuin-specific insight ("AI shell tool capability is
gated by a hardcoded enum") is harder to mechanize — it requires
tracing a three-layer flow through tool parsing → capability
filtering → execution dispatch. That kind of three-hop trace is where
LLM analysis adds genuine value over grep; the one-hop surface
patterns above should be collectors.

---

## §C — Things I checked and chose not to flag

Positive absence signals from this pass:

- **`cargo unsafe` / `unsafe` blocks in client crates:** spot-checked
  `rg "unsafe" crates/atuin-client/` — zero. Same for atuin-ai.
  Rust memory-safety posture is clean in the hot paths.
- **Git-pinned or alternative-registry deps:** none. `Cargo.lock`
  checksum sources are all crates.io. Confirmed by the provenance
  analyst's pass.
- **`unwrap()` on network deserialization:** the SSE parsing in
  `stream.rs:155-225` uses `if let Ok(json) = serde_json::from_str(...)`
  throughout. A malformed frame from the server produces a silent skip,
  not a panic. DoS-resistant.
- **Panic paths reachable from Hub responses:** the deserialize code
  for `CliCodeResponse` / `CliVerifyResponse` uses `resp.json::<T>().await?`
  (hub.rs:281, 302) which returns `Result`, not panic.
- **Sync password handling:** `atuin-server/src/handlers/user.rs`
  bcrypt-hashes passwords on registration; no plain-text password
  storage path. (Spot-checked, not deeply audited.)
- **History filter regex DoS:** the user-supplied `history_filter` and
  `cwd_filter` regexes in config are compiled with `RegexSet`. The
  Rust `regex` crate guarantees linear-time matching, so
  ReDoS-via-user-config is not possible. (Positive signal for the
  sandbox-the-user case where a shell user might be given attacker-
  controlled config.)
- **Server-side idx collision / record overwrite:** the sqlite
  migration `20231203124112_create-store.sql` has `PRIMARY KEY(id)` on
  record id (UUID v7). Append-only semantics enforced at the DB
  layer. A server cannot make the *client's own* local store forget
  records; only downloads are vulnerable (§8).
- **`Command::env_clear` on the AI shell tool:** deliberately not
  called (`tools/mod.rs:672-681`). The LLM-invoked shell inherits the
  full environment, including API tokens, etc. Flagged as deliberate
  design choice, not bug — the user obviously wants `git status` to
  work against their private SSH agent. But worth being aware of:
  anything in your shell env is reachable via an allowed `Shell()`
  rule.
- **Recent changelog security-relevant entries:** I spot-checked PR
  #3301 ("Call ensure_hub_session even if primary sync endpoint is
  self-hosted"), #3290 ("Clarify what data is sent when using Atuin
  AI during setup (only OS and shell)"), #3370 (the permissions
  system). Changelog discipline is good; security-affecting changes
  are consistently flagged.
- **`sqlx::query` vs. `sqlx::query_as!`:** the client uses raw
  `sqlx::query(...)` with parameter binding, not string interpolation.
  No SQL injection shape in the record store or history DB code I
  traced.

Not looked at (unknown-absence, not positive-absence):

- The atuin-server `metrics` endpoint — prometheus exporter. Didn't
  check if it leaks per-user data or if it has auth.
- The `atuin-kv`, `atuin-dotfiles`, `atuin-scripts` sync surfaces
  beyond noting they use the same PASETO record store.
- `ui/backend` (excluded from the workspace per top-level
  `Cargo.toml:9`) — this is apparently a separate desktop app; I
  didn't touch it.
- Powershell integration — I read the shell plugin for bash/zsh
  behavior around secrets, didn't audit powershell init.

---

## Severity tag summary

| Finding | Severity |
|---|---|
| §7 Capability allowlist gates AI tool surface at transport | **positive** (prior review overstated risk) |
| §8 Sync server can censor records (no monotonicity check) | **medium** |
| §5 Daemon Unix socket no peer-cred check | **medium** on shared hosts, **low** single-user |
| §5 Windows daemon unauthenticated TCP 127.0.0.1:8889 | **medium** on multi-user Windows |
| §3 crates.io publish from maintainer laptop | **medium** (supply chain) |
| §4 No hardcoded denylist for credential paths | **low** (informational given §7) |
| §1 Commits unsigned by omission | **low** |
| §2 `deny.toml` unenforced in CI | **low** |
| §9 Hub auth request minimal fingerprint | **informational** |
| §6 `create_file` NYI returns hard error | **informational** |
| Key file `~/.local/share/atuin/key` umask-inherited perms | **low** |
| AI shell tool inherits full env on spawn | **informational** (by design) |
| Auto-install of AI agent hooks during `install.sh` | **low** (invasive UX, not exploit) |
| Update-check hardcoded to `api.atuin.sh` even when self-hosted | **informational** |

---

## Recommended-action delta from round 1

The round-1 recommendation "if you enable Atuin AI, write a
`~/.config/atuin/permissions.ai.toml` with deny rules for SSH/AWS/etc."
is still worth doing, but its priority drops now that §7 is understood.
The realistic default-configuration threat is a hostile Hub querying
`atuin_history` under permission prompts — so the practical guidance
becomes:

1. **Understand what a permission prompt for `atuin_history` means.**
   Approving it lets the LLM run a history search. That search goes
   back to the server as a `tool_result`, so the answer is visible to
   whoever controls the Hub.
2. **Set `ai.capabilities.enable_history_search = false`** if you
   don't want *any* history access by the AI. Then the remaining
   reachable client-side tool surface is empty; all tool calls get
   rejected at transport.
3. The "write a permissions.ai.toml" advice is still prudent belt-and-
   suspenders but is no longer the first thing I'd do.

Everything else from round 1 stands: update-check phone-home, self-
hostability caveats (AI not self-hostable), `install.sh` silently
modifying agent configs, Windows daemon TCP, key-file perms.

---

## Net assessment

For the use case "record and encrypt shell history, optionally sync to
a server I control": the project continues to look solid.

The AI subsystem, which was the main concern in round 1, is
materially better-defended than I stated — the permission prompt is
the second line of defense, not the first. The first is a client-
hardcoded capability enum that currently only lights up history
search. That's an architecture that will age well even as more tools
get added, **provided** the default-true pattern on
`enable_history_search` isn't replicated for shell/write capabilities.

The real remaining concern sits in the supply chain: unsigned commits,
unenforced deny.toml, local-laptop crates.io publish. None of those
is a vulnerability you can exploit today, but they're the cheap-to-
fix fixtures that a project with atuin's data-sensitivity should have
in place. Between "who can write this code?" (strong — see Michelle
analysis) and "how does it get to users?" (the GitHub-release path is
attested, the `cargo install` path is not), the latter is the softer
link.

The provenance analyst's framing — "the social-engineering attack
surface is realer than the false-flag one" — matches what I see in
the code trajectory. Michelle's commits are a clean in-lane
expansion, and the code she owns (permission system, capability
gating) is the defensive layer I'd hope a reviewer-adversarial actor
would try to weaken but hasn't.
