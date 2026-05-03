# Ruby / Gems ecosystem provider — design and implementation plan

Status: **not started** (2026-05-03). This document is the scoping
and design artifact for the RubyGems ecosystem provider.

Cross-references:
- [`rust.md`](rust.md) — the Cargo plan (shipped); structural template
  for this document
- [`npm-plan.txt`](npm-plan.txt) — the npm shipped-state doc
- [`potential-pypi.md`](potential-pypi.md) — the PyPI gap analysis
- [`agent-facing-contract.md`](agent-facing-contract.md) §"M3" —
  pluggable resolver registry
- [`v0.1-invariants.md`](v0.1-invariants.md) §Invariant 2 — collectors
  live under `internal/signal/registry/<eco>/`
- [`signal-storage-evolution.md`](signal-storage-evolution.md) — future
  signals already sketched
- [`threat-landscape/2026-05-02-bufferzonecorp-campaign.md`](threat-landscape/2026-05-02-bufferzonecorp-campaign.md) —
  identifies rubygems coverage gap, mints `identity:rubygems/<login>`

## Purl type

The [purl spec](https://github.com/package-url/purl-spec) uses **`gem`**
as the type identifier for RubyGems packages. Canonical URIs are:

- `pkg:gem/<name>` (e.g., `pkg:gem/rails`)
- `pkg:gem/<name>@<version>` (e.g., `pkg:gem/rails@7.1.3`)

The ecosystem slug in signatory is `"gem"`. This is already used in
test fixtures (`analyze_error_test.go`, `collectors_test.go`) as the
canonical example of an "unwired" ecosystem — those tests will need
updating when this ships.

## Why gems next

| Dimension | Cargo (shipped) | RubyGems |
|-----------|-----------------|----------|
| Manifest formats | 1 (Cargo.toml) | 1 primary (Gemfile) + gemspec |
| Lockfile formats | 1 (Cargo.lock) | 1 (Gemfile.lock) |
| Graph extraction | TOML parse of Cargo.lock | Text parse of Gemfile.lock |
| Name normalization | `_` → `-` | Case-insensitive lookup only |
| Maintainer model | First-class accounts + teams | First-class accounts (handles) |
| Per-version publisher | `published_by` on every version | Not exposed per-version |
| Download stats | `recent_downloads` in main response | `version_downloads` in gem response |
| Build-time code execution | `build.rs` (build script) | Native extensions (`extconf.rb`) |
| Registry API | JSON, public, no auth for reads | JSON, public (owners may need key) |
| Trusted publishing | Yes (OIDC) | Yes (GitHub OIDC, launched 2023) |

Gems is the right next target because:
1. The threat landscape doc already identifies a coverage gap for
   rubygems publishers
2. The API surface is simple (3 endpoints cover everything)
3. The lockfile format is structured enough for reliable graph extraction
4. Native extensions are the direct analog of `build.rs` and
   `postinstall` — the supply-chain danger vector we track in every
   ecosystem
5. Rails and its dependency tree constitute a meaningful dogfood
   exercise for enterprise-grade projects

## RubyGems.org API surface

All endpoints are public, unauthenticated for reads, and return JSON:

| Endpoint | Returns | Rate limit |
|----------|---------|------------|
| `GET /api/v1/gems/{name}.json` | Gem metadata: name, downloads, version_downloads, version, version_created_at, created_at, authors, licenses, source_code_uri, homepage_uri, changelog_uri, bug_tracker_uri, mfa_required |  10 req/s |
| `GET /api/v1/versions/{name}.json` | All versions: number, created_at, downloads_count, prerelease, yanked, sha, platform, rubygems_mfa_required | 10 req/s |
| `GET /api/v1/gems/{name}/owners.json` | Owner list: handle, email (handle is always public; email may be redacted) | May require API key |

**Fallback for owners:** If the owners endpoint requires authentication
(returns 401), degrade to the `authors` field from the gem info
response. This is a comma-separated string of display names — lower
fidelity than handles, but enough for a `maintainer_count` signal with
a reduced forgery-resistance rating.

**Rate limiting:** 10 req/s with a courtesy `Retry-After` header on 429.
The collector should back off on 429 (same pattern as the cargo client).

## Name normalization

RubyGems has a subtler normalization story than cargo:

- **Case:** Lookups on rubygems.org are case-insensitive, but the
  canonical form is what the publisher registered. In practice, ~99.9%
  of gems use all-lowercase names. We normalize to lowercase on the
  URI (like PyPI) to prevent storage fragmentation.
- **Hyphens vs underscores:** Unlike cargo, these are **distinct** in
  RubyGems. `foo-bar` and `foo_bar` can be different gems. Do NOT
  normalize hyphens/underscores.
- **Dots:** Allowed in gem names (e.g., `ruby-lsp-rails`). Preserved
  verbatim.

```go
// NormalizeGemName applies rubygems canonical normalization: lowercase only.
// Hyphens and underscores are NOT equivalent in RubyGems (unlike cargo).
func NormalizeGemName(name string) string {
    return strings.ToLower(name)
}
```

Wire into `CanonicalPackageURI`:
```go
case "gem":
    name = NormalizeGemName(name)
```

## Gemfile / Gemfile.lock format analysis

### Gemfile (Ruby DSL)

Gemfile is a Ruby DSL, not a structured data format. Full parsing
requires a Ruby evaluator. However, the common patterns cover >95% of
real-world usage and are parseable with line scanning:

```ruby
source 'https://rubygems.org'

ruby '3.2.2'

gem 'rails', '~> 7.1'
gem 'pg', '>= 0.18', '< 2.0'
gem 'puma', '~> 6.0'

group :development, :test do
  gem 'rspec-rails', '~> 6.0'
  gem 'debug', platforms: [:mri, :mingw, :x64_mingw]
end

gem 'local_thing', path: '../local_thing'
gem 'my_engine', git: 'https://github.com/org/my_engine.git'
```

**Parseable patterns** (handle with line scanning):
- `gem 'name'` / `gem "name"`
- `gem 'name', 'constraint'` / `gem 'name', '>= x', '< y'`
- `source 'url'`
- `ruby 'version'`
- `group :name do` / `end` (for context, not filtering)
- `gem 'name', path: '...'` → classify as `gem-local`
- `gem 'name', git: '...'` → classify as `gem-local`

**Unparseable patterns** (skip gracefully):
- Conditional logic: `if RUBY_PLATFORM =~ /linux/`
- Method calls: `gem install_if(-> { ... }) { gem 'x' }`
- String interpolation in names (pathological, ignore)
- `eval_gemfile 'other/Gemfile'` (recursive include — not common)

**Strategy:** Parse Gemfile for direct dep declarations and project
metadata (ruby version, source). When Gemfile.lock exists alongside it,
use the lockfile as the authoritative source for resolved versions and
the full transitive graph.

### Gemfile.lock (structured text)

Gemfile.lock is a well-defined text format produced by Bundler:

```
GEM
  remote: https://rubygems.org/
  specs:
    actioncable (7.1.3)
      actionpack (= 7.1.3)
      nio4r (~> 2.0)
      websocket-driver (>= 0.6.1)
    actionpack (7.1.3)
      actionview (= 7.1.3)
      rack (~> 3.0)

GIT
  remote: https://github.com/org/my_engine.git
  revision: abc123def
  specs:
    my_engine (0.1.0)

PATH
  remote: ../local_thing
  specs:
    local_thing (1.0.0)

PLATFORMS
  ruby
  x86_64-linux

DEPENDENCIES
  actioncable (~> 7.1)
  my_engine!
  local_thing!
  rails (~> 7.1)

RUBY VERSION
   ruby 3.2.2p53

BUNDLED WITH
   2.4.19
```

**Parsing strategy:**

1. **Section detection:** Lines with no leading whitespace are section
   headers (`GEM`, `GIT`, `PATH`, `PLATFORMS`, `DEPENDENCIES`,
   `RUBY VERSION`, `BUNDLED WITH`).

2. **GEM section:** `remote:` gives the registry URL. `specs:` subsection
   contains resolved packages. Each spec entry is indented 4 spaces:
   `    name (version)`. Sub-dependencies are indented 6 spaces:
   `      dep_name (constraint)`.

3. **DEPENDENCIES section:** Lists direct deps, one per line, indented
   2 spaces. Names ending with `!` are local/git deps (their source
   is in the GIT/PATH sections above).

4. **Graph extraction:** The nested structure under `GEM > specs:`
   directly encodes parent→child edges. Each 4-space-indented line is
   a package; each 6-space-indented line beneath it is a dependency
   of that package.

5. **Non-registry deps:** Entries from `GIT` and `PATH` sections get
   ecosystem `"gem-local"` and no `CanonicalURI` (same pattern as
   cargo's path/git deps and npm's workspace/file deps).

**Size caps:** `maxGemfileBytes = 64 * 1024`,
`maxGemfileLockBytes = 1 * 1024 * 1024` (match cargo).

## Signals to emit

### Reusing existing signal types (no registration needed)

| Signal | Source field | Group | Derivation |
|--------|-------------|-------|------------|
| `last_publish` | `gem-registry` | Vitality | Newest non-yanked, non-prerelease version's `created_at` |
| `version_count` | `gem-registry` | Vitality | Total version count from versions endpoint |
| `recent_downloads` | `gem-registry` | Criticality | `version_downloads` from gem info (last ~90d) |
| `maintainer_count` | `gem-registry` | Governance | Owner count from owners endpoint (or authors fallback) |
| `owner_count` | `gem-registry` | Governance | Same source as maintainer_count |
| `yanked_release_count` | `gem-registry` | Publication | Count of yanked versions from versions endpoint |
| `publish_origin_consistency` | `gem-registry` | Publication | Distinct author strings across recent version window (lower fidelity than npm/cargo — no per-version publisher login) |

### New gem-specific signals (need registration in types.go)

| Signal | Group | Derivation | Rationale |
|--------|-------|------------|-----------|
| `native_extension_present` | Publication | Whether any non-`"ruby"` platform variant exists in recent versions, or gem has `extensions` in metadata | Native extensions execute arbitrary C code at `gem install` time — the rubygems analog of build.rs and postinstall |
| `native_extension_introduced` | Publication | Longitudinal: did native extension appear in the latest version where prior versions lacked it? | Same anomaly-detection pattern as `build_script_introduced` and `postinstall_introduced` |
| `mfa_required` | Governance | `rubygems_mfa_required` field on gem or version | Whether the gem's publisher enabled mandatory MFA for pushes — a governance signal unique to rubygems (Cargo/npm equivalents are account-level, not surfaced per-package) |

### Signals intentionally NOT emitted

- **`trusted_publishing`** — RubyGems supports OIDC trusted publishing
  (GitHub Actions), but the API doesn't surface whether a specific
  version was published via OIDC vs. API key. Defer until the API
  exposes this or we find a mechanistic detection path.
- **`owner_team_present`** — RubyGems doesn't have a "team" ownership
  concept like cargo. All owners are individual accounts.

## Publisher entity minting

Following the pattern from the threat landscape doc and the cargo
collector's `ensurePublisherEntities`:

- Owner handles → `identity:rubygems/<handle>` entities
- Minted via `EntityStore.EnsureEntityByCanonicalURI`
- Nil-safe: no EntityStore wired → no minting (test/offline path)

## Source resolution

The gem info response provides several URL fields for source resolution,
in priority order:

1. `source_code_uri` — most reliable, explicitly declared as source
2. `homepage_uri` — often points to GitHub; validate host before using
3. `bug_tracker_uri` — sometimes a github issues URL from which the
   repo can be derived

Resolution chain in `client.ResolveRepoURL`:
```
source_code_uri → homepage_uri → "" (no source declared)
```

Each candidate is normalized through `NormalizeDeclaredRepoURL` (shared
utility already used by cargo/npm) which strips `.git` suffixes,
trailing slashes, and validates the host is github.com/gitlab.com.

## URL acceptance

`parseRubyGemsURL` in `internal/profile/target.go`:

**Accepted shapes:**
- `https://rubygems.org/gems/rails` → `("rails", "")`
- `https://rubygems.org/gems/rails/versions/7.1.3` → `("rails", "7.1.3")`
- `http://rubygems.org/gems/rails` → `("rails", "")`
- `rubygems.org/gems/rails` → `("rails", "")`

**Rejected:**
- Other rubygems.org paths (docs, profiles, search results)
- Non-rubygems.org URLs

**Host-anchoring:** Exact match on `rubygems.org` after scheme strip
(same pattern as `parseCratesIOURL`).

## File inventory

### Phase A — Manifest parser

| File | Action | Notes |
|------|--------|-------|
| `internal/manifest/gem/parse.go` | Create | `Parse(path)`, `ParseGraph(lockPath)` |
| `internal/manifest/gem/parse_test.go` | Create | Hermetic; testdata fixtures |
| `internal/manifest/gem/testdata/simple/Gemfile` | Create | Basic gem declarations |
| `internal/manifest/gem/testdata/simple/Gemfile.lock` | Create | Resolved lock with transitives |
| `internal/manifest/gem/testdata/with-git-deps/Gemfile` | Create | path: and git: deps |
| `internal/manifest/gem/testdata/with-git-deps/Gemfile.lock` | Create | Mixed GIT/PATH/GEM sections |
| `internal/profile/gem.go` | Create | `NormalizeGemName` |
| `internal/profile/gem_test.go` | Create | Normalization cases |
| `internal/profile/uri.go` | Modify | Add `"gem"` case |
| `internal/manifest/detect.go` | Modify | Add `{"Gemfile", "gem"}` |
| `internal/survey/survey.go` | Modify | Add `case "Gemfile":` to both dispatchers |
| `internal/ecosystem/detect.go` | Modify | Add gem ecosystem constant + manifest signal |

### Phase B — Registry signal collector

| File | Action | Notes |
|------|--------|-------|
| `internal/signal/registry/gem/wire.go` | Create | Response structs |
| `internal/signal/registry/gem/client.go` | Create | GetGem, GetVersions, GetOwners, ResolveRepoURL, ValidateGemName |
| `internal/signal/registry/gem/client_test.go` | Create | httptest-backed |
| `internal/signal/registry/gem/collector.go` | Create | signal.Collector impl, 9 signals |
| `internal/signal/registry/gem/collector_test.go` | Create | Fixtures, edge cases |
| `internal/signal/types.go` | Modify | Register `native_extension_present`, `native_extension_introduced`, `mfa_required` |
| `cmd/signatory/collectors.go` | Modify | Add `case "gem":` |

### Phase C — URL acceptance + source resolver

| File | Action | Notes |
|------|--------|-------|
| `internal/profile/target.go` | Modify | Add `parseRubyGemsURL`, wire into `ResolveTarget` |
| `internal/profile/target_test.go` | Modify | Acceptance + rejection shapes |
| `internal/ecosystem/resolver/gem.go` | Create | `GemResolver`, init registration |
| `internal/ecosystem/resolver/gem_test.go` | Create | httptest-backed |

### Phase D — Orchestrator wiring

| File | Action | Notes |
|------|--------|-------|
| `cmd/signatory/analyze.go` | Modify | `resolveGemRepo`, resolution block, `resolvableEcosystems` |
| `cmd/signatory/analyze_error_test.go` | Modify | Update `pkg:gem/rails` fixtures → `pkg:nuget/...` |
| `cmd/signatory/collectors_test.go` | Modify | Same: unwired-ecosystem fixture swap |
| `cmd/signatory/main.go` | Modify | `GemRegistryURL string` in Globals |

## Implementation sequence (TDD)

### Phase A (est. 1-2 sessions)

1. `NormalizeGemName` + test — trivial, establishes the URI convention
2. Write testdata fixtures (real-world Gemfile/Gemfile.lock from a
   small project like `puma` or `sidekiq`)
3. Failing test: `TestParse_Simple` — expects dep list from testdata
4. Implement `Parse()`: Gemfile line-scanning + Gemfile.lock merge
5. Failing test: `TestParseGraph_Simple` — expects edges from lockfile
6. Implement `ParseGraph()`: lockfile section parsing
7. Wire: `detect.go`, `survey.go`, `ecosystem/detect.go`
8. Integration: `signatory survey` on a Ruby project clone

### Phase B (est. 2-3 sessions)

1. `wire.go` — pure data types
2. Failing tests: `client_test.go` — GetGem, GetVersions, GetOwners,
   ResolveRepoURL, name validation
3. Implement `client.go`
4. Register new signal types in `types.go`
5. Failing tests: `collector_test.go` — fixture-based, all signals
6. Implement `collector.go`
7. Verify: `go test ./internal/signal/registry/gem/...`

### Phase C (est. 1 session)

1. Failing test: `TestResolveTarget_RubyGemsURLs`
2. Implement `parseRubyGemsURL` + wire into `ResolveTarget`
3. Wire normalization into `CanonicalPackageURI` + `resolveCanonicalURI`
4. Failing tests: `gem_test.go` in resolver package
5. Implement `GemResolver` + init registration

### Phase D (est. 1 session)

1. Add `"gem": true` to `resolvableEcosystems`
2. Write `resolveGemRepo` (symmetric with `resolveCargoRepo`)
3. Add resolution block to `AnalyzeCmd.Run`
4. Wire `case "gem":` in `collectorsFor`
5. Add `GemRegistryURL` to Globals
6. Update `pkg:gem/rails` test fixtures to use `pkg:nuget/...`
7. Smoke test: `signatory analyze pkg:gem/rails --clone --refresh`

## Risks and mitigations

| Risk | Mitigation |
|------|-----------|
| Gemfile DSL too complex for line scanning | Use Gemfile.lock as primary source; Gemfile scanning is best-effort for direct-dep identification when no lockfile exists |
| Owners endpoint requires API key | Degrade to `authors` string parsing; record absence with reason |
| `publish_origin_consistency` has low fidelity (no per-version publisher login) | Document the lower forgery-resistance rating; emit the signal with reduced confidence annotation |
| Native extension detection is indirect | Use `platform != "ruby"` in versions + `extensions` field; document as heuristic |
| Pre-release version conventions vary | Use rubygems' own `prerelease` boolean field on version entries — no heuristic needed |

## Smoke test targets

- **rails** — large dependency tree, mature, good for survey testing
- **puma** — small, has native C extension, good for extension detection
- **nokogiri** — native extension poster child (libxml2 binding)
- **sidekiq** — pure Ruby, single maintainer, good bus-factor signal
- **devise** — moderate size, multiple maintainers, good governance test

## Comparison with shipped ecosystems

| Capability | Go | npm | PyPI | Cargo | Gems (planned) |
|------------|----|----|------|-------|----------------|
| Manifest parse | ✓ | ✓ | ✓ | ✓ | Phase A |
| Lockfile graph | ✓ | — | — | ✓ | Phase A |
| Registry collector | ✓ (gopublish) | ✓ | partial | ✓ | Phase B |
| URL acceptance | — | ✓ | ✓ | ✓ | Phase C |
| Source resolver | ✓ | ✓ | ✓ | ✓ | Phase C |
| Orchestrator wiring | ✓ | ✓ | ✓ | ✓ | Phase D |
| Build-time code signal | — | `postinstall_*` | — | `build_script_*` | `native_extension_*` |
| Entity minting | — | ✓ | ✓ | ✓ | Phase B |
