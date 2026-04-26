# pyproject.toml parser — design plan

## Status headline

THIS WORK IS NOW COMPLETE

## details

Status: Commit 5 (5a + 5b + 5c) shipped. Commit 6 (Poetry handler)
pending — the architectural framing has been revised since v1
and v2; see v3 revision below.

Revision history:
- 2026-04-26 v1: initial plan, two commits (PEP 621, Poetry).
  PEP 735 marked as out of scope ("smaller adoption, tooling still
  settling").
- 2026-04-26 v2: revised after fetching the PEP 735 spec from
  packaging.python.org. PEP 735 is final (October 2024) and is
  the standardized solution for the application/non-package
  case PEP 621 doesn't cover. Pulled into Commit 5 alongside
  PEP 621 with full spec-derived semantics (PEP 503 group-name
  normalization, no-dedupe rule, visited-set cycle detection,
  explicit error cases).
- 2026-04-26 v3: revised after live verification of two
  candidate Poetry dogfood targets via WebFetch on their
  pyproject.toml. Counter-finding: Poetry-the-repo (the largest
  Poetry user) has migrated to a HYBRID configuration —
  PEP 621 [project] for the primary descriptor, plus
  [tool.poetry.group.*.dependencies] for dev/test groups. The
  v1/v2 framing of "Poetry as fallback when PEP 621/PEP 735 absent"
  was wrong: a hybrid file has [project] AND [tool.poetry.*]
  AND no fallthrough trigger, and we'd silently miss the dev
  deps. Architecture revised: [tool.poetry.*] becomes a third
  independent table-handler that runs alongside PEP 621 and
  PEP 735, contributing its own deps to the union. The
  errNoModernFormat sentinel revises to "no [project] AND no
  [dependency-groups] AND no [tool.poetry] table." Real-world
  smoke targets confirmed by fetch: Poetry-the-repo (hybrid
  shape), Textualize/rich (pure Poetry-only with legacy
  [tool.poetry.dev-dependencies]).

Cross-references:
- [`design/potential-pypi.md`](potential-pypi.md) §Layer 3 — the
  full gap analysis this plan implements a slice of
- [`internal/manifest/pypi/`](../internal/manifest/pypi/) —
  destination package; `requirements.go` + `parse.go` are shipped
- [`internal/manifest/pypi/parse.go`](../internal/manifest/pypi/parse.go) —
  the existing dispatcher, currently returns
  `ErrPyProjectTOMLNotYetSupported` for `pyproject.toml`

## What's already shipped (context)

Seven commits ship the pipeline up through PEP 621 + PEP 735:

| Commit | Layer | What it adds |
|---|---|---|
| `e133043` | 2 | `manifest.Detect` recognizes pyproject.toml, setup.py, requirements.txt |
| `c8c9e14` | 3 | `pypi.ParseRequirements` — full requirements.txt parser |
| `91ff96b` | 3 | `pypi.Parse` dispatcher routing by basename |
| `0b6524c` | 9 | survey wires `parseManifest` and `parseGraph` to PyPI |
| `194d007` | 3 | adopt `github.com/BurntSushi/toml` v1.6.0 (four-way SHA verification recorded in commit message) |
| `1df4af5` | 3 | extract `parsePEP508Requirement` shared helper for use by both requirements.txt and pyproject.toml |
| `5ef6fc1` | 3 | parse pyproject.toml — PEP 621 `[project]` + PEP 735 `[dependency-groups]` with size cap and permanent smoke fixtures |

The pipeline is end-to-end functional for modern Python projects
using PEP 621 + PEP 735. The `pypi.Parse` dispatcher routes
requirements.txt to `ParseRequirements`, pyproject.toml to
`parsePyProject`, and setup.py to `ErrSetupPyNotParseable`.
Files using only `[tool.poetry]` (no `[project]`, no
`[dependency-groups]`) currently surface the
`ErrPyProjectTOMLNotYetSupported` sentinel. Commit 6 adds the
Poetry handler that closes that gap.

## Resolved: TOML library decision

`github.com/BurntSushi/toml` v1.6.0 was vetted via signatory's
own /analyze pipeline (analysis session
`a186fb43-5e3e-4f6b-9300-9ce603a98e5e`), trusted-for-now, and
adopted in commit `194d007` with four-way SHA verification
(synthesis-cited / proxy.golang.org / GitHub upstream /
sum.golang.org content hash). The hand-roll path is no longer
under consideration.

## Two-commit scope split (regardless of library decision)

### Commit 5 — PEP 621 `[project]` + PEP 735 `[dependency-groups]` parser

The modern Python packaging standards combo. PEP 621 covers the
package's own metadata + runtime/optional deps; PEP 735 covers
dev/test/docs deps decoupled from packaging (and is the only
standardized location for deps in non-package projects like CLIs
or applications). They are independent top-level tables and can
coexist; modern uv/hatch/pdm projects increasingly use both.

What ships:

- New file `internal/manifest/pypi/pyproject.go`
- New function `parsePyProject(path string) (manifest.ProjectInfo, []manifest.Dep, error)`
  - Reads the file; unmarshals (or hand-rolls) both
    `[project]` and `[dependency-groups]` tables
  - Both are optional. The function succeeds if EITHER is
    present, returning the union of their deps. If NEITHER is
    present, returns a sentinel `errNoModernFormat` (private,
    package-level — NOT the user-facing
    `ErrPyProjectTOMLNotYetSupported`) so Commit 6's Poetry
    parser can fall through cleanly.
- Dispatcher update: `pypi.Parse` for pyproject.toml now calls
  `parsePyProject` first; on `errNoModernFormat` it returns the
  existing `ErrPyProjectTOMLNotYetSupported` (Poetry fallback
  arrives in Commit 6, so files with neither modern format still
  surface a clear message)

#### What gets extracted from `[project]` (PEP 621)

| TOML field | Maps to | Notes |
|---|---|---|
| `name` | `ProjectInfo.Name` | Verbatim, no PEP 503 normalization on project's own name |
| `version` | (deferred) | `ProjectInfo` has no `Version` field in v0.1; add when a consumer needs it |
| `requires-python` | `ProjectInfo.EcoVersion` | e.g., `">=3.10"` |
| `dependencies` | `[]Dep` with `Direct=true` | Array of PEP 508 strings |
| `optional-dependencies.<group>` | `[]Dep` with `Direct=true` | Each group's array; flatten across groups |
| `dynamic` | (informational) | If present and includes "dependencies", emit a warning — deps are computed by the build backend, our enumeration is incomplete |

#### What gets extracted from `[dependency-groups]` (PEP 735)

| TOML structure | Maps to | Notes |
|---|---|---|
| `<group-name> = ["<pep508-spec>", ...]` | `[]Dep` with `Direct=true` | Each group's array; flatten across groups |
| `<group-name>` value entries that are tables `{include-group = "<other>"}` | resolved inline at the include's position | See "PEP 735 semantic rules" below |

#### PEP 735 semantic rules (from the spec)

These are NOT optional implementation details — they are
spec-mandated and tested:

- **Group name normalization is PEP 503-style.** The reference
  implementation is `re.sub(r"[-_.]+", "-", name).lower()` — the
  same transformation we apply to package names. Reuse the
  existing `pep503Normalize` helper for both.
- **Comparisons happen on normalized names.** When resolving
  `{include-group = "Dev"}`, look up the included group by its
  normalized form (`dev`) — match groups whose original name
  normalizes to the same string.
- **Duplicate normalized group names → error.** If a file
  declares both `[dependency-groups].dev` and
  `[dependency-groups].dev_deps`, both normalize to `dev` and we
  error. Spec language: tools "SHOULD emit an error" — we treat
  it as MUST since silent merge would change the dep set.
- **No deduplication during include expansion.** If `bar` is
  `["a", {include-group = "foo"}]` and `foo` is `["a"]`, the
  resolved `bar` deps include `a` twice. Spec is explicit:
  "Tools SHOULD NOT deduplicate or otherwise alter the list
  contents produced by the include."
- **Cycles forbidden.** Spec: "Dependency Group Includes MUST NOT
  include cycles, and tools SHOULD report an error if they
  detect a cycle." Implementation: visited-set during recursive
  resolution. Visited-set is the right structure (NOT a depth
  cap, which is what `requirements.go` uses for `-r` recursion —
  different shape; here legitimate include chains can be
  moderately deep without being cyclic).
- **Includes resolve recursively.** A group that includes
  another group can itself be included. The visited-set check
  applies across the whole resolution, not per-level.
- **Include of an undefined group → error.** Reference to a
  group name that doesn't exist (after normalization) is a
  malformed file.
- **Mixed entries are valid.** A group's array can mix PEP 508
  strings and `{include-group = ...}` tables freely. Order is
  preserved on flatten.
- **Group-name overlap with `[project.optional-dependencies]` is
  legal.** A file CAN declare both `[project.optional-dependencies].dev`
  and `[dependency-groups].dev`. They are distinct collections;
  do not try to merge or dedupe by name.

#### PEP 508 string handling (shared between [project] and [dependency-groups])

PEP 508 string parsing reuses the existing logic from
`requirements.go` — same syntax for `name[extras]==version`,
environment markers, etc. Refactor opportunity: extract the
single-line PEP 508 parser from `requirements.go` into a shared
helper `parsePEP508Requirement(line string) (manifest.Dep, bool)`
so all call sites (requirements.txt, [project].dependencies,
[project.optional-dependencies], [dependency-groups]) use one
implementation. Worth doing as part of Commit 5; otherwise the
parsers drift.

#### What signatory does with multiple groups

Survey wants a flat dep list. Every dep from every group
(including `[project.optional-dependencies]` AND every
`[dependency-groups]` group) flattens into the returned `[]Dep`,
all marked `Direct=true`. Rationale: a malicious test/dev dep is
just as dangerous as a runtime dep — it runs on developer
machines and CI, often with broader permissions. Surfacing the
full dep surface area is the right behavior for a trust survey.

A future enhancement could add a `Group` field to `manifest.Dep`
so renderers can show which group(s) a dep came from. Out of
scope for v0.1.

### Commit 6 — Poetry `[tool.poetry]` handler (third independent table-handler)

**Architecture (revised v3 after dogfood verification).** Poetry
support is NOT a fallback parser. It's a third independent
table-handler that runs alongside the PEP 621 and PEP 735
handlers in `parsePyProject`, contributing its own deps to the
union. Real-world Poetry projects come in three shapes — see
"Three confirmed shapes" below — and the only architecture that
handles all three correctly is "run all handlers regardless of
who else found something."

What ships:

- New function `extractPoetryDeps(file *pyProjectFile) (poetryProjectMeta, []manifest.Dep, error)` invoked from inside `parsePyProject` after the PEP 621 and PEP 735 handlers have done their work
- The `pyProjectFile` struct extends with a `Tool` field that captures `[tool.poetry]` (and only `[tool.poetry]`; other `[tool.*]` blocks remain ignored)
- ProjectInfo merge rule: PEP 621 wins for Name and EcoVersion when present. When PEP 621 is absent, fall back to `[tool.poetry].name` for Name and `[tool.poetry.dependencies].python` for EcoVersion. This matches the hybrid case where Poetry-the-repo's metadata lives under `[project]`.
- The `errNoModernFormat` sentinel revises to "no `[project]` AND no `[dependency-groups]` AND no `[tool.poetry]` table." When ALL three are absent we surface the existing user-facing `ErrPyProjectTOMLNotYetSupported`.

#### Three confirmed shapes (verified via WebFetch)

The verification pass on real Poetry projects (2026-04-26)
established three distinct shapes the parser must handle:

| Shape | `[project]` | `[tool.poetry]` | Dev deps location | Verified target |
|---|---|---|---|---|
| Pure Poetry, legacy | absent | metadata + main deps | `[tool.poetry.dev-dependencies]` | **Textualize/rich** |
| Pure Poetry, modern groups | absent | metadata + main deps | `[tool.poetry.group.<name>.dependencies]` | (synthesized fixture only — no confirmed real target) |
| Hybrid | metadata + main deps | groups only | `[tool.poetry.group.<name>.dependencies]` | **python-poetry/poetry** |

The hybrid case was what broke v1/v2's "fallback" framing: a
hybrid file's `[project]` table makes PEP 621 succeed, no
fallthrough triggers, and dev/test deps under
`[tool.poetry.group.*]` would have been silently dropped.

#### What gets extracted from `[tool.poetry]`

| TOML location | Maps to | Notes |
|---|---|---|
| `name` | `ProjectInfo.Name` (only when `[project].name` absent) | PEP 621 wins when both present |
| `version` | (deferred — `ProjectInfo` has no `Version` field in v0.1) | |
| `dependencies.<name>` (excluding `python`) | `[]Dep` with `Direct=true` | Value can be string or table — see "Poetry value shapes" |
| `dependencies.python` | `ProjectInfo.EcoVersion` (only when `[project].requires-python` absent) | The runtime pin, NOT a dep |
| `dev-dependencies.<name>` | `[]Dep` with `Direct=true` | Legacy form (Textualize/rich's shape) |
| `group.<name>.dependencies.<name>` | `[]Dep` with `Direct=true` | Modern form (Poetry-the-repo's shape); flatten across groups |

#### Poetry value shapes

```toml
# Simple — value is a string version spec
requests = "^2.31.0"

# Table — extras + version
requests = { version = "^2.31.0", extras = ["security"] }

# Table — VCS source → pypi-local
requests = { git = "https://github.com/psf/requests.git", branch = "main" }

# Table — local path → pypi-local
mylib = { path = "../mylib" }

# Table — URL → pypi-local
foo = { url = "https://example.com/foo-1.0.tar.gz" }
```

Mapping rule: presence of `git`/`path`/`url`/`file` keys in a
Poetry dep table classifies it as `Ecosystem="pypi-local"` with
empty `CanonicalURI`. Otherwise extract the `version` string and
treat as a registry dep.

Poetry version syntax (`^`, `~`) is preserved verbatim in
`Dep.Version` — the existing convention for all parsers (npm
preserves `^4.18.0` as-is too). No translation to PEP 440.

#### Smoke fixture coverage

Three synthesized fixtures, each a separate testdata
subdirectory so the basename `pyproject.toml` is preserved
(survey dispatcher requires that):

- `internal/manifest/pypi/testdata/poetry-pure-legacy/pyproject.toml` — pure Poetry, `[tool.poetry.dev-dependencies]` form
- `internal/manifest/pypi/testdata/poetry-pure-modern/pyproject.toml` — pure Poetry, `[tool.poetry.group.*.dependencies]` form
- `internal/manifest/pypi/testdata/poetry-hybrid/pyproject.toml` — `[project]` + `[tool.poetry.group.*.dependencies]`

Plus survey-level smoke tests in `survey_test.go` that drive
the full pipeline against each fixture. Same pattern as the
PEP 621 + PEP 735 smoke fixtures from Commit 5c.

#### Real-world manual dogfood after landing

After Commit 6 lands, run `signatory survey` against the two
verified real targets:

```
git clone --depth=1 https://github.com/python-poetry/poetry /tmp/poetry-dogfood
signatory survey --manifest /tmp/poetry-dogfood/pyproject.toml

git clone --depth=1 https://github.com/Textualize/rich /tmp/rich-dogfood
signatory survey --manifest /tmp/rich-dogfood/pyproject.toml
```

Both should produce a clean dep enumeration with no errors.
Discrepancies (deps missing, malformed entries, etc.) get
filed in `dogfood-errors.md` for follow-up.

## What both parsers share

Both produce `[]manifest.Dep` and need the same downstream rules:

- All deps are `Direct=true` — pyproject.toml has no transitive concept
- Names that are valid PEP 508 distribution names get
  `CanonicalURI = "pkg:pypi/" + pep503Normalize(name)`
- Names that aren't valid PEP 508 grammar get an empty
  `CanonicalURI` (defensive — same pattern as npm's malformed-name
  handling)
- Non-registry specs (VCS, URL, local path) get
  `Ecosystem="pypi-local"` with empty `CanonicalURI`
- `ManifestPath` in `ProjectInfo` is always absolute

All five rules are already implemented in `requirements.go` and
should be reused, not duplicated. The shared helpers
(`pep503Normalize`, `isValidPEP508Name`, `splitNameAndVersion`,
`stripExtras`) are already package-private to `pypi`.

## Defensive parsing — size cap before TOML decode

The BurntSushi/toml synthesis (analysis session
`a186fb43-…`) recommended size-capping untrusted TOML input
before calling `toml.Decode`. Concrete suggestion: ≤64 KiB.
Captured here so the constraint isn't lost between dep adoption
(commit `194d007`) and the parser commits where it actually
applies.

Rationale: TOML decoding is a recursive operation, and a
maliciously deep / wide input can drive memory and CPU
consumption far past what a real pyproject.toml needs. The
provenance analyst flagged unbounded inline-array nesting as a
medium-severity concern (conclusion
`table-nesting-bounded-but-inline-arrays-unbounded`); a size cap
is the cheap front-line defense.

How it lands in the parser commits:

- The cap is enforced at the read site. `os.ReadFile` (or a
  bounded `io.LimitedReader` wrapping `os.Open`) checks size
  BEFORE handing bytes to `toml.Decode`. A file over the cap
  fails fast with a clear error naming the limit and the actual
  size.
- 64 KiB is generous for real-world pyproject.toml. The
  largest realistic files (Poetry monorepos with dozens of
  groups and many dependencies declared as tables-with-extras)
  rarely exceed 16 KiB. 64 KiB leaves comfortable headroom
  while still ruling out pathological adversarial input.
- Cap value is a named constant in the parser package
  (`maxPyProjectBytes` or similar) so it's easy to find, test,
  and adjust if a real-world file legitimately exceeds it.
- Test cases:
  - File at the exact cap → parses normally
  - File one byte over → fails with the size-cap error
  - File comfortably under cap → parses normally (sanity)
  - The same cap applies to BOTH `parsePyProject` (Commit 5c)
    and `parsePoetry` (Commit 6) since they both end up calling
    the same TOML decoder.

Open question: does the cap apply to requirements.txt too?
Current `ParseRequirements` reads with no explicit cap. Real
requirements.txt files (especially with `-r` recursion across
multiple files) can legitimately be larger than a single
pyproject.toml. Defer the requirements.txt cap decision until a
real concern surfaces; for now the cap is pyproject-only.

## Test plan

Mirroring the requirements.txt test layout. Each parser commit
ships its own `*_test.go` file.

### Commit 5 tests

Two test files: `pyproject_pep621_test.go` and
`pyproject_pep735_test.go`. Both exercise `parsePyProject` (the
combined entry point) but each focuses on its respective table.

#### PEP 621 — `pyproject_pep621_test.go`

Table-driven shape tests:

- `TestParsePyProject_PEP621_Basic` — minimal [project] with name + dependencies; assert ProjectInfo + Deps
- `TestParsePyProject_PEP621_RequiresPython` — surfaces in `EcoVersion`
- `TestParsePyProject_PEP621_DependenciesArray` — multiple deps, all Direct, all PEP 503 normalized
- `TestParsePyProject_PEP621_OptionalDependencies` — multiple groups, flattened, all Direct
- `TestParsePyProject_PEP621_ExtrasInDependency` — `requests[security]==2.31.0` parses identically to requirements.txt
- `TestParsePyProject_PEP621_EnvironmentMarker` — marker stripped (same rule as requirements.txt)
- `TestParsePyProject_PEP621_NonRegistryDep` — PEP 508 URL form `pkg @ https://...` → pypi-local

Edge cases:

- `TestParsePyProject_PEP621_NoProjectTable_NoDependencyGroups` — file with only `[build-system]` → returns `errNoModernFormat`
- `TestParsePyProject_PEP621_DynamicDependencies` — `dynamic = ["dependencies"]` — emit warning or surface in ProjectInfo? Decide during impl.
- `TestParsePyProject_PEP621_EmptyDependencies` — `dependencies = []` → no error, empty deps slice

#### PEP 735 — `pyproject_pep735_test.go`

Core shape tests:

- `TestParsePyProject_PEP735_Basic` — single group with PEP 508 string entries
- `TestParsePyProject_PEP735_MultipleGroups` — multiple groups flatten into one Deps slice
- `TestParsePyProject_PEP735_MixedEntries` — group with both string entries and `{include-group = ...}` tables
- `TestParsePyProject_PEP735_IncludeResolvesInOrder` — include expands at its position; surrounding entries before/after are preserved
- `TestParsePyProject_PEP735_NestedIncludes` — group A includes B, B includes C — full chain flattens
- `TestParsePyProject_PEP735_NoDeduplication` — `bar = ["a", {include-group = "foo"}]` with `foo = ["a"]` produces `a` twice

Spec-mandated semantic tests:

- `TestParsePyProject_PEP735_GroupNameNormalization` — declaring `dev` and including `Dev` resolves successfully (case + separator equivalence per PEP 503)
- `TestParsePyProject_PEP735_DuplicateNormalizedGroupNames` — file with both `dev` and `dev_deps` (which normalize to the same name) returns an error
- `TestParsePyProject_PEP735_IncludeUndefinedGroup` — `{include-group = "missing"}` returns an error
- `TestParsePyProject_PEP735_DirectCycle` — group A includes itself returns a cycle error
- `TestParsePyProject_PEP735_TransitiveCycle` — A→B→C→A returns a cycle error
- `TestParsePyProject_PEP735_InvalidIncludeShape` — table entry with keys other than `include-group`, or with multiple keys, returns an error
- `TestParsePyProject_PEP735_InvalidEntryType` — non-string non-table entry returns an error

Coexistence tests:

- `TestParsePyProject_PEP621AndPEP735_Combined` — file with both tables; deps from both surface, all Direct
- `TestParsePyProject_PEP621AndPEP735_DuplicateAcrossTables` — `requests` in both `[project.dependencies]` and `[dependency-groups].dev`; both surface (no cross-table dedupe)
- `TestParsePyProject_PEP735_OnlyNoProject` — file with only `[dependency-groups]` and no `[project]` (the application/CLI use case); `ProjectInfo.Name` is empty, deps surface
- `TestParsePyProject_PEP735_SameNameAsOptionalDependencyGroup` — `[project.optional-dependencies].dev` AND `[dependency-groups].dev` both present; both surface independently

End-to-end via `Parse`:

- `TestParse_PyProjectTOML_PEP621_Boto3Shaped` — fixture mimicking
  a real package project's pyproject.toml. Confirms dispatcher routing.
- `TestParse_PyProjectTOML_PEP735_AppShaped` — fixture for a non-package
  project (CLI/app) where `[dependency-groups]` is the only deps source.

### Commit 6 tests (`pyproject_poetry_test.go`)

Value-shape tests (table-driven where reasonable):

- `TestParsePyProject_Poetry_BasicStringValueDeps` — `requests = "^2.31.0"`
- `TestParsePyProject_Poetry_TableValueVersion` — `{ version = "^2.31" }`
- `TestParsePyProject_Poetry_TableValueExtras` — `{ version = "^2.31", extras = ["security"] }`
- `TestParsePyProject_Poetry_GitDependency` — `{ git = "..." }` → pypi-local with empty CanonicalURI
- `TestParsePyProject_Poetry_PathDependency` — `{ path = "../mylib" }` → pypi-local
- `TestParsePyProject_Poetry_URLDependency` — `{ url = "..." }` → pypi-local
- `TestParsePyProject_Poetry_FiltersPythonRuntimePin` — `python = "^3.10"` populates `EcoVersion`, NOT a Dep

Location tests (each Poetry shape needs its own coverage):

- `TestParsePyProject_Poetry_LegacyDevDependencies` — `[tool.poetry.dev-dependencies]` (rich's shape)
- `TestParsePyProject_Poetry_ModernGroupDependencies` — `[tool.poetry.group.dev.dependencies]`
- `TestParsePyProject_Poetry_MultipleGroupsFlatten` — multiple `[tool.poetry.group.<name>.dependencies]` tables
- `TestParsePyProject_Poetry_LegacyAndModernGroupsCoexist` — both `[tool.poetry.dev-dependencies]` AND `[tool.poetry.group.test.dependencies]` present (rare but legal)

Three-shape integration tests against the testdata fixtures:

- `TestParsePyProject_PoetryShape_PureLegacy` — uses `testdata/poetry-pure-legacy/pyproject.toml`; asserts `[tool.poetry]` provides Name + EcoVersion + main deps + dev-dependencies all surface
- `TestParsePyProject_PoetryShape_PureModern` — uses `testdata/poetry-pure-modern/pyproject.toml`; same as above but with `[tool.poetry.group.*.dependencies]`
- `TestParsePyProject_PoetryShape_Hybrid` — uses `testdata/poetry-hybrid/pyproject.toml`; asserts `[project]` provides Name + main deps AND `[tool.poetry.group.*.dependencies]` ALSO surfaces (the case that broke v1/v2's framing)

Coexistence and merge-rule tests:

- `TestParsePyProject_PEP621WinsOverPoetryName` — file declares both `[project].name` and `[tool.poetry].name`; ProjectInfo.Name comes from `[project]` (PEP 621 is the standard)
- `TestParsePyProject_PoetryNameUsedWhenPEP621Absent` — file has only `[tool.poetry].name`; ProjectInfo.Name comes from `[tool.poetry]`
- `TestParsePyProject_PEP621RequiresPythonWinsOverPoetryPython` — same merge rule for EcoVersion
- `TestParsePyProject_AllThreeTablesPresent` — `[project]`, `[dependency-groups]`, `[tool.poetry]` all present; deps from all three surface, none deduped

errNoModernFormat revision test:

- `TestParsePyProject_NoneOfThreeTables` — file with only `[build-system]` (or other non-recognized tables) → `errNoModernFormat` fires; dispatcher surfaces `ErrPyProjectTOMLNotYetSupported`. Existing test from Commit 5c is still valid; no rewrite needed.

Survey-level smoke tests (in `internal/survey/survey_test.go`):

- `TestRun_PyProjectTOML_Poetry_PureLegacy_SmokeFixture` — fixture-backed end-to-end via Run()
- `TestRun_PyProjectTOML_Poetry_PureModern_SmokeFixture` — same, modern groups shape
- `TestRun_PyProjectTOML_Poetry_Hybrid_SmokeFixture` — same, hybrid shape (PEP 621 + Poetry groups)

## Dispatcher state machine, before/after

Current (after `0b6524c`):

```
pypi.Parse(path)
  ├─ requirements.txt → ParseRequirements + sparse ProjectInfo
  ├─ pyproject.toml   → ErrPyProjectTOMLNotYetSupported (sentinel)
  ├─ setup.py         → ErrSetupPyNotParseable (permanent redirect)
  └─ other            → unrecognized-filename error
```

After Commit 5:

```
pypi.Parse(path)
  ├─ requirements.txt → ParseRequirements
  ├─ pyproject.toml   → parsePyProject  // tries PEP 621 + PEP 735
  │                       │  - if [project] exists, extract deps
  │                       │  - if [dependency-groups] exists, flatten with
  │                       │    include resolution + cycle detection
  │                       │  - return union of both (deps from either or
  │                       │    both; all Direct=true)
  │                       └─ on errNoModernFormat → ErrPyProjectTOMLNotYetSupported
  ├─ setup.py         → ErrSetupPyNotParseable
  └─ other            → unrecognized-filename error
```

After Commit 6:

```
pypi.Parse(path)
  ├─ requirements.txt → ParseRequirements
  ├─ pyproject.toml   → parsePyProject  // runs THREE handlers, returns union
  │                       │  - if [project] exists: extract metadata + deps (PEP 621)
  │                       │  - if [dependency-groups] exists: flatten with
  │                       │    include-resolution + cycle detection (PEP 735)
  │                       │  - if [tool.poetry] exists: extract metadata
  │                       │    (only when [project] absent) + deps from
  │                       │    [tool.poetry.dependencies], legacy
  │                       │    [tool.poetry.dev-dependencies], and modern
  │                       │    [tool.poetry.group.*.dependencies]
  │                       │  - return union of all three handlers' deps
  │                       │    (all Direct=true; no cross-handler dedup)
  │                       └─ on errNoModernFormat (NONE of the three present)
  │                            → ErrPyProjectTOMLNotYetSupported
  ├─ setup.py         → ErrSetupPyNotParseable
  └─ other            → unrecognized-filename error
```

Note that the v3 architecture **does not introduce any new
exported sentinels**. `errNoModernFormat` (package-private)
revises in meaning to "all three table-sets absent." The
caller-facing error stays `ErrPyProjectTOMLNotYetSupported`,
which is now a slight misnomer — by Commit 6 the parser
DOES support pyproject.toml comprehensively, and this error
fires only on files with NONE of the three recognized table
sets (i.e., a pyproject.toml that's just `[build-system]` or
some entirely unrecognized future format). A rename to
`ErrNoRecognizedPyProjectFormat` is a nice-to-have follow-up;
not blocking Commit 6.

## Out of scope (deferred)

These come up in the gap analysis but are not part of the
pyproject.toml parser commits:

- **`[build-system]` requires** — the build backend's own deps
  are interesting but distinct from project deps. Defer.
- **Poetry `source` blocks** — alternative package indexes. We
  don't track these; deps from non-PyPI indexes resolve the
  same as PyPI deps for our purposes (canonical URI is still
  `pkg:pypi/<name>`).
- **Hatchling, PDM-specific extensions** — same logic as Poetry
  but with different namespace. Add when a project hits us.
- **setup.cfg** — declining format; usually accompanied by
  pyproject.toml. Defer until a real project surfaces only-setup.cfg.
- **`dynamic` deps materialization** — running the build backend
  to compute deps. Crosses the static-analyzer boundary. Out.

## Refactor opportunity to take while we're here

Extract the PEP 508 single-line parser from
`requirements.go:parseRequirementLine` into a shared helper
`parsePEP508Requirement(line string) (manifest.Dep, bool)`. Both
this commit and `requirements.go` should call into it. Without the
extraction, two slightly-different implementations of the same
syntax accrete, and a future bug fix has to land in two places.

Lands in Commit 5 alongside the PEP 621 parser. Tests for the
helper live in `requirements_test.go` since that's where the
behavior is currently exercised; add a few targeted helper tests
to lock in the contract.

## Phasing (commits-in-flight summary)

| Commit | Status | Scope | Actual size |
|---|---|---|---|
| **5a** | shipped (`194d007`) | Adopt `github.com/BurntSushi/toml` v1.6.0 with four-way SHA verification recorded in commit message | 2 files, +3 lines (go.mod + go.sum entries only) |
| **5b** | shipped (`1df4af5`) | Extract `parsePEP508Requirement` shared helper; create new `pep508.go` + `pep508_test.go`; thin pip-wrapping wrapper stays in `requirements.go` | 3 files, +387 / -156 lines |
| **5c** | shipped (`5ef6fc1`) | PEP 621 + PEP 735 parser (`pyproject.go`); two unit test files; two testdata fixtures; survey-level smoke tests; size cap; `// indirect` marker drops on go.mod tidy | 9 files, +993 / -13 lines |
| **6** | **pending** | Poetry handler — third independent table-handler in `parsePyProject` covering all three confirmed shapes (pure legacy, pure modern groups, hybrid). Three new testdata fixtures + survey smoke tests. | est. ~250-300 LOC parser + ~500 LOC tests + 3 fixtures |

Each commit individually testable. Each commit's pre-commit hook
(gofmt, vet, full test suite) is the gate for the next.

## Questions that should not block starting

- Whether to surface the project's own `version` field somewhere
  in `ProjectInfo` (currently `Name` and `EcoVersion` only). Decide
  during impl; default to "no, until a consumer needs it."
- Whether to capture environment markers verbatim somewhere instead
  of stripping. Same answer as requirements.txt: defer until a
  signal needs them.
- Whether to validate the TOML against the PEP 621 schema strictly
  vs. lenient. Lenient — we're a survey tool, not a packaging
  validator. A pyproject.toml that mostly works should produce a
  mostly-correct dep list.

## Resolved questions

- **TOML library:** BurntSushi/toml v1.6.0 adopted in commit
  `194d007`. See "Resolved: TOML library decision" near the top.
- **PEP 735 in scope:** confirmed in scope after the live spec
  fetch (v2 revision); shipped in Commit 5c.
- **Poetry as fallback vs. third independent handler:** confirmed
  third independent handler after the live verification of two
  Poetry dogfood targets (v3 revision). Hybrid case (Poetry-the-repo)
  invalidated the fallback framing; rich validates the pure
  Poetry-only case.
