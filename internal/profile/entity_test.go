package profile

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEntityTypeConstants(t *testing.T) {
	t.Parallel()

	types := []EntityType{EntityProject, EntityPackage, EntityIdentity, EntityPatch}
	for _, et := range types {
		assert.NotEmpty(t, string(et), "EntityType constant must not be empty")
	}

	// Ensure all values are distinct.
	seen := make(map[EntityType]bool)
	for _, et := range types {
		assert.False(t, seen[et], "duplicate EntityType: %s", et)
		seen[et] = true
	}
}

// TestEntityTypeForScheme covers the scheme→EntityType mapping. Kept as
// a single source of truth so callers (analyze, posture, analyst-output
// ingest, future EnsureEntityByCanonicalURI) cannot drift out of sync.
//
// The mapping mirrors the URI scheme constants in uri.go. Unknown
// schemes fall back to EntityPackage — the least-surprising default,
// matching the contract previously held by cmd/signatory/posture.go's
// package-private entityTypeForScheme.
func TestEntityTypeForScheme(t *testing.T) {
	t.Parallel()

	cases := []struct {
		scheme string
		want   EntityType
	}{
		{"repo", EntityProject},
		{"pkg", EntityPackage},
		{"identity", EntityIdentity},
		{"org", EntityOrg},
		{"patch", EntityPatch},
	}
	for _, tc := range cases {
		t.Run(tc.scheme, func(t *testing.T) {
			t.Parallel()
			got := EntityTypeForScheme(tc.scheme)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestEntityTypeForScheme_Unknown documents the defensive fallback for
// schemes outside the recognized set. ResolveTarget normally constrains
// the set upstream, but the helper is the persistence-boundary safety
// net — return EntityPackage rather than panic so the store stays
// permissive at the boundary.
func TestEntityTypeForScheme_Unknown(t *testing.T) {
	t.Parallel()

	got := EntityTypeForScheme("bogus")
	assert.Equal(t, EntityPackage, got, "unknown scheme must fall back to EntityPackage")

	got = EntityTypeForScheme("")
	assert.Equal(t, EntityPackage, got, "empty scheme must fall back to EntityPackage")
}

// TestEntityTypeForURI covers the convenience wrapper that takes a full
// canonical URI and returns the EntityType for its scheme. This is the
// form most callers want — analyst-output ingest, in particular, has a
// URI in hand and shouldn't have to extract the scheme manually.
func TestEntityTypeForURI(t *testing.T) {
	t.Parallel()

	cases := []struct {
		uri  string
		want EntityType
	}{
		{"repo:github/alecthomas/kong", EntityProject},
		{"pkg:npm/lodash", EntityPackage},
		{"pkg:npm/@stripe/stripe-react-native", EntityPackage},
		{"pkg:cargo/atuin", EntityPackage},
		{"pkg:golang/github.com/alecthomas/kong", EntityPackage},
		{"pkg:pypi/click", EntityPackage},
		{"identity:github/bufferzonecorp", EntityIdentity},
		{"org:github/vitest-dev", EntityOrg},
		{"patch:github/foo/bar/123", EntityPatch},
	}
	for _, tc := range cases {
		t.Run(tc.uri, func(t *testing.T) {
			t.Parallel()
			got := EntityTypeForURI(tc.uri)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestEntityTypeForURI_Malformed documents the defensive fallback for
// URIs without a recognizable scheme prefix. ValidateCanonicalURI catches
// these upstream in production paths, but the helper is total — malformed
// input returns EntityPackage rather than panic.
func TestEntityTypeForURI_Malformed(t *testing.T) {
	t.Parallel()

	cases := []string{
		"",
		"not-a-uri",
		":no-scheme",
		"pkg", // no colon at all
	}
	for _, uri := range cases {
		t.Run(uri, func(t *testing.T) {
			t.Parallel()
			got := EntityTypeForURI(uri)
			assert.Equal(t, EntityPackage, got, "malformed URI must fall back to EntityPackage")
		})
	}
}

func TestTemporalEraConstants(t *testing.T) {
	t.Parallel()

	eras := []TemporalEra{EraPreLLM, EraEarlyLLM, EraModernAI, EraMatureCyber}
	for _, e := range eras {
		assert.NotEmpty(t, string(e), "TemporalEra constant must not be empty")
	}

	seen := make(map[TemporalEra]bool)
	for _, e := range eras {
		assert.False(t, seen[e], "duplicate TemporalEra: %s", e)
		seen[e] = true
	}
}

func TestPreLLMEnd(t *testing.T) {
	t.Parallel()

	boundary := PreLLMEnd()
	expected := time.Date(2022, 11, 30, 0, 0, 0, 0, time.UTC)
	assert.Equal(t, expected, boundary)
}

func TestEarlyLLMEnd(t *testing.T) {
	t.Parallel()

	boundary := EarlyLLMEnd()
	expected := time.Date(2025, 11, 24, 0, 0, 0, 0, time.UTC)
	assert.Equal(t, expected, boundary)
}

func TestBoundaryAccessorsAreImmutable(t *testing.T) {
	t.Parallel()

	// Taking the value should not allow mutation of the internal variable.
	before1 := PreLLMEnd()
	before2 := PreLLMEnd()
	assert.Equal(t, before1, before2, "PreLLMEnd must return the same value on repeated calls")

	early1 := EarlyLLMEnd()
	early2 := EarlyLLMEnd()
	assert.Equal(t, early1, early2, "EarlyLLMEnd must return the same value on repeated calls")
}

func TestClassifyEra(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		input    time.Time
		expected TemporalEra
	}{
		{
			name:     "ZeroTime_IsPreLLM",
			input:    time.Time{},
			expected: EraPreLLM,
		},
		{
			name:     "WellBeforePreLLMBoundary",
			input:    time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC),
			expected: EraPreLLM,
		},
		{
			name:     "OneNanosecondBeforePreLLMEnd",
			input:    time.Date(2022, 11, 29, 23, 59, 59, 999999999, time.UTC),
			expected: EraPreLLM,
		},
		{
			name:     "ExactPreLLMEnd_IsEarlyLLM",
			input:    time.Date(2022, 11, 30, 0, 0, 0, 0, time.UTC),
			expected: EraEarlyLLM,
		},
		{
			name:     "OneNanosecondAfterPreLLMEnd",
			input:    time.Date(2022, 11, 30, 0, 0, 0, 1, time.UTC),
			expected: EraEarlyLLM,
		},
		{
			name:     "MiddleOfEarlyLLM",
			input:    time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC),
			expected: EraEarlyLLM,
		},
		{
			name:     "OneNanosecondBeforeEarlyLLMEnd",
			input:    time.Date(2025, 11, 23, 23, 59, 59, 999999999, time.UTC),
			expected: EraEarlyLLM,
		},
		{
			name:     "ExactEarlyLLMEnd_IsModernAI",
			input:    time.Date(2025, 11, 24, 0, 0, 0, 0, time.UTC),
			expected: EraModernAI,
		},
		{
			name:     "OneNanosecondAfterEarlyLLMEnd",
			input:    time.Date(2025, 11, 24, 0, 0, 0, 1, time.UTC),
			expected: EraModernAI,
		},
		{
			name:     "FarFuture",
			input:    time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC),
			expected: EraMatureCyber,
		},
		{
			// Illustrative case in the mature-cyber era. The boundary itself
			// is interpretive (target durability, not authorship capability),
			// so we don't carry the nanosecond-precision triple the earlier
			// boundaries get — one mid-range case suffices.
			name:     "MidMatureCyber",
			input:    time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
			expected: EraMatureCyber,
		},
		{
			name:     "NonUTCTimezone_StillClassifiedCorrectly",
			input:    time.Date(2022, 11, 29, 20, 0, 0, 0, time.FixedZone("EST", -5*60*60)),
			expected: EraEarlyLLM, // 2022-11-30 01:00 UTC
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			result := ClassifyEra(tc.input)
			assert.Equal(t, tc.expected, result)
		})
	}
}

func TestEntityJSONRoundTrip(t *testing.T) {
	t.Parallel()

	now := time.Date(2024, 6, 15, 10, 30, 0, 0, time.UTC)
	entity := Entity{
		ID:        "pkg-123",
		Type:      EntityPackage,
		ShortName: "lodash",
		Ecosystem: "npm",
		URL:       "https://github.com/lodash/lodash",
		CreatedAt: now,
		UpdatedAt: now.Add(24 * time.Hour),
	}

	data, err := json.Marshal(entity)
	require.NoError(t, err)

	var decoded Entity
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)

	assert.Equal(t, entity.ID, decoded.ID)
	assert.Equal(t, entity.Type, decoded.Type)
	assert.Equal(t, entity.ShortName, decoded.ShortName)
	assert.Equal(t, entity.Ecosystem, decoded.Ecosystem)
	assert.Equal(t, entity.URL, decoded.URL)
	assert.True(t, entity.CreatedAt.Equal(decoded.CreatedAt))
	assert.True(t, entity.UpdatedAt.Equal(decoded.UpdatedAt))
}

func TestEntityJSON_OmitsEmptyFields(t *testing.T) {
	t.Parallel()

	entity := Entity{
		ID:        "id-1",
		Type:      EntityProject,
		ShortName: "myproject",
	}

	data, err := json.Marshal(entity)
	require.NoError(t, err)

	var raw map[string]json.RawMessage
	err = json.Unmarshal(data, &raw)
	require.NoError(t, err)

	_, hasEcosystem := raw["ecosystem"]
	_, hasURL := raw["url"]
	assert.False(t, hasEcosystem, "empty ecosystem should be omitted from JSON")
	assert.False(t, hasURL, "empty url should be omitted from JSON")
}

func TestEntityJSON_ZeroValue(t *testing.T) {
	t.Parallel()

	var entity Entity
	data, err := json.Marshal(entity)
	require.NoError(t, err)
	assert.NotEmpty(t, data)

	var decoded Entity
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)
	assert.Equal(t, entity, decoded)
}

func TestProfileJSONRoundTrip(t *testing.T) {
	t.Parallel()

	now := time.Date(2024, 6, 15, 10, 30, 0, 0, time.UTC)
	p := Profile{
		Entity: Entity{
			ID:        "pkg-1",
			Type:      EntityPackage,
			ShortName: "test-pkg",
			CreatedAt: now,
			UpdatedAt: now,
		},
		Signals: []Signal{
			{
				ID:                "sig-1",
				EntityID:          "pkg-1",
				Type:              "commit-activity",
				Group:             SignalGroupVitality,
				Source:            "github",
				ForgeryResistance: ForgeryHigh,
				Value:             json.RawMessage(`{"count":42}`),
				CollectedAt:       now,
				ExpiresAt:         now.Add(24 * time.Hour),
			},
		},
		Posture: &Posture{
			EntityID:  "pkg-1",
			Tier:      PostureVettedFrozen,
			Version:   "1.0.0",
			Rationale: "audited",
			SetBy:     "alice",
			SetAt:     now,
		},
		Burn: &Burn{
			EntityID: "pkg-1",
			Reason:   "malware found",
			Source:   BurnSourceLocal,
			BurnedAt: now,
			BurnedBy: "bob",
		},
		Era: EraEarlyLLM,
	}

	data, err := json.Marshal(p)
	require.NoError(t, err)

	var decoded Profile
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)

	assert.Equal(t, p.Entity.ID, decoded.Entity.ID)
	assert.Equal(t, p.Era, decoded.Era)
	require.NotNil(t, decoded.Posture)
	assert.Equal(t, p.Posture.Tier, decoded.Posture.Tier)
	require.NotNil(t, decoded.Burn)
	assert.Equal(t, p.Burn.Reason, decoded.Burn.Reason)
	require.Len(t, decoded.Signals, 1)
	assert.Equal(t, p.Signals[0].ID, decoded.Signals[0].ID)
}

func TestProfileJSON_NilOptionalFields(t *testing.T) {
	t.Parallel()

	p := Profile{
		Entity: Entity{
			ID:        "e-1",
			Type:      EntityProject,
			ShortName: "bare",
		},
		Signals: nil,
		Posture: nil,
		Burn:    nil,
	}

	data, err := json.Marshal(p)
	require.NoError(t, err)

	var raw map[string]json.RawMessage
	err = json.Unmarshal(data, &raw)
	require.NoError(t, err)

	_, hasPosture := raw["posture"]
	_, hasBurn := raw["burn"]
	assert.False(t, hasPosture, "nil posture should be omitted from JSON")
	assert.False(t, hasBurn, "nil burn should be omitted from JSON")
}

func TestProfileJSON_EmptySignals(t *testing.T) {
	t.Parallel()

	p := Profile{
		Entity: Entity{
			ID:        "e-1",
			Type:      EntityProject,
			ShortName: "test",
		},
		Signals: []Signal{},
	}

	data, err := json.Marshal(p)
	require.NoError(t, err)

	var decoded Profile
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)

	// Empty slice serializes as [] and deserializes as empty slice
	assert.NotNil(t, decoded.Signals)
	assert.Empty(t, decoded.Signals)
}
