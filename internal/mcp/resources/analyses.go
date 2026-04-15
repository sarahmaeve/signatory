package resources

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"github.com/sarahmaeve/signatory/internal/mcp"
	"github.com/sarahmaeve/signatory/internal/store"
)

// AnalysesResource serves signatory://analyses{?target} — a listing of
// ingested analyst outputs, optionally filtered by target URI.
//
// URI convention:
//   - signatory://analyses          → all outputs, keyed by entity URI
//   - signatory://analyses?target=… → outputs for the named target only
//
// URIPattern returns the base URI without query string. The protocol
// layer matches the incoming URI by prefix and passes the full URI
// (including any query string) to Read, which parses the ?target
// parameter itself.
type AnalysesResource struct {
	// Store is the persistence backend. Must be non-nil.
	Store store.Store
}

// URIPattern returns the base URI (no query) for prefix matching.
func (r *AnalysesResource) URIPattern() string {
	return "signatory://analyses"
}

// Description summarises the resource for resources/list.
func (r *AnalysesResource) Description() string {
	return "READ THIS when the user wants an index of analyses for a specific target, via ?target=<uri>. Same underlying data as the signatory_show_analyses tool, but as a resource — prefer the tool for rich filtering; prefer this resource when the caller just wants structured JSON for one target or the whole index."
}

// analysesData is the response shape when no target filter is applied.
type analysesData struct {
	// Outputs is the full listing, newest-ingested first.
	Outputs []store.AnalystOutputSummary `json:"outputs"`
	// Total is len(Outputs) provided for convenience.
	Total int `json:"total"`
}

// Read parses the optional ?target query parameter from uri and calls
// store.ListAnalystOutputs with the appropriate filter.
//
// When target resolves to an unknown entity, store.ListAnalystOutputs
// returns store.ErrNotFound; Read maps that to mcp.CodeNotFound so
// agents get a meaningful error rather than an empty array.
func (r *AnalysesResource) Read(ctx context.Context, uri string) *mcp.Response {
	// Parse the incoming URI (which may include a query string) via net/url.
	// signatory:// is not a standard scheme but net/url handles opaque URIs
	// fine as long as we access the raw query explicitly.
	parsed, err := url.Parse(uri)
	if err != nil {
		return mcp.Err(mcp.CodeSchemaViolation,
			"malformed URI: "+err.Error(), nil)
	}

	target := parsed.Query().Get("target")

	filter := store.AnalystOutputFilter{}
	if target != "" {
		filter.EntityURI = target
	}

	outputs, err := r.Store.ListAnalystOutputs(ctx, filter)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return mcp.Err(mcp.CodeNotFound,
				fmt.Sprintf("target %q not found in store", target),
				map[string]string{"target": target})
		}
		return mcp.Err(mcp.CodeInternalError,
			fmt.Sprintf("list analyst outputs: %v", err), nil)
	}

	// Return an empty slice rather than null.
	if outputs == nil {
		outputs = []store.AnalystOutputSummary{}
	}

	return mcp.OK(analysesData{
		Outputs: outputs,
		Total:   len(outputs),
	})
}
