package main

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDeltasCmd_ValidateFlags_RangeMutex: --range is mutually
// exclusive with --since, --last, and --all.
func TestDeltasCmd_ValidateFlags_RangeMutex(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cmd  DeltasCmd
	}{
		{"range+since", DeltasCmd{Range: "2d..1d", Since: "12h"}},
		{"range+last", DeltasCmd{Range: "2d..1d", Last: 5}},
		{"range+all", DeltasCmd{Range: "2d..1d", All: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.cmd.validateFlags()
			require.Error(t, err)
			assert.Contains(t, err.Error(), "mutually exclusive")
		})
	}
}

// TestDeltasCmd_ValidateFlags_RangeAlone: --range alone is valid.
func TestDeltasCmd_ValidateFlags_RangeAlone(t *testing.T) {
	t.Parallel()
	cmd := DeltasCmd{Range: "2d..1d"}
	assert.NoError(t, cmd.validateFlags())
}

// TestDeltasCmd_Window_RangeProduced: cmd.Range populated → window()
// produces a TimeWindow with RangeStart/RangeEnd set.
func TestDeltasCmd_Window_RangeProduced(t *testing.T) {
	t.Parallel()
	cmd := DeltasCmd{Range: "2026-05-10T00:00:00Z..2026-05-12T23:59:59Z"}
	w, err := cmd.window()
	require.NoError(t, err)
	assert.Equal(t, "range", w.Kind())
	assert.Equal(t, time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC), w.RangeStart)
	assert.Equal(t, time.Date(2026, 5, 12, 23, 59, 59, 0, time.UTC), w.RangeEnd)
}

// TestDeltasCmd_Window_RangeError: malformed range string produces a
// UsageError so the CLI exits 64 (EX_USAGE).
func TestDeltasCmd_Window_RangeError(t *testing.T) {
	t.Parallel()
	cmd := DeltasCmd{Range: "garbage"}
	_, err := cmd.window()
	require.Error(t, err)
	// usage errors are wrapped via NewUsageError; the sentinel is
	// ErrUsage in the same package.
	assert.ErrorContains(t, err, "--range")
}

// TestDeltasRun_RangeWindow_E2E: end-to-end through Run using
// sample.db. The axios entity has 2 observations with collected_at
// values seeded in the test DB. A range that brackets both should
// surface the transition; a range that excludes the older observation
// should leave only one observation (no transition).
func TestDeltasRun_RangeWindow_E2E(t *testing.T) {
	// Not Parallel: shares sample.db with the other E2E tests; a
	// fleet of parallel SQLite opens against the same file under
	// the full `go test ./...` load flakes intermittently. The
	// DB-touching deltas tests run serial; pure-unit tests above
	// (validateFlags, window resolver) stay parallel.
	// Wide range — brackets both observations.
	var stdout, stderr bytes.Buffer
	cmd := &DeltasCmd{
		Target: "pkg:npm/axios",
		Range:  "2026-01-01T00:00:00Z..2030-01-01T00:00:00Z",
		Yes:    true,
		Stdout: &stdout,
		Stderr: &stderr,
	}
	require.NoError(t, cmd.Run(deltasTestGlobals(t)))
	assert.Contains(t, stdout.String(), "trusted_publishing",
		"wide range must include the seeded transitions")
	assert.Contains(t, stdout.String(), "CHANGED")
}

// runDeltasRange is a small helper for the range-probe scenarios
// below — same shape as runDeltas in deltas_e2e_test.go but accepts
// a Range string and target.
func runDeltasRange(t *testing.T, target, rng string) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd := &DeltasCmd{
		Target: target,
		Range:  rng,
		Stdout: &stdout,
		Stderr: &stderr,
	}
	require.NoError(t, cmd.Run(deltasTestGlobals(t)))
	return stdout.String()
}

// TestDeltasRun_RangeWindow_BracketsAll: range covers all four
// observations of the probe entity — three transitions surface
// (t1→t2, t2→t3, t3→t4), each with a count increment.
func TestDeltasRun_RangeWindow_BracketsAll(t *testing.T) {
	out := runDeltasRange(t, "pkg:npm/range-window-probe",
		"2026-04-01T00:00:00Z..2026-06-01T00:00:00Z")
	assert.Contains(t, out, "range 2026-04-01T00:00:00Z to 2026-06-01T00:00:00Z",
		"header reflects the range")
	assert.Contains(t, out, "1 → 2", "t1→t2 transition")
	assert.Contains(t, out, "2 → 3", "t2→t3 transition")
	assert.Contains(t, out, "3 → 4", "t3→t4 transition")
}

// TestDeltasRun_RangeWindow_BracketsSubset: range excludes t1 and
// t4 (covers only t2 and t3) — exactly one transition surfaces.
func TestDeltasRun_RangeWindow_BracketsSubset(t *testing.T) {
	out := runDeltasRange(t, "pkg:npm/range-window-probe",
		"2026-05-10T00:00:00Z..2026-05-22T00:00:00Z")
	// Only t2 (2026-05-12) and t3 (2026-05-20) are inside.
	assert.Contains(t, out, "2 → 3", "the t2→t3 transition surfaces")
	assert.NotContains(t, out, "1 → 2", "t1 excluded — no t1→t2 transition")
	assert.NotContains(t, out, "3 → 4", "t4 excluded — no t3→t4 transition")
}

// TestDeltasRun_RangeWindow_SinglePoint: range that admits exactly
// one observation (just t2). No transitions render (you need two
// observations to diff). Default IncludeUnchanged=false suppresses
// the lone signal entirely.
func TestDeltasRun_RangeWindow_SinglePoint(t *testing.T) {
	out := runDeltasRange(t, "pkg:npm/range-window-probe",
		"2026-05-12T00:00:00Z..2026-05-13T00:00:00Z")
	// No transitions; signal suppressed (default behavior).
	assert.NotContains(t, out, "CHANGED")
	assert.NotContains(t, out, " → ")
}

// TestDeltasRun_RangeWindow_Empty: range falls entirely outside the
// observation timeline — the renderer surfaces an explicit
// "no observations" line.
func TestDeltasRun_RangeWindow_Empty(t *testing.T) {
	out := runDeltasRange(t, "pkg:npm/range-window-probe",
		"2030-01-01T00:00:00Z..2030-01-02T00:00:00Z")
	assert.Contains(t, out, "no observations in this window",
		"empty-range output must signal the empty-window case")
}
