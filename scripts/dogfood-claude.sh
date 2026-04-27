#!/usr/bin/env bash
# scripts/dogfood-claude.sh — launch Claude Code with OTEL env
# vars exported, so the dogfood-metrics receiver captures traces
# from this session.
#
# Why a wrapper instead of relying on `.claude/settings.json`
# env block: in our first live capture (round 5,
# design/agent-otel.md), traces didn't flow even though hooks
# fired correctly — settings.json was clearly being read.
# Round 6 noted the official docs say the env block should
# apply to "every session" (which the verifying agent read as
# including Claude Code's own process). The contradiction is
# unresolved; the wrapper exists as a defensive belt-and-
# suspenders so the dogfood pipeline works regardless of which
# layer is actually wrong. If proper diagnosis later shows
# settings.json IS sufficient, the wrapper becomes redundant
# (still harmless).
#
# Use:
#   ./scripts/dogfood-claude.sh              # interactive session
#   ./scripts/dogfood-claude.sh -p "..."     # any claude args pass through
#
# Pre-reqs:
#   1. dogfood-metrics receiver running:
#        dogfood-metrics start
#   2. Receiver bound to localhost:4318 (the default).
#      Override via OTEL_EXPORTER_OTLP_ENDPOINT if needed.
#
# Output: traces land in dogfood-metrics/raw/traces.jsonl;
# hooks (set up via .claude/settings.json) land in
# dogfood-metrics/raw/hooks-<session_id>.jsonl. List sessions
# with `dogfood-metrics list-sessions` and render with
# `dogfood-metrics report <session-id>`.

set -euo pipefail

# Telemetry envelope. Each var is the canonical OTEL or
# Claude-Code-specific name; values match what
# .claude/settings.json declares (kept in sync deliberately —
# the settings block is documentation even when Claude Code
# itself doesn't read it).
export CLAUDE_CODE_ENABLE_TELEMETRY=1
export CLAUDE_CODE_ENHANCED_TELEMETRY_BETA=1     # required for trace export (round 4)
export OTEL_TRACES_EXPORTER=otlp
export OTEL_LOGS_EXPORTER=otlp
export OTEL_EXPORTER_OTLP_PROTOCOL=http/json
export OTEL_EXPORTER_OTLP_ENDPOINT=${OTEL_EXPORTER_OTLP_ENDPOINT:-http://localhost:4318}
export OTEL_LOG_TOOL_DETAILS=1

# Quick reachability check so we fail loudly if the receiver
# isn't up — silently sending traces to nothing is the failure
# mode that wasted the round-5 capture window.
if ! curl -sf -o /dev/null --max-time 2 \
        -X POST "$OTEL_EXPORTER_OTLP_ENDPOINT/v1/traces" \
        -H 'Content-Type: application/json' \
        --data '{"resourceSpans":[]}'; then
    echo "warning: receiver at $OTEL_EXPORTER_OTLP_ENDPOINT did not respond" >&2
    echo "         start it with: dogfood-metrics start" >&2
    echo "         continuing anyway — Claude Code will retry exports silently" >&2
fi

# exec so claude inherits the PID, signals route directly, and
# `ps` shows `claude` instead of a wrapper.
exec claude "$@"
