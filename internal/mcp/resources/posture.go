// Package resources implements the MCP resource handlers for signatory.
// Each resource implements mcp.Resource, reads from a narrow store
// interface, and returns a uniform mcp.Response envelope.
package resources

import (
	"context"
	"fmt"
	"time"

	"github.com/sarahmaeve/signatory/internal/mcp"
	"github.com/sarahmaeve/signatory/internal/store"
)

// postureOverviewStore is the narrow read surface PostureResource needs.
// Declared here (consumer-side) so the resource depends on an interface
// rather than the concrete *store.SQLite, and the aggregation SQL lives
// in the store package where it is unit-testable. *store.SQLite
// satisfies this naturally.
type postureOverviewStore interface {
	CountPosturesByTier(ctx context.Context) (map[string]int, int, error)
	PostureBoundaries(ctx context.Context) (oldest, newest *store.PostureAnchor, err error)
}

// Compile-time check that the production store satisfies the interface.
var _ postureOverviewStore = (*store.SQLite)(nil)

// PostureResource serves signatory://posture — an aggregated overview of
// all recorded posture decisions grouped by tier, plus the oldest and
// newest posture entries.
type PostureResource struct {
	// Store is the persistence backend. Must be non-nil.
	Store postureOverviewStore
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

// Read returns the per-tier posture counts and boundary rows. The SQL
// lives in store.CountPosturesByTier / store.PostureBoundaries; this
// handler maps those into the response shape and validates that the
// stored set_at timestamps are RFC3339 before emitting them.
func (r *PostureResource) Read(ctx context.Context, _ string) *mcp.Response {
	byTier, total, err := r.Store.CountPosturesByTier(ctx)
	if err != nil {
		return mcp.Err(mcp.CodeInternalError,
			fmt.Sprintf("query posture counts: %v", err), nil)
	}

	oldest, newest, err := r.Store.PostureBoundaries(ctx)
	if err != nil {
		return mcp.Err(mcp.CodeInternalError,
			fmt.Sprintf("query posture boundaries: %v", err), nil)
	}

	oldestAnchor, err := toPostureAnchor(oldest)
	if err != nil {
		return mcp.Err(mcp.CodeInternalError,
			fmt.Sprintf("oldest posture: %v", err), nil)
	}
	newestAnchor, err := toPostureAnchor(newest)
	if err != nil {
		return mcp.Err(mcp.CodeInternalError,
			fmt.Sprintf("newest posture: %v", err), nil)
	}

	return mcp.OK(postureData{
		Total:         total,
		ByTier:        byTier,
		OldestPosture: oldestAnchor,
		NewestPosture: newestAnchor,
	})
}

// toPostureAnchor maps a store boundary row to the response shape,
// normalising/validating its timestamp. Returns (nil, nil) when the
// boundary is absent (empty postures table). The stored value is
// already RFC3339 per the store's write path; the parse is a defensive
// guard against corrupt rows.
func toPostureAnchor(a *store.PostureAnchor) (*postureAnchor, error) {
	if a == nil {
		return nil, nil
	}
	if _, err := time.Parse(time.RFC3339, a.SetAt); err != nil {
		return nil, fmt.Errorf("parse set_at %q: %w", a.SetAt, err)
	}
	return &postureAnchor{EntityID: a.EntityID, SetAt: a.SetAt}, nil
}
