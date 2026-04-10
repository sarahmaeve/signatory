package store

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigration_BackupIncludesWALData verifies that backups contain
// all committed data, including transactions still in the WAL.
// (Review 3, C2)
func TestMigration_BackupIncludesWALData(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wal-test.db")

	// Create and populate database in WAL mode.
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	_, err = db.Exec("PRAGMA journal_mode=WAL")
	require.NoError(t, err)
	_, err = db.Exec("CREATE TABLE test_data (id TEXT PRIMARY KEY, value TEXT)")
	require.NoError(t, err)
	_, err = db.Exec("INSERT INTO test_data (id, value) VALUES ('key1', 'important_data')")
	require.NoError(t, err)
	// Do NOT checkpoint — data may be in WAL only.

	// Backup using our function — should checkpoint WAL first.
	err = backupDatabase(db, path, 0)
	require.NoError(t, err)
	db.Close()

	// Find and open the backup.
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)

	var backupPath string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "wal-test.db.backup-v0-") {
			backupPath = filepath.Join(dir, e.Name())
		}
	}
	require.NotEmpty(t, backupPath, "backup file should exist")

	// Open the backup and verify the data is present.
	backupDB, err := sql.Open("sqlite", backupPath)
	require.NoError(t, err)
	defer backupDB.Close()

	var value string
	err = backupDB.QueryRow("SELECT value FROM test_data WHERE id = 'key1'").Scan(&value)
	require.NoError(t, err, "backup should contain committed data including WAL contents")
	assert.Equal(t, "important_data", value)
}

// TestMigration_BackupVersionTagUsesIterationVersion verifies that
// each migration in a batch gets a uniquely-tagged backup.
// (Review 3, H2)
func TestMigration_BackupVersionTagUsesIterationVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "version-tag.db")

	// Create backups for version 0 and version 1.
	// Use a short sleep to ensure different timestamps.
	err := backupDatabase(nil, path+"doesnotexist", 0)
	assert.NoError(t, err) // no-op for nonexistent file

	// Create the file so backups work.
	f, err := os.Create(path)
	require.NoError(t, err)
	f.Write([]byte("test"))
	f.Close()

	err = backupDatabase(nil, path, 0)
	require.NoError(t, err)
	err = backupDatabase(nil, path, 1)
	require.NoError(t, err)

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)

	v0Count := 0
	v1Count := 0
	for _, e := range entries {
		if strings.Contains(e.Name(), ".backup-v0-") {
			v0Count++
		}
		if strings.Contains(e.Name(), ".backup-v1-") {
			v1Count++
		}
	}
	assert.Equal(t, 1, v0Count, "should have exactly one v0 backup")
	assert.Equal(t, 1, v1Count, "should have exactly one v1 backup")
}
