package profile

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostureTierConstants(t *testing.T) {
	t.Parallel()

	tiers := []PostureTier{
		PostureVettedFrozen,
		PostureTrustedForNow,
		PostureUnexamined,
		PostureUnknownProvenance,
	}

	for _, tier := range tiers {
		assert.NotEmpty(t, string(tier), "PostureTier constant must not be empty")
	}

	seen := make(map[PostureTier]bool)
	for _, tier := range tiers {
		assert.False(t, seen[tier], "duplicate PostureTier: %s", tier)
		seen[tier] = true
	}
}

func TestBurnSourceConstants(t *testing.T) {
	t.Parallel()

	sources := []BurnSource{BurnSourceLocal, BurnSourceInherited}
	for _, s := range sources {
		assert.NotEmpty(t, string(s), "BurnSource constant must not be empty")
	}
	assert.NotEqual(t, BurnSourceLocal, BurnSourceInherited)
}

func TestPostureJSONRoundTrip(t *testing.T) {
	t.Parallel()

	now := time.Date(2024, 8, 1, 9, 0, 0, 0, time.UTC)
	posture := Posture{
		EntityID:  "ent-42",
		Tier:      PostureVettedFrozen,
		Version:   "2.3.1",
		Rationale: "Code audit completed by security team",
		SetBy:     "security-bot",
		SetAt:     now,
	}

	data, err := json.Marshal(posture)
	require.NoError(t, err)

	var decoded Posture
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)

	assert.Equal(t, posture.EntityID, decoded.EntityID)
	assert.Equal(t, posture.Tier, decoded.Tier)
	assert.Equal(t, posture.Version, decoded.Version)
	assert.Equal(t, posture.Rationale, decoded.Rationale)
	assert.Equal(t, posture.SetBy, decoded.SetBy)
	assert.True(t, posture.SetAt.Equal(decoded.SetAt))
}

func TestPostureJSON_OmitsEmptyVersion(t *testing.T) {
	t.Parallel()

	posture := Posture{
		EntityID:  "ent-1",
		Tier:      PostureUnexamined,
		Rationale: "not reviewed yet",
		SetBy:     "default",
		SetAt:     time.Now().UTC(),
	}

	data, err := json.Marshal(posture)
	require.NoError(t, err)

	var raw map[string]json.RawMessage
	err = json.Unmarshal(data, &raw)
	require.NoError(t, err)

	_, hasVersion := raw["version"]
	assert.False(t, hasVersion, "empty version should be omitted from JSON")
}

func TestPostureJSON_ZeroValue(t *testing.T) {
	t.Parallel()

	var posture Posture
	data, err := json.Marshal(posture)
	require.NoError(t, err)

	var decoded Posture
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)

	assert.Equal(t, posture, decoded)
}

func TestBurnJSONRoundTrip(t *testing.T) {
	t.Parallel()

	now := time.Date(2024, 9, 15, 14, 30, 0, 0, time.UTC)
	burn := Burn{
		EntityID:  "ent-99",
		Reason:    "backdoor discovered in build script",
		Source:    BurnSourceLocal,
		SourceOrg: "",
		BurnedAt:  now,
		BurnedBy:  "incident-response",
	}

	data, err := json.Marshal(burn)
	require.NoError(t, err)

	var decoded Burn
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)

	assert.Equal(t, burn.EntityID, decoded.EntityID)
	assert.Equal(t, burn.Reason, decoded.Reason)
	assert.Equal(t, burn.Source, decoded.Source)
	assert.Equal(t, burn.BurnedBy, decoded.BurnedBy)
	assert.True(t, burn.BurnedAt.Equal(decoded.BurnedAt))
}

func TestBurnJSON_InheritedWithSourceOrg(t *testing.T) {
	t.Parallel()

	burn := Burn{
		EntityID:  "ent-55",
		Reason:    "upstream advisory",
		Source:    BurnSourceInherited,
		SourceOrg: "CERT-CC",
		BurnedAt:  time.Date(2024, 10, 1, 0, 0, 0, 0, time.UTC),
		BurnedBy:  "auto-sync",
	}

	data, err := json.Marshal(burn)
	require.NoError(t, err)

	var raw map[string]json.RawMessage
	err = json.Unmarshal(data, &raw)
	require.NoError(t, err)

	_, hasSourceOrg := raw["source_org"]
	assert.True(t, hasSourceOrg, "non-empty source_org should be present in JSON")

	var decoded Burn
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)
	assert.Equal(t, "CERT-CC", decoded.SourceOrg)
}

func TestBurnJSON_OmitsEmptySourceOrg(t *testing.T) {
	t.Parallel()

	burn := Burn{
		EntityID: "ent-1",
		Reason:   "test",
		Source:   BurnSourceLocal,
		BurnedAt: time.Now().UTC(),
		BurnedBy: "tester",
	}

	data, err := json.Marshal(burn)
	require.NoError(t, err)

	var raw map[string]json.RawMessage
	err = json.Unmarshal(data, &raw)
	require.NoError(t, err)

	_, hasSourceOrg := raw["source_org"]
	assert.False(t, hasSourceOrg, "empty source_org should be omitted from JSON")
}

func TestBurnJSON_ZeroValue(t *testing.T) {
	t.Parallel()

	var burn Burn
	data, err := json.Marshal(burn)
	require.NoError(t, err)

	var decoded Burn
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)

	assert.Equal(t, burn, decoded)
}
