// Package resources implements the MCP resource handlers for signatory.
// Each resource implements mcp.Resource, reads from a store.Store (or its
// underlying *sql.DB for aggregation queries), and returns a uniform
// mcp.Response envelope.
package resources

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/sarahmaeve/signatory/internal/mcp"
	"github.com/sarahmaeve/signatory/internal/store"
)

// PostureResource serves signatory://posture — an aggregated overview of
// all recorded posture decisions grouped by tier, plus the oldest and
// newest posture entries.
type PostureResource struct {
	// Store is the persistence backend. Must be non-nil. The resource
	// calls Store.DB() directly for the aggregation query rather than
	// iterating every posture row, so the concrete type must expose DB().
	Store *store.SQLite
}

// URIPattern returns the literal URI for this static resource.
func (r *PostureResource) URIPattern() string {
	return "signatory://posture"
}

// Description summarises the resource for resources/list.
func (r *PostureResource) Description() string {
	return "READ THIS for a high-level posture overview ('how many deps have I assessed?', 'what's my posture distribution?'). Returns total count, breakdown by tier (trusted-for-now, vetted-frozen, rejected, etc.), and oldest/newest entries. Prefer this over signatory_show_analyses when the user wants counts and distribution, not per-analysis detail."
}

// postureData is the JSON shape returned by signatory://posture.
type postureData struct {
	Total         int            `json:"total"`
	ByTier        map[string]int `json:"by_tier"`
	OldestPosture *postureAnchor `json:"oldest_posture,omitempty"`
	NewestPosture *postureAnchor `json:"newest_posture,omitempty"`
}

// postureAnchor is the minimal shape for oldest/newest posture entries.
type postureAnchor struct {
	EntityID string `json:"entity_id"`
	SetAt    string `json:"set_at"` // RFC3339
}

// Read queries the postures table for tier counts and boundary rows.
//
// SQL used:
//
//	SELECT tier, COUNT(*) FROM postures GROUP BY tier
//	SELECT entity_id, set_at FROM postures ORDER BY set_at ASC  LIMIT 1
//	SELECT entity_id, set_at FROM postures ORDER BY set_at DESC LIMIT 1
//
// All three queries run within Read's context; cancellation aborts them.
func (r *PostureResource) Read(ctx context.Context, _ string) *mcp.Response {
	db := r.Store.DB()

	byTier, total, err := queryPostureCounts(ctx, db)
	if err != nil {
		return mcp.Err(mcp.CodeInternalError,
			fmt.Sprintf("query posture counts: %v", err), nil)
	}

	oldest, err := queryPostureAnchor(ctx, db, "ASC")
	if err != nil {
		return mcp.Err(mcp.CodeInternalError,
			fmt.Sprintf("query oldest posture: %v", err), nil)
	}

	newest, err := queryPostureAnchor(ctx, db, "DESC")
	if err != nil {
		return mcp.Err(mcp.CodeInternalError,
			fmt.Sprintf("query newest posture: %v", err), nil)
	}

	return mcp.OK(postureData{
		Total:         total,
		ByTier:        byTier,
		OldestPosture: oldest,
		NewestPosture: newest,
	})
}

// queryPostureCounts runs the GROUP BY aggregation and returns the
// per-tier counts plus the sum.
func queryPostureCounts(ctx context.Context, db *sql.DB) (map[string]int, int, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT tier, COUNT(*) FROM postures GROUP BY tier`)
	if err != nil {
		return nil, 0, err
	}
	// rows.Err() below surfaces any iteration error. Close on a local
	// SQLite *Rows releases an in-process cursor — it has no I/O to
	// fail against, so ignoring its return is safe.
	defer rows.Close() //nolint:errcheck // local sqlite cursor close cannot fail recoverably; rows.Err() covers iteration errors

	byTier := make(map[string]int)
	total := 0
	for rows.Next() {
		var tier string
		var count int
		if err := rows.Scan(&tier, &count); err != nil {
			return nil, 0, err
		}
		byTier[tier] = count
		total += count
	}
	return byTier, total, rows.Err()
}

// queryPostureAnchor returns the oldest (order="ASC") or newest
// (order="DESC") posture row, or nil when the table is empty.
//
// Security: order is an internal constant ("ASC" or "DESC"), never
// derived from user input. The entity_id is read from the DB and
// returned verbatim — it is a UUID from a trusted store.
func queryPostureAnchor(ctx context.Context, db *sql.DB, order string) (*postureAnchor, error) {
	// order is a compile-time constant passed from Read — not user input.
	q := fmt.Sprintf( //nolint:gosec // G201: order is an internal ASC/DESC constant, not user input
		`SELECT entity_id, set_at FROM postures ORDER BY set_at %s LIMIT 1`, order)
	var entityID, setAt string
	err := db.QueryRowContext(ctx, q).Scan(&entityID, &setAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// Normalise to RFC3339 for consistent output; the stored value is
	// already RFC3339 per the store's write path.
	if _, parseErr := time.Parse(time.RFC3339, setAt); parseErr != nil {
		return nil, fmt.Errorf("parse set_at %q: %w", setAt, parseErr)
	}
	return &postureAnchor{EntityID: entityID, SetAt: setAt}, nil
}
