package resources

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/sarahmaeve/signatory/internal/mcp"
	"github.com/sarahmaeve/signatory/internal/store"
)

// UnexaminedResource serves signatory://unexamined — entities that have
// been observed as dependencies but have no posture decision recorded.
//
// These are the blind spots in the trust inventory: things the team has
// pulled in but never evaluated.
type UnexaminedResource struct {
	// Store is the persistence backend. Must be non-nil.
	Store *store.SQLite
}

// URIPattern returns the literal URI for this static resource.
func (r *UnexaminedResource) URIPattern() string {
	return "signatory://unexamined"
}

// Description summarises the resource for resources/list.
func (r *UnexaminedResource) Description() string {
	return "Dependencies without a posture decision, sorted by most-recently observed first."
}

// unexaminedEntity is the per-row shape in the response array.
type unexaminedEntity struct {
	EntityID     string `json:"entity_id"`
	CanonicalURI string `json:"canonical_uri"`
	ShortName    string `json:"short_name"`
	// CreatedAt is the entity's store-creation timestamp. Used as a
	// placeholder sort key in v0.1 (see v0.2 note below).
	CreatedAt string `json:"created_at"` // RFC3339
}

// Read queries for entities without posture rows and returns them sorted
// by entity creation timestamp (descending — most recently seen first).
//
// V0.1 simplification: criticality sort is a placeholder using entity
// created_at DESC as a proxy for "recent additions to the store." The
// proper criticality sort (stars, download count, transitive fan-out)
// requires signal data per entity, which would add a LEFT JOIN against
// the signals table and JSON extraction for each signal type.
//
// V0.2 upgrade path: replace ORDER BY e.created_at DESC with a
// subquery that extracts the criticality signal value for each entity
// (e.g., MAX(CAST(json_extract(s.value, '$') AS INTEGER)) for
// signal_group = 'criticality') and sorts by that. The response shape
// can also gain a criticality_proxy field at that point.
//
// SQL: entities that appear in dependency_observations but have no row
// in the postures table.
func (r *UnexaminedResource) Read(ctx context.Context, _ string) *mcp.Response {
	db := r.Store.DB()

	rows, err := db.QueryContext(ctx, `
		SELECT DISTINCT e.id, e.canonical_uri, e.short_name, e.created_at
		FROM entities e
		INNER JOIN dependency_observations do ON do.entity_id = e.id
		WHERE NOT EXISTS (
			SELECT 1 FROM postures p WHERE p.entity_id = e.id
		)
		ORDER BY e.created_at DESC`)
	if err != nil {
		return mcp.Err(mcp.CodeInternalError,
			fmt.Sprintf("query unexamined entities: %v", err), nil)
	}
	// scanUnexaminedRows calls rows.Err() to surface iteration errors.
	// Close on a local SQLite *Rows releases an in-process cursor — it
	// has no I/O to fail against, so ignoring its return is safe.
	defer rows.Close() //nolint:errcheck // local sqlite cursor close cannot fail recoverably; rows.Err() covers iteration errors

	entities, err := scanUnexaminedRows(rows)
	if err != nil {
		return mcp.Err(mcp.CodeInternalError,
			fmt.Sprintf("scan unexamined rows: %v", err), nil)
	}
	return mcp.OK(entities)
}

func scanUnexaminedRows(rows *sql.Rows) ([]unexaminedEntity, error) {
	out := []unexaminedEntity{}
	for rows.Next() {
		var e unexaminedEntity
		if err := rows.Scan(&e.EntityID, &e.CanonicalURI, &e.ShortName, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
