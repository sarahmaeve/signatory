package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

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
	return "USE THIS when you have produced a v1-schema AnalystOutput as a specialist analyst (security, provenance, synthesist) and need to land your conclusions in the store. The output becomes queryable by signatory_show_analyses, signatory_show_conclusions, and signatory_analyze. Idempotent on content — re-ingesting identical payload returns the existing row's IDs with idempotent=true. If validation fails, the response names the first offending field so you can fix the payload and retry rather than dropping fields. Required payload shape: analyst_output carrying attribution{analyst_id}, a non-empty target, and any conclusions/observations/positive_absences/methodology_trace your analysis produced. attribution.model and attribution.invoked_at are server-stamped — DO NOT include them; the validator rejects caller-supplied values to prevent agents from hallucinating model identity or wall-clock timestamps. See design/mcp-dual-analyst-architecture.md for the schema."
}

func (t *IngestAnalysisTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"analyst_output": {
				"type": "object",
				"description": "v1-schema AnalystOutput payload. Required nested fields: attribution (with analyst_id), target (canonical URI or URL). Optional: conclusions, positive_absences, observations, methodology_trace, supersedes, round_notes. DO NOT include attribution.model or attribution.invoked_at: those are server-stamped (model from OTEL backfill, invoked_at from time.Now() at ingest); the validator rejects caller-supplied values."
			},
			"source": {
				"type": "string",
				"description": "Optional identifier for the origin of this output — agent role, session id, or file path. Recorded on the store row for audit. Defaults to 'mcp' when omitted."
			},
			"collected_from": {
				"type": "string",
				"description": "Optional primary-identity override: the target URI the original caller asked about. When set and it resolves to a different canonical URI than analyst_output.target, the analysis is indexed under collected_from with analyst_output.target captured as the resolved source. Use when the caller asked about pkg:<eco>/<name> but the analysis was performed against the resolved github source repo. See agent-facing-contract §3.2."
			},
			"analysis_session_id": {
				"type": "string",
				"description": "Optional analysis_sessions.id to link this output to. When set, the ingested row's analysis_session_id FK is stamped at INSERT, making the output visible under 'signatory analysis show|timing <session-id>'. Get the session id from 'signatory analysis begin' (the skill's dispatch step). Unknown ids surface as an ingest failure (FK constraint). Omit when ingesting outside a session; the column stays NULL and the ingest still succeeds."
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

	// CollectedFrom, when non-empty, names the target URI the
	// caller originally asked about. The store uses it as the
	// primary identity; analyst_output.target becomes the
	// collected_from link. Agent-facing-contract §3.2.
	CollectedFrom string `json:"collected_from,omitempty"`

	// AnalysisSessionID, when non-empty, stamps the
	// analyst_outputs.analysis_session_id FK on the new row. The
	// skill's /analyze dispatch calls `signatory analysis begin` to
	// get the id and substitutes it into each analyst's handoff
	// template; the analyst passes it through verbatim here.
	AnalysisSessionID string `json:"analysis_session_id,omitempty"`
}

// ingestAnalysisData is the result payload.
type ingestAnalysisData struct {
	OutputID              string               `json:"output_id"`
	EntityID              string               `json:"entity_id"`
	CollectedFromEntityID string               `json:"collected_from_entity_id,omitempty"`
	Idempotent            bool                 `json:"idempotent"`
	Counts                ingestAnalysisCounts `json:"counts"`
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
	// The validator also rejects caller-supplied attribution.model
	// and attribution.invoked_at: those are server-stamped inside
	// the store layer (see analyst_output.go IngestAnalystOutput
	// for where invoked_at is filled in from time.Now() after the
	// content hash is computed).
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

	var ingestOpts []store.IngestOption
	if in.CollectedFrom != "" {
		ingestOpts = append(ingestOpts, store.WithPrimaryTarget(in.CollectedFrom))
	}
	if in.AnalysisSessionID != "" {
		ingestOpts = append(ingestOpts, store.WithAnalysisSession(in.AnalysisSessionID))
	}

	result, err := t.Store.IngestAnalystOutput(ctx, &out, source, ingestOpts...)
	if err != nil {
		// Synthesis-specific contract violations are agent-correctable:
		// surface them as schema violations naming the missing field
		// so the agent retries with the corrected payload in the same
		// turn. Any other ingest error is internal.
		if errors.Is(err, store.ErrSynthesisRequiresSession) {
			return mcp.Err(mcp.CodeSchemaViolation,
				"synthesis outputs require analysis_session_id; pass it as the top-level field of the ingest call (the SESSION_INSTRUCTION block in your handoff names the id)",
				map[string]string{"field": "analysis_session_id"})
		}
		// Non-sentinel ingest failures are internal — store / driver
		// text does not cross the MCP boundary. The agent gets a
		// stable opaque message plus the target it tried to ingest
		// in the structured details, which is enough to retry or
		// surface to the user.
		return mcp.Err(mcp.CodeInternalError, "ingest failed",
			map[string]string{"operation": "IngestAnalystOutput", "target": out.Target})
	}

	patternCount := 0
	if out.MethodologyTrace != nil {
		patternCount = len(out.MethodologyTrace.Patterns)
	}

	return mcp.OK(ingestAnalysisData{
		OutputID:              result.OutputID,
		EntityID:              result.EntityID,
		CollectedFromEntityID: result.CollectedFromEntityID,
		Idempotent:            result.Idempotent,
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
