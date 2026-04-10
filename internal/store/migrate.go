package store

import (
	"database/sql"
	"fmt"
	"io"
	"os"
	"time"
)

// Migration represents a single schema migration with both forward
// (Up) and reverse (Down) SQL. Every migration must be reversible
// to protect against data corruption during upgrades.
type Migration struct {
	Version     int
	Description string
	Up          string
	Down        string
}

// migrations is the ordered list of all schema migrations.
// New migrations are appended to the end with the next version number.
// NEVER modify an existing migration — always add a new one.
var migrations = []Migration{
	{
		Version:     1,
		Description: "initial schema",
		Up:          initialSchema,
		Down:        dropInitialSchema,
	},
}

// initialSchema is the v1 schema, extracted from the original
// CREATE TABLE IF NOT EXISTS statements.
const initialSchema = `
CREATE TABLE IF NOT EXISTS entities (
	id         TEXT PRIMARY KEY,
	type       TEXT NOT NULL,
	name       TEXT NOT NULL,
	ecosystem  TEXT NOT NULL DEFAULT '',
	url        TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_entities_name_type ON entities(name, type);

CREATE TABLE IF NOT EXISTS signals (
	id                 TEXT PRIMARY KEY,
	entity_id          TEXT NOT NULL REFERENCES entities(id),
	type               TEXT NOT NULL,
	signal_group       TEXT NOT NULL,
	source             TEXT NOT NULL,
	forgery_resistance TEXT NOT NULL,
	value              TEXT NOT NULL,
	collected_at       TEXT NOT NULL,
	expires_at         TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_signals_entity ON signals(entity_id);
CREATE INDEX IF NOT EXISTS idx_signals_entity_group ON signals(entity_id, signal_group);

CREATE TABLE IF NOT EXISTS postures (
	entity_id TEXT PRIMARY KEY REFERENCES entities(id),
	tier      TEXT NOT NULL,
	version   TEXT NOT NULL DEFAULT '',
	rationale TEXT NOT NULL,
	set_by    TEXT NOT NULL,
	set_at    TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS burns (
	entity_id  TEXT PRIMARY KEY REFERENCES entities(id),
	reason     TEXT NOT NULL,
	source     TEXT NOT NULL,
	source_org TEXT NOT NULL DEFAULT '',
	burned_at  TEXT NOT NULL,
	burned_by  TEXT NOT NULL
);
`

const dropInitialSchema = `
DROP TABLE IF EXISTS burns;
DROP TABLE IF EXISTS postures;
DROP TABLE IF EXISTS signals;
DROP TABLE IF EXISTS entities;
`

// migrate runs all pending migrations on the database. It:
// 1. Creates the schema_version table if it doesn't exist
// 2. Detects legacy databases (tables exist but no version) and marks them as v1
// 3. Backs up the database file before each migration
// 4. Applies each migration in a transaction
// 5. Refuses to open if the database is newer than the code supports
func migrate(db *sql.DB, dbPath string) error {
	// Create the version tracking table.
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS schema_version (
			version    INTEGER NOT NULL,
			applied_at TEXT NOT NULL
		)
	`); err != nil {
		return fmt.Errorf("create schema_version table: %w", err)
	}

	currentVersion, err := getCurrentVersion(db)
	if err != nil {
		return err
	}

	// Detect legacy database: tables exist but no version recorded.
	if currentVersion == 0 {
		hasLegacyTables, err := detectLegacyTables(db)
		if err != nil {
			return err
		}
		if hasLegacyTables {
			// Mark as v1 — the initial schema is already applied.
			if err := recordVersion(db, 1); err != nil {
				return fmt.Errorf("record legacy version: %w", err)
			}
			currentVersion = 1
		}
	}

	latestVersion := len(migrations)

	// Refuse to open a database newer than this code supports.
	if currentVersion > latestVersion {
		return fmt.Errorf(
			"database schema version %d is newer than this version of signatory supports (max %d); "+
				"upgrade signatory or use the database with a newer version",
			currentVersion, latestVersion)
	}

	// Apply pending migrations.
	for i := currentVersion; i < latestVersion; i++ {
		m := migrations[i]

		// Backup before migration.
		if dbPath != "" {
			if err := backupDatabase(db, dbPath, i); err != nil {
				return fmt.Errorf("backup before migration %d: %w", m.Version, err)
			}
		}

		// Apply migration and record version atomically in one transaction.
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", m.Version, err)
		}

		if _, err := tx.Exec(m.Up); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d (%s) failed: %w", m.Version, m.Description, err)
		}

		if _, err := tx.Exec(
			"INSERT INTO schema_version (version, applied_at) VALUES (?, ?)",
			m.Version, time.Now().UTC().Format(time.RFC3339)); err != nil {
			tx.Rollback()
			return fmt.Errorf("record version %d: %w", m.Version, err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", m.Version, err)
		}
	}

	return nil
}

// migrateDown rolls back the most recent migration. It backs up the
// database before rolling back.
func migrateDown(db *sql.DB, dbPath string) error {
	currentVersion, err := getCurrentVersion(db)
	if err != nil {
		return err
	}

	if currentVersion == 0 {
		return fmt.Errorf("database is at version 0, nothing to roll back")
	}

	if currentVersion > len(migrations) {
		return fmt.Errorf("database version %d is newer than code supports (max %d)", currentVersion, len(migrations))
	}

	m := migrations[currentVersion-1]

	// Backup before rollback.
	if dbPath != "" {
		if err := backupDatabase(db, dbPath, currentVersion); err != nil {
			return fmt.Errorf("backup before rollback from %d: %w", m.Version, err)
		}
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin rollback %d: %w", m.Version, err)
	}

	if _, err := tx.Exec(m.Down); err != nil {
		tx.Rollback()
		return fmt.Errorf("rollback %d (%s) failed: %w", m.Version, m.Description, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit rollback %d: %w", m.Version, err)
	}

	// Update version: delete the rolled-back version entry.
	if _, err := db.Exec("DELETE FROM schema_version WHERE version = ?", m.Version); err != nil {
		return fmt.Errorf("delete version %d: %w", m.Version, err)
	}

	return nil
}

// getCurrentVersion returns the highest applied migration version,
// or 0 if no migrations have been recorded.
func getCurrentVersion(db *sql.DB) (int, error) {
	var version int
	err := db.QueryRow("SELECT COALESCE(MAX(version), 0) FROM schema_version").Scan(&version)
	if err != nil {
		return 0, fmt.Errorf("get current version: %w", err)
	}
	return version, nil
}

// detectLegacyTables checks if the database has the original schema
// tables but no version tracking.
func detectLegacyTables(db *sql.DB) (bool, error) {
	var count int
	err := db.QueryRow(
		"SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='entities'",
	).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("detect legacy tables: %w", err)
	}
	return count > 0, nil
}

// recordVersion inserts a version record into schema_version.
func recordVersion(db *sql.DB, version int) error {
	_, err := db.Exec(
		"INSERT INTO schema_version (version, applied_at) VALUES (?, ?)",
		version, time.Now().UTC().Format(time.RFC3339))
	return err
}

// backupDatabase checkpoints the WAL and copies the database file to a
// timestamped backup. The checkpoint ensures all committed transactions
// are flushed to the main database file before copying.
// Format: signatory.db.backup-v{version}-{timestamp}
func backupDatabase(db *sql.DB, dbPath string, fromVersion int) error {
	backupPath := fmt.Sprintf("%s.backup-v%d-%s",
		dbPath, fromVersion,
		time.Now().UTC().Format("20060102-150405"))

	// Checkpoint WAL to ensure all committed data is in the main file.
	// TRUNCATE mode flushes the WAL and truncates it to zero bytes,
	// ensuring the backup of the main file is complete.
	if db != nil {
		if _, err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
			return fmt.Errorf("checkpoint WAL before backup: %w", err)
		}
	}

	src, err := os.Open(dbPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // Nothing to back up for a new database.
		}
		return err
	}
	defer src.Close()

	dst, err := os.OpenFile(backupPath, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("create backup at %s: %w", backupPath, err)
	}

	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		return fmt.Errorf("copy database to backup: %w", err)
	}

	// Explicit close to catch flush errors (M1 from review).
	if err := dst.Close(); err != nil {
		return fmt.Errorf("finalize backup: %w", err)
	}

	return nil
}
