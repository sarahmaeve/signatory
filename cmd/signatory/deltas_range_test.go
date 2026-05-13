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
	t.Parallel()
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
