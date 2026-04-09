//go:build network_access_ok

// These tests hit the real GitHub API. They are NOT run by default.
//
// To run:   go test -tags network_access_ok ./cmd/signatory/...
//
// Requirements:
//   - Network access to api.github.com
//   - GITHUB_TOKEN env var recommended (unauthenticated: 60 req/hr)
//
// These tests exist to validate the full end-to-end path against live
// data. They should be run manually before releases, not in CI or
// pre-commit hooks.

package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
	ghcollector "github.com/sarahmaeve/signatory/internal/signal/github"
	"github.com/sarahmaeve/signatory/internal/store"
)

func liveGlobals(t *testing.T) *Globals {
	t.Helper()
	return &Globals{
		DBPath:     filepath.Join(t.TempDir(), "live-test.db"),
		Collectors: defaultCollectors(),
	}
}

func TestLive_AnalyzeKong(t *testing.T) {
	globals := liveGlobals(t)

	cmd := &AnalyzeCmd{Target: "alecthomas/kong", Refresh: true}
	require.NoError(t, cmd.Run(globals))

	// Verify signals were persisted.
	s, err := store.OpenSQLite(globals.DBPath)
	require.NoError(t, err)
	defer s.Close()

	signals, err := s.GetSignals(context.Background(), "alecthomas/kong")
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(signals), 14, "should collect at least 14 signals")
}

func TestLive_AnalyzeMousetrap_PreLLMEra(t *testing.T) {
	globals := liveGlobals(t)

	cmd := &AnalyzeCmd{Target: "inconshreveable/mousetrap", Refresh: true, JSON: true}
	require.NoError(t, cmd.Run(globals))
}

func TestLive_AnalyzeTestify_OrgOwner(t *testing.T) {
	globals := liveGlobals(t)

	cmd := &AnalyzeCmd{Target: "stretchr/testify", Refresh: true}
	require.NoError(t, cmd.Run(globals))

	s, err := store.OpenSQLite(globals.DBPath)
	require.NoError(t, err)
	defer s.Close()

	signals, err := s.GetSignals(context.Background(), "stretchr/testify")
	require.NoError(t, err)

	// Find owner_type signal and verify it's an Organization.
	for _, sig := range signals {
		if sig.Type == "owner_type" {
			assert.Contains(t, string(sig.Value), "Organization")
		}
	}
}

func TestLive_CollectorDirectly(t *testing.T) {
	collector := ghcollector.NewCollector()
	ctx := context.Background()

	entity := &profile.Entity{
		ID:   "test:alecthomas/kong",
		Type: profile.EntityProject,
		Name: "alecthomas/kong",
	}

	signals, err := collector.Collect(ctx, entity)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(signals), 14)

	// Verify signal groups are populated.
	groups := make(map[profile.SignalGroup]bool)
	for _, s := range signals {
		groups[s.Group] = true
	}
	assert.True(t, groups[profile.SignalGroupVitality])
	assert.True(t, groups[profile.SignalGroupGovernance])
	assert.True(t, groups[profile.SignalGroupCriticality])
}
