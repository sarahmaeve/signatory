package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/sarahmaeve/signatory/internal/exchange"
	"github.com/sarahmaeve/signatory/internal/mcp"
	"github.com/sarahmaeve/signatory/internal/store"
)

// ShowConclusionsTool implements signatory_show_conclusions.
//
// Queries conclusions across ingested analyst outputs. The severity filter
// accepts an array of string values matching exchange.SeverityValue; each
// value is validated — an unrecognised severity is a schema violation.
// design_intent is a nullable bool (pointer): when present, limits to
// conclusions with that design_intent flag value.
type ShowConclusionsTool struct {
	Store store.Store
}

func (t *ShowConclusionsTool) Name() string { return "signatory_show_conclusions" }

func (t *ShowConclusionsTool) Description() string {
	return "USE THIS when the user asks about specific concerns or conclusions across analyses — 'what conclusions mention supply-chain risk?', 'show medium-severity conclusions for X', 'are there design-intent positives recorded?'. Searches individual Conclusion records across every ingested analysis, with filters for target, severity, category, signal_type, and design-intent. Returns per-conclusion records; use signatory_show_analyses when the user wants analysis-level summaries, not individual conclusions."
}

func (t *ShowConclusionsTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"target":        {"type": "string"},
			"analyst_id":    {"type": "string"},
			"signal_type":   {"type": "string"},
			"severity":      {
				"type": "array",
				"items": {
					"type": "string",
					"enum": ["positive", "informational", "low", "medium", "high", "critical"]
				}
			},
			"design_intent": {"type": "boolean"},
			"limit":         {"type": "integer", "minimum": 0, "default": 20}
		},
		"additionalProperties": false
	}`)
}

// showConclusionsInput is the typed input for signatory_show_conclusions.
// design_intent uses *bool so we can distinguish "not set" from false.
type showConclusionsInput struct {
	Target       string   `json:"target,omitempty"`
	AnalystID    string   `json:"analyst_id,omitempty"`
	SignalType   string   `json:"signal_type,omitempty"`
	Severity     []string `json:"severity,omitempty"`
	DesignIntent *bool    `json:"design_intent,omitempty"`
	Limit        int      `json:"limit,omitempty"`
}

// Compile-time interface check.
var _ mcp.Tool = (*ShowConclusionsTool)(nil)

func (t *ShowConclusionsTool) Handle(ctx context.Context, raw json.RawMessage) *mcp.Response {
	var in showConclusionsInput
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return mcp.Err(mcp.CodeSchemaViolation, err.Error(), nil)
	}

	// Validate each severity string against the exchange enum.
	severities := make([]exchange.SeverityValue, 0, len(in.Severity))
	for _, s := range in.Severity {
		sv := exchange.SeverityValue(s)
		if !sv.Valid() {
			return mcp.Err(mcp.CodeSchemaViolation,
				fmt.Sprintf("invalid severity %q (valid values: positive, informational, low, medium, high, critical)", s),
				map[string]string{"field": "severity", "received": s})
		}
		severities = append(severities, sv)
	}

	limit := in.Limit
	if limit == 0 {
		limit = 20
	}

	// See ShowAnalysesTool.Handle / lookup.go for why filter
	// resolution moved to the alternate-walking LookupEntityID.
	entityID, lookupErr := resolveTargetForFilter(ctx, t.Store, in.Target)
	if errResp := mapTargetLookupErr(lookupErr, in.Target); errResp != nil {
		return errResp
	}

	filter := store.ConclusionFilter{
		EntityID:   entityID,
		AnalystID:  in.AnalystID,
		SignalType: in.SignalType,
		SeverityIn: severities,
		Limit:      limit,
	}
	if in.DesignIntent != nil && *in.DesignIntent {
		filter.DesignIntentOnly = true
	}

	rows, err := t.Store.ListConclusions(ctx, filter)
	if errors.Is(err, store.ErrNotFound) {
		// Defensive — resolveTargetForFilter already gated absent
		// entities; this branch keeps the tool's failure shape stable
		// if a future store-side change reintroduces a NotFound path.
		return mcp.Err(mcp.CodeNotFound,
			"entity not found: "+in.Target,
			map[string]string{"target": in.Target})
	}
	if err != nil {
		return mcp.Err(mcp.CodeInternalError, "list conclusions failed: "+err.Error(), nil)
	}

	if rows == nil {
		rows = []store.ConclusionSummary{}
	}

	return mcp.OK(map[string]any{
		"conclusions": rows,
		"count":       len(rows),
	})
}
