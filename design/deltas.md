# Signal Deltas — Cheap, Deterministic Change Detection

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
| Time-window flags | `--since` (words / durations / timestamps), `--last N`, `--all` |
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
| `--last <n>` | Show the most recent `n` observations per `(type, source)` group. Mutually exclusive with `--since`. |
| `--all` | Show the full history. Mutually exclusive with `--since` and `--last`. Emits a stderr warning when the result set would exceed 100 observations total; processes the full set regardless. |
| `--type <name>` | Filter by signal type (e.g. `trusted_publishing`, `version_unpublish_observed`). |
| `--source <name>` | Filter by collector source (e.g. `npm-registry`, `github`, `git`). |
| `--group <name>` | Filter by signal group (vitality / governance / publication / hygiene / criticality / identity). |
| `--json` | Emit structured JSON instead of human-readable text. |

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
```

### Time parsing extension

The existing `parseSinceFlag` in `cmd/signatory/analysis.go:407`
already handles Go duration + RFC3339. We extend it to recognize
word-shaped inputs before falling through to duration parsing:

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

### File layout

```
internal/deltas/
  doc.go              package comment, public API
  diff.go             pure function Diff(prior, current map[string]any) ValueDiff
  diff_test.go        the eight scenarios above plus edge cases
  render.go           render(ValueDiff) → text (human) / JSON (structured)
  render_test.go      golden-file tests for render output

cmd/signatory/
  deltas.go           CLI verb: DeltasCmd struct + Run method
  deltas_test.go      end-to-end with seeded store

cmd/signatory/main.go modified: add Deltas subcommand to the kong CLI
                      structure
```

### Public API of internal/deltas

```go
package deltas

// ValueDiff carries the structural diff between two signal values.
// Top-level operation is per-key add/remove/change; nested objects
// recurse; arrays use length+stable-key heuristics. See design/deltas.md.
type ValueDiff struct {
    Added   map[string]any       // keys present in current, absent in prior
    Removed map[string]any       // keys present in prior, absent in current
    Changed map[string]Change    // keys present in both with differing values
}

// Change carries the before/after pair for a single changed key,
// plus a kind discriminator so renderers can dispatch on shape.
type Change struct {
    Kind     ChangeKind  // "scalar", "object", "array", "opaque"
    Before   any
    After    any
    Nested   *ValueDiff  // populated when Kind == "object"
    Elements []ElementChange // populated when Kind == "array" with positional/keyed alignment
}

type ChangeKind string

// SignalDelta is the unit the CLI verb emits: a (type, source) group
// plus the chronological observation pairs and the diff between each.
type SignalDelta struct {
    Type        string
    Source      string
    Group       string
    Observations []Observation  // chronological
    PairDiffs   []ValueDiff     // len = len(Observations) - 1
}

type Observation struct {
    CollectedAt time.Time
    Value       map[string]any
}

// Diff is the pure-function workhorse.
func Diff(prior, current map[string]any) ValueDiff
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

Extend (or replace) `parseSinceFlag` in `cmd/signatory/analysis.go`:

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

### Phase 1 (this design's implementation scope)

- `internal/deltas/` package: pure `Diff` function + `Render` text/JSON.
- `cmd/signatory/deltas.go` CLI verb with the flags spec'd above
  (`--since`, `--last`, `--all`, `--type`, `--source`, `--group`,
  `--json`).
- COUNT-based pre-query before `--all` to detect large result sets;
  stderr warning at > 100 observations total.
- Time-shorthand parsing extension (`yesterday`, `last-week`,
  `2d`, RFC3339).
- The eight adversary-shape test scenarios.

### Phase 2 (follow-up scope, separate design pass)

- **`--html` output for time-series visualization.** Mirrors
  `signatory show-synthesis --html`'s static-page model. A chart
  per `(type, source)` group plotting numeric fields over
  `collected_at`; categorical fields rendered as state-change
  timelines. Useful for "show me the trend of attestation
  consistency over the last 30 days."
- **MCP tool `signatory_deltas`.** Wraps the same query+diff path
  for agent consumption during analysis-skill flows. The JSON
  output of Phase 1 is already structured for this; Phase 2 is
  just the MCP wiring.

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
- The COUNT-warning, parse errors, and progress narration go to
  stderr.
- Mirrors the discipline of other signatory verbs (`analyze`,
  `survey`).

## Open questions

1. **Group-by-group rendering vs. chronological-stream rendering.**
   Two natural shapes for the Phase 1 text output:
   - Group-by-(type, source): one signal type at a time, all its
     observations, all its diffs (the sketch above).
   - Chronological stream: interleave changes across signal types in
     the order they occurred.

   Group-by is easier to read for one-signal investigation; stream is
   better for "what happened on day X." Phase 1 ships group-by; a
   `--chronological` flag can be added later if the stream framing
   proves useful.

2. **`--vs-baseline` framing for Phase 1.** As noted above, pair-wise
   diffs may sometimes obscure the "current vs starting point"
   reading that consumers want. Could be a small flag that compares
   the latest observation against the oldest in the window directly,
   skipping intermediate diffs. Defer until pair-wise feedback
   surfaces a clear gap.

3. **Phase 2 HTML chart shapes per signal type.** Numeric fields
   plot naturally on a Y-axis (e.g., `unpublished_count`,
   `divergence_days`, `non_human_count`). Categorical fields are
   state-change timelines (e.g., `shape="active-repo-paused-publishes"`).
   Array-valued fields (e.g., `workflow_refs`) need a different
   visualization — possibly a stacked-row timeline. Phase 2 design
   pass will need to inventory the signal-value type-shapes and
   spec a chart-per-shape mapping.

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

### Phase 1 (this implementation)

1. Pure `internal/deltas/diff.go` + tests for all eight adversary
   scenarios (RED → GREEN).
2. `internal/deltas/render.go` text and JSON output (golden tests),
   newest-change highlighting + unchanged-signal suppression.
3. `cmd/signatory/deltas.go` CLI verb wiring with `GetSignals`,
   filter, group, diff, render. Includes COUNT pre-query for the
   `--all` warning.
4. Time-shorthand extension to `parseTimeShorthand` (`yesterday`,
   `last-week`, `2d`, RFC3339).
5. End-to-end test against a seeded store.
6. Manual dogfood: run `signatory analyze --refresh` twice on a
   target (with a few-minute gap to ensure distinct timestamps),
   then `signatory deltas <target>` and verify the second
   observation produces a coherent diff against the first.

### Phase 2 (separate design pass + implementation)

7. `signatory deltas --html <target>` — static-page renderer with
   per-`(type, source)` time-series charts. Models on
   `signatory show-synthesis --html`. Chart-shape inventory per
   signal-value type-shape is the design-pass dependency.
8. MCP tool `signatory_deltas(target, since, last, all, type, source,
   group)` — wraps the same query+diff path that the JSON CLI output
   already produces. Documentation in the MCP server's tool list.
