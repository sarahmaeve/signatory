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

// TestIngest_OversizedFile_RejectedBeforeParse is the regression
// guard for the F003 OOM-via-unbounded-ReadFile finding in
// design/analysis/signatory-security-v1.json. Pre-fix, IngestCmd
// called os.ReadFile with no size cap; an attacker who could place
// a multi-GB file on disk and socially-engineer the operator into
// running `signatory ingest` could OOM the process before any
// validation ran.
//
// Post-fix, the file is read via readBoundedAnalystFile, which
// caps consumption at maxAnalystFileBytes+1 and returns
// errAnalystFileTooLarge for anything larger.
//
// Revert proof: change `readBoundedAnalystFile(cmd.File)` back to
// `os.ReadFile(cmd.File)` in IngestCmd.Run; this test fails because
// the read succeeds and the returned error names the parse phase
// instead of the size cap.
//
// Sparse-file fixture: makeSparseFile uses Truncate, so a 10 MiB+1
// fixture costs ~zero disk I/O and ~zero wall time.
func TestIngest_OversizedFile_RejectedBeforeParse(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "oversized.json")
	makeSparseFile(t, path, maxAnalystFileBytes+1)

	cmd := &IngestCmd{File: path, Format: "json", Quiet: true}
	err := cmd.Run(newTestGlobals(t))
	require.Error(t, err)
	assert.ErrorIs(t, err, errAnalystFileTooLarge,
		"oversized ingest input must be rejected with errAnalystFileTooLarge before parse runs")
}

func TestIngest_KongIntegration_FormatFlag(t *testing.T) {
	path := writeTempFile(t, "valid.json", minimalValidJSON)
	_, cli := parseCLI(t, "ingest", path, "--format", "json", "-q")
	assert.Equal(t, "json", cli.Ingest.Format)
}

// TestIngest_WithAnalysisSessionID exercises the CLI parity of the
// Phase 3 linkage: `signatory ingest --analysis-session-id=X` stamps
// the FK on the new row, making the output surface under the
// session's timing + show verbs. CLI mirrors the MCP ingest path;
// keeping them at parity is what lets operator-driven ingests show
// up in the same view as agent-driven ones.
func TestIngest_WithAnalysisSessionID(t *testing.T) {
	path := writeTempFile(t, "valid.json", minimalValidJSON)
	g := newTestGlobals(t)

	// Create the session the ingest will link against. Route via
	// the CLI verb so we're exercising the same path real users do.
	sessionID := beginSessionViaCmd(t, g, "pkg:test/example")

	cmd := &IngestCmd{
		File:              path,
		Format:            "auto",
		AnalysisSessionID: sessionID,
		Quiet:             true,
	}
	require.NoError(t, cmd.Run(g))

	// Verify: the output surfaces under the session via
	// ListOutputsForSession.
	s, err := g.OpenStore(t.Context())
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck // test cleanup

	outputs, err := s.ListOutputsForSession(t.Context(), sessionID)
	require.NoError(t, err)
	require.Len(t, outputs, 1,
		"output must surface under the session it was ingested with")
}

// TestIngest_WithAnalysisSessionID_UnknownFailsCleanly — passing a
// bogus session id trips the FK constraint at ingest time. The CLI
// surfaces that as an error rather than silently dropping the
// linkage.
func TestIngest_WithAnalysisSessionID_UnknownFailsCleanly(t *testing.T) {
	path := writeTempFile(t, "valid.json", minimalValidJSON)
	g := newTestGlobals(t)

	cmd := &IngestCmd{
		File:              path,
		Format:            "auto",
		AnalysisSessionID: "00000000-0000-0000-0000-000000000000",
		Quiet:             true,
	}
	err := cmd.Run(g)
	require.Error(t, err, "unknown analysis-session-id must fail the ingest")
	assert.Contains(t, err.Error(), "ingest",
		"error should identify the ingest phase so the caller sees the linkage failure, not a generic parse error")
}
