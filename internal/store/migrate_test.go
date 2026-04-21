package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Version tracking ---

func TestMigration_VersionTableCreated(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	var version int
	err := s.db.QueryRowContext(ctx,
		"SELECT MAX(version) FROM schema_version").Scan(&version)
	require.NoError(t, err, "schema_version table should exist")
	assert.Equal(t, len(migrations), version,
		"version should equal the number of migrations")
}

func TestMigration_VersionRecordsHaveTimestamps(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	var appliedAt string
	err := s.db.QueryRowContext(ctx,
		"SELECT applied_at FROM schema_version WHERE version = 1").Scan(&appliedAt)
	require.NoError(t, err)
	assert.NotEmpty(t, appliedAt, "applied_at should be populated")
}

// --- Idempotent reopen ---

func TestMigration_IdempotentReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	// Open, write data, close.
	s1, err := OpenSQLite(t.Context(), path)
	require.NoError(t, err)

	_, err = s1.db.ExecContext(t.Context(),
		`INSERT INTO entities (id, canonical_uri, type, short_name, description, ecosystem, url, created_at, updated_at)
		 VALUES ('test-1', 'pkg:npm/test', 'package', 'test', '', '', '', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`)
	require.NoError(t, err)
	s1.Close()

	// Reopen — migrations should be idempotent, data preserved.
	s2, err := OpenSQLite(t.Context(), path)
	require.NoError(t, err)
	defer s2.Close()

	var shortName string
	err = s2.db.QueryRowContext(context.Background(),
		"SELECT short_name FROM entities WHERE id = 'test-1'").Scan(&shortName)
	require.NoError(t, err)
	assert.Equal(t, "test", shortName)

	// Version should still be correct.
	var version int
	err = s2.db.QueryRowContext(context.Background(),
		"SELECT MAX(version) FROM schema_version").Scan(&version)
	require.NoError(t, err)
	assert.Equal(t, len(migrations), version)
}

// --- Legacy database upgrade ---

// TestMigration_LegacyDatabaseUpgraded verifies that a database
// created with the original v1 schema (no schema_version table) is
// correctly detected and upgraded all the way to the current version.
// Critically, it verifies that the v1 `name` column data is carried
// forward into the v2 `short_name` column by the migrationV2Up
// UPDATE statement, not silently lost.
func TestMigration_LegacyDatabaseUpgraded(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.db")

	// Create a legacy database with the v1 schema but NO schema_version table.
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), initialSchema)
	require.NoError(t, err)

	// Insert data using v1 schema (has `name` column).
	_, err = db.ExecContext(t.Context(),
		`INSERT INTO entities (id, type, name, ecosystem, url, created_at, updated_at)
		 VALUES ('legacy-1', 'package', 'legacy-pkg', '', '', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`)
	require.NoError(t, err)
	db.Close()

	// Open with OpenSQLite — should detect legacy and upgrade through
	// the whole chain (legacy v1 → marked as v1 → migrated to v2).
	s, err := OpenSQLite(t.Context(), path)
	require.NoError(t, err)
	defer s.Close()

	// Data preserved in the new columns: canonical_uri populated from
	// legacy id, short_name populated from legacy name.
	var shortName, canonicalURI string
	err = s.db.QueryRowContext(context.Background(),
		"SELECT short_name, canonical_uri FROM entities WHERE id = 'legacy-1'").Scan(&shortName, &canonicalURI)
	require.NoError(t, err)
	assert.Equal(t, "legacy-pkg", shortName, "v1 name should have been copied to v2 short_name")
	assert.Equal(t, "legacy-1", canonicalURI, "v1 id should have been copied to v2 canonical_uri")

	// V1 name column should be gone.
	var dummy string
	err = s.db.QueryRowContext(context.Background(),
		"SELECT name FROM pragma_table_info('entities') WHERE name = 'name'").Scan(&dummy)
	assert.Equal(t, sql.ErrNoRows, err, "v1 name column should be dropped after v2 migration")

	// Version table exists and is current.
	var version int
	err = s.db.QueryRowContext(context.Background(),
		"SELECT MAX(version) FROM schema_version").Scan(&version)
	require.NoError(t, err)
	assert.Equal(t, len(migrations), version)
}

// --- Future version protection ---

func TestMigration_RefusesNewerVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "future.db")

	// Create database with a version higher than we support.
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `CREATE TABLE IF NOT EXISTS schema_version (
		version INTEGER NOT NULL, applied_at TEXT NOT NULL)`)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), initialSchema)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), "INSERT INTO schema_version (version, applied_at) VALUES (9999, '2026-01-01T00:00:00Z')")
	require.NoError(t, err)
	db.Close()

	// OpenSQLite should refuse.
	_, err = OpenSQLite(t.Context(), path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "newer than this version")
}

// --- Backup ---

func TestMigration_BackupCreatedBeforeMigration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "backup-test.db")

	// Create a legacy v1 database — tables exist, no schema_version.
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), initialSchema)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(),
		`INSERT INTO entities (id, type, name, ecosystem, url, created_at, updated_at)
		 VALUES ('backup-data', 'package', 'data', '', '', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`)
	require.NoError(t, err)
	db.Close()

	// Open with OpenSQLite — the detector marks the DB as v1, then the
	// v2 migration runs and creates a backup before touching the schema.
	s, err := OpenSQLite(t.Context(), path)
	require.NoError(t, err)
	defer s.Close()

	// Find the v2 migration's pre-migration backup. The backup is named
	// .backup-vN- where N is the zero-based index of the migration being
	// applied — v2 is index 1, so we expect `.backup-v1-`.
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)

	backupFound := false
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "backup-test.db.backup-v1-") {
			backupFound = true
			info, err := e.Info()
			require.NoError(t, err)
			assert.Greater(t, info.Size(), int64(0), "backup should not be empty")
		}
	}
	assert.True(t, backupFound, "v2 migration should create a pre-migration backup")
}

func TestMigration_BackupPreservesPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "perm-test.db")

	// Create a file.
	f, err := os.Create(path)
	require.NoError(t, err)
	f.Write([]byte("test data"))
	f.Close()
	os.Chmod(path, 0600)

	err = backupDatabase(t.Context(), nil, path, 1)
	require.NoError(t, err)

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "perm-test.db.backup-v1-") {
			info, err := e.Info()
			require.NoError(t, err)
			assert.Equal(t, os.FileMode(0600), info.Mode().Perm(),
				"backup should have 0600 permissions")
		}
	}
}

func TestMigration_BackupNonexistentFile(t *testing.T) {
	// Backing up a file that doesn't exist should be a no-op.
	err := backupDatabase(t.Context(), nil, "/nonexistent/path/db.sqlite", 0)
	assert.NoError(t, err)
}

// --- Rollback ---

// TestMigration_RollbackDown verifies that rolling back one migration
// at a time unwinds the schema step-by-step. A full rollback walks
// vN → vN-1 → ... → v0 through N migrateDown calls.
func TestMigration_RollbackDown(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollback.db")

	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer db.Close()
	db.SetMaxOpenConns(1)

	// Migrate all the way up.
	err = migrate(t.Context(), db, path)
	require.NoError(t, err)

	version, err := getCurrentVersion(t.Context(), db)
	require.NoError(t, err)
	assert.Equal(t, len(migrations), version)

	// Insert data using v2 schema (short_name, canonical_uri).
	_, err = db.ExecContext(t.Context(),
		`INSERT INTO entities (id, canonical_uri, type, short_name, description, ecosystem, url, created_at, updated_at)
		 VALUES ('roll-1', 'pkg:npm/rollback-test', 'package', 'rollback-test', '', '', '', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`)
	require.NoError(t, err)

	// First rollback: v7 → v6. Drops the collected_from_entity_id
	// column from analyst_outputs. Pre-M2 data unaffected (it had
	// the column set to NULL).
	err = migrateDown(t.Context(), db, path)
	require.NoError(t, err)

	version, err = getCurrentVersion(t.Context(), db)
	require.NoError(t, err)
	assert.Equal(t, 6, version)

	// Second rollback: v6 → v5. Drops soft-delete columns from
	// postures/burns. Entity/signal/burn data is unaffected.
	err = migrateDown(t.Context(), db, path)
	require.NoError(t, err)

	version, err = getCurrentVersion(t.Context(), db)
	require.NoError(t, err)
	assert.Equal(t, 5, version)

	// Third rollback: v5 → v4. Renames conclusion tables back to finding
	// tables. The v4-and-earlier data we care about (entities, signals,
	// burns) is unaffected.
	err = migrateDown(t.Context(), db, path)
	require.NoError(t, err)

	version, err = getCurrentVersion(t.Context(), db)
	require.NoError(t, err)
	assert.Equal(t, 4, version)

	// Third rollback: v4 → v3. Drops the analyst-output stream
	// tables and signal_evidence; removes the signals.details column.
	// The v3-and-earlier data we care about (entities, signals,
	// burns) is unaffected.
	err = migrateDown(t.Context(), db, path)
	require.NoError(t, err)

	version, err = getCurrentVersion(t.Context(), db)
	require.NoError(t, err)
	assert.Equal(t, 3, version)

	// Fourth rollback: v3 → v2. Drops the append-only triggers.
	// Data is unaffected.
	err = migrateDown(t.Context(), db, path)
	require.NoError(t, err)

	version, err = getCurrentVersion(t.Context(), db)
	require.NoError(t, err)
	assert.Equal(t, 2, version)

	var v2ShortName string
	err = db.QueryRowContext(t.Context(), "SELECT short_name FROM entities WHERE id = 'roll-1'").Scan(&v2ShortName)
	require.NoError(t, err, "entity should still be readable after v3 rollback")
	assert.Equal(t, "rollback-test", v2ShortName)

	// Fifth rollback: v2 → v1. Data should survive; name column restored.
	err = migrateDown(t.Context(), db, path)
	require.NoError(t, err)

	version, err = getCurrentVersion(t.Context(), db)
	require.NoError(t, err)
	assert.Equal(t, 1, version)

	var v1Name string
	err = db.QueryRowContext(t.Context(), "SELECT name FROM entities WHERE id = 'roll-1'").Scan(&v1Name)
	require.NoError(t, err, "v1 name column should be readable after rollback from v2")
	assert.Equal(t, "rollback-test", v1Name, "v2 short_name should have been copied back to v1 name")

	// Sixth rollback: v1 → v0. Entities table dropped.
	err = migrateDown(t.Context(), db, path)
	require.NoError(t, err)

	version, err = getCurrentVersion(t.Context(), db)
	require.NoError(t, err)
	assert.Equal(t, 0, version)

	var count int
	err = db.QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='entities'").Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "entities table should be dropped after rolling back to v0")
}

func TestMigration_RollbackAtVersionZero(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.db")

	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer db.Close()
	db.SetMaxOpenConns(1)

	_, err = db.ExecContext(t.Context(), `CREATE TABLE IF NOT EXISTS schema_version (
		version INTEGER NOT NULL, applied_at TEXT NOT NULL)`)
	require.NoError(t, err)

	err = migrateDown(t.Context(), db, path)
	assert.Error(t, err, "should refuse to roll back from version 0")
	assert.Contains(t, err.Error(), "nothing to roll back")
}

func TestMigration_RollbackCreatesBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollback-backup.db")

	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer db.Close()
	db.SetMaxOpenConns(1)

	err = migrate(t.Context(), db, path)
	require.NoError(t, err)

	err = migrateDown(t.Context(), db, path)
	require.NoError(t, err)

	// migrateDown uses `currentVersion` as the backup's fromVersion, so
	// rolling back from v2 produces a `.backup-v2-` file.
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)

	backupFound := false
	for _, e := range entries {
		if strings.Contains(e.Name(), ".backup-v2-") {
			backupFound = true
		}
	}
	assert.True(t, backupFound, "rollback from v2 should create a .backup-v2- file")
}

// --- Migration consistency ---

func TestMigration_AllMigrationsHaveUpAndDown(t *testing.T) {
	for _, m := range migrations {
		assert.NotEmpty(t, m.Up, "migration %d (%s) has empty Up SQL", m.Version, m.Description)
		assert.NotEmpty(t, m.Down, "migration %d (%s) has empty Down SQL", m.Version, m.Description)
		assert.NotEmpty(t, m.Description, "migration %d has empty description", m.Version)
	}
}

func TestMigration_VersionsAreSequential(t *testing.T) {
	for i, m := range migrations {
		assert.Equal(t, i+1, m.Version,
			"migration at index %d should have version %d, got %d", i, i+1, m.Version)
	}
}

func TestMigration_UpDownRoundTrip(t *testing.T) {
	// Apply all migrations, then roll them all back, then apply again.
	// This verifies up/down are truly reversible at the schema level.
	dir := t.TempDir()
	path := filepath.Join(dir, "roundtrip.db")

	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer db.Close()
	db.SetMaxOpenConns(1)

	// Up.
	err = migrate(t.Context(), db, path)
	require.NoError(t, err)

	version, err := getCurrentVersion(t.Context(), db)
	require.NoError(t, err)
	assert.Equal(t, len(migrations), version)

	// Down all the way.
	for version > 0 {
		err = migrateDown(t.Context(), db, path)
		require.NoError(t, err)
		version, err = getCurrentVersion(t.Context(), db)
		require.NoError(t, err)
	}
	assert.Equal(t, 0, version)

	// Up again — should work cleanly.
	err = migrate(t.Context(), db, path)
	require.NoError(t, err)

	version, err = getCurrentVersion(t.Context(), db)
	require.NoError(t, err)
	assert.Equal(t, len(migrations), version)
}

// TestMigration_V2DataRoundTrip is the data-preservation version of
// TestMigration_UpDownRoundTrip. It starts at v1, seeds realistic legacy
// data, migrates up to v2, verifies the data was correctly translated,
// rolls back to v1, verifies the v1 schema is readable again, and
// re-applies v2. This is the regression gate for migration v2.
//
// What we verify survives:
//   - Entity id → canonical_uri copy on up-path
//   - Entity name → short_name copy on up-path
//   - Entity id unchanged through both paths
//   - short_name → name copy on down-path
//   - Posture rows carried through the PK change
//   - Burn rows untouched (they weren't part of v2's entity changes)
//   - Signal rows untouched (v2 is append-only but schema is same as v1)
func TestMigration_V2DataRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data-roundtrip.db")

	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer db.Close()
	db.SetMaxOpenConns(1)

	// --- Start at v1 with seed data. ---
	_, err = db.ExecContext(t.Context(), `CREATE TABLE IF NOT EXISTS schema_version (
		version INTEGER NOT NULL, applied_at TEXT NOT NULL)`)
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), initialSchema)
	require.NoError(t, err)
	require.NoError(t, recordVersion(t.Context(), db, 1))

	_, err = db.ExecContext(t.Context(), `
		INSERT INTO entities (id, type, name, ecosystem, url, created_at, updated_at) VALUES
			('pkg:npm:express', 'package', 'express', 'npm', 'https://github.com/expressjs/express',
			 '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'),
			('pkg:npm:lodash', 'package', 'lodash', 'npm', '',
			 '2026-01-02T00:00:00Z', '2026-01-02T00:00:00Z');

		INSERT INTO signals (id, entity_id, type, signal_group, source, forgery_resistance, value, collected_at, expires_at)
		VALUES ('sig-1', 'pkg:npm:express', 'stars', 'criticality', 'github', 'medium-declining',
		        '{"count":65000}', '2026-01-01T00:00:00Z', '2026-01-02T00:00:00Z');

		INSERT INTO postures (entity_id, tier, version, rationale, set_by, set_at)
		VALUES ('pkg:npm:express', 'vetted-frozen', '4.18.2', 'audited', 'sarah', '2026-01-01T00:00:00Z');

		INSERT INTO burns (entity_id, reason, source, source_org, burned_at, burned_by)
		VALUES ('pkg:npm:lodash', 'dry run burn', 'local', '', '2026-01-03T00:00:00Z', 'sarah');
	`)
	require.NoError(t, err)

	// --- Up: v1 → vN (currently v3). ---
	err = migrate(t.Context(), db, path)
	require.NoError(t, err)

	version, err := getCurrentVersion(t.Context(), db)
	require.NoError(t, err)
	assert.Equal(t, len(migrations), version)

	// Entity data should be translated: id unchanged, canonical_uri copied
	// from id, short_name copied from name, v1 name column gone.
	var shortName, canonicalURI string
	err = db.QueryRowContext(t.Context(), "SELECT short_name, canonical_uri FROM entities WHERE id = 'pkg:npm:express'").
		Scan(&shortName, &canonicalURI)
	require.NoError(t, err)
	assert.Equal(t, "express", shortName)
	assert.Equal(t, "pkg:npm:express", canonicalURI)

	// v1 name column should be gone.
	var col string
	err = db.QueryRowContext(t.Context(),
		`SELECT name FROM pragma_table_info('entities') WHERE name = 'name'`).Scan(&col)
	assert.ErrorIs(t, err, sql.ErrNoRows, "v1 name column should not exist after v2 up")

	// Signals and burns untouched.
	var sigCount, burnCount int
	require.NoError(t, db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM signals").Scan(&sigCount))
	assert.Equal(t, 1, sigCount)
	require.NoError(t, db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM burns").Scan(&burnCount))
	assert.Equal(t, 1, burnCount)

	// Posture survived the PK change.
	var postureCount int
	require.NoError(t, db.QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM postures WHERE entity_id = 'pkg:npm:express' AND version = '4.18.2'").
		Scan(&postureCount))
	assert.Equal(t, 1, postureCount)

	// --- Down: vN → v6 (drops collected_from_entity_id from analyst_outputs). ---
	// The v7 migration added one nullable column; rolling back drops
	// it. V7 is M2 identity-indexing; pre-M2 data had the column set
	// to NULL (untouched by backfill per D2).
	err = migrateDown(t.Context(), db, path)
	require.NoError(t, err)

	version, err = getCurrentVersion(t.Context(), db)
	require.NoError(t, err)
	assert.Equal(t, 6, version)

	// --- Down: v6 → v5 (drops soft-delete columns from postures/burns). ---
	// The v6 migration added withdrawn_at/by/reason columns to both
	// tables; rolling back drops them. The v2 entity/signal/posture
	// data we care about for this test is untouched.
	err = migrateDown(t.Context(), db, path)
	require.NoError(t, err)

	version, err = getCurrentVersion(t.Context(), db)
	require.NoError(t, err)
	assert.Equal(t, 5, version)

	// --- Down: v5 → v4 (renames conclusion tables back to finding tables). ---
	// The v5 migration only renames tables; no data is dropped.
	// The v2 entity/signal data we care about for this test is untouched.
	err = migrateDown(t.Context(), db, path)
	require.NoError(t, err)

	version, err = getCurrentVersion(t.Context(), db)
	require.NoError(t, err)
	assert.Equal(t, 4, version)

	// --- Down: v4 → v3 (drops analyst-output stream + signal evidence). ---
	// The v4 migration adds new tables that have no v3 precedent;
	// rolling back drops them and removes the signals.details column.
	// The v2 entity/signal data we care about for this test is
	// untouched.
	err = migrateDown(t.Context(), db, path)
	require.NoError(t, err)

	version, err = getCurrentVersion(t.Context(), db)
	require.NoError(t, err)
	assert.Equal(t, 3, version)

	// --- Down: v3 → v2 (drops append-only triggers). ---
	// This is a no-op for the data we care about — the v3 migration
	// only adds triggers, so rolling it back leaves the v2 schema and
	// data intact. We still walk it explicitly so the round-trip is
	// step-by-step.
	err = migrateDown(t.Context(), db, path)
	require.NoError(t, err)

	version, err = getCurrentVersion(t.Context(), db)
	require.NoError(t, err)
	assert.Equal(t, 2, version)

	// --- Down: v2 → v1. ---
	err = migrateDown(t.Context(), db, path)
	require.NoError(t, err)

	version, err = getCurrentVersion(t.Context(), db)
	require.NoError(t, err)
	assert.Equal(t, 1, version)

	// Entity data restored: v1 name column is back, populated from short_name.
	var name string
	err = db.QueryRowContext(t.Context(), "SELECT name FROM entities WHERE id = 'pkg:npm:express'").Scan(&name)
	require.NoError(t, err)
	assert.Equal(t, "express", name, "v1 name should have been restored from v2 short_name")

	// v2 columns should be gone.
	err = db.QueryRowContext(t.Context(),
		`SELECT name FROM pragma_table_info('entities') WHERE name = 'canonical_uri'`).Scan(&col)
	assert.ErrorIs(t, err, sql.ErrNoRows, "v2 canonical_uri column should not exist after rollback")

	// Signals and burns still there.
	require.NoError(t, db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM signals").Scan(&sigCount))
	assert.Equal(t, 1, sigCount)
	require.NoError(t, db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM burns").Scan(&burnCount))
	assert.Equal(t, 1, burnCount)

	// --- Up again: v1 → vN. Second forward trip should work cleanly. ---
	err = migrate(t.Context(), db, path)
	require.NoError(t, err)

	version, err = getCurrentVersion(t.Context(), db)
	require.NoError(t, err)
	assert.Equal(t, len(migrations), version)

	err = db.QueryRowContext(t.Context(), "SELECT short_name FROM entities WHERE id = 'pkg:npm:express'").Scan(&shortName)
	require.NoError(t, err)
	assert.Equal(t, "express", shortName, "data should survive a full v1→vN→v1→vN loop")
}
