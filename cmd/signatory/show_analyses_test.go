package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "modernc.org/sqlite"
)

// TestShowAnalyses_JSON_WithResults verifies that --json returns a
// structured array of outputs when analyses exist for the target.
func TestShowAnalyses_JSON_WithResults(t *testing.T) {
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
	cmd := &ShowAnalysesCmd{
		Target: "https://github.com/JedWatson/classnames",
		JSON:   true,
		Stdout: &stdout,
	}
	require.NoError(t, cmd.Run(globals))

	var result ShowAnalysesResult
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &result),
		"stdout must be valid JSON; got: %s", stdout.String())

	assert.Equal(t, "ok", result.Status)
	require.Len(t, result.Analyses, 1)
	assert.Equal(t, "signatory-security-v1", result.Analyses[0].AnalystID)
	assert.NotEmpty(t, result.Analyses[0].OutputID)
}

// TestShowAnalyses_JSON_NoEntity verifies that --json returns a
// clear status when the target has never been ingested.
func TestShowAnalyses_JSON_NoEntity(t *testing.T) {
	t.Parallel()

	globals := testGlobals(t)

	var stdout bytes.Buffer
	cmd := &ShowAnalysesCmd{
		Target: "https://github.com/nonexistent/repo",
		JSON:   true,
		Stdout: &stdout,
	}
	require.NoError(t, cmd.Run(globals))

	var result ShowAnalysesResult
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &result))

	assert.Equal(t, "no_entity", result.Status)
	assert.Empty(t, result.Analyses)
}

// TestShowAnalyses_JSON_NoAnalyses verifies that --json returns an
// empty array (not an error) when the entity exists but has no
// outputs.
func TestShowAnalyses_JSON_NoAnalyses(t *testing.T) {
	t.Parallel()

	globals := testGlobals(t)
	ctx := t.Context()

	s, err := globals.OpenStore(ctx)
	require.NoError(t, err)
	defer s.Close()

	// Create an entity but don't ingest anything.
	_, err = ensureEntity(ctx, s, "https://github.com/JedWatson/classnames")
	require.NoError(t, err)

	var stdout bytes.Buffer
	cmd := &ShowAnalysesCmd{
		Target: "https://github.com/JedWatson/classnames",
		JSON:   true,
		Stdout: &stdout,
	}
	require.NoError(t, cmd.Run(globals))

	var result ShowAnalysesResult
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &result))

	assert.Equal(t, "empty", result.Status)
	assert.Empty(t, result.Analyses)
}
