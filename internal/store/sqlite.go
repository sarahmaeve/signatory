// Package store provides the persistence interface for signatory's
// entity, signal, posture, burn, dependency, audit, and team identity
// data. The primary implementation uses SQLite via modernc.org/sqlite.
//
// The SQLite implementation here follows the schema defined in
// design/entity-model-v2.md. Key invariants:
//
//   - Signals, dependency observations, audit entries, and signal
//     resolutions are APPEND-ONLY. Their Append* methods never update
//     or delete existing rows. ID collisions fail the insert; they are
//     not silently overwritten.
//   - Postures are versioned: the primary key is (entity_id, version),
//     so posture decisions for different versions of the same entity
//     coexist. SetPosture upserts within a given (entity_id, version).
//   - Entities use UUID IDs as the internal primary key and a
//     canonical_uri as the external identifier. FindEntityByURI is the
//     lookup path when the caller only knows the URI (e.g., from user
//     input on the CLI).
//   - Team identities have one-way lifecycle fields (halted_at,
//     revoked_at) — once set they should not be cleared by a subsequent
//     PutTeamIdentity. That rule lives at the caller; the store accepts
//     what it's given.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // SQL driver registration.

	"github.com/sarahmaeve/signatory/internal/profile"
)

// SQLite implements the Store interface using a local SQLite database.
type SQLite struct {
	db *sql.DB
}

// chmodFunc is a package-level injection point for the os.Chmod call
// in OpenSQLite. Production code uses os.Chmod directly; tests can
// replace this var to observe the database file's permissions
// immediately before the chmod runs. This is the test seam that makes
// the issue #83 TOCTOU window observable from a black-box test —
// without it, the chmod always runs synchronously before OpenSQLite
// returns, making the bug invisible at the post-OpenSQLite invariant
// level.
var chmodFunc = os.Chmod

// OpenSQLite opens or creates a SQLite database at the given path.
// It creates the parent directory if it does not exist, restricts the
// database file to 0600 permissions, enables WAL mode and foreign keys,
// and runs schema migrations.
//
// ctx is threaded through every SQL operation (Ping, PRAGMA setup,
// migrations). Cancellation during Open aborts the in-progress SQL
// call cleanly; the partially-initialized *sql.DB is then closed.
// For interactive CLI use, context.Background() is fine. For the MCP
// server, pass the server's lifecycle context so shutdown interrupts
// a slow migration rather than blocking until it finishes.
//
// File-permission safety (#83): the database file is pre-created via
// os.OpenFile with mode 0600 BEFORE being handed to sql.Open, so there
// is no window during which the file is world-readable. The previous
// implementation relied on sql.Open → db.Ping creating the file with
// the process's default umask (typically 0644), then narrowing it via
// os.Chmod afterward — leaving a TOCTOU window during which a co-user
// on a multi-user system could read the database. Pre-creation closes
// that window because mode 0600 is preserved through default umask
// (0600 & ~0022 = 0600). The chmodFunc call below remains as defense
// in depth for the case where the file already existed with looser
// perms when OpenSQLite was first called.
func OpenSQLite(ctx context.Context, path string) (*SQLite, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}

	// Atomically create the database file at 0600 BEFORE sql.Open.
	// O_CREATE without O_EXCL: a no-op if the file already exists,
	// in which case the chmodFunc call below narrows any pre-existing
	// looser permissions. The mode 0600 is preserved through default
	// umask 0022 because the bits don't intersect.
	//
	// G304: path is OpenSQLite's explicit argument — the caller names
	// the DB file to open. The function's purpose IS to open that
	// file; traversal defense belongs at the caller boundary, where
	// kong's type:"path" validation and store.ResolvePath normalize it.
	if f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600); err != nil { //nolint:gosec // G304: caller-supplied DB path; function's purpose is to open it
		return nil, fmt.Errorf("create database file: %w", err)
	} else if err := f.Close(); err != nil {
		return nil, fmt.Errorf("close pre-created database file: %w", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close() // already in error path; close failure would mask the real error
		return nil, fmt.Errorf("ping database: %w", err)
	}

	// Defense in depth: narrow permissions if the file already existed
	// with looser perms when OpenSQLite was called. New files are
	// already at 0600 from the pre-create above; this is a no-op for
	// the common case.
	if err := chmodFunc(path, 0600); err != nil {
		_ = db.Close() // already in error path; close failure would mask the real error
		return nil, fmt.Errorf("set database permissions: %w", err)
	}

	// Limit to one connection. SQLite allows only one writer at a time;
	// database/sql's connection pool would open multiple connections whose
	// per-connection PRAGMA settings (busy_timeout, journal_mode) are not
	// shared. A single connection serializes access and ensures pragmas
	// apply consistently. This is the standard recommendation for SQLite
	// with Go's database/sql and is appropriate for a single-user CLI tool.
	db.SetMaxOpenConns(1)

	// Verify WAL mode is enabled.
	var journalMode string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&journalMode); err != nil {
		_ = db.Close() // already in error path; close failure would mask the real error
		return nil, fmt.Errorf("set journal_mode: %w", err)
	}
	if journalMode != "wal" {
		_ = db.Close() // already in error path; close failure would mask the real error
		return nil, fmt.Errorf("WAL mode not supported (got %q); signatory requires WAL for safe concurrent access", journalMode)
	}

	// Set additional pragmas.
	for _, pragma := range []string{
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			_ = db.Close() // already in error path; close failure would mask the real error
			return nil, fmt.Errorf("set %s: %w", pragma, err)
		}
	}

	if err := migrate(ctx, db, path); err != nil {
		_ = db.Close() // already in error path; close failure would mask the real error
		return nil, fmt.Errorf("migrate database: %w", err)
	}

	return &SQLite{db: db}, nil
}

// DB returns the underlying *sql.DB for use in tests that need to
// insert corrupted data or verify raw database state. Not for
// production use.
func (s *SQLite) DB() *sql.DB {
	return s.db
}

// Close closes the database connection.
func (s *SQLite) Close() error {
	return s.db.Close()
}

// --- Entity operations ---

// entityColumns is the canonical column list for SELECTs — kept as a
// constant so GetEntity/FindEntityByURI/scanEntity stay in sync.
const entityColumns = `id, canonical_uri, type, short_name, description, ecosystem, url, created_at, updated_at`

// GetEntity retrieves an entity by its internal UUID.
func (s *SQLite) GetEntity(ctx context.Context, id string) (*profile.Entity, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+entityColumns+` FROM entities WHERE id = ?`, id)
	return scanEntity(row)
}

// FindEntityByURI retrieves an entity by its canonical URI (purl or
// signatory URI scheme). This is the primary lookup path when the
// caller only has user-supplied input like a GitHub URL or purl string.
func (s *SQLite) FindEntityByURI(ctx context.Context, canonicalURI string) (*profile.Entity, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+entityColumns+` FROM entities WHERE canonical_uri = ?`, canonicalURI)
	return scanEntity(row)
}

// GetOutputEntity returns the entity that an analyst_output was
// indexed under — the FK-walking shortcut `posture accept` uses so
// it doesn't have to re-derive the target URI and risk a string-vs-
// stored-URI drift.
//
// Written as a single JOIN so callers don't pay two round trips for
// what is always a 1:1 relationship, and so the "row missing" /
// "entity missing" cases collapse to the same ErrNotFound — either
// way, there's no entity to act on. Returns ErrNotFound when
// outputID doesn't exist; any other error is an integrity signal
// (e.g. FK pointing at a deleted entity).
func (s *SQLite) GetOutputEntity(ctx context.Context, outputID string) (*profile.Entity, error) {
	if outputID == "" {
		return nil, fmt.Errorf("%w: outputID required", ErrNilInput)
	}
	// Prefix each selected column with `e.` so the JOIN is unambiguous
	// and scanEntity keeps reading the same columns in the same order.
	row := s.db.QueryRowContext(ctx,
		`SELECT e.id, e.canonical_uri, e.type, e.short_name,
		        e.description, e.ecosystem, e.url, e.created_at, e.updated_at
		   FROM analyst_outputs ao
		   INNER JOIN entities e ON e.id = ao.entity_id
		  WHERE ao.id = ?`, outputID)
	return scanEntity(row)
}

// PutEntity inserts or updates an entity. On INSERT conflict on id, the
// canonical_uri, type, short_name, description, ecosystem, url, and
// updated_at fields are updated. created_at and id are immutable after
// first insert.
//
// Validation:
//
//   - id, canonical_uri, short_name, and type must be non-empty.
//     This is a Go-layer guard — SQLite's NOT NULL constraint would
//     catch id/short_name/type, and the unique index on canonical_uri
//     would catch duplicate empties eventually, but an explicit Go
//     error is clearer.
//   - canonical_uri must pass profile.ValidateCanonicalURI. This is
//     the persistence-boundary defense for issue #78: even if a CLI
//     command, library caller, or future code path forgets to validate
//     input, the store rejects bad data (control chars, non-ASCII
//     lookalikes, unknown schemes, excessive length).
func (s *SQLite) PutEntity(ctx context.Context, entity *profile.Entity) error {
	if entity == nil {
		return ErrNilInput
	}
	if entity.ID == "" || entity.CanonicalURI == "" || entity.ShortName == "" || entity.Type == "" {
		return fmt.Errorf("%w: entity ID, canonical URI, short name, and type are required", ErrNilInput)
	}
	if err := profile.ValidateCanonicalURI(entity.CanonicalURI); err != nil {
		return fmt.Errorf("invalid canonical URI: %w", err)
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO entities (id, canonical_uri, type, short_name, description, ecosystem, url, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
			canonical_uri = excluded.canonical_uri,
			type          = excluded.type,
			short_name    = excluded.short_name,
			description   = excluded.description,
			ecosystem     = excluded.ecosystem,
			url           = excluded.url,
			updated_at    = excluded.updated_at`,
		entity.ID, entity.CanonicalURI, string(entity.Type), entity.ShortName,
		entity.Description, entity.Ecosystem, entity.URL,
		entity.CreatedAt.Format(time.RFC3339), entity.UpdatedAt.Format(time.RFC3339))
	return err
}

// --- Signal operations (append-only) ---

// signalColumns is the canonical signal SELECT list.
const signalColumns = `id, entity_id, type, signal_group, source, forgery_resistance, value, collected_at, expires_at`

// GetSignals returns ALL signals for an entity in chronological order,
// including signals that have been superseded by a resolution. Use
// GetLatestSignals for the "current state" view.
func (s *SQLite) GetSignals(ctx context.Context, entityID string) ([]profile.Signal, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+signalColumns+`
		 FROM signals WHERE entity_id = ?
		 ORDER BY collected_at ASC`, entityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // close on read-only rows; any real error surfaced during Scan
	return scanSignals(rows)
}

// GetLatestSignals returns the current-state view: the most recent
// non-superseded signal per (type, source) for an entity.
//
// "Current state" means:
//   - Latest collected_at wins for a given (type, source) pair
//   - Signals referenced as superseded_signal_id in any signal_resolutions
//     row belonging to THIS entity are excluded (they've been explicitly
//     overridden). The subquery is entity-scoped (#91): a resolution for
//     entity A must never hide signals belonging to entity B even if the
//     IDs happen to collide. Today's collectors generate IDs that include
//     the entity ID as a substring so the bug is unreachable from
//     production code paths, but the query's correctness should not depend
//     on the collector's ID-generation scheme.
//   - Different sources coexist — if github and peer:acme both report a
//     signal of the same type, both are returned with their source tag,
//     per design/entity-model-v2.md §Signal Conflict Resolution
//
// This is the query that powers `signatory analyze` and the display
// layer. Historical views should use GetSignals instead.
func (s *SQLite) GetLatestSignals(ctx context.Context, entityID string) ([]profile.Signal, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+signalColumns+`
		 FROM (
		   SELECT `+signalColumns+`,
		          ROW_NUMBER() OVER (
		            PARTITION BY type, source
		            ORDER BY collected_at DESC
		          ) AS rn
		   FROM signals
		   WHERE entity_id = ?
		     AND id NOT IN (
		       SELECT superseded_signal_id FROM signal_resolutions
		       WHERE entity_id = ?
		     )
		 )
		 WHERE rn = 1
		 ORDER BY signal_group, type, source`, entityID, entityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // close on read-only rows; any real error surfaced during Scan
	return scanSignals(rows)
}

// GetSignalsByGroup retrieves all signals for an entity filtered by
// signal group (e.g., SignalGroupVitality). Returns the full history
// for the group, not just the latest — use a filter over GetLatestSignals
// if you want the current-state view.
func (s *SQLite) GetSignalsByGroup(ctx context.Context, entityID string, group profile.SignalGroup) ([]profile.Signal, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+signalColumns+`
		 FROM signals WHERE entity_id = ? AND signal_group = ?
		 ORDER BY collected_at ASC`, entityID, string(group))
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // close on read-only rows; any real error surfaced during Scan
	return scanSignals(rows)
}

// AppendSignals inserts signals without upsert. ID collisions fail the
// insert — append-only means append-only. Callers must generate unique
// signal IDs (the design uses `{source}:{entity_id}:{type}:{collected_at}`
// which makes accidental collisions essentially impossible).
//
// The batch is committed as a single transaction: either all signals
// land or none do.
func (s *SQLite) AppendSignals(ctx context.Context, signals []profile.Signal) error {
	if len(signals) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rollback error is meaningless after commit

	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO signals (id, entity_id, type, signal_group, source, forgery_resistance, value, collected_at, expires_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close() //nolint:errcheck // close on prepared statement; any real error surfaced during Exec above

	for _, sig := range signals {
		if !json.Valid(sig.Value) {
			return fmt.Errorf("append signal %s: invalid JSON value", sig.ID)
		}
		if _, err := stmt.ExecContext(ctx,
			sig.ID, sig.EntityID, sig.Type, string(sig.Group),
			sig.Source, string(sig.ForgeryResistance), string(sig.Value),
			sig.CollectedAt.Format(time.RFC3339), sig.ExpiresAt.Format(time.RFC3339)); err != nil {
			return fmt.Errorf("append signal %s: %w", sig.ID, err)
		}
	}
	return tx.Commit()
}

// --- Posture operations (versioned) ---

// postureColumns is the canonical posture SELECT list.
const postureColumns = `entity_id, version, tier, rationale, set_by, set_at`

// GetPosture retrieves the posture for an entity at a specific version.
// Use empty string for the "unversioned" posture (set without --version).
// Returns ErrNotFound if no posture exists for the exact (entity_id,
// version) pair OR if the matching row has been withdrawn (soft-delete
// via the M4 undo verbs). For "latest across all versions" semantics,
// call GetPostures and pick the first result.
func (s *SQLite) GetPosture(ctx context.Context, entityID string, version string) (*profile.Posture, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+postureColumns+` FROM postures
		 WHERE entity_id = ? AND version = ? AND withdrawn_at = ''`, entityID, version)
	return scanPosture(row)
}

// GetPostures returns all active (non-withdrawn) postures for an
// entity, ordered newest-first by set_at. The CLI uses this to
// implement "latest + hint about other versions" display.
func (s *SQLite) GetPostures(ctx context.Context, entityID string) ([]profile.Posture, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+postureColumns+` FROM postures
		 WHERE entity_id = ? AND withdrawn_at = ''
		 ORDER BY set_at DESC`, entityID)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // close on read-only rows; any real error surfaced during Scan

	var postures []profile.Posture
	for rows.Next() {
		p, err := scanPostureRow(rows)
		if err != nil {
			return nil, err
		}
		postures = append(postures, *p)
	}
	return postures, rows.Err()
}

// SetPosture inserts or updates a posture for a given (entity_id,
// version) pair. Re-calling with the same pair replaces the earlier
// posture's tier, rationale, set_by, and set_at — this is intentional:
// revising a vetted decision with a new rationale is a normal edit,
// not a conflict. The per-version PK means different versions coexist.
//
// If the existing row is withdrawn, the upsert clears withdrawal
// state as a side effect — setting a new posture on a previously-
// unset entity "reactivates" it with fresh metadata. The audit_log
// preserves both the original set, the unset, and the re-set.
func (s *SQLite) SetPosture(ctx context.Context, posture *profile.Posture) error {
	if posture == nil {
		return ErrNilInput
	}
	if posture.EntityID == "" || posture.Tier == "" {
		return fmt.Errorf("%w: posture entity ID and tier are required", ErrNilInput)
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO postures (entity_id, version, tier, rationale, set_by, set_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(entity_id, version) DO UPDATE SET
			tier               = excluded.tier,
			rationale          = excluded.rationale,
			set_by             = excluded.set_by,
			set_at             = excluded.set_at,
			withdrawn_at       = '',
			withdrawn_by       = '',
			withdrawal_reason  = ''`,
		posture.EntityID, posture.Version, string(posture.Tier), posture.Rationale,
		posture.SetBy, posture.SetAt.Format(time.RFC3339))
	return err
}

// WithdrawPosture marks a posture row as withdrawn (soft delete). The
// row stays in the table with the withdrawal metadata filled in;
// reads default-filter it out. Returns ErrNotFound if no posture
// exists for the (entity_id, version) pair, or if the row is already
// withdrawn (idempotent "posture is already inactive" semantic is
// the caller's choice — we report truthfully and let the caller
// decide).
//
// reason is optional context ("author left the org", "reassessment
// pending") the caller may supply; empty string is fine.
func (s *SQLite) WithdrawPosture(ctx context.Context, entityID, version, withdrawnBy, reason string, at time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE postures
		 SET withdrawn_at = ?, withdrawn_by = ?, withdrawal_reason = ?
		 WHERE entity_id = ? AND version = ? AND withdrawn_at = ''`,
		at.Format(time.RFC3339), withdrawnBy, reason, entityID, version)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// --- Burn operations ---

// GetBurn retrieves the active burn for an entity, or ErrNotFound if
// none exists OR the burn has been withdrawn (soft-delete via the M4
// undo verb burn remove).
func (s *SQLite) GetBurn(ctx context.Context, entityID string) (*profile.Burn, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT entity_id, reason, source, source_org, burned_at, burned_by
		 FROM burns WHERE entity_id = ? AND withdrawn_at = ''`, entityID)

	var b profile.Burn
	var burnedAt string
	err := row.Scan(&b.EntityID, &b.Reason, (*string)(&b.Source), &b.SourceOrg, &burnedAt, &b.BurnedBy)
	if errors.Is(err, sql.ErrNoRows) {
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

// SetBurn inserts or updates a burn for an entity. Burns are a one-per-
// entity decision (unlike postures, they are not versioned) because a
// compromised maintainer compromises the entity's identity, not a
// specific version.
func (s *SQLite) SetBurn(ctx context.Context, burn *profile.Burn) error {
	if burn == nil {
		return ErrNilInput
	}
	if burn.EntityID == "" || burn.Reason == "" {
		return fmt.Errorf("%w: burn entity ID and reason are required", ErrNilInput)
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO burns (entity_id, reason, source, source_org, burned_at, burned_by)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(entity_id) DO UPDATE SET
			reason             = excluded.reason,
			source             = excluded.source,
			source_org         = excluded.source_org,
			burned_at          = excluded.burned_at,
			burned_by          = excluded.burned_by,
			withdrawn_at       = '',
			withdrawn_by       = '',
			withdrawal_reason  = ''`,
		burn.EntityID, burn.Reason, string(burn.Source), burn.SourceOrg,
		burn.BurnedAt.Format(time.RFC3339), burn.BurnedBy)
	return err
}

// WithdrawBurn marks a burn row as withdrawn (soft delete). Returns
// ErrNotFound if no active burn exists for the entity — includes the
// already-withdrawn case; the caller decides whether that's an error
// in their context.
func (s *SQLite) WithdrawBurn(ctx context.Context, entityID, withdrawnBy, reason string, at time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE burns
		 SET withdrawn_at = ?, withdrawn_by = ?, withdrawal_reason = ?
		 WHERE entity_id = ? AND withdrawn_at = ''`,
		at.Format(time.RFC3339), withdrawnBy, reason, entityID)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// ListBurns returns all currently-active (non-withdrawn) burns.
func (s *SQLite) ListBurns(ctx context.Context) ([]profile.Burn, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT entity_id, reason, source, source_org, burned_at, burned_by
		 FROM burns WHERE withdrawn_at = ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // close on read-only rows; any real error surfaced during Scan

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

// --- Dependency observations (append-only) ---

// AppendDependencyObservations records a batch of dependency observations
// from a single survey. All observations in a batch share a survey_id
// (enforced by the caller, not the store). Pure insert — no upsert.
func (s *SQLite) AppendDependencyObservations(ctx context.Context, obs []profile.DependencyObservation) error {
	if len(obs) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO dependency_observations (id, project_id, entity_id, version, direct, observed_at, survey_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close() //nolint:errcheck // close on prepared statement; any real error surfaced during Exec above

	for _, o := range obs {
		direct := 0
		if o.Direct {
			direct = 1
		}
		if _, err := stmt.ExecContext(ctx,
			o.ID, o.ProjectID, o.EntityID, o.Version, direct,
			o.ObservedAt.Format(time.RFC3339), o.SurveyID); err != nil {
			return fmt.Errorf("append observation %s: %w", o.ID, err)
		}
	}
	return tx.Commit()
}

// GetLatestDependencies returns the observations from the most recent
// survey for a project — i.e. the current dependency tree as of that
// survey. Returns nil (not ErrNotFound) if no surveys have been recorded.
func (s *SQLite) GetLatestDependencies(ctx context.Context, projectID string) ([]profile.DependencyObservation, error) {
	// Resolve the latest survey_id for this project.
	var surveyID string
	err := s.db.QueryRowContext(ctx,
		`SELECT survey_id FROM dependency_observations
		 WHERE project_id = ?
		 ORDER BY observed_at DESC
		 LIMIT 1`, projectID).Scan(&surveyID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, project_id, entity_id, version, direct, observed_at, survey_id
		 FROM dependency_observations
		 WHERE project_id = ? AND survey_id = ?`, projectID, surveyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // close on read-only rows; any real error surfaced during Scan

	var observations []profile.DependencyObservation
	for rows.Next() {
		var o profile.DependencyObservation
		var direct int
		var observedAt string
		if err := rows.Scan(&o.ID, &o.ProjectID, &o.EntityID, &o.Version, &direct, &observedAt, &o.SurveyID); err != nil {
			return nil, err
		}
		o.Direct = direct != 0
		parsed, parseErr := time.Parse(time.RFC3339, observedAt)
		if parseErr != nil {
			return nil, fmt.Errorf("parse observation observed_at %q: %w", observedAt, parseErr)
		}
		o.ObservedAt = parsed
		observations = append(observations, o)
	}
	return observations, rows.Err()
}

// --- Audit log (append-only) ---

// AppendAuditEntry writes a single audit log entry. EntityID is
// optional — non-entity actions (e.g., a global survey) leave it empty
// and it's stored as SQL NULL.
func (s *SQLite) AppendAuditEntry(ctx context.Context, entry *profile.AuditEntry) error {
	if entry == nil {
		return ErrNilInput
	}
	if entry.ID == "" || entry.Actor == "" || entry.Action == "" {
		return fmt.Errorf("%w: audit entry ID, actor, and action are required", ErrNilInput)
	}
	var entityID sql.NullString
	if entry.EntityID != "" {
		entityID = sql.NullString{String: entry.EntityID, Valid: true}
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit_log (id, timestamp, actor, action, entity_id, detail)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		entry.ID, entry.Timestamp.Format(time.RFC3339), entry.Actor, entry.Action, entityID, entry.Detail)
	return err
}

// --- Signal resolutions (append-only) ---

// AppendResolution writes a conflict resolution record. The referenced
// kept_signal_id and superseded_signal_id must exist in the signals
// table (enforced by FK). Once superseded, a signal is excluded from
// GetLatestSignals but remains in GetSignals for history.
func (s *SQLite) AppendResolution(ctx context.Context, r *profile.SignalResolution) error {
	if r == nil {
		return ErrNilInput
	}
	if r.ID == "" || r.EntityID == "" || r.KeptSignalID == "" || r.SupersededSignalID == "" || r.Action == "" {
		return fmt.Errorf("%w: resolution requires ID, entity_id, kept_signal_id, superseded_signal_id, and action", ErrNilInput)
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO signal_resolutions (id, entity_id, signal_type, kept_signal_id, superseded_signal_id, action, resolved_by, resolved_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.EntityID, r.SignalType, r.KeptSignalID, r.SupersededSignalID,
		r.Action, r.ResolvedBy, r.ResolvedAt.Format(time.RFC3339))
	return err
}

// --- Team identities ---

// GetTeamIdentity retrieves a team identity by ID, or ErrNotFound if
// none exists. Halted and revoked timestamps are nullable in the
// database; they're returned as nil *time.Time if not set.
func (s *SQLite) GetTeamIdentity(ctx context.Context, id string) (*profile.TeamIdentity, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, name, created_at, halted_at, revoked_at, revoke_reason
		 FROM team_identities WHERE id = ?`, id)

	var t profile.TeamIdentity
	var createdAt string
	var haltedAt, revokedAt, revokeReason sql.NullString
	err := row.Scan(&t.ID, &t.Name, &createdAt, &haltedAt, &revokedAt, &revokeReason)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	t.CreatedAt, err = time.Parse(time.RFC3339, createdAt)
	if err != nil {
		return nil, fmt.Errorf("parse team identity created_at %q: %w", createdAt, err)
	}
	if haltedAt.Valid {
		parsed, perr := time.Parse(time.RFC3339, haltedAt.String)
		if perr != nil {
			return nil, fmt.Errorf("parse team identity halted_at %q: %w", haltedAt.String, perr)
		}
		t.HaltedAt = &parsed
	}
	if revokedAt.Valid {
		parsed, perr := time.Parse(time.RFC3339, revokedAt.String)
		if perr != nil {
			return nil, fmt.Errorf("parse team identity revoked_at %q: %w", revokedAt.String, perr)
		}
		t.RevokedAt = &parsed
	}
	if revokeReason.Valid {
		t.RevokeReason = revokeReason.String
	}
	return &t, nil
}

// PutTeamIdentity inserts or updates a team identity. Halted and
// revoked timestamps + revoke reason are written whenever provided;
// the store does not enforce one-way lifecycle semantics (that is a
// caller/command-layer concern, since the correct response to a
// rollback is context-dependent).
func (s *SQLite) PutTeamIdentity(ctx context.Context, t *profile.TeamIdentity) error {
	if t == nil {
		return ErrNilInput
	}
	if t.ID == "" || t.Name == "" {
		return fmt.Errorf("%w: team identity ID and name are required", ErrNilInput)
	}

	var haltedAt, revokedAt, revokeReason sql.NullString
	if t.HaltedAt != nil {
		haltedAt = sql.NullString{String: t.HaltedAt.Format(time.RFC3339), Valid: true}
	}
	if t.RevokedAt != nil {
		revokedAt = sql.NullString{String: t.RevokedAt.Format(time.RFC3339), Valid: true}
	}
	if t.RevokeReason != "" {
		revokeReason = sql.NullString{String: t.RevokeReason, Valid: true}
	}

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO team_identities (id, name, created_at, halted_at, revoked_at, revoke_reason)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
			name          = excluded.name,
			halted_at     = excluded.halted_at,
			revoked_at    = excluded.revoked_at,
			revoke_reason = excluded.revoke_reason`,
		t.ID, t.Name, t.CreatedAt.Format(time.RFC3339), haltedAt, revokedAt, revokeReason)
	return err
}

// --- Row-scan helpers ---

// scanEntity scans a single entity row and parses timestamps. The
// column order must match entityColumns.
func scanEntity(row *sql.Row) (*profile.Entity, error) {
	var e profile.Entity
	var createdAt, updatedAt string
	err := row.Scan(&e.ID, &e.CanonicalURI, (*string)(&e.Type), &e.ShortName, &e.Description,
		&e.Ecosystem, &e.URL, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
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

// scanSignals scans multiple signal rows. Callers are responsible for
// deferring rows.Close().
func scanSignals(rows *sql.Rows) ([]profile.Signal, error) {
	signals := []profile.Signal{}
	for rows.Next() {
		var sig profile.Signal
		var group, forgery, value, collectedAt, expiresAt string
		if err := rows.Scan(&sig.ID, &sig.EntityID, &sig.Type, &group, &sig.Source,
			&forgery, &value, &collectedAt, &expiresAt); err != nil {
			return nil, err
		}
		sig.Group = profile.SignalGroup(group)
		sig.ForgeryResistance = profile.ForgeryResistance(forgery)
		if !json.Valid([]byte(value)) {
			return nil, fmt.Errorf("invalid JSON in signal %s value: %q", sig.ID, value[:min(len(value), 100)])
		}
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

// scanPosture scans a single posture row from a *sql.Row (single-result
// query). scanPostureRow is the *sql.Rows counterpart for iteration.
func scanPosture(row *sql.Row) (*profile.Posture, error) {
	var p profile.Posture
	var setAt string
	err := row.Scan(&p.EntityID, &p.Version, (*string)(&p.Tier), &p.Rationale, &p.SetBy, &setAt)
	if errors.Is(err, sql.ErrNoRows) {
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

// scanPostureRow scans a single posture row from a *sql.Rows iterator.
func scanPostureRow(rows *sql.Rows) (*profile.Posture, error) {
	var p profile.Posture
	var setAt string
	if err := rows.Scan(&p.EntityID, &p.Version, (*string)(&p.Tier), &p.Rationale, &p.SetBy, &setAt); err != nil {
		return nil, err
	}
	parsed, parseErr := time.Parse(time.RFC3339, setAt)
	if parseErr != nil {
		return nil, fmt.Errorf("parse posture set_at %q for %s: %w", setAt, p.EntityID, parseErr)
	}
	p.SetAt = parsed
	return &p, nil
}

// Compile-time interface check.
var _ Store = (*SQLite)(nil)
