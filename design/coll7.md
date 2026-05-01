# Collector #7: Cross-Version Source-Evolution Matrix

Status: draft v3 (post-iteration on network surface and gopublish coupling). Ready for review and edit-in-place.

## Goal

Surface the *sleeper → weaponized* publication pattern as a Layer-1 mechanical signal, where the load-bearing evidence is direct comparison of source content across tagged versions of the same module.

The 2026-04-30 BufferZoneCorp public report enumerates a 17-repo Go+Ruby campaign whose signature behavior is "publish v0.1.0 with plausible utility code; later add the malicious payload at v0.3.0+". Signatory's existing pipeline catches this *correlatively* via tag-cadence + zero-tests heuristics (the cluster of conclusions analysts label PROV-009-style). It does not catch it *directly* — i.e., "the source code at v0.1.0 differs from v0.3.0 in ways that introduce new init() functions, network calls, sensitive-path reads, and exec invocations."

The matrix is the direct measurement. Forgery resistance: VERY HIGH, because it's a property of the immutable git ref-graph anchored to the registry-side publication SHA.

## Layer placement

Provenance lane. Two reasons:

1. The signal is identity / chain-of-custody at heart: "what did the publishing identity ship at each tagged release event?"
2. The provenance handoff template already auto-routes signals grouped under `SignalGroupPublication` into the analyst's `signals.publication.<type>` view. No analyst-prompt re-architecture needed.

The provenance analyst, not the security analyst, reads the matrix and produces the `category: source_evolution_payload_introduction` conclusion.

## Existing collector subsystem map

Key files and line ranges (verified against tree):

| Path | Role |
|---|---|
| `internal/profile/signal.go:30-41` | `profile.Signal` struct: `{ID, EntityID, Type, Group, Source, ForgeryResistance, Value json.RawMessage, ...}`. New signal types JSON-marshal into `Value`; no schema migration. |
| `internal/signal/types.go:107-646` | Canonical type registry. New types must be registered here. `signal.Make` panics on unregistered types via `RecordSignal` (`internal/signal/make.go:73-78`). |
| `internal/signal/collector.go:10-31` | `Collector` interface: `{Name() string, Collect(ctx, *profile.Entity) (*CollectionResult, error)}`. Per-signal failures land as absences via `RecordFailure`; returning error is reserved for "collection cannot proceed at all". |
| `internal/signal/registry/gopublish/client.go:286-309` | `GetVersionList(ctx, mod)` — proxy.golang.org `@v/list`. |
| `internal/signal/registry/gopublish/client.go:314-333` | `GetVersionInfo(ctx, mod, ver)` — proxy.golang.org `@v/<v>.info`. **Returns `Origin.Hash` — the proxy's pinned commit SHA at publication time. The new `version_pin_table` signal aggregates these into a single record consumed by source-evolution.** |
| `internal/signal/git/exec.go:48-72` | `runGit` wrapper; env-strip discipline; the established subprocess pattern. |
| `internal/gitenv/` | Project-central git-subprocess discipline (env-strip, `WaitDelay`, cancellation). The 2026-04-24 worktree-corruption postmortem cited at `internal/signal/git/exec.go:25` is institutional memory. |
| `cmd/signatory/handoff.go:329-342` | Detects `cmd.Role == "provenance"` and dispatches to `assembleProvenanceSignals`. |
| `cmd/signatory/handoff.go:553-626` | `assembleProvenanceSignals` reads entity, loads latest signals, runs `profile.Summarize`, substitutes JSON into the handoff template's `{LAYER_1_SIGNALS}` placeholder. |
| `internal/profile/summary.go:39-76` | `profile.Summarize` buckets signals by `SignalGroup` into the JSON shape `{vitality, governance, publication, hygiene, criticality}`. **A new `SignalGroupPublication` signal lands automatically under `signals.publication.<type>` with no template wire change.** |
| `cmd/signatory/collectors.go:102-` | `collectorsFor` dispatch — the new collector wires in here for Go-ecosystem entities. |
| `cmd/signatory/collectors.go:343-413` | `ensureCloneAtPath` — the existing `--clone --refresh` machinery the new collector reuses unchanged. |
| `templates/handoffs/provenance-review-v1.md` | Provenance handoff. `{LAYER_1_SIGNALS}` placeholder around line 80; "Signal types" prose around lines 477-509. |

Reference signals (existing) that share posture-shape with the new ones:

- `version_count` — `gopublish/collector.go:96-107`. Cheap longitudinal count from `@v/list`.
- `publish_origin` — `gopublish/collector.go:175-195`. Per-version Origin block from `@v/<v>.info`. The forgery-resistance anchor; the new `version_pin_table` extends gopublish to emit a joint table over the same data.
- `publish_origin_consistency` — `internal/signal/types.go:438-449`. Closest existing precedent: longitudinal cross-version signal with VeryHigh forgery resistance because the registry-side data is publish-stamped.
- `tag_signing_status` — `internal/signal/git/tags.go:141-203`. Single-collection-call multi-version emission shape we mirror.
- `postinstall_introduced` — `internal/signal/types.go:427-437`. npm-side longitudinal "appeared in latest version that didn't have it before" signal — same conceptual shape as `source_evolution_anomaly` but binary, not matrix-valued.

## Integration points

### Where new code lives

`internal/signal/source/` — directory reserved by `design/v0.1-invariants.md:145` and `design/mcp-dual-analyst-architecture.md:122`. Currently empty.

```
internal/signal/source/
  collector.go         Collector type, Collect dispatcher, signal emission
  collector_test.go
  doc.go               package overview (parallels gopublish/doc.go)
  errors.go            sentinel errors (ErrSHAMissingFromClone, ErrPinTableNotAvailable, ...)
  budget.go            version-selection (last-N + leaves-of-each-major) + caps
  budget_test.go
  pinsource.go         VersionPinSource impl backed by signal store / in-run CollectionResult
  pinsource_test.go
  blobstream.go        git cat-file --batch wrapper; SourceProvider impl
  blobstream_test.go
  matrix.go            per-version feature aggregation + cross-version diff stat
  matrix_test.go
  golang/              Go-language AST analyzer (package golang)
    analyze.go         consumes (path, content) iterator, emits Features
    analyze_test.go    pure in-memory unit tests
    patterns.go        sensitive-path patterns, network packages, exec patterns
    patterns_test.go
  testdata/
    progressions/      fixture data for the integration test (small text payloads
                       loaded into the programmatic git repo at test time)
```

The `go/` subpackage isolates language-specific analysis behind a `LanguageAnalyzer` interface. When ecosystem expansion lands later, sibling `source/python/`, `source/rust/` packages implement the same interface; the collector dispatches by entity ecosystem.

### Three new signal types

One in gopublish, two in source-evolution:

```
"version_pin_table"  (gopublish)
  Group:   Publication
  Forgery: VeryHigh
  Caveats:
    - Only emitted for Go-ecosystem entities; other ecosystems skip silently.
    - Per-version Origin.Hash is read from proxy.golang.org's @v/<v>.info;
      pre-Go-1.20 publishes with no Origin block fall back to local refs/tags.
    - Existing per-version `publish_origin` signals continue to be emitted
      alongside; consumers that want the joint table prefer this signal,
      consumers that want one specific version's Origin prefer publish_origin.

"source_evolution_matrix"  (source)
  Group:   Publication
  Forgery: VeryHigh
  Caveats:
    - Bounded by collector budget (last-N + leaves-of-each-major).
    - Go-specific in v0.1; non-Go entities skip without emitting.
    - Sensitive-path patterns are conservative; intentionally
      false-negative-heavy to keep analyst trust in spike signals.
    - AST count of init() does not distinguish legitimate package
      init from payload bootstrap; the analyst's job to interpret.

"source_evolution_anomaly"  (source)
  Group:   Publication
  Forgery: VeryHigh
  Caveats:
    - Synthesized boolean+pointer summary derived from the matrix:
      "an inflection point exists between consecutive tagged
      versions where ≥2 feature counts spike above their previous
      baseline." Includes suspect version pair + which features spiked.
    - Refactors and legitimate feature additions can also spike;
      Layer-2 analyst classifies.
    - Threshold is conservative (false-negative-heavy by design).
```

Two source-evolution signals, not one, because the analyst should be able to read the boolean as a fast-path summary AND the full matrix to reason about it. The matrix is too verbose to be the only signal; the boolean is too thin to be the only signal.

### Provenance handoff template edits

Three additions to `templates/handoffs/provenance-review-v1.md`:

1. **Signal types list (around lines 477-509)** under "Publication integrity":
   ```
   - version_pin_table — joint per-version commit-SHA pin table from
     proxy.golang.org. Source for `tag_sha` citations in
     source_evolution_matrix conclusions.
   - source_evolution_matrix — per-tagged-version AST feature counts
     (init()s, network calls, sensitive-path reads, exec calls,
     base64-decoded constants), file diff stats, and new-symbol-export
     counts. The matrix exposes sleeper→weaponized progression directly.
   - source_evolution_anomaly — derived boolean: an inflection between
     consecutive versions where ≥2 feature counts spike. Cite both
     signals when you draw a conclusion: the anomaly for the verdict,
     the matrix for the citation.
   ```

2. **New "Reading the source-evolution matrix" prose section** between the Calibration notes and the Output format (around line 175):
   - Zeros at v0.1.0 followed by spikes at v0.3.0 is the BufferZoneCorp signature.
   - One feature spiking on its own is rarely conclusive; multiple features spiking simultaneously (init + sensitive-path + network) is the joint distribution that matters.
   - A high baseline that stays flat is "this is just what this code does." A library that has had network calls since v0.1.0 is not anomalous; a tag-parsing utility that suddenly gains network calls at v0.3.0 is.
   - The matrix is per-tag; Layer 1 doesn't classify, the analyst's job is to read the actual files at the spike SHA and decide.
   - A row with `tag_sha_local_status: missing_from_clone` is itself a signal: the proxy has a SHA that `--clone --refresh` did not fetch. Treat as forgery-resistance HIGH for that version.

3. **Citation form** for matrix-driven conclusions (repo-tree-scope per the existing citation grammar at `provenance-review-v1.md:217-220`):
   ```
   Citation: "source_evolution_matrix at v0.3.0:
              init=1 (was 0), sensitive_path_reads=8 (was 0), exec_calls=3 (was 0)"
   ```

### Storage schema

No migration. `Signal.Value` is `json.RawMessage`; the matrix marshals into the existing append-only `signals` table. Three new entries in `internal/signal/types.go` are the only schema-shaped change, and that file is compile-time only.

Per-target row size: ~12KB at the default budget (16 versions × ~750B/row). Within SQLite's comfort zone.

### Synthesis template

`templates/handoffs/synthesis-v1.md` should briefly note that `source_evolution_anomaly` is forgery-resistance VERY HIGH and weights comparably to direct payload citations (the F001-class). The synthesis pipeline already weights by forgery-resistance, but explicitly naming the new signal in the prose helps the synthesist's calibration when it appears for the first time. Small edit, mid-priority — folded into the handoff-prose commit (#15).

## Architectural decisions

### D1. Source acquisition: `git cat-file --batch` against the existing clone (NOT worktree-add)

The collector reuses the clone the orchestrator already populated via `--clone --refresh`. No new clone, no worktree-add, no temp-dir cleanup, no on-disk mutation. Operations are read-only against the existing object DB:

- `git ls-tree -r <SHA>` → enumerate files at the version. Filter to `*.go`, exclude `_test.go` and `vendor/`.
- `git cat-file --batch` → persistent subprocess for the collector run; pipe blob SHAs to stdin, read formatted blob contents from stdout. Streamed straight into `go/parser.ParseFile(fset, name, src, mode)` where `src` is `[]byte`.
- `git diff --numstat <sha-prev>..<sha-curr>` → diff stat between consecutive versions; one subprocess per pair.
- **No `git fetch` by default.** If `cat-file --batch` reports `<sha> missing`, the row is preserved with `tag_sha_local_status: "missing_from_clone"` and `ast: null`. This is itself a forgery-resistance HIGH signal — the proxy has a SHA that the orchestrator's `--clone --refresh` didn't fetch. The opt-in `--allow-fetch` flag enables a one-shot `git fetch origin <sha>` on miss, marking the row `tag_sha_local_status: "fetched_via_allow_fetch"`.

Why not worktree-add per version: working-tree materialization is wasted I/O. The AST analyzer never needs files on disk — `go/parser` accepts `[]byte` directly. Worktree-add would also require defer-cleanup discipline and a `git worktree prune` defense; cat-file has neither failure mode.

Why not `git archive` to tarball: still materializes bytes that the parser would just re-read. Cat-file is one less hop.

Why not in-process git library (go-git): the project chose shell-to-git definitively (`internal/gitenv/` postmortem heritage). Adding a git library now is a backward step.

### D2. Compound signal record per target (NOT per-version-pair)

One `source_evolution_matrix` record per target, plus one `source_evolution_anomaly` per target. Not N records per version pair.

Rationale: the `signals` table is append-only with deterministic ID; per-version emission collides with `Make`'s ID format, and `GetLatestSignals` (which does `MAX(collected_at) GROUP BY type`) gets slower as row count per type grows. The matrix's whole point is the cross-version comparison; splitting it into per-pair records forces every consumer to JOIN them back together. The compound JSON value stays bounded (~12KB) by the budget.

Compound JSON shape:

```json
{
  "module_path": "github.com/BufferZoneCorp/grpc-client",
  "ecosystem": "go",
  "language": "go",
  "budget": {
    "selected_versions": ["v0.1.0", "v0.2.0", "v0.3.0", "v0.3.1"],
    "skipped_versions": [],
    "selection_strategy": "last_n_plus_major_leaves",
    "last_n": 12,
    "major_leaves": 4
  },
  "rows": [
    {
      "version": "v0.1.0",
      "tag_sha": "abc123...",
      "tag_sha_source": "proxy.golang.org",
      "tag_sha_local_status": "present",
      "ast": {
        "init_count": 0,
        "network_call_sites": 0,
        "sensitive_path_reads": 0,
        "exec_calls": 0,
        "byte_array_xor_decode_loops": 0,
        "base64_decoded_const_bytes_over_threshold": 0
      },
      "structural": {
        "go_file_count": 12,
        "go_loc": 850,
        "new_top_level_packages": [],
        "new_symbol_exports": []
      },
      "diff_from_previous": null
    },
    {
      "version": "v0.3.0",
      "tag_sha": "xyz789...",
      "tag_sha_source": "proxy.golang.org",
      "tag_sha_local_status": "present",
      "ast": {
        "init_count": 1,
        "network_call_sites": 3,
        "sensitive_path_reads": 8,
        "exec_calls": 3,
        "byte_array_xor_decode_loops": 1,
        "base64_decoded_const_bytes_over_threshold": 240
      },
      "structural": {
        "go_file_count": 18,
        "go_loc": 1320,
        "new_top_level_packages": ["github.com/.../internal/util"],
        "new_symbol_exports": ["util.Apply", "util.Decode"]
      },
      "diff_from_previous": {
        "files_added": 6,
        "files_changed": 4,
        "files_removed": 0,
        "lines_added": 470,
        "lines_removed": 0
      }
    },
    {
      "version": "v0.4.0",
      "tag_sha": "deadbeef...",
      "tag_sha_source": "proxy.golang.org",
      "tag_sha_local_status": "missing_from_clone",
      "ast": null,
      "structural": null,
      "diff_from_previous": null
    }
  ]
}
```

`tag_sha_local_status` enum: `"present"` | `"missing_from_clone"` | `"fetched_via_allow_fetch"`. Rows with `missing_from_clone` are preserved (analyst sees the gap explicitly) but have null analysis blocks.

### D3. Tag-SHA pinning: gopublish emits, source-evolution consumes

Tag-SHA pinning is gopublish's responsibility. The source-evolution collector does not touch proxy.golang.org directly.

**gopublish change**: extend `internal/signal/registry/gopublish/collector.go` to emit a new `version_pin_table` signal alongside the existing per-version `publish_origin` signals. The pin table is a single compound record per Go-ecosystem entity:

```json
{
  "module_path": "github.com/foo/bar",
  "pins": [
    {"version": "v0.1.0", "sha": "abc...", "source": "proxy.golang.org"},
    {"version": "v0.2.0", "sha": "def...", "source": "proxy.golang.org"},
    {"version": "v0.3.0", "sha": "ghi...", "source": "local_clone_refs_tags"}
  ],
  "missing_origin_versions": ["v0.3.0"]
}
```

Per-pin `source` enum: `"proxy.golang.org"` (default) | `"local_clone_refs_tags"` (proxy lacked Origin block; pre-Go-1.20 publish; see `gopublish/collector.go:178-186`).

**source-evolution change**: a `VersionPinSource` interface abstracts pin lookup:

```go
type VersionPinSource interface {
    VersionPinTable(ctx context.Context, entity *profile.Entity) (PinTable, error)
}

type PinTable struct {
    ModulePath string
    Pins       []VersionPin
}

type VersionPin struct {
    Version string
    SHA     string
    Source  string  // "proxy.golang.org" | "local_clone_refs_tags"
}
```

Production impl reads from the in-run `CollectionResult` first (the same analysis just ran gopublish), falling back to `signal.Store.GetLatestSignals` (a previous analysis). If neither has a `version_pin_table` for the entity, source-evolution emits an absence with reason `"version pin table required; gopublish collector did not run or did not emit"` and exits cleanly — no proxy access, no GitHub access.

**Sequencing**: `cmd/signatory/collectors.go` dispatch wires gopublish before source-evolution for Go entities. Test fakes inject hand-built `PinTable` directly.

### D4. Budget: last-N + leaves-of-each-major, with caps

Defaults:
- `last_n = 12` (most recent N tagged versions, regardless of major)
- `major_leaves = 4` (highest version within each of the last 4 majors)
- Hard cap on total selected versions: 20

Rationale: payload-introduction events are most often in recent versions. Major-leaves protect against a campaign that introduces malice in v1.0.0 of an existing v0.x clean line. The 20-cap bounds collector cost.

Knobs: `WithBudget(BudgetOpts)` constructor option for tests and future tuning. Fields exposed in the matrix's `budget` block so the analyst can see what was selected and skipped.

### D5. AST analysis in-process (NOT subprocess)

Go's `go/parser` is stdlib; one .go file parses in microseconds. A typical Go module's tagged version (50-200 .go files, total <500KB source) parses in under 10ms. Subprocess overhead is dominantly slower than the parse work for these sizes.

Memory bound: AST nodes for a 500KB source tree consume <50MB heap; comfortable. Parsing does not execute init() or evaluate constants beyond `constant.Value`; no risk of executing the malicious payload during analysis.

The one place where subprocess discipline matters is the source-acquisition step (cat-file as a persistent subprocess), not parsing.

### D6. Analyzer interface: file-content iterator (NOT directory walker)

```go
// SourceFile is one source file's path-and-bytes presented to the analyzer.
type SourceFile struct {
    Path    string  // posix-style relative to module root
    Content []byte  // file contents
}

// LanguageAnalyzer parses an iterator of source files and emits a
// normalized feature set. One implementation per ecosystem.
type LanguageAnalyzer interface {
    Name() string                                                       // "go"
    Analyze(ctx context.Context, files iter.Seq2[SourceFile, error]) (Features, error)
}
```

`iter.Seq2` is Go 1.23+; project supports Go 1.24+ minimum (go.mod targets 1.25.1 actively).

The analyzer never touches `os` or `filepath`. AST unit tests construct in-memory file maps and feed them through the iterator — no git, no temp dirs, no fixtures-on-disk.

### D7. SourceProvider interface: blob streamer + diff stat

```go
type SourceProvider interface {
    // EnumerateGoFiles iterates (path, content) pairs for *.go files
    // at the given commit SHA, excluding _test.go and vendor/.
    EnumerateGoFiles(ctx context.Context, sha string) iter.Seq2[SourceFile, error]

    // DiffStat returns numstat between two commit SHAs.
    DiffStat(ctx context.Context, sha1, sha2 string) (DiffStat, error)

    // Close terminates the cat-file subprocess.
    Close() error
}
```

Production impl: `BlobStreamer` wrapping `git cat-file --batch` (single persistent subprocess) plus per-call `git ls-tree` and `git diff --numstat` invocations against the same clone. Emits `ErrSHAMissingFromClone` (sentinel) when cat-file reports a missing SHA; the matrix assembler catches this and produces a row with `tag_sha_local_status: "missing_from_clone"`.

If `--allow-fetch` is set, the BlobStreamer wraps the missing-SHA path with one `git fetch origin <sha>` retry before giving up.

Tests: in-memory fake (`fakeProvider{filesBySHA: map[string][]SourceFile, diffsByPair: map[[2]string]DiffStat, missingSHAs: map[string]bool}`). No git fixture needed for AST or matrix-assembly unit tests; only the BlobStreamer's own tests exercise real git.

### D8. Patterns as package-level `var`, not config file

`internal/signal/source/golang/patterns.go` holds the catalog as `var SensitivePathPatterns []string`, `var NetworkEgressCallSites []CallSite`, `var ExecCallSites []CallSite`. Constructor accepts `WithPatterns(Patterns)` for test override; production uses defaults.

This is a security catalog, not a user preference. Users editing it down to nothing silently disables detection of the payload class this collector exists to catch. PR review + test coverage is the right change-control path; user-edited TOML is not.

Initial `SensitivePathPatterns` (informed by BufferZoneCorp variants):

```go
"/.ssh/", ".ssh/authorized_keys", ".ssh/id_rsa", ".ssh/id_ed25519",
"/.aws/", ".aws/credentials", ".aws/config",
"/.npmrc", ".npmrc",
"/.netrc",
"/.kube/", ".kube/config",
"/.docker/", ".docker/config.json",
"/.config/gh/", ".config/gh/hosts.yml",
"/.gnupg/",
"/etc/passwd", "/etc/shadow",
"169.254.169.254",  // IMDS — informed by go-stdlog payload
"go.sum",            // go-metrics-sdk variant directly tampers go.sum
```

Initial network-egress sites: `net/http.{Get,Post,PostForm,Do,NewRequest,NewRequestWithContext}`, `net.{Dial,DialTimeout,DialContext}`. Import-aliasing handled by the analyzer (track import map per file).

Initial exec sites: `os/exec.{Command,CommandContext}`.

### D9. Anomaly threshold: multi-feature joint, conservative

Anomaly fires only when ≥2 feature counts cross from 0 (or near-zero baseline) to non-zero within 1-2 consecutive selected versions.

The BufferZoneCorp pattern (init + network + sensitive-path together) fits comfortably. A single feature growing alone (e.g., a network library's network call count rising) does not fire. Conservative on purpose: false negatives are recoverable (the matrix is still in the handoff and the analyst can still notice); false positives erode analyst trust in the boolean.

`WithAnomalyThreshold(Threshold)` constructor option for tuning as we acquire more campaign corpus.

### D10. Dispatch: Go entities only in v0.1

`cmd/signatory/collectors.go:114-121` adds (after the existing gopublish wiring):
```go
case "golang", "go":
    collectors = append(collectors,
        gopublishcollector.NewCollector(...),  // already present; emits version_pin_table
        sourcecollector.NewCollector(clonePath, pinSource, sourceOpts...),
    )
```

`pinSource` is the production `VersionPinSource` impl (reads from in-run `CollectionResult` then signal store). `sourceOpts` carries the `--allow-fetch` toggle and any budget/pattern overrides.

Non-Go entities skip silently (no absence emitted). Go entities without a clone path emit an absence with reason `"local clone required for source-evolution matrix"`. Go entities where gopublish didn't run or didn't emit `version_pin_table` emit absence with reason `"version pin table required; gopublish collector did not run or did not emit"`.

When Python/Rust/etc. analyzers land later, the dispatch grows additional cases with the same shape; the collector's internal language-analyzer registry picks the right `LanguageAnalyzer`.

### D11. Network surface

The collector's network surface, codified for the operator:

| Bucket | Operations | When |
|---|---|---|
| **Local-only (always)** | `git ls-tree -r <SHA>`, `git cat-file --batch`, `git diff --numstat`, all AST parsing | Every collection run after `--clone --refresh` |
| **proxy.golang.org (via gopublish)** | `@v/list`, `@v/<v>.info` | Every gopublish collection for a Go entity. Source-evolution does *not* touch proxy directly; it consumes gopublish's `version_pin_table` signal |
| **GitHub (only with `--allow-fetch`)** | `git fetch origin <sha>` for one specific SHA | Only when cat-file reports `<sha> missing` AND `--allow-fetch` is set. Default (no flag): row preserved with `tag_sha_local_status: "missing_from_clone"`, no fetch attempted |

Default operational stance: **after `--clone --refresh` completes, the source-evolution collector touches no remote**. A missing local SHA is preserved as a signal, not papered over with another fetch. `--clone --refresh` is the explicit gate; if it ran and the SHA is still absent, that's diagnostic information about the publish chain (force-push, proxy/clone divergence, registry-side anomaly).

`--allow-fetch` is opt-in for the case where the operator knows the clone may be partial and prefers completeness over the missing-SHA signal.

## TDD test plan

### First failing test (Commit 2 driver — source-evolution registry)

```go
// internal/signal/types_test.go
func TestRegistry_HasSourceEvolutionMatrix(t *testing.T) {
    spec, ok := signal.LookupType("source_evolution_matrix")
    require.True(t, ok)
    assert.Equal(t, signal.SignalGroupPublication, spec.Group)
    assert.Equal(t, signal.ForgeryVeryHigh, spec.ForgeryResistance)
}
```

(Commit 1 is the gopublish `version_pin_table` registration, with a parallel `TestRegistry_HasVersionPinTable`.)

### AST analyzer first green (Commit 3 driver)

Pure in-memory; no git involved. The analyzer takes a `SourceFile` iterator, so:

```go
// internal/signal/source/golang/analyze_test.go
func TestAnalyze_EmptySource_ZerosAllFeatures(t *testing.T) {
    a := golang.NewAnalyzer()
    files := iterErrFree(slices.Values([]golang.SourceFile{}))
    feats, err := a.Analyze(t.Context(), files)
    require.NoError(t, err)
    assert.Zero(t, feats.InitCount)
    // ...
}

func TestAnalyze_SingleInit_CountsOne(t *testing.T) {
    a := golang.NewAnalyzer()
    files := iterErrFree(slices.Values([]golang.SourceFile{
        {Path: "main.go", Content: []byte("package main\nfunc init() { _ = 1 }\n")},
    }))
    feats, err := a.Analyze(t.Context(), files)
    require.NoError(t, err)
    assert.Equal(t, 1, feats.InitCount)
}
```

`iterErrFree` is a small test helper that adapts `iter.Seq[SourceFile]` into `iter.Seq2[SourceFile, error]` for the analyzer interface.

### Existing fixture conventions to follow

- `internal/signal/git/collector_test.go:27-72` — `initRepo` / `mustRunGit` / `commitEmpty`. Programmatic git construction. The integration test in commit 13 follows this pattern.
- `internal/signal/git/collector_test.go:251+` — `indexByType` / `findSignal` / `unmarshalValue` helpers for asserting on emitted signals.

No new top-level testdata directories needed for AST work; the analyzer accepts in-memory bytes. Larger payload fixtures (the v0.3.0 "weaponized" file in the integration test) live as `internal/signal/source/testdata/progressions/<name>.go.txt` and are loaded into the programmatic git repo at test time. The `.txt` extension keeps `go vet` and `go test` from accidentally parsing them.

### Validation strategy (negative + positive cases)

Three layers, in order of cost. **Run in this order; the malicious target is the final validation, not the first.**

**Layer 1 — Programmatic test fixtures (every CI build):**

Alongside the load-bearing positive test (commit 13: `TestCollector_SyntheticProgression_MatrixSpikesAtV020`):

- `TestCollector_CleanProgression_AllZeros` — boring evolution; matrix shows zeros across all rows; anomaly false. *Validates: legitimate package growth doesn't fire.*
- `TestCollector_StableHighBaseline_NoAnomaly` — every version has the same legitimate `http.Get` call. Network call count non-zero across all rows but stable. Anomaly false. *Validates: high stable baseline ≠ anomaly.*
- `TestCollector_GradualGrowth_NoAnomaly` — one feature grows per version (network at v0.2.0, exec at v0.3.0, sensitive-path at v0.4.0), never multi-jointly within consecutive versions. Anomaly false. *Validates: multi-feature-joint discipline.*
- `TestCollector_LegitimateInitFunction_NoAnomaly` — adds `init() { flag.Parse() }`. InitCount rises 0→1; no other features spike. Anomaly false. *Validates: single-feature-cross-zero alone doesn't fire.*

**Layer 2 — Real-module smoke tests (gated under `-short` skip):**

Run on demand against real proxy.golang.org + real clones:

| Target | Why | Expected matrix shape |
|---|---|---|
| `github.com/google/uuid` | Rock-solid baseline. Tiny, heavily-used, multi-version, mature | Zeros across all rows. Anomaly false. *If this fires anything, we have a bug.* |
| `github.com/hashicorp/go-retryablehttp` | **Most important real-world test.** Legitimate HTTP client; canonical ancestor of the BufferZoneCorp typosquat | Stable non-zero network-call baseline; zeros elsewhere. Anomaly false. *Validates we don't false-positive on the very library the campaign was imitating.* |
| `golang.org/x/mod` | Already a project dependency; well-maintained | Mostly stable. Anomaly false |

**Layer 3 — Dogfood against signatory itself:**

After implementation lands, run the collector against signatory's canonical URI. Persist the matrix to `design/dogfood/source-evolution-self.md` per the project's "always record trust analyses" discipline. Validates the dogfood story end-to-end.

**Re-pointing at BufferZoneCorp/grpc-client is the *last* validation step**, after Layers 1-3 succeed. The pairing in step 4 + final-step gives us the demo case: HashiCorp's stable baseline next to BufferZoneCorp's spike from zero, both produced by the same collector.

### Load-bearing integration test (Commit 13 driver)

```go
func TestCollector_SyntheticProgression_MatrixSpikesAtV020(t *testing.T) {
    repo := initRepoWithProgression(t, []Version{
        {Tag: "v0.1.0", Files: map[string]string{
            "main.go": "package main\nfunc Hello() string { return \"hi\" }\n",
        }},
        {Tag: "v0.2.0", Files: map[string]string{
            "main.go": loadFixture(t, "progressions/v020-weaponized.go.txt"),
        }},
    })

    // Hand-built pin table; bypasses gopublish for the integration test.
    pinSource := &fakePinSource{
        table: source.PinTable{
            ModulePath: "example.com/synth",
            Pins: []source.VersionPin{
                {Version: "v0.1.0", SHA: shaOf(t, repo, "v0.1.0"), Source: "proxy.golang.org"},
                {Version: "v0.2.0", SHA: shaOf(t, repo, "v0.2.0"), Source: "proxy.golang.org"},
            },
        },
    }

    c := source.NewCollector(repo, pinSource)
    result, err := c.Collect(t.Context(), &profile.Entity{
        ID:           "test",
        CanonicalURI: "pkg:golang/example.com/synth",
        Ecosystem:    "golang",
    })
    require.NoError(t, err)

    matrix := unmarshalMatrix(t, findSignal(t, result, "source_evolution_matrix"))
    require.Len(t, matrix.Rows, 2)

    assert.Zero(t, matrix.Rows[0].AST.InitCount)
    assert.Zero(t, matrix.Rows[0].AST.NetworkCallSites)
    assert.Zero(t, matrix.Rows[0].AST.SensitivePathReads)

    assert.Equal(t, 1, matrix.Rows[1].AST.InitCount)
    assert.GreaterOrEqual(t, matrix.Rows[1].AST.NetworkCallSites, 1)
    assert.GreaterOrEqual(t, matrix.Rows[1].AST.SensitivePathReads, 1)
    assert.GreaterOrEqual(t, matrix.Rows[1].AST.ExecCalls, 1)

    anomaly := unmarshalAnomaly(t, findSignal(t, result, "source_evolution_anomaly"))
    assert.True(t, anomaly.AnomalyPresent)
    assert.Equal(t, "v0.2.0", anomaly.FirstAnomalousVersion)
}
```

This exercises: `fakePinSource` → BlobStreamer (real `git cat-file --batch` against the programmatic repo) → AST analyzer → matrix assembler → anomaly detector → signal emission. End-to-end real-git-bytes work, no mocks for the git surface.

## Step-by-step commit breakdown

Each commit lands a failing test first, then production change to green it, then refactor as needed. Smallest vertical slice first.

| # | Commit | Failing test | Production change |
|---|---|---|---|
| 1 | gopublish: emit version_pin_table | `TestRegistry_HasVersionPinTable`, `TestGopublishCollector_EmitsVersionPinTable_AllProxyOrigins`, `TestGopublishCollector_EmitsVersionPinTable_PartialMissingOrigins` | Register `version_pin_table` in `internal/signal/types.go`; extend `internal/signal/registry/gopublish/collector.go` to assemble per-version Origin hashes into a single compound signal. Existing per-version `publish_origin` emission stays |
| 2 | Register source-evolution signal types | `TestRegistry_HasSourceEvolutionMatrix`, `TestRegistry_HasSourceEvolutionAnomaly` | Two entries in `internal/signal/types.go` |
| 3 | AST: empty + init() count | `TestAnalyze_EmptySource_ZerosAllFeatures`, `TestAnalyze_SingleInit_CountsOne`, `TestAnalyze_NestedInitFunctions_CountedFromAllFiles`, `TestAnalyze_MethodNamedInit_NotCounted` | `internal/signal/source/golang/analyze.go` with `Features` struct, `Analyzer`, AST walker, init() detection |
| 4 | AST: network call sites with import aliasing | `TestAnalyze_HTTPGet_CountsOne`, `TestAnalyze_AliasedHTTPImport_CountsOne`, `TestAnalyze_NetDial_Counts` | Extend analyzer with import-map tracking; `patterns.go` with `NetworkEgressCallSites` |
| 5 | AST: sensitive-path reads | `TestAnalyze_OpenSSHIDRSA_Counts`, `TestAnalyze_LocalConfigPath_NotCounted`, `TestAnalyze_FilepathJoinHomeAWSCredentials_Counts` | String-literal extraction + pattern matching against `SensitivePathPatterns`; constant-folding for `filepath.Join(home, ".aws/credentials")` |
| 6 | AST: exec + lexical features | `TestAnalyze_ExecCommandSh_Counts`, `TestAnalyze_XORDecodeLoop_DetectedHeuristically`, `TestAnalyze_Base64DecodedConstantOverThreshold_Counted` | Extend analyzer with exec patterns; lexical scan for byte-array-XOR shape and base64 size threshold |
| 7 | Budget version selection | `TestBudget_LastNOnly_PicksLastN`, `TestBudget_MajorLeaves_PicksHighestPerMajor`, `TestBudget_HardCap_BoundsTotal` | `internal/signal/source/budget.go` with `Select(versions []string, opts BudgetOpts)`. Uses `golang.org/x/mod/semver` for ordering |
| 8 | VersionPinSource: read from CollectionResult / signal store | `TestPinSource_FromInRunResult_ReturnsTable`, `TestPinSource_FallsBackToStore`, `TestPinSource_NoPinTable_ReturnsErrPinTableNotAvailable` | `internal/signal/source/pinsource.go` with the production impl |
| 9 | BlobStreamer: cat-file --batch (no fetch) | `TestBlobStreamer_KnownSHA_StreamsContent`, `TestBlobStreamer_MissingSHA_ReturnsErrSHAMissingFromClone`, `TestBlobStreamer_ContextCancel_KillsSubprocess` | `internal/signal/source/blobstream.go` with `git cat-file --batch` persistent subprocess; `git ls-tree -r` enumeration. **No fetch fallback** by default |
| 10 | BlobStreamer: --allow-fetch opt-in | `TestBlobStreamer_AllowFetch_FetchesAndRetries`, `TestBlobStreamer_AllowFetch_StillMissing_ReturnsErrSHAMissingFromClone` | Extend BlobStreamer with `WithAllowFetch(true)` option that wraps missing-SHA path in one `git fetch origin <sha>` retry |
| 11 | DiffStat | `TestDiffStat_TwoTags_NumstatParsed`, `TestDiffStat_AddedAndRemovedFiles_BothCounted` | Extend `blobstream.go` (or split into `diffstat.go`) with `git diff --numstat` invocation and parsing |
| 12 | Matrix assembler — single version | `TestAssembleMatrix_OneVersion_OneRowDiffNil`, `TestAssembleMatrix_TagSHAFromProxy_RecordedAsSource`, `TestAssembleMatrix_MissingSHA_RowPreservedNullAST` | `internal/signal/source/matrix.go`. Wires versions → BlobStreamer → analyzer → row. Handles missing-SHA via `tag_sha_local_status` field |
| 13 | Matrix assembler — cross-version diff and exports | `TestAssembleMatrix_NewTopLevelPackage_RecordedInRow`, `TestAssembleMatrix_NewSymbolExports_RecordedInRow`, `TestAssembleMatrix_DiffFromPrevious_NonNilForSecondRow` | Extend `matrix.go` with diff stats and export-set comparison |
| 14 | Anomaly detection | `TestAnomaly_FlatBaseline_NoAnomaly`, `TestAnomaly_TwoFeaturesCrossZero_FiresAnomaly`, `TestAnomaly_OneFeatureGrows_NoAnomaly`, `TestAnomaly_GradualGrowth_NoAnomaly`, `TestAnomaly_LegitimateInitOnly_NoAnomaly` | Extend `matrix.go` with anomaly value computation; conservative multi-feature-joint threshold |
| 15 | Collector — wires everything | `TestCollector_NonGoEntity_RecordsAbsenceCleanly`, `TestCollector_GoEntity_NoPinTable_RecordsAbsence`, `TestCollector_CleanProgression_AllZeros`, `TestCollector_StableHighBaseline_NoAnomaly`, `TestCollector_LegitimateInitFunction_NoAnomaly`, the load-bearing `TestCollector_SyntheticProgression_MatrixSpikesAtV020` | `internal/signal/source/collector.go`. Implements `signal.Collector`. Composes analyzer + budget + pinSource + BlobStreamer + matrix |
| 16 | Wire collector into dispatch + plumb --allow-fetch flag | Extend `cmd/signatory/collectors_test.go` — `TestCollectorsFor_GoEntity_IncludesSourceCollector`. Update existing `TestCollectorsFor_CloneHappyPath` count assertion (4 → 5). `TestAnalyzeCmd_AllowFetchFlag_PropagatesToCollector` | `cmd/signatory/collectors.go:114-121` adds the new case after gopublish; CLI flag in `cmd/signatory/analyze.go` |
| 17 | Provenance + synthesis handoff prose | Extend `cmd/signatory/handoff_provenance_signals_test.go` (existing pattern at line 234) | Edit `templates/handoffs/provenance-review-v1.md` (3 additions per §"Provenance handoff template edits"); brief addition to `templates/handoffs/synthesis-v1.md` noting forgery-resistance VERY HIGH on the new signal |
| 18 | Long-history budget integration test | `TestCollector_RealWorldLongHistory_RespectsBudget` — fixture with 200+ programmatic tags, asserts matrix has exactly `last_n + len(major_leaves)` rows after dedup. Skipped under `-short` since slower | No production change beyond what 7+15 already shipped; this test is regression-protection for the budget cap |

Total: 18 commits. Commit 15 is the first end-to-end working milestone; 1-14 each pass their own tests in isolation. 16-18 are integration + UX polish.

Layer-2 real-module smoke tests are NOT in the commit table — they run on demand, gated under `-short`. Layer-3 dogfood is a manual ritual after the work lands.

## Open questions

For iteration. Q5 is now resolved (D3 + D11).

### Q1. Typed Go structs for the matrix value, or `map[string]any`?

Existing pattern in `internal/signal/git/tags.go:191-202` uses `map[string]any`. New signal value is the most complex JSON shape the store has held; typed struct catches field-name typos at compile time and dramatically simplifies test assertions.

**Proposed: typed structs** in the `source` package (`type MatrixValue struct {...}`) that JSON-marshal into the existing `Signal.Value` contract. Store doesn't care; sees `json.RawMessage` either way.

### Q2. Anomaly threshold — Option A absolute-jump-from-zero vs Option B multi-feature-joint?

- **A**: any feature 0→non-zero between consecutive versions fires.
- **B**: ≥2 features cross 0→non-zero within 1-2 consecutive versions fires.

**Proposed: B as default** with `WithAnomalyThreshold` option for tuning. B avoids the false-positive flood from libraries that legitimately add a single network capability at v0.X.

### Q3. Analyzer's treatment of `_test.go` and `vendor/`?

**Proposed: exclude both from AST-feature counts.** Threat model is "what executes when the package is imported" — test files don't run on import; vendored code isn't the package's own. Include `vendor/` in *structural* file count for transparency (a sudden vendor explosion is its own signal), but flagged separately.

### Q4. What level of import-alias and selector-expression analysis?

The analyzer needs to recognize `h.Get(...)` after `import h "net/http"`, plus dot-imports (`. "net/http"; Get(...)`), plus selector chains (`pkg.Sub.Get(...)`).

**Proposed**: per-file import map + `ast.SelectorExpr` walking with package-name lookup against the import map. Skip dot-imports for v0.1 (rare in package init; ship a follow-up if seen). Document the dot-import gap in the matrix `caveats` field.

### Q5. ~~How does the collector behave when proxy.golang.org is unreachable?~~ RESOLVED

**Resolution (D3 + D11)**: source-evolution does not touch proxy directly. gopublish owns proxy access and emits `version_pin_table`. If gopublish failed (proxy unreachable), source-evolution gets no pin table from `CollectionResult` → tries store → if also empty, emits absence with reason `"version pin table required; gopublish collector did not run or did not emit"`. The proxy-unreachable failure mode is gopublish's problem, not source-evolution's; analyst sees the absence cleanly.

If gopublish *partially* succeeded (some versions had Origin, others fell back to local refs/tags), the pin table records `source` per pin and source-evolution proceeds with degraded forgery-resistance for those rows.

### Q6. Cost knob — should the collector be opt-in?

This is the heaviest collector in the project (per-version blob enumeration + AST parsing + diff stat). Budget caps make it tractable, but for analyses where cheap signals already drive a confident posture, running it is wasted work.

**Proposed: always-on by default**, with `--skip-source-evolution` flag on `signatory analyze` for the case where someone is iterating fast on a different signal class. Long-term, the right answer is conditional execution: run if `version_count > N` or if `tag_signing_status.signed_ratio == 0` (cheap signals that already raised the operator's concern).

### Q7. Re-run cadence for sleeper watch

The collector's standing presence is the watch mechanism for sleepers (`log-core`-shape packages with no payload yet). If we ingest one analysis today and the operator weaponizes at v0.5.0 next week, our signal goes stale.

**Proposed: out of scope for this collector.** The watch is a scheduling/orchestration concern that belongs elsewhere (the schedule skill, or a dedicated re-collection cron). The collector just needs to produce a fresh, accurate matrix when invoked.

### Q8. Does the collector need to know the module's declared module path?

For Go: yes — `git ls-tree` gives us paths relative to the repo root, but the module path matters for "new top-level packages" (a new directory under `internal/` is a new package only relative to the module path).

**Proposed**: read `go.mod` from each version's tree (via the BlobStreamer) and use `golang.org/x/mod/modfile.Parse` to get the module path. Cache per-SHA. If `go.mod` is missing or unparseable, log and degrade structural analysis for that version.

## Critical files for implementation

- `internal/signal/types.go` — register `version_pin_table` (commit 1) + the two source-evolution types (commit 2).
- `internal/signal/registry/gopublish/collector.go:96-195` — extend with `version_pin_table` joint emission alongside existing per-version `publish_origin` (commit 1).
- `internal/signal/registry/gopublish/client.go:286-333` — `GetVersionList` + `GetVersionInfo` reused unchanged; the new joint signal aggregates over them.
- `internal/signal/git/collector_test.go:27-72, 251+` — test helpers (`initRepo`, `mustRunGit`, `commitEmpty`, `indexByType`, `findSignal`, `unmarshalValue`) the new collector's tests follow.
- `internal/signal/git/exec.go:48-72` — `runGit` wrapper for subprocess discipline; pattern reused by BlobStreamer.
- `internal/gitenv/` — env-strip and `WaitDelay` discipline; BlobStreamer's persistent subprocess uses this.
- `cmd/signatory/collectors.go:102+` — `collectorsFor` dispatch (commit 16 wires the new collector here, after gopublish).
- `cmd/signatory/analyze.go` — `--allow-fetch` flag plumbing (commit 16).
- `cmd/signatory/handoff.go:553-626` — `assembleProvenanceSignals` auto-routes the new signal into the handoff's `signals.publication.<type>` view; no template wire change needed beyond the prose additions.
- `templates/handoffs/provenance-review-v1.md` — analyst-facing prose additions (commit 17).
- `templates/handoffs/synthesis-v1.md` — synthesist-facing forgery-resistance note (commit 17).

## Notes on what changed

### v2 → v3

- **Network surface explicit (D11)**: three buckets — local-only always, proxy-via-gopublish, GitHub-only-with-`--allow-fetch`.
- **No fetch by default** (D1): missing-SHA preserves the row with `tag_sha_local_status: "missing_from_clone"` rather than fetching. The opt-in `--allow-fetch` flag re-enables fetch fallback when desired.
- **Tag-SHA pinning lives in gopublish** (D3): new compound `version_pin_table` signal in gopublish; source-evolution consumes via `VersionPinSource` interface. No proxy access from source-evolution.
- **Sequencing**: dispatch wires gopublish before source-evolution; source-evolution emits absence cleanly when pin table is unavailable.
- **Q5 resolved** (was open in v2).
- **Commit count: 15 → 18.** Added gopublish work as commit 1. Split `--allow-fetch` BlobStreamer behavior into its own commit (10). Added `pinsource.go` work as commit 8.
- **JSON shape**: `tag_sha_local_status` field added to row struct.
- **Validation strategy** (negative + positive cases) added as a section: 3 layers, real-module smoke tests including `hashicorp/go-retryablehttp` as the canonical-ancestor false-positive control.

### v1 → v2

- Source acquisition simplified from `git worktree add --detach <SHA>` per version to `git cat-file --batch` against the existing clone. No new clones, no worktrees, no temp-dir cleanup, no `git worktree prune` defer.
- AST analyzer interface changed from `Analyze(ctx, srcRoot string)` (directory walker) to `Analyze(ctx, files iter.Seq2[SourceFile, error])` (file-content iterator). AST unit tests are now pure in-memory; no git fixtures needed for the AST commits.
- Open question on worktree prune defer removed; nothing to prune.
