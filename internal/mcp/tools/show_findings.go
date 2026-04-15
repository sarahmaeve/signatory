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

// ShowFindingsTool implements signatory_show_findings.
//
// Queries findings across ingested analyst outputs. The severity filter
// accepts an array of string values matching exchange.SeverityValue; each
// value is validated — an unrecognised severity is a schema violation.
// design_intent is a nullable bool (pointer): when present, limits to
// findings with that design_intent flag value.
type ShowFindingsTool struct {
	Store store.Store
}

func (t *ShowFindingsTool) Name() string { return "signatory_show_findings" }

func (t *ShowFindingsTool) Description() string {
	return "Query findings across ingested analyst outputs with optional filtering."
}

func (t *ShowFindingsTool) InputSchema() json.RawMessage {
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

// showFindingsInput is the typed input for signatory_show_findings.
// design_intent uses *bool so we can distinguish "not set" from false.
type showFindingsInput struct {
	Target       string   `json:"target,omitempty"`
	AnalystID    string   `json:"analyst_id,omitempty"`
	SignalType   string   `json:"signal_type,omitempty"`
	Severity     []string `json:"severity,omitempty"`
	DesignIntent *bool    `json:"design_intent,omitempty"`
	Limit        int      `json:"limit,omitempty"`
}

// Compile-time interface check.
var _ mcp.Tool = (*ShowFindingsTool)(nil)

func (t *ShowFindingsTool) Handle(ctx context.Context, raw json.RawMessage) *mcp.Response {
	var in showFindingsInput
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

	filter := store.FindingFilter{
		EntityURI:  normalizeTargetURI(in.Target),
		AnalystID:  in.AnalystID,
		SignalType: in.SignalType,
		SeverityIn: severities,
		Limit:      limit,
	}
	if in.DesignIntent != nil && *in.DesignIntent {
		filter.DesignIntentOnly = true
	}

	rows, err := t.Store.ListFindings(ctx, filter)
	if errors.Is(err, store.ErrNotFound) {
		return mcp.Err(mcp.CodeNotFound,
			"entity not found: "+in.Target,
			map[string]string{"target": in.Target})
	}
	if err != nil {
		return mcp.Err(mcp.CodeInternalError, "list findings failed: "+err.Error(), nil)
	}

	if rows == nil {
		rows = []store.FindingSummary{}
	}

	return mcp.OK(map[string]interface{}{
		"findings": rows,
		"count":    len(rows),
	})
}
