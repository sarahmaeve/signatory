package profile

import "time"

// EntityType classifies what kind of entity a profile describes.
type EntityType string

const (
	EntityProject  EntityType = "project"
	EntityPackage  EntityType = "package"
	EntityIdentity EntityType = "identity"
	EntityPatch    EntityType = "patch"
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
	ID        string     `json:"id"`
	Type      EntityType `json:"type"`
	Name      string     `json:"name"`
	Ecosystem string     `json:"ecosystem,omitempty"` // e.g., "npm", "pypi"
	URL       string     `json:"url,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// Profile is the complete trust profile for an entity, composed of signals.
type Profile struct {
	Entity  Entity      `json:"entity"`
	Signals []Signal    `json:"signals"`
	Posture *Posture    `json:"posture,omitempty"`
	Burn    *Burn       `json:"burn,omitempty"`
	Era     TemporalEra `json:"era,omitempty"`
}
