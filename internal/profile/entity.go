package profile

import (
	"crypto/rand"
	"fmt"
	"strings"
	"time"
)

// NewEntityID generates a random UUIDv4-style string for use as an
// Entity.ID. The v2 model uses opaque UUIDs as the internal primary
// key so that renaming or re-normalizing an entity's canonical URI
// doesn't require cascading FK updates across signals, postures,
// burns, dependency observations, audit log, and resolutions.
//
// Format: 8-4-4-4-12 hex, RFC 4122 version 4 variant. On the
// astronomically unlikely event that crypto/rand.Read fails, this
// panics — the caller is creating a new entity and cannot proceed
// without an ID, so there is no meaningful fallback.
func NewEntityID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("profile: crypto/rand.Read failed: %v", err))
	}
	b[6] = (b[6] & 0x0f) | 0x40 // Version 4.
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant.
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// EntityType classifies what kind of entity a profile describes.
type EntityType string

const (
	EntityProject  EntityType = "project"
	EntityPackage  EntityType = "package"
	EntityIdentity EntityType = "identity"
	EntityPatch    EntityType = "patch"
	EntityOrg      EntityType = "org"
)

// EntityTypeForScheme maps a canonical-URI scheme name (without the
// trailing colon — "repo", "pkg", "identity", "org", "patch") to the
// EntityType that should be stored on the entity row.
//
// This is the single source of truth for the scheme→type mapping.
// Callers that mint entity rows (analyze, posture, analyst-output
// ingest, EnsureEntityByCanonicalURI) MUST route through here so the
// stored type cannot drift from the canonical URI.
//
// Unknown schemes return EntityPackage as the least-surprising
// fallback — purl-style identifiers are the broadest scheme. Callers
// upstream of here (ResolveTarget, ValidateCanonicalURI) constrain
// the recognized set; this helper stays total to keep the persistence
// boundary defensive against future code paths that forget to validate
// before passing a scheme through.
//
// History: previously lived as cmd/signatory/posture.go's package-
// private entityTypeForScheme. Hoisted to internal/profile/ so
// store-layer code (internal/store/analyst_output.go) can call it
// without an import-cycle violation. Replaces a hardcoded
// Type=EntityProject in the analyst-output ingest path that produced
// mistyped pkg: rows for every analyst output ingested.
func EntityTypeForScheme(scheme string) EntityType {
	switch scheme {
	case "repo":
		return EntityProject
	case "pkg":
		return EntityPackage
	case "identity":
		return EntityIdentity
	case "org":
		return EntityOrg
	case "patch":
		return EntityPatch
	default:
		return EntityPackage
	}
}

// EntityTypeForURI extracts the scheme from a canonical URI and returns
// the matching EntityType. Convenience wrapper over EntityTypeForScheme
// for callers that have a full URI in hand and don't want to extract
// the scheme themselves.
//
// Defensive: malformed input (no colon, leading colon, empty string)
// falls through to EntityTypeForScheme's unknown-scheme branch, which
// returns EntityPackage. Callers that need strict validation should
// use ValidateCanonicalURI separately; this helper's contract is
// "always return a usable EntityType."
func EntityTypeForURI(uri string) EntityType {
	idx := strings.Index(uri, ":")
	if idx <= 0 {
		return EntityPackage
	}
	return EntityTypeForScheme(uri[:idx])
}

// TemporalEra classifies when code was produced, affecting signal interpretation.
type TemporalEra string

const (
	EraPreLLM   TemporalEra = "pre-llm"   // Before 30 Nov 2022
	EraEarlyLLM TemporalEra = "early-llm" // 30 Nov 2022 — 24 Nov 2025
	EraModernAI TemporalEra = "modern-ai" // 24 Nov 2025 — 30 Apr 2026

	// EraMatureCyber labels code touched after multi-vendor frontier
	// cyber capability is established. The era is a notation for
	// analysis: code unexamined and unrepaired from before this phase
	// is likely to have surfaces that will not weather current attacks
	// well. The boundary is not a claim about authorship capability —
	// it is a claim about target durability.
	EraMatureCyber TemporalEra = "mature-cyber" // After 30 Apr 2026
)

// Temporal era boundary dates (unexported to prevent runtime manipulation).
var (
	preLLMEnd   = time.Date(2022, 11, 30, 0, 0, 0, 0, time.UTC)
	earlyLLMEnd = time.Date(2025, 11, 24, 0, 0, 0, 0, time.UTC)
	modernAIEnd = time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC)
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
	case t.Before(modernAIEnd):
		return EraModernAI
	default:
		return EraMatureCyber
	}
}

// Entity represents a tracked entity with its profile.
//
// The v2 model distinguishes three string-valued identifiers that are easy
// to confuse:
//
//   - ID           — opaque UUID, internal primary key, stable forever,
//     never shown to humans, used as the FK target everywhere.
//   - CanonicalURI — external parseable identifier (purl for packages,
//     signatory scheme `{type}:{platform}/{path}` for repos,
//     identities, orgs, patches). Unique per entity.
//   - ShortName    — human-friendly display label, e.g. "Kong".
//
// Description is a one-line context string ("Go CLI argument parser"),
// separate from ShortName, populated from API data or user-edited project
// notes. See design/entity-model-v2.md for the full model.
type Entity struct {
	ID           string     `json:"id"`            // UUID, internal primary key
	CanonicalURI string     `json:"canonical_uri"` // purl or signatory URI scheme
	Type         EntityType `json:"type"`
	ShortName    string     `json:"short_name"` // human-friendly display label
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
