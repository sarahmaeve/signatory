package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sarahmaeve/signatory/internal/profile"
)

// analystIDSeparator joins ExpectedAnalysts for storage in the
// single-column expected_analysts field. Chosen because analyst
// role IDs are kebab-case identifiers (e.g. "external-sec-v1")
// that do not contain commas — splitting on "," round-trips
// without ambiguity.
const analystIDSeparator = ","

// CreateAnalysisSession inserts a new session row. Caller supplies
// ID (UUID) and StartedAt; Status must be in_progress at creation
// time. Terminal states are only reachable via CloseAnalysisSession
// so the lifecycle machine stays one-way.
//
// No idempotency short-circuit — each begin creates a distinct row.
// Operators running `signatory analyze begin` twice for the same
// target get two sessions, which is correct (they may want a re-run
// with different parameters).
func (s *SQLite) CreateAnalysisSession(ctx context.Context, session *profile.AnalysisSession) error {
	if session == nil {
		return ErrNilInput
	}
	if session.ID == "" {
		return fmt.Errorf("%w: session ID required", ErrNilInput)
	}
	if session.EntityID == "" {
		return fmt.Errorf("%w: session EntityID required", ErrNilInput)
	}
	if session.Status != profile.AnalysisSessionInProgress {
		return fmt.Errorf("%w: new session must start in_progress (got %q)", ErrNilInput, session.Status)
	}
	if session.StartedAt.IsZero() {
		return fmt.Errorf("%w: session StartedAt required", ErrNilInput)
	}

	endedAt := ""
	if session.EndedAt != nil && !session.EndedAt.IsZero() {
		endedAt = session.EndedAt.UTC().Format(time.RFC3339)
	}
	synthesisOutputID := nullableString(session.SynthesisOutputID)

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO analysis_sessions
		 (id, entity_id, target_uri, target_version, invoked_by,
		  pipeline_session_id, expected_analysts, started_at,
		  ended_at, status, synthesis_output_id, notes)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		session.ID, session.EntityID, session.TargetURI,
		session.TargetVersion, session.InvokedBy,
		session.PipelineSessionID,
		strings.Join(session.ExpectedAnalysts, analystIDSeparator),
		session.StartedAt.UTC().Format(time.RFC3339),
		endedAt, string(session.Status), synthesisOutputID,
		session.Notes)
	if err != nil {
		return fmt.Errorf("insert analysis_session: %w", err)
	}
	return nil
}

// GetAnalysisSession loads one session by ID. Returns ErrNotFound
// if absent.
func (s *SQLite) GetAnalysisSession(ctx context.Context, id string) (*profile.AnalysisSession, error) {
	if id == "" {
		return nil, fmt.Errorf("%w: id required", ErrNilInput)
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT id, entity_id, target_uri, target_version, invoked_by,
		        pipeline_session_id, expected_analysts, started_at,
		        ended_at, status, synthesis_output_id, notes
		   FROM analysis_sessions WHERE id = ?`, id)
	return scanAnalysisSession(row)
}

// ListAnalysisSessions returns sessions matching the filter, sorted
// newest-first. Empty filter lists all sessions (capped by Limit if
// set).
func (s *SQLite) ListAnalysisSessions(ctx context.Context, filter AnalysisSessionFilter) ([]profile.AnalysisSession, error) {
	var clauses []string
	var args []any
	if filter.EntityID != "" {
		clauses = append(clauses, "entity_id = ?")
		args = append(args, filter.EntityID)
	}
	if filter.TargetVersion != "" {
		clauses = append(clauses, "target_version = ?")
		args = append(args, filter.TargetVersion)
	}
	if filter.Status != "" {
		clauses = append(clauses, "status = ?")
		args = append(args, string(filter.Status))
	}
	if !filter.Since.IsZero() {
		clauses = append(clauses, "started_at >= ?")
		args = append(args, filter.Since.UTC().Format(time.RFC3339))
	}
	whereSQL := ""
	if len(clauses) > 0 {
		whereSQL = "WHERE " + strings.Join(clauses, " AND ")
	}
	limitSQL := ""
	if filter.Limit > 0 {
		// Safe: filter.Limit is an int, %d emits digits only.
		// Matches the house idiom in analyst_output_query.go.
		limitSQL = fmt.Sprintf("LIMIT %d", filter.Limit)
	}
	query := fmt.Sprintf(`
		SELECT id, entity_id, target_uri, target_version, invoked_by,
		       pipeline_session_id, expected_analysts, started_at,
		       ended_at, status, synthesis_output_id, notes
		  FROM analysis_sessions
		  %s
		 ORDER BY started_at DESC
		  %s`, whereSQL, limitSQL)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list analysis_sessions: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only rows

	var out []profile.AnalysisSession
	for rows.Next() {
		sess, err := scanAnalysisSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *sess)
	}
	return out, rows.Err()
}

// CloseAnalysisSession transitions a session's lifecycle from
// in_progress to a terminal state. Rejects transitions out of an
// already-terminal session (closed sessions stay closed; a re-run
// begins a fresh session).
//
// params.SynthesisOutputID is only populated when the close was
// triggered by a synthesis ingest. For manual `signatory analyze
// end` transitions it stays empty.
//
// The parameter is named `params` rather than `close` because the
// bare word `close` is a Go builtin (channel close); naming it
// anything else avoids predeclared-identifier shadowing.
func (s *SQLite) CloseAnalysisSession(ctx context.Context, id string, params profile.AnalysisSessionCloseParams) error {
	if id == "" {
		return fmt.Errorf("%w: id required", ErrNilInput)
	}
	if !params.Status.IsTerminal() {
		return fmt.Errorf("%w: params.Status must be terminal (got %q)", ErrNilInput, params.Status)
	}
	if params.EndedAt.IsZero() {
		return fmt.Errorf("%w: params.EndedAt required", ErrNilInput)
	}

	// Read-then-write inside one tx so a concurrent writer can't
	// race us into double-closing. Unconditional deferred rollback
	// — rollback-after-commit is a no-op, but rollback-on-early-
	// return is the whole point. The previous closure form
	// (if err != nil { tx.Rollback() }) leaked the tx on any
	// return path that didn't update err first — that was the
	// deadlock source during Phase 3 rework.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin close tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback-after-commit is a no-op

	var currentStatus string
	err = tx.QueryRowContext(ctx,
		`SELECT status FROM analysis_sessions WHERE id = ?`, id).Scan(&currentStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("load current status: %w", err)
	}
	if profile.AnalysisSessionStatus(currentStatus).IsTerminal() {
		return fmt.Errorf("cannot close session %s: already %s (terminal); begin a new session for a re-run",
			id, currentStatus)
	}

	synthesisArg := nullableString(params.SynthesisOutputID)
	_, err = tx.ExecContext(ctx,
		`UPDATE analysis_sessions
		    SET status = ?, ended_at = ?, synthesis_output_id = ?
		  WHERE id = ?`,
		string(params.Status),
		params.EndedAt.UTC().Format(time.RFC3339),
		synthesisArg, id)
	if err != nil {
		return fmt.Errorf("update session status: %w", err)
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit close tx: %w", err)
	}
	return nil
}

// ListOutputsForSession returns summary rows for every
// analyst_output linked to the session, sorted by invoked_at. Reuses
// AnalystOutputSummary so the CLI `analyze show` can render against
// the same formatter show-analyses uses.
func (s *SQLite) ListOutputsForSession(ctx context.Context, sessionID string) ([]AnalystOutputSummary, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("%w: sessionID required", ErrNilInput)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			ao.id, ao.entity_id, e.canonical_uri,
			ao.collected_from_entity_id, COALESCE(cf.canonical_uri, ''),
			ao.analyst_id, ao.model, ao.prompt_version,
			ao.invoked_at, ao.ingested_at, ao.round,
			ao.target_commit, ao.source_path, ao.content_hash,
			(SELECT COUNT(*) FROM conclusions         WHERE output_id = ao.id) AS conclusions_count,
			(SELECT COUNT(*) FROM positive_absences  WHERE output_id = ao.id) AS absence_count,
			(SELECT COUNT(*) FROM observations       WHERE output_id = ao.id) AS obs_count,
			(SELECT COUNT(*) FROM methodology_patterns WHERE output_id = ao.id) AS pat_count
		FROM analyst_outputs ao
		INNER JOIN entities e ON e.id = ao.entity_id
		LEFT JOIN entities cf ON cf.id = ao.collected_from_entity_id
		WHERE ao.analysis_session_id = ?
		ORDER BY ao.invoked_at ASC`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list outputs for session: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var out []AnalystOutputSummary
	for rows.Next() {
		var row AnalystOutputSummary
		var collectedFromID sql.NullString
		if err := rows.Scan(
			&row.OutputID, &row.EntityID, &row.EntityURI,
			&collectedFromID, &row.CollectedFromURI,
			&row.AnalystID, &row.Model, &row.PromptVersion,
			&row.InvokedAt, &row.IngestedAt, &row.Round,
			&row.TargetCommit, &row.SourcePath, &row.ContentHash,
			&row.ConclusionsCount, &row.PositiveAbsenceCount,
			&row.ObservationCount, &row.PatternCount,
		); err != nil {
			return nil, fmt.Errorf("scan output row: %w", err)
		}
		if collectedFromID.Valid {
			row.CollectedFromEntityID = collectedFromID.String
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// scanAnalysisSession handles one row of the analysis_sessions
// column set. Accepts anything with a Scan method — *sql.Row from
// QueryRowContext, or *sql.Rows from the List path.
func scanAnalysisSession(row interface {
	Scan(dest ...any) error
}) (*profile.AnalysisSession, error) {
	var (
		sess              profile.AnalysisSession
		startedAtStr      string
		endedAtStr        string
		expectedAnalysts  string
		status            string
		synthesisOutputID sql.NullString
	)
	err := row.Scan(
		&sess.ID, &sess.EntityID, &sess.TargetURI, &sess.TargetVersion,
		&sess.InvokedBy, &sess.PipelineSessionID, &expectedAnalysts,
		&startedAtStr, &endedAtStr, &status, &synthesisOutputID, &sess.Notes,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan analysis_session: %w", err)
	}
	sess.StartedAt, err = time.Parse(time.RFC3339, startedAtStr)
	if err != nil {
		return nil, fmt.Errorf("parse started_at %q: %w", startedAtStr, err)
	}
	if endedAtStr != "" {
		t, err := time.Parse(time.RFC3339, endedAtStr)
		if err != nil {
			return nil, fmt.Errorf("parse ended_at %q: %w", endedAtStr, err)
		}
		sess.EndedAt = &t
	}
	if expectedAnalysts != "" {
		sess.ExpectedAnalysts = strings.Split(expectedAnalysts, analystIDSeparator)
	}
	sess.Status = profile.AnalysisSessionStatus(status)
	if synthesisOutputID.Valid {
		sess.SynthesisOutputID = synthesisOutputID.String
	}
	return &sess, nil
}
