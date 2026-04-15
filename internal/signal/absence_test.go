package signal

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
)

func TestMakeSignal(t *testing.T) {
	sig := profile.Signal{
		ID:       "test-id",
		EntityID: "entity-1",
		Type:     "stars",
	}
	soa := MakeSignal(sig)
	assert.False(t, soa.IsAbsence())
	assert.Equal(t, "test-id", soa.ToSignal().ID)
}

func TestMakeAbsence(t *testing.T) {
	now := time.Now().UTC()
	soa := MakeAbsence("entity-1", "contributors", "github", "rate limited", true, now)

	assert.True(t, soa.IsAbsence())

	sig := soa.ToSignal()
	assert.Equal(t, "entity-1", sig.EntityID)
	assert.Equal(t, "absence:contributors", sig.Type)
	assert.Equal(t, "github", sig.Source)
	assert.Equal(t, profile.ForgeryHigh, sig.ForgeryResistance)

	var val map[string]interface{}
	require.NoError(t, json.Unmarshal(sig.Value, &val))
	assert.Equal(t, true, val["absent"])
	assert.Equal(t, "rate limited", val["reason"])
	assert.Equal(t, true, val["retryable"])
}

func TestAbsence_ShortTTL(t *testing.T) {
	now := time.Now().UTC()
	soa := MakeAbsence("entity-1", "stars", "github", "timeout", true, now)
	sig := soa.ToSignal()

	// Absence signals should have a short TTL (1 hour) to encourage retry.
	assert.Equal(t, now.Add(1*time.Hour), sig.ExpiresAt)
}

func TestAbsence_NonRetryable(t *testing.T) {
	now := time.Now().UTC()
	soa := MakeAbsence("entity-1", "go_dependencies", "github", "no go.mod found", false, now)

	var val map[string]interface{}
	require.NoError(t, json.Unmarshal(soa.ToSignal().Value, &val))
	assert.Equal(t, false, val["retryable"])
}

func TestAbsence_SignalGroupMapping(t *testing.T) {
	tests := []struct {
		signalType string
		wantGroup  profile.SignalGroup
	}{
		{"stars", profile.SignalGroupCriticality},
		{"forks", profile.SignalGroupCriticality},
		{"adoption", profile.SignalGroupCriticality},
		{"contributors", profile.SignalGroupGovernance},
		{"commit_signing", profile.SignalGroupGovernance},
		{"owner_profile", profile.SignalGroupGovernance},
		{"tags", profile.SignalGroupPublication},
		{"license", profile.SignalGroupHygiene},
		{"ci_cd", profile.SignalGroupHygiene},
		{"last_commit", profile.SignalGroupVitality},
		{"unknown_type", profile.SignalGroupVitality},
	}

	now := time.Now().UTC()
	for _, tt := range tests {
		t.Run(tt.signalType, func(t *testing.T) {
			soa := MakeAbsence("entity-1", tt.signalType, "test", "reason", false, now)
			assert.Equal(t, tt.wantGroup, soa.ToSignal().Group)
		})
	}
}

func TestCollectionResult_Summary(t *testing.T) {
	result := &CollectionResult{
		Collected: []SignalOrAbsence{
			MakeSignal(profile.Signal{Type: "stars"}),
			MakeSignal(profile.Signal{Type: "forks"}),
			MakeAbsence("e", "contributors", "gh", "rate limited", true, time.Now()),
		},
		Failures: []CollectionError{
			{SignalType: "contributors", Source: "gh", Retryable: true},
		},
	}

	summary := result.Summary()
	assert.Contains(t, summary, "3 signals")
	assert.Contains(t, summary, "1 failures")
	assert.Contains(t, summary, "1 retryable")
}

func TestCollectionResult_NoFailures(t *testing.T) {
	result := &CollectionResult{
		Collected: []SignalOrAbsence{
			MakeSignal(profile.Signal{Type: "stars"}),
		},
	}

	assert.False(t, result.HasFailures())
	assert.Contains(t, result.Summary(), "1 signals")
	assert.NotContains(t, result.Summary(), "failures")
}
