package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "modernc.org/sqlite"
)

// TestShowConclusions_JSON_WithResults verifies that --json returns
// structured conclusions when they exist.
func TestShowConclusions_JSON_WithResults(t *testing.T) {
	t.Parallel()

	globals := testGlobals(t)
	ctx := t.Context()

	s, err := globals.OpenStore(ctx)
	require.NoError(t, err)
	defer s.Close()

	sess := newTestAnalysisSession(t, s,
		"https://github.com/JedWatson/classnames",
		[]string{"signatory-security-v1"},
	)
	ingestTestOutput(t, s, sess.ID, "signatory-security-v1")

	var stdout bytes.Buffer
	cmd := &ShowConclusionsCmd{
		Target: "https://github.com/JedWatson/classnames",
		JSON:   true,
		Stdout: &stdout,
	}
	require.NoError(t, cmd.Run(globals))

	var result ShowConclusionsResult
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &result),
		"stdout must be valid JSON; got: %s", stdout.String())

	assert.Equal(t, "ok", result.Status)
	require.NotEmpty(t, result.Conclusions)
	assert.Equal(t, "T001", result.Conclusions[0].ConclusionLocalID)
}

// TestShowConclusions_JSON_NoEntity verifies the no-entity status.
func TestShowConclusions_JSON_NoEntity(t *testing.T) {
	t.Parallel()

	globals := testGlobals(t)

	var stdout bytes.Buffer
	cmd := &ShowConclusionsCmd{
		Target: "https://github.com/nonexistent/repo",
		JSON:   true,
		Stdout: &stdout,
	}
	require.NoError(t, cmd.Run(globals))

	var result ShowConclusionsResult
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &result))

	assert.Equal(t, "no_entity", result.Status)
	assert.Empty(t, result.Conclusions)
}

// TestShowConclusions_JSON_Empty verifies the empty-set status.
func TestShowConclusions_JSON_Empty(t *testing.T) {
	t.Parallel()

	globals := testGlobals(t)
	ctx := t.Context()

	s, err := globals.OpenStore(ctx)
	require.NoError(t, err)
	defer s.Close()

	_, err = ensureEntity(ctx, s, "https://github.com/JedWatson/classnames")
	require.NoError(t, err)

	var stdout bytes.Buffer
	cmd := &ShowConclusionsCmd{
		Target: "https://github.com/JedWatson/classnames",
		JSON:   true,
		Stdout: &stdout,
	}
	require.NoError(t, cmd.Run(globals))

	var result ShowConclusionsResult
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &result))

	assert.Equal(t, "empty", result.Status)
	assert.Empty(t, result.Conclusions)
}
