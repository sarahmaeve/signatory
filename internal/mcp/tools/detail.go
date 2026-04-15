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

// DetailTool implements signatory_detail.
//
// Drills into one signal group for an entity. The signal_group input must
// be one of the six canonical values; anything else is a strict-reject
// schema violation so the caller knows immediately what the valid values
// are.
type DetailTool struct {
	Store store.Store
}

func (t *DetailTool) Name() string { return "signatory_detail" }

func (t *DetailTool) Description() string {
	return "USE THIS when the user asks about one specific *dimension* of an entity's trust (e.g. 'what's the vitality of X?', 'show me the hygiene signals for Y', 'how does Z's governance look?'). Scopes to one signal group at a time: vitality, governance, publication, hygiene, posture, or criticality. Use signatory_signals instead when the user wants every group at once."
}

func (t *DetailTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"target": {
				"type": "string",
				"description": "Canonical URI, URL, or owner/repo shorthand"
			},
			"signal_group": {
				"type": "string",
				"enum": ["vitality", "governance", "publication", "hygiene", "criticality", "history"],
				"description": "Signal group to drill into"
			}
		},
		"required": ["target", "signal_group"],
		"additionalProperties": false
	}`)
}

// detailInput is the typed input for signatory_detail.
type detailInput struct {
	Target      string `json:"target"`
	SignalGroup string `json:"signal_group"`
}

// validSignalGroups is the enum set for signal_group. "history" maps to the
// posture group — it's the historical-decision lens into a target.
// The spec lists "history" as a valid input enum; internally posture is the
// nearest group, and the tool returns signals from SignalGroupPosture for it.
var validSignalGroups = map[string]profile.SignalGroup{
	"vitality":    profile.SignalGroupVitality,
	"governance":  profile.SignalGroupGovernance,
	"publication": profile.SignalGroupPublication,
	"hygiene":     profile.SignalGroupHygiene,
	"criticality": profile.SignalGroupCriticality,
	"history":     profile.SignalGroupPosture,
}

// Compile-time interface check.
var _ mcp.Tool = (*DetailTool)(nil)

func (t *DetailTool) Handle(ctx context.Context, raw json.RawMessage) *mcp.Response {
	var in detailInput
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return mcp.Err(mcp.CodeSchemaViolation, err.Error(), nil)
	}
	if in.Target == "" {
		return mcp.Err(mcp.CodeSchemaViolation, "target is required", nil)
	}
	if in.SignalGroup == "" {
		return mcp.Err(mcp.CodeSchemaViolation, "signal_group is required", nil)
	}

	group, ok := validSignalGroups[in.SignalGroup]
	if !ok {
		return mcp.Err(mcp.CodeSchemaViolation,
			`signal_group must be one of: vitality, governance, publication, hygiene, criticality, history`,
			map[string]string{
				"field":        "signal_group",
				"received":     in.SignalGroup,
				"valid_values": "vitality, governance, publication, hygiene, criticality, history",
			})
	}

	canonicalURI, _, _, normErr := profile.NormalizeGitHubRepoInput(in.Target)
	if normErr != nil {
		canonicalURI = in.Target
	}

	entity, err := t.Store.FindEntityByURI(ctx, canonicalURI)
	if errors.Is(err, store.ErrNotFound) {
		return mcp.Err(mcp.CodeNotFound,
			"entity not found: "+in.Target,
			map[string]string{"target": in.Target})
	}
	if err != nil {
		return mcp.Err(mcp.CodeInternalError, "store lookup failed: "+err.Error(), nil)
	}

	signals, err := t.Store.GetSignalsByGroup(ctx, entity.ID, group)
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

	return mcp.OK(map[string]interface{}{
		"target":       entity.CanonicalURI,
		"signal_group": in.SignalGroup,
		"signals":      records,
	})
}
