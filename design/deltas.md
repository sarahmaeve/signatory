# Signal Deltas — Cheap, Deterministic Change Detection

## Status (as of 2026-05-13)

**Phase 1 shipped.** Pure `Diff`, text + JSON renderers, CLI verb,
time-shorthand parser, seeded sample.db with 9 adversary-shape
scenarios, full e2e coverage. The `--all` confirmation prompt
landed as a y/N interactive prompt at >10 *runs* (not the
originally-spec'd passive stderr warning at >100 *observations* —
see the "Implementation order" section for the rationale).

**`--range T1..T2` added post-Phase-1.** Bounded windows
(inclusive on both ends) with the same time-shorthand each
endpoint accepts. Mutex with `--since` / `--last` / `--all`.

**Phase 2 partial.** The MCP tool `signatory_deltas` is live —
agent-facing surface mirrors the CLI's `--json` wire shape with
a 200-transition cap and `truncated` flag. Time scope is
required (no implicit default; `--all` has no MCP equivalent).
The remaining Phase 2 item — `signatory deltas --html` for
time-series charts — is still deferred behind its own
chart-shape design pass.

**Internal refactor:** the store→filter→window→diff pipeline
lives in `internal/deltas.Computer`, mirroring
`internal/summary.Assembler`. Both the CLI verb and the MCP tool
construct a `Computer` with the store and call `Compute(ctx,
Params) (RenderInput, error)`.

**Gitignore fix:** the e2e sample.db was originally caught by a
broad `*.db` rule and silently never tracked. An explicit
`!internal/deltas/testdata/sample.db` exception now keeps the
fixture under version control.

## Problem statement

`signatory analyze` produces point-in-time observations. Layer 2
(analyst dispatch) interprets those observations and produces
verdicts at LLM cost. Between the two extremes — cheap collection
and expensive interpretation — there's a missing middle layer:
**deterministic change detection over accumulated observations**.

v0.1 deliberately focused on point-in-time analysis and the "wow"
of LLM interpretation. The append-only signal store was built with
history-aware queries in mind (see
[`stalesignals.md`](stalesignals.md)) but the CLI surface only
exposed the current-state view. This document specs the missing
middle: a deltas presentation primitive that surfaces what changed
about a target between observations, cheaply and without LLM cost.

The use cases this unblocks:

- Scheduled drift monitoring (cron `signatory analyze --refresh`;
  alert on non-trivial deltas).
- CI gates on dependency-change shapes (new git-URL dep introduced,
  attestation lost, workflow ref changed, tag rewritten).
- Incident retrospective: "what changed about my deps in the last
  N days when campaign X was active."
- Cheap re-checks after remediation.

None of these require LLM dispatch. Analyst-layer interpretation can
still run on top when a delta looks suspicious, but the day-to-day
loop is deterministic.

## What's already in place

The store and signal model were built for this. From
[`stalesignals.md`](stalesignals.md) and the relevant
`internal/store/sqlite.go` functions:

| Capability | Status |
|---|---|
| Append-only signal writes | ✓ `AppendSignals`; SQLite triggers ABORT UPDATE/DELETE |
| Unique IDs with timestamp suffix | ✓ `{source}:{entity_id}:{type}:{collected_at_nanos}` |
| Full-history query | ✓ `GetSignals(entityID)` — *"Use `GetLatestSignals` for current state"* |
| Latest-only query | ✓ `GetLatestSignals(entityID)` — ROW_NUMBER window over partitions |
| Cross-row supersession | ✓ `signal_resolutions` table + `autoSupersedeConflicts` |

What's missing is the **presentation layer**: a CLI verb that calls
`GetSignals`, a diff helper that renders changes between
observations, and (optionally) an MCP tool wrapping the same.

## Confirmed design decisions

| Question | Answer |
|---|---|
| Diff granularity | Per-field — show *what actually changed* |
| Default time window | Last 24 hours |
| Time-window flags | `--since` (words / durations / timestamps), `--last N`, `--range T1..T2`, `--all` (mutually exclusive) |
| Package home | `internal/deltas/` |

## CLI surface

### Verb

```
signatory deltas <target> [flags]
```

### Flags

| Flag | Purpose |
|---|---|
| `--since <value>` | Time-bounded view. Accepts: Go duration (`2d`, `12h`, `30m`), word-shaped (`yesterday`, `today`, `last-week`, `last-month`), or RFC3339 timestamp. Default if no time flag is `--since 24h`. |
| `--last <n>` | Show the most recent `n` observations per `(type, source)` group. |
| `--range <T1..T2>` | Bounded range (inclusive on both ends). Each endpoint accepts the same syntax as `--since`. Mirrors git rev-range. |
| `--all` | Show the full history. Prompts y/N when more than 10 collection runs are present (a "run" = distinct `collected_at` timestamp). `--yes` / `-y` bypasses. EOF / closed stdin defaults to no. |
| `--type <name>` | Filter by signal type (e.g. `trusted_publishing`, `version_unpublish_observed`). |
| `--source <name>` | Filter by collector source (e.g. `npm-registry`, `github`, `git`). |
| `--group <name>` | Filter by signal group (vitality / governance / publication / hygiene / criticality / identity). |
| `--include-unchanged` | Surface signals that have no diffs in the window. Default behavior suppresses them. |
| `--json` | Emit structured JSON instead of human-readable text. |
| `--yes` / `-y` | Skip the confirmation prompt for large `--all` expansions. |

The four window flags (`--since`, `--last`, `--range`, `--all`)
are mutually exclusive; the CLI verb returns an EX_USAGE error
when more than one is set.

### Examples

```
signatory deltas pkg:npm/@tanstack/react-router
  → last 24 hours of changes across all signals

signatory deltas pkg:npm/@tanstack/react-router --since 'last week'
  → last 7 days

signatory deltas pkg:pypi/cryptography --type attestation_consistency
  → workflow-ref changes only, last 24h default

signatory deltas repo:github/tj-actions/changed-files --all
  → full tag-rewrite history (once tag_sha_mapping ships)

signatory deltas pkg:npm/lodash --last 5 --json
  → most recent 5 observations per (type, source), JSON-formatted

signatory deltas pkg:npm/@tanstack/react-router \
    --range '2026-05-10T00:00:00Z..2026-05-12T23:59:59Z'
  → bounded window, both endpoints inclusive

signatory deltas pkg:npm/@tanstack/react-router --range 'last-week..yesterday'
  → bounded window using time-shorthand on both endpoints
```

### Time parsing extension

**As shipped:** the parser lives at `cmd/signatory/deltas_time.go`
as a standalone `parseTimeShorthand` (and `parseRangeShorthand`
for `--range`), not as an extension of an existing function. The
design rationale below still describes the accepted forms.

The original sketch:

```go
// New parseTimeWindowFlag (or extend existing parseSinceFlag):
//
//   Word shortcuts:
//     "today"        → now - elapsed-since-midnight-UTC
//     "yesterday"    → now - 24h
//     "last-week"    → now - 168h
//     "last-month"   → now - 720h  (~30 days; not calendar-month)
//
//   Then Go duration (existing): "2d" (NOT valid in Go; spec note),
//     "12h", "30m" → relative to now.
//
//   Then RFC3339 timestamp (existing): "2026-05-12T19:20:00Z" → absolute.
```

Note: Go's `time.ParseDuration` does **not** accept `2d` natively
(only `ns/us/µs/ms/s/m/h`). The implementation should either:
- Accept `2d`/`7d` via a small pre-parser, or
- Document that days must be expressed as `48h`/`168h`.

I'd lean toward the pre-parser — `2d` is what users naturally write,
and the parsing is a four-line regex.

## Diff semantics

Pure function: `Diff(prior, current map[string]any) ValueDiff`.

### Per-field treatment

For each key in the union of `prior` and `current`:

- **Key in current only** → "added"
- **Key in prior only** → "removed"
- **Key in both, values equal** → unchanged (omitted from output)
- **Key in both, values differ** → "changed", with type-specific rendering:

| Value type | Diff representation |
|---|---|
| scalar (string, number, bool) | literal `before → after` |
| object | recurse; emit nested ValueDiff |
| array of primitives, same length | per-position diff, show only changed positions |
| array of primitives, different length | "gained N entries, lost M entries"; full list for short arrays (<10), summary for longer |
| array of objects with stable key field (`login`, `version`, `tag_name`) | align by key; per-element diff |
| array of objects without stable key | opaque before/after; v2 may align by structural similarity |

### Recursion depth

Bounded at 5 levels. Signal values are generally shallow; 5 is
defensive against pathological inputs without producing truncated
diffs on real data.

### Value-equality discipline

Use `reflect.DeepEqual` for the "values equal" check after both
sides are decoded from JSON into `map[string]any`. JSON's
number-decoding ambiguity (everything becomes `float64`) means
integer-vs-float comparisons work correctly through DeepEqual on
decoded values; we don't need a custom equality function.

### Stable-key heuristic for arrays of objects

Recognized stable keys (in priority order):
- `version` — for version arrays (`unpublished_versions`)
- `login` — for publisher / contributor arrays (`logins`, `publishers`)
- `tag_name` — for tag arrays
- `path` — for file arrays
- `name` — for dep arrays (`git_url_deps_in_latest`)

When the first array element has one of these keys, all elements
are assumed to have it (validated; mixed shapes fall through to
opaque). The diff pairs elements by stable key and renders per-key
addition / removal / change.

## Test scenarios modeled on adversary reports

The diff helper's correctness is best demonstrated against real
attack shapes. Each case below is a (prior, current) signal-value
pair drawn from documented incidents. Each becomes one test in
`internal/deltas/diff_test.go`.

### 1. axios — trusted-publishing lost

Source: `design/threat-landscape/example-axios-attack.md`.

Signal: `trusted_publishing` (npm).

```
prior:   {"present": true, "version_checked": "1.13.0",
          "publisher_kind": "GitHub", "source_repository": "axios/axios",
          "workflow": "release.yml"}
current: {"present": false, "version_checked": "1.14.1"}
```

Expected diff:
- `present: true → false`
- `version_checked: 1.13.0 → 1.14.1`
- `publisher_kind: removed (was "GitHub")`
- `source_repository: removed (was "axios/axios")`
- `workflow: removed (was "release.yml")`

### 2. TanStack — versions unpublished after cleanup

Source: `design/threat-landscape/2026-05-12-tanstack-mini-shai-hulud.md`
+ the in-tree raw data at `raw-data/2026-05-12-tanstack-react-router-signals.json`.

Signal: `version_unpublish_observed`.

```
prior:   {"unpublished_count": 0, "unpublished_versions": [],
          "list_capped": false}
current: {"unpublished_count": 2,
          "unpublished_versions": [
            {"version": "1.169.8", "published_at": "2026-05-11T19:26:17Z"},
            {"version": "1.169.5", "published_at": "2026-05-11T19:20:42Z"}
          ],
          "most_recent_unpublished_publish_time": "2026-05-11T19:26:17Z",
          "list_capped": false}
```

Expected diff:
- `unpublished_count: 0 → 2`
- `unpublished_versions: gained 2 entries (1.169.8, 1.169.5)`
- `most_recent_unpublished_publish_time: added`
- `list_capped: unchanged (omitted)`

### 3. TanStack — cadence-divergence post-incident

Source: same entry as #2.

Signal: `commit_publish_cadence_divergence`.

```
prior:   {"commit_days_ago": 1, "publish_days_ago": 0,
          "divergence_days": -1, "shape": "synchronized"}
current: {"commit_days_ago": 0, "publish_days_ago": 6,
          "divergence_days": 6, "shape": "active-repo-paused-publishes"}
```

Expected diff:
- `commit_days_ago: 1 → 0`
- `publish_days_ago: 0 → 6`
- `divergence_days: -1 → 6`
- `shape: "synchronized" → "active-repo-paused-publishes"`

### 4. tj-actions — tag rewrite (forward-looking)

Source: `design/threat-landscape/2025-03-14-tj-actions-changed-files.md`.

Signal: `tag_sha_mapping` (Tier 2, not yet shipped — included to spec
the eventual diff shape).

```
prior:   {"tag_name": "v45.0.7",
          "current_sha": "abc123...", "first_observed": true}
current: {"tag_name": "v45.0.7",
          "current_sha": "0e58ed8671d6b60d0890c21b07f8835ace038e67",
          "first_observed": false}
```

Expected diff:
- `current_sha: "abc123..." → "0e58ed867..."` — **the tag-rewrite detection**
- `first_observed: true → false`

### 5. TanStack — careful-variant workflow ref change (sketch 5)

Source: same TanStack entry, "What this exposes as a gap" section.

Signal: `attestation_consistency` (PyPI, but conceptually parallel
on npm once landed).

```
prior:   {"consistent": true, "workflow_refs":
            ["pypi-publish.yml", "pypi-publish.yml", "pypi-publish.yml"],
          "latest_workflow_ref": "pypi-publish.yml",
          "unique_workflow_refs": 1, "workflow_ref_transitions": 0,
          ... other fields ...}
current: {"consistent": true, "workflow_refs":
            ["release-v2.yml", "pypi-publish.yml", "pypi-publish.yml"],
          "latest_workflow_ref": "release-v2.yml",
          "unique_workflow_refs": 2, "workflow_ref_transitions": 1,
          ... other fields ...}
```

Expected diff:
- `workflow_refs: position 0 changed: "pypi-publish.yml" → "release-v2.yml"`
- `latest_workflow_ref: "pypi-publish.yml" → "release-v2.yml"`
- `unique_workflow_refs: 1 → 2`
- `workflow_ref_transitions: 0 → 1`
- Other fields unchanged (omitted from output)

### 6. publisher_account_class — bot publisher appears

Source: tj-actions case, generalized for PyPI/npm publisher addition.

Signal: `publisher_account_class`.

```
prior:   {"logins": [{"login": "alice", "class": "human"}],
          "total_count": 1, "non_human_count": 0}
current: {"logins": [
            {"login": "alice", "class": "human"},
            {"login": "evil-publisher-bot", "class": "service-account",
             "matched_pattern": "-bot"}
          ],
          "total_count": 2, "non_human_count": 1}
```

Expected diff:
- `logins: gained 1 entry — evil-publisher-bot (class=service-account)`
- `total_count: 1 → 2`
- `non_human_count: 0 → 1`

### 7. bufferzonecorp — version-burst flag flip

Source: `design/threat-landscape/2026-05-02-bufferzonecorp-campaign.md`.

Signal: `version_publish_burst`.

```
prior:   {"burst_detected": false, "versions_checked": 5,
          "versions_in_window": 5, "window_hours": 168}
current: {"burst_detected": true, "versions_checked": 10,
          "versions_in_window": 10, "window_hours": 72}
```

Expected diff:
- `burst_detected: false → true`
- `versions_checked: 5 → 10`
- `versions_in_window: 5 → 10`
- `window_hours: 168 → 72`

### 8. Maintainer churn — list addition

Source: general pattern, ties to `2026-04-21-vercel-contextai-incident.md`
§"Identity-surface exposure".

Signal: `maintainer_count`.

```
prior:   {"count": 2, "logins": ["alice", "bob"]}
current: {"count": 3, "logins": ["alice", "bob", "newcomer"]}
```

Expected diff:
- `count: 2 → 3`
- `logins: gained 1 entry — newcomer`

These eight cases anchor the diff helper's behavior against real
attack shapes. The threat-landscape entries themselves are the
specification: any signal-value transition documented in those
entries should be cleanly surfaced by the deltas view.

## Implementation outline

### File layout (as shipped)

```
internal/deltas/
  doc.go                  package comment
  types.go                Observation, SignalDelta, TimeWindow,
                          RenderInput, TextOpts; TimeWindow.Kind()
                          and MarshalJSON
  types_test.go           TimeWindow precedence + JSON shape
  diff.go                 pure Diff(prior, current) ValueDiff
  diff_test.go            8 adversary-shape scenarios + edge cases
  render.go               RenderText + RenderJSON with newest-
                          change highlighting + unchanged-signal
                          suppression
  render_test.go          renderer behavior tests
  compute.go              ComputerStore interface (narrow:
                          FindEntityByURI + GetSignals), Params,
                          Computer.Compute, ErrEntityNotFound
                          sentinel
  compute_test.go         7 unit tests against an in-memory
                          fakeStore; mutation-verified resolver
                          coverage
  testdata/sample.db      committed e2e fixture (9 scenarios)
  testdata/generate/      generator program for the fixture

cmd/signatory/
  deltas.go               thin CLI shell: flags, mutex check,
                          window resolver, --all prompt, render
                          wiring (calls deltas.Computer)
  deltas_time.go          parseTimeShorthand, parseRangeShorthand
  deltas_time_test.go     parser coverage
  deltas_range_test.go    DeltasCmd flag/window tests + range
                          e2e against sample.db
  deltas_confirm.go       allRunsPromptThreshold,
                          countRunsInRender, confirmAllExpansion
  deltas_confirm_test.go  prompt-path coverage + run-counter
  deltas_e2e_test.go      e2e against sample.db across all 8
                          original adversary scenarios

internal/mcp/tools/
  deltas.go               DeltasTool + DeltasResponse;
                          DeltasTransitionsCap = 200; buildWindow
                          enforces "must set one of since / last /
                          range_start+range_end"
  deltas_test.go          11 unit tests against openTestStore
```

### Public API of internal/deltas (as shipped)

```go
package deltas

// ValueDiff / Change / ChangeKind / ElementChange — the
// structural diff types. Top-level is per-key add/remove/change;
// nested objects recurse; arrays use length+stable-key heuristics.
type ValueDiff struct {
    Added   map[string]any
    Removed map[string]any
    Changed map[string]Change
}

type Change struct {
    Kind     ChangeKind
    Before   any
    After    any
    Nested   *ValueDiff
    Elements []ElementChange
}

// SignalDelta groups chronological observations of one
// (type, source) pair plus the pair-wise diffs.
type SignalDelta struct {
    Type         string
    Source       string
    SignalGroup  string
    Observations []Observation
    PairDiffs    []ValueDiff
}

func (s SignalDelta) HasAnyChange() bool

// TimeWindow's four modes (All / Last / Range / Cutoff) are
// mutually exclusive; Kind() returns the discriminator the
// renderer dispatches on.
type TimeWindow struct {
    All        bool
    Last       int
    RangeStart time.Time
    RangeEnd   time.Time
    Cutoff     time.Time
}
func (w TimeWindow) Kind() string  // "all" / "last" / "range" / "since"

// RenderInput is what RenderText and RenderJSON consume — and
// what Computer.Compute returns.
type RenderInput struct {
    Target string
    Window TimeWindow
    Groups []SignalDelta
}

// Diff is the pure-function workhorse.
func Diff(prior, current map[string]any) ValueDiff

// ComputerStore is the narrow Store subset Compute needs.
// *store.SQLite satisfies it; tests inject fakes.
type ComputerStore interface {
    FindEntityByURI(ctx context.Context, canonicalURI string) (*profile.Entity, error)
    GetSignals(ctx context.Context, entityID string) ([]profile.Signal, error)
}

// Params describes the inputs to Compute. Target accepts any
// form profile.ResolveTarget understands (canonical URI, URL,
// owner/repo shorthand).
type Params struct {
    Target string
    Window TimeWindow
    Type   string
    Source string
    Group  string
}

type Computer struct{ Store ComputerStore }

func New(s ComputerStore) *Computer

// Compute resolves the target, queries history, applies the
// filters and window, and returns the rendered-shape input.
// Returns ErrEntityNotFound (wrapped) when the target misses;
// other store errors propagate wrapped.
func (c *Computer) Compute(ctx context.Context, p Params) (RenderInput, error)

var ErrEntityNotFound = errors.New("no entity matches target")

// Renderers consume RenderInput directly.
func RenderText(w io.Writer, in RenderInput, opts TextOpts) error
func RenderJSON(w io.Writer, in RenderInput) error
```

### CLI flow

```go
func (cmd *DeltasCmd) Run(globals *Globals) error {
    s := openStore(globals.DB)
    entity := resolveTarget(s, cmd.Target)

    cutoff, err := parseTimeWindow(cmd)
    if err != nil { return err }

    allSignals := s.GetSignals(ctx, entity.ID)
    filtered := filterByTime(allSignals, cutoff, cmd.Last, cmd.All)
    filtered = filterByType(filtered, cmd.Type, cmd.Source, cmd.Group)

    grouped := groupByTypeSource(filtered)  // map[(type, source)][]Observation

    deltas := []SignalDelta{}
    for key, observations := range grouped {
        sort.Sort(byCollectedAt(observations))
        pairDiffs := []ValueDiff{}
        for i := 1; i < len(observations); i++ {
            pairDiffs = append(pairDiffs,
                deltas.Diff(observations[i-1].Value, observations[i].Value))
        }
        deltas = append(deltas, SignalDelta{
            Type: key.Type, Source: key.Source,
            Observations: observations, PairDiffs: pairDiffs})
    }

    return renderDeltas(deltas, cmd.JSON)
}
```

### Time-window parsing

**As shipped:** lives at `cmd/signatory/deltas_time.go` +
`cmd/signatory/deltas.go`'s `window()` method, with `--range`
folded in as a fourth window mode. The original sketch below
predates `--range`; the actual implementation honors the same
mutex with one extra arm.

```go
// parseTimeWindow resolves the (--since | --last | --all) trio into a
// concrete filter spec. Mutual exclusion is enforced; default is
// since=24h.
func parseTimeWindow(cmd *DeltasCmd) (TimeWindow, error) {
    set := 0
    if cmd.Since != "" { set++ }
    if cmd.Last > 0   { set++ }
    if cmd.All        { set++ }
    if set > 1 {
        return TimeWindow{}, NewUsageError(
            errors.New("--since, --last, and --all are mutually exclusive"))
    }
    if cmd.All { return TimeWindow{All: true}, nil }
    if cmd.Last > 0 {
        return TimeWindow{Last: cmd.Last}, nil
    }
    raw := cmd.Since
    if raw == "" { raw = "24h" } // default
    cutoff, err := parseTimeShorthand(raw)
    if err != nil {
        return TimeWindow{}, NewUsageError(fmt.Errorf("--since %q: %w", raw, err))
    }
    return TimeWindow{Cutoff: cutoff}, nil
}

// parseTimeShorthand recognizes:
//   - word shortcuts: "today", "yesterday", "last-week", "last-month"
//   - Go duration with day extension: "2d", "12h", "30m"
//   - RFC3339 timestamps: "2026-05-12T19:20:00Z"
func parseTimeShorthand(raw string) (time.Time, error) {
    // ...
}
```

## Non-goals

- **Reaper / GC of old signals.** Addressed in
  [`stalesignals.md`](stalesignals.md) — long-term need, separate
  scope. Deltas don't depend on GC; they happily query the full
  history including ancient rows.
- **Cross-entity deltas.** "What changed about my dependency tree as
  a whole?" is `signatory survey` territory; deltas v1 is
  single-entity.
- **Verdict generation.** The deltas view is observational. Whether
  a delta is *bad* belongs at the analyst layer.
- **Alerts / push notifications.** Outside scope; downstream
  consumers (`cron`, CI configs, etc.) build alerting on top of the
  CLI exit code and JSON output.
- **Signal-resolution display.** `signal_resolutions` rows are
  themselves a kind of meta-delta ("this signal superseded that
  one"). For v1, deltas reads through the unresolved signal history
  via `GetSignals`. Surfacing resolutions as first-class delta
  events is a v2 refinement.

## Phased delivery

The design splits naturally into two phases. Phase 1 lands the
deterministic-diff primitive and the human/machine-readable surfaces.
Phase 2 adds visualization and the agent-facing surface.

### Phase 1 — shipped

- `internal/deltas/` package: pure `Diff` + `RenderText` / `RenderJSON`.
- `cmd/signatory/deltas.go` CLI verb with the flags from the
  table above. `--range T1..T2` was added post-Phase-1 but
  belongs to the same surface — the implementation cost was a
  parser + a TimeWindow field.
- The `--all` over-expansion guard shipped as an interactive
  y/N prompt at >10 distinct collection runs, not the
  originally-spec'd passive stderr warning at >100 observations.
  Rationale: tanstack's actual production data sat at ~18 runs
  in one day, and a passive warning was not load-bearing enough
  to prevent unwanted scrolling. The y/N + `--yes` bypass +
  EOF-defaults-to-no is closer to standard CLI hygiene
  (`apt`/`dnf` style).
- Time-shorthand parsing (`yesterday`, `last-week`, `2d`, RFC3339).
- `internal/deltas/testdata/sample.db` with 9 seeded scenarios
  (8 original adversary shapes + a 4-observation range-window
  probe).

### Phase 2 — partial

- **`signatory_deltas` MCP tool — shipped.** Wraps
  `internal/deltas.Computer` for agent consumption. Wire shape
  mirrors the CLI's `--json` output (same `RenderInput`) with
  three meta fields appended: `truncated`, `groups_total`,
  `groups_returned`. Caps total transitions at 200; agents are
  required to scope the time window explicitly (no implicit
  default; no `--all` equivalent). See
  `internal/mcp/tools/deltas.go` and the routing line in
  `internal/mcp/handshake.go`.

- **`signatory deltas --html` — still deferred.** Static-page
  renderer with per-`(type, source)` time-series charts.
  Mirrors `signatory show-synthesis --html`. Chart-shape
  inventory per signal-value type-shape is the design-pass
  dependency. Open question #3 below.

## Rendering for maximum coherency

Two output modes in Phase 1, each shaped for its consumer:

### `--json` (machine-readable, also Phase 2 MCP feed)

Structured diff blobs grouped by `(type, source)`. Each group
carries the chronological observations plus the per-pair
`ValueDiff` records. Direct consumption by:
- Scripts and CI gates (`jq` filters)
- The Phase 2 MCP tool (no transformation needed)
- The Phase 2 HTML renderer (charts derive from the JSON
  representation)

Shape (sketch):

```json
{
  "target": "pkg:npm/@tanstack/react-router",
  "window": {"kind": "since", "cutoff": "2026-05-11T15:55:00Z"},
  "groups": [
    {
      "type": "version_unpublish_observed",
      "source": "npm-registry",
      "signal_group": "publication",
      "observations": [
        {"collected_at": "2026-05-11T19:20Z", "value": {...}},
        {"collected_at": "2026-05-12T15:55Z", "value": {...}}
      ],
      "pair_diffs": [
        {"added": {...}, "removed": {...}, "changed": {...}}
      ]
    }
  ]
}
```

### Text output (default, human-readable)

The principle: **highlight what changed, suppress what didn't.**
Two design choices baked in:

- **Newest-change is the focal point.** A reader scanning the output
  asks "what changed most recently?" first. The most-recent observation
  carries a CHANGED marker; the diff against the prior observation
  follows inline. Older observations appear as context above.
- **Unchanged signals are suppressed by default.** A target with 40
  signals where only 2 changed should produce a short output, not a
  40-block scroll. `--include-unchanged` (Phase 1 flag) restores the
  full view when the operator wants to confirm "nothing else
  changed either."

Sketch of the text layout for a single changed signal:

```
version_unpublish_observed (npm-registry, publication)
  2026-05-11T19:20:39Z  unpublished_count=0
  2026-05-12T15:55:08Z  unpublished_count=2  ◀ CHANGED
    unpublished_count: 0 → 2
    unpublished_versions: gained 2 entries
        + 1.169.8 (published 2026-05-11T19:26:17Z)
        + 1.169.5 (published 2026-05-11T19:20:42Z)
    most_recent_unpublished_publish_time: added
```

For a signal with no change in the window:

```
attestation_consistency (npm-registry, publication)
  2 observations, no change
```

For multiple changes in one signal (more than two observations with
changes between adjacent pairs), each transition is shown as its
own diff block, chronological top-to-bottom.

The implementation MAY decide to also emphasize the "current state
differs from the baseline" framing — comparing the latest observation
to the oldest in the window rather than only to the immediately
prior. v1 does pair-wise (latest-vs-prior); a `--vs-baseline` flag
is a small Phase 1 follow-up if the pair-wise framing reads
poorly in practice.

### Output-channel discipline

- The structured JSON or text goes to stdout.
- The `--all` prompt, parse errors, and progress narration go
  to stderr.
- Mirrors the discipline of other signatory verbs (`analyze`,
  `survey`).

## Open questions

1. ~~Group-by-group rendering vs. chronological-stream rendering.~~
   **Resolved**: shipped group-by. A `--chronological` flag has not
   been requested in dogfood; leave for a future ask.

2. ~~`--vs-baseline` framing for Phase 1.~~ **Deferred**: not
   shipped. Pair-wise framing has worked well in dogfood (intraday
   drift on tanstack reads naturally as "this changed, then this
   changed"). Revisit if a consumer surfaces a specific gap.

3. **Phase 2 HTML chart shapes per signal type.** Still open.
   Numeric fields plot naturally on a Y-axis (e.g.,
   `unpublished_count`, `divergence_days`, `non_human_count`).
   Categorical fields are state-change timelines (e.g.,
   `shape="active-repo-paused-publishes"`). Array-valued fields
   (e.g., `workflow_refs`) need a different visualization —
   possibly a stacked-row timeline. The Phase 2 design pass will
   need to inventory the signal-value type-shapes and spec a
   chart-per-shape mapping before `--html` can be implemented
   coherently.

## Cross-references

- [`stalesignals.md`](stalesignals.md) — the design discipline
  underlying the append-only signal store. The deltas view is the
  presentation layer that exposes the history that doc preserves.
- [`threat-landscape/`](threat-landscape/) — the eight test
  scenarios above are drawn directly from these entries. The threat-
  landscape corpus is the spec for what a deltas view should make
  visible.
- [`trust-model.md`](trust-model.md) §"Trust is mutable state" —
  *"The data model must support retroactive degradation of trust
  signals. This is not a static scorecard."* Deltas operationalize
  that principle.
- `internal/store/sqlite.go` — `GetSignals`, `GetLatestSignals`,
  `signal_resolutions` table. The storage primitives this design
  consumes.
- `/tmp/signal-sketch.md` — Tier 1 and Tier 2 sketches that drove
  the architectural realization. Sketch 1 (`commit_publish_cadence_divergence`)
  showed that derived-signal composition was already supported via
  `WithInRun`; that same pattern recognition applies here — the
  deltas view is a presentation layer over already-supported
  storage, not a new architectural primitive.

## Implementation order

### Phase 1 — done

1. ✓ Pure `internal/deltas/diff.go` + 8 adversary-scenario tests.
2. ✓ `internal/deltas/render.go` text + JSON with newest-change
   highlighting and unchanged-signal suppression.
3. ✓ `cmd/signatory/deltas.go` CLI verb. The COUNT-warning
   originally spec'd here was replaced by the y/N prompt at >10
   runs (see Phase 1 section above for the rationale).
4. ✓ Time-shorthand parsing.
5. ✓ End-to-end tests against `internal/deltas/testdata/sample.db`
   (committed, 9 scenarios).
6. ✓ Manual dogfood: ran `signatory analyze --refresh
   pkg:npm/@tanstack/react-router` twice on the same day, then
   `signatory deltas pkg:npm/@tanstack/react-router --all`
   surfaced the real intraday drift (stars/forks counts, owner
   profile, last_push date, cadence divergence).

### Phase 1.5 — done (post-Phase-1 refinements)

A. ✓ `--range T1..T2` CLI flag. Syntax mirrors git rev-range;
   each endpoint accepts the same time-shorthand as `--since`;
   inclusive on both ends; rejects start>end.
B. ✓ Extracted `internal/deltas.Computer` to share the
   store→filter→window→diff path between the CLI and MCP. The
   CLI verb is now a thin shell around `Computer.Compute`.
C. ✓ Fixed the e2e fixture's gitignore exclusion. `sample.db`
   was being silently dropped by the broad `*.db` rule; an
   explicit negation now tracks it.

### Phase 2 — partial

7. **Deferred:** `signatory deltas --html <target>` —
   chart-shape inventory needed first; see Open Question #3.
8. ✓ MCP tool `signatory_deltas`. Required `target`; mutex on
   `since` / `last` / `range_start+range_end` (at least one;
   no implicit default; no `--all` equivalent); optional
   `type` / `source` / `group` filters. Response inlines the
   `RenderInput` shape plus `truncated` / `groups_total` /
   `groups_returned`. 200-transition cap. Strict
   `additionalProperties:false`. See `internal/mcp/tools/deltas.go`.
