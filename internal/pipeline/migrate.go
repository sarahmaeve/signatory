package pipeline

import (
	"context"
	"database/sql"
	"fmt"
)

// migrate ensures the pipeline tables exist. It uses IF NOT EXISTS
// so it's safe to call on every OpenStore — no version tracking
// needed for these ephemeral tables.
func migrate(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, pipelineSchema)
	if err != nil {
		return fmt.Errorf("create pipeline tables: %w", err)
	}
	return nil
}

const pipelineSchema = `
CREATE TABLE IF NOT EXISTS pipeline_sessions (
    id         TEXT PRIMARY KEY,
    target     TEXT NOT NULL,
    status     TEXT NOT NULL DEFAULT 'active',
    created_at TEXT NOT NULL,
    metadata   TEXT
);

CREATE TABLE IF NOT EXISTS pipeline_messages (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id TEXT NOT NULL REFERENCES pipeline_sessions(id),
    role       TEXT NOT NULL,
    msg_type   TEXT NOT NULL,
    content    TEXT NOT NULL,
    created_at TEXT NOT NULL,
    metadata   TEXT
);

CREATE INDEX IF NOT EXISTS idx_pipeline_messages_session_role_type
    ON pipeline_messages(session_id, role, msg_type);
`
