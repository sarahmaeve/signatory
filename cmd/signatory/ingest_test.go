package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/sarahmaeve/signatory/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestGlobals returns a Globals pointed at a fresh temp DB.
// Mirrors the cmd/signatory test pattern of giving each test its
// own isolated database.
func newTestGlobals(t *testing.T) *Globals {
	t.Helper()
	dir := t.TempDir()
	return &Globals{
		DBPath:        filepath.Join(dir, "test.db"),
		AuditFilePath: filepath.Join(dir, "audit.log"),
	}
}

func TestIngest_ValidJSON_StoresAndCounts(t *testing.T) {
	path := writeTempFile(t, "valid.json", minimalValidJSON)
	g := newTestGlobals(t)

	cmd := &IngestCmd{File: path, Format: "auto", Quiet: true}
	require.NoError(t, cmd.Run(g))

	// Verify the row landed by re-opening the store and counting.
	s, err := g.OpenStore(t.Context())
	require.NoError(t, err)
	defer s.Close()

	sqliteStore, ok := s.(*store.SQLite)
	require.True(t, ok, "default store implementation should be *store.SQLite")
	var count int
	require.NoError(t, sqliteStore.DB().QueryRowContext(
		context.Background(),
		`SELECT COUNT(*) FROM analyst_outputs`).Scan(&count))
	assert.Equal(t, 1, count, "one output should be in the DB after ingest")
}

func TestIngest_IdempotentReingest(t *testing.T) {
	path := writeTempFile(t, "valid.json", minimalValidJSON)
	g := newTestGlobals(t)

	cmd := &IngestCmd{File: path, Format: "auto", Quiet: true}
	require.NoError(t, cmd.Run(g))
	require.NoError(t, cmd.Run(g)) // re-ingest

	s, err := g.OpenStore(t.Context())
	require.NoError(t, err)
	defer s.Close()

	sqliteStore := s.(*store.SQLite)
	var count int
	require.NoError(t, sqliteStore.DB().QueryRowContext(
		context.Background(),
		`SELECT COUNT(*) FROM analyst_outputs`).Scan(&count))
	assert.Equal(t, 1, count, "re-ingest should be idempotent (same content_hash)")
}

func TestIngest_InvalidJSON_FailsAtValidation(t *testing.T) {
	bad := `{
  "conclusions": [{"id": "F001", "category": "x", "severity": {"default": "medium"}, "citations": [{"path": "p", "line_start": 1}]}]
}`
	path := writeTempFile(t, "bad.json", bad)
	g := newTestGlobals(t)

	cmd := &IngestCmd{File: path, Format: "auto", Quiet: true}
	err := cmd.Run(g)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "validate")
}

func TestIngest_MalformedJSON_FailsAtParse(t *testing.T) {
	bad := `{this isn't json}`
	path := writeTempFile(t, "malformed.json", bad)
	g := newTestGlobals(t)

	cmd := &IngestCmd{File: path, Format: "auto", Quiet: true}
	err := cmd.Run(g)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse")
}

func TestIngest_FileNotFound_FailsCleanly(t *testing.T) {
	cmd := &IngestCmd{
		File:   "/nonexistent/path/to/file.json",
		Format: "json",
		Quiet:  true,
	}
	err := cmd.Run(newTestGlobals(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read")
}

func TestIngest_KongIntegration(t *testing.T) {
	path := writeTempFile(t, "valid.json", minimalValidJSON)
	_, cli := parseCLI(t, "ingest", path, "--quiet")
	require.NotNil(t, cli)
	assert.Equal(t, path, cli.Ingest.File)
	assert.True(t, cli.Ingest.Quiet)
	assert.Equal(t, "auto", cli.Ingest.Format)
}

func TestIngest_KongIntegration_FormatFlag(t *testing.T) {
	path := writeTempFile(t, "valid.json", minimalValidJSON)
	_, cli := parseCLI(t, "ingest", path, "--format", "json", "-q")
	assert.Equal(t, "json", cli.Ingest.Format)
}
