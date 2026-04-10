package profile

import "time"

// EntityType classifies what kind of entity a profile describes.
type EntityType string

const (
	EntityProject  EntityType = "project"
	EntityPackage  EntityType = "package"
	EntityIdentity EntityType = "identity"
	EntityPatch    EntityType = "patch"
	EntityOrg      EntityType = "org"
)

// TemporalEra classifies when code was produced, affecting signal interpretation.
type TemporalEra string

const (
	EraPreLLM   TemporalEra = "pre-llm"   // Before 30 Nov 2022
	EraEarlyLLM TemporalEra = "early-llm" // 30 Nov 2022 — 24 Nov 2025
	EraModernAI TemporalEra = "modern-ai" // After 24 Nov 2025
)

// Temporal era boundary dates (unexported to prevent runtime manipulation).
var (
	preLLMEnd   = time.Date(2022, 11, 30, 0, 0, 0, 0, time.UTC)
	earlyLLMEnd = time.Date(2025, 11, 24, 0, 0, 0, 0, time.UTC)
)

// PreLLMEnd returns the boundary date before which code is classified as pre-LLM era.
func PreLLMEnd() time.Time { return preLLMEnd }

// EarlyLLMEnd returns the boundary date before which code is classified as early-LLM era.
func EarlyLLMEnd() time.Time { return earlyLLMEnd }

// ClassifyEra returns the temporal era for a given timestamp.
func ClassifyEra(t time.Time) TemporalEra {
	switch {
	case t.Before(preLLMEnd):
		return EraPreLLM
	case t.Before(earlyLLMEnd):
		return EraEarlyLLM
	default:
		return EraModernAI
	}
}

// Entity represents a tracked entity with its profile.
type Entity struct {
	ID           string     `json:"id"`                      // UUID, internal primary key
	CanonicalURI string     `json:"canonical_uri"`           // purl or signatory URI scheme
	Type         EntityType `json:"type"`
	Name         string     `json:"name"`                    // human-friendly short name
	Description  string     `json:"description,omitempty"`
	Ecosystem    string     `json:"ecosystem,omitempty"`
	URL          string     `json:"url,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// Profile is the complete trust profile for an entity, composed of signals.
type Profile struct {
	Entity   Entity      `json:"entity"`
	Signals  []Signal    `json:"signals"`
	Postures []Posture   `json:"postures,omitempty"` // one per version
	Posture  *Posture    `json:"posture,omitempty"`  // latest/default (for backward compat)
	Burn     *Burn       `json:"burn,omitempty"`
	Era      TemporalEra `json:"era,omitempty"`
}

// DependencyObservation records that a project depends on an entity
// at a specific version, as observed during a survey.
type DependencyObservation struct {
	ID         string    `json:"id"`
	ProjectID  string    `json:"project_id"`
	EntityID   string    `json:"entity_id"`
	Version    string    `json:"version"`
	Direct     bool      `json:"direct"`
	ObservedAt time.Time `json:"observed_at"`
	SurveyID   string    `json:"survey_id"`
}

// AuditEntry records a trust-modifying action.
type AuditEntry struct {
	ID        string    `json:"id"`
	Timestamp time.Time `json:"timestamp"`
	Actor     string    `json:"actor"`
	Action    string    `json:"action"`
	EntityID  string    `json:"entity_id,omitempty"`
	Detail    string    `json:"detail"`
}

// TeamIdentity represents a human-LLM team signing identity.
type TeamIdentity struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	CreatedAt    time.Time  `json:"created_at"`
	HaltedAt     *time.Time `json:"halted_at,omitempty"`
	RevokedAt    *time.Time `json:"revoked_at,omitempty"`
	RevokeReason string     `json:"revoke_reason,omitempty"`
}

// SignalResolution records a conflict resolution decision.
type SignalResolution struct {
	ID                 string    `json:"id"`
	EntityID           string    `json:"entity_id"`
	SignalType         string    `json:"signal_type"`
	KeptSignalID       string    `json:"kept_signal_id"`
	SupersededSignalID string    `json:"superseded_signal_id"`
	Action             string    `json:"action"`
	ResolvedBy         string    `json:"resolved_by"`
	ResolvedAt         time.Time `json:"resolved_at"`
}
