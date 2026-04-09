package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"github.com/sarahmaeve/signatory/internal/profile"
)

// SQLite implements the Store interface using a local SQLite database.
type SQLite struct {
	db *sql.DB
}

// OpenSQLite opens or creates a SQLite database at the given path.
// It creates the parent directory if it does not exist and runs
// schema migrations.
func OpenSQLite(path string) (*SQLite, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	// Limit to one connection. SQLite allows only one writer at a time;
	// database/sql's connection pool would open multiple connections whose
	// per-connection PRAGMA settings (busy_timeout, journal_mode) are not
	// shared. A single connection serializes access and ensures pragmas
	// apply consistently. This is the standard recommendation for SQLite
	// with Go's database/sql and is appropriate for a single-user CLI tool.
	db.SetMaxOpenConns(1)

	// Set pragmas for WAL mode and busy timeout.
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("set %s: %w", pragma, err)
		}
	}

	s := &SQLite{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate database: %w", err)
	}

	return s, nil
}

func (s *SQLite) migrate() error {
	_, err := s.db.Exec(schema)
	return err
}

const schema = `
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

// Close closes the database connection.
func (s *SQLite) Close() error {
	return s.db.Close()
}

// GetEntity retrieves an entity by ID.
func (s *SQLite) GetEntity(ctx context.Context, id string) (*profile.Entity, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, type, name, ecosystem, url, created_at, updated_at FROM entities WHERE id = ?`, id)
	return scanEntity(row)
}

// FindEntity retrieves an entity by name and type.
func (s *SQLite) FindEntity(ctx context.Context, name string, entityType profile.EntityType) (*profile.Entity, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, type, name, ecosystem, url, created_at, updated_at FROM entities WHERE name = ? AND type = ?`,
		name, string(entityType))
	return scanEntity(row)
}

// PutEntity inserts or updates an entity.
func (s *SQLite) PutEntity(ctx context.Context, entity *profile.Entity) error {
	if entity == nil {
		return ErrNilInput
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO entities (id, type, name, ecosystem, url, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
			type = excluded.type,
			name = excluded.name,
			ecosystem = excluded.ecosystem,
			url = excluded.url,
			updated_at = excluded.updated_at`,
		entity.ID, string(entity.Type), entity.Name, entity.Ecosystem, entity.URL,
		entity.CreatedAt.Format(time.RFC3339), entity.UpdatedAt.Format(time.RFC3339))
	return err
}

// GetSignals retrieves all signals for an entity.
func (s *SQLite) GetSignals(ctx context.Context, entityID string) ([]profile.Signal, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, entity_id, type, signal_group, source, forgery_resistance, value, collected_at, expires_at
		 FROM signals WHERE entity_id = ?`, entityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSignals(rows)
}

// GetSignalsByGroup retrieves signals for an entity filtered by group.
func (s *SQLite) GetSignalsByGroup(ctx context.Context, entityID string, group profile.SignalGroup) ([]profile.Signal, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, entity_id, type, signal_group, source, forgery_resistance, value, collected_at, expires_at
		 FROM signals WHERE entity_id = ? AND signal_group = ?`, entityID, string(group))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanSignals(rows)
}

// PutSignals inserts or updates signals.
func (s *SQLite) PutSignals(ctx context.Context, signals []profile.Signal) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO signals (id, entity_id, type, signal_group, source, forgery_resistance, value, collected_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
			type = excluded.type,
			signal_group = excluded.signal_group,
			source = excluded.source,
			forgery_resistance = excluded.forgery_resistance,
			value = excluded.value,
			collected_at = excluded.collected_at,
			expires_at = excluded.expires_at`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, sig := range signals {
		valueBytes, err := json.Marshal(sig.Value)
		if err != nil {
			return fmt.Errorf("marshal signal value: %w", err)
		}
		_, err = stmt.ExecContext(ctx,
			sig.ID, sig.EntityID, sig.Type, string(sig.Group),
			sig.Source, string(sig.ForgeryResistance), string(valueBytes),
			sig.CollectedAt.Format(time.RFC3339), sig.ExpiresAt.Format(time.RFC3339))
		if err != nil {
			return err
		}
	}

	return tx.Commit()
}

// GetPosture retrieves the posture for an entity.
func (s *SQLite) GetPosture(ctx context.Context, entityID string) (*profile.Posture, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT entity_id, tier, version, rationale, set_by, set_at FROM postures WHERE entity_id = ?`, entityID)

	var p profile.Posture
	var setAt string
	err := row.Scan(&p.EntityID, (*string)(&p.Tier), &p.Version, &p.Rationale, &p.SetBy, &setAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p.SetAt, err = time.Parse(time.RFC3339, setAt)
	if err != nil {
		return nil, fmt.Errorf("parse posture set_at %q: %w", setAt, err)
	}
	return &p, nil
}

// SetPosture inserts or updates a posture.
func (s *SQLite) SetPosture(ctx context.Context, posture *profile.Posture) error {
	if posture == nil {
		return ErrNilInput
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO postures (entity_id, tier, version, rationale, set_by, set_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(entity_id) DO UPDATE SET
			tier = excluded.tier,
			version = excluded.version,
			rationale = excluded.rationale,
			set_by = excluded.set_by,
			set_at = excluded.set_at`,
		posture.EntityID, string(posture.Tier), posture.Version, posture.Rationale,
		posture.SetBy, posture.SetAt.Format(time.RFC3339))
	return err
}

// GetBurn retrieves the burn for an entity.
func (s *SQLite) GetBurn(ctx context.Context, entityID string) (*profile.Burn, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT entity_id, reason, source, source_org, burned_at, burned_by FROM burns WHERE entity_id = ?`, entityID)

	var b profile.Burn
	var burnedAt string
	err := row.Scan(&b.EntityID, &b.Reason, (*string)(&b.Source), &b.SourceOrg, &burnedAt, &b.BurnedBy)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	b.BurnedAt, err = time.Parse(time.RFC3339, burnedAt)
	if err != nil {
		return nil, fmt.Errorf("parse burn burned_at %q: %w", burnedAt, err)
	}
	return &b, nil
}

// SetBurn inserts or updates a burn.
func (s *SQLite) SetBurn(ctx context.Context, burn *profile.Burn) error {
	if burn == nil {
		return ErrNilInput
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO burns (entity_id, reason, source, source_org, burned_at, burned_by)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(entity_id) DO UPDATE SET
			reason = excluded.reason,
			source = excluded.source,
			source_org = excluded.source_org,
			burned_at = excluded.burned_at,
			burned_by = excluded.burned_by`,
		burn.EntityID, burn.Reason, string(burn.Source), burn.SourceOrg,
		burn.BurnedAt.Format(time.RFC3339), burn.BurnedBy)
	return err
}

// ListBurns retrieves all burns.
func (s *SQLite) ListBurns(ctx context.Context) ([]profile.Burn, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT entity_id, reason, source, source_org, burned_at, burned_by FROM burns`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var burns []profile.Burn
	for rows.Next() {
		var b profile.Burn
		var burnedAt string
		if err := rows.Scan(&b.EntityID, &b.Reason, (*string)(&b.Source), &b.SourceOrg, &burnedAt, &b.BurnedBy); err != nil {
			return nil, err
		}
		var parseErr error
		b.BurnedAt, parseErr = time.Parse(time.RFC3339, burnedAt)
		if parseErr != nil {
			return nil, fmt.Errorf("parse burn burned_at %q for entity %s: %w", burnedAt, b.EntityID, parseErr)
		}
		burns = append(burns, b)
	}
	return burns, rows.Err()
}

// scanEntity scans a single entity row.
func scanEntity(row *sql.Row) (*profile.Entity, error) {
	var e profile.Entity
	var createdAt, updatedAt string
	err := row.Scan(&e.ID, (*string)(&e.Type), &e.Name, &e.Ecosystem, &e.URL, &createdAt, &updatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var parseErr error
	e.CreatedAt, parseErr = time.Parse(time.RFC3339, createdAt)
	if parseErr != nil {
		return nil, fmt.Errorf("parse entity created_at %q: %w", createdAt, parseErr)
	}
	e.UpdatedAt, parseErr = time.Parse(time.RFC3339, updatedAt)
	if parseErr != nil {
		return nil, fmt.Errorf("parse entity updated_at %q: %w", updatedAt, parseErr)
	}
	return &e, nil
}

// scanSignals scans multiple signal rows.
func scanSignals(rows *sql.Rows) ([]profile.Signal, error) {
	var signals []profile.Signal
	for rows.Next() {
		var sig profile.Signal
		var group, forgery, value, collectedAt, expiresAt string
		if err := rows.Scan(&sig.ID, &sig.EntityID, &sig.Type, &group, &sig.Source,
			&forgery, &value, &collectedAt, &expiresAt); err != nil {
			return nil, err
		}
		sig.Group = profile.SignalGroup(group)
		sig.ForgeryResistance = profile.ForgeryResistance(forgery)
		sig.Value = json.RawMessage(value)
		var parseErr error
		sig.CollectedAt, parseErr = time.Parse(time.RFC3339, collectedAt)
		if parseErr != nil {
			return nil, fmt.Errorf("parse signal collected_at %q: %w", collectedAt, parseErr)
		}
		sig.ExpiresAt, parseErr = time.Parse(time.RFC3339, expiresAt)
		if parseErr != nil {
			return nil, fmt.Errorf("parse signal expires_at %q: %w", expiresAt, parseErr)
		}
		signals = append(signals, sig)
	}
	return signals, rows.Err()
}

// Compile-time interface check.
var _ Store = (*SQLite)(nil)
