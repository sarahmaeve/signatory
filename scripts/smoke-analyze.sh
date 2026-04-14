#!/usr/bin/env bash
# Smoke test for the `signatory analyze` freshness-check pattern.
#
# Walks the full ingestion + analyze loop end-to-end against the
# checked-in JSON fixtures, asserting each step. Exits non-zero on
# any unexpected output.
#
# This is not a substitute for the Go _test.go suites — those test
# the units. This script tests the *agent-facing UX* of the analyze
# command: the messages, the section headers, the cache-hit behavior.
#
# Run from the repo root:
#   ./scripts/smoke-analyze.sh

set -euo pipefail

# ---------- helpers ----------
RED=$(tput setaf 1 2>/dev/null || echo "")
GREEN=$(tput setaf 2 2>/dev/null || echo "")
RESET=$(tput sgr0 2>/dev/null || echo "")

DB=$(mktemp -d)/signatory.db
TMPOUT=$(mktemp)
trap 'rm -rf "$(dirname "$DB")" "$TMPOUT"' EXIT

step=0
pass() { step=$((step+1)); echo "${GREEN}[PASS $step]${RESET} $1"; }
fail() { step=$((step+1)); echo "${RED}[FAIL $step]${RESET} $1"; echo "--- output: ---"; cat "$TMPOUT"; exit 1; }

assert_contains() {
    local needle="$1"; local label="$2"
    if grep -qF "$needle" "$TMPOUT"; then
        pass "$label"
    else
        fail "$label (expected to find: $needle)"
    fi
}

assert_not_contains() {
    local needle="$1"; local label="$2"
    if grep -qF "$needle" "$TMPOUT"; then
        fail "$label (should NOT have found: $needle)"
    else
        pass "$label"
    fi
}

run() {
    if ! go run ./cmd/signatory --db "$DB" "$@" > "$TMPOUT" 2>&1; then
        echo "${RED}signatory exited non-zero:${RESET}"
        cat "$TMPOUT"
        exit 1
    fi
}

# ---------- the test ----------

echo "=== signatory analyze pattern: smoke test ==="
echo "DB: $DB"
echo

# Step A: empty store, analyze a target → "no cached data" prompt.
run analyze https://github.com/nvbn/thefuck
assert_contains "No cached data for: https://github.com/nvbn/thefuck" \
    "empty-store analyze prompts user to refresh or ingest"
assert_contains "Resolved to: repo:github/nvbn/thefuck" \
    "empty-store analyze normalizes URL → canonical URI"
assert_contains "Run with --refresh" \
    "empty-store analyze suggests --refresh path"

# Step B: ingest one analyst output for the target.
run ingest --quiet filestore/analysis/thefuck-security-v1.json

# Step C: re-analyze; freshness check should surface the analyst output.
run analyze https://github.com/nvbn/thefuck
assert_contains "Cached analyses" \
    "post-ingest analyze includes Cached analyses section"
assert_contains "external-sec-v1" \
    "post-ingest analyze identifies the security analyst"
assert_contains "10 finding(s)" \
    "post-ingest analyze surfaces finding count"
assert_contains "thefuck-security-v1.json" \
    "post-ingest analyze cites the source JSON path"
assert_not_contains "No cached" \
    "post-ingest analyze does NOT regress to 'no cached' message"

# Step D: ingest the second analyst output for the same target.
run ingest --quiet filestore/analysis/thefuck-provenance-v1.json

# Step E: re-analyze; both outputs should appear.
run analyze https://github.com/nvbn/thefuck
assert_contains "external-sec-v1" "both analysts: security listed"
assert_contains "signatory-provenance" "both analysts: provenance listed"
# Header is plural-of-rows, not literally "(2 outputs)" — verify both outputs visible.
secresult=$(grep -c "external-sec-v1\|signatory-provenance" "$TMPOUT" || true)
if [ "$secresult" -ge 2 ]; then
    pass "both analyst rows visible in single analyze invocation"
else
    fail "expected at least two analyst-id mentions, got $secresult"
fi

# Step F: re-ingest the same JSON file → idempotent (no second row).
run ingest filestore/analysis/thefuck-security-v1.json
assert_contains "idempotent" \
    "re-ingest of same content is reported as idempotent"

# Step G: --max-age filter excludes everything when set tiny.
# (24h is permissive; -1ns would be filtered to 0 by our positive-only
#  policy; here we simply confirm the flag is accepted and behavior
#  is consistent.)
run analyze --max-age 24h https://github.com/nvbn/thefuck
assert_contains "Cached analyses (last 24h0m0s)" \
    "--max-age=24h labels the section with the duration"
assert_contains "external-sec-v1" "--max-age=24h still includes recent outputs"

# Step H: cross-output query — show-findings --severity positive
# should surface the F010 finding from thefuck-security across the
# whole DB.
run show-findings --severity positive
assert_contains "F010" \
    "show-findings --severity positive includes thefuck F010"
assert_contains "[positive]" "positive severity tag rendered"
assert_contains "unbypassable_hosted_callback" \
    "signal_type column rendered correctly"

# Step I: ingest the atuin trial fixture (different target).
run ingest --quiet internal/exchange/testdata/atuin-schema-trial.json

# Step J: a cross-target query — show-findings --signal-type ai_capability_gating_model.
# That signal_type is present only on atuin's F001 (the positive
# correction). Should return exactly one row.
run show-findings --signal-type ai_capability_gating_model
assert_contains "F001" "cross-target signal_type query finds atuin F001"
assert_contains "pkg:cargo/atuin" "cross-target query attributes to atuin entity"
hits=$(grep -c "^F[0-9]" "$TMPOUT" || true)
if [ "$hits" -eq 1 ]; then
    pass "exactly one finding matches the signal_type filter"
else
    fail "expected exactly 1 finding for ai_capability_gating_model, got $hits"
fi

# Step K: methodology aggregation — show-methodology --signal-group
# network_endpoints --hit-on-target hit. Should show the atuin trial's
# 3 network_endpoints patterns (MP-NET-01/02/03).
run show-methodology --signal-group network_endpoints --hit-on-target hit
for pat in "MP-NET-01" "MP-NET-02" "MP-NET-03"; do
    assert_contains "$pat" "methodology aggregation shows $pat"
done

# Step L: empty-state probe — analyze a target we've never seen.
run analyze https://github.com/never/seen
assert_contains "No cached data" "untouched target reports as such"

echo
echo "${GREEN}=== all smoke checks passed (${step} assertions) ===${RESET}"
