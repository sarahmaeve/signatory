# Agent OTEL: dogfood telemetry for /analyze

Status: **planning** as of 2026-04-27. Captures the architecture for
observing /analyze runs from outside the LLM — token costs per
subagent, tool-call patterns, local-DB-vs-external classification,
evidence of underspecification — without relying on LLM
self-reporting.

## Why

V0.1 work (per `design/ROADMAP.md`) calls for "improve economics" and
"validate through dogfood." Both depend on being able to *see* what
each /analyze run actually does:

- Tokens spent per analyst (security-review, provenance-review,
  synthesist) — the cost story we'll need to optimize against
- Tool calls per analyst, classified — local DB hits vs external
  WebFetch/curl/git API calls (any external call to data we already
  have is a cache miss, per the ROADMAP's framing)
- Reads against signatory's own source tree — the analyst should
  never need to read `internal/...` or `cmd/signatory/...`. If it
  does, the handoff or MCP surface didn't tell it what it needed
  (underspecification signal)

LLM self-reporting can't be trusted for this. The instrumentation
must observe from outside — at the OTEL trace layer and the
Claude Code hook layer — and correlate after the fact.

## What was verified

Three rounds of verification against current Claude Code docs. Each
round corrected a load-bearing assumption from the prior:

| Round | Verified | Notable correction to prior thinking |
|---|---|---|
| 1 | Subagent CWD inheritance, hook input fields, OTEL `query_source` and `subagent_type` attributes, project-scoped settings precedence, transcript JSONL location | Hooks alone can't attribute calls to subagents — OTEL is required for that |
| 2 | OTEL exporter options (`otlp` / `console` / `prometheus` / `none`) | **No native file exporter exists.** `otlp` is network-only, `console` is unstructured text |
| 3 | Hook payload includes `session_id` + `transcript_path`, OTEL `session.id` resource attribute matches hook `session_id`, MCP tool calls under subagents nest correctly under the subagent span, `OTEL_LOG_TOOL_DETAILS=1` exposes `full_command` for Bash | Round 1 said hooks didn't have `transcript_path`; round 3 confirmed they do. Stop-hook end-of-session aggregates ARE possible (deferred for now) |
| 4 | `OTEL_EXPORTER_OTLP_PROTOCOL=http/json` is first-class supported (not experimental); standard OTLP/HTTP paths `/v1/traces` and `/v1/logs`; wire bodies follow OTLP JSON Mapping spec strictly (`resourceSpans` / `resourceLogs` top-level); 202 Accepted on success | **Trace export requires `CLAUDE_CODE_ENHANCED_TELEMETRY_BETA=1`** — the OTEL config itself is stable, but trace data (which we need for per-subagent attribution) is beta-gated. Compression behavior is undocumented — receiver must handle both gzipped and ungzipped bodies defensively |
| 5 (live, 2026-04-27) | First real capture against `claude` launched in another window. Hooks fired correctly (settings.json read), receiver was reachable, OTEL trace stream stayed empty | **Initial blame attributed to settings.json `env` block scope.** Round 6 contradicts that — see Round 6 row. The env-block claim in this row is **suspect and pending proper diagnosis**, NOT a confirmed fact. The wrapper script `scripts/dogfood-claude.sh` was added as a defensive workaround; whether it's actually needed depends on Round 6's investigation |
| 6 (2026-04-27) | All telemetry config verified against current docs (env var names, span names, attribute names, token field names, OTLP wire shape). Plus: hook payload includes `hook_event_name` (Round 1 missed it; useful — we now prefer it over the `--event` flag); `SubagentStop` event DOES exist (Round 1 wrongly said no); PreToolUse blocking via `hookSpecificOutput.permissionDecision` (top-level `decision` is deprecated, we don't block so non-issue); token attributes confirmed as `input_tokens`, `output_tokens`, `cache_read_tokens`, `cache_creation_tokens` on `claude_code.llm_request` — unblocks slice 5 design | **Live docs say settings.json `env` block applies to "every session," which the verifying agent read as including Claude Code's own process.** Our Round 5 symptom contradicts this. Per the project's discipline (don't blame the upstream until you've ruled out your own reading, your own implementation, and your own testing), the conflict is a flag that one of those three is wrong. To investigate properly when we get to it: (a) verify the env block reaches Claude Code's process (e.g., have the spawned `claude` print `os.environ` somehow), (b) verify the OTEL exporter is initialized (some other startup signal), (c) verify the receiver actually rejects nothing (e.g., introduce a deliberate test span). Don't rewrite the wrapper script's rationale until that diagnosis is done |

Cross-cutting confirmations from round 3:

- `claude_code.llm_request` spans carry `query_source` (subagent name)
  as documented — not spec-aspirational
- `claude_code.tool` spans carry `full_command` for Bash when
  `OTEL_LOG_TOOL_DETAILS=1`, enabling Bash classification at the
  post-processor level
- Subagent spans nest under the parent's `claude_code.tool` (Task)
  span, so per-subagent attribution works for MCP calls — which
  dominate /analyze tool usage

## Architectural decision: write our own collector

The standard OTEL ingest path would be `otelcol-contrib` (the
OpenTelemetry Collector contrib distribution) receiving OTLP and
writing OTLP-JSON to disk via the `file` exporter. This was the
initial plan, then dropped on review:

- **Dependency vetting cost.** Adopting `otelcol-contrib` means
  vetting a multi-hundred-thousand-LOC Go binary plus its dep tree
  through signatory's own /vet-dependency or /analyze pipeline. The
  cost is real and out of proportion to what we need.
- **What we actually need is a tiny HTTP server.** Claude Code
  supports `OTEL_EXPORTER_OTLP_PROTOCOL=http/json`, so OTLP arrives
  over HTTP as JSON-encoded protobuf (per the OTLP JSON Mapping
  spec). A small Go receiver (`net/http` + `encoding/json` from
  stdlib) accepts POST `/v1/traces` and `/v1/logs`, writes the JSON
  bodies to disk, and that's the entire receiver job.
- **Zero new external deps.** Hand-rolled minimal struct types
  match just the OTLP fields we care about
  (`service.name`, `claude_code.llm_request.query_source`,
  `claude_code.tool.full_command`, `session.id`, span hierarchy).
  Anything else decodes into `json.RawMessage` and persists
  verbatim — so the on-disk format stays OTLP-JSON-compatible for
  later SigNoz replay or any other OTEL-aware downstream.
- **Aligns with signatory's identity.** We're a Go shop building a
  trust-analysis tool; carrying our own ~300-LOC OTLP receiver is
  consistent with the project's stance on dependency footprint.

## Architecture

```
Claude Code session  (launched via scripts/dogfood-claude.sh,
                      which exports the env vars below before exec'ing claude;
                      see "Round 5" — settings.json env block doesn't reach
                      Claude Code's own process, only its subprocesses)
  ├─ env (must be SHELL-EXPORTED, not just in settings.json):
  │       CLAUDE_CODE_ENABLE_TELEMETRY=1
  │       CLAUDE_CODE_ENHANCED_TELEMETRY_BETA=1   # required for trace export
  │       OTEL_TRACES_EXPORTER=otlp
  │       OTEL_LOGS_EXPORTER=otlp
  │       OTEL_EXPORTER_OTLP_PROTOCOL=http/json
  │       OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318
  │       OTEL_LOG_TOOL_DETAILS=1
  │
  ├─ OTLP/HTTP/JSON ──> localhost:4318
  │                       POST /v1/traces  (ExportTraceServiceRequest)
  │                       POST /v1/logs    (ExportLogsServiceRequest)
  │                       (cmd/dogfood-metrics serve, returns 202)
  │                       └─ writes dogfood-metrics/raw/otel.jsonl
  │
  └─ PreToolUse hook ──> scripts/dogfood-hook.sh
                           └─ writes dogfood-metrics/raw/hooks-<session_id>.jsonl
                              (real-time classification)

cmd/dogfood-metrics report <session-id>
  reads dogfood-metrics/raw/{otel.jsonl, hooks-<session_id>.jsonl}
  joins on session_id
  emits dogfood-metrics/sessions/<session-id>/report.md
```

**Receiver must handle gzipped bodies.** Claude Code's compression
behavior is undocumented; check `Content-Encoding: gzip` on each
incoming request and decompress before JSON parsing.

**The `.claude/settings.json` `env` block is left in place** even
though it doesn't reach Claude Code's own process. It DOES apply
to subprocesses Claude Code spawns (Bash tool calls, hook scripts),
where these `OTEL_*` vars are harmless. Removing it would lose the
documentation value of "here's what env Claude Code expects,"
which a future maintainer reading the settings would want to see.

## Per-subagent attribution

Three mechanisms collaborate:

1. **`query_source` attribute** on `claude_code.llm_request` spans
   names the subagent that issued the LLM request.
2. **Span nesting under Task** — when a parent dispatches a subagent
   via the Task tool, the subagent's `llm_request` and `tool` spans
   nest under the parent's `claude_code.tool` (Task) span. Walking
   the trace tree reveals the delegation graph.
3. **`session.id` resource attribute** equals the hook payload's
   `session_id`, providing the join key between the OTEL stream and
   the hook stream.

For MCP tool calls (`mcp__signatory__*`), which dominate /analyze:
they appear as `claude_code.tool` spans nested under whichever
subagent issued them. Per-subagent MCP attribution works without
extra wiring.

## Components

### Committed to the repo

- `cmd/dogfood-metrics/` — Go binary, `serve` and `report`
  subcommands. Stdlib only (no new external deps).
- `scripts/dogfood-hook.sh` — receives JSON on stdin, classifies the
  tool call, writes a JSONL line. Bash for portability and zero deps.
- `dogfood-metrics/README.md` — usage instructions: how to start the
  receiver, env vars to set, how to run a /analyze cycle, how to
  generate a report.
- `.claude/settings.json` — hook registration + OTEL env vars
  (set via the `update-config` skill).
- `.gitignore` — excludes `dogfood-metrics/raw/` and
  `dogfood-metrics/sessions/`.

### Local only (gitignored)

- `dogfood-metrics/raw/` — OTLP-JSON files from the receiver, plus
  per-session hook JSONL files.
- `dogfood-metrics/sessions/<session-id>/` — generated per-session
  reports.

## Slices

Each slice is a self-contained, individually-reviewable commit.

### Slice 1 — Receiver-only Go binary

- `cmd/dogfood-metrics/main.go` (`serve` subcommand only)
- Listens on `:4318`, accepts POST `/v1/traces` and `/v1/logs`,
  validates minimally (request body is JSON, has expected top-level
  shape), writes the body to `dogfood-metrics/raw/otel.jsonl` (one
  request per line)
- `dogfood-metrics/README.md` with usage instructions
- `.gitignore` entries
- TDD with `httptest`-driven tests using captured OTLP-JSON sample
  bodies as fixtures (one ungzipped fixture + one gzipped fixture
  with `Content-Encoding: gzip`, since round 4 flagged Claude
  Code's compression behavior as undocumented)
- **Verification (no Claude Code yet):** start the receiver, `curl`
  a sample OTLP-JSON payload (ungzipped), then `curl` a gzipped
  payload with `Content-Encoding: gzip`, confirm both land on
  disk and the response is 202 Accepted

### Slice 2 — Hook script + Claude Code wiring

- `scripts/dogfood-hook.sh` — reads JSON from stdin, classifies
  (local_db / external_web / external_curl / external_git /
  signatory_source / local_other), writes one event line to
  `dogfood-metrics/raw/hooks-<session_id>.jsonl`
- Update `.claude/settings.json` via the `update-config` skill:
  - PreToolUse and PostToolUse hooks invoking the script
  - OTEL env vars from the architecture diagram
- **Verification:** run a small Claude Code action (e.g., one MCP
  tool call), confirm both OTEL spans land in `raw/otel.jsonl` and
  hook events land in `raw/hooks-<session_id>.jsonl` simultaneously

### Slice 3 — Report subcommand

- Add `report <session-id>` to `cmd/dogfood-metrics/main.go`
- Reads `raw/otel.jsonl` (filters by `session.id`) and
  `raw/hooks-<session_id>.jsonl`, joins on `session_id` /
  `tool_use_id`
- Emits `dogfood-metrics/sessions/<session-id>/report.md` with:
  per-subagent token totals (from `claude_code.llm_request` spans),
  per-subagent tool-call breakdown by classification, source-read
  evidence list (for the underspecification signal)
- TDD with captured fixtures from slice 2's verification

## Open questions and deferred work

- **OTEL-JSON wire shape verification.** Round 4 confirmed Claude
  Code follows the OTLP JSON Mapping spec strictly per docs, but
  the receiver's struct types should still be confirmed against
  actual captured bodies in slice 1. Plan: capture one sample
  early, pin the schema, write the receiver against it.
- **Compression behavior.** Round 4 flagged this — Claude Code
  docs don't mention `OTEL_EXPORTER_OTLP_COMPRESSION`, so we
  don't know if request bodies arrive gzipped or plain by default.
  Receiver inspects `Content-Encoding` per request and handles
  both. If we observe consistent gzip in slice 1 testing, we can
  document it; if not, leave the defensive handling.
- **`CLAUDE_CODE_ENHANCED_TELEMETRY_BETA` flag verification.**
  Round 4 reported this as required for trace export. Worth
  confirming at install time that the env var name is current —
  beta-gated APIs sometimes get renamed. If the trace stream is
  empty in slice 1 testing, the env var is the first thing to
  check. (Round 6 confirmed the name against current docs.)
- **First live capture failed to produce traces** despite the
  receiver being reachable and the hook block of
  `.claude/settings.json` working correctly. The env-block
  scope conflict between Round 5 (symptom) and Round 6 (docs)
  is unresolved. **Diagnostic plan + ordered hypothesis list
  is captured in `design/dogfood-errors.md` under "dogfood-
  metrics OTEL trace stream not flowing from new sessions."**
  Don't rewrite the wrapper-script rationale or revise Round 5
  here until that diagnosis runs.
- **Stop-hook end-of-session aggregates.** Round 3 confirmed
  `transcript_path` is in hook payloads; we could use the Stop hook
  to emit a session-end summary directly without waiting for the
  report subcommand. Deferred — the OTEL stream already has
  everything we need, and adding Stop-hook logic doubles the data
  paths.
- **File rotation by session.** Currently the receiver writes
  everything to one rolling `otel.jsonl`; the report subcommand
  fans out by session. If volume grows, switch to per-session files
  in the receiver.
- **SigNoz upgrade path.** When we want a real visibility layer,
  the on-disk OTLP-JSON files replay into any OTEL collector via
  the `otlpjsonfile` receiver. Or we add a second writer in our
  receiver that forwards to a real collector endpoint. Either path
  preserves the on-disk archive.
- **Metrics surface.** This design covers traces and logs. OTEL
  metrics (e.g., cumulative session-token counters) might be useful
  later; not in scope for v0.1 dogfood.

## References

- [Claude Code Monitoring Reference](https://code.claude.com/docs/en/monitoring-usage.md)
  — OTEL exporter options, span attributes, env vars
- [Claude Code Hooks Reference](https://code.claude.com/docs/en/hooks.md)
  — hook payload schema, lifecycle, blocking semantics
- [Claude Agent SDK Observability](https://code.claude.com/docs/en/agent-sdk/observability.md)
  — subagent span nesting, `query_source` semantics
- [OTLP JSON Mapping spec](https://opentelemetry.io/docs/specs/otlp/#json-protobuf-encoding)
  — wire format the receiver decodes
- `design/ROADMAP.md` — V0.1 "Improve economics" and "Validate
  through dogfood" subsections this work serves
- `design/dogfood-errors.md` — the existing dogfood-friction log
  this telemetry will feed into
