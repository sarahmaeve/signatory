#!/usr/bin/env bash
# Smoke test: does commit_publish_cadence_divergence fire across the
# six supported registry ecosystems?
#
# Background: sketch 1 (signal-sketch.md) claims the cadence collector
# is ecosystem-agnostic — it reads last_commit/last_push and
# last_publish from the in-run accumulator, and every registry
# collector (npm/pypi/cargo/gem/gopublish/maven) emits last_publish.
# This script verifies the claim by running analyze --refresh --clone
# against one canonical target per ecosystem and reporting whether the
# derived signal landed.
#
# Diagnostic, not pass/fail: a "no" result is information (e.g., the
# forge collector didn't resolve, or the signal genuinely didn't fire).
# The script reports per-target shape and exits 0 unless analyze itself
# crashes — the human reads the table.
#
# Run from the repo root:
#   ./scripts/smoke-cadence-cross-ecosystem.sh
#
# Output: a per-ecosystem table of (fired? / shape / commit_days_ago /
# publish_days_ago / divergence_days), and the path to the populated
# DB so the user can poke at it with `signatory show-*` afterwards.
#
# Cost: six clone+fetch+analyze runs; expect 2–5 minutes wall-clock
# depending on network and the size of each repo. Guava (Maven) is the
# slowest clone.

set -euo pipefail

# ---------- setup ----------
RED=$(tput setaf 1 2>/dev/null || echo "")
GREEN=$(tput setaf 2 2>/dev/null || echo "")
YELLOW=$(tput setaf 3 2>/dev/null || echo "")
RESET=$(tput sgr0 2>/dev/null || echo "")

# Script-scoped tmp dir holds the DB and per-target clones. Not
# auto-cleaned: the user may want to inspect results afterwards.
SMOKE_DIR=${SMOKE_DIR:-$(mktemp -d -t signatory-cadence-smoke.XXXXXX)}
DB="$SMOKE_DIR/signatory.db"
CLONES_DIR="$SMOKE_DIR/clones"
mkdir -p "$CLONES_DIR"

echo "=== signatory cadence cross-ecosystem smoke test ==="
echo "DB:     $DB"
echo "Clones: $CLONES_DIR"
echo

# Require jq for the JSON assertion step.
if ! command -v jq >/dev/null 2>&1; then
    echo "${RED}jq not found on PATH; install jq to parse analyze --json output.${RESET}" >&2
    exit 2
fi

# Build once so the six invocations don't each re-compile.
echo "Building signatory binary..."
BIN="$SMOKE_DIR/signatory"
go build -o "$BIN" ./cmd/signatory
echo

# ---------- targets ----------
# One canonical, well-known target per ecosystem. Chosen for stable
# GitHub-hosted source, active publish cadence, and small-enough clones
# to keep the script tractable. Maven's guava is the largest by clone
# size; if it's painful, swap to a smaller artifact.
#
# Format: each line is "ecosystem|target|short-name". The short-name
# is the clone subdir.
TARGETS=(
    "npm|pkg:npm/axios|axios"
    "pypi|pkg:pypi/requests|requests"
    "cargo|pkg:cargo/serde|serde"
    "gem|pkg:gem/rake|rake"
    "golang|pkg:golang/github.com/spf13/cobra|cobra"
    "maven|pkg:maven/io.micrometer/micrometer-core|micrometer-core"
)

# ---------- per-target run ----------
# For each target:
#   1. analyze --refresh --clone --path <dir> --db <db>
#      (populates signals and last_publish; cadence collector fires
#      if commit-side + publish-side inputs are both present)
#   2. analyze --db <db> --json
#      (read-only read of the cached signals; jq parses for the
#      cadence signal and extracts its shape/days fields)
#
# Both steps log full stderr to a per-target file under SMOKE_DIR so
# failures are inspectable without re-running.

declare -a RESULT_ROWS

run_one() {
    local ecosystem="$1"
    local target="$2"
    local shortname="$3"
    local path="$CLONES_DIR/$shortname"
    local logfile="$SMOKE_DIR/$ecosystem.log"

    echo "${YELLOW}--- $ecosystem: $target ---${RESET}"

    # Step 1: refresh collection. --clone ensures the repo is present
    # at --path; --refresh runs all collectors. Failures here are real
    # (network, auth, or a registry returning unexpected wire format)
    # — exit so the user sees them.
    if ! "$BIN" --db "$DB" analyze --refresh --clone --path "$path" "$target" \
        >"$logfile" 2>&1; then
        echo "${RED}  analyze --refresh failed; see $logfile${RESET}"
        RESULT_ROWS+=("$ecosystem|$target|ERROR|see $logfile")
        return
    fi

    # Step 2: read-only JSON dump → jq for cadence presence.
    # The display structure puts signals at .Profile.Signals[]; each
    # has .Type and .Value (an object with the shape-specific fields).
    local json
    if ! json=$("$BIN" --db "$DB" analyze --json "$target" 2>/dev/null); then
        echo "${RED}  analyze --json failed${RESET}"
        RESULT_ROWS+=("$ecosystem|$target|ERROR|json read failed")
        return
    fi

    # Extract cadence signal value if present. The JSON shape:
    # AnalysisDisplay embeds *profile.Profile, so Signals is promoted
    # to the top level. Field tags are lowercase. Path is .signals[],
    # with .type and .value (NOT .Profile.Signals / .Type / .Value).
    local cadence
    cadence=$(echo "$json" | jq -c '
        .signals[]?
        | select(.type == "commit_publish_cadence_divergence")
        | .value
    ' 2>/dev/null || echo "")

    if [ -z "$cadence" ] || [ "$cadence" = "null" ]; then
        # Surface commit-side and publish-side presence + total signal
        # count so "cadence didn't fire because input missing" is
        # distinguishable from "no signals at all collected" and from
        # "cadence didn't fire despite both inputs."
        local total has_commit has_publish
        total=$(echo "$json" | jq -r '[.signals[]?] | length')
        has_commit=$(echo "$json" | jq -r '
            [.signals[]? | select(.type == "last_commit" or .type == "last_push")] | length
        ')
        has_publish=$(echo "$json" | jq -r '
            [.signals[]? | select(.type == "last_publish")] | length
        ')
        echo "  ${RED}NOT FIRED${RESET} (total signals: $total, commit-side: $has_commit, last_publish: $has_publish)"
        RESULT_ROWS+=("$ecosystem|$target|no|total=$total commit=$has_commit publish=$has_publish")
        return
    fi

    # Extract the shape + day counts. jq // "?" makes missing fields
    # visible rather than silently empty.
    local shape commit_days publish_days divergence
    shape=$(echo "$cadence" | jq -r '.shape // "?"')
    commit_days=$(echo "$cadence" | jq -r '.commit_days_ago // "?"')
    publish_days=$(echo "$cadence" | jq -r '.publish_days_ago // "?"')
    divergence=$(echo "$cadence" | jq -r '.divergence_days // "?"')

    echo "  ${GREEN}FIRED${RESET}: shape=$shape commit=${commit_days}d publish=${publish_days}d divergence=${divergence}d"
    RESULT_ROWS+=("$ecosystem|$target|yes|$shape commit=${commit_days}d publish=${publish_days}d divergence=${divergence}d")
}

for entry in "${TARGETS[@]}"; do
    IFS='|' read -r ecosystem target shortname <<<"$entry"
    run_one "$ecosystem" "$target" "$shortname"
done

# ---------- report ----------
echo
echo "=== summary ==="
printf "%-10s %-40s %-8s %s\n" "ecosystem" "target" "fired?" "details"
printf "%-10s %-40s %-8s %s\n" "---------" "------" "------" "-------"
for row in "${RESULT_ROWS[@]}"; do
    IFS='|' read -r ecosystem target fired details <<<"$row"
    color="$GREEN"
    case "$fired" in
        no) color="$RED" ;;
        ERROR) color="$RED" ;;
    esac
    printf "%-10s %-40s ${color}%-8s${RESET} %s\n" "$ecosystem" "$target" "$fired" "$details"
done

echo
echo "DB preserved at: $DB"
echo "Per-target logs: $SMOKE_DIR/<ecosystem>.log"
echo "Inspect with:    $BIN --db $DB analyze <target>"
