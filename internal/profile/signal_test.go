package profile

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSignalGroupConstants(t *testing.T) {
	t.Parallel()

	groups := []SignalGroup{
		SignalGroupVitality,
		SignalGroupGovernance,
		SignalGroupPublication,
		SignalGroupHygiene,
		SignalGroupPosture,
		SignalGroupCriticality,
	}

	for _, g := range groups {
		assert.NotEmpty(t, string(g), "SignalGroup constant must not be empty")
	}

	seen := make(map[SignalGroup]bool)
	for _, g := range groups {
		assert.False(t, seen[g], "duplicate SignalGroup: %s", g)
		seen[g] = true
	}
}

func TestForgeryResistanceConstants(t *testing.T) {
	t.Parallel()

	levels := []ForgeryResistance{
		ForgeryVeryHigh,
		ForgeryHigh,
		ForgeryMediumDeclining,
		ForgeryLowDeclining,
	}

	for _, l := range levels {
		assert.NotEmpty(t, string(l), "ForgeryResistance constant must not be empty")
	}

	seen := make(map[ForgeryResistance]bool)
	for _, l := range levels {
		assert.False(t, seen[l], "duplicate ForgeryResistance: %s", l)
		seen[l] = true
	}
}

func TestSignalJSONRoundTrip(t *testing.T) {
	t.Parallel()

	now := time.Date(2024, 7, 1, 12, 0, 0, 0, time.UTC)
	sig := Signal{
		ID:                "sig-abc",
		EntityID:          "ent-123",
		Type:              "scorecard-check",
		Group:             SignalGroupHygiene,
		Source:            "openssf-scorecard",
		ForgeryResistance: ForgeryVeryHigh,
		Value:             json.RawMessage(`{"score":8.5,"checks":["signed-releases","branch-protection"]}`),
		CollectedAt:       now,
		ExpiresAt:         now.Add(7 * 24 * time.Hour),
	}

	data, err := json.Marshal(sig)
	require.NoError(t, err)

	var decoded Signal
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)

	assert.Equal(t, sig.ID, decoded.ID)
	assert.Equal(t, sig.EntityID, decoded.EntityID)
	assert.Equal(t, sig.Type, decoded.Type)
	assert.Equal(t, sig.Group, decoded.Group)
	assert.Equal(t, sig.Source, decoded.Source)
	assert.Equal(t, sig.ForgeryResistance, decoded.ForgeryResistance)
	assert.JSONEq(t, string(sig.Value), string(decoded.Value))
	assert.True(t, sig.CollectedAt.Equal(decoded.CollectedAt))
	assert.True(t, sig.ExpiresAt.Equal(decoded.ExpiresAt))
}

func TestSignalJSON_NullValue(t *testing.T) {
	t.Parallel()

	sig := Signal{
		ID:       "sig-1",
		EntityID: "ent-1",
		Type:     "test",
		Group:    SignalGroupVitality,
		Value:    json.RawMessage(`null`),
	}

	data, err := json.Marshal(sig)
	require.NoError(t, err)

	var decoded Signal
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)

	assert.Equal(t, "null", string(decoded.Value))
}

func TestSignalJSON_ComplexValue(t *testing.T) {
	t.Parallel()

	// Ensure arbitrary nested JSON can survive round-trip via RawMessage.
	complexJSON := `{"nested":{"deep":true},"list":[1,2,3],"str":"hello"}`
	sig := Signal{
		ID:    "sig-complex",
		Value: json.RawMessage(complexJSON),
	}

	data, err := json.Marshal(sig)
	require.NoError(t, err)

	var decoded Signal
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)

	assert.JSONEq(t, complexJSON, string(decoded.Value))
}

func TestSignalJSON_ZeroValue(t *testing.T) {
	t.Parallel()

	var sig Signal
	data, err := json.Marshal(sig)
	require.NoError(t, err)

	var decoded Signal
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)

	assert.Equal(t, sig.ID, decoded.ID)
	assert.Equal(t, sig.Group, decoded.Group)
}
