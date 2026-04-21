package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/sarahmaeve/signatory/internal/mcp"
	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/store"
	"github.com/sarahmaeve/signatory/internal/summary"
)

// SummaryTool implements signatory_summary — the M7 "one place to
// start" tool. Returns the composed view of everything signatory
// knows about a target at a decision level (canonical URI, posture,
// burn status, analyses rollup, related identities) in one call.
//
// Designed to replace the cross-tool flail yesterday's synthesist
// agent exhibited (show-analyses → show-conclusions → show-
// methodology, plus bash CLI variants) to piece together a view.
// Callers that need drill-down use the dedicated per-concern tools
// (signatory_show_conclusions, signatory_show_methodology,
// signatory_signals, signatory_detail); summary is the breadth pass
// that makes those drill-downs targeted.
type SummaryTool struct {
	Store store.Store
}

func (t *SummaryTool) Name() string { return "signatory_summary" }

func (t *SummaryTool) Description() string {
	return "USE THIS FIRST when asked anything about a specific package, repo, or identity — 'what do we know about X?', 'is Y safe?', 'what have we analyzed for Z?'. Composes canonical URI, related identities (M2 collected_from links in both directions), current posture, active burn, and per-analyst rollup (severity-counted conclusions, positive absences, observations, methodology-pattern counts) into one response. Drill-down tools (signatory_show_conclusions, signatory_show_methodology, signatory_signals, signatory_detail) are for when you already know what you're looking for; summary is the breadth pass that tells you whether to drill in at all. Returns NotFound with a descriptive error when the target hasn't been ingested."
}

func (t *SummaryTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"target": {"type": "string", "description": "Canonical URI, URL, or owner/repo shorthand."}
		},
		"required": ["target"],
		"additionalProperties": false
	}`)
}

// summaryInput is the typed input for signatory_summary.
type summaryInput struct {
	Target string `json:"target"`
}

func (t *SummaryTool) Handle(ctx context.Context, raw json.RawMessage) *mcp.Response {
	var in summaryInput
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return mcp.Err(mcp.CodeSchemaViolation, err.Error(), nil)
	}
	if in.Target == "" {
		return mcp.Err(mcp.CodeSchemaViolation, "target is required", nil)
	}

	resolved, err := profile.ResolveTarget(in.Target)
	if err != nil {
		return mcp.Err(mcp.CodeSchemaViolation,
			"target "+in.Target+" does not resolve to a canonical URI: "+err.Error(),
			map[string]string{"phase": "resolve"})
	}

	s, err := summary.New(t.Store).Assemble(ctx, resolved.CanonicalURI)
	if err != nil {
		if errors.Is(err, summary.ErrEntityNotFound) {
			return mcp.Err(mcp.CodeNotFound, err.Error(),
				map[string]string{"target": resolved.CanonicalURI})
		}
		return mcp.Err(mcp.CodeInternalError, "assemble summary: "+err.Error(), nil)
	}
	return mcp.OK(s)
}
