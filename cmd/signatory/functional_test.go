package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal"
	"github.com/sarahmaeve/signatory/internal/store"
)

// mockCollector returns canned signals without network access.
//
// Each Collect() call generates a unique signal ID by appending a
// monotonic call counter — this matters because AppendSignals is
// now append-only and will reject duplicate IDs, so running the same
// mock collector twice with the same entity needs to produce
// different IDs the second time.
type mockCollector struct {
	name    string
	signals []profile.Signal
	err     error

	callCount int64 // atomic
}

func (m *mockCollector) Name() string { return m.name }

func (m *mockCollector) Collect(_ context.Context, entity *profile.Entity) (*signal.CollectionResult, error) {
	if m.err != nil {
		return nil, m.err
	}
	seq := atomic.AddInt64(&m.callCount, 1)
	var result signal.CollectionResult
	for i, s := range m.signals {
		s.EntityID = entity.ID
		s.ID = fmt.Sprintf("%s:%s:%s:%d:%d", m.name, entity.ID, s.Type, seq, i)
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

// testGlobals creates Globals with mock collectors, a temp database,
// and an isolated audit log file. Tests that hit the CLI commands
// should use this helper rather than constructing Globals inline so
// the audit log path is always redirected away from ~/.signatory.
func testGlobals(t *testing.T, collectors ...signal.Collector) *Globals {
	t.Helper()
	dir := t.TempDir()
	return &Globals{
		DBPath:        filepath.Join(dir, "test.db"),
		Collectors:    collectors,
		AuditFilePath: filepath.Join(dir, "audit.log"),
	}
}

// --- Posture functional tests ---

func TestFunctional_PostureSetAndGet(t *testing.T) {
	globals := testGlobals(t)

	setCmd := &PostureSetCmd{
		Target:    "pkg:npm/express",
		Tier:      "trusted-for-now",
		Rationale: "Strong vitality, no anomalies",
		Version:   "4.18.2",
	}
	require.NoError(t, setCmd.Run(globals))

	// Read back via store directly to verify persistence.
	s, err := store.OpenSQLite(t.Context(), globals.DBPath)
	require.NoError(t, err)
	defer s.Close()

	entity, err := s.FindEntityByURI(context.Background(), "pkg:npm/express")
	require.NoError(t, err)

	posture, err := s.GetPosture(context.Background(), entity.ID, "4.18.2")
	require.NoError(t, err)
	assert.Equal(t, profile.PostureTrustedForNow, posture.Tier)
	assert.Equal(t, "4.18.2", posture.Version)
	assert.Equal(t, "Strong vitality, no anomalies", posture.Rationale)
}

func TestFunctional_PostureGetNotFound(t *testing.T) {
	globals := testGlobals(t)

	getCmd := &PostureGetCmd{Target: "pkg:npm/nonexistent"}

	// Should not error — just prints "No posture recorded".
	require.NoError(t, getCmd.Run(globals))
}

func TestFunctional_PostureSetCreatesEntity(t *testing.T) {
	globals := testGlobals(t)

	setCmd := &PostureSetCmd{
		Target:    "pkg:npm/lodash",
		Tier:      "unexamined",
		Rationale: "Haven't looked yet",
	}
	require.NoError(t, setCmd.Run(globals))

	// Verify the entity was created — by canonical URI, not by
	// the UUID (which we don't know).
	s, err := store.OpenSQLite(t.Context(), globals.DBPath)
	require.NoError(t, err)
	defer s.Close()

	entity, err := s.FindEntityByURI(context.Background(), "pkg:npm/lodash")
	require.NoError(t, err)
	assert.Equal(t, "pkg:npm/lodash", entity.CanonicalURI)
}

func TestFunctional_PostureVersionedGetLatest(t *testing.T) {
	globals := testGlobals(t)

	// Record postures for two versions.
	require.NoError(t, (&PostureSetCmd{
		Target: "alecthomas/kong", Tier: "vetted-frozen",
		Rationale: "audited v1.15.0", Version: "v1.15.0",
	}).Run(globals))
	require.NoError(t, (&PostureSetCmd{
		Target: "alecthomas/kong", Tier: "trusted-for-now",
		Rationale: "minor bump", Version: "v1.16.0",
	}).Run(globals))

	// Get with no --version returns the latest (most recent set_at).
	require.NoError(t, (&PostureGetCmd{Target: "alecthomas/kong"}).Run(globals))
	// Get with --version pulls the exact row.
	require.NoError(t, (&PostureGetCmd{Target: "alecthomas/kong", Version: "v1.15.0"}).Run(globals))
	// --all shows both.
	require.NoError(t, (&PostureGetCmd{Target: "alecthomas/kong", All: true}).Run(globals))
}

func TestFunctional_DBPathCustom(t *testing.T) {
	// Verify that a custom --db path works.
	dbPath := filepath.Join(t.TempDir(), "custom", "path", "my.db")

	setCmd := &PostureSetCmd{
		Target:    "pkg:npm/express",
		Tier:      "trusted-for-now",
		Rationale: "test",
	}
	globals := &Globals{
		DBPath:        dbPath,
		AuditFilePath: filepath.Join(t.TempDir(), "audit.log"),
	}
	require.NoError(t, setCmd.Run(globals))

	// Verify the file was created at the custom path.
	s, err := store.OpenSQLite(t.Context(), dbPath)
	require.NoError(t, err)
	defer s.Close()

	entity, err := s.FindEntityByURI(context.Background(), "pkg:npm/express")
	require.NoError(t, err)
	postures, err := s.GetPostures(context.Background(), entity.ID)
	require.NoError(t, err)
	require.Len(t, postures, 1)
	assert.Equal(t, profile.PostureTrustedForNow, postures[0].Tier)
}

// --- Burn functional tests ---

func TestFunctional_BurnAndReadBack(t *testing.T) {
	globals := testGlobals(t)

	burnCmd := &BurnAddCmd{
		Target: "pkg:npm/evil-package",
		Reason: "Maintainer account compromised",
	}
	require.NoError(t, burnCmd.Run(globals))

	// Read back via store directly.
	s, err := store.OpenSQLite(t.Context(), globals.DBPath)
	require.NoError(t, err)
	defer s.Close()

	entity, err := s.FindEntityByURI(context.Background(), "pkg:npm/evil-package")
	require.NoError(t, err)

	burn, err := s.GetBurn(context.Background(), entity.ID)
	require.NoError(t, err)
	assert.Equal(t, "Maintainer account compromised", burn.Reason)
	assert.Equal(t, profile.BurnSourceLocal, burn.Source)
}

func TestFunctional_BurnCreatesEntity(t *testing.T) {
	globals := testGlobals(t)

	burnCmd := &BurnAddCmd{
		Target: "pkg:npm/compromised",
		Reason: "Supply chain attack",
	}
	require.NoError(t, burnCmd.Run(globals))

	s, err := store.OpenSQLite(t.Context(), globals.DBPath)
	require.NoError(t, err)
	defer s.Close()

	entity, err := s.FindEntityByURI(context.Background(), "pkg:npm/compromised")
	require.NoError(t, err)
	assert.Equal(t, "pkg:npm/compromised", entity.CanonicalURI)
}

func TestFunctional_BurnOverwriteExisting(t *testing.T) {
	globals := testGlobals(t)

	// First burn.
	burn1 := &BurnAddCmd{Target: "pkg:npm/bad", Reason: "suspicious activity"}
	require.NoError(t, burn1.Run(globals))

	// Second burn overwrites.
	burn2 := &BurnAddCmd{Target: "pkg:npm/bad", Reason: "confirmed malware"}
	require.NoError(t, burn2.Run(globals))

	s, err := store.OpenSQLite(t.Context(), globals.DBPath)
	require.NoError(t, err)
	defer s.Close()

	entity, err := s.FindEntityByURI(context.Background(), "pkg:npm/bad")
	require.NoError(t, err)
	burn, err := s.GetBurn(context.Background(), entity.ID)
	require.NoError(t, err)
	assert.Equal(t, "confirmed malware", burn.Reason)
}

// TestFunctional_BurnAuditDetailOverwriteFlagReflectsPriorBurn locks in
// the contract that audit_log.detail.overwrite == true iff the entity
// already had a burn before the current call. Issue #92: the prior
// implementation computed this via `"overwrite": err == nil` at the
// end of BurnAddCmd.Run, where `err` referred to the outer-scope GetBurn
// result from the start of the function. The reasoning was load-bearing
// on which specific `err` happened to be in scope at the moment the
// audit detail map was constructed — a one-character refactor of the
// SetBurn call (changing its `:=` to `=`, which would shadow the outer
// err) would silently flip the meaning to "did SetBurn succeed," which
// is always true on the success path. Every burn would then be logged
// as an overwrite, corrupting the audit trail.
//
// This test catches that class of bug: a fresh burn must be logged
// with overwrite=false, and a second burn over the same entity must be
// logged with overwrite=true. The test reads the actual persisted
// audit_log.detail JSON, not just the in-memory state.
func TestFunctional_BurnAuditDetailOverwriteFlagReflectsPriorBurn(t *testing.T) {
	tests := []struct {
		name          string
		preBurn       bool
		wantOverwrite bool
	}{
		{name: "fresh burn", preBurn: false, wantOverwrite: false},
		{name: "overwrite existing", preBurn: true, wantOverwrite: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			globals := testGlobals(t)

			if tc.preBurn {
				first := &BurnAddCmd{
					Target: "pkg:npm/audit-overwrite-test",
					Reason: "initial burn",
				}
				require.NoError(t, first.Run(globals))
			}

			// The burn whose audit detail we're inspecting.
			second := &BurnAddCmd{
				Target: "pkg:npm/audit-overwrite-test",
				Reason: "the burn under test",
			}
			require.NoError(t, second.Run(globals))

			// Read the most recent burn audit entry directly from the
			// audit_log table. ORDER BY ROWID DESC uses SQLite's implicit
			// auto-incrementing rowid which reflects insertion order —
			// reliable even when timestamps collide at second precision
			// and audit_log.id is a random hex string with no temporal
			// component.
			s, err := store.OpenSQLite(t.Context(), globals.DBPath)
			require.NoError(t, err)
			defer s.Close()

			var detailJSON string
			require.NoError(t, s.DB().QueryRowContext(context.Background(),
				`SELECT detail FROM audit_log WHERE action = 'burn' ORDER BY ROWID DESC LIMIT 1`,
			).Scan(&detailJSON))

			var detail struct {
				Overwrite    bool   `json:"overwrite"`
				Reason       string `json:"reason"`
				CanonicalURI string `json:"canonical_uri"`
			}
			require.NoError(t, json.Unmarshal([]byte(detailJSON), &detail))

			// Sanity check we read the right entry.
			assert.Equal(t, "the burn under test", detail.Reason,
				"this assertion failing means we read the wrong audit entry")
			assert.Equal(t, "pkg:npm/audit-overwrite-test", detail.CanonicalURI)

			// THE CRITICAL ASSERTION: the overwrite flag must reflect
			// reality, not be a side effect of which `err` happens to
			// be in scope at construction time.
			assert.Equal(t, tc.wantOverwrite, detail.Overwrite,
				"audit detail.overwrite must match whether a prior burn existed")
		})
	}
}

func TestFunctional_BurnListEmpty(t *testing.T) {
	globals := testGlobals(t)

	listCmd := &BurnListCmd{}
	require.NoError(t, listCmd.Run(globals))
}

func TestFunctional_BurnListWithEntries(t *testing.T) {
	globals := testGlobals(t)

	for _, target := range []string{"pkg:npm/bad-1", "pkg:npm/bad-2"} {
		cmd := &BurnAddCmd{Target: target, Reason: "compromised"}
		require.NoError(t, cmd.Run(globals))
	}

	listCmd := &BurnListCmd{}
	require.NoError(t, listCmd.Run(globals))
}

// --- Analyze functional tests (mock collector, no network) ---

func TestFunctional_AnalyzeRefreshWithMock(t *testing.T) {
	globals := testGlobals(t, newMockCollector())

	cmd := &AnalyzeCmd{Target: "owner/repo", Refresh: true}
	require.NoError(t, cmd.Run(globals))

	// Verify signals were persisted. The entity was created with a
	// UUID ID, so we have to look it up via the canonical URI.
	s, err := store.OpenSQLite(t.Context(), globals.DBPath)
	require.NoError(t, err)
	defer s.Close()

	entity, err := s.FindEntityByURI(context.Background(), "repo:github/owner/repo")
	require.NoError(t, err)

	signals, err := s.GetSignals(context.Background(), entity.ID)
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

// TestFunctional_AnalyzeInputFormsCollapse verifies that three
// equivalent target forms all resolve to the SAME entity — no
// duplicate fragmentation (#53).
func TestFunctional_AnalyzeInputFormsCollapse(t *testing.T) {
	globals := testGlobals(t, newMockCollector())

	for _, target := range []string{
		"owner/repo",
		"github.com/owner/repo",
		"https://github.com/owner/repo",
	} {
		cmd := &AnalyzeCmd{Target: target, Refresh: true}
		require.NoError(t, cmd.Run(globals), "target %q should succeed", target)
	}

	// Only one entity should exist.
	s, err := store.OpenSQLite(t.Context(), globals.DBPath)
	require.NoError(t, err)
	defer s.Close()

	var count int
	require.NoError(t, s.DB().QueryRow(
		"SELECT COUNT(*) FROM entities WHERE canonical_uri = 'repo:github/owner/repo'").Scan(&count))
	assert.Equal(t, 1, count, "equivalent inputs should collapse to one entity")

	// Signals should accumulate — 3 calls × 2 signals = 6 rows.
	entity, err := s.FindEntityByURI(context.Background(), "repo:github/owner/repo")
	require.NoError(t, err)
	signals, err := s.GetSignals(context.Background(), entity.ID)
	require.NoError(t, err)
	assert.Len(t, signals, 6, "append-only: every refresh adds rows")
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
		Rationale: "Looks good", Version: "v1.0.0",
	}
	require.NoError(t, postureCmd.Run(globals))

	// Analyze with refresh.
	analyzeCmd := &AnalyzeCmd{Target: "owner/repo", Refresh: true}
	require.NoError(t, analyzeCmd.Run(globals))
}

// --- Audit log functional tests ---

// TestFunctional_AuditLogWrittenOnPostureSet verifies the full chain:
// running a posture-set command writes both a DB audit entry and a
// JSON-lines file entry.
func TestFunctional_AuditLogWrittenOnPostureSet(t *testing.T) {
	globals := testGlobals(t)

	require.NoError(t, (&PostureSetCmd{
		Target: "alecthomas/kong", Tier: "vetted-frozen",
		Rationale: "audited", Version: "v1.15.0",
	}).Run(globals))

	// DB side.
	s, err := store.OpenSQLite(t.Context(), globals.DBPath)
	require.NoError(t, err)
	defer s.Close()

	var count int
	require.NoError(t, s.DB().QueryRow(
		"SELECT COUNT(*) FROM audit_log WHERE action = 'set_posture'").Scan(&count))
	assert.Equal(t, 1, count, "audit log should have one set_posture entry in DB")

	// File side.
	data, err := readFileBytes(t, globals.AuditFilePath)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"action":"set_posture"`)
	assert.Contains(t, string(data), `"tier":"vetted-frozen"`)
}

func readFileBytes(t *testing.T, path string) ([]byte, error) {
	t.Helper()
	return os.ReadFile(path)
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
