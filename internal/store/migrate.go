package store

import (
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	{
		Version:     2,
		Description: "entity model v2: UUID PKs, canonical URI, append-only signals, versioned posture, dependency observations, audit log",
		Up:          migrationV2Up,
		Down:        migrationV2Down,
	},
	{
		Version:     3,
		Description: "append-only enforcement: triggers blocking UPDATE/DELETE on signals, dependency_observations, signal_resolutions, audit_log",
		Up:          migrationV3Up,
		Down:        migrationV3Down,
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

// migrationV2Up evolves the schema for entity model v2.
// Key changes:
//   - Entities: drop v1 `name`, add canonical_uri, short_name, description.
//     Per design/entity-model-v2.md, short_name + description replace the
//     old single-purpose name column.
//   - Signals: append-only (no upsert), keep existing data.
//   - Postures: versioned PK (entity_id, version).
//   - New tables: dependency_observations, signal_resolutions, audit_log,
//     team_identities.
//
// Index sequencing note: SQLite's ALTER TABLE DROP COLUMN refuses to drop
// a column that is referenced by an index. The v1 schema has
// idx_entities_name_type ON entities(name, type), so we must drop that
// index *before* dropping the name column, otherwise the migration fails.
const migrationV2Up = `
-- Add new columns to entities (keep v1 name column for now — we copy
-- from it below before dropping).
ALTER TABLE entities ADD COLUMN canonical_uri TEXT NOT NULL DEFAULT '';
ALTER TABLE entities ADD COLUMN short_name TEXT NOT NULL DEFAULT '';
ALTER TABLE entities ADD COLUMN description TEXT NOT NULL DEFAULT '';

-- Populate canonical_uri and short_name from legacy data. Only rows with
-- empty canonical_uri are touched, so re-running is a no-op (defensive
-- even though migrations run once).
UPDATE entities SET canonical_uri = id, short_name = name WHERE canonical_uri = '';

-- Drop the v1 index that references name, then drop the name column
-- itself. Order matters: SQLite blocks DROP COLUMN if any index still
-- references the column.
DROP INDEX IF EXISTS idx_entities_name_type;
ALTER TABLE entities DROP COLUMN name;

-- V2 indexes per the entity-model-v2.md spec.
CREATE UNIQUE INDEX IF NOT EXISTS idx_entities_canonical_uri ON entities(canonical_uri);
CREATE INDEX IF NOT EXISTS idx_entities_type ON entities(type);

-- Postures: recreate with composite PK (entity_id, version).
-- SQLite cannot alter primary keys, so we recreate the table.
CREATE TABLE postures_v2 (
	entity_id TEXT NOT NULL REFERENCES entities(id),
	version   TEXT NOT NULL DEFAULT '',
	tier      TEXT NOT NULL,
	rationale TEXT NOT NULL,
	set_by    TEXT NOT NULL,
	set_at    TEXT NOT NULL,
	PRIMARY KEY (entity_id, version)
);
INSERT INTO postures_v2 (entity_id, version, tier, rationale, set_by, set_at)
	SELECT entity_id, version, tier, rationale, set_by, set_at FROM postures;
DROP TABLE postures;
ALTER TABLE postures_v2 RENAME TO postures;

-- Dependency observations (append-only).
CREATE TABLE IF NOT EXISTS dependency_observations (
	id          TEXT PRIMARY KEY,
	project_id  TEXT NOT NULL REFERENCES entities(id),
	entity_id   TEXT NOT NULL REFERENCES entities(id),
	version     TEXT NOT NULL,
	direct      INTEGER NOT NULL,
	observed_at TEXT NOT NULL,
	survey_id   TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_depobs_project ON dependency_observations(project_id);
CREATE INDEX IF NOT EXISTS idx_depobs_survey ON dependency_observations(survey_id);

-- Signal resolutions (append-only conflict resolution).
CREATE TABLE IF NOT EXISTS signal_resolutions (
	id                   TEXT PRIMARY KEY,
	entity_id            TEXT NOT NULL,
	signal_type          TEXT NOT NULL,
	kept_signal_id       TEXT NOT NULL REFERENCES signals(id),
	superseded_signal_id TEXT NOT NULL REFERENCES signals(id),
	action               TEXT NOT NULL,
	resolved_by          TEXT NOT NULL,
	resolved_at          TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_resolutions_entity ON signal_resolutions(entity_id);

-- Audit log (append-only).
CREATE TABLE IF NOT EXISTS audit_log (
	id         TEXT PRIMARY KEY,
	timestamp  TEXT NOT NULL,
	actor      TEXT NOT NULL,
	action     TEXT NOT NULL,
	entity_id  TEXT,
	detail     TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_audit_timestamp ON audit_log(timestamp);
CREATE INDEX IF NOT EXISTS idx_audit_entity ON audit_log(entity_id);

-- Team identities.
CREATE TABLE IF NOT EXISTS team_identities (
	id            TEXT PRIMARY KEY,
	name          TEXT NOT NULL,
	created_at    TEXT NOT NULL,
	halted_at     TEXT,
	revoked_at    TEXT,
	revoke_reason TEXT
);
`

// migrationV2Down rolls back migration v2 to a readable v1 state.
//
// Rollback semantics per design/entity-model-v2.md:246 — rollback is a
// recovery mechanism, not a feature. Data only present in v2 (audit log,
// dependency observations, signal resolutions, team identities, posture
// version history) is lost on rollback. The v1 `entities` and `signals`
// tables are fully restored, and `postures` collapses to one row per
// entity by keeping whichever row SQLite returns last (order-insensitive
// rollback — the user gets a working v1 schema, not a time machine).
//
// Order matters here too. To restore the v1 `name` column we must:
//  1. Drop the v2 indexes that reference canonical_uri / type
//  2. Re-add the `name` column
//  3. Copy short_name → name so v1 code can read it
//  4. Drop the v2 columns
//  5. Recreate the v1 index on (name, type)
//
// Dropping v2 indexes before v2 columns avoids the same DROP COLUMN
// "column in use by index" error that bit us on the up-path.
const migrationV2Down = `
-- Drop v2-only tables (their data is lost by design).
DROP TABLE IF EXISTS team_identities;
DROP TABLE IF EXISTS audit_log;
DROP TABLE IF EXISTS signal_resolutions;
DROP TABLE IF EXISTS dependency_observations;

-- Recreate postures with original PK (entity_id only). Version history
-- is collapsed — we keep one row per entity, whichever the SELECT
-- yields last. This is acceptable loss for a recovery rollback.
CREATE TABLE postures_v1 (
	entity_id TEXT PRIMARY KEY REFERENCES entities(id),
	tier      TEXT NOT NULL,
	version   TEXT NOT NULL DEFAULT '',
	rationale TEXT NOT NULL,
	set_by    TEXT NOT NULL,
	set_at    TEXT NOT NULL
);
INSERT OR REPLACE INTO postures_v1 (entity_id, version, tier, rationale, set_by, set_at)
	SELECT entity_id, version, tier, rationale, set_by, set_at FROM postures;
DROP TABLE postures;
ALTER TABLE postures_v1 RENAME TO postures;

-- Drop v2 entity indexes before dropping v2 columns — SQLite DROP
-- COLUMN fails if the column is referenced by an index.
DROP INDEX IF EXISTS idx_entities_type;
DROP INDEX IF EXISTS idx_entities_canonical_uri;

-- Restore v1 name column and populate it from short_name so v1 code
-- can still read these rows.
ALTER TABLE entities ADD COLUMN name TEXT NOT NULL DEFAULT '';
UPDATE entities SET name = short_name;

-- Drop the v2-only entity columns.
-- SQLite 3.35.0+ supports ALTER TABLE DROP COLUMN. The modernc driver
-- ships SQLite 3.51+, so this is safe.
ALTER TABLE entities DROP COLUMN canonical_uri;
ALTER TABLE entities DROP COLUMN short_name;
ALTER TABLE entities DROP COLUMN description;

-- Recreate the v1 index on the restored name column.
CREATE INDEX IF NOT EXISTS idx_entities_name_type ON entities(name, type);
`

// migrationV3Up adds BEFORE UPDATE and BEFORE DELETE triggers to the
// four append-only tables. This converts the convention documented at
// sqlite.go:8 (signals, dependency observations, signal resolutions,
// and audit entries are append-only) into a schema-enforced invariant.
//
// The triggers fire on any UPDATE or DELETE statement, regardless of
// source — Go application code, raw SQL via the sqlite3 shell, or
// anything else that goes through the SQLite parser. RAISE(ABORT, ...)
// aborts the offending statement and rolls back any pending changes
// from that statement; the surrounding transaction is not affected
// unless the caller chooses to roll it back.
//
// Tables that are NOT append-only — entities, postures, burns,
// team_identities — are intentionally left mutable. Their methods
// (PutEntity, SetPosture, SetBurn, PutTeamIdentity) upsert.
const migrationV3Up = `
CREATE TRIGGER signals_no_update BEFORE UPDATE ON signals
    BEGIN SELECT RAISE(ABORT, 'signals are append-only'); END;
CREATE TRIGGER signals_no_delete BEFORE DELETE ON signals
    BEGIN SELECT RAISE(ABORT, 'signals are append-only'); END;

CREATE TRIGGER dependency_observations_no_update BEFORE UPDATE ON dependency_observations
    BEGIN SELECT RAISE(ABORT, 'dependency_observations are append-only'); END;
CREATE TRIGGER dependency_observations_no_delete BEFORE DELETE ON dependency_observations
    BEGIN SELECT RAISE(ABORT, 'dependency_observations are append-only'); END;

CREATE TRIGGER signal_resolutions_no_update BEFORE UPDATE ON signal_resolutions
    BEGIN SELECT RAISE(ABORT, 'signal_resolutions are append-only'); END;
CREATE TRIGGER signal_resolutions_no_delete BEFORE DELETE ON signal_resolutions
    BEGIN SELECT RAISE(ABORT, 'signal_resolutions are append-only'); END;

CREATE TRIGGER audit_log_no_update BEFORE UPDATE ON audit_log
    BEGIN SELECT RAISE(ABORT, 'audit_log is append-only'); END;
CREATE TRIGGER audit_log_no_delete BEFORE DELETE ON audit_log
    BEGIN SELECT RAISE(ABORT, 'audit_log is append-only'); END;
`

// migrationV3Down drops the append-only triggers, restoring the
// pre-v3 behavior where these tables can be mutated freely. This is
// a recovery path only — running it on a populated database removes
// the schema-level append-only enforcement and reverts to convention.
const migrationV3Down = `
DROP TRIGGER IF EXISTS audit_log_no_delete;
DROP TRIGGER IF EXISTS audit_log_no_update;
DROP TRIGGER IF EXISTS signal_resolutions_no_delete;
DROP TRIGGER IF EXISTS signal_resolutions_no_update;
DROP TRIGGER IF EXISTS dependency_observations_no_delete;
DROP TRIGGER IF EXISTS dependency_observations_no_update;
DROP TRIGGER IF EXISTS signals_no_delete;
DROP TRIGGER IF EXISTS signals_no_update;
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
//
// Format: signatory.db.backup-v{version}-{timestamp}-{random}
//
// The {random} suffix is generated by os.CreateTemp and serves three
// distinct security purposes (issue #82):
//
//  1. O_EXCL atomic creation prevents clobbering an existing file at
//     the predicted path. The previous implementation used O_CREATE|
//     O_WRONLY without O_EXCL, so any pre-existing file at the path
//     was opened for writing and partially overwritten by the database
//     bytes (and without O_TRUNC, the trailing bytes survived,
//     producing a corrupt backup).
//  2. The unguessable random suffix prevents symlink-race attacks. An
//     attacker who could predict the backup path could plant a symlink
//     pointing at e.g. /etc/cron.d/something and redirect the database
//     bytes to that target. With a random suffix the attacker has no
//     path to plant the symlink at.
//  3. The random suffix prevents within-second collisions when two
//     backups happen in the same second (the timestamp is only second-
//     precision).
//
// CreateTemp opens with O_RDWR|O_CREATE|O_EXCL, mode 0600 — same
// permission as the previous explicit OpenFile call.
func backupDatabase(db *sql.DB, dbPath string, fromVersion int) error {
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

	dir := filepath.Dir(dbPath)
	pattern := fmt.Sprintf("%s.backup-v%d-%s-*",
		filepath.Base(dbPath), fromVersion,
		time.Now().UTC().Format("20060102-150405"))

	dst, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return fmt.Errorf("create backup: %w", err)
	}

	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		// Clean up the partial backup file on copy failure so we don't
		// leave a truncated backup masquerading as a valid one.
		os.Remove(dst.Name())
		return fmt.Errorf("copy database to backup: %w", err)
	}

	// Explicit close to catch flush errors (M1 from review).
	if err := dst.Close(); err != nil {
		os.Remove(dst.Name())
		return fmt.Errorf("finalize backup: %w", err)
	}

	return nil
}
