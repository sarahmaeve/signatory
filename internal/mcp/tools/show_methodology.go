package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/sarahmaeve/signatory/internal/mcp"
	"github.com/sarahmaeve/signatory/internal/store"
)

// ShowMethodologyTool implements signatory_show_methodology.
//
// Queries methodology patterns across ingested analyst outputs. The
// hit_on_target field is an optional enum: "hit" (true), "miss" (false),
// or omitted (no filter). An unrecognised hit_on_target value is a
// strict-reject schema violation.
type ShowMethodologyTool struct {
	Store store.Store
}

func (t *ShowMethodologyTool) Name() string { return "signatory_show_methodology" }

func (t *ShowMethodologyTool) Description() string {
	return "USE THIS when the user asks *how* signatory analyzes something — 'what patterns does signatory look for?', 'which methodology hit on X?', 'show the grep-automatable patterns'. Methodology patterns are the reusable detection recipes that produce conclusions; conclusions are what those patterns turn up. Filterable by signal_group (vitality, hygiene, etc.), automation hint (grep_precision), and hit_on_target (hit/miss)."
}

func (t *ShowMethodologyTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"target":        {"type": "string"},
			"analyst_id":    {"type": "string"},
			"signal_group":  {"type": "string"},
			"hit_on_target": {"type": "string", "enum": ["hit", "miss"]},
			"limit":         {"type": "integer", "minimum": 0, "default": 20}
		},
		"additionalProperties": false
	}`)
}

// showMethodologyInput is the typed input for signatory_show_methodology.
type showMethodologyInput struct {
	Target      string `json:"target,omitempty"`
	AnalystID   string `json:"analyst_id,omitempty"`
	SignalGroup string `json:"signal_group,omitempty"`
	HitOnTarget string `json:"hit_on_target,omitempty"`
	Limit       int    `json:"limit,omitempty"`
}

// Compile-time interface check.
var _ mcp.Tool = (*ShowMethodologyTool)(nil)

func (t *ShowMethodologyTool) Handle(ctx context.Context, raw json.RawMessage) *mcp.Response {
	var in showMethodologyInput
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return mcp.Err(mcp.CodeSchemaViolation, err.Error(), nil)
	}

	// Validate the hit_on_target enum.
	if in.HitOnTarget != "" && in.HitOnTarget != "hit" && in.HitOnTarget != "miss" {
		return mcp.Err(mcp.CodeSchemaViolation,
			`hit_on_target must be "hit" or "miss"`,
			map[string]string{
				"field":        "hit_on_target",
				"received":     in.HitOnTarget,
				"valid_values": "hit, miss",
			})
	}

	limit := in.Limit
	if limit == 0 {
		limit = 20
	}

	filter := store.MethodologyPatternFilter{
		EntityURI:   normalizeTargetURI(in.Target),
		AnalystID:   in.AnalystID,
		SignalGroup: in.SignalGroup,
		Limit:       limit,
	}
	switch in.HitOnTarget {
	case "hit":
		v := true
		filter.HitOnTarget = &v
	case "miss":
		v := false
		filter.HitOnTarget = &v
	}

	rows, err := t.Store.ListMethodologyPatterns(ctx, filter)
	if errors.Is(err, store.ErrNotFound) {
		return mcp.Err(mcp.CodeNotFound,
			"entity not found: "+in.Target,
			map[string]string{"target": in.Target})
	}
	if err != nil {
		return mcp.Err(mcp.CodeInternalError, "list methodology patterns failed: "+err.Error(), nil)
	}

	if rows == nil {
		rows = []store.MethodologyPatternSummary{}
	}

	return mcp.OK(map[string]any{
		"patterns": rows,
		"count":    len(rows),
	})
}
