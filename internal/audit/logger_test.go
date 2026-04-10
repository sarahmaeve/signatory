package audit

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
)

// fakeStore captures audit entries for verification.
type fakeStore struct {
	entries []profile.AuditEntry
	err     error
}

func (f *fakeStore) AppendAuditEntry(_ context.Context, entry *profile.AuditEntry) error {
	if f.err != nil {
		return f.err
	}
	f.entries = append(f.entries, *entry)
	return nil
}

func TestLogger_WritesToStoreAndFile(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "audit.log")
	store := &fakeStore{}
	logger := New(store, filePath)

	err := logger.LogAction(context.Background(),
		"team:sarah+claude", "set_posture", "ent-1",
		`{"version":"1.0.0","tier":"vetted-frozen"}`)
	require.NoError(t, err)

	// Store received the entry.
	require.Len(t, store.entries, 1)
	entry := store.entries[0]
	assert.Equal(t, "team:sarah+claude", entry.Actor)
	assert.Equal(t, "set_posture", entry.Action)
	assert.Equal(t, "ent-1", entry.EntityID)
	assert.NotEmpty(t, entry.ID, "logger should auto-assign an ID")
	assert.False(t, entry.Timestamp.IsZero(), "logger should auto-assign a timestamp")

	// File received the same entry as JSON lines.
	content, err := os.ReadFile(filePath)
	require.NoError(t, err)

	scanner := bufio.NewScanner(file(t, filePath))
	defer file(t, filePath).Close() //nolint:errcheck
	lines := 0
	for scanner.Scan() {
		lines++
		var line fileLine
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &line), "each line must be valid JSON")
		assert.Equal(t, "team:sarah+claude", line.Actor)
		assert.Equal(t, "set_posture", line.Action)
		assert.Equal(t, "ent-1", line.Entity)
		// Detail should be embedded as raw JSON, not a string.
		var detail map[string]interface{}
		require.NoError(t, json.Unmarshal(line.Detail, &detail))
		assert.Equal(t, "vetted-frozen", detail["tier"])
	}
	assert.Equal(t, 1, lines)

	_ = content
}

// file is a test helper that opens the audit log for reading.
func file(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	return f
}

func TestLogger_AppendsMultipleLines(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "audit.log")
	store := &fakeStore{}
	logger := New(store, filePath)

	for i := 0; i < 3; i++ {
		require.NoError(t, logger.LogAction(context.Background(),
			"team:sarah", "analyze", "ent-1", "{}"))
	}

	assert.Len(t, store.entries, 3)

	data, err := os.ReadFile(filePath)
	require.NoError(t, err)
	lineCount := 0
	for _, b := range data {
		if b == '\n' {
			lineCount++
		}
	}
	assert.Equal(t, 3, lineCount, "file should have one JSON line per log call")
}

func TestLogger_StoreErrorPropagates(t *testing.T) {
	dir := t.TempDir()
	store := &fakeStore{err: errors.New("db offline")}
	logger := New(store, filepath.Join(dir, "audit.log"))

	err := logger.LogAction(context.Background(), "team:sarah", "analyze", "ent-1", "{}")
	assert.Error(t, err, "store write failure must propagate")
}

// TestLogger_FileErrorDoesNotFail verifies the best-effort file
// writer: if the file path is unwritable, the store write still
// succeeds and Log returns nil.
func TestLogger_FileErrorDoesNotFail(t *testing.T) {
	store := &fakeStore{}
	// Point at a path that can't be created — a file inside a non-
	// existent sentinel directory where the parent is a regular file.
	dir := t.TempDir()
	sentinel := filepath.Join(dir, "not-a-dir")
	require.NoError(t, os.WriteFile(sentinel, []byte("blocker"), 0600))
	logger := New(store, filepath.Join(sentinel, "audit.log"))

	err := logger.LogAction(context.Background(), "team:sarah", "analyze", "ent-1", "{}")
	assert.NoError(t, err, "file write failure should not fail the call")
	assert.Len(t, store.entries, 1, "store write should still have happened")
}

func TestLogger_NilEntryRejected(t *testing.T) {
	store := &fakeStore{}
	logger := New(store, "")
	assert.Error(t, logger.Log(context.Background(), nil))
}

func TestLogger_EmptyFilePathSkipsFile(t *testing.T) {
	store := &fakeStore{}
	logger := New(store, "")
	err := logger.LogAction(context.Background(), "team:sarah", "analyze", "ent-1", "{}")
	require.NoError(t, err)
	assert.Len(t, store.entries, 1)
}

func TestLogger_PreservesExistingIDAndTimestamp(t *testing.T) {
	store := &fakeStore{}
	logger := New(store, "")

	when := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	err := logger.Log(context.Background(), &profile.AuditEntry{
		ID:        "my-id",
		Timestamp: when,
		Actor:     "team:sarah",
		Action:    "analyze",
	})
	require.NoError(t, err)
	assert.Equal(t, "my-id", store.entries[0].ID)
	assert.True(t, when.Equal(store.entries[0].Timestamp))
}

func TestLogger_InvalidDetailFallsBackToEmptyObject(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "audit.log")
	store := &fakeStore{}
	logger := New(store, filePath)

	// Caller passes a non-JSON detail by mistake — the file line
	// should still be valid JSON.
	require.NoError(t, logger.LogAction(context.Background(),
		"team:sarah", "analyze", "ent-1", "not json at all"))

	data, err := os.ReadFile(filePath)
	require.NoError(t, err)
	var line fileLine
	require.NoError(t, json.Unmarshal(data[:len(data)-1], &line))
	// Detail should be parseable as JSON (empty object fallback).
	var detail map[string]interface{}
	require.NoError(t, json.Unmarshal(line.Detail, &detail))
	assert.Empty(t, detail)
}
