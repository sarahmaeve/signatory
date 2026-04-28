package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/sarahmaeve/signatory/internal/exchange"
	"github.com/sarahmaeve/signatory/internal/mcp"
	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/store"
)

// ShowSynthesisTool implements signatory_show_synthesis — the MCP
// read surface for Layer-3 synthesist output. The synthesist's
// SynthesisSupplement (proposed posture, reasoning, cross-analyst
// concordance/contradictions, weighted citations, gaps, action items)
// lives on analyst_outputs.synthesis_supplement_json. Without this
// tool, agents wanting the synthesist's view have no MCP path —
// signatory_show_conclusions filters out synthesis rows by design
// (synthesists don't emit Conclusion records; those are Layer-2),
// signatory_show_analyses returns row metadata but not the body, and
// signatory_summary aggregates posture+analyses but not the
// reasoning text. The CLI has `signatory show-synthesis <id>` for
// this purpose; this tool is the MCP-side complement.
//
// Input shape: exactly one of `target` or `output_id`. Both omitted
// or both supplied is a CodeSchemaViolation. Both branches end at the
// same response shape so callers don't switch on input form.
type ShowSynthesisTool struct {
	Store store.Store
}

func (t *ShowSynthesisTool) Name() string { return "signatory_show_synthesis" }

func (t *ShowSynthesisTool) Description() string {
	return "Read the synthesist's Layer-3 output for an entity: proposed posture tier, reasoning narrative, cross-analyst concordance and contradictions, weighted citations to specific analyst conclusions, gaps, and action items. USE WHEN you want to know 'what did the synthesist conclude?' rather than 'what did each analyst find?' (signatory_show_conclusions). Two input shapes: pass `target` (canonical URI / shorthand) for the latest synthesis on an entity, or `output_id` for a specific synthesis row. Returns CodeNotFound with `specified synthesis does not exist` when the entity has no synthesis row, the output_id is unknown, or the output_id refers to a Layer-2 analyst output rather than a synthesis."
}

func (t *ShowSynthesisTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"target":    {"type": "string", "description": "Canonical URI, URL, or owner/repo shorthand. Returns the most-recent synthesis output for the resolved entity."},
			"output_id": {"type": "string", "description": "UUID of a specific synthesis row (analyst_outputs.id). Errors if the row isn't a synthesis."}
		},
		"additionalProperties": false
	}`)
}

// showSynthesisInput is the typed input. Both fields are optional at
// the schema layer; the Handle method enforces the "exactly one"
// constraint with explicit CodeSchemaViolation messages.
type showSynthesisInput struct {
	Target   string `json:"target,omitempty"`
	OutputID string `json:"output_id,omitempty"`
}

// ShowSynthesisResponse is the success payload. Mirrors the
// information signatory show-synthesis renders as markdown, but
// structured so MCP callers don't have to parse markdown to extract
// fields.
type ShowSynthesisResponse struct {
	OutputID    string                        `json:"output_id"`
	Entity      ShowSynthesisEntity           `json:"entity"`
	Attribution exchange.AgentAttribution     `json:"attribution"`
	IngestedAt  string                        `json:"ingested_at,omitempty"`
	Target      string                        `json:"target"`
	Supplement  *exchange.SynthesisSupplement `json:"supplement"`
}

// ShowSynthesisEntity is the entity-identity slice the response
// includes. Just enough for an agent to navigate to drill-down
// tools (signatory_show_conclusions etc.) without re-resolving.
type ShowSynthesisEntity struct {
	CanonicalURI string `json:"canonical_uri"`
	ShortName    string `json:"short_name"`
}

// errSynthesisNotFound is the uniform sentinel for every case where
// the requested synthesis simply doesn't exist — output_id unknown,
// output_id refers to a non-synthesis row, target has no synthesis.
// All collapse to the same caller-facing message: "specified synthesis
// does not exist." Per design discussion 2026-04-28: agents handle
// one not-found path; the discriminating detail is in the metadata.
var errSynthesisNotFound = errors.New("specified synthesis does not exist")

func (t *ShowSynthesisTool) Handle(ctx context.Context, raw json.RawMessage) *mcp.Response {
	var in showSynthesisInput
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return mcp.Err(mcp.CodeSchemaViolation, err.Error(), nil)
	}

	switch {
	case in.Target == "" && in.OutputID == "":
		return mcp.Err(mcp.CodeSchemaViolation,
			"exactly one of target or output_id is required",
			map[string]string{"hint": "pass target=<canonical URI> for the latest synthesis on an entity, or output_id=<uuid> for a specific synthesis row"})
	case in.Target != "" && in.OutputID != "":
		return mcp.Err(mcp.CodeSchemaViolation,
			"target and output_id are mutually exclusive — pass exactly one",
			nil)
	case in.OutputID != "":
		return t.handleByOutputID(ctx, in.OutputID)
	default:
		return t.handleByTarget(ctx, in.Target)
	}
}

// handleByOutputID resolves a specific synthesis row. Loads the
// AnalystOutput via GetAnalystOutput; if the row has no
// SynthesisSupplement (i.e. it's a Layer-2 analyst output, or just
// doesn't exist), surfaces the uniform "specified synthesis does
// not exist" error.
func (t *ShowSynthesisTool) handleByOutputID(ctx context.Context, outputID string) *mcp.Response {
	out, err := t.Store.GetAnalystOutput(ctx, outputID)
	if errors.Is(err, store.ErrNotFound) {
		return mcp.Err(mcp.CodeNotFound, errSynthesisNotFound.Error(),
			map[string]string{"output_id": outputID})
	}
	if err != nil {
		return mcp.Err(mcp.CodeInternalError, "load analyst output: "+err.Error(),
			map[string]string{"output_id": outputID})
	}
	if out.SynthesisSupplement == nil {
		return mcp.Err(mcp.CodeNotFound, errSynthesisNotFound.Error(),
			map[string]string{
				"output_id":  outputID,
				"analyst_id": out.Attribution.AnalystID,
				"hint":       "this output_id is a Layer-2 analyst row; synthesis output_ids belong to rows whose analyst_id starts with " + exchange.SynthesistAnalystIDPrefix,
			})
	}

	entity, err := t.Store.GetOutputEntity(ctx, outputID)
	if err != nil {
		return mcp.Err(mcp.CodeInternalError, "load entity for output: "+err.Error(),
			map[string]string{"output_id": outputID})
	}
	return mcp.OK(buildShowSynthesisResponse(outputID, entity, out))
}

// handleByTarget resolves a URI to an entity (via LookupEntity so
// case-fold / pkg:go↔pkg:golang / vanity Go path equivalences work)
// and surfaces the latest synthesis attached to it. Surfaces the
// uniform not-found error when the entity exists with no synthesis,
// or when the URI doesn't resolve to any entity at all. Malformed
// input (URIs that ResolveTarget rejects) comes back as
// CodeSchemaViolation since it's caller error, not a missing-data
// condition.
func (t *ShowSynthesisTool) handleByTarget(ctx context.Context, target string) *mcp.Response {
	entity, err := store.LookupEntity(ctx, t.Store, target)
	if errors.Is(err, store.ErrNotFound) {
		return mcp.Err(mcp.CodeNotFound, errSynthesisNotFound.Error(),
			map[string]string{"target": target, "phase": "entity-lookup"})
	}
	if err != nil {
		// ResolveTarget malformed-input errors come back here. They're
		// schema-class errors from the caller's perspective — the
		// input string didn't parse as any recognized URI form.
		return mcp.Err(mcp.CodeSchemaViolation,
			"target "+target+" did not resolve: "+err.Error(),
			map[string]string{"phase": "resolve"})
	}

	outputID, out, err := t.Store.GetLatestSynthesisForEntity(ctx, entity.ID)
	if errors.Is(err, store.ErrNotFound) {
		return mcp.Err(mcp.CodeNotFound, errSynthesisNotFound.Error(),
			map[string]string{
				"target":        target,
				"canonical_uri": entity.CanonicalURI,
				"hint":          "the entity is in the store, but no synthesis output is attached. Run /analyze or check signatory_show_analyses to see what Layer-2 work exists.",
			})
	}
	if err != nil {
		return mcp.Err(mcp.CodeInternalError, "load latest synthesis: "+err.Error(),
			map[string]string{"canonical_uri": entity.CanonicalURI})
	}
	return mcp.OK(buildShowSynthesisResponse(outputID, entity, out))
}

// buildShowSynthesisResponse composes the response struct from a
// hydrated AnalystOutput plus its entity. Pure function over the
// inputs so both Handle paths can use it identically.
func buildShowSynthesisResponse(outputID string, entity *profile.Entity, out *exchange.AnalystOutput) *ShowSynthesisResponse {
	return &ShowSynthesisResponse{
		OutputID:    outputID,
		Entity:      ShowSynthesisEntity{CanonicalURI: entity.CanonicalURI, ShortName: entity.ShortName},
		Attribution: out.Attribution,
		IngestedAt:  formatIngestedAt(out),
		Target:      out.Target,
		Supplement:  out.SynthesisSupplement,
	}
}

// formatIngestedAt extracts the ingested_at timestamp from the
// AnalystOutput. Today AnalystOutput doesn't carry ingested_at on
// its public surface (the field is internal to the row); we compose
// from Attribution.InvokedAt as the closest analyst-visible time.
// When the analyst-output schema gains a public ingested_at we'll
// route through it directly.
func formatIngestedAt(out *exchange.AnalystOutput) string {
	if out.Attribution.InvokedAt != "" {
		return out.Attribution.InvokedAt
	}
	return time.Now().UTC().Format(time.RFC3339)
}

// Compile-time check that the tool implements the MCP Tool interface.
var _ mcp.Tool = (*ShowSynthesisTool)(nil)
