package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/sarahmaeve/signatory/internal/mcp"
	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/store"
)

// ShowAnalysesTool implements signatory_show_analyses.
//
// Lists ingested analyst outputs from the store. The ErrNotFound sentinel
// (target URI supplied but not in store) maps to CodeNotFound; an empty
// slice (target exists but no outputs yet) returns OK with an empty array.
// This distinction matters for callers that need to know "has this target
// ever been ingested" vs. "it has been ingested but has no analyses yet."
type ShowAnalysesTool struct {
	Store store.Store
}

func (t *ShowAnalysesTool) Name() string { return "signatory_show_analyses" }

func (t *ShowAnalysesTool) Description() string {
	return "USE THIS when the user asks 'what has signatory analyzed?', 'what assessments exist recently?', or 'are there any analyses of X?'. Lists trust analyses recorded in signatory's store, with optional filters for target URI, analyst_id, and since-timestamp. An empty list is a real answer — it means nothing has been ingested yet, not that the tool failed. For specific concerns within analyses, use signatory_show_conclusions."
}

func (t *ShowAnalysesTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"target":      {"type": "string", "description": "Optional target URI or URL form."},
			"analyst_id":  {"type": "string", "description": "Filter by analyst_id."},
			"since":       {"type": "string", "description": "RFC3339 timestamp; return outputs ingested at or after this time."},
			"limit":       {"type": "integer", "minimum": 0, "default": 20}
		},
		"additionalProperties": false
	}`)
}

// showAnalysesInput is the typed input for signatory_show_analyses.
type showAnalysesInput struct {
	Target    string `json:"target,omitempty"`
	AnalystID string `json:"analyst_id,omitempty"`
	Since     string `json:"since,omitempty"`
	Limit     int    `json:"limit,omitempty"`
}

// Compile-time interface check.
var _ mcp.Tool = (*ShowAnalysesTool)(nil)

func (t *ShowAnalysesTool) Handle(ctx context.Context, raw json.RawMessage) *mcp.Response {
	var in showAnalysesInput
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return mcp.Err(mcp.CodeSchemaViolation, err.Error(), nil)
	}

	limit := in.Limit
	if limit == 0 {
		limit = 20
	}

	filter := store.AnalystOutputFilter{
		EntityURI: normalizeTargetURI(in.Target),
		AnalystID: in.AnalystID,
		Limit:     limit,
	}

	if in.Since != "" {
		t2, err := time.Parse(time.RFC3339, in.Since)
		if err != nil {
			return mcp.Err(mcp.CodeSchemaViolation,
				"since must be an RFC3339 timestamp: "+err.Error(),
				map[string]string{"field": "since"})
		}
		filter.Since = t2
	}

	rows, err := t.Store.ListAnalystOutputs(ctx, filter)
	if errors.Is(err, store.ErrNotFound) {
		return mcp.Err(mcp.CodeNotFound,
			"entity not found: "+in.Target,
			map[string]string{"target": in.Target})
	}
	if err != nil {
		return mcp.Err(mcp.CodeInternalError, "list analyst outputs failed: "+err.Error(), nil)
	}

	// Nil slice → empty JSON array, not null.
	if rows == nil {
		rows = []store.AnalystOutputSummary{}
	}

	return mcp.OK(map[string]any{
		"analyses": rows,
		"count":    len(rows),
	})
}

// normalizeTargetURI returns the canonical form of a target string for use
// in store filters. Already-canonical URIs (pkg:, repo:, etc.) pass through
// unchanged; GitHub-shaped inputs (owner/repo, https://github.com/..., etc.)
// go through profile.NormalizeGitHubRepoInput to reach the canonical
// repo:github/owner/name form; anything else falls through as-is so the
// store's FindEntityByURI can either match a verbatim-stored canonical
// URI or return ErrNotFound. This mirrors normalizeTargetForQuery in
// cmd/signatory/show.go — both CLI and MCP surfaces accept the same
// target shapes.
func normalizeTargetURI(target string) string {
	if target == "" {
		return ""
	}
	for _, prefix := range []string{"pkg:", "repo:", "identity:", "org:", "patch:"} {
		if strings.HasPrefix(target, prefix) {
			return target
		}
	}
	if uri, _, _, err := profile.NormalizeGitHubRepoInput(target); err == nil {
		return uri
	}
	return target
}
