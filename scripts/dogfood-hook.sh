#!/usr/bin/env bash
# scripts/dogfood-hook.sh — Claude Code hook entry point for
# dogfood telemetry. Wraps `dogfood-metrics hook`, building the
# binary on demand if it's missing or stale relative to its
# source files.
#
# Why a wrapper instead of pointing settings.json directly at the
# binary: hooks fire per tool call, so latency matters and we
# can't use `go run` (which recompiles each invocation). The
# wrapper does the build once when source changes and execs the
# pre-built binary on every other call.
#
# Stale-detection: any *.go file under cmd/dogfood-metrics newer
# than the binary triggers a rebuild. Build output goes to stderr
# so the hook's stdout (used by Claude Code's hook protocol) stays
# clean. Build failures emit a warning to stderr and exit 0 — a
# busted dev-tree shouldn't block the user's tool calls.
#
# Usage:
#   scripts/dogfood-hook.sh --event PreToolUse [other args]
# (Configured automatically by the project-scoped hook in
# .claude/settings.json.)

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$REPO_ROOT/dogfood-metrics-bin"
SRC="$REPO_ROOT/cmd/dogfood-metrics"

# Rebuild if the binary is missing OR any .go source file is
# newer than the binary. `find -newer` is BSD-flavor-compatible
# (works on macOS without GNU coreutils).
needs_build=0
if [ ! -x "$BIN" ]; then
    needs_build=1
elif find "$SRC" -name '*.go' -newer "$BIN" -type f 2>/dev/null | grep -q .; then
    needs_build=1
fi

if [ "$needs_build" -eq 1 ]; then
    if ! (cd "$REPO_ROOT" && go build -o "$BIN" ./cmd/dogfood-metrics) >&2; then
        echo "dogfood-hook: build failed; skipping hook" >&2
        exit 0
    fi
fi

exec "$BIN" hook "$@"
