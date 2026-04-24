package resources_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/mcp/resources"
	"github.com/sarahmaeve/signatory/internal/profile"
)

// URIPattern() is covered by the registration contract test in
// cmd/signatory (TestMCPRegistration_Contract).

func TestSignalTypesResource_HappyPath(t *testing.T) {
	t.Parallel()
	r := &resources.SignalTypesResource{}

	resp := r.Read(t.Context(), "signatory://signal-types")

	require.Equal(t, "ok", resp.Status)
	require.Nil(t, resp.Error)
	// Metadata.ServerVersion is stamped by the dispatch layer at
	// emission — not by handlers. See internal/mcp/server_test.go for
	// the end-to-end version assertion.

	raw := mustMarshal(t, resp.Data)
	var decoded struct {
		Groups []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"groups"`
		ForgeryResistanceScale []string `json:"forgery_resistance_scale"`
		Note                   string   `json:"note"`
	}
	require.NoError(t, unmarshal(raw, &decoded))

	assert.NotEmpty(t, decoded.Groups, "registry must include at least one group")
	assert.NotEmpty(t, decoded.ForgeryResistanceScale, "registry must include forgery resistance scale")
	assert.NotEmpty(t, decoded.Note, "v0.1 note must be present")
}

func TestSignalTypesResource_AllGroupsPresent(t *testing.T) {
	t.Parallel()
	r := &resources.SignalTypesResource{}

	resp := r.Read(t.Context(), "signatory://signal-types")
	require.Equal(t, "ok", resp.Status)

	raw := mustMarshal(t, resp.Data)
	var decoded struct {
		Groups []struct {
			Name string `json:"name"`
		} `json:"groups"`
	}
	require.NoError(t, unmarshal(raw, &decoded))

	names := make(map[string]bool, len(decoded.Groups))
	for _, g := range decoded.Groups {
		names[g.Name] = true
	}

	// All profile.SignalGroup constants must be represented.
	expectedGroups := []string{
		string(profile.SignalGroupVitality),
		string(profile.SignalGroupGovernance),
		string(profile.SignalGroupPublication),
		string(profile.SignalGroupHygiene),
		string(profile.SignalGroupPosture),
		string(profile.SignalGroupCriticality),
	}
	for _, want := range expectedGroups {
		assert.True(t, names[want], "group %q must appear in signal-types registry", want)
	}
}

func TestSignalTypesResource_ForgeryScaleOrder(t *testing.T) {
	t.Parallel()
	r := &resources.SignalTypesResource{}

	resp := r.Read(t.Context(), "signatory://signal-types")
	raw := mustMarshal(t, resp.Data)
	var decoded struct {
		ForgeryResistanceScale []string `json:"forgery_resistance_scale"`
	}
	require.NoError(t, unmarshal(raw, &decoded))

	// Must include all four ForgeryResistance constants.
	allValues := []string{
		string(profile.ForgeryVeryHigh),
		string(profile.ForgeryHigh),
		string(profile.ForgeryMediumDeclining),
		string(profile.ForgeryLowDeclining),
	}
	inScale := make(map[string]bool)
	for _, v := range decoded.ForgeryResistanceScale {
		inScale[v] = true
	}
	for _, want := range allValues {
		assert.True(t, inScale[want], "forgery resistance value %q must be in scale", want)
	}

	// Verify first is the strongest resistance value.
	require.NotEmpty(t, decoded.ForgeryResistanceScale)
	assert.Equal(t, string(profile.ForgeryVeryHigh), decoded.ForgeryResistanceScale[0],
		"scale must start with the highest forgery resistance")
}

// TestSignalTypesResource_MutationVerify_ConsistentAcrossCalls proves the
// registry is stable: two consecutive calls must return identical output.
// This catches any unintentional non-determinism (e.g., map iteration order).
func TestSignalTypesResource_MutationVerify_ConsistentAcrossCalls(t *testing.T) {
	t.Parallel()
	r := &resources.SignalTypesResource{}

	resp1 := r.Read(t.Context(), "signatory://signal-types")
	resp2 := r.Read(t.Context(), "signatory://signal-types")

	raw1 := mustMarshal(t, resp1.Data)
	raw2 := mustMarshal(t, resp2.Data)
	assert.Equal(t, string(raw1), string(raw2),
		"mutation-verify: signal-types registry must be deterministic across calls")
}
