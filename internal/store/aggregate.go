package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Store-wide aggregation reads that back the MCP overview resources
// (signatory://posture, signatory://unexamined). They live here, next
// to the tables they query, so the SQL is unit-testable in the store
// package rather than embedded in the MCP presentation layer.

// PostureAnchor is a minimal posture boundary row: the entity holding a
// posture and the timestamp it was set. Returned by PostureBoundaries.
// SetAt is the value stored in the postures.set_at column (RFC3339 per
// the write path); presentation-layer callers validate/format it.
type PostureAnchor struct {
	EntityID string
	SetAt    string
}

// UnexaminedEntity is an entity observed as a dependency that has no
// posture decision recorded — a blind spot in the trust inventory.
// Timestamps are the raw stored (RFC3339) values.
type UnexaminedEntity struct {
	EntityID     string
	CanonicalURI string
	ShortName    string
	CreatedAt    string
}

// CountPosturesByTier groups active posture rows by tier, returning
// the per-tier counts and the grand total.
//
// Withdrawn postures (those with a non-empty withdrawn_at) are
// excluded, matching every other active posture read (GetPosture,
// GetPostures, HasPostures, ListPostures): a withdrawn posture is a
// retracted decision retained only for audit continuity, not a current
// assessment, so it must not inflate an overview that answers "how many
// deps have I assessed?".
func (s *SQLite) CountPosturesByTier(ctx context.Context) (map[string]int, int, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT tier, COUNT(*) FROM postures WHERE withdrawn_at = '' GROUP BY tier`)
	if err != nil {
		return nil, 0, fmt.Errorf("count postures by tier: %w", err)
	}
	defer rows.Close() //nolint:errcheck // local sqlite cursor close cannot fail recoverably; rows.Err() covers iteration errors

	byTier := make(map[string]int)
	total := 0
	for rows.Next() {
		var tier string
		var count int
		if err := rows.Scan(&tier, &count); err != nil {
			return nil, 0, fmt.Errorf("scan posture tier count: %w", err)
		}
		byTier[tier] = count
		total += count
	}
	return byTier, total, rows.Err()
}

// postureOldestQuery / postureNewestQuery select the boundary posture
// rows. Written as two literal queries (rather than interpolating an
// ASC/DESC token) so there is no fmt.Sprintf-into-SQL to justify. Both
// filter withdrawn rows, matching CountPosturesByTier.
const (
	postureOldestQuery = `SELECT entity_id, set_at FROM postures WHERE withdrawn_at = '' ORDER BY set_at ASC LIMIT 1`
	postureNewestQuery = `SELECT entity_id, set_at FROM postures WHERE withdrawn_at = '' ORDER BY set_at DESC LIMIT 1`
)

// PostureBoundaries returns the oldest and newest active posture rows
// by set_at. Either return is nil when no active posture exists.
// Withdrawn rows are excluded (see CountPosturesByTier).
func (s *SQLite) PostureBoundaries(ctx context.Context) (oldest, newest *PostureAnchor, err error) {
	oldest, err = s.postureAnchor(ctx, postureOldestQuery)
	if err != nil {
		return nil, nil, fmt.Errorf("oldest posture: %w", err)
	}
	newest, err = s.postureAnchor(ctx, postureNewestQuery)
	if err != nil {
		return nil, nil, fmt.Errorf("newest posture: %w", err)
	}
	return oldest, newest, nil
}

// postureAnchor runs a single-row boundary query, returning nil (not an
// error) when the table is empty.
func (s *SQLite) postureAnchor(ctx context.Context, query string) (*PostureAnchor, error) {
	var a PostureAnchor
	err := s.db.QueryRowContext(ctx, query).Scan(&a.EntityID, &a.SetAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// ListUnexaminedEntities returns entities observed in
// dependency_observations that have no ACTIVE posture, most-recently-
// created first.
//
// The NOT EXISTS subquery requires an empty withdrawn_at, so an entity
// whose only posture was later withdrawn reappears here: a withdrawn
// decision is a retracted one, and "which deps still need a posture?"
// must list it again. Consistent with CountPosturesByTier /
// PostureBoundaries and the rest of the active-posture read surface.
//
// V0.1 simplification: the sort uses entities.created_at DESC as a
// proxy for "recent additions." A criticality-aware sort (stars,
// downloads, transitive fan-out) is the v0.2 upgrade and would extract
// the criticality signal per entity.
func (s *SQLite) ListUnexaminedEntities(ctx context.Context) ([]UnexaminedEntity, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT e.id, e.canonical_uri, e.short_name, e.created_at
		FROM entities e
		INNER JOIN dependency_observations do ON do.entity_id = e.id
		WHERE NOT EXISTS (
			SELECT 1 FROM postures p WHERE p.entity_id = e.id AND p.withdrawn_at = ''
		)
		ORDER BY e.created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list unexamined entities: %w", err)
	}
	defer rows.Close() //nolint:errcheck // local sqlite cursor close cannot fail recoverably; rows.Err() covers iteration errors

	out := []UnexaminedEntity{}
	for rows.Next() {
		var e UnexaminedEntity
		if err := rows.Scan(&e.EntityID, &e.CanonicalURI, &e.ShortName, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan unexamined entity: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
