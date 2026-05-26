# Cargo Source-AST Coverage Map

Follow-up to
[`2026-05-24-trapdoor-crypto-stealer.md`](2026-05-24-trapdoor-crypto-stealer.md)
("Cargo source AST is the dominant cargo-side gap"). Records the
bucketing pass AST.md §4 mandates before promising coverage on a
new ecosystem:

> **Bucket every threat as AST / registry / both / neither before
> promising coverage. Write the "neither" list down so it isn't
> re-litigated.**

The cargo source-AST analyzer landed across branch `cargo-rust-ast`
(commits `ad580f2` … `3ebfa57`). This document maps the cargo-side
attack shapes named in the Trapdoor write-up and prior incidents
(`rustdecimal` 2022, `xrvrv`/`postgress` 2023) to the collector that
catches them.

The mapping is the corpus drove the implementation — every entry in
§1 has a corresponding red fixture under
`internal/signal/source/rust/analyze_test.go` (per-field) plus an
integrated `clean → clean → weaponized` end-to-end fixture under
`internal/signal/source/collector_test.go`.

---

## 1. AST-side coverage (cargo source-evolution analyzer)

| Attack primitive | Counts field | Detection shape |
|---|---|---|
| Named env-credential read at build time | `EnvCredentialReads` | `env::var` / `env::var_os` fn calls **or** `env!` / `option_env!` macros, first arg matches `astfeature.IsCredentialEnvName` |
| Sensitive-path read (SSH keys, AWS creds, wallet keystores) | `SensitivePathReads` | `fs::read*` / `File::open` calls whose first arg matches `astfeature.IsSensitivePath` (which already includes the Trapdoor-added Sui/Solana/Aptos/Ethereum keystore patterns) |
| Persistence write (`authorized_keys`, shell rc, crontab, agent configs) | `SensitivePathWrites` | `fs::write` / `fs::create_dir*` / `File::create` calls whose first arg matches `astfeature.IsPersistencePath` |
| Network egress | `NetworkCallSites` | `reqwest::{get,post}` / `reqwest::blocking::{get,post}` / `ureq::{get,post}` / `surf::*` / `std::net::TcpStream::connect` direct-fn forms |
| Cloud metadata / SSRF-pivot (IMDS) | `CloudMetadataCalls` | Subset of `NetworkCallSites` where the first-arg URL matches `astfeature.IsCloudMetadataURL`; ordered first in the analyzer's case statement so IMDS calls aren't double-counted as generic egress |
| Base64 decode of obfuscated payload | `Base64DecodeCalls` | Both `base64::decode` (deprecated free-fn) **and** modern `STANDARD.decode` / `URL_SAFE.decode` engine form; flate2/brotli decompressors included as opaque-payload-decode analogues |
| XOR-with-literal-key obfuscation | `XORAssignments` | `^=` token in any non-`macro_rules!` scope. Trapdoor's `XOR_KEY = "cargo-build-helper-2026"` primitive spikes this from zero between two versions |
| Process exec of a named binary | `ExecCalls` | `std::process::Command::new` / `Command::new` / `tokio::process::Command::new` with a **resolvable** first arg. Unresolvable args (`Command::new(rustc_var)` — anyhow's build.rs idiom) deliberately don't spike; see §3 |
| Calls at cargo build time | `ImportTimeCallSites` | Any call inside `fn main()` when the file's basename is `build.rs`. Naturally non-zero on benign crates (cargo's `println!("cargo:rerun-if-changed=…")` idiom) — load-bearing only as a SPIKE metric, never as an absolute threshold |
| Cross-version anomaly | `source_evolution_anomaly` | The existing differential detector — fires when ≥ `MinSpikedFeatures` (=2) fields cross 0 → non-zero between adjacent rows. Trapdoor-shape `build.rs` weaponization crosses all 8 AST fields above in a single version |

### Fixture-driven validation

The integrated `clean → clean → weaponized` synthetic fixture in
`internal/signal/source/collector_test.go` exercises every field
above end-to-end through the live `signatory` analyze pipeline:

- `TestCollector_CargoEntity_BenignBaseline` — 3 benign versions,
  every catalog field stays zero, anomaly false
- `TestCollector_CargoWeaponizedProgression_FiresAnomaly` — clean →
  clean → Trapdoor-shape, 8 catalog fields spike at the third
  version, anomaly fires naming each one, the `(clean, clean) → 0`
  middle pair stays anomaly-free (regression guard against
  false-positives on benign evolution)

Plus dogfood validation on `pkg:cargo/anyhow` (~100 versions,
v-prefix tags, build.rs probes rustc) and `pkg:cargo/serde` (~315
versions): all catalog fields zero across every selected row,
anomaly false. Regression-clean on
`pkg:golang/alecthomas/kong` / `pkg:npm/ms` / `pkg:pypi/sigstore`.

---

## 2. Registry-side coverage (cargo registry collector)

Cargo-specific signals already present pre-Trapdoor and now
composing with the AST layer:

| Attack pattern | Signal | Source |
|---|---|---|
| First version with `build.rs` after several without | `build_script_introduced` | `internal/signal/registry/cargo/collector.go` |
| `build.rs` exists in the latest version | `build_script_present` | (same) |
| Distinct publishers between versions in window | `publish_origin_consistency` | (same) |
| Burst of recent publishes (mass-publish campaign) | `version_publish_burst` | (same) |
| Yanked release count | `yanked_release_count` | (same) |
| Crate→repo divergence (tarball ≠ git tree at tag) | `artifact_repo_divergence` | `internal/signal/artifact/` — recovers `git.sha1` from `.cargo_vcs_info.json` inside the published `.crate` tarball |
| Per-version SHA anchor for source-evolution | `version_pin_table` | `internal/signal/registry/cargo/pintable.go` — tag-match against the local clone with `cargo-tag-match` provenance label |

A future `.cargo_vcs_info.json`-upgrade pass on the pin-table
collector can land alongside this as a higher-confidence
provenance tier (publisher-stamped, gated through
`fulcio.IsGitObjectID`), parallel to npm's gitHead → attestation
upgrade. The two-tier pattern is already in place; only the second
tier is deferred.

---

## 3. Structurally cannot see (the "neither" list)

Recording these explicitly so they aren't re-litigated. Each is a
gap the cargo source-AST analyzer **cannot** close without
fundamentally different methods. None is a TODO; all are accepted
scope boundaries.

### Compile-time intrinsics whose payload is the substituted value

- **`include_str!` / `include_bytes!`** baking a secret-bearing file
  into the binary at compile time. The static analyzer sees the
  macro call but not the resolved file contents — by design the
  macro substitutes byte content from a different file, and we
  don't follow paths across files at parse time. A `build.rs` that
  writes a credential to disk and then `include_str!`s it into
  `src/lib.rs` is invisible to AST analysis on either file.
- **`cfg!` / `#[cfg(target_os = "linux")]`** payload-gating. The
  parser tokenizes attributes opaquely; we don't track which arm
  of a `cfg`-conditional compilation is active. A payload behind
  `#[cfg(target_arch = "x86_64")]` parses the same as one under a
  `cfg(any())` (never-compiled) guard.

### Proc-macro expansion

- **Procedural-macro crate body**. A proc-macro crate's source is
  Rust code that runs at the **consumer's** compile time, generating
  the actual code the consumer sees. Our analysis reads the
  proc-macro source (so the source-evolution matrix populates), but
  the expansion result — what the consumer's `rustc` actually
  compiles — is not statically derivable without invoking the
  macro. Partially mitigated by the existing `proc_macro_crate`
  registry signal (declares intent up front).

### Build-script directives that pull external artifacts

- **`println!("cargo:rustc-link-arg=…")` / `cargo:rerun-if-…` /
  `cargo:rustc-cfg=…` directives** in `build.rs`. Cargo build
  directives are strings emitted by `build.rs` at runtime; our
  static analysis sees the `println!` call but not the dynamically
  computed string content. A `build.rs` that constructs a
  rustc-link-arg pointing at a remote attacker-controlled library
  is a documented blind spot.

### Receiver-flow / variable-bound idioms

- **Method-chain catalog matches**: `reqwest::blocking::Client::new()
  .post("…").body(…).send()`. The parser records the leftmost call
  (`Client::new`, no useful URL arg); the `.post()` / `.send()`
  steps are recorded as bare callees and never match a catalog. The
  Trapdoor synthetic fixture uses direct-fn forms
  (`reqwest::blocking::get("…")`) to exercise the catalog reliably;
  builder-pattern exfil is a deliberate documented miss.
- **Variable-bound shells**: `let sh = "sh"; Command::new(&sh)`.
  The static resolver doesn't follow constant references, so an
  attacker who indirects through a variable defeats the
  `ExecCalls` arg gate. Same shape as Python's receiver-flow gap
  on `pathlib.Path(p).read_text()` paths.
- **`std::env::var` with a runtime-built name**:
  `env::var(format!("{}_TOKEN", prefix))`. The first arg can't be
  resolved to a literal, so `IsCredentialEnvName` doesn't fire.
  Documented gap; matches the same posture python/node take on
  dynamic env names.

### Authorized-keys-as-source (read) and credentials-to-disk (write)

- **Reads of persistence paths** (e.g. `fs::read("~/.ssh/authorized_keys")`
  for reconnaissance of which keys are trusted, *not* for credential
  theft). The persistence-path catalog is populated for the WRITE
  side (`SensitivePathWrites`); a read of the same path doesn't
  trigger `SensitivePathReads` because `SensitivePathPatterns` is
  the credential-target catalog, not the persistence-target catalog.
  This is by design — `authorized_keys` is the destination of a
  payload, not its source — but pathological recon would slip
  through.
- **Writes of credential bytes to a non-sensitive path**
  (e.g. `fs::write("/tmp/x", aws_key)`). The path argument is what
  catalog matching gates on; the data argument isn't inspected.

### Non-AST cargo-side risks

These are not AST-detectable patterns; signatory addresses them at
other layers, but mentioned here for completeness so the bucketing
question doesn't recur:

- **Typo-squatting** (`rustdecimal`-style). Requires cross-crate
  name-similarity comparison, orthogonal to per-version source
  analysis. Out of scope for source-evolution.
- **Dependency confusion** (malicious public crate matching an
  internal private name). Same orthogonality.
- **Maintainer rotation → first publish under a new account**.
  Partially covered by registry-side `publish_origin_consistency`;
  the source-AST layer would see this as ordinary cross-version
  diff and isn't designed to flag it.
- **Coordinated cross-ecosystem operator clustering** (Trapdoor's
  parallel npm+PyPI+crates.io publishes from the same operator).
  Captured by identity-graph / operator-URI work, not source-AST.

---

## 4. Cross-references

- [`../architecture/AST.md`](../architecture/AST.md) §4 — the
  bucketing-before-coverage discipline this document follows
- [`2026-05-24-trapdoor-crypto-stealer.md`](2026-05-24-trapdoor-crypto-stealer.md) —
  the incident corpus that drove the cargo source-AST work
- [`../../internal/signal/source/rust/`](../../internal/signal/source/rust/) —
  lexer, parser, analyzer
- [`../../internal/signal/source/astfeature/`](../../internal/signal/source/astfeature/) —
  shared catalogs the rust analyzer consumes
  (`SensitivePathPatterns` / `PersistencePathPatterns` /
  `CredentialEnvNames` / `CloudMetadataHosts`)
- [`../../internal/signal/registry/cargo/pintable.go`](../../internal/signal/registry/cargo/pintable.go) —
  the `cargo-tag-match` pin-table emitter the source-evolution
  matrix anchors to
- [`../../internal/signal/source/collector_test.go`](../../internal/signal/source/collector_test.go) —
  `TestCollector_CargoEntity_BenignBaseline` and
  `TestCollector_CargoWeaponizedProgression_FiresAnomaly`
  (the synthetic Trapdoor-shape end-to-end fixture)
