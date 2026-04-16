package signal

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/sarahmaeve/signatory/internal/profile"
)

// SignalOrAbsence represents either a collected signal or a recorded
// absence. Absence means "we tried to collect this and couldn't" —
// which is different from "we never tried." In the trust model,
// absence is a negative signal, not neutral.
type SignalOrAbsence struct {
	Signal  *profile.Signal
	Absence *AbsenceRecord
}

// IsAbsence returns true if this is an absence record rather than a signal.
func (s *SignalOrAbsence) IsAbsence() bool {
	return s.Absence != nil
}

// ToSignal converts the SignalOrAbsence to a profile.Signal. For absence
// records, this creates a signal with the absence metadata in the value.
func (s *SignalOrAbsence) ToSignal() profile.Signal {
	if s.Signal != nil {
		return *s.Signal
	}
	if s.Absence != nil {
		return s.Absence.ToSignal()
	}
	return profile.Signal{}
}

// AbsenceRecord documents that a signal was expected but could not be
// collected. This is stored as a signal with type "absence:{signal_type}"
// so it appears in the entity profile and can be surfaced by the MCP.
type AbsenceRecord struct {
	// EntityID is the entity we were collecting for.
	EntityID string

	// SignalType is the signal we tried to collect (e.g., "contributors").
	SignalType string

	// Source is the collector that tried (e.g., "github").
	Source string

	// Reason explains why collection failed.
	Reason string

	// Retryable indicates whether a future attempt might succeed.
	Retryable bool

	// CollectedAt is when the absence was recorded.
	CollectedAt time.Time
}

// ToSignal converts an absence record to a storable signal.
//
// The Group is inherited from the parent type's registry entry so the
// absence signal is categorized alongside real observations of that
// type. If the parent type is not registered, the absence falls back
// to vitality — matching the legacy default — but a registered parent
// is the strongly-preferred path; unregistered types shouldn't be
// producing absences any more than they should be producing signals.
//
// ForgeryResistance is always high for absences, regardless of the
// parent type's rating: the "we tried to collect this and couldn't"
// property is hard to fake independently of whether the underlying
// signal is stars (easy to forge) or commit_signing (hard to forge).
func (a *AbsenceRecord) ToSignal() profile.Signal {
	value, _ := json.Marshal(map[string]interface{}{
		"absent":    true,
		"reason":    a.Reason,
		"retryable": a.Retryable,
	})

	group := profile.SignalGroupVitality
	if info, ok := GetSignalTypeInfo(a.SignalType); ok {
		group = info.Group
	}

	// Signal ID includes collected_at nanos per the v2 spec so that
	// re-runs append instead of colliding with earlier absence records.
	return profile.Signal{
		ID:                fmt.Sprintf("%s:%s:absence:%s:%d", a.Source, a.EntityID, a.SignalType, a.CollectedAt.UnixNano()),
		EntityID:          a.EntityID,
		Type:              "absence:" + a.SignalType,
		Group:             group,
		Source:            a.Source,
		ForgeryResistance: profile.ForgeryHigh, // Absence itself is hard to fake.
		Value:             json.RawMessage(value),
		CollectedAt:       a.CollectedAt,
		ExpiresAt:         a.CollectedAt.Add(1 * time.Hour), // Short TTL — retry sooner.
	}
}

// MakeSignal is a helper to wrap a collected signal.
func MakeSignal(s profile.Signal) SignalOrAbsence {
	return SignalOrAbsence{Signal: &s}
}

// MakeAbsence is a helper to create an absence record.
func MakeAbsence(entityID, signalType, source, reason string, retryable bool, collectedAt time.Time) SignalOrAbsence {
	return SignalOrAbsence{
		Absence: &AbsenceRecord{
			EntityID:    entityID,
			SignalType:  signalType,
			Source:      source,
			Reason:      reason,
			Retryable:   retryable,
			CollectedAt: collectedAt,
		},
	}
}
