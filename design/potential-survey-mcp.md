# Potential: wire `signatory_survey` MCP tool to `survey.Run`

Status: proposed; **deprioritized for v0.1**. Captured 2026-04-24
from the v0.1 burndown scoping conversation.

Deprioritization reasoning (2026-04-24):

1. **v0.1 Invariant 1 bars direct Anthropic API access.** The only
   way to exercise MCP tool behavior end-to-end is through a live
   Claude Code session driven by hand. There is no scripted
   integration path for MCP in v0.1.
2. **No dogfood testing methodology for MCP.** CLI changes validate
   via unit tests plus the per-target token-delta measurement
   (see `design/ImproveProvSignals.md §Validation`). MCP changes
   have no analogous measurement loop — landing improvements
   without one means "seems fine in a session" is the bar, which
   is below v0.1's standard.

Revisit when either constraint changes: a dogfood plan lands for
MCP, or the API-access invariant relaxes in v0.2.

The plan below is preserved because the library half
(`internal/survey.Run`) is stable and the wiring work is small and
bounded — picking this up later will not require re-discovery.

## Context

`internal/mcp/tools/survey.go` is currently a stub: every call returns
`CodeNotFound` with "dependency parsing is not yet implemented." The
library half (`internal/survey.Run`) is fully functional for go.mod
and package.json — the CLI `signatory survey` uses it today. The MCP
tool never imports it.

The MCP server nevertheless advertises `signatory_survey` prominently:

- `internal/mcp/handshake.go:193` — `serverInstructions` emitted on
  every `initialize` lists it in the recommended tool set.
- `internal/mcp/resources/help.go:93` — `signatory://help` maps the
  user question "How is the whole project's dep tree?" directly at it.
- `internal/mcp/resources/help.go:140` — documents the stub's
  `CodeNotFound` as "the v0.1 limitation, not a real failure."

No signatory-authored skill or handoff invokes it (`/analyze` and
`/vet-dependency` are per-target; handoff templates are per-target).
The expected callers are:

- A human typing a project-level question into a Claude Code session.
- An LLM that reads `serverInstructions` and picks the advertised tool
  for a project-level user question.

Both hit the stub, get `CodeNotFound`, and are steered to
`signatory_analyze` per-dep — which is more expensive and pushes
toward the scanner-style use pattern the v0.1 economics framing
(`design/analysis-economics.md §7`) tries to prevent.

The library-half work already exists; this task is wiring.

## Goal

Turn `internal/mcp/tools/survey.go` from a `CodeNotFound` stub into a
thin adapter over `internal/survey.Run`, with parity to the CLI for
the ecosystems the library already supports (go.mod, package.json).
Any unrecognized manifest surfaces the same `supported in v0.1`
message as the CLI.

## Out of scope

- Auto-detection of the manifest from cwd (MCP tool has no cwd concept
  anyway; operator passes the path). The `InputSchema` keeps
  `manifest_path` required.
- `refresh: true` behavior — accepted but not acted on, matching the
  CLI's `--refresh` stub. Deferred with a shared TODO, not re-scoped
  in this change.
- New ecosystem support (PyPI, Cargo) — those need library-side work;
  out of scope for this task.
- Library-side graph extraction for npm — orthogonal; survey already
  renders the "drill-down unavailable" fallback when
  `manifest.ErrGraphUnavailable` is returned.

## Design decisions

### 1. Dependency injection: add a `Store` field

`SurveyTool` becomes:

```go
type SurveyTool struct {
    Store store.Store
}
```

Parallels `AnalyzeTool`, `SummaryTool`, `DetailTool`, `SignalsTool`,
`ShowAnalysesTool`, `ShowConclusionsTool`, `ShowMethodologyTool`,
`IngestAnalysisTool` — same pattern, injected at construction.

### 2. Wire the store at registration

`cmd/signatory/mcp.go:94` currently registers `&tools.SurveyTool{}`
and carries a now-incorrect comment that it's "a pure read-only
dispatcher in Phase 1 … no store field required." Replace with
`&tools.SurveyTool{Store: s}` and delete the Phase-1 comment — the
store IS needed because `survey.Run` does per-dep store lookups for
tier resolution.

### 3. MCP payload type, not library type

`survey.Result` has no JSON struct tags — Go's default marshaling
would emit CamelCase keys (`Project`, `Deps`, `Summary`). Every other
MCP tool uses snake_case (see `analyzeData` at
`internal/mcp/tools/analyze.go:63`).

Introduce a payload type in the tool file, structurally parallel to
`survey.Result` but with `json:"snake_case"` tags, and a small mapping
function that populates it from the library type. Same pattern
`AnalyzeTool` uses (`analyzeData` wraps `profile.SignalsSummary`
etc.). Keeps `internal/survey` decoupled from MCP's serialization
convention.

The payload carries the same data the CLI's `--json` path already
exposes, just with different key casing:

```
{
  project:  { name, ecosystem, eco_version, manifest_path },
  deps:     [{ name, version, ecosystem, direct, canonical_uri,
               tier, posture_version, posture_rationale,
               burn_reason, other_versions?, reachability? }, ...],
  summary:  { total, direct, indirect, by_tier: {...},
              needs_review: [...], indirect_by_reachability: {...} }
}
```

### 4. Error mapping

| Library outcome | MCP code | Rationale |
|---|---|---|
| `manifestPath == ""` | `CodeSchemaViolation` | Keeps existing behavior unchanged |
| `os.IsNotExist` on the manifest read | `CodeNotFound` | "Path you named doesn't exist" is a real NotFound |
| Unrecognized filename (`parseManifest` fallthrough) | `CodeSchemaViolation` | Caller sent a path we can't dispatch; same as an unknown enum value |
| Manifest parse error | `CodeValidationFailed` | Matches MCP envelope convention — input was shaped right but contents invalid |
| Per-dep store lookup error from `resolveDep` | `CodeInternalError` | Store error is not the caller's fault |
| Happy path | `mcp.OK(payload).WithCacheHit(true)` | Survey IS a cache lookup — no collection happens |

`CodeNotFound` specifically is **no longer** returned as the default.
That's the current stub's failure mode and the one the help resource
documents as "the v0.1 limitation, not a real failure." After this
change, `CodeNotFound` means what it says — the manifest file isn't
there.

### 5. `Description()` rewrite

Current (`internal/mcp/tools/survey.go:28`):

> …Takes a manifest path (go.mod, package.json, Cargo.toml,
> pyproject.toml). **v0.1 limitation: dependency parsing is not yet
> implemented — this tool returns CodeNotFound with guidance to call
> signatory_analyze per-dep until v0.2.** Still useful to confirm a
> manifest is recognised.

New (advertises what now actually works; still accurate about what
doesn't):

> USE THIS when the user asks about a whole project's dependency
> tree, not one dep in isolation — 'what's the posture of all my
> deps?', 'which of my transitive deps are unassessed?'. Takes a path
> to a manifest file. Supported in v0.1: go.mod, package.json (with
> package-lock.json v2/v3 for transitive deps). Returns per-dep tier
> resolution plus an aggregate summary — burned, vetted-frozen,
> trusted-for-now, unexamined, not-in-store, rejected, local-replace.
> Pyproject.toml and Cargo.toml return CodeSchemaViolation until
> their parsers land.

### 6. Consequential doc/help updates (same change-set)

- `internal/mcp/resources/help.go:93` — remove `(v0.1: stubbed)`
  annotation on the `signatory_survey` row.
- `internal/mcp/resources/help.go:140` — delete the "CodeNotFound
  from signatory_survey: the v0.1 limitation" failure-mode entry;
  leave the others unchanged.
- No change to `internal/mcp/handshake.go:193` — `serverInstructions`
  already advertises `signatory_survey` correctly for the new
  reality.

### 7. Tests

Existing `internal/mcp/tools/survey_test.go` has six tests — five
assert the stub behavior and must change, one
(`TestSurveyTool_Name`) is unaffected.

**Delete outright:**

- `TestSurveyTool_GoMod_NotImplemented`
- `TestSurveyTool_PackageJSON_NotImplemented`

Both assert the stub behavior for ecosystems that will now work.
Deleting is correct; these aren't regressions to guard, they're
documentation of the stub.

**Rewrite:**

- `TestSurveyTool_CargoToml_NotImplemented` →
  `TestSurveyTool_UnsupportedEcosystem_Cargo` — asserts the new
  behavior: `CodeSchemaViolation` with the "supported in v0.1:
  go.mod, package.json" message. The test name keeps the ecosystem
  breadcrumb so a reader searching for "cargo" still finds this guard.

**Keep unchanged:**

- `TestSurveyTool_EmptyManifestPath` (contract unchanged)
- `TestSurveyTool_EmptyManifestPath_MutationCheck` (contract unchanged)
- `TestSurveyTool_SchemaViolation_UnknownField` (input schema unchanged)
- `TestSurveyTool_Name` (unchanged)
- `TestSurveyTool_InputSchemaValid` (schema still valid JSON)

**New tests** (borrowing `openTestStore` / `seedEntity` fixtures from
`analyze_test.go`):

- `TestSurveyTool_GoMod_HappyPath` — write a minimal `go.mod` to
  `t.TempDir()`, seed a store with one entity matching one of its
  deps plus a vetted-frozen posture, assert `OK` status,
  `CacheHit=true`, and that `data.deps[i].tier == "vetted-frozen"`
  for the seeded dep, `"not-in-store"` for the unseeded.
- `TestSurveyTool_PackageJSON_HappyPath` — write a minimal
  `package.json` to `t.TempDir()`, seed entity + posture for one npm
  dep, assert same shape.
- `TestSurveyTool_PackageJSON_WithLockfile` — same as above plus a
  `package-lock.json` v3 with a transitive dep, assert
  `summary.indirect > 0` in the returned payload.
- `TestSurveyTool_ManifestNotFound` — pass `/nonexistent/go.mod`,
  assert `CodeNotFound`.
- `TestSurveyTool_UnrecognizedManifest` — pass `something.xml`,
  assert `CodeSchemaViolation` with the "supported in v0.1" message.
- `TestSurveyTool_RequiresStore_MutationCheck` — parallel to
  `TestAnalyzeTool_HappyPath_RequiresStore` at `analyze_test.go:81`.
  Construct with a real store and seeded data; assert `OK`. Guards
  against someone reintroducing the `Store`-less stub shape.

Target: 7 new passing tests, 2 deleted, 1 rewritten, 4 untouched.

## File-by-file change summary

| File | Change | Rough size |
|---|---|---|
| `internal/mcp/tools/survey.go` | Add Store field; rewrite `Handle`; add payload type + mapper; rewrite `Description`; keep `Name`/`InputSchema`/`detectEcosystemFromPath` | ~120 LoC net (replaces ~60) |
| `internal/mcp/tools/survey_test.go` | 2 deletes, 1 rewrite, 7 adds, 4 untouched | ~180 LoC net |
| `cmd/signatory/mcp.go` | `&tools.SurveyTool{}` → `&tools.SurveyTool{Store: s}`; delete stale Phase-1 comment | 2 lines |
| `internal/mcp/resources/help.go` | Remove `(v0.1: stubbed)` annotation; delete CodeNotFound failure-mode entry | 2 edits |

## Verification

1. `go build ./...` and `go test -race ./internal/mcp/... ./internal/survey/...`
   — existing test suite plus new tests.
2. `go test ./cmd/signatory/...` — picks up the registration change.
3. Manual MCP smoke: `cmd/smoke-mcp/main.go` already lists
   `signatory_survey` in its expected-tools set at line 178; its
   initialize-and-list test should continue to pass. If the smoke
   binary has a tools-call path, extend it to call `signatory_survey`
   against a fixture `go.mod`.
4. End-to-end sanity: `signatory mcp` started locally, then a
   JSON-RPC `tools/call` with a real `go.mod` path against a store
   that has a few postures — eyeball the JSON payload against what
   the CLI's `--json` mode produces.

## Risks / things to watch

- **Library-type stability.** Deliberately wrapping `survey.Result`
  rather than adding JSON tags to it, precisely so the CLI's `--json`
  output (which currently emits CamelCase) doesn't silently flip to
  snake_case and break any consumer that's scraping it. Worth
  deciding separately whether we want CLI and MCP to converge on
  snake_case eventually, but that's a breaking change for CLI JSON
  callers and not part of this task.
- **Manifest-path security posture.** The tool accepts any path and
  reads it — same capability the CLI has. In v0.1 (local-only per
  Invariant 1) this is the expected trust boundary. Worth a comment
  in the handler saying so, and a `//nolint:gosec // G304:`
  annotation on the read path (matches the convention in
  `internal/manifest/npm/parse.go:30` and `gomod/parse.go`).
- **Error-code taxonomy drift.** Introducing the first MCP tool that
  returns `CodeValidationFailed` (for parse errors). Worth grepping
  the codebase to confirm that code is defined (it is —
  `interfaces.go:149`) and that the envelope doc permits it for tool
  responses, not just ingest.

## Sequencing if approved

1. Confirm the approach, especially the CamelCase-vs-snake-case call
   (wrap vs retag).
2. TDD: land the test file first with the new tests marked as
   expected-to-fail (via `t.Skip` removal pattern, or just let them
   fail for one commit).
3. Land the handler rewrite + registration fix together.
4. Land the help-resource edits in the same commit (they'd lie about
   the tool's behavior otherwise).
5. Verify against a real `go.mod` + store that has postures.

## References

- `internal/survey/doc.go` — library package doc stating the
  "Run is the shared core, CLI is a thin renderer, web UI imports
  the same function" design intent.
- `internal/survey/survey.go:30` — `Run` signature.
- `internal/mcp/tools/analyze.go` — reference implementation for a
  read-only, store-backed MCP tool (the shape this change follows).
- `cmd/signatory/survey.go:102` — CLI's call to `survey.Run`; the
  MCP handler becomes structurally similar.
- `design/mcp-protocol-envelopes.md` — error-code and envelope
  conventions.
- `design/v0.1-invariants.md §"Invariant 4"` — named transports;
  MCP is the LLM data channel.
