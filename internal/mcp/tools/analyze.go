// Package tools implements the read-only MCP tool handlers for signatory.
//
// Each tool is a small struct with a Store field (dependency-injected at
// construction). Each file contains: the struct, the mcp.Tool method set,
// a typed input struct (decoded with DisallowUnknownFields for strict-reject),
// and the per-tool output shape. Input/output schemas match
// design/mcp-protocol-envelopes.md exactly.
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
// Returns the cached profile for a target. On cache miss with
// refresh:false returns CodeCacheMissRequiresRefresh. In v0.1 refresh:true
// returns the same cache-miss error rather than performing a live collect —
// the actual refresh is done by signatory_refresh.
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
	Entity            analyzeEntity  `json:"entity"`
	SignalsSummary    signalsSummary `json:"signals_summary"`
	Anomalies         []string       `json:"anomalies"`
	ForgeryResistance string         `json:"forgery_resistance"`
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

type signalsSummary struct {
	Vitality    map[string]interface{} `json:"vitality,omitempty"`
	Governance  map[string]interface{} `json:"governance,omitempty"`
	Criticality map[string]interface{} `json:"criticality,omitempty"`
	Hygiene     map[string]interface{} `json:"hygiene,omitempty"`
	Publication map[string]interface{} `json:"publication,omitempty"`
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
		return mcp.Err(mcp.CodeInternalError, "store lookup failed: "+err.Error(), nil)
	}

	signals, err := t.Store.GetLatestSignals(ctx, entity.ID)
	if err != nil {
		return mcp.Err(mcp.CodeInternalError, "read signals failed: "+err.Error(), nil)
	}
	if len(signals) == 0 {
		if in.Refresh {
			return mcp.Err(mcp.CodeCacheMissRequiresRefresh,
				"entity exists but has no signals; run signatory_refresh to collect",
				map[string]string{"target": in.Target})
		}
		return mcp.Err(mcp.CodeCacheMissRequiresRefresh,
			"no cached signals for "+in.Target+"; run signatory_refresh to collect",
			map[string]string{"target": in.Target})
	}

	postures, err := t.Store.GetPostures(ctx, entity.ID)
	if err != nil {
		return mcp.Err(mcp.CodeInternalError, "read postures failed: "+err.Error(), nil)
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

	summary := buildSignalsSummary(signals)
	forgery := dominantForgeryResistance(signals)

	data := analyzeData{
		Entity:            ent,
		SignalsSummary:    summary,
		Anomalies:         []string{},
		ForgeryResistance: forgery,
	}
	return mcp.OK(data).WithCacheHit(true)
}

// buildSignalsSummary groups the latest signals by group and collapses
// each signal's JSON value into the per-group map so the caller gets a
// flattened summary keyed by signal type.
func buildSignalsSummary(signals []profile.Signal) signalsSummary {
	s := signalsSummary{}
	for _, sig := range signals {
		// Summary-path unmarshal: a corrupt Signal.Value for one
		// signal should not drop the whole summary. On decode failure
		// val stays nil and the explicit nil-guard below substitutes
		// an empty map, which is the documented shape for an unknown
		// or unreadable value. The raw bytes remain in the store for
		// debugging via signatory_signals.
		var val map[string]interface{}
		_ = json.Unmarshal(sig.Value, &val) //nolint:errcheck // see comment above: nil-safe summary on decode failure
		if val == nil {
			val = map[string]interface{}{}
		}
		switch sig.Group {
		case profile.SignalGroupVitality:
			if s.Vitality == nil {
				s.Vitality = map[string]interface{}{}
			}
			s.Vitality[sig.Type] = val
		case profile.SignalGroupGovernance:
			if s.Governance == nil {
				s.Governance = map[string]interface{}{}
			}
			s.Governance[sig.Type] = val
		case profile.SignalGroupCriticality:
			if s.Criticality == nil {
				s.Criticality = map[string]interface{}{}
			}
			s.Criticality[sig.Type] = val
		case profile.SignalGroupHygiene:
			if s.Hygiene == nil {
				s.Hygiene = map[string]interface{}{}
			}
			s.Hygiene[sig.Type] = val
		case profile.SignalGroupPublication:
			if s.Publication == nil {
				s.Publication = map[string]interface{}{}
			}
			s.Publication[sig.Type] = val
		}
	}
	return s
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
