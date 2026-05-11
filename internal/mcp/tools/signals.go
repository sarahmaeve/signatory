package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/sarahmaeve/signatory/internal/mcp"
	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/store"
)

// SignalsTool implements signatory_signals.
//
// Returns the full raw signal records (Layer 1) for a target — no
// summarization, no LLM pass. Distinct from signatory_analyze which
// returns a summarised profile; this tool is "show me everything the
// collectors wrote."
type SignalsTool struct {
	Store store.Store
}

func (t *SignalsTool) Name() string { return "signatory_signals" }

func (t *SignalsTool) Description() string {
	return "USE THIS when the user wants the raw, unfiltered trust-signal evidence for a target — usually to see what's behind an assessment, to spot-check data, or to compare signals across targets. Returns every signal record for one entity. Prefer signatory_analyze for a summarised profile; this tool is the 'show me everything' option."
}

func (t *SignalsTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"target": {"type": "string", "description": "Canonical URI, URL, or owner/repo shorthand"}
		},
		"required": ["target"],
		"additionalProperties": false
	}`)
}

// signalsInput is the typed input for signatory_signals.
type signalsInput struct {
	Target string `json:"target"`
}

// signalRecord is the wire shape of a single signal returned by this tool.
// It matches the signals table shape plus parsed details.
type signalRecord struct {
	ID                string          `json:"id"`
	EntityID          string          `json:"entity_id"`
	Type              string          `json:"type"`
	Group             string          `json:"group"`
	Source            string          `json:"source"`
	ForgeryResistance string          `json:"forgery_resistance"`
	Value             json.RawMessage `json:"value"`
	CollectedAt       string          `json:"collected_at"`
	ExpiresAt         string          `json:"expires_at"`
}

// Compile-time interface check.
var _ mcp.Tool = (*SignalsTool)(nil)

func (t *SignalsTool) Handle(ctx context.Context, raw json.RawMessage) *mcp.Response {
	var in signalsInput
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return mcp.Err(mcp.CodeSchemaViolation, err.Error(), nil)
	}
	if in.Target == "" {
		return mcp.Err(mcp.CodeSchemaViolation, "target is required", nil)
	}

	// Multi-forge target resolution. ResolveTarget handles every
	// canonical URI scheme (pkg:, repo:, identity:, org:, patch:),
	// every recognized forge URL (github / codeberg / gitlab), every
	// recognized registry URL (npmjs.com, pypi.org, crates.io, …),
	// and bare github shorthand. Replaces the pre-2026-05-10
	// NormalizeGitHubRepoInput call which permissively prefix-stripped
	// any URL and silently misclassified codeberg/gitlab targets as
	// repo:github/<host>/<owner> — same misclassification
	// rejectUnrecognizedForgeURL guards against on the analyze path.
	// Mirrors summary.go:64-69.
	resolved, resolveErr := profile.ResolveTarget(in.Target)
	if resolveErr != nil {
		return mcp.Err(mcp.CodeSchemaViolation,
			"target "+in.Target+" does not resolve to a canonical URI: "+resolveErr.Error(),
			map[string]string{"target": in.Target, "phase": "resolve"})
	}
	canonicalURI := resolved.CanonicalURI

	entity, err := t.Store.FindEntityByURI(ctx, canonicalURI)
	if errors.Is(err, store.ErrNotFound) {
		return mcp.Err(mcp.CodeNotFound,
			"entity not found: "+in.Target,
			map[string]string{"target": in.Target})
	}
	if err != nil {
		return mcp.Err(mcp.CodeInternalError, "store lookup failed: "+err.Error(), nil)
	}

	signals, err := t.Store.GetLatestSignals(ctx, entity.ID)
	if err != nil {
		return mcp.Err(mcp.CodeInternalError, "read signals failed: "+err.Error(), nil)
	}

	records := make([]signalRecord, 0, len(signals))
	for _, s := range signals {
		records = append(records, signalRecord{
			ID:                s.ID,
			EntityID:          s.EntityID,
			Type:              s.Type,
			Group:             string(s.Group),
			Source:            s.Source,
			ForgeryResistance: string(s.ForgeryResistance),
			Value:             s.Value,
			CollectedAt:       s.CollectedAt.Format("2006-01-02T15:04:05Z07:00"),
			ExpiresAt:         s.ExpiresAt.Format("2006-01-02T15:04:05Z07:00"),
		})
	}

	return mcp.OK(map[string]any{
		"target":  entity.CanonicalURI,
		"signals": records,
	}).WithCacheHit(len(records) > 0)
}
