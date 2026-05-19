# Source-Evolution AST Analysis — Orientation & Lessons Learned

Status: written after pypi→Go parity (PR #143); updated after **npm**
parity + the threat-landscape signal expansion (branch `npm-ast`).
Audience: whoever adds the next ecosystem (cargo / gem / maven / …).

Done so far: **Go**, **pypi**, **npm/TypeScript**. The smoke driver
is now ecosystem-agnostic. If you are adding cargo/gem, the pattern
below is fully worn in — read §4's npm lessons first, they are the
freshest and the most transferable.

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
  `Language() string`. `golang.Analyzer`, `python.Analyzer`, and
  `node.Analyzer` implement it. The Assembler is otherwise
  language-blind. `node` covers JS **and** TypeScript — one analyzer,
  `Language() == "javascript"`, `ecosystemForLanguage` maps it to
  `npm`.
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
   SHAs it had been discarding). npm: the packument the npm registry
   collector *already fetches* carries `versions[v].gitHead`
   per-version — zero new acquisition for the base table. **Look for
   SHAs the ecosystem's collector already fetches before adding new
   acquisition.** npm is the strongest case of this: the SHA was
   sitting in a field already parsed for `artifact_url`.
   - **Provenance-strength labelling.** npm pins are stamped
     `npm-gitHead` (publisher-asserted, low forgery resistance) and
     upgraded to `npm-attestation` (Fulcio source-repo-digest, medium)
     when a provenance attestation exists for that version — a bounded
     extra fetch over the recent window only. The `source` field on
     each pin records which, so an analyst reads provenance strength
     off the row. Reuse this two-tier pattern for any ecosystem whose
     cheap SHA is weaker than its attested SHA.
   - **Trust boundary (load-bearing).** A registry-supplied SHA is
     attacker-controlled and flows verbatim into `git ls-tree` /
     `cat-file` / `diff` argv. It MUST pass
     `fulcio.IsGitObjectID` at emission (npm gitHead is gated exactly
     like the pypi Fulcio value). The cert→OID→DER-unwrap→git-argv
     gate now lives once in **`internal/sigstore/fulcio`**; pypi and
     npm both consume it — do not copy it a third time.
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
  `install_hook_overrides`, and from the npm work
  `env_credential_reads`, `sensitive_path_writes`,
  `cloud_metadata_calls`) even if only one language populates it
  today. Other ecosystems leave it zero.
- Do **not** reuse a field whose name lies for the new language
  (reusing `init_count` for Python module-scope calls would mislabel
  the JSON — exactly the `go_loc`→`loc` wart we had to undo).
- Every new `Counts` field must be wired in **three** places or the
  signal is half-built:
  1. `astfeature/counts.go` — field + doc.
  2. `anomaly.go` `spikedFeatures` — the 0→n crossing check
     (**without this the anomaly never fires for the new field**).
  3. `cmd/smoke-source-evolution/main.go` — the `matrixAST` mirror
     struct. (The smoke driver is now ecosystem-agnostic, but this
     mirror is still hand-maintained — a missing field here makes the
     end-to-end JSON assertion silently ignore it.)
- **The fixture must come first, and from a real incident.** The
  three npm fields were each demanded by a named-incident fixture
  (env-cred → TanStack/litellm; persistence-write → node-ipc/
  bufferzonecorp; cloud-metadata → TanStack IMDS). Process worth
  repeating per ecosystem: read `design/threat-landscape`, turn each
  *mechanically-observable* technique into a red fixture (true
  positive + benign twin scoring zero), and let the failing fixture
  justify the field. Do not add a field speculatively.
- **New *signal types* (registry side) have their own "or it's
  half-built" rule:** a new `result.RecordSignal(type, …)` panics at
  runtime unless `type` is registered in
  **`internal/signal/types.go`** (group + forgery resistance +
  caveats). This bit us on `maintainer_email_set`. The npm
  attestation signals (`latest_attestation_builder`,
  `attestation_consistency`) deliberately reuse pypi's *already-
  registered* type names so the store schema and synthesis treat
  ecosystems uniformly — emit the same shape, register nothing new.

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
- **Regression**: any change to the shared `Counts` struct or the
  shared assembler/anomaly path must be dogfooded on **all** live
  ecosystems — `pkg:golang/…`, `pkg:pypi/…`, **and** `pkg:npm/…` —
  `analyze` + `deltas`, to prove the additive change didn't regress
  the others (a new `Counts` field must show present-and-zero on the
  ecosystems that don't populate it). The 3-field npm expansion was
  validated this way on fresh targets (chalk, google/uuid,
  packaging).
- **npm provenance**: pick a target you *know* publishes with npm
  provenance (e.g. `pkg:npm/tuf-js`, `pkg:npm/sigstore`) to exercise
  `attestation_consistency` / `latest_attestation_builder` against
  real Fulcio certs — a healthy package must show `consistent=true`,
  `transition_detected=false`. A `npm-gitHead`-only package (e.g.
  `chalk`) correctly emits **no** attestation signal: that silence
  is correct, not a bug.
- `cmd/smoke-source-evolution` is now **ecosystem-agnostic**:
  `profileForTarget` derives an `ecosystemProfile` from the target's
  purl prefix (`pkg:golang/`, `pkg:npm/`, `pkg:pypi/`) carrying the
  per-ecosystem expectations — matrix ecosystem/language, accepted
  pin sources, accepted SHA hex lengths (SHA-1 or, for Fulcio
  digests, SHA-256), and which count invariants apply (the gopublish
  processed-cap and bucket-exhaustion are **Go-only**; npm/pypi have
  processed-but-not-pinned versions by construction). Run it on a
  small target of any supported ecosystem:
  `go run ./cmd/smoke-source-evolution -target pkg:npm/ms`. A new
  ecosystem adds one `profileForTarget` case, not a new driver.

Pick a dogfood target whose registry JSON is < 10 MiB
(`pydantic-core` blows the cap — large compiled wheel matrices).

---

## 4. Lessons learned & caveats

### npm / TypeScript (newest — most transferable to cargo/gem)

- **The map held.** The §1 claim ("resolution, acquisition, provenance
  are generic; the new work is a bounded leaf") was true again. The
  hand-written JS/TS lexer+parser was ~90% of the effort; the wiring
  (`languageProfile`, `ecosystemForLanguage`, file filter, dispatch
  gate) was the predicted ~5 lines each. Do not spike infra you think
  is missing — re-read §1 instead.
- **Port `robustness_test.go` *first*, and bound every self-recursive
  scan.** Go/python had `maxArgScanTokens` / `maxResolveDepth`. node's
  `scanTemplate` recursed through nested `` `${`…`}` `` with **no**
  cap — a ~3M-level file under the 10 MiB BlobStreamer cap
  stack-overflows and aborts the whole collection (a DoS *and* an
  evasion). The robustness test (parity with python's) caught it
  before any real input did; fix = `maxTemplateDepth`, over the cap a
  nested backtick is an ordinary byte (bounded, conservative, never a
  false call). For any new hand-written parser: write the adversarial
  test first, then prove every recursion has a bound.
- **JS lexical hazards, decided once:** regex-literal-vs-division by
  previous-significant-token (erring toward division is *not* always
  safe — `/eval(x)/` mis-lexed as division would surface a false
  call; the standard heuristic handles the common forms and the
  residual is documented). Template **and** regex literals are
  *opaque* tokens — a call/keyword spelled inside one must never
  tokenize; this is the security property the lexer test asserts
  end-to-end. **TypeScript: do not model the type system.** Lex type
  syntax leniently, let the parser ignore it. Modelling types buys
  zero trust signal and `<T>`-vs-JSX is a tar pit. Say so in the
  package doc so nobody "fixes" it.
- **`NAME(params){` is a declaration, not a call** — the JS analog of
  python's def-header skip. Without it every function/method
  definition inflates `import_time_call_sites` and records phantom
  callees. Disambiguator: a real call's `)` is followed by
  `;`/`.`/`)`/`,`; a definition's by `{` (an arrow body's `{` is
  *inside* the parens). A whole false-positive class, caught only by
  the benign-baseline fixture — every language needs its equivalent
  of this skip.
- **Alias/receiver resolution is higher-value in JS than python.**
  `const cp = require('child_process'); cp.exec()` and the inline
  `require('cp').exec()` chain are the *dominant* shapes, not edges.
  Decide alias handling by how pervasive aliasing is in the language's
  idiom, up front. Bare receiver-flow (`const e = process.env; e.X`)
  is a deliberate documented gap.
- **Some intent lives in argument 2.** `Buffer.from(x,'base64')` is
  THE npm decode primitive but, unlike python's `base64.b64decode`
  (a plain callee), the decode intent is the *second* arg. A
  callee-name catalog is not always sufficient; `resolveArgN` + a
  value catalog is the fix — added only because the event-stream
  fixture demanded it.
- **Extract trust-boundary code before copying it.** The
  cert→OID→git-argv gate moved to `internal/sigstore/fulcio` so pypi
  and npm share one audited copy. npm extended it
  (`ExtractBuilderIdentity`, GHA OIDC OIDs) with a *different* gate —
  `printable-safe`, not `IsGitObjectID`, because builder URIs flow
  into persisted JSON, not a git argv. Keep the gate matched to where
  the value flows.
- **Prefer a sibling ecosystem's registered signal shape over a new
  one.** npm emits pypi's exact `attestation_consistency` /
  `latest_attestation_builder` — no new registered types, no synthesis
  change, ecosystem-uniform store. Only `maintainer_email_set` was
  genuinely new (and hashed: PII discipline — store change-detection,
  never the raw value; always-emit so first appearance is a diffable
  transition, not a missing-signal ambiguity).
- **Test-staleness from success: repurpose, never silence.** Adding
  npm as *supported* invalidated "npm skips source-evolution" tests;
  an always-emitted signal invalidated exact-count assertions. The
  premises were broken *by the feature working*. Faithful fixes:
  point the unsupported-skip test at a still-unsupported ecosystem
  (cargo), flip the dispatch assertion mirroring the existing pypi
  flipped-assertion precedent, bump counts with an honest comment.
- **Bucket every threat as AST / registry / both / neither before
  promising coverage.** The `design/threat-landscape` review drove
  the 3 new `Counts` fields *and* honestly scoped out what these
  methods structurally cannot see: CI/workflow posture, OIDC-from-
  process-memory, identity clusters / cross-ecosystem correlation,
  tarball-vs-git divergence (a *third* collector, not this one).
  Write the "neither" list down so it isn't re-litigated.

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

**node / JS-TS specifically** (each a false negative, never a false
positive):

- `=> expr` arrow bodies (no brace) count as module scope — inflates
  the `import_time_call_sites` *spike metric* only (absolute is not
  load-bearing per §4 Architecture), so accepted.
- Variable-bound receiver-flow: `const e = process.env; e.NPM_TOKEN`
  and `const w = fs.writeFile; w('~/.ssh/authorized_keys')` are
  missed — only the direct `process.env.X` / `fs.writeFile(...)`
  shapes resolve. Same conservative posture as python's `pathlib`
  receiver gap.
- Chained calls on expression results (`getThing().exec()`) — the
  `.exec` is recorded as `.exec` (dot-prefixed) and so never matches
  the bare/qualified catalogs. Shared with python.
- Code inside template-literal `${ }` interpolations is not
  tokenized (the lexer treats the whole template as opaque), and
  template nesting past `maxTemplateDepth` collapses to one opaque
  string. Both are deliberate bounded misses.
- Whole-environment capture (`{...process.env}`, passing `process.env`
  to a `child_process` spawn) is **not** counted — it is pervasive in
  benign code, so only a *named*, catalog-matched env read is the
  signal. A payload that captures the whole env and filters names at
  runtime is a documented miss (the alternative is a false-positive
  flood).
- `InitCount` / `InstallHookOverrides` stay 0 for node by design:
  npm install hooks are `package.json` scripts, already covered by
  the npm registry collector's `postinstall_*` signals — counting a
  source construct here would double-report and mislabel the vector.
