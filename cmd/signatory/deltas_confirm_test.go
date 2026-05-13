package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/sarahmaeve/signatory/internal/deltas"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCountRunsInRender_DistinctCollectedAt verifies the "runs"
// metric is the number of distinct CollectedAt timestamps across
// all observations in the rendered set. Signals emitted in the
// same analyze invocation share a timestamp; this collapses them
// into one run for the prompt.
func TestCountRunsInRender_DistinctCollectedAt(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 5, 12, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(2 * time.Hour)
	t2 := t0.Add(4 * time.Hour)

	// Two groups sharing some timestamps with one another — the
	// dedup must collapse across groups, not just within one.
	in := deltas.RenderInput{
		Groups: []deltas.SignalDelta{
			{
				Type: "a",
				Observations: []deltas.Observation{
					{CollectedAt: t0}, {CollectedAt: t1},
				},
			},
			{
				Type: "b",
				Observations: []deltas.Observation{
					{CollectedAt: t0}, {CollectedAt: t2}, // t0 shared with group a
				},
			},
		},
	}
	assert.Equal(t, 3, countRunsInRender(in),
		"distinct timestamps are t0, t1, t2")
}

func TestCountRunsInRender_Empty(t *testing.T) {
	t.Parallel()
	assert.Equal(t, 0, countRunsInRender(deltas.RenderInput{}))
	assert.Equal(t, 0, countRunsInRender(deltas.RenderInput{Groups: []deltas.SignalDelta{}}))
}

// TestConfirmAllExpansion_YesFlagBypasses: --yes short-circuits the
// prompt regardless of run count.
func TestConfirmAllExpansion_YesFlagBypasses(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	proceed, err := confirmAllExpansion(&out, nil, 9999, "pkg:npm/x", true)
	require.NoError(t, err)
	assert.True(t, proceed)
	assert.Empty(t, out.String(), "--yes should not print a prompt")
}

// TestConfirmAllExpansion_BelowThreshold: small history skips the
// prompt entirely.
func TestConfirmAllExpansion_BelowThreshold(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	proceed, err := confirmAllExpansion(&out, nil, allRunsPromptThreshold, "pkg:npm/x", false)
	require.NoError(t, err)
	assert.True(t, proceed, "exactly threshold runs should not prompt")
	assert.Empty(t, out.String())
}

// TestConfirmAllExpansion_UserSaysYes: above threshold + stdin "y\n"
// → proceed. Prompt content reaches the writer (typically stderr).
func TestConfirmAllExpansion_UserSaysYes(t *testing.T) {
	t.Parallel()
	in := strings.NewReader("y\n")
	var out bytes.Buffer
	proceed, err := confirmAllExpansion(&out, in, allRunsPromptThreshold+5, "pkg:npm/x", false)
	require.NoError(t, err)
	assert.True(t, proceed)
	prompt := out.String()
	assert.Contains(t, prompt, "warning")
	assert.Contains(t, prompt, "runs")
	assert.Contains(t, prompt, "Continue")
}

// TestConfirmAllExpansion_YVariants: case-insensitive accept.
func TestConfirmAllExpansion_YVariants(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"Y\n", "yes\n", "YES\n", " y \n", "Yes\n"} {
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			in := strings.NewReader(raw)
			var out bytes.Buffer
			proceed, err := confirmAllExpansion(&out, in, 50, "pkg:npm/x", false)
			require.NoError(t, err)
			assert.True(t, proceed)
		})
	}
}

// TestConfirmAllExpansion_UserSaysNo: above threshold + "n\n" → bail.
func TestConfirmAllExpansion_UserSaysNo(t *testing.T) {
	t.Parallel()
	in := strings.NewReader("n\n")
	var out bytes.Buffer
	proceed, err := confirmAllExpansion(&out, in, 50, "pkg:npm/x", false)
	require.NoError(t, err)
	assert.False(t, proceed)
}

// TestConfirmAllExpansion_EmptyDefaultsNo: bare Enter is the safer
// default — exiting clean is harmless.
func TestConfirmAllExpansion_EmptyDefaultsNo(t *testing.T) {
	t.Parallel()
	in := strings.NewReader("\n")
	var out bytes.Buffer
	proceed, err := confirmAllExpansion(&out, in, 50, "pkg:npm/x", false)
	require.NoError(t, err)
	assert.False(t, proceed, "empty input should NOT proceed (safer default)")
}

// TestConfirmAllExpansion_GarbageDefaultsNo: anything that isn't a
// y-variant is treated as no.
func TestConfirmAllExpansion_GarbageDefaultsNo(t *testing.T) {
	t.Parallel()
	in := strings.NewReader("maybe\n")
	var out bytes.Buffer
	proceed, err := confirmAllExpansion(&out, in, 50, "pkg:npm/x", false)
	require.NoError(t, err)
	assert.False(t, proceed)
}

// TestConfirmAllExpansion_EOFDefaultsNo: a closed/empty pipe must not
// hang; it returns EOF and defaults to bail. This covers the
// non-interactive script case (no --yes, no piped input).
func TestConfirmAllExpansion_EOFDefaultsNo(t *testing.T) {
	t.Parallel()
	in := strings.NewReader("") // EOF immediately
	var out bytes.Buffer
	proceed, err := confirmAllExpansion(&out, in, 50, "pkg:npm/x", false)
	require.NoError(t, err)
	assert.False(t, proceed, "EOF must not hang and must default to no")
}

// TestDeltasRun_YesFlagSuppressesPrompt is an end-to-end check: with
// --yes on, the rendered output appears and no prompt text leaks into
// stderr. Sample.db's axios entity has only 2 runs so the prompt
// wouldn't fire anyway, but --yes is the contract we care about here.
func TestDeltasRun_YesFlagSuppressesPrompt(t *testing.T) {
	// Not Parallel: opens sample.db; see TestDeltasRun_RangeWindow_E2E
	// note for why DB-touching tests run serial.
	var stdout, stderr bytes.Buffer
	cmd := &DeltasCmd{
		Target: "pkg:npm/axios",
		All:    true,
		Yes:    true,
		Stdout: &stdout,
		Stderr: &stderr,
	}
	require.NoError(t, cmd.Run(deltasTestGlobals(t)))
	assert.NotContains(t, stderr.String(), "Continue",
		"--yes should suppress the prompt entirely")
	assert.Contains(t, stdout.String(), "Deltas for pkg:npm/axios")
}
