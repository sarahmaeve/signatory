package signal

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/sarahmaeve/signatory/internal/profile"
)

// Make builds a profile.Signal for an observation of signalType, looking
// up Group and ForgeryResistance from the registry.
//
// Returns an error if:
//   - signalType is not registered (programming error — every emitted
//     signal type must have a registry entry; register it before
//     emitting it);
//   - value cannot be JSON-marshalled.
//
// Callers that know the type is registered at build time (most
// collectors) should prefer CollectionResult.RecordSignal, which wraps
// this and promotes the registration error to a panic with context —
// since an unregistered type at runtime is always a bug worth failing
// loudly on.
//
// The ID format matches the v2 entity-model spec:
// {source}:{entityID}:{signalType}:{collectedAtNanos}. The nanos
// suffix makes re-runs append cleanly in the append-only store rather
// than colliding on duplicate IDs.
func Make(entityID, signalType, source string, collectedAt time.Time,
	ttl time.Duration, value any) (profile.Signal, error) {

	info, ok := GetSignalTypeInfo(signalType)
	if !ok {
		return profile.Signal{}, fmt.Errorf(
			"signal.Make: type %q is not registered in the signal type registry — register it in internal/signal/types.go before emitting",
			signalType,
		)
	}

	valueBytes, err := json.Marshal(value)
	if err != nil {
		return profile.Signal{}, fmt.Errorf("signal.Make: marshal value for %q: %w", signalType, err)
	}

	return profile.Signal{
		ID:                fmt.Sprintf("%s:%s:%s:%d", source, entityID, signalType, collectedAt.UnixNano()),
		EntityID:          entityID,
		Type:              signalType,
		Group:             info.Group,
		Source:            source,
		ForgeryResistance: info.ForgeryResistance,
		Value:             json.RawMessage(valueBytes),
		CollectedAt:       collectedAt,
		ExpiresAt:         collectedAt.Add(ttl),
	}, nil
}

// RecordSignal builds a registered signal and appends it to Collected.
//
// Panics on unregistered type or marshal failure. Both conditions are
// programming errors: unregistered types mean a collector is emitting
// a type name the codebase hasn't declared; marshal failure on a
// map[string]any composed of scalars and strings shouldn't happen
// absent an NaN, a cyclic structure, or a custom type with a broken
// MarshalJSON.
//
// Callers that need graceful error handling should use Make directly
// and decide how to present the error.
func (r *CollectionResult) RecordSignal(entityID, signalType, source string,
	collectedAt time.Time, ttl time.Duration, value any) {

	sig, err := Make(entityID, signalType, source, collectedAt, ttl, value)
	if err != nil {
		panic(fmt.Sprintf("RecordSignal: %v", err))
	}
	r.Collected = append(r.Collected, SignalOrAbsence{Signal: &sig})
}

// RecordAbsence appends an absence record to Collected. Use this when a
// collection attempt produced a definitive "this signal doesn't exist"
// answer — distinct from a failure where the collection couldn't be
// attempted. If you have an upstream error, use RecordFailure instead,
// which records both the absence AND tracks the failure for retry
// reporting.
func (r *CollectionResult) RecordAbsence(entityID, signalType, source, reason string,
	retryable bool, collectedAt time.Time) {

	r.Collected = append(r.Collected, MakeAbsence(entityID, signalType, source, reason, retryable, collectedAt))
}

// RecordFailure records a collection failure: it appends an absence
// signal to Collected (so the absence is visible in the entity profile)
// AND appends a CollectionError to Failures (so the run summary knows
// what didn't work). This is the pattern every collector uses on
// upstream API errors.
//
// The split matters: Collected is what gets stored long-term; Failures
// is the short-lived run diagnostic. Before this helper, every sub-
// collector hand-rolled the "append to both" pair, and it was easy to
// forget one half.
func (r *CollectionResult) RecordFailure(entityID, signalType, source, reason string,
	retryable bool, collectedAt time.Time) {

	r.RecordAbsence(entityID, signalType, source, reason, retryable, collectedAt)
	r.Failures = append(r.Failures, CollectionError{
		SignalType: signalType,
		Source:     source,
		Reason:     reason,
		Retryable:  retryable,
	})
}
