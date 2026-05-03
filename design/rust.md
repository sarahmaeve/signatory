# Rust / Cargo ecosystem provider — gap analysis and implementation plan

Status: **not started** (2026-05-02). This document is the scoping
and design artifact for the Cargo ecosystem provider.

Cross-references:
- [`npm-plan.txt`](npm-plan.txt) — the npm shipped-state doc and
  architecture template
- [`potential-pypi.md`](potential-pypi.md) — the PyPI gap analysis
  (partially shipped)
- [`agent-facing-contract.md`](agent-facing-contract.md) §"M3" —
  pluggable resolver registry
- [`v0.1-invariants.md`](v0.1-invariants.md) §Invariant 2 —
  collectors live under `internal/signal/registry/<eco>/`
- [`signal-storage-evolution.md`](signal-storage-evolution.md) —
  crates-specific signals already named (unsafe_code_posture,
  crates_io_trusted_publishing, cargo-dist release_tooling, etc.)
- [`ROADMAP.md`](ROADMAP.md) — "Rust and cargo support: needs to be
  planned and scoped" under v0.1 packaging/polish

## Purl type

The [purl spec](https://github.com/package-url/purl-spec) and the
reference Rust implementation ([docs.rs/purl](https://docs.rs/purl/latest/purl/))
both use **`cargo`** as the type identifier. Canonical URIs are:

- `pkg:cargo/<name>` (e.g., `pkg:cargo/serde`)
- `pkg:cargo/<name>@<semver>` (e.g., `pkg:cargo/serde@1.0.219`)

This is already accepted by `profile.ResolveTarget` and exercised in
tests (`target_test.go:126`, `collectors_test.go:514`,
`analyze_error_test.go:991`).

## Why Cargo is the right next ecosystem

Cargo is the smallest-effort new ecosystem provider — roughly half
the LOC of npm, a third of PyPI — because the ecosystem is
well-designed:

| Dimension | npm | PyPI | Cargo |
|-----------|-----|------|-------|
| Manifest formats | 1 (package.json) | 5+ | 1 (Cargo.toml) |
| Lockfile formats | 1 (package-lock.json) | 4+ | 1 (Cargo.lock) |
| Graph extraction | Needs lockfile walk | Needs Python environment or lockfile-specific parser | Pure TOML parse of Cargo.lock — no toolchain |
| Name normalization | None (case-sensitive) | Case + underscore + hyphen + dot (PEP 503) | Hyphen ↔ underscore only, no case folding |
| Maintainer model | First-class accounts (medium FR) | Free-form text (very low FR) | First-class accounts + teams (high FR) |
| Per-version publisher | `_npmUser` on every version | Spotty `uploaded_by` | `published_by` on every version (since 2020) |
| Download stats | First-party (`api.npmjs.org`) | Third-party only (pypistats.org) | First-party (`recent_downloads` in main response) |
| Trusted publishing | `dist.attestations` in main JSON | PEP 740 via separate `/simple/` endpoint | In the main API surface |
| TOML library needed | No (JSON) | Yes (already vendored) | Yes (already vendored — same `BurntSushi/toml`) |
| Existing test fixtures | Had to create | Had to create | `pkg:cargo/atuin` already used throughout test suite |

## Current state: what already exists

| Surface | Status | Location |
|---------|--------|----------|
| `EcosystemCrates` constant | working | `internal/ecosystem/detect.go:21` |
| Remote root-file classification (`Cargo.toml`) | working | `ecosystem/detect.go:152` + priority position 2 |
| `pkg:cargo/<name>` canonical URI acceptance | working | `profile.ResolveTarget` generic purl branch |
| `pkg:cargo/<name>@<ver>` versioned URI | working | `target_test.go:247` confirms |
| `crates_io_trusted_publishing` signal type registered | working | `signal/types.go:408-416` |
| `security-review-rust-v1.md` handoff template | working | `templates/handoffs/` |
| Tests exercising "unwired ecosystem" path | working | `collectors_test.go:514` — `pkg:cargo/serde` zero collectors, no error |
| Posture/burn commands against `pkg:cargo/*` URIs | working | URI-shape-agnostic per M1 |

**What works end-to-end today:**

- `signatory analyze pkg:cargo/atuin` — creates entity, fires
  git/github collectors if `--path`/`--clone` given, no cargo-
  specific signals
- `/analyze` skill — dispatches LLM analyst agents using
  `security-review-rust-v1.md`, ingests conclusions without Layer 1
  mechanical signals beneath them
- Posture set/get/burn commands on `pkg:cargo/*` URIs

**What doesn't work:**

- `signatory analyze https://crates.io/crates/atuin` — no
  `parseCratesURL` helper
- `signatory survey` in a Rust project — `Cargo.toml` not in
  `manifest.Detect`'s candidate list
- No mechanical registry signals for cargo entities
- No source resolver for `pkg:cargo/<name>` → github URL

## Ecosystem slug

Use `"cargo"` as canonical (matching the purl spec). Register the
resolver under both `"cargo"` and `"crates"` (mirroring Go's
`"go"` + `"golang"` dual-registration pattern) so both the purl
URI ecosystem and the `EcosystemCrates` constant route to the same
resolver.

--------------------------------------------------------------------

## Layer-by-layer implementation plan

### Layer 1: Canonical URI acceptance + crates.io URL parser

**Already done:** `pkg:cargo/<name>` and `pkg:cargo/<name>@<ver>`
parse cleanly via `resolveCanonicalURI`.

**Missing:**

- `parseCratesURL` helper in `internal/profile/target.go` (parallel
  to `parseNpmjsURL` at :250 / `parsePyPIURL` at :415).
  - Accept: `https://crates.io/crates/<name>`,
    `https://crates.io/crates/<name>/<version>`
  - Variants: `http://`, trailing slash, query strings, fragments
  - Host-anchoring against `crates.io.attacker.com` lookalikes
  - Reject non-`/crates/` paths (e.g., `/teams/`, `/policies/`)
- **Name normalization at lookup**: crates.io treats `_` and `-` as
  equivalent for lookup (e.g., `serde_json` and `serde-json` resolve
  to the same crate). The registry stores the name as the owner
  published it, and lookups normalize. Decision: store the
  registry-canonical form (owner-published). The URL parser should
  normalize the URL-path input to a lookup form (replace `_` with
  `-` for URL construction only), then let the registry response
  provide the canonical name. In practice this is simpler than
  PyPI's PEP 503 — no case folding, no dot handling, no repeated-
  separator collapse.

**Estimate:** ~80 LOC (function + tests). Simpler than npm's
scoped-package grammar or PyPI's PEP 503.

### Layer 2: Manifest detection (local filesystem)

**Missing:** `Cargo.toml` not in `internal/manifest/detect.go:38`'s
candidate list.

**Fix:** Add `{"Cargo.toml", "cargo"}` to the candidates slice.
Position: between pyproject.toml/setup.py/requirements.txt entries
and `package.json`, mirroring the priority reasoning in
`ecosystem/detect.go` (Cargo.toml is unambiguous; polyglot drag-in
is rare).

**Estimate:** ~3 LOC.

### Layer 3: Manifest parser

**New package: `internal/manifest/cargo/`**

Uses `github.com/BurntSushi/toml` (already vendored, vetted for
PyPI use in `194d007`).

#### `Cargo.toml` dep tables

| Table | Classification |
|-------|---------------|
| `[dependencies]` | Direct runtime |
| `[dev-dependencies]` | Direct dev |
| `[build-dependencies]` | Direct build (build.rs inputs) |
| `[target.'cfg(...)'.dependencies]` | Platform-conditional, still direct |
| `[target.'cfg(...)'.dev-dependencies]` | Platform-conditional dev |
| `[target.'cfg(...)'.build-dependencies]` | Platform-conditional build |

Each dep entry takes one of these forms in TOML:

```toml
serde = "1.0"                            # version-only shorthand
serde = { version = "1.0", features = ["derive"] }  # inline table
tokio = { version = "1", optional = true }           # feature-gated
local-thing = { path = "../local-thing" }            # local path
forked = { git = "https://github.com/me/forked" }   # git dep
```

#### Workspace handling

A workspace `Cargo.toml` has:

```toml
[workspace]
members = ["crate-a", "crate-b"]

[workspace.dependencies]   # shared dep declarations
serde = "1.0"
```

Members can inherit workspace deps via:

```toml
[dependencies]
serde = { workspace = true }
```

**Decision (per user):** flat union of all workspace members' deps
for v0.1. Per-member breakdown is a future survey enhancement.

Implementation: detect `[workspace]` table → glob-expand `members`
→ parse each member's `Cargo.toml` → union deps. Workspace-level
`[workspace.dependencies]` serves as the version source for
`workspace = true` references.

#### `Cargo.lock` (transitive resolution + graph)

TOML format. Structure:

```toml
[[package]]
name = "serde"
version = "1.0.219"
source = "registry+https://github.com/rust-lang/crates.io-index"
checksum = "abc123..."
dependencies = [
    "serde_derive 1.0.219",
]
```

Every `[[package]]` entry that isn't in the manifest's direct deps
is `Direct=false`. The `dependencies` array gives parent→child
edges for `manifest.Graph` — pure TOML parsing, no external
toolchain.

**Graph extraction ships in Phase A** (unlike npm/PyPI which defer
it). Cargo.lock's `dependencies` field is an explicit edge list;
parsing it into `manifest.Graph` is ~50 LOC of TOML decode + loop.

#### Canonical URI construction

- `pkg:cargo/<name>` — verbatim from the dep name as declared.
  No normalization required (manifest already uses the canonical
  form; `Cargo.lock` records the registry-canonical form).
- Non-registry deps: `path = "..."` and `git = "..."` →
  `Ecosystem="cargo-local"`, empty CanonicalURI (matches the
  `npm-local` / `go-local-replace` / `pypi-local` convention).

#### Direct vs indirect

- Direct: anything in `[dependencies]` / `[dev-dependencies]` /
  `[build-dependencies]` / `[target.*.dependencies]` (any table).
- Indirect: anything in `Cargo.lock` that isn't declared as direct
  in any workspace member's manifest.

#### ProjectInfo

- `Name`: `[package].name` (or `[workspace]` root package name)
- `ManifestPath`: absolute path to the parsed `Cargo.toml`
- `Ecosystem`: `"cargo"`
- `EcoVersion`: `rust-version` field from `[package]` if present
  (MSRV declaration); else empty.

**Estimate:** ~450-500 LOC (parser + workspace walk + lockfile
graph extraction + tests). Smaller than the PyPI manifest suite
(one format, not five) but workspace handling adds moderate
complexity.

### Layer 4: Registry client

**New package: `internal/signal/registry/cargo/client.go`**

crates.io has a clean, well-documented JSON API.

**Endpoints:**

| Method | Endpoint | Returns |
|--------|----------|---------|
| GET | `https://crates.io/api/v1/crates/{name}` | Crate metadata + all versions |
| GET | `https://crates.io/api/v1/crates/{name}/owners` | Owner list (users + teams) |
| GET | `https://crates.io/api/v1/crates/{name}/{version}` | Per-version detail |
| GET | `https://crates.io/api/v1/crates/{name}/reverse_dependencies` | Dependents count |

For the v0.1 signal set, the first endpoint provides nearly
everything (versions, downloads, repository URL, published_by per
version). The owners endpoint adds maintainer identity. Only two
HTTP calls per analyze — same as npm.

**Defenses (mirror npm/gopublish pattern):**

- HTTPS-only redirect check
- 10 MB response-body size cap (generous; even `serde` with 300+
  versions is <1 MB)
- Error-body sanitization (#93)
- Crate-name validation before URL construction:
  `^[a-zA-Z][a-zA-Z0-9_-]{0,63}$` (crates.io's published grammar)
- Context cancellation propagation
- `json.DisallowUnknownFields()` on wire models
- `User-Agent` header — **required by crates.io policy** (403 on
  missing UA). Set to `signatory/<version>
  (https://github.com/sarahmaeve/signatory)`

**Rate limiting:** crates.io rate-limits at 1 req/sec for
unauthenticated clients. For single-target analyze (2 calls) this
doesn't matter. For batch survey operations, the client should
respect 429 + `Retry-After`. A simple token-bucket (1 token/sec)
in the client is the clean solution.

**Wire models:**

```go
type CrateResponse struct {
    Crate    Crate     `json:"crate"`
    Versions []Version `json:"versions"`
}

type Crate struct {
    Name            string `json:"name"`
    Downloads       int    `json:"downloads"`
    RecentDownloads int    `json:"recent_downloads"`
    Repository      string `json:"repository"`
    Homepage        string `json:"homepage"`
    Documentation   string `json:"documentation"`
    CreatedAt       string `json:"created_at"`
    UpdatedAt       string `json:"updated_at"`
    MaxVersion      string `json:"max_version"`
    MaxStableVer    string `json:"max_stable_version"`
}

type Version struct {
    Num         string  `json:"num"`
    Yanked      bool    `json:"yanked"`
    License     string  `json:"license"`
    CrateSize   int     `json:"crate_size"`
    CreatedAt   string  `json:"created_at"`
    PublishedBy *User   `json:"published_by"` // nullable for old versions
    Checksum    string  `json:"checksum"`
    Features    map[string][]string `json:"features"`
}

type User struct {
    Login string `json:"login"`
    Name  string `json:"name"`
    URL   string `json:"url"`
}

type OwnersResponse struct {
    Users []Owner `json:"users"`
}

type Owner struct {
    Login string `json:"login"`
    Kind  string `json:"kind"` // "user" or "team"
    Name  string `json:"name"`
}
```

**Estimate:** ~350-400 LOC. Simpler than npm (no scoped-name
polymorphic JSON, no separate downloads endpoint) and simpler than
gopublish (no sum-db, no meta-tag chase).

### Layer 5: Signal collector

**New file: `internal/signal/registry/cargo/collector.go`**

Implements `signal.Collector`, scheme-filtered to `pkg:cargo/*`.

#### Signal set

crates.io's API gives us strong analogs to ALL of npm's signals,
most with equal or better forgery resistance:

| Signal | Source | Group | Forgery resistance |
|--------|--------|-------|-------------------|
| `last_publish` | `versions[0].created_at` (latest stable) | vitality | medium-declining |
| `maintainer_count` | `/owners` — count of users + teams | governance | high |
| `recent_downloads` | `crate.recent_downloads` (first-party!) | criticality | low |
| `crates_io_trusted_publishing` | OIDC audit_actions presence | publication | very_high |
| `build_script_present` | `has_build_script` in version metadata or `[package] build` in Cargo.toml | publication | high |
| `publish_origin_consistency` | Cross-version `published_by` stability | publication | very_high |
| `yanked_release_count` | Count of `versions` where `yanked=true` | publication | high |
| `owner_count` | `/owners` user count (bus-factor signal) | governance | high |

#### Longitudinal signals (B.6-equivalent)

- `publish_origin_consistency` — trivial: `published_by` is
  per-version (same strength as npm's `_npmUser`). Window bounded at
  10 versions, ordered by `created_at`, newest-first (identical
  pattern to npm Phase B.6).
- `build_script_introduced` — parallel to `postinstall_introduced`:
  did `has_build_script` change from false→true in a recent version?

#### Cargo-unique signals

| Signal | Why it matters | Group | FR |
|--------|---------------|-------|-----|
| `build_script_present` | build.rs = arbitrary code at compile time. Rust's postinstall. | publication | high |
| `proc_macro_crate` | Proc macros execute inside the compiler. Distinct attack surface from runtime. | publication | high |
| `owner_team_present` | Team ownership (vs sole-user) is a governance positive on crates.io. | governance | high |

**Signals deferred (analyst territory, not mechanical):**

- `unsafe_code_posture` — requires reading source
  (`#![forbid(unsafe_code)]` attribute). The `repofiles` collector
  already scans the clone tree; extending it with a Rust-specific
  attribute check is the right path, not a registry collector.
- `cargo_deny_config` / `supply_chain_policy_config` — presence of
  `deny.toml` in the repo root. Same: `repofiles` collector
  territory.
- `ci_supply_chain_gate` — whether CI invokes `cargo-deny` /
  `cargo-audit`. Workflow analysis, not registry.

All new signal types MUST be added to `internal/signal/types.go`'s
`signalTypeRegistry` with Group / ForgeryResistance / Description /
Caveats populated, or `signal.Make` panics at first emission.

**Estimate:** ~500-600 LOC. More signals than the PyPI collector's
current thin slice, each simpler to extract than npm's (clean API,
no endpoint fragmentation).

### Layer 6: Source resolver

**New file: `internal/ecosystem/resolver/cargo.go`**

crates.io exposes `repository` as a top-level string field in the
crate metadata response. Much cleaner than PyPI's `project_urls`
free-form map — single field, single read.

```go
type CargoResolver struct {
    client *cargo.Client
}

func (r *CargoResolver) ResolveSource(ctx context.Context, name string) (DeclaredSource, error) {
    resp, err := r.client.GetCrate(ctx, name)
    if err != nil {
        return DeclaredSource{}, err
    }
    repoURL := resp.Crate.Repository
    if repoURL == "" {
        return DeclaredSource{SelfReported: true}, nil
    }
    normalized := NormalizeDeclaredRepoURL(repoURL)
    if normalized == "" {
        return DeclaredSource{SelfReported: true}, nil
    }
    resolved, err := profile.ResolveTarget(normalized)
    if err != nil {
        return DeclaredSource{}, err
    }
    return DeclaredSource{
        URI:          resolved.CanonicalURI,
        URL:          resolved.CloneURL,
        SelfReported: true,
    }, nil
}

func init() {
    Default.Register("cargo", NewCargoResolver())
    Default.Register("crates", NewCargoResolver())
}
```

`NormalizeDeclaredRepoURL` handles the same patterns as npm/PyPI:
strip `git+` prefix, rewrite `ssh://git@github.com` →
`https://github.com`, drop `.git` suffix and `#fragment`, refuse
`git://`. Likely shares implementation with the existing
normalize helpers; worth extracting a shared utility if the third
copy makes that clear.

**Estimate:** ~100-140 LOC (resolver + normalize + tests). Thin
adapter.

### Layer 7: CLI collector dispatch

One-line addition to `cmd/signatory/collectors.go:177` switch:

```go
case "cargo", "crates":
    collectors = append(collectors, cargocollector.NewCollector().WithEntityStore(opts.EntityStore))
```

Plus the import.

**Estimate:** ~3 LOC.

### Layer 8: Handoff templates

`security-review-rust-v1.md` already exists and is the
Rust-specific analyst template (409 LOC). No additional template
work needed for security analysis.

**Verify:** `provenance-review-v1.md`'s ECOSYSTEM-switch prose
should get a Cargo section if it doesn't already have one (registry
URLs, manifest file list, etc.).

### Layer 9: Survey integration

Three changes:

1. `internal/manifest/detect.go:38` — add `{"Cargo.toml", "cargo"}`
   (Layer 2).
2. `internal/survey/survey.go` `parseManifest` switch — add
   `case "Cargo.toml": return cargo.Parse(path)`.
3. `internal/survey/survey.go` `parseGraph` switch — add
   `case "Cargo.toml": return cargo.ParseGraph(lockPath)` (or
   derive lockPath from manifest dir + "Cargo.lock").

Unlike npm and PyPI, **graph extraction is available from day one**
because Cargo.lock is self-contained TOML with explicit edge lists.
Survey's reachability buckets work out-of-the-box for Rust projects.

### Layer 10: Tests and fixtures

- `internal/manifest/cargo/testdata/` — sample `Cargo.toml`
  (single-crate, workspace, platform-conditional deps, workspace
  inheritance), sample `Cargo.lock`
- `internal/signal/registry/cargo/` — httptest-backed client and
  collector tests (mirror npm's coverage)
- `internal/ecosystem/resolver/cargo_test.go` — mock client + assert
  resolution across repository-field variations
- Integration fixture target: `atuin` (already the `pkg:cargo/*`
  fixture entity in the test suite; its registry JSON can be a
  recorded fixture)

--------------------------------------------------------------------

## Phasing

### Phase A — manifest + survey + resolver

**"Survey works for Rust projects; source resolution works for
`/analyze` and `--network-precheck`."**

Delivers:
- Layer 2: `Cargo.toml` in `manifest.Detect`
- Layer 3: `internal/manifest/cargo/` — `Cargo.toml` dep parsing +
  workspace union + `Cargo.lock` transitive resolution + graph
  extraction
- Layer 6: `internal/ecosystem/resolver/cargo.go`
- Layer 9: survey wiring + graph wiring
- Layer 10: manifest testdata + resolver tests

After this:
- `signatory survey` in a Rust project produces a full dep
  enumeration with reachability buckets
- `signatory handoff security --target pkg:cargo/X --network-precheck`
  resolves the source repo deterministically
- `/analyze` can dispatch analysts with resolved source URLs

### Phase B — registry signals

**"`signatory analyze pkg:cargo/X` produces mechanical evidence
without `--path`/`--clone`."**

Delivers:
- Layer 4: `internal/signal/registry/cargo/client.go`
- Layer 5: `internal/signal/registry/cargo/collector.go` — full
  signal set
- Layer 7: CLI dispatch wiring
- Layer 10: httptest-backed collector tests
- Publisher-entity minting (`identity:cargo/<login>`) via
  `WithEntityStore`
- New signal-type entries in `signal/types.go`

After this:
- `signatory analyze pkg:cargo/atuin` produces 8+ mechanical
  signals without needing a local clone
- The `/analyze` skill has Layer 1 signals beneath Layer 2
  analyst conclusions for cargo entities

### Phase C — URL acceptance + polish

- Layer 1: `parseCratesURL` + host-anchoring tests
- Layer 8: provenance template Cargo section
- Name normalization (hyphen ↔ underscore lookup equivalence)

--------------------------------------------------------------------

## Cargo-specific complications

Things that don't map 1:1 from the other ecosystems.

### 1. Workspace manifests

A workspace root's `Cargo.toml` has `[workspace]` instead of (or in
addition to) `[package]`. It lists members via `members = [...]`
which can contain globs. Each member has its own `Cargo.toml` with
its own deps.

**Implication:** the parser needs to detect workspace roots and
walk members. Not difficult — `BurntSushi/toml` decodes the
workspace table directly; glob expansion uses `filepath.Glob`. The
dep-union operation is a map merge (canonical-URI-keyed).

### 2. Platform-conditional deps

```toml
[target.'cfg(unix)'.dependencies]
nix = "0.29"

[target.'cfg(windows)'.dependencies]
windows-sys = "0.59"
```

These are still direct dependencies — they just activate
conditionally at build time. For trust analysis the right thing is
to include ALL of them regardless of build platform: an attacker
can compromise a platform-conditional dep and it's in the lockfile
on every platform.

**Implication:** enumerate all `[target.*.dependencies]` /
`[target.*.dev-dependencies]` / `[target.*.build-dependencies]`
tables and union into the dep set. The `cfg(...)` expression is
opaque (we don't evaluate it).

### 3. Feature-gated optional deps

```toml
[dependencies]
serde = { version = "1.0", optional = true }
```

Optional deps only activate when a feature enables them. However,
they're still IN the lockfile (resolved) and they're still a supply-
chain surface. For signatory's purposes: include them as direct deps
with a marker (could use a future `Optional bool` on `manifest.Dep`
if we want survey to distinguish). For v0.1: include them, don't
distinguish.

### 4. Build scripts (build.rs)

build.rs is the Rust analog of npm's `postinstall`: arbitrary code
that runs at build time. Unlike npm's lifecycle scripts (which are
declared in `package.json`'s `scripts` field), build scripts are
implicit when a `build.rs` file exists at the crate root OR when
`[package] build = "custom_build.rs"` names an alternate path.

The registry metadata exposes `has_build_script` per version.

**Implication:** `build_script_present` signal is straightforward
from the registry response. `build_script_introduced` (longitudinal)
tracks the transition, same as npm's `postinstall_introduced`.

### 5. Proc macros

Proc-macro crates (`[lib] proc-macro = true`) execute at compile
time inside the compiler process. They're a distinct attack surface:
a malicious proc macro can read files, hit the network, and
exfiltrate data during `cargo build` — before any of the compiled
code runs.

The registry metadata exposes this categorization. Worth a
dedicated signal (`proc_macro_crate`) because the blast radius
mental model is different from a runtime dep.

### 6. Name normalization (hyphen ↔ underscore)

crates.io normalizes lookups: `serde-json` and `serde_json` resolve
to the same crate. The stored canonical form is the one the owner
published under (`serde_json` in this case). This is MUCH simpler
than PyPI's PEP 503 (no case folding, no dot handling, no repeated-
separator collapse).

**Implication:** the canonical URI stores the registry-canonical
form. The URL parser and any user-facing input normalization should
canonicalize on lookup (replace `-` with `_` in the lookup path, or
just let the registry redirect, and take the name from the
response). In practice, the manifest already uses the canonical form
and Cargo.lock uses the canonical form; the only place normalization
matters is the URL parser (Layer 1, Phase C).

### 7. Rate limiting

crates.io enforces 1 request/second for unauthenticated clients.
For single-target analyze (2 requests) this doesn't matter. For
batch operations (e.g., surveying 200 deps), the client needs a
token-bucket or simple `time.Sleep` between calls. A 1 req/sec
rate limiter in the client is the clean solution.

--------------------------------------------------------------------

## Scope estimate

| Piece | npm LOC (reference) | Cargo estimate |
|-------|---------------------|----------------|
| Manifest parser + workspace + lockfile graph | 722 | ~500 |
| Registry client | 969 | ~380 |
| Signal collector | 1,412 | ~550 |
| Source resolver | 161 | ~140 |
| CLI dispatch + survey wiring | ~10 | ~10 |
| Signal-type registry entries | 7 entries | ~8 entries |
| URL parser (Phase C) | 65 | ~80 |
| **Total** | **~3,300** | **~1,660** |

--------------------------------------------------------------------

## Relationship to existing PyPI state

The PyPI collector (`internal/signal/registry/pypi/collector.go`)
is wired and shipping: it emits `maintainer_count` and mints
`identity:pypi/<login>` publisher entities. The infrastructure
works. What's deferred behind a forcing function is the broader
"PyPI-unique signals" cut (`trusted_publishing` PEP 740,
`pypi_stdlib_shadow`, `sdist_present`, `sdist_wheel_size_ratio`,
`yanked_release_count`). Those signals need a PyPI-specific dogfood
pain to justify the effort vs the LLM-analyst path.

Cargo's Phase A + B is independent of PyPI's expansion and should
ship first — three ecosystems with mechanical Layer 1 coverage
(npm full, Go full, Cargo full) plus PyPI's thin slice is a
stronger v0.1 story than expanding PyPI's signal breadth.

--------------------------------------------------------------------

## Security invariants (per-ecosystem pattern)

Carried forward from npm-plan.txt §"Security invariants":

- `ValidateCanonicalURI` at persistence boundary
- HTTPS-only redirect in cargo client
- Max response size cap (10 MB)
- Error sanitization: response body NEVER in error string
- Crate-name validation before URL construction
- Context cancellation propagation tested
- `json.DisallowUnknownFields()` on every registry-response struct
- `signalTypeRegistry` entry for every new signal type

## Test discipline (per-ecosystem pattern)

- Table-driven unit tests with explicit `name` fields and
  `t.Run(tc.name, …)` per case
- TDD for security-adjacent fixes: failing test before the fix
- Writer injection for any stdout output; no global `os.Stdout`
  redirection — breaks `t.Parallel()`
- Functional tests via `httptest`, never live crates.io

--------------------------------------------------------------------

## Open questions

1. **Shared `NormalizeDeclaredRepoURL` utility.** npm, PyPI, and
   Cargo all have nearly-identical URL normalization logic (strip
   `git+`, rewrite SSH → HTTPS, drop `.git`, drop fragment). This
   is the third copy. Extract a shared `resolver.NormalizeRepoURL`
   utility and delegate from all three, or keep them independent
   (minor drift is acceptable; shared utility adds coupling)?
   Lean: extract when shipping Cargo's resolver; the three copies
   are the forcing function.

2. **`has_build_script` source.** The crates.io registry API
   doesn't expose `has_build_script` as a top-level boolean on
   the wire (it's part of the index metadata, not the JSON API).
   Alternatives: (a) check Cargo.toml in the local clone via
   repofiles collector, (b) check for `build.rs` in the clone
   tree, (c) infer from the per-version download and inspect the
   `.crate` tarball. Recommendation: (b) via repofiles collector
   (already scans clone trees) is simplest and grounded.

3. **Workspace member discovery without filesystem.** For
   `signatory analyze pkg:cargo/serde` (no local clone), the
   registry metadata doesn't expose workspace members. The
   manifest parser (Layer 3) is clone-side; the registry collector
   (Layer 5) is network-side. Workspace semantics apply only to
   survey (which has the filesystem); analyze doesn't need them.
   Confirm: this is fine as-is, no cross-layer complication.

--------------------------------------------------------------------

## Next steps

1. Implementation begins with Phase A (manifest parser + survey +
   resolver). This is the highest-value slice: users get
   `signatory survey` working in Rust projects, and `/analyze`
   resolves sources deterministically.

2. Phase B (registry signals) follows once Phase A is merged and
   dogfooded. The signal set is well-defined; the implementation
   is straightforward given the clean crates.io API.

3. Phase C (URL parser + polish) is low-priority convenience work
   that can slot in whenever.
