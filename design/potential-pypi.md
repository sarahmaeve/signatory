# Potential: PyPI ecosystem provider — gap analysis vs. npm / Go / GitHub

Status: proposed. Captured 2026-04-24 during the v0.1 burndown scoping
conversation. The goal of this doc is to fully enumerate what's missing
for PyPI to reach parity with the shipped npm and Go providers, so the
eventual implementation plan starts with accurate scope.

Cross-references:
- [`npm-plan.txt`](npm-plan.txt) — the npm shipped-state doc and
  architecture template
- [`agent-facing-contract.md`](agent-facing-contract.md) §"M3" —
  pluggable resolver registry the new PyPI resolver plugs into
- [`v0.1-invariants.md`](v0.1-invariants.md) §Invariant 2 —
  collectors live under `internal/signal/registry/<eco>/`

## Methodology

This analysis is source-driven. I walked every point in the tree where
an ecosystem-specific branch exists (or should exist) and catalogued
what's present for npm + Go vs. what's missing for PyPI. Line-counts
are from the current working copy; see the matrix below.

The goal is NOT to produce a phased implementation plan. That's a
follow-up. This doc is the gap inventory — the "what needs to exist"
catalog that an implementation plan will phase.

## Current state: what's already in place for PyPI

**Scaffolding that exists but is non-functional.** These are the bits
that give the appearance of PyPI support but don't carry weight:

| File | Line(s) | What's there | Status |
|---|---|---|---|
| `internal/ecosystem/detect.go` | 23–25, 154–157, 184 | `EcosystemPyPI` const; `pyproject.toml`/`setup.py` in `manifestSignals`; PyPI in `priorityOrder`. | **Working** — `ecosystem.Detect()` correctly identifies a repo as PyPI from its root files. |
| `cmd/signatory/handoff.go` | 63, 308, 1280 | `--ecosystem=pypi` in the flag enum; provenance role accepts it; provenance template prose mentions PyPI. | **Partial** — flag plumbing works; downstream rendering works; no PyPI-specific security template. |
| `templates/handoffs/provenance-review-v1.md` | 528–535, 570–576 | PyPI-specific registry-URL and manifest guidance in the ECOSYSTEM-switch prose. | **Working** — handoff renders correctly for PyPI targets when `--ecosystem=pypi` is passed. |
| `internal/profile/target.go` | 53 (comment only) | `Ecosystem` field documents "npm, pypi, cargo, golang" as accepted values. | **Field works, no URL parser** — a `pkg:pypi/X` canonical URI parses correctly via `resolveCanonicalURI`, but there is no `parsePyPIURL` helper and `target_test.go:104` explicitly rejects `https://pypi.org/project/requests/` as an input. |
| `internal/profile/uri_test.go` | 17, 203, 327–329 | `pkg:pypi/requests` + `pkg:pypi/requests@2.31.0` covered by canonical-URI grammar tests. | **Working** — the URI shape is valid on the wire; nothing downstream knows what to do with it. |
| `internal/signal/types.go` | 127, 306 | `last_publish` and `maintainer_count` entries mention PyPI in their `Description`. | **Inherited** — signals are ecosystem-agnostic; these two would apply to PyPI once a collector emits them. |
| `internal/mcp/tools/survey.go` | 28, 37, 101–102 | PyPI appears in the tool description and in `detectEcosystemFromPath`. | **Advertising only** — returns `CodeNotFound` today (stub; see `design/potential-survey-mcp.md`). |

**What works end-to-end for a `pkg:pypi/X` target right now:**

- Canonical URI acceptance: `signatory analyze pkg:pypi/requests` runs
  `profile.ResolveTarget`, gets a valid `ResolvedTarget` with
  `Ecosystem="pypi"`.
- Storage: an entity at `pkg:pypi/requests` can be inserted into
  the SQLite store with correct fields.
- Posture set/get/accept and burn commands work against PyPI URIs
  (they're URI-shape-agnostic per M1's per-version identity work).
- Handoff dispatch with `--ecosystem=pypi` renders a handoff template
  that points the analyst at the right pypi.org endpoints and manifest
  files.

**What DOESN'T work:**

- `signatory analyze pkg:pypi/requests` cannot gather any registry
  signals — there is no PyPI collector.
- `signatory analyze https://pypi.org/project/requests/` is rejected
  at `ResolveTarget` (the URL parser has an npm-specific branch only).
- `pkg:pypi/requests` cannot be resolved to its source repository —
  there is no PyPI resolver in `internal/ecosystem/resolver/`.
- `signatory survey pyproject.toml` fails with "unrecognized manifest
  filename" — there is no PyPI manifest parser.
- `signatory handoff security --ecosystem=pypi --language=python` is
  served by `security-review-v1.md`, which IS the Python-specific
  template (despite the name) — this one works incidentally. See
  §"Template naming" below.
- `signatory show analyses` and other surfaces render correctly if
  data is in the store, but nothing puts data there for PyPI.

## Reference: shipped surfaces for npm and Go

For each layer, the file(s) and line counts that constitute a
complete ecosystem provider.

### Layer 1 — canonical URI acceptance

| Surface | npm | Go | PyPI |
|---|---|---|---|
| `pkg:<eco>/<name>` grammar | ✓ `internal/profile/uri.go` + `validURISchemes` | ✓ same | ✓ same (generic purl-shaped) |
| Ecosystem-native URL parser | ✓ `parseNpmjsURL` at `target.go:250` (65 LOC) | n/a — Go uses module paths, not pkg URLs | ✗ missing |
| URL rejection test guarding lookalikes | ✓ `target_test.go` npmjs.com lookalike cases | n/a | ✗ no positive tests; only rejection |

### Layer 2 — manifest detection

| Surface | npm | Go | PyPI |
|---|---|---|---|
| Signal-file table | ✓ `internal/ecosystem/detect.go:158` (`package.json`) | ✓ `detect.go:148` (`go.mod`) | ✓ `detect.go:154` (`pyproject.toml`, `setup.py`) |
| Priority ordering in polyglot | ✓ last position | ✓ first position | ✓ third position |
| Detect tests | ✓ | ✓ | ✗ no PyPI-specific fixture tests |

### Layer 3 — manifest parser (ecosystem-neutral `Dep` shape)

| Surface | npm | Go |
|---|---|---|
| Package | `internal/manifest/npm/` | `internal/manifest/gomod/` |
| Parser | `parse.go` — 285 LOC | `parse.go` — 290 LOC |
| Tests | `parse_test.go` — 437 LOC | `parse_test.go` — 362 LOC |
| Fixtures | `testdata/` (referenced) | n/a (uses inline data) |
| Formats handled | `package.json` + `package-lock.json` v2/v3 | `go.mod` |
| Direct vs indirect | Flatten deps+devDeps+peerDeps+optDeps; direct=true. Transitive from lockfile; direct=false. | `require` directives; `indirect` comment. |
| Local/non-registry specs | `file:`, `git:`, `github:`, `http(s):`, `npm:`, `workspace:`, `portal:`, `link:` → `ecosystem="npm-local"` + empty URI | `replace` directives → local path detected as `ecosystem="go-local-replace"` |
| Replace / override handling | n/a (lockfile carries resolved tree) | Yes — `replace` rewrites Name + CanonicalURI |
| Canonical URI construction | `pkg:npm/` + validated name (or empty on malformed) | `repo:github/owner/repo` if github.com/; else `pkg:go/<full-path>` |
| Graph extraction (for survey reachability) | ✗ `manifest.ErrGraphUnavailable` | ✓ `ParseGraph` via `go mod graph` subprocess |

**PyPI needs:** a whole `internal/manifest/pypi/` package. See §"PyPI-specific complications" for why this is larger than npm or Go.

### Layer 4 — registry client

| Surface | npm | Go | PyPI |
|---|---|---|---|
| Package | `internal/signal/registry/npm/` | n/a — Go uses offline path-prefix rules in the resolver (see Layer 6) | ✗ missing entirely |
| HTTP client | `client.go` — 382 LOC | — | ✗ |
| Response-body size cap | ✓ 10 MB registry, 64 KB downloads | — | ✗ |
| HTTPS-only redirect check | ✓ `checkRedirect` | — | ✗ |
| Name validation before URL construction | ✓ `ValidatePackageName` | — | ✗ |
| Error-body sanitization | ✓ (`#93`) | — | ✗ |
| Typed wire models | ✓ `RegistryPackage`, `DistTags`, `Maintainer`, `PackageVersion`, `NpmUser`, `Scripts`, `Dist`, `Repository` (with polymorphic JSON) | — | ✗ |

### Layer 5 — signal collector

| Surface | npm | PyPI |
|---|---|---|
| Package | `internal/signal/registry/npm/` | ✗ |
| Collector | `collector.go` — 488 LOC | ✗ |
| Tests | `collector_test.go` — 924 LOC | ✗ |
| Scheme-filtered (`pkg:npm/` only) | ✓ | ✗ |
| Shipped signals | 7: `last_publish`, `maintainer_count`, `postinstall_present`, `trusted_publishing`, `weekly_downloads`, `postinstall_introduced`, `publish_origin_consistency` | ✗ |
| Longitudinal window | ✓ 10-version window for axios-shape detection | ✗ |
| Registration in `signal/types.go` | ✓ all 7 entries with Group/ForgeryResistance/Caveats | `last_publish` and `maintainer_count` text mentions PyPI but no PyPI-specific signals |

### Layer 6 — identity / source resolver

| Surface | npm | Go | PyPI |
|---|---|---|---|
| File | `internal/ecosystem/resolver/npm.go` — 70 LOC | `internal/ecosystem/resolver/gomod.go` — 176 LOC | ✗ missing |
| Network-backed vs offline | Network — queries registry.npmjs.org for `repository.url` | Offline — hardcoded path-prefix rules (github.com/, golang.org/x/, gopkg.in/) | — |
| `init()` registration with `resolver.Default` | ✓ `Register("npm", NewNpmResolver())` | ✓ `Register("go", NewGoModResolver())` | ✗ |
| Tests | `npm_test.go` — 91 LOC | `gomod_test.go` — 179 LOC | ✗ |
| `NormalizeDeclaredRepoURL` equivalent | ✓ `internal/signal/registry/npm/resolve.go` — 106 LOC | n/a (paths are pre-canonical) | ✗ |

### Layer 7 — CLI collector dispatch

| Surface | Status | Location |
|---|---|---|
| `collectorsFor` (npm) | ✓ | `cmd/signatory/collectors.go:80` — `entity.Ecosystem == "npm"` adds `npmcollector.NewCollector()` |
| `collectorsFor` (pypi) | ✗ | No branch exists. The comment at :64–66 calls out "npm, pypi, ..." as the intended pattern — one line needed when collector ships. |

### Layer 8 — handoff templates

| Template | Covers | Lines | PyPI handling |
|---|---|---|---|
| `security-review-v1.md` | Python-specific (name is misleading — see §"Template naming") | 409 | ✓ full Python-specific pattern catalog |
| `security-review-go-v1.md` | Go-specific | 461 | n/a |
| `security-review-rust-v1.md` | Rust-specific | 436 | n/a |
| `security-review-generic-v1.md` | Fallback | 386 | Used for any language without a specific template |
| `provenance-review-v1.md` | All ecosystems (includes PyPI § prose) | 645 | ✓ PyPI registry endpoint + manifest list inlined |
| `synthesis-v1.md` | Ecosystem-agnostic | 284 | n/a |

### Layer 9 — survey integration

| Surface | npm | Go | PyPI |
|---|---|---|---|
| `parseManifest` dispatch | ✓ `survey.go:163` (`case "package.json"`) | ✓ `survey.go:161` (`case "go.mod"`) | ✗ falls into default — error |
| `parseGraph` dispatch | Partial — returns `ErrGraphUnavailable` as placeholder | ✓ runs `go mod graph` subprocess | ✗ not present |
| `manifest.Detect` table entry | ✓ `detect.go:38` | ✓ `detect.go:37` | ✗ not present (even though `ecosystem/detect.go` has it) |

### Layer 10 — test fixtures / end-to-end exercise

| Surface | npm | PyPI |
|---|---|---|
| Exchange testdata (analyst output fixtures) | `testdata/thefuck-*.json` — these ARE PyPI fixtures (thefuck is a Python project) | ✓ this one's actually present |
| Registry client fixtures | `collector_test.go` uses httptest.Server | ✗ |
| Manifest parser fixtures | `testdata/` | ✗ |
| Integration test against a real small PyPI package | ✗ (npm uses httptest) | ✗ |

## What's missing, organized by layer

This is the implementation-ready list. Each bullet is a concrete
deliverable a commit can close.

### Layer 1: Canonical URI acceptance

- **`parsePyPIURL` helper in `internal/profile/target.go`** (parallel
  to `parseNpmjsURL`). Must accept:
  - `https://pypi.org/project/<name>/` (→ `pkg:pypi/<name>`)
  - `https://pypi.org/project/<name>/<version>/` (→ `pkg:pypi/<name>@<version>`)
  - Variants: `http://`, `www.pypi.org`, trailing slash, query
    strings, fragments.
  - **Normalization per PEP 503** — `Requests`, `requests`,
    `requests-2` all normalize to `requests`. This is harder than
    npm (which is case-sensitive) — signatory needs to decide
    whether the canonical URI stores the normalized form (my
    recommendation: yes, always emit PEP-503 normalized) and
    whether the input form surfaces anywhere.
- **Host-anchoring against `pypi.org.attacker.com` lookalikes** —
  same trick as `parseNpmjsURL`.
- **Reject lookalike hosts in `target_test.go`** — current tests
  only cover the "reject all pypi.org" path; swap for full
  accept/reject coverage.

### Layer 2: Manifest detection

- No work needed at `ecosystem/detect.go` level — already shipped.
- **`internal/manifest/detect.go:33` needs two new candidate entries** —
  `pyproject.toml` and `setup.py` both mapping to `pypi`. First-match
  ordering: `pyproject.toml` wins over `setup.py` when both present.
  Also consider `requirements.txt` as a fallback when neither exists
  (see §"PyPI-specific complications" on requirements.txt identity).

### Layer 3: Manifest parser

**New package: `internal/manifest/pypi/`.** Dispatch entry point
parallel to `gomod.Parse` and `npm.Parse`. Internally, dispatch to
the format-specific sub-parser based on which input file the caller
passes. Must handle:

#### Primary formats (must ship for v0.1 parity)

- **`pyproject.toml` PEP 621 `[project]` table** — the modern
  standard. Deps in `[project.dependencies]` + `[project.optional-dependencies]`.
  Project name in `[project.name]`.
- **`pyproject.toml` Poetry legacy `[tool.poetry]` table** —
  still the majority of real-world Python projects. Deps under
  `[tool.poetry.dependencies]` (excluding `python = "^3.X"`
  which is the runtime pin, not a dep) and
  `[tool.poetry.dev-dependencies]` or `[tool.poetry.group.*.dependencies]`.
  Name in `[tool.poetry.name]`.
- **`requirements.txt`** — line-oriented. `name`, `name==version`,
  `name>=version`, `name[extras]==version`, `git+https://...`,
  `-e .`, `-r other-requirements.txt` (recursive), `--hash=sha256:...`
  (PEP 471), `# comments`, continuation lines. No project metadata;
  only deps. When used standalone (no pyproject.toml), survey
  treats it as a deps-only manifest.

#### Secondary formats (can defer; mark `ErrGraphUnavailable` shape)

- **`setup.py`** — legacy, Python source code. **Cannot be safely
  statically parsed** — executes arbitrary code at import time.
  Options: (a) skip entirely and hope for `pyproject.toml`
  fallback, (b) shell out to Python + `setup.py --name` /
  `--requires` if a Python interpreter is available. (b) is heavy
  and introduces a runtime dependency. Recommendation: **(a) skip
  setup.py parsing in v0.1**; detect its presence but return
  `ErrSetupPyUnparseable` and let the caller fall back.
- **`setup.cfg`** — legacy but statically parseable (INI format).
  Low priority — projects that still use setup.cfg usually have
  a companion `pyproject.toml` under modern tooling. Defer.

#### Lockfiles (transitive deps for survey)

Each Python package manager has its own lockfile format. The
`package-lock.json` parallel is fragmented. In rough priority order:

- **`poetry.lock`** — TOML, Poetry-specific. Widely used.
- **`uv.lock`** — TOML, Astral's uv. Fast-growing adoption.
- **`pdm.lock`** — TOML, PDM.
- **`Pipfile.lock`** — JSON, Pipenv. Declining but installed base.
- **`requirements.txt` with hashes** — not a true lockfile but
  functions as one when produced by `pip-compile` / `uv pip compile`.

Recommendation: ship `poetry.lock` + `uv.lock` first (covers the
modern majority). Add others as real projects demand them.

#### Canonical URI construction for PyPI

- Base: `pkg:pypi/<PEP-503-normalized-name>`
- With version: `pkg:pypi/<name>@<PEP-440-version>`
- **Name normalization is mandatory before URI emission** —
  otherwise `pkg:pypi/Requests` and `pkg:pypi/requests` and
  `pkg:pypi/python-dotenv` vs `pkg:pypi/python_dotenv` produce
  drift in the store. This is unlike npm (case-sensitive) and Go
  (verbatim paths).

#### Direct vs indirect

- Direct: anything listed in the manifest.
- Indirect: anything in the lockfile that isn't listed in the
  manifest. (Same shape as npm.)

#### Non-registry specs (→ `pypi-local` ecosystem slug)

- `git+https://`, `git+ssh://` — VCS installs
- `file:./path`, `-e .`, `-e ./subdir` — local installs
- URL-to-wheel/sdist — `https://example.com/foo-1.0.whl`
- `@` path specifications (PEP 508 URL form) —
  `requests @ git+https://github.com/psf/requests.git`

Mark all of the above with `Ecosystem="pypi-local"` and an empty
`CanonicalURI`, matching the `npm-local` and `go-local-replace`
convention.

#### Graph extraction

**Hard.** Python has no `go mod graph` equivalent that works
without a full Python environment. Options:

- Skip for v0.1 — return `ErrGraphUnavailable`. Same as npm
  currently does. Survey's reachability-bucket rendering falls
  back gracefully.
- Require the user to produce the graph ahead of time — parse
  `poetry.lock` / `uv.lock` which DO carry a resolved tree with
  parent-child edges. Shippable; format-specific. Modest LOC per
  format.

Recommendation: **skip for initial PyPI cut (`ErrGraphUnavailable`);
add poetry.lock graph extraction as Phase C** (after npm's own
graph extraction lands, so the idiom is set).

### Layer 4: Registry client

**New package: `internal/signal/registry/pypi/client.go`.** Mirror
the npm client's defenses:

- `Client` + `NewClient()` + `NewClientWithBaseURL(base)` for tests.
- 60-second timeout.
- `checkRedirect` — HTTPS-only, <10 hops.
- `ValidatePackageName` using PEP 508 name grammar:
  `^([A-Z0-9]|[A-Z0-9][A-Z0-9._-]*[A-Z0-9])$` (case-insensitive).
  **Must normalize before lookup** — the registry canonicalizes
  names itself, so `REQUESTS` and `requests` return the same
  response, but we want determinism in our own URL construction.
- Response-body size cap. PyPI's JSON response for a popular package
  (e.g. `boto3` with thousands of releases) can be 100+ MB. Choose
  a higher cap than npm's 10 MB — recommend 50 MB with a warning
  log when the cap is approached.
- Error-body sanitization.

**Endpoints to model:**

- `https://pypi.org/pypi/<name>/json` — legacy but fully functional
  JSON API. Returns project-level metadata + all releases in one
  payload. The primary read surface.
- `https://pypi.org/pypi/<name>/<version>/json` — per-release
  metadata. Narrower payload per release.
- `https://pypi.org/simple/<name>/` (PEP 691 JSON) — the newer
  "simple" repository API. Has attestations (PEP 740) that the
  legacy JSON API lacks. Needed for `trusted_publishing` signal.
- Download stats: **NOT via pypi.org** — PyPI exposes these only
  via BigQuery on the public dataset. For an HTTP-queryable
  source, use `https://pypistats.org/api/packages/<name>/recent`
  (third-party, free, but not first-party). Alternative: skip the
  `weekly_downloads` signal for PyPI in v0.1; surface absence.

**Typed wire models:**

- `Project` (mirror of `RegistryPackage`)
  - `info.name`, `info.version` (= latest)
  - `info.author`, `info.author_email`
  - `info.maintainer`, `info.maintainer_email`
  - `info.project_urls` (map — can contain repository URL under
    various keys: "Repository", "Source", "Source Code", "Homepage",
    "Bug Tracker", etc.) — the `repository.url` equivalent is here.
  - `info.license`, `info.classifiers` (license-in-classifier form)
  - `info.requires_python`, `info.requires_dist` (deps as strings)
  - `releases` (map: version → [`Distribution`])
  - `urls` (latest release's distributions)
- `Distribution` (per-file metadata — sdists + wheels)
  - `upload_time`, `upload_time_iso_8601`
  - `digests` (sha256, md5, blake2b)
  - `filename`, `packagetype` ("sdist" or "bdist_wheel")
  - `size`
  - `yanked`, `yanked_reason` — **important for a
    `yanked_releases` signal not present in npm**.

**PyPI-specific signal opportunities** (things the npm collector
has no equivalent for):

- `yanked_releases` — PyPI's yank mechanism lets maintainers mark
  a release as "do not install" without deleting it. Presence is
  a non-trivial signal about release discipline.
- `sdist_vs_wheel` — presence of source distribution alongside
  wheels. Some supply-chain attacks ship only a malicious wheel
  (harder to inspect) without a matching sdist.
- `author_email_domain` vs `project_urls.Repository` domain — a
  mismatch (gmail.com author, example.corp repo) is a governance
  signal.

### Layer 5: Signal collector

**New package: `internal/signal/registry/pypi/collector.go`.** Same
contract as npm: implements `signal.Collector`, scheme-filtered to
`pkg:pypi/*`, emits signals via `signal.CollectionResult`.

**Signals to ship in the initial cut** (picking directly-analogous
ones from the npm set):

| Signal | npm equivalent | PyPI sourcing |
|---|---|---|
| `last_publish` | ✓ ships | `info.version` + `urls[0].upload_time` of latest release |
| `maintainer_count` | ✓ ships | `info.author` + `info.maintainer` (note: PyPI's maintainer model is weaker than npm's — see below) |
| `weekly_downloads` | ✓ ships | `pypistats.org/api/packages/<name>/recent` (third-party; or defer) |
| `trusted_publishing` | ✓ ships | PEP 740 attestation presence via `/simple/<name>/` (NOT via the legacy JSON API) |
| `release_integrity` | partial (dist integrity) | `urls[].digests` + `yanked` flag per release |

**Longitudinal signals** (Phase B.6 equivalent):

| Signal | npm | PyPI |
|---|---|---|
| `postinstall_introduced` | npm-specific (lifecycle scripts) | **No PyPI equivalent at the wire level.** Python has setup.py's arbitrary code execution — any sdist can run anything on install — but it's not declaratively flagged like npm's `postinstall`. A signal analog would require unpacking the sdist and inspecting setup.py, which crosses the collector/analyst boundary. **Recommend: defer. Analyst territory.** |
| `publish_origin_consistency` | cross-version `_npmUser` stability | PyPI exposes no publisher-per-release field in the JSON API. The `urls[].uploaded_by` field does exist on some releases but coverage is spotty. **Recommend: defer until we've surveyed 10+ real PyPI projects to see how populated this is.** |

**PyPI-unique signals worth shipping:**

- `yanked_release_count` — count of yanked versions in the last N.
  Non-zero is a governance discipline signal.
- `sdist_present` — per-version, presence of an sdist alongside
  wheels. Long-term pattern of "wheels only" is a publish-hygiene
  signal.
- `sdist_wheel_size_ratio` — sudden divergence can indicate a
  wheel carrying artifacts not in the sdist (a known supply-chain
  pattern).

All new signal types MUST be added to
`internal/signal/types.go`'s `signalTypeRegistry` with
Group / ForgeryResistance / Description / Caveats populated, or
`signal.Make` will panic at emit time.

### Layer 6: Identity / source resolver

**New file: `internal/ecosystem/resolver/pypi.go`.** Structurally
parallel to `npm.go` — network-backed resolver that reads a field
from the registry and normalizes it.

**Complication:** PyPI's `project_urls` is a free-form map. The
repository URL can live under:

- `Repository` (PEP 621 canonical)
- `Source`, `Source Code`, `SourceCode`, `source`
- `Homepage` (sometimes — when the project's only URL is a GitHub
  repo)
- `Code`, `GitHub`, `Repo`

Resolver needs a priority-ordered key lookup across all of these,
plus a fallback to `info.home_page` (deprecated field, still
populated on older releases). Closest analog: npm's polymorphic
`Repository` unmarshaler, but with more shape variation.

**Normalization** (similar to npm's `NormalizeDeclaredRepoURL`):

- Strip `git+` prefix
- Handle `git://` (reject per the same policy as npm)
- Handle `ssh://git@github.com/...` → `https://github.com/...`
- Strip `#fragment`
- Drop `.git` suffix
- Delegate to `profile.ResolveTarget` for the github URL grammar

`init()` registers `pypi` with `resolver.Default`.

### Layer 7: CLI dispatch

**One-line addition to `cmd/signatory/collectors.go:80`:**

```go
case "pypi":
    collectors = append(collectors, pypicollector.NewCollector())
```

Plus an import.

**`cmd/signatory/handoff.go` already has `pypi` in the enum** at
line 63. No flag work needed.

### Layer 8: Handoff templates

#### Template naming issue (flag inside this doc)

`templates/handoffs/security-review-v1.md` is the **Python-specific**
security template, but its filename has no "python" marker. Other
language-specific templates are correctly named
(`security-review-go-v1.md`, `security-review-rust-v1.md`, plus
`security-review-generic-v1.md`). The dispatch in
`cmd/signatory/handoff.go:1306–1307` is:

```go
case "python":
    return "handoffs/security-review-v1.md"
```

Recommendation: **rename `security-review-v1.md` →
`security-review-python-v1.md`** as a standalone fix (it's a
consistency bug regardless of the PyPI arc). Add the same rename
to this doc's prerequisite list.

#### Provenance template

`provenance-review-v1.md` already covers PyPI in its
ECOSYSTEM-switch prose. **Need to verify** (once a collector ships)
that the Layer-1 signals block from `ImproveProvSignals` Phase 1
renders the PyPI-specific signals correctly. Likely no template
change, but a render test against a seeded PyPI entity should be
added.

### Layer 9: Survey integration

Three one-line additions to `internal/survey/survey.go`:

- `parseManifest` dispatch for `pyproject.toml`, `setup.py`, and
  optionally `requirements.txt`.
- `parseGraph` dispatch to `pypi.ParseGraph` (which returns
  `ErrGraphUnavailable` in the initial cut, or parses poetry.lock
  if Phase C ships).

**`internal/mcp/tools/survey.go` is stubbed** — no PyPI-specific
work needed there until the MCP survey wiring lands (see
`design/potential-survey-mcp.md`).

### Layer 10: Tests and fixtures

- `internal/manifest/pypi/testdata/` — sample `pyproject.toml`
  (PEP 621 + Poetry variants), sample `poetry.lock`, sample
  `requirements.txt`.
- `internal/signal/registry/pypi/` — httptest-backed client and
  collector tests (mirror npm's scope).
- `internal/ecosystem/resolver/pypi_test.go` — mock client + assert
  resolution across the `project_urls` key variations.
- Integration test target: pick one small real PyPI package
  (e.g., `python-dotenv`, `semver`, or `click`) for a recorded-fixture
  end-to-end test. Consider going the other way and using `thefuck`
  since we have its analyst output already in
  `internal/exchange/testdata/`.

## PyPI-specific complications (things that DON'T map 1:1 from npm)

These are the issues that make PyPI genuinely harder than npm or Go,
not just "more of the same." Each has implementation implications
that the phased plan should address explicitly.

### 1. Format sprawl

npm has one manifest (`package.json`) and one lockfile
(`package-lock.json`). Go has one (`go.mod`). PyPI has:

- Manifest formats: `pyproject.toml` (PEP 621), `pyproject.toml`
  (Poetry legacy), `setup.py`, `setup.cfg`, `requirements.txt`
  (standalone).
- Lockfile formats: `poetry.lock`, `uv.lock`, `pdm.lock`,
  `Pipfile.lock`, pinned `requirements.txt`.

Implication: `internal/manifest/pypi/` will be multiple files
(one per format), plus a dispatch layer. Larger than npm's
`parse.go`.

### 2. Name normalization (PEP 503)

PyPI normalizes package names for lookup:

- Case-insensitive
- `_`, `-`, and `.` are equivalent
- Repeated runs collapse: `python__dotenv` == `python-dotenv`

Implication: the canonical URI for a PyPI package MUST use the
normalized form. Storage and query correctness both depend on it.
A test fixture set with mixed cases is mandatory.

### 3. Versioning (PEP 440)

PEP 440 has:

- Pre-release markers: `1.0.0a1`, `1.0.0b2`, `1.0.0rc3`
- Post-release: `1.0.0.post1`
- Dev release: `1.0.0.dev1`
- Epoch: `1!1.0.0`
- Local version identifier: `1.0.0+ubuntu1`

Implication: version parsing and comparison is harder than semver.
For v0.1, treat versions as opaque strings (same as the current
`manifest.Dep.Version` contract) and let downstream consumers
parse when needed. No normalization pass.

### 4. Setup.py arbitrary code execution

`setup.py` is Python source. It can `import requests`, read
`os.environ`, or hit the network during install. A static parser
can't get deterministic dep lists from it. 

Implication: **skip `setup.py` in v0.1.** Detect its presence,
emit a warning, and require `pyproject.toml` or `requirements.txt`
for dep enumeration. Flag this clearly in the user-facing error.

### 5. Maintainer model is weaker than npm's

npm has a first-class `maintainers` list per package, with
logins the registry displays and revokes against. PyPI has:

- `info.author` + `info.author_email` (free-form strings)
- `info.maintainer` + `info.maintainer_email` (same)
- `info.classifiers` may contain `Development Status :: ...`

No registry-backed account list. Maintainer identity for a PyPI
package is much weaker than for an npm package.

Implication: `maintainer_count` for PyPI is best-effort (count
of distinct email/name pairs across recent releases) and its
`ForgeryResistance` rating should be lower than npm's. Document
this clearly in the `signal/types.go` caveats.

### 6. Publisher identity is not per-release (usually)

npm records `_npmUser` on every version — the account that ran
`npm publish`. PyPI's `urls[].uploaded_by` field is present on
some releases but coverage is inconsistent (older releases
predate it; some projects' uploads never record it).

Implication: `publish_origin_consistency` as shipped in npm B.6
has no reliable PyPI equivalent. **Defer this signal for PyPI**
until we've surveyed real-world coverage.

### 7. Downloads stats aren't first-party

npm serves stats at `api.npmjs.org/downloads`. PyPI's stats live
in BigQuery. Third-party mirrors (pypistats.org) are common but
not authoritative. 

Implication: for v0.1, either (a) use pypistats.org and document
the third-party dependency, or (b) omit the `weekly_downloads`
signal for PyPI and surface absence. Both are defensible; my
lean is (b) for v0.1 so we don't add a new third-party host to
the trust boundary — introduce (a) in v0.1.1 or v0.2 if users
ask.

### 8. Trusted publishing (PEP 740) lives in the simple index

npm's `dist.attestations` is in the main JSON response. PyPI's
PEP 740 attestations are under `/simple/<name>/` (PEP 691 JSON),
NOT `/pypi/<name>/json`. The collector needs to hit both endpoints
for a package that might have attestations.

Implication: two HTTP calls per PyPI collect, not one. Still fast
— both endpoints are cached heavily.

### 9. No `go mod graph` equivalent

Python offers no way to produce a dependency graph without a Python
environment. `pip install --dry-run --report=json` requires `pip`
and a usable Python interpreter. `uv pip compile` requires `uv`.

Implication: graph extraction is only possible from an already-
resolved lockfile (poetry.lock etc.). Projects without a lockfile
get `ErrGraphUnavailable` — survey's reachability rendering
degrades but still works.

### 10. Name collisions with stdlib and built-ins

PyPI packages can be named identically to Python stdlib modules
(`json`, `os`, `http`) or to common type names. This enables
dependency-confusion attacks where a malicious `json` on PyPI
shadows a non-existent internal `json` package. signatory
should at minimum flag any dependency whose name appears in a
known Python stdlib list as a "name-shadows-stdlib" signal —
same shape as `postinstall_present` for npm.

Implication: a new signal type (`pypi_stdlib_shadow` or similar)
that fires when the package name is a known stdlib module.
Small, high-value, PyPI-specific.

## Proposed phasing (not a schedule; a dependency order)

Mirroring the npm A/B/C shape, applied to PyPI's complications:

- **Phase A: Foundation.** Canonical URI acceptance (`parsePyPIURL`),
  manifest detection entry, `pyproject.toml` PEP 621 parser,
  `pkg:pypi/` → source resolver (without network — offline
  path-rule resolver? no, PyPI has no hardcoded-path pattern like
  Go's, so this IS network-backed), CLI dispatch wire-up,
  `last_publish` + `maintainer_count` signals only.
- **Phase B: Registry depth.** Poetry legacy `pyproject.toml`
  parser, `requirements.txt` parser, `trusted_publishing` signal
  (PEP 740 via simple API), `yanked_release_count`,
  `sdist_present`. Decide on `weekly_downloads` via pypistats or
  defer.
- **Phase B.6: Longitudinal (deferred decision).** Evaluate
  whether PyPI's `urls[].uploaded_by` field is populated enough
  across real targets to support a `publish_origin_consistency`
  analog. If yes, ship. If no, document the gap and skip.
- **Phase C: Lockfile + graph.** `poetry.lock` parser + graph
  extraction for reachability buckets. `uv.lock` follows. Other
  lockfiles (`pdm.lock`, `Pipfile.lock`) ship as targets demand.
- **Phase D: PyPI-unique signals.** `pypi_stdlib_shadow`,
  `sdist_wheel_size_ratio`, `author_email_domain_match`. These
  don't exist for npm — they're genuine value-add from PyPI's
  attack-surface specifics.

Phases are independent-reviewable commits. Each phase's test suite
green is the gate for the next.

## Out of scope for initial PyPI cut

- `setup.py` dep enumeration (Python subprocess; heavy; risky).
- `setup.cfg` parser (declining format).
- `Pipfile.lock` parser (declining format).
- PyPI-side federated burn list integration (v0.2+).
- Cross-ecosystem name-collision detection (npm `express` vs PyPI
  `express`) — v0.2+ correlation work.
- Windows-specific entry_point scripts analysis — analyst territory,
  not collector.
- PyPI's `.well-known` attestation lookups if PEP 740 gets new
  endpoints — iterate as the PEPs finalize.

## Rough scope estimate

Based on the npm shipped LOC as a calibration:

| Piece | npm LOC (code + tests) | PyPI estimate |
|---|---|---|
| Manifest parser | 722 | ~1,200 (more formats) |
| Registry client | 969 | ~700 (fewer endpoints; more careful JSON shape) |
| Signal collector | 1,412 | ~1,000 (fewer longitudinal signals initially) |
| Source resolver | 161 | ~200 (more `project_urls` variation) |
| CLI dispatch + survey | ~10 | ~10 |
| Signal-type registry entries | 7 entries | 5-7 new entries |
| Testdata fixtures | small | modest |
| **Total** | **~3,300 LOC** | **~3,100 LOC** |

Not a smaller change than npm was; just a differently-shaped one.

## Recommended next steps if we schedule this

1. Fix the `security-review-v1.md` → `security-review-python-v1.md`
   rename as a standalone commit (consistency bug; unblocks nothing
   but removes ambiguity while we plan).
2. Write the Phase-A-scoped implementation plan as a sibling doc
   (`design/pypi-plan.txt`, structural parallel to `npm-plan.txt`).
3. TDD the Phase A stack: `parsePyPIURL` first (single-file change;
   regression-free), then manifest parser, then client + collector,
   then resolver, then CLI wire-up. Each commit individually testable
   against public PyPI.
4. Target package for the end-to-end verification run: `python-dotenv`
   (small, well-known, has a clean `pyproject.toml`, has active
   releases, single maintainer → exercises the thin-maintainer case).

## References

- `internal/signal/registry/npm/` — the reference implementation
  this plan mirrors.
- `internal/manifest/npm/parse.go` — the manifest-parser shape,
  including lockfile handling and non-registry spec classification.
- `internal/ecosystem/resolver/npm.go` — the resolver shape for
  network-backed source resolution.
- `internal/profile/target.go:250-313` — `parseNpmjsURL` as the
  model for `parsePyPIURL`.
- `design/npm-plan.txt` — the "shipped state" template the eventual
  `pypi-plan.txt` should follow.
- [PEP 503](https://peps.python.org/pep-0503/) — name normalization.
- [PEP 440](https://peps.python.org/pep-0440/) — version scheme.
- [PEP 621](https://peps.python.org/pep-0621/) — pyproject.toml
  project metadata.
- [PEP 691](https://peps.python.org/pep-0691/) — JSON simple index.
- [PEP 740](https://peps.python.org/pep-0740/) — attestations via
  simple index.
