// Package tools implements the read-only MCP tool handlers for signatory.
//
// Each tool is a small struct with a Store field (dependency-injected at
// construction). Each file contains: the struct, the mcp.Tool method set,
// a typed input struct (decoded with DisallowUnknownFields for strict-reject),
// and the per-tool output shape.
//
// Convention: all Handle implementations return mcp.Err on decode failure,
// required-field absence, or unrecognised enum values; mcp.OK wraps the
// tool-specific payload on success.
package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/sarahmaeve/signatory/internal/mcp"
	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/store"
)

// AnalyzeTool implements signatory_analyze.
//
// Returns the cached profile for a target. CodeCacheMissRequiresRefresh
// fires only when the entity itself isn't in the store — i.e. no
// analyst (manual or automated) has ever assessed this target. An
// entity that exists but has no Layer 1 signals (the natural state
// of any target processed via the /analyze skill, which dispatches
// analyst agents without invoking signal collectors) returns OK
// with an empty SignalsSummary. The tool's response shape carries
// both Layer 1 (signals_summary, forgery_resistance) and Layer 2
// (entity.posture) data; either may be empty independently. Layer 1
// refresh is the job of signatory_refresh — refresh:true on this
// tool is informational only in v0.1 and does not affect the
// returned data.
type AnalyzeTool struct {
	Store store.Store
}

func (t *AnalyzeTool) Name() string { return "signatory_analyze" }

func (t *AnalyzeTool) Description() string {
	return "USE THIS when the user asks about the trust posture, safety, or security of a specific package or repo (e.g. 'is X safe?', 'can I trust Y?', 'what's the assessment of Z?'). Returns the cached trust profile (Layer 1 signals + this entity's Layer 2 trust decision) for one target. For a store-wide posture overview ('how many deps have I assessed?', 'show me everything vetted-frozen'), read signatory://posture instead — this tool is entity-scoped. Prefer signatory_signals when the user wants raw evidence instead of a summary. Returns NotFound if the target hasn't been analyzed — that itself is informative: it means no analyst has assessed this target yet."
}

func (t *AnalyzeTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"target":  {"type": "string", "description": "Canonical URI, URL, or owner/repo shorthand"},
			"depth":   {"type": "string", "enum": ["provenance", "signals"], "default": "provenance"},
			"refresh": {"type": "boolean", "default": false}
		},
		"required": ["target"],
		"additionalProperties": false
	}`)
}

// analyzeInput is the typed input for signatory_analyze.
type analyzeInput struct {
	Target  string `json:"target"`
	Depth   string `json:"depth,omitempty"`
	Refresh bool   `json:"refresh,omitempty"`
}

// analyzeData is the output payload for signatory_analyze.
type analyzeData struct {
	Entity            analyzeEntity          `json:"entity"`
	SignalsSummary    profile.SignalsSummary `json:"signals_summary"`
	Anomalies         []string               `json:"anomalies"`
	ForgeryResistance string                 `json:"forgery_resistance"`
}

type analyzeEntity struct {
	CanonicalURI string          `json:"canonical_uri"`
	ShortName    string          `json:"short_name"`
	EntityType   string          `json:"entity_type"`
	TemporalEra  string          `json:"temporal_era,omitempty"`
	Posture      *analyzePosture `json:"posture,omitempty"`
}

type analyzePosture struct {
	Tier  string `json:"tier"`
	SetAt string `json:"set_at"`
}

func (t *AnalyzeTool) Handle(ctx context.Context, raw json.RawMessage) *mcp.Response {
	var in analyzeInput
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return mcp.Err(mcp.CodeSchemaViolation, err.Error(), nil)
	}
	if in.Target == "" {
		return mcp.Err(mcp.CodeSchemaViolation, "target is required", nil)
	}
	if in.Depth != "" && in.Depth != "provenance" && in.Depth != "signals" {
		return mcp.Err(mcp.CodeSchemaViolation, `depth must be "provenance" or "signals"`, map[string]string{"field": "depth"})
	}

	// Normalize the target to a canonical URI.
	canonicalURI, _, _, normErr := profile.NormalizeGitHubRepoInput(in.Target)
	if normErr != nil {
		// Fall back to raw target in case it is already a canonical URI.
		canonicalURI = in.Target
	}

	entity, err := t.Store.FindEntityByURI(ctx, canonicalURI)
	if errors.Is(err, store.ErrNotFound) {
		return mcp.Err(mcp.CodeCacheMissRequiresRefresh,
			"no cached data for "+in.Target+"; run signatory_refresh to collect signals",
			map[string]string{"target": in.Target})
	}
	if err != nil {
		// Internal store/driver errors do not cross the MCP boundary.
		// The agent caller has no use for SQLite text and we don't
		// want DB paths or schema topology in transcripts. Operation
		// context goes in the structured details map.
		return mcp.Err(mcp.CodeInternalError, "store lookup failed",
			map[string]string{"operation": "FindEntityByURI", "target": in.Target})
	}

	signals, err := t.Store.GetLatestSignals(ctx, entity.ID)
	if err != nil {
		return mcp.Err(mcp.CodeInternalError, "read signals failed",
			map[string]string{"operation": "GetLatestSignals", "entity_id": entity.ID})
	}
	// Empty signals are NOT a cache miss. The /analyze skill produces
	// entities with Layer 2 conclusions/posture/synthesis but no
	// Layer 1 signals (skill dispatches analysts; doesn't invoke
	// signal collectors). Returning an error here would lie about
	// the data being absent and point users at signatory_refresh,
	// which is the wrong remedy when what they want is the Layer 2
	// verdict. Empty signals → empty SignalsSummary in the response,
	// which is honest about the cache state.

	postures, err := t.Store.GetPostures(ctx, entity.ID)
	if err != nil {
		return mcp.Err(mcp.CodeInternalError, "read postures failed",
			map[string]string{"operation": "GetPostures", "entity_id": entity.ID})
	}

	ent := analyzeEntity{
		CanonicalURI: entity.CanonicalURI,
		ShortName:    entity.ShortName,
		EntityType:   string(entity.Type),
	}
	if len(postures) > 0 {
		p := postures[0]
		ent.Posture = &analyzePosture{
			Tier:  string(p.Tier),
			SetAt: p.SetAt.Format("2006-01-02T15:04:05Z07:00"),
		}
	}

	summary := profile.Summarize(signals)
	forgery := dominantForgeryResistance(signals)

	data := analyzeData{
		Entity:            ent,
		SignalsSummary:    summary,
		Anomalies:         []string{},
		ForgeryResistance: forgery,
	}
	return mcp.OK(data).WithCacheHit(true)
}

// forgeryResistanceRank returns a numeric rank for ordering (higher is better)
// and whether the value was a known enum member. Callers must check the bool;
// rank 0 with known==false means "skip this value" rather than "weakest."
func forgeryResistanceRank(fr profile.ForgeryResistance) (rank int, known bool) {
	switch fr {
	case profile.ForgeryVeryHigh:
		return 4, true
	case profile.ForgeryHigh:
		return 3, true
	case profile.ForgeryMediumDeclining:
		return 2, true
	case profile.ForgeryLowDeclining:
		return 1, true
	}
	slog.Warn("analyze: unknown forgery_resistance value", "value", string(fr))
	return 0, false
}

// Compile-time interface checks — fail here if a tool stops satisfying mcp.Tool.
var _ mcp.Tool = (*AnalyzeTool)(nil)

// dominantForgeryResistance returns the minimum (weakest) forgery resistance
// across all signals — the overall posture is only as strong as its weakest
// signal. Unknown ForgeryResistance values are skipped so that a new or
// unrecognised enum member cannot silently drag the result to a worse tier
// than the known signals justify. If every signal carries an unknown value,
// "" is returned (same as the empty-signals case).
func dominantForgeryResistance(signals []profile.Signal) string {
	worstRank := -1
	var worst profile.ForgeryResistance
	for _, s := range signals {
		rank, known := forgeryResistanceRank(s.ForgeryResistance)
		if !known {
			continue
		}
		if worstRank < 0 || rank < worstRank {
			worstRank = rank
			worst = s.ForgeryResistance
		}
	}
	if worstRank < 0 {
		return ""
	}
	return string(worst)
}
