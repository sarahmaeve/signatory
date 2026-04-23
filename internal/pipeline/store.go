package pipeline

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrSessionNotFound is returned by DepositMessage when the session
// referenced by Message.SessionID does not exist. Callers use
// errors.Is to branch: the server maps this to a 400 with a useful
// "session %q not found" message, rather than letting the raw FK
// constraint failure surface as an opaque "internal error."
//
// The error exists at the store layer (rather than being string-
// matched at the server layer) so the same check composes into
// future consumers — a test harness asserting "rejected the insert,"
// a new endpoint with its own error-mapping — without each caller
// re-sniffing SQLite driver error text.
var ErrSessionNotFound = errors.New("pipeline session not found")

// Store provides persistence for pipeline sessions and messages.
// It operates on the same SQLite database as the main signatory
// store, using its own namespaced tables.
type Store struct {
	db *sql.DB
}

// OpenStore wraps an existing *sql.DB connection and ensures the
// pipeline tables exist. The caller owns the DB lifecycle — Store
// does not close it.
//
// The caller is responsible for configuring the DB connection for
// SQLite concurrency safety (SetMaxOpenConns(1), WAL mode, busy
// timeout, foreign keys). When used via `signatory serve`, the
// main store's OpenSQLite handles this. For standalone use or
// testing, call ConfigureDB first.
func OpenStore(ctx context.Context, db *sql.DB) (*Store, error) {
	if err := migrate(ctx, db); err != nil {
		return nil, fmt.Errorf("pipeline migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// ConfigureDB sets the SQLite pragmas required for safe concurrent
// use: single connection, WAL journal mode, busy timeout, and
// foreign key enforcement. Call this when the *sql.DB was not
// opened via the main store's OpenSQLite (which sets these itself).
func ConfigureDB(ctx context.Context, db *sql.DB) error {
	db.SetMaxOpenConns(1)
	pragmas := []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
	}
	for _, p := range pragmas {
		if _, err := db.ExecContext(ctx, p); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
	}
	return nil
}

// CountActiveSessions returns the number of sessions with status 'active'.
func (s *Store) CountActiveSessions(ctx context.Context) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pipeline_sessions WHERE status = 'active'`,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count active sessions: %w", err)
	}
	return count, nil
}

// CreateSession starts a new pipeline session for the given target.
func (s *Store) CreateSession(ctx context.Context, target, metadata string) (*Session, error) {
	sess := &Session{
		ID:        uuid.New().String(),
		Target:    target,
		Status:    "active",
		CreatedAt: time.Now().UTC(),
		Metadata:  metadata,
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO pipeline_sessions (id, target, status, created_at, metadata)
		 VALUES (?, ?, ?, ?, ?)`,
		sess.ID, sess.Target, sess.Status,
		sess.CreatedAt.Format(time.RFC3339), sess.Metadata,
	)
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}
	return sess, nil
}

// GetSession retrieves a session by ID. Returns sql.ErrNoRows
// (wrapped) if the session does not exist.
func (s *Store) GetSession(ctx context.Context, id string) (*Session, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, target, status, created_at, metadata
		 FROM pipeline_sessions WHERE id = ?`, id,
	)
	return scanSession(row)
}

// UpdateSessionStatus sets the status of a session.
func (s *Store) UpdateSessionStatus(ctx context.Context, id, status string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE pipeline_sessions SET status = ? WHERE id = ?`,
		status, id,
	)
	if err != nil {
		return fmt.Errorf("update session status: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("session %q not found", id)
	}
	return nil
}

// DepositMessage stores a message in a session. Returns
// ErrSessionNotFound (wrapped) when the SessionID does not name an
// existing pipeline_sessions row — the FK constraint in the schema
// catches the miss at INSERT time, and this method translates the
// driver-level error into a domain sentinel so callers don't have
// to sniff for it themselves.
//
// The INSERT-first-then-translate shape (vs. pre-SELECT) is
// deliberate: it collapses two round-trips to one on the happy
// path, and an INSERT racing against a DELETE of the target session
// still produces ErrSessionNotFound (the pre-SELECT shape would
// race through the gap and surface the constraint error unclassified).
func (s *Store) DepositMessage(ctx context.Context, msg *Message) (*Message, error) {
	msg.CreatedAt = time.Now().UTC()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO pipeline_messages (session_id, role, msg_type, content, created_at, metadata)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		msg.SessionID, msg.Role, msg.MsgType, msg.Content,
		msg.CreatedAt.Format(time.RFC3339), msg.Metadata,
	)
	if err != nil {
		if isForeignKeyFailure(err) {
			return nil, fmt.Errorf("%w: %q", ErrSessionNotFound, msg.SessionID)
		}
		return nil, fmt.Errorf("deposit message: %w", err)
	}
	id, _ := res.LastInsertId()
	msg.ID = id
	return msg, nil
}

// isForeignKeyFailure reports whether err is SQLite's
// "FOREIGN KEY constraint failed" error. String-matching the driver
// error text is fragile in the abstract — but this particular
// message is stable across SQLite versions and every Go SQLite
// driver (modernc.org/sqlite, mattn/go-sqlite3) carries it
// verbatim, because it comes from the SQLite library's own
// sqlite3ErrStr() table where it's been unchanged since the FK
// enforcement feature landed.
//
// Alternative approaches considered:
//   - Driver-specific error types (modernc.org/sqlite.Error with
//     code 787): requires importing the driver into the store
//     package, which otherwise uses only database/sql. Couples the
//     store to a specific driver choice.
//   - A pre-check SELECT for session existence: adds a round-trip on
//     every deposit and still races against concurrent delete.
//
// String-matching a contained, stable driver message is the
// pragmatic middle path.
func isForeignKeyFailure(err error) bool {
	return err != nil && strings.Contains(err.Error(), "FOREIGN KEY constraint failed")
}

// GetMessages retrieves messages matching the filter.
func (s *Store) GetMessages(ctx context.Context, f MessageFilter) ([]Message, error) {
	query := `SELECT id, session_id, role, msg_type, content, created_at, metadata
	          FROM pipeline_messages WHERE session_id = ?`
	args := []any{f.SessionID}

	if f.Role != "" {
		query += ` AND role = ?`
		args = append(args, f.Role)
	}
	if f.MsgType != "" {
		query += ` AND msg_type = ?`
		args = append(args, f.MsgType)
	}
	query += ` ORDER BY created_at ASC, id ASC`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("get messages: %w", err)
	}
	defer rows.Close() //nolint:errcheck // close on read-only rows; any real error surfaced during Scan

	var msgs []Message
	for rows.Next() {
		msg, err := scanMessageRow(rows)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, msg)
	}
	return msgs, rows.Err()
}

// GetLatestMessage retrieves the most recent message matching the
// filter. Returns sql.ErrNoRows (wrapped) if no message matches.
func (s *Store) GetLatestMessage(ctx context.Context, f MessageFilter) (*Message, error) {
	query := `SELECT id, session_id, role, msg_type, content, created_at, metadata
	          FROM pipeline_messages WHERE session_id = ?`
	args := []any{f.SessionID}

	if f.Role != "" {
		query += ` AND role = ?`
		args = append(args, f.Role)
	}
	if f.MsgType != "" {
		query += ` AND msg_type = ?`
		args = append(args, f.MsgType)
	}
	query += ` ORDER BY created_at DESC, id DESC LIMIT 1`

	row := s.db.QueryRowContext(ctx, query, args...)
	var msg Message
	var createdStr string
	var metadata sql.NullString
	err := row.Scan(&msg.ID, &msg.SessionID, &msg.Role, &msg.MsgType,
		&msg.Content, &createdStr, &metadata)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("get latest message: %w", sql.ErrNoRows)
		}
		return nil, fmt.Errorf("get latest message: %w", err)
	}
	msg.CreatedAt, err = time.Parse(time.RFC3339, createdStr)
	if err != nil {
		return nil, fmt.Errorf("get latest message: parse created_at %q: %w", createdStr, err)
	}
	if metadata.Valid {
		msg.Metadata = metadata.String
	}
	return &msg, nil
}

// ListSessions returns all sessions, most recent first.
func (s *Store) ListSessions(ctx context.Context) ([]Session, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, target, status, created_at, metadata
		 FROM pipeline_sessions ORDER BY created_at DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close() //nolint:errcheck // close on read-only rows; any real error surfaced during Scan

	var sessions []Session
	for rows.Next() {
		var sess Session
		var createdStr string
		var metadata sql.NullString
		if err := rows.Scan(&sess.ID, &sess.Target, &sess.Status, &createdStr, &metadata); err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		var err error
		sess.CreatedAt, err = time.Parse(time.RFC3339, createdStr)
		if err != nil {
			return nil, fmt.Errorf("list sessions: parse created_at %q: %w", createdStr, err)
		}
		if metadata.Valid {
			sess.Metadata = metadata.String
		}
		sessions = append(sessions, sess)
	}
	return sessions, rows.Err()
}

// DeleteSession removes a session and all its messages.
func (s *Store) DeleteSession(ctx context.Context, id string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback after commit is a no-op

	if _, err := tx.ExecContext(ctx, `DELETE FROM pipeline_messages WHERE session_id = ?`, id); err != nil {
		return fmt.Errorf("delete messages: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM pipeline_sessions WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return tx.Commit()
}

// --- scan helpers ---

func scanSession(row *sql.Row) (*Session, error) {
	var sess Session
	var createdStr string
	var metadata sql.NullString
	if err := row.Scan(&sess.ID, &sess.Target, &sess.Status, &createdStr, &metadata); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("scan session: %w", sql.ErrNoRows)
		}
		return nil, fmt.Errorf("scan session: %w", err)
	}
	var err error
	sess.CreatedAt, err = time.Parse(time.RFC3339, createdStr)
	if err != nil {
		return nil, fmt.Errorf("scan session: parse created_at %q: %w", createdStr, err)
	}
	if metadata.Valid {
		sess.Metadata = metadata.String
	}
	return &sess, nil
}

type scannable interface {
	Scan(dest ...any) error
}

func scanMessageRow(row scannable) (Message, error) {
	var msg Message
	var createdStr string
	var metadata sql.NullString
	if err := row.Scan(&msg.ID, &msg.SessionID, &msg.Role, &msg.MsgType,
		&msg.Content, &createdStr, &metadata); err != nil {
		return msg, fmt.Errorf("scan message: %w", err)
	}
	var err error
	msg.CreatedAt, err = time.Parse(time.RFC3339, createdStr)
	if err != nil {
		return msg, fmt.Errorf("scan message: parse created_at %q: %w", createdStr, err)
	}
	if metadata.Valid {
		msg.Metadata = metadata.String
	}
	return msg, nil
}
