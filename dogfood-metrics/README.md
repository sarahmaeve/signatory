# Dogfood metrics

Telemetry capture for /analyze runs — observes Claude Code from the
outside (OTEL traces + hook events), correlates by session, produces
per-session reports.

See `design/agent-otel.md` for the architecture and the verification
rounds that informed it.

## What lives here

- `README.md` (this file) — committed
- `raw/` — OTLP-JSON files from the receiver and per-session hook
  JSONL files. **Gitignored.**
- `sessions/<session-id>/report.md` — generated per-session reports.
  **Gitignored.**

## Components

| Piece | Where |
|---|---|
| OTLP/HTTP/JSON receiver | `cmd/dogfood-metrics serve` |
| Background daemon mode | `cmd/dogfood-metrics start` / `stop` / `restart` |
| Claude Code hook entry point | `scripts/dogfood-hook.sh` |
| Hook event classifier (Go) | `cmd/dogfood-metrics hook` |
| Claude Code wiring (env + hooks) | `.claude/settings.json` |
| Session listing | `cmd/dogfood-metrics list-sessions` |
| Per-session report generator | `cmd/dogfood-metrics report <id>` |
| End-to-end smoke driver | `cmd/dogfood-metrics-smoke` |

## Subcommand summary

```
dogfood-metrics serve         [-addr :4318] [-out-dir dogfood-metrics/raw]    (foreground)
dogfood-metrics start         [-addr :4318] [-out-dir dogfood-metrics/raw]    (background daemon)
dogfood-metrics stop          [-out-dir dogfood-metrics/raw]
dogfood-metrics restart       [-addr :4318] [-out-dir dogfood-metrics/raw]
dogfood-metrics hook          --event PreToolUse [-out-dir dogfood-metrics/raw]
dogfood-metrics list-sessions [-in-dir dogfood-metrics/raw]
dogfood-metrics report        [-in-dir dogfood-metrics/raw] [-out-dir dogfood-metrics/sessions] <session-id>
```

### Daemon lifecycle (start / stop / restart)

`serve` runs the receiver in the foreground (good for one-off
captures and CI-like contexts). `start` / `stop` / `restart`
manage a detached background process so you don't need to keep
a terminal open.

State lives in `<out-dir>/`:
- `.receiver.pid` — PID of the running daemon (gitignored)
- `.receiver.log` — receiver stdout + stderr (gitignored)

Behavior:
- `start` refuses to launch a second daemon if one is already
  running for that out-dir
- `start` waits up to 5s for the receiver to bind, then reports
  failure (and kills the half-started child) if it didn't
- `stop` SIGTERMs the daemon, waits up to 5s, escalates to
  SIGKILL if needed, then removes the PID file
- `restart` does best-effort stop then start (not-running is
  fine) with a 500ms pause between for the port to release
- `stop` against a stale PID file (process is already dead)
  cleans up the file and returns the not-running status

### list-sessions

Scans `<in-dir>` for both hook JSONL files and OTLP traces,
joins on session id, prints a sorted table newest-last-seen
first. Use it to find the right session id before
`report <session-id>`.

The receiver:
- Listens on `:4318` (configurable via `-addr`)
- Accepts `POST /v1/traces` (writes to `raw/traces.jsonl`)
- Accepts `POST /v1/logs` (writes to `raw/logs.jsonl`)
- Handles `Content-Encoding: gzip` defensively (per round-4
  verification — Claude Code's compression behavior is undocumented)
- Returns `202 Accepted` on success
- Caps individual request bodies at 16 MiB

Each line in the output JSONL files is the verbatim OTLP-JSON body
of one request, compacted (one JSON value per line). The format is
directly replayable into any OTEL collector via the `otlpjsonfile`
receiver — see `design/agent-otel.md` "SigNoz upgrade path."

The hook subcommand:
- Reads one Claude Code hook event JSON from stdin
- Classifies the tool call (`local_db` / `local_signatory_cli` /
  `local_other` / `external_web` / `external_curl` / `external_git`
  / `signatory_source`)
- Appends one JSON line to `raw/hooks-<session_id>.jsonl`
- Always exits 0 — exit 2 from a PreToolUse hook would block the
  tool call (round-3 verification)

The report subcommand:
- Reads `raw/traces.jsonl` (filters by `session.id` resource attr)
- Reads `raw/hooks-<session-id>.jsonl`
- Joins on session id, aggregates spans by `query_source`
  (subagent name) and hook events by classification
- Renders `sessions/<session-id>/report.md` with five fixed
  sections: subagent activity, tool-call classification, external
  calls (cache-miss candidates), source reads (underspecification
  candidates), plus the header

## Verification

**Verification lives in Go**, not in shell scripts. Two layers:

### Unit tests

```bash
go test ./cmd/dogfood-metrics/...
```

Covers the http.Handler, the hook classifier, the OTLP-JSON
parser, the report renderer — each in isolation.

### End-to-end smoke binary

```bash
go run ./cmd/dogfood-metrics-smoke
```

Builds `dogfood-metrics` into a temp dir, spawns the receiver as
a subprocess on a free port, exercises every `serve` path
(ungzipped + gzipped POST, malformed JSON, unknown path, GET),
pipes a sample event through the `hook` subprocess, runs the
`report` subprocess, asserts at each step. Expected output ends
with `All N assertions passed.` (currently 24).

The smoke binary is the project's official "does the binary
work end-to-end?" check — modeled on `cmd/smoke-mcp/` and
written in Go because tests are permanent fixtures, not
temporary scripts. Earlier curl-based verification has been
retired in favor of this driver.

## Live capture (against a real Claude Code session)

Once everything is committed and `/hooks` (or session restart)
has picked up `.claude/settings.json`:

1. **In one terminal**, start the receiver:
   ```bash
   go run ./cmd/dogfood-metrics serve
   ```

2. **In another terminal**, run Claude Code in this project as
   normal. The hook fires per tool call (writes to
   `raw/hooks-<session>.jsonl`), and Claude Code emits OTEL
   traces to the receiver (writes to `raw/traces.jsonl` and
   `raw/logs.jsonl`).

3. **After a session**, find the session id and render the report:
   ```bash
   go run ./cmd/dogfood-metrics list-sessions     # shows all known sessions, newest first
   go run ./cmd/dogfood-metrics report <session-id>
   open dogfood-metrics/sessions/<session-id>/report.md
   ```

The report's "External calls" section is the cache-miss-candidate
list per the ROADMAP "improve economics" subsection. The
"Source reads" section is the underspecification signal per
`design/agent-otel.md`. Both feed into `design/dogfood-errors.md`
when patterns surface.
