package profile

import (
	"encoding/json"
	"time"
)

// SignalGroup categorizes signals by what question they answer.
type SignalGroup string

const (
	SignalGroupVitality    SignalGroup = "vitality"    // "Is anyone home?"
	SignalGroupGovernance  SignalGroup = "governance"  // "Who's responsible?"
	SignalGroupPublication SignalGroup = "publication" // "How was this published?"
	SignalGroupHygiene     SignalGroup = "hygiene"     // "Does it look like they care?"
	SignalGroupPosture     SignalGroup = "posture"     // "What's the consumer's posture?"
	SignalGroupCriticality SignalGroup = "criticality" // "How critical is this?"
)

// ForgeryResistance indicates how difficult a signal is to fake.
type ForgeryResistance string

const (
	ForgeryVeryHigh        ForgeryResistance = "very-high"
	ForgeryHigh            ForgeryResistance = "high"
	ForgeryMediumDeclining ForgeryResistance = "medium-declining"
	ForgeryLowDeclining    ForgeryResistance = "low-declining"
)

// Signal represents a single trust signal collected about an entity.
type Signal struct {
	ID                string            `json:"id"`
	EntityID          string            `json:"entity_id"`
	Type              string            `json:"type"`
	Group             SignalGroup       `json:"group"`
	Source            string            `json:"source"`
	ForgeryResistance ForgeryResistance `json:"forgery_resistance"`
	Value             json.RawMessage   `json:"value"`
	CollectedAt       time.Time         `json:"collected_at"`
	ExpiresAt         time.Time         `json:"expires_at"`
}
