package resources

import (
	"context"
	"fmt"

	"github.com/sarahmaeve/signatory/internal/mcp"
	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/store"
)

// BurnsResource serves signatory://burns — the full list of active burn
// records, returned as a raw array in the data envelope.
type BurnsResource struct {
	// Store is the persistence backend. Must be non-nil.
	Store store.Store
}

// URIPattern returns the literal URI for this static resource.
func (r *BurnsResource) URIPattern() string {
	return "signatory://burns"
}

// Description summarises the resource for resources/list.
func (r *BurnsResource) Description() string {
	return "READ THIS when the user asks 'what has signatory burned?', 'is anything marked compromised?', or before recommending a dependency to check it isn't on the burn list. Returns every active burn record with reason, source (local vs. inherited), and timestamp. A burn is a hard-reject marker independent of posture."
}

// Read calls store.ListBurns and returns the full array. An empty
// store returns an empty JSON array, not null.
func (r *BurnsResource) Read(ctx context.Context, _ string) *mcp.Response {
	burns, err := r.Store.ListBurns(ctx)
	if err != nil {
		return mcp.Err(mcp.CodeInternalError,
			fmt.Sprintf("list burns: %v", err), nil)
	}
	// Guarantee a non-nil slice so JSON encodes as [] not null.
	if burns == nil {
		burns = []profile.Burn{}
	}
	return mcp.OK(burns)
}
