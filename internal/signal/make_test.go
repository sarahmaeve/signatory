package signal

import (
	"encoding/json"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
)

func TestMake_RegisteredTypeProducesSignalWithRegistryMetadata(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 4, 16, 12, 0, 0, 0, time.UTC)
	sig, err := Make("entity-1", "stars", "github", now, 24*time.Hour,
		map[string]any{"count": 29152})

	require.NoError(t, err)

	info, _ := GetSignalTypeInfo("stars")
	assert.Equal(t, "entity-1", sig.EntityID)
	assert.Equal(t, "stars", sig.Type)
	assert.Equal(t, info.Group, sig.Group, "Make must inherit Group from registry")
	assert.Equal(t, info.ForgeryResistance, sig.ForgeryResistance, "Make must inherit ForgeryResistance from registry")
	assert.Equal(t, "github", sig.Source)
	assert.Equal(t, now, sig.CollectedAt)
	assert.Equal(t, now.Add(24*time.Hour), sig.ExpiresAt)

	var v map[string]any
	require.NoError(t, json.Unmarshal(sig.Value, &v))
	assert.Equal(t, float64(29152), v["count"])
}

// TestMake_IDFormatMatchesEntityModelV2 — the ID format is contractual
// with the append-only store: {source}:{entityID}:{signalType}:{nanos}.
// Changing it risks collision with existing stored IDs.
func TestMake_IDFormatMatchesEntityModelV2(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 4, 16, 12, 0, 0, 123456789, time.UTC)
	sig, err := Make("ent-1", "stars", "github", now, time.Hour, map[string]any{"count": 1})
	require.NoError(t, err)

	// Format: source:entityID:signalType:nanos
	// Derive the nano suffix from the time value itself rather than
	// hardcoding — makes the test robust to timezone/epoch arithmetic
	// mistakes in the test itself.
	expected := fmt.Sprintf("github:ent-1:stars:%d", now.UnixNano())
	assert.Equal(t, expected, sig.ID,
		"ID must be {source}:{entityID}:{signalType}:{collectedAt.UnixNano()}")
}

// TestMake_IDIncludesNanos_EnablesAppendOnlyReruns — the nano-precision
// ID is what allows two collection runs at the "same" second to produce
// distinct IDs. Before this, the store's append-only trigger would
// reject the second run. The test pins the behavior so a future "let's
// drop nanos for brevity" refactor fails loudly.
func TestMake_IDIncludesNanos_EnablesAppendOnlyReruns(t *testing.T) {
	t.Parallel()

	t1 := time.Date(2026, 4, 16, 12, 0, 0, 1, time.UTC)
	t2 := time.Date(2026, 4, 16, 12, 0, 0, 2, time.UTC)

	sig1, err := Make("ent-1", "stars", "github", t1, time.Hour, map[string]any{"count": 1})
	require.NoError(t, err)
	sig2, err := Make("ent-1", "stars", "github", t2, time.Hour, map[string]any{"count": 1})
	require.NoError(t, err)

	assert.NotEqual(t, sig1.ID, sig2.ID, "nano-separated collections must produce distinct IDs")
}

func TestMake_UnregisteredTypeReturnsError(t *testing.T) {
	t.Parallel()

	_, err := Make("entity-1", "fictional_signal_type", "github",
		time.Now(), time.Hour, map[string]any{"x": 1})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "not registered",
		"error must explain the cause so callers can remediate")
	assert.Contains(t, err.Error(), "fictional_signal_type",
		"error must name the offending type")
}

// TestMake_UnmarshallableValueReturnsError — a value that JSON can't
// represent (NaN, +Inf, channel, function) must produce an error, not
// a panic, and not a silently-truncated zero-value signal.
func TestMake_UnmarshallableValueReturnsError(t *testing.T) {
	t.Parallel()

	_, err := Make("entity-1", "stars", "github",
		time.Now(), time.Hour, map[string]any{"count": math.NaN()})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "marshal",
		"error must identify the marshalling stage")
}

func TestRecordSignal_AppendsToCollected(t *testing.T) {
	t.Parallel()

	var result CollectionResult
	now := time.Now().UTC()

	result.RecordSignal("entity-1", "stars", "github", now, time.Hour,
		map[string]any{"count": 100})

	require.Len(t, result.Collected, 1)
	assert.False(t, result.Collected[0].IsAbsence())
	assert.Equal(t, "stars", result.Collected[0].Signal.Type)
	assert.Equal(t, profile.SignalGroupCriticality, result.Collected[0].Signal.Group,
		"RecordSignal must flow registry Group through to the stored signal")
}

// TestRecordSignal_UnregisteredTypePanics — unregistered types at
// runtime are a programming error; the panic is intentional. The
// message must name the offending type so debugging is immediate.
func TestRecordSignal_UnregisteredTypePanics(t *testing.T) {
	t.Parallel()

	var result CollectionResult
	assert.PanicsWithValue(t,
		"RecordSignal: signal.Make: type \"bogus_type\" is not registered in the signal type registry — register it in internal/signal/types.go before emitting",
		func() {
			result.RecordSignal("entity-1", "bogus_type", "github",
				time.Now(), time.Hour, map[string]any{})
		},
		"panic message must include the exact unregistered type name")
}

func TestRecordAbsence_AppendsAbsenceOnly(t *testing.T) {
	t.Parallel()

	var result CollectionResult
	now := time.Now().UTC()

	result.RecordAbsence("entity-1", "stars", "github", "rate limited", true, now)

	require.Len(t, result.Collected, 1)
	assert.True(t, result.Collected[0].IsAbsence())
	assert.Empty(t, result.Failures,
		"RecordAbsence must NOT append to Failures — that's RecordFailure's job")
}

func TestRecordFailure_AppendsBothAbsenceAndFailure(t *testing.T) {
	t.Parallel()

	var result CollectionResult
	now := time.Now().UTC()

	result.RecordFailure("entity-1", "contributors", "github", "rate limited", true, now)

	// The absence half.
	require.Len(t, result.Collected, 1, "RecordFailure must append an absence to Collected")
	assert.True(t, result.Collected[0].IsAbsence())

	// The failure half.
	require.Len(t, result.Failures, 1, "RecordFailure must append a CollectionError to Failures")
	assert.Equal(t, "contributors", result.Failures[0].SignalType)
	assert.Equal(t, "github", result.Failures[0].Source)
	assert.Equal(t, "rate limited", result.Failures[0].Reason)
	assert.True(t, result.Failures[0].Retryable)
}

// TestRecordFailure_NonRetryable_PropagatesFlag — the retryable flag
// flows to both halves: the absence record (so cache TTL can be tuned
// by consumers) and the CollectionError (so the run summary can
// distinguish "we'll try again" from "it's not coming back").
func TestRecordFailure_NonRetryable_PropagatesFlag(t *testing.T) {
	t.Parallel()

	var result CollectionResult
	now := time.Now().UTC()

	result.RecordFailure("entity-1", "contributors", "github", "not found", false, now)

	require.Len(t, result.Failures, 1)
	assert.False(t, result.Failures[0].Retryable,
		"non-retryable flag must flow through to CollectionError")

	var val map[string]any
	require.NoError(t, json.Unmarshal(result.Collected[0].ToSignal().Value, &val))
	assert.Equal(t, false, val["retryable"],
		"non-retryable flag must flow through to the absence record")
}
