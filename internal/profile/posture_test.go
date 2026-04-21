package profile

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/sarahmaeve/signatory/internal/exchange"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostureTierConstants(t *testing.T) {
	t.Parallel()

	// The five-tier vocabulary specified in
	// design/trust-policy-v1.md and documented in the synthesis
	// handoff template. Any future addition or rename must update
	// this table, the synthesis template, and the CLI's enum tag
	// in lockstep — drift between them is what a fresh dogfood
	// run caught when the synthesist returned "rejected" (a valid
	// design tier) and the CLI rejected it as unrecognized.
	tiers := []PostureTier{
		PostureVettedFrozen,
		PostureTrustedForNow,
		PostureUnexamined,
		PostureUnknownProvenance,
		PostureRejected,
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

// TestPostureTier_LiteralValues pins the wire-format string for
// each tier. These literals land in the database, the audit log
// detail JSON, and MCP response envelopes — a silent rename would
// break historical queries and any external tooling that pattern-
// matches on the tier string. Renames are allowed, but they must
// change this test in the same commit so the intent is explicit.
func TestPostureTier_LiteralValues(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "vetted-frozen", string(PostureVettedFrozen))
	assert.Equal(t, "trusted-for-now", string(PostureTrustedForNow))
	assert.Equal(t, "unexamined", string(PostureUnexamined))
	assert.Equal(t, "unknown-provenance", string(PostureUnknownProvenance))
	assert.Equal(t, "rejected", string(PostureRejected))
}

// TestPostureTier_ExchangeVocabularyMatches pins the exchange
// package's ProposedPosture tier vocabulary (used by
// SynthesisSupplement.ProposedPosture.Tier, M6a) to the canonical
// PostureTier constants defined here. The exchange package cannot
// import profile without a cycle, so it maintains its own string
// literals — this test is the safety net that catches drift if a
// tier gets added, renamed, or removed in one place but not the
// other. A failure here means the two vocabularies have drifted;
// update both together in the same PR.
func TestPostureTier_ExchangeVocabularyMatches(t *testing.T) {
	t.Parallel()

	// Pin each profile tier to its exchange counterpart by literal
	// string. These must match character-for-character.
	assert.Equal(t, string(PostureVettedFrozen), exchange.ProposedTierVettedFrozen)
	assert.Equal(t, string(PostureTrustedForNow), exchange.ProposedTierTrustedForNow)
	assert.Equal(t, string(PostureUnexamined), exchange.ProposedTierUnexamined)
	assert.Equal(t, string(PostureUnknownProvenance), exchange.ProposedTierUnknownProvenance)
	assert.Equal(t, string(PostureRejected), exchange.ProposedTierRejected)

	// Every profile tier must validate as a proposed-posture tier.
	// If exchange grows a tier that profile doesn't have (or loses
	// one profile still has), ValidProposedPostureTier returns the
	// wrong answer and we fail here.
	for _, tier := range []PostureTier{
		PostureVettedFrozen, PostureTrustedForNow, PostureUnexamined,
		PostureUnknownProvenance, PostureRejected,
	} {
		assert.True(t,
			exchange.ValidProposedPostureTier(string(tier)),
			"profile tier %q must be a valid exchange ProposedPosture tier", tier)
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
