# Source-Evolution AST Analysis — Orientation & Lessons Learned

Status: written after bringing **pypi** to parity with **Go** (PR #143).
Audience: whoever adds the next ecosystem (npm / cargo / gem / …).

The single most important thing to internalize first:

> **The program already does resolution, isolated cloning, and
> provenance generically. A new ecosystem's AST support is a *leaf*
> that plugs into them. The genuinely new work is small and bounded.**

The recurring failure mode in this work was "discovering" existing
infrastructure mid-task and proposing spikes for already-solved
problems. Hold the map below and you won't.

---

## 1. How it fits together

`internal/signal/source` is **one** `signal.Collector`
(`collector.go`). Three ecosystem-agnostic layers sit underneath it
and you almost never touch them:

| Layer | Where | What it already does |
|---|---|---|
| **Resolution** | `internal/ecosystem/resolver/<eco>.go` | `ResolveSource(name) → DeclaredSource{URI,URL}` — pkg → declared source repo. Solved for npm/pypi/cargo/gem/maven. |
| **Acquisition** | `cmd/signatory/collectors.go` (`resolveClonePath` / `cloneToTempIsolated`) | Generic git clone of `entity.URL` into an isolated tempdir. `BlobStreamer` reads *any* clone. |
| **Provenance / pin** | the `version_pin_table` signal | Per-version `(version, sha, source, published_at)`. Consumed ecosystem-blind by `pinSourceImpl` / `pinTableFromSignals` in `pinsource.go` (matches `sig.Type == "version_pin_table"`, nothing language-specific). |

### The per-language seam

```
BlobStreamer ──filter──▶ astfeature.SourceFile stream
                              │
                              ▼
        LanguageAnalyzer.Analyze(ctx, files) ─▶ astfeature.Counts
                              │
                Assembler.Build(pinTable) ─▶ MatrixValue (per-version rows)
                              │
                   DetectAnomaly(rows) ─▶ source_evolution_anomaly
```

- **`LanguageAnalyzer`** (interface in `matrix.go`): `Analyze(...)` +
  `Language() string`. `golang.Analyzer` and `python.Analyzer`
  implement it. The Assembler is otherwise language-blind.
- **`astfeature`** package: the shared, language-neutral `Counts`
  (the per-version feature tally) and `SourceFile` (path+bytes). Every
  consumer (anomaly, matrix JSON, store, deltas) stays language-blind
  because everything funnels through `Counts`.
- **`BlobStreamer`** (`blobstream.go`): one persistent
  `git cat-file --batch` subprocess (ctx-bound — see caveats),
  `WithSourceFileFilter(func(path) bool)` selects which tree paths to
  stream. Default `isGoSourceFile`; pypi passes `isPythonSourceFile`.
- **`Collector.languageProfile(ecosystem)`** (`collector.go`):
  `ecosystem → (fileFilter, analyzer, ok)`. `ok=false` ⇒ skip
  silently. This is the gate.
- **`Assembler`** (`matrix.go`) + **budget** (`budget.go`): selects
  which versions to analyze (recency + major-leaf coverage, bounded)
  and emits rows newest-first.
- **`DetectAnomaly`** (`anomaly.go`): fires when ≥
  `MinSpikedFeatures` (=2) `Counts` fields cross **0 → non-zero**
  between adjacent versions. Spike-based and differential — see
  caveats.

---

## 2. Adding a new ecosystem — the checklist

Bounded. In rough order:

1. **Pin emission.** The ecosystem's *registry* collector must emit a
   `version_pin_table` signal in gopublish's exact JSON shape
   (`module_path`, `version_count_total/processed`, `pins[]` of
   `{version, sha, source, published_at}`, `missing_origin_versions`,
   `fetch_failed_versions`). Go: gopublish off proxy.golang.org.
   pypi: the pypi registry collector synthesizes it from the PEP 740
   attestation sweep it was *already running* (per-version Fulcio
   SHAs it had been discarding). **Look for SHAs the ecosystem's
   collector already fetches before adding new acquisition.**
2. **Analyzer package** `internal/signal/source/<lang>/`: an
   `Analyzer` implementing `LanguageAnalyzer`. Preserve
   `golang.Analyzer`'s error/ctx contract exactly (a mid-stream
   provider error aborts the row; ctx cancellation honored) so the
   Assembler treats all analyzers identically.
3. **File filter** `is<Lang>SourceFile(path)` in `blobstream.go` —
   the language's importable runtime source, excluding tests/vendored.
4. **`languageProfile` case** in `collector.go`.
5. **Dispatch gate** in `cmd/signatory/collectors.go` — add the
   ecosystem to the `entity.Ecosystem == … ||` guard that appends the
   source-evolution collector (it is appended LAST so the registry
   collector's pin table is already in the in-run result).
6. **`ecosystemForLanguage`** map in `matrix.go` (language → the
   ecosystem label stamped on the matrix).
7. **Counts fields.** Reuse shared fields where the analog is strong
   (network/exec/path-read/decode/xor). Add a new shared field *only*
   when a real attack fixture demands it — see schema decision below.

Steps 3–6 are ~5 lines each. Step 2 (the parser) is the only real
cost center.

### Schema-decision precedent (load-bearing, user-owned)

When a threat has no existing `Counts` analog, the established
pattern (don't re-litigate it each time, but surface it):

- **One new field on the shared `Counts`**, named **generically /
  cross-language** (`dynamic_eval_calls`, `import_time_call_sites`,
  `install_hook_overrides`) even if only one language populates it
  today. Other ecosystems leave it zero.
- Do **not** reuse a field whose name lies for the new language
  (reusing `init_count` for Python module-scope calls would mislabel
  the JSON — exactly the `go_loc`→`loc` wart we had to undo).
- Every new field must be wired in **three** places or the signal is
  half-built:
  1. `astfeature/counts.go` — field + doc.
  2. `anomaly.go` `spikedFeatures` — the 0→n crossing check
     (**without this the anomaly never fires for the new field**).
  3. `cmd/smoke-source-evolution/main.go` — the `matrixAST` mirror
     struct.

---

## 3. Test & dogfood procedures

**TDD is mandatory** (CLAUDE.md, red/green). For new API the red is a
compile failure; that's fine. Drive detection tests from **real
attack shapes**, not invented ones (project doctrine: validate the
signal model against real-world attacks).

Test layers, each its own red/green:

- **Lexer unit** (`<lang>/lexer_test.go`): the language's hard lexical
  cases (indentation, string prefixes, line-joining, the obfuscation
  operators). Assert the security-relevant property: *constructs
  inside strings/comments must not be tokenized as code.*
- **Parser unit**: scope discrimination (runs-at-import vs nested),
  call/​import shapes, static arg resolution.
- **Extractor unit** (`<lang>/analyze_test.go`): an adversarial
  fixture (e.g. `exec(base64.b64decode(...))` import-time payload)
  asserts each `Counts` field; a benign fixture asserts **zero**.
- **Collector integration** (`source/collector_test.go`): a
  clean→weaponized version progression via
  `initRepoWithVersionedProgression` + a `fakePinSource` fires
  `source_evolution_anomaly` and names the spiked features.
- **No-false-positive baseline**: a legit fixture/package scores the
  attack features 0.

Running:

- `go test ./internal/signal/source/...` for the fast loop.
- The **pre-commit hook** runs `gofmt`, `go vet`, and full
  `go test ./...` (includes the live `cmd/signatory` tests, ~35s).
  It does **not** run `golangci-lint` — run that yourself before a
  PR: `golangci-lint run ./internal/signal/source/...`.

**Live dogfood** (the step that catches what unit tests don't):

```
go run ./cmd/signatory analyze pkg:<eco>/<name> --refresh --clone -v
```

- A *legitimate, popular* package must score every attack feature 0
  and `anomaly_present=false`. This is the catalog-over-breadth
  smoke test — it caught `re.compile` being miscounted as
  `dynamic_eval`.
- A package with real history shows the matrix populated, rows
  pin-anchored, `ecosystem`/`language` correct.
- **Regression**: also dogfood a Go target (`pkg:golang/...`)
  `analyze` + `deltas` to prove the shared path didn't regress.
- `cmd/smoke-source-evolution` is the Go-only end-to-end driver
  (kong/go-retryablehttp baselines); not yet generalized per
  ecosystem.

Pick a dogfood target whose registry JSON is < 10 MiB
(`pydantic-core` blows the cap — large compiled wheel matrices).

---

## 4. Lessons learned & caveats

### Architecture

- **Version ordering is ecosystem-sensitive.** `budget.go:parseSemver`
  is deliberately Go-strict (`v`-prefix, 3 parts; its "invalid" tests
  are intentional). Bare PyPI / PEP 440 versions are all "invalid"
  there. The fix was an **ecosystem-neutral chronological axis**:
  order by the pin table's `PublishedAt`, semver only as the
  no-timestamp fallback. Any non-`v`-semver ecosystem inherits this
  for free *as long as its pins carry real `published_at`* — make
  sure step 1's pin emission populates it.
- **Anomaly needs rows newest-first.** `DetectAnomaly` walks
  `rows[i+1]`(older)→`rows[i]`(newer). The Assembler reorders, so in
  *tests* pin tables are written **oldest-first** and must carry
  `PublishedAt` for the publish-time path to engage. A test with
  bare versions and no timestamps silently falls back to semver and
  the anomaly won't fire — this bit us.
- **`import_time_call_sites` (and similar surface metrics) are
  naturally non-zero** for any real package (`logging.getLogger`
  at module top, etc.). Their value is the **spike**, never the
  absolute. Don't design thresholds on absolute values.

### Parsers

- **Build a security-relevant subset, not a conformant grammar.** A
  full CPython parser is enormous and unnecessary for trust signals.
  Scope to the threat catalog (imports, calls, scope, string args,
  class bases). State this explicitly in package docs so nobody
  "fixes" it into a real parser.
- **No third-party parser dependency.** A stale/unmaintained language
  parser is itself supply-chain risk *in a supply-chain tool*. Hand-
  write it; you may read another impl's logic for reference only.
- **Be lenient.** Malformed/adversarial input must yield a best-effort
  partial result, never abort the file. Conservative misses (false
  negatives) are acceptable; a benign construct that spikes a false
  anomaly is not.
- **Catalog matching: exact / dotted-suffix, never last-segment.**
  Matching `dynamic_eval` by last segment counted `re.compile` (regex,
  ubiquitous) as code-from-data execution — a false-positive that
  would trip an anomaly on the first regex a package adds. Match the
  bare builtin or an explicit qualified path; the specificity *is* the
  signal. Every catalog add must be paired with a legit-package
  dogfood proving 0.
- **Differential detectors hate noisy features.** We dropped the
  dynamic-dispatch (`getattr`/`globals`) signal entirely: it's
  pervasive in benign metaprogramming, so it rarely crosses 0→n and
  adds schema cost for marginal detection. "Cover everything" is the
  wrong instinct; cover what spikes *rarely and meaningfully*.

### Process / tooling

- **`go vet ./...` in the pre-commit hook walks on-disk packages**,
  including the gitignored `filestore/clones/*` left by `--clone`
  dogfoods. A foreign Go module with external test deps (e.g.
  `github.com/pkg/errors`) trips vet/build there. **Delete the
  dogfood clones you created before committing.** Don't touch the
  hook — it's CI config, not ours to bypass.
- **Splitting a feature across commits when changes interleave the
  same files**: no interactive `git add -p` here. Use
  *snapshot → strip → restore*: `cp` the mixed files aside, Edit out
  the upper layer, commit the lower layers, `cp` the snapshot back
  (byte-exact, zero reconstruction risk), commit the rest. Order
  commits by dependency so each tree is independently green
  (the hook enforces it).
- **Use Read/Edit/Write, not `sed`/`grep`/`cat` against files.** BSD
  `sed` on macOS has boundary quirks that silently miss; dedicated
  tools also keep intent legible in the transcript. Use the Explore
  agent for broad multi-file search.
- **`contextcheck`**: long-lived subprocesses (`git cat-file --batch`)
  must derive their context from the caller's ctx, not
  `context.Background()`, so an aborted collection tears them down.
- Apply **modernize** hints (`strings.SplitSeq`,
  `slices.ContainsFunc`) — repo targets Go 1.24+.

### Known conservative gaps (carry forward / document per language)

Each is a *false negative*, never a false positive:

- One-line bodies (`def f(): exec(x)`), default-arg calls in def
  headers, chained calls on expression results
  (`expr().b64decode()`), receiver-flow paths
  (`pathlib.Path(p).read_text()` — the path is `Path`'s arg).
- Fully dynamic values (f-strings with interpolation, `.format`, `%`,
  names, call results) are unresolved by the static arg resolver by
  design.
