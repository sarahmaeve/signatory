package pipeline

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Store provides persistence for pipeline sessions and messages.
// It operates on the same SQLite database as the main signatory
// store, using its own namespaced tables.
type Store struct {
	db *sql.DB
}

// OpenStore wraps an existing *sql.DB connection and ensures the
// pipeline tables exist. The caller owns the DB lifecycle — Store
// does not close it.
func OpenStore(ctx context.Context, db *sql.DB) (*Store, error) {
	if err := migrate(ctx, db); err != nil {
		return nil, fmt.Errorf("pipeline migrate: %w", err)
	}
	return &Store{db: db}, nil
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

// GetSession retrieves a session by ID.
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

// DepositMessage stores a message in a session.
func (s *Store) DepositMessage(ctx context.Context, msg *Message) (*Message, error) {
	msg.CreatedAt = time.Now().UTC()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO pipeline_messages (session_id, role, msg_type, content, created_at, metadata)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		msg.SessionID, msg.Role, msg.MsgType, msg.Content,
		msg.CreatedAt.Format(time.RFC3339), msg.Metadata,
	)
	if err != nil {
		return nil, fmt.Errorf("deposit message: %w", err)
	}
	id, _ := res.LastInsertId()
	msg.ID = id
	return msg, nil
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
	defer rows.Close()

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

// GetLatestMessage retrieves the most recent message matching the filter.
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
		return nil, fmt.Errorf("get latest message: %w", err)
	}
	msg.CreatedAt, _ = time.Parse(time.RFC3339, createdStr)
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
	defer rows.Close()

	var sessions []Session
	for rows.Next() {
		var sess Session
		var createdStr string
		var metadata sql.NullString
		if err := rows.Scan(&sess.ID, &sess.Target, &sess.Status, &createdStr, &metadata); err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		sess.CreatedAt, _ = time.Parse(time.RFC3339, createdStr)
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
		return nil, fmt.Errorf("scan session: %w", err)
	}
	sess.CreatedAt, _ = time.Parse(time.RFC3339, createdStr)
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
	msg.CreatedAt, _ = time.Parse(time.RFC3339, createdStr)
	if metadata.Valid {
		msg.Metadata = metadata.String
	}
	return msg, nil
}
