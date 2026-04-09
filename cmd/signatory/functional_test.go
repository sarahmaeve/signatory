package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal"
	"github.com/sarahmaeve/signatory/internal/store"
)

// mockCollector returns canned signals without network access.
type mockCollector struct {
	name    string
	signals []profile.Signal
	err     error
}

func (m *mockCollector) Name() string { return m.name }
func (m *mockCollector) Collect(_ context.Context, entity *profile.Entity) (*signal.CollectionResult, error) {
	if m.err != nil {
		return nil, m.err
	}
	var result signal.CollectionResult
	for _, s := range m.signals {
		s.EntityID = entity.ID
		s.ID = m.name + ":" + entity.ID + ":" + s.Type
		result.Collected = append(result.Collected, signal.MakeSignal(s))
	}
	return &result, nil
}

func newMockCollector() *mockCollector {
	now := time.Now().UTC()
	return &mockCollector{
		name: "mock",
		signals: []profile.Signal{
			{Type: "stars", Group: profile.SignalGroupCriticality, Source: "mock",
				ForgeryResistance: profile.ForgeryMediumDeclining,
				Value:             json.RawMessage(`{"count":1000}`), CollectedAt: now, ExpiresAt: now.Add(time.Hour)},
			{Type: "last_commit", Group: profile.SignalGroupVitality, Source: "mock",
				ForgeryResistance: profile.ForgeryMediumDeclining,
				Value:             json.RawMessage(`{"days_ago":5}`), CollectedAt: now, ExpiresAt: now.Add(time.Hour)},
		},
	}
}

// testGlobals creates Globals with mock collectors and a temp database.
func testGlobals(t *testing.T, collectors ...signal.Collector) *Globals {
	t.Helper()
	return &Globals{
		DBPath:     filepath.Join(t.TempDir(), "test.db"),
		Collectors: collectors,
	}
}

func TestFunctional_PostureSetAndGet(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")

	// Set posture via command.
	setCmd := &PostureSetCmd{
		Target:    "pkg:npm:express",
		Tier:      "trusted-for-now",
		Rationale: "Strong vitality, no anomalies",
		Version:   "4.18.2",
	}
	globals := &Globals{DBPath: dbPath}
	require.NoError(t, setCmd.Run(globals))

	// Read back via store directly to verify persistence.
	s, err := store.OpenSQLite(dbPath)
	require.NoError(t, err)
	defer s.Close()

	posture, err := s.GetPosture(context.Background(), "pkg:npm:express")
	require.NoError(t, err)
	assert.Equal(t, profile.PostureTrustedForNow, posture.Tier)
	assert.Equal(t, "4.18.2", posture.Version)
	assert.Equal(t, "Strong vitality, no anomalies", posture.Rationale)
}

func TestFunctional_PostureGetNotFound(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")

	getCmd := &PostureGetCmd{Target: "nonexistent"}
	globals := &Globals{DBPath: dbPath}

	// Should not error — just prints "No posture recorded".
	require.NoError(t, getCmd.Run(globals))
}

func TestFunctional_PostureSetCreatesEntity(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")

	setCmd := &PostureSetCmd{
		Target:    "pkg:npm:lodash",
		Tier:      "unexamined",
		Rationale: "Haven't looked yet",
	}
	globals := &Globals{DBPath: dbPath}
	require.NoError(t, setCmd.Run(globals))

	// Verify the entity was created.
	s, err := store.OpenSQLite(dbPath)
	require.NoError(t, err)
	defer s.Close()

	entity, err := s.GetEntity(context.Background(), "pkg:npm:lodash")
	require.NoError(t, err)
	assert.Equal(t, "pkg:npm:lodash", entity.Name)
	assert.Equal(t, profile.EntityPackage, entity.Type)
}

func TestFunctional_DBPathCustom(t *testing.T) {
	// Verify that a custom --db path works.
	dbPath := filepath.Join(t.TempDir(), "custom", "path", "my.db")

	setCmd := &PostureSetCmd{
		Target:    "pkg:npm:express",
		Tier:      "trusted-for-now",
		Rationale: "test",
	}
	globals := &Globals{DBPath: dbPath}
	require.NoError(t, setCmd.Run(globals))

	// Verify the file was created at the custom path.
	s, err := store.OpenSQLite(dbPath)
	require.NoError(t, err)
	defer s.Close()

	posture, err := s.GetPosture(context.Background(), "pkg:npm:express")
	require.NoError(t, err)
	assert.Equal(t, profile.PostureTrustedForNow, posture.Tier)
}

// --- Burn functional tests ---

func TestFunctional_BurnAndReadBack(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")

	burnCmd := &BurnAddCmd{
		Target: "pkg:npm:evil-package",
		Reason: "Maintainer account compromised",
	}
	globals := &Globals{DBPath: dbPath}
	require.NoError(t, burnCmd.Run(globals))

	// Read back via store directly.
	s, err := store.OpenSQLite(dbPath)
	require.NoError(t, err)
	defer s.Close()

	burn, err := s.GetBurn(context.Background(), "pkg:npm:evil-package")
	require.NoError(t, err)
	assert.Equal(t, "Maintainer account compromised", burn.Reason)
	assert.Equal(t, profile.BurnSourceLocal, burn.Source)
}

func TestFunctional_BurnCreatesEntity(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")

	burnCmd := &BurnAddCmd{
		Target: "pkg:npm:compromised",
		Reason: "Supply chain attack",
	}
	globals := &Globals{DBPath: dbPath}
	require.NoError(t, burnCmd.Run(globals))

	s, err := store.OpenSQLite(dbPath)
	require.NoError(t, err)
	defer s.Close()

	entity, err := s.GetEntity(context.Background(), "pkg:npm:compromised")
	require.NoError(t, err)
	assert.Equal(t, "pkg:npm:compromised", entity.Name)
}

func TestFunctional_BurnOverwriteExisting(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	globals := &Globals{DBPath: dbPath}

	// First burn.
	burn1 := &BurnAddCmd{Target: "pkg:npm:bad", Reason: "suspicious activity"}
	require.NoError(t, burn1.Run(globals))

	// Second burn overwrites.
	burn2 := &BurnAddCmd{Target: "pkg:npm:bad", Reason: "confirmed malware"}
	require.NoError(t, burn2.Run(globals))

	s, err := store.OpenSQLite(dbPath)
	require.NoError(t, err)
	defer s.Close()

	burn, err := s.GetBurn(context.Background(), "pkg:npm:bad")
	require.NoError(t, err)
	assert.Equal(t, "confirmed malware", burn.Reason)
}

func TestFunctional_BurnListEmpty(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")

	listCmd := &BurnListCmd{}
	globals := &Globals{DBPath: dbPath}
	require.NoError(t, listCmd.Run(globals))
}

func TestFunctional_BurnListWithEntries(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	globals := &Globals{DBPath: dbPath}

	for _, name := range []string{"pkg:npm:bad-1", "pkg:npm:bad-2"} {
		cmd := &BurnAddCmd{Target: name, Reason: "compromised"}
		require.NoError(t, cmd.Run(globals))
	}

	listCmd := &BurnListCmd{}
	require.NoError(t, listCmd.Run(globals))
}

// --- ResolvePath tests ---

// --- Analyze functional tests (mock collector, no network) ---

func TestFunctional_AnalyzeRefreshWithMock(t *testing.T) {
	globals := testGlobals(t, newMockCollector())

	cmd := &AnalyzeCmd{Target: "owner/repo", Refresh: true}
	require.NoError(t, cmd.Run(globals))

	// Verify signals were persisted.
	s, err := store.OpenSQLite(globals.DBPath)
	require.NoError(t, err)
	defer s.Close()

	signals, err := s.GetSignals(context.Background(), "owner/repo")
	require.NoError(t, err)
	assert.Len(t, signals, 2)
}

func TestFunctional_AnalyzeCachedFromMock(t *testing.T) {
	globals := testGlobals(t, newMockCollector())

	// First call with --refresh to populate cache.
	cmd1 := &AnalyzeCmd{Target: "owner/repo", Refresh: true}
	require.NoError(t, cmd1.Run(globals))

	// Second call without --refresh reads from cache.
	cmd2 := &AnalyzeCmd{Target: "owner/repo", Refresh: false}
	require.NoError(t, cmd2.Run(globals))
}

func TestFunctional_AnalyzeNoDataNoRefresh(t *testing.T) {
	globals := testGlobals(t, newMockCollector())

	cmd := &AnalyzeCmd{Target: "owner/repo", Refresh: false}
	require.NoError(t, cmd.Run(globals))
}

func TestFunctional_AnalyzeJSONOutput(t *testing.T) {
	globals := testGlobals(t, newMockCollector())

	cmd := &AnalyzeCmd{Target: "owner/repo", Refresh: true, JSON: true}
	require.NoError(t, cmd.Run(globals))
}

func TestFunctional_AnalyzeWithPostureAndBurn(t *testing.T) {
	globals := testGlobals(t, newMockCollector())

	// Set posture first.
	postureCmd := &PostureSetCmd{
		Target: "owner/repo", Tier: "trusted-for-now",
		Rationale: "Looks good",
	}
	require.NoError(t, postureCmd.Run(globals))

	// Analyze with refresh.
	analyzeCmd := &AnalyzeCmd{Target: "owner/repo", Refresh: true}
	require.NoError(t, analyzeCmd.Run(globals))
}

// --- ResolvePath tests ---

func TestFunctional_ResolvePath_Tilde(t *testing.T) {
	path, err := store.ResolvePath("~/custom/signatory.db")
	require.NoError(t, err)
	assert.NotContains(t, path, "~", "tilde should be expanded")
	assert.Contains(t, path, "custom/signatory.db")
}

func TestFunctional_ResolvePath_Empty(t *testing.T) {
	path, err := store.ResolvePath("")
	require.NoError(t, err)
	assert.NotContains(t, path, "~")
	assert.Contains(t, path, ".signatory/signatory.db")
}

func TestFunctional_ResolvePath_Absolute(t *testing.T) {
	path, err := store.ResolvePath("/tmp/my.db")
	require.NoError(t, err)
	assert.Equal(t, "/tmp/my.db", path)
}
