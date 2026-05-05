# The analysis flow

This page is the answer to "I want to assess X — what do I run, in what
order, and how do I check it worked?"

It documents three surfaces that look related but answer different
questions:

| Surface | What it is | What it produces |
| --- | --- | --- |
| `signatory <cmd>` (CLI) | Local Go binary. Deterministic. Mechanical. | Layer 1 signals; cached views of stored postures, analyses, conclusions. |
| `signatory_*` (MCP) | Read/write tools the binary exposes over stdio. Same store as the CLI. | Same data the CLI reads, structured as JSON for an LLM caller. Plus one mutating tool — `signatory_ingest_analysis`. |
| `/analyze` (Claude skill) | Orchestrator that runs in a Claude Code session. | Layer 2 reasoned conclusions + a synthesist verdict. Calls the CLI for collection and the MCP write tool for ingest. |

The single most common mistake — the one this page exists to prevent —
is treating these as interchangeable. They are not. `signatory analyze`
the CLI command does **not** produce an analysis. `/analyze` does. The
verb collision is a known scope wart (see `cmd/signatory/analyze.go`
header for the history).

## Mental model: the two-layer split

Every assessment in signatory has two layers, and they are produced by
different tools.

**Layer 1 — mechanistic.** Signals that a Go program collects from
forges, registries, git, and well-known files. Examples:

- GitHub API — repo metadata, default branch, archived flag, primary
  language.
- Git — commit history, signing presence, age of HEAD.
- Package registry — npm `package.json`, PyPI `project_urls`, Go
  `pkg.go.dev` index, etc.
- Repofiles — `.github/workflows/*.yml`, `SECURITY.md`, `CODEOWNERS`,
  `.goreleaser.yaml`.
- OpenSSF Scorecard — `api.securityscorecards.dev` per-check scores
  (added 2026-04-29 in commit `2787667`).

These are deterministic, cacheable, and cheap. Layer 1 is what
`signatory analyze --refresh` populates. The MCP tool that returns raw
Layer 1 data is `signatory_signals`; the summarized view is
`signatory_analyze`'s `signals_summary` field.

**Layer 2 — reasoned.** Conclusions, positive absences, observations,
methodology traces, and a synthesist verdict. Examples:

- "This repo's release process is automated end-to-end via tag-driven
  workflow `release.yml`; an attacker would need to compromise a
  protected tag." (conclusion + rationale)
- "No `prepare`/`postinstall` lifecycle script; supply-chain runtime
  risk during `npm install` is therefore absent." (positive absence)
- "Recommend `vetted-frozen` at v1.2.3 with a 90-day re-review trigger
  on the next major version bump." (synthesist proposal)

These are produced by LLM analyst agents (security, provenance) and
distilled by a synthesist agent. Layer 2 is what `/analyze` produces.
The store row carrying Layer 2 is an `analyst_output`. The MCP tool
that lists them is `signatory_show_analyses`; per-concern drill-down
is `signatory_show_conclusions`.

The `signatory_summary` MCP tool composes both layers into one
response — it is the "start here" verb for any assessment question.

## Routing: pick the right surface

Before doing anything, decide what question you're answering. The
right tool is almost always cheaper than the wrong one.

| Question | First call | Why |
| --- | --- | --- |
| "What do we know about X?" | `signatory_summary` (MCP) or `signatory summary <target>` (CLI) | Composes posture + burn + analyses rollup + related identities in one call. Replaces the show-analyses → show-conclusions → posture get flail. |
| "Is X safe? / Is X trustworthy?" | `signatory_analyze` (MCP) — cache-only lookup | If it returns OK, answer from the cached profile. If `NotFound`, escalate to `/analyze`. |
| "Show me the raw evidence for X." | `signatory_signals` (MCP) | Returns every Layer 1 signal record verbatim — payload, source, forgery_resistance, expiry. |
| "What has signatory analyzed recently?" | `signatory_show_analyses` (MCP) or `signatory show-analyses` (CLI) | Lists Layer 2 outputs, optionally filtered by target / analyst / since. |
| "Show me every conclusion about <concern>." | `signatory_show_conclusions` (MCP) | Searches Layer 2 conclusions across analyses with text/tag filters. |
| "What methodology patterns has the corpus covered?" | `signatory_show_methodology` (MCP) | Aggregates `methodology_trace` entries across analyses. |
| "Refresh Layer 1 for X — collectors only, no LLM." | `signatory analyze --refresh <target>` (CLI) | Mechanistic. No analyst dispatch. Writes signals to the store. |
| "Run a fresh full analysis for X." | `/analyze <target>` (Claude skill) | Layer 1 refresh + analyst dispatch + synthesis + posture proposal. Expensive. |
| "Walk every dep in my project." | `signatory survey <manifest>` (CLI) | **v0.1 stub.** Recognizes manifests but does not yet parse them. Plan is per-dep `/analyze` until v0.2. See "Known gaps" below. |
| "Record a posture I already decided." | `signatory posture set` / `posture accept` (CLI) | See `recording-trust-decisions.md`. |

If you find yourself reading `go.mod` directly, you've routed wrong.
The store is the source of truth for "what have we assessed"; the
manifest only matters when scoping a survey, and survey is a v0.2
feature.

## The full `/analyze` pipeline, step by step

This is what the Claude `/analyze` skill orchestrates. Each step names
the CLI command it runs (so you can replicate it by hand) and the
artifact it produces.

```
Step 0   check store         → signatory show-analyses + show-conclusions
Step 0a  preflight TLS       → signatory certs check
Step 1   sessions + handoffs → signatory serve start
                               signatory pipeline session create
                               signatory analysis begin
                               signatory handoff security|provenance
Step 1b  refresh Layer 1     → signatory analyze --refresh --path …
Step 2   dispatch analysts   → Claude Agent tool x2 (parallel)
                               agents call signatory_ingest_analysis
Step 3   verify landed       → signatory show-analyses
                               signatory analysis show <ANALYSIS_SID>
Step 4   dispatch synthesist → signatory handoff synthesist
                               Claude Agent tool x1
                               agent calls signatory_ingest_analysis
Step 5   record posture      → signatory posture accept <output-id>
Step 6   close session       → signatory analysis end <ANALYSIS_SID>
```

The detail per step is in `.claude/skills/analyze/SKILL.md`. What
follows here is the mapping you need to replicate by hand.

### Step 0 — check the store before doing anything

Always cheaper than re-collecting.

```bash
signatory summary "$TARGET"                  # one-call composed view
signatory show-analyses "$TARGET"            # list Layer 2 outputs
signatory show-conclusions --target "$TARGET" # drill into conclusions
```

MCP equivalents (for an agent context): `signatory_summary`,
`signatory_show_analyses`, `signatory_show_conclusions`.

If the target is in the store with current data, **stop**. Hand the
existing assessment to whoever asked. Re-running is a token bill, not
a feature.

### Step 0a — TLS preflight

Only relevant when the orchestrator is going to dispatch agents that
WebFetch the local pipeline service. Skip for pure-CLI flows.

```bash
signatory certs check     # exit 0 ok; non-zero needs `certs init`
```

### Step 1 — sessions + handoffs

Two different "session" concepts; both are required for a full
`/analyze` run, neither is needed for a CLI-only Layer-1 refresh.

- **Pipeline session** (`SESSION_ID`): transport-layer. The HTTP
  service holds rendered handoff prompts that dispatched agents
  retrieve via WebFetch. Created with `signatory pipeline session
  create`.
- **Analysis session** (`ANALYSIS_SID`): audit identity. Lives in the
  signatory store. Every analyst output the run lands carries this id
  via the `analysis_session_id` FK so `signatory analysis show
  <id>` and `signatory analysis timing <id>` surface the linked
  outputs. Created with `signatory analysis begin`.

```bash
signatory serve status >/dev/null 2>&1 || signatory serve start
SESSION_ID=$(signatory pipeline session create "$TARGET")
ANALYSIS_SID=$(signatory analysis begin "$TARGET" \
  --expected-analyst signatory-security-v1 \
  --expected-analyst signatory-provenance-v1 \
  --expected-analyst signatory-synthesis-v1 \
  --pipeline-session-id "$SESSION_ID")

signatory handoff security "$TARGET" \
  --analysis-session-id "$ANALYSIS_SID" \
  --network-precheck --clone-dir filestore/clones/ \
  --deposit-to "$SESSION_ID"

TARGET_NAME=$(basename "$TARGET" .git)
signatory handoff provenance "$TARGET" \
  --analysis-session-id "$ANALYSIS_SID" \
  --network-precheck --path "filestore/clones/$TARGET_NAME" \
  --deposit-to "$SESSION_ID"
```

`signatory handoff` clones the repo (security handoff does the clone;
provenance reuses the path) and renders a role-specific prompt from
`templates/handoffs/`. The `--deposit-to` flag posts the rendered bytes
to the pipeline service so an agent can WebFetch them — this is the
workaround for WebFetch being GET-only.

### Step 1b — Layer 1 refresh

This is the step that populates `signatory_signals` so dispatched
analysts don't WebFetch the same data themselves.

```bash
signatory analyze --refresh \
  --path "filestore/clones/$TARGET_NAME" \
  "$TARGET"
```

Per-ecosystem collector dispatch (verified):

- `pkg:golang/...` → gopublish + github + git + repofiles + openssf
- `pkg:npm/...` → npm + github + git + repofiles + openssf
- `pkg:pypi/...` → pypi + github + git + repofiles + openssf
- `repo:github/...` → github + git + repofiles + openssf

The openssf collector is the one added in commit `2787667`; it gates
on `isGitHostedEntity` so non-GitHub targets emit empty results
without erroring (`internal/signal/openssf/collector.go`).

This step is also the right CLI verb for "I want to refresh Layer 1
without doing a full `/analyze`" — e.g. testing a new collector. The
output exits 0 on success and prints per-collector summaries to
stderr.

### Step 2 — dispatch analyst agents (Claude only)

This step has no CLI equivalent. It requires the Claude Code `Agent`
tool. Two parallel agents dispatched in one message:

- security analyst (`allowed-tools: Read Glob Grep WebFetch
  mcp__signatory__signatory_ingest_analysis`)
- provenance analyst (same toolset)

Each agent retrieves its handoff via WebFetch from the pipeline
service, performs analysis on the local clone, and calls
`signatory_ingest_analysis` with a v1-schema JSON payload.

The MCP write tool validates the payload against the v1 schema before
inserting; on `CodeSchemaViolation` the agent fixes and retries in the
same turn. Markdown intermediate files are not part of this flow.

### Step 3 — verify analyst output landed

```bash
signatory show-analyses "$TARGET"             # all rounds, all targets
signatory analysis show "$ANALYSIS_SID"        # this run's expected vs landed
```

Expected: two rows per target, one per analyst role
(`signatory-security-v1`, `signatory-provenance-v1`). The `analysis
show` view names the missing analyst directly in its `missing:` line
when one didn't land — that's the most direct signal of who failed.

### Step 4 — dispatch synthesist (Claude only)

```bash
signatory handoff synthesist "$TARGET" \
  --analysis-session-id "$ANALYSIS_SID" \
  --deposit-to "$SESSION_ID"
```

The synthesist handoff inlines every analyst conclusion + positive
absence + observation in its body, so the agent does not query the
store directly. One agent dispatched; it produces a synthesis-supplement
JSON and ingests it via `signatory_ingest_analysis`. Returns an
`output_id` that Step 5 needs.

### Step 5 — record posture (user decision)

```bash
signatory posture accept "$SYNTHESIS_OUTPUT_ID" --yes
```

The user can override the synthesist's tier or rationale via flags
(`--tier`, `--rationale-file`) without re-running synthesis. See
`recording-trust-decisions.md`.

### Step 6 — close the session

```bash
signatory analysis end "$ANALYSIS_SID" \
  --status completed \
  --synthesis-output-id "$SYNTHESIS_OUTPUT_ID"
```

Statuses: `completed`, `failed`, `partial`. One-way transition;
re-running `/analyze` on the same target opens a fresh session.

## CLI replication: which steps you can do by hand

Without invoking `/analyze`:

| Step | CLI replicable? | Notes |
| --- | --- | --- |
| 0  Check the store | Yes | `signatory summary` / `show-analyses` / `show-conclusions`. |
| 0a Preflight TLS | Yes | `signatory certs check`. |
| 1  Sessions + handoffs | Yes | All four CLIs work standalone. The handoff `.md` is just a rendered prompt; you can read it. |
| 1b Layer 1 refresh | **Yes — primary CLI use case.** | This is *the* CLI verb for "populate signals without LLM". Use directly when testing a collector. |
| 2  Analyst dispatch | **No.** | Requires the Claude Code `Agent` tool. The CLI cannot dispatch an LLM by Invariant 1 (no Anthropic SDK in the binary). |
| 3  Verify landed | Yes | `signatory show-analyses` / `analysis show`. |
| 4  Synthesist dispatch | **No.** | Same Invariant 1 reason. The handoff renderer is CLI; the dispatch is not. |
| 5  Record posture | Yes | `signatory posture set` / `posture accept`. |
| 6  Close session | Yes | `signatory analysis end`. |

The Layer 1 cone of the pipeline is fully scriptable. The Layer 2
cone is not. That is the v0.1 invariant boundary, not an oversight.

## MCP tool reference

Read tools (no side effects):

| Tool | Returns | Use when |
| --- | --- | --- |
| `signatory_summary` | Composed view: posture + burn + analyses rollup + related identities. | First call for any "what do we know about X" question. |
| `signatory_analyze` | Cached profile: entity + signals_summary + posture. | "Is X safe?" — cache lookup, not a live scan. `NotFound` means escalate to `/analyze`. |
| `signatory_signals` | Every Layer 1 signal record for one target, raw. | Spot-checking collector output, debugging, comparing across targets. |
| `signatory_detail` | One analyst output by id, full payload. | Drill into a specific row found via `show-analyses`. |
| `signatory_show_analyses` | List of analyst outputs, filterable. | "What's been analyzed?" / "Are there analyses of X?" |
| `signatory_show_conclusions` | Conclusions across analyses, filterable. | "What does the corpus say about <concern>?" |
| `signatory_show_methodology` | Methodology patterns across analyses. | "Has anyone covered MP-GO-HYG-02?" |
| `signatory_show_synthesis` | Rendered synthesis markdown for one output. | Reading a synthesist's verdict in human form. |
| `signatory_survey` | **v0.2 stub.** Confirms manifest is recognized; returns `CodeNotFound` for parsing. | When v0.2 lands; today, fall back to per-dep. |

Write tools (mutating):

| Tool | What it writes | Caller |
| --- | --- | --- |
| `signatory_ingest_analysis` | One `analyst_output` row (Layer 2). v1-schema validated. Idempotent on payload hash. | Analyst agents and the synthesist agent during `/analyze`. Not a humans-call-it tool. |

There is no MCP tool for posture / burn / signal writes — those go
through the CLI. The MCP write surface is intentionally narrow:
analyst output ingest, full stop.

## Verification recipes

### Did Layer 1 collection land?

```bash
signatory analyze --refresh --path filestore/clones/$NAME $TARGET
# then via MCP from a Claude session:
#   signatory_signals(target: $TARGET)
# inspect: count, types, sources, forgery_resistance
```

For a new collector specifically (e.g. testing the openssf one):

```bash
# After --refresh, query signals filtered to your collector's signal
# type. The openssf collector emits signal_type=scorecard-check with
# source=openssf-scorecard.
signatory_signals(target: $TARGET)
# look for: source: "openssf-scorecard"
#           type: "scorecard-check"
#           value: { score: <num>, checks: { ... } }
```

### Did a Layer 2 analyst output land?

```bash
signatory show-analyses "$TARGET"
# expect: rows for signatory-security-v1 + signatory-provenance-v1
#         + signatory-synthesis-v1 (after Step 4)
```

Or session-scoped, post-`/analyze`:

```bash
signatory analysis show "$ANALYSIS_SID"
# expected-vs-landed rollup names any missing analyst
```

### Did the dogfood pain (cache-miss) actually go away?

This is the integration outcome and only meaningful inside a captured
`/analyze` session. After a run:

```bash
dogfood-metrics report <session-id>
# read dogfood-metrics/sessions/<session-id>/report.md
# look at: External calls (cache-miss candidates)
# the URL you closed (e.g. api.securityscorecards.dev) should drop to 0
```

Without this step, you've verified the unit (the collector wrote a
signal) but not the purpose (analysts stopped re-fetching).

## Known interface gaps in v0.1

These are real gaps, not user error. If you hit one, you didn't route
wrong; the surface isn't there yet.

- **`signatory survey <manifest>` does not parse manifests.** Returns
  `CodeNotFound` with a `Use signatory_analyze per-dependency as a
  workaround until v0.2 survey support lands` hint. Affected
  ecosystems: go, npm, pypi, cargo. Tracked: design/potential-survey-mcp.md.
- **`signatory analyze` (CLI) is named for a verb it does not perform.**
  The CLI collects Layer 1 signals only; it does not produce a trust
  verdict. The `/analyze` Claude skill is the actual analysis pipeline.
  Scope mismatch tracked in the command's docstring header.
- **No CLI verb for "show me which deps need a fresh `/analyze` run."**
  Operators eyeball `signatory show-analyses --since` and compare to
  their own dep list. Survey is meant to close this; v0.2.
- **`signatory_analyze` MCP tool's `refresh:true` parameter is a
  no-op in v0.1.** Layer 1 refresh is a CLI side effect, not an MCP
  capability. To refresh, run the CLI; then the next MCP read returns
  fresh data.
- **No MCP write tool for posture / burn.** Decisions go through the
  CLI by design (operator intent is auditable from terminal history).

If you find yourself working around one of these, write it up — the
gaps page in this manual is where corrections to the interface
surface land.

## See also

- `.claude/skills/analyze/SKILL.md` — full orchestrator script with
  step-by-step rationale.
- `design/mcp-dual-analyst-architecture.md` — why the security and
  provenance roles are split, and what their handoff templates encode.
- `design/v0.1-invariants.md` — the four invariants that explain why
  the LLM dispatch lives in Claude, not in the binary.
- `design/agent-facing-contract.md` — wire-level contract for every
  MCP tool listed above.
- `recording-trust-decisions.md` — the posture / burn surface this
  page references but does not duplicate.
