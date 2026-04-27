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

| Piece | Where | Status |
|---|---|---|
| OTLP/HTTP/JSON receiver | `cmd/dogfood-metrics serve` | Slice 1 (this) |
| PreToolUse hook script | `scripts/dogfood-hook.sh` | Slice 2 |
| Claude Code wiring (env + hooks) | `.claude/settings.json` | Slice 2 |
| Per-session report generator | `cmd/dogfood-metrics report <id>` | Slice 3 |

## Running the receiver (slice 1)

```bash
# Build
go build -o /tmp/dogfood-metrics ./cmd/dogfood-metrics

# Run (defaults: -addr :4318, -out-dir dogfood-metrics/raw)
/tmp/dogfood-metrics serve

# Or directly
go run ./cmd/dogfood-metrics serve
```

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
receiver.

## Verifying the receiver (slice 1)

Without Claude Code wired up yet, you can confirm the receiver works
with `curl`:

```bash
# Start the receiver in one terminal
go run ./cmd/dogfood-metrics serve

# In another terminal — ungzipped POST
curl -i -X POST http://localhost:4318/v1/traces \
  -H 'Content-Type: application/json' \
  --data '{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"test"}}]},"scopeSpans":[{"spans":[{"name":"x"}]}]}]}'

# Expect: HTTP/1.1 202 Accepted
# And: cat dogfood-metrics/raw/traces.jsonl  → one line of OTLP-JSON

# Gzipped POST (defense path)
echo -n '{"resourceSpans":[{"resource":{"attributes":[]},"scopeSpans":[]}]}' | \
  gzip | \
  curl -i -X POST http://localhost:4318/v1/traces \
       -H 'Content-Type: application/json' \
       -H 'Content-Encoding: gzip' \
       --data-binary @-

# Expect: HTTP/1.1 202 Accepted
# And: cat dogfood-metrics/raw/traces.jsonl  → second line, decompressed
```

## Coming next (slices 2–3)

- Slice 2 wires Claude Code to talk to this receiver via OTEL env
  vars and adds the PreToolUse hook script.
- Slice 3 adds the `report <session-id>` subcommand that joins the
  OTLP-JSON stream with the hook JSONL stream and emits a markdown
  per-session report.
