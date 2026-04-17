package tools

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/sarahmaeve/signatory/internal/exchange"
	"github.com/sarahmaeve/signatory/internal/mcp"
	"github.com/sarahmaeve/signatory/internal/store"
)

// IngestAnalysisTool implements signatory_ingest_analysis — the
// first mutating tool in the MCP surface.
//
// Context: v0.1 Invariant 3 ("SQLite is canonical; scratch files
// are not") requires analyst subagents to write directly to the
// store instead of producing markdown that the orchestrator later
// converts to JSON and ingests. Invariant 4 names the transport
// for child-agent output as "MCP write tool; v1-schema JSON". This
// tool is that transport.
//
// Before this tool existed, the /analyze skill's Step 3 ran a
// markdown→build-output→JSON→ingest chain. That chain:
//
//   - requires the subagent to produce structured markdown (an
//     intermediate format with its own parsing rules);
//   - leaves orphan .md and .json files in filestore/analysis/ on
//     mid-pipeline failure;
//   - makes an ingest failure happen AFTER the subagent has
//     exited, forcing the orchestrator to re-dispatch rather
//     than surfacing the error to the writer in real time.
//
// This tool collapses all of that to one MCP call per analyst. A
// subagent finishes analysis, serializes an AnalystOutput to JSON,
// invokes signatory_ingest_analysis. If the payload is malformed
// or fails v1 schema validation, the tool returns
// CodeSchemaViolation naming the offending field — the agent sees
// the error in the same turn and can fix the payload before
// exiting.
type IngestAnalysisTool struct {
	Store store.Store
}

func (t *IngestAnalysisTool) Name() string { return "signatory_ingest_analysis" }

func (t *IngestAnalysisTool) Description() string {
	return "USE THIS when you have produced a v1-schema AnalystOutput as a specialist analyst (security, provenance, synthesist) and need to land your conclusions in the store. The output becomes queryable by signatory_show_analyses, signatory_show_conclusions, and signatory_analyze. Idempotent on content — re-ingesting identical payload returns the existing row's IDs with idempotent=true. If validation fails, the response names the first offending field so you can fix the payload and retry rather than dropping fields. Required payload shape: analyst_output carrying attribution{analyst_id, model, invoked_at}, a non-empty target, and any conclusions/observations/positive_absences/methodology_trace your analysis produced. See design/mcp-dual-analyst-architecture.md for the schema."
}

func (t *IngestAnalysisTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"analyst_output": {
				"type": "object",
				"description": "v1-schema AnalystOutput payload. Required nested fields: attribution (with analyst_id, model, invoked_at), target (canonical URI or URL). Optional: conclusions, positive_absences, observations, methodology_trace, supersedes, round_notes."
			},
			"source": {
				"type": "string",
				"description": "Optional identifier for the origin of this output — agent role, session id, or file path. Recorded on the store row for audit. Defaults to 'mcp' when omitted."
			}
		},
		"required": ["analyst_output"],
		"additionalProperties": false
	}`)
}

// ingestAnalysisInput is the typed input for signatory_ingest_analysis.
type ingestAnalysisInput struct {
	// AnalystOutput is the full v1-schema payload as raw JSON so it
	// can be unmarshaled into exchange.AnalystOutput with a single
	// source of truth for the schema (the exchange package).
	AnalystOutput json.RawMessage `json:"analyst_output"`

	// Source is an optional origin identifier; "mcp" by default.
	Source string `json:"source,omitempty"`
}

// ingestAnalysisData is the result payload.
type ingestAnalysisData struct {
	OutputID   string               `json:"output_id"`
	EntityID   string               `json:"entity_id"`
	Idempotent bool                 `json:"idempotent"`
	Counts     ingestAnalysisCounts `json:"counts"`
}

// ingestAnalysisCounts reports the row counts written for this
// AnalystOutput — useful for the caller to verify the payload
// landed with the shape they expected.
type ingestAnalysisCounts struct {
	Conclusions         int `json:"conclusions"`
	PositiveAbsences    int `json:"positive_absences"`
	Observations        int `json:"observations"`
	MethodologyPatterns int `json:"methodology_patterns"`
}

func (t *IngestAnalysisTool) Handle(ctx context.Context, raw json.RawMessage) *mcp.Response {
	var in ingestAnalysisInput
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return mcp.Err(mcp.CodeSchemaViolation, err.Error(), nil)
	}
	if len(in.AnalystOutput) == 0 {
		return mcp.Err(mcp.CodeSchemaViolation, "analyst_output is required", nil)
	}

	// Decode into the v1 type. Any shape mismatch surfaces here
	// as a json.Unmarshal error; we report it as a schema
	// violation with the decoder's own message (which names the
	// field) so the caller can self-correct in one turn.
	var out exchange.AnalystOutput
	if err := json.Unmarshal(in.AnalystOutput, &out); err != nil {
		return mcp.Err(mcp.CodeSchemaViolation,
			"analyst_output is not valid v1 JSON: "+err.Error(),
			map[string]string{"phase": "unmarshal"})
	}

	// Run the exchange-layer validator. This catches the
	// structural shape issues that json.Unmarshal accepts but the
	// store's ingest would reject — missing attribution fields,
	// empty target, conclusions without verdict/rationale, etc.
	if err := out.Validate(); err != nil {
		return mcp.Err(mcp.CodeSchemaViolation,
			"analyst_output failed v1 schema validation: "+err.Error(),
			map[string]string{"phase": "validate"})
	}

	source := in.Source
	if source == "" {
		// Default source tag lets us distinguish MCP-written rows
		// from CLI-written rows at query time without losing the
		// ability for specific callers to override with a more
		// precise identifier.
		source = "mcp"
	}

	result, err := t.Store.IngestAnalystOutput(ctx, &out, source)
	if err != nil {
		return mcp.Err(mcp.CodeInternalError,
			"ingest failed: "+err.Error(), nil)
	}

	patternCount := 0
	if out.MethodologyTrace != nil {
		patternCount = len(out.MethodologyTrace.Patterns)
	}

	return mcp.OK(ingestAnalysisData{
		OutputID:   result.OutputID,
		EntityID:   result.EntityID,
		Idempotent: result.Idempotent,
		Counts: ingestAnalysisCounts{
			Conclusions:         len(out.Conclusions),
			PositiveAbsences:    len(out.PositiveAbsences),
			Observations:        len(out.Observations),
			MethodologyPatterns: patternCount,
		},
	})
}

// Compile-time interface check.
var _ mcp.Tool = (*IngestAnalysisTool)(nil)
