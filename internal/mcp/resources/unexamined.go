package resources

import (
	"context"
	"fmt"

	"github.com/sarahmaeve/signatory/internal/mcp"
	"github.com/sarahmaeve/signatory/internal/store"
)

// unexaminedStore is the narrow read surface UnexaminedResource needs.
// Declared consumer-side so the resource depends on an interface, not
// the concrete *store.SQLite. *store.SQLite satisfies it naturally.
type unexaminedStore interface {
	ListUnexaminedEntities(ctx context.Context) ([]store.UnexaminedEntity, error)
}

// Compile-time check that the production store satisfies the interface.
var _ unexaminedStore = (*store.SQLite)(nil)

// UnexaminedResource serves signatory://unexamined — entities that have
// been observed as dependencies but have no posture decision recorded.
//
// These are the blind spots in the trust inventory: things the team has
// pulled in but never evaluated.
type UnexaminedResource struct {
	// Store is the persistence backend. Must be non-nil.
	Store unexaminedStore
}

// URIPattern returns the literal URI for this static resource.
func (r *UnexaminedResource) URIPattern() string {
	return "signatory://unexamined"
}

// Description summarises the resource for resources/list.
func (r *UnexaminedResource) Description() string {
	return "READ THIS when the user asks 'what haven't I vetted yet?', 'which deps still need a posture?', or 'what's the unassessed surface?'. Returns dependencies observed in a manifest that have no recorded posture, sorted most-recently-observed first. Empty until a manifest has been ingested — an empty list means no manifest-level surveying has happened, not that everything is assessed."
}

// unexaminedEntity is the per-row shape in the response array.
type unexaminedEntity struct {
	EntityID     string `json:"entity_id"`
	CanonicalURI string `json:"canonical_uri"`
	ShortName    string `json:"short_name"`
	// CreatedAt is the entity's store-creation timestamp, used as the
	// v0.1 sort proxy (see store.ListUnexaminedEntities for the v0.2
	// criticality-sort upgrade path).
	CreatedAt string `json:"created_at"` // RFC3339
}

// Read returns entities observed as dependencies with no posture row.
// The query lives in store.ListUnexaminedEntities; this handler maps
// the store rows into the response shape.
func (r *UnexaminedResource) Read(ctx context.Context, _ string) *mcp.Response {
	entities, err := r.Store.ListUnexaminedEntities(ctx)
	if err != nil {
		return mcp.Err(mcp.CodeInternalError,
			fmt.Sprintf("query unexamined entities: %v", err), nil)
	}

	out := make([]unexaminedEntity, 0, len(entities))
	for _, e := range entities {
		out = append(out, unexaminedEntity{
			EntityID:     e.EntityID,
			CanonicalURI: e.CanonicalURI,
			ShortName:    e.ShortName,
			CreatedAt:    e.CreatedAt,
		})
	}
	return mcp.OK(out)
}
