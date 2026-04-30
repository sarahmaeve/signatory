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

	var val map[string]any
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

	var val map[string]any
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

// TestCollectionResult_Summary_EnumeratesSignalTypes pins the contract
// that Summary names each collected type in the per-collector line,
// not just the count. The collector loop in AnalyzeCmd.Run emits this
// string verbatim ([%s] %s), so a manual user sees:
//
//	[github] Collected 17 signals: stars, forks, contributors, ...
//
// rather than the opaque "Collected 17 signals" — same data, but the
// user can scan WHICH signals fired without scrolling through the
// rendered profile.
//
// Absences are enumerated with their "absence:" prefix so the user
// can distinguish "we collected stars" from "we recorded an absence
// for publish_origin" at a glance.
func TestCollectionResult_Summary_EnumeratesSignalTypes(t *testing.T) {
	now := time.Now()
	result := &CollectionResult{
		Collected: []SignalOrAbsence{
			MakeSignal(profile.Signal{Type: "stars"}),
			MakeSignal(profile.Signal{Type: "forks"}),
			MakeAbsence("e", "publish_origin", "gopublish", "no origin block", false, now),
		},
	}
	summary := result.Summary()
	assert.Contains(t, summary, "stars",
		"summary must enumerate each collected signal type so the user can see what fired")
	assert.Contains(t, summary, "forks",
		"summary must enumerate each collected signal type")
	assert.Contains(t, summary, "absence:publish_origin",
		"summary must enumerate absences with their 'absence:' prefix so the user can distinguish definitive negatives from positive observations")
}

// TestCollectionResult_Summary_EnumeratesFailures pins the contract
// that the per-collector summary line surfaces WHICH signals failed
// and WHY, not just the counts. The collector loop in AnalyzeCmd.Run
// emits this string verbatim (`[%s] %s`), so dropping the failure
// detail there is the difference between
//
//	[github] Collected 17 signals, 1 failures
//
// and
//
//	[github] Collected 17 signals: stars, ...; 1 failures: adoption=GitHub API 403
//
// for a manual CLI user trying to figure out what to fix.
func TestCollectionResult_Summary_EnumeratesFailures(t *testing.T) {
	result := &CollectionResult{
		Collected: []SignalOrAbsence{
			MakeSignal(profile.Signal{Type: "stars"}),
		},
		Failures: []CollectionError{
			{SignalType: "adoption", Source: "github", Reason: "GitHub API 403", Retryable: false},
			{SignalType: "contributors", Source: "github", Reason: "rate limited", Retryable: true},
		},
	}

	summary := result.Summary()

	// Existing contract preserved: counts and retryable bulk count.
	assert.Contains(t, summary, "1 signals")
	assert.Contains(t, summary, "2 failures")
	assert.Contains(t, summary, "1 retryable")

	// New contract: per-failure detail. Each failed signal type AND
	// its reason must appear in the line so the user can see what to
	// look at without re-running with extra logging.
	assert.Contains(t, summary, "adoption",
		"summary must name the failed signal type so the user knows what to investigate")
	assert.Contains(t, summary, "GitHub API 403",
		"summary must include the failure reason so the user knows whether to retry, fix auth, etc.")
	assert.Contains(t, summary, "contributors",
		"summary must enumerate every failed signal type, not just the first")
	assert.Contains(t, summary, "rate limited",
		"summary must include each failure's reason")
}
