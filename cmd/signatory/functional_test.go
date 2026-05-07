package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal"
	npmregistry "github.com/sarahmaeve/signatory/internal/signal/registry/npm"
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

// TestFunctional_PerVersionBurn_IsolatedFromRoot verifies the core
// M1 contract: burning a versioned URI (pkg:npm/invariant@2.2.4)
// creates a distinct entity row from the unversioned root
// (pkg:npm/invariant). The kong v1.14.0 case — "this one tag is
// bad, the package itself is fine" — depends on this isolation.
func TestFunctional_PerVersionBurn_IsolatedFromRoot(t *testing.T) {
	globals := testGlobals(t)

	// Burn only the specific version.
	burnVersion := &BurnAddCmd{
		Target: "pkg:npm/invariant@2.2.4",
		Reason: "orphaned tag; commit not reachable from master",
	}
	require.NoError(t, burnVersion.Run(globals))

	s, err := store.OpenSQLite(t.Context(), globals.DBPath)
	require.NoError(t, err)
	defer s.Close()

	// Two entities should exist... except only one does: the
	// versioned URI. The unversioned root is untouched.
	versionedEntity, err := s.FindEntityByURI(context.Background(), "pkg:npm/invariant@2.2.4")
	require.NoError(t, err, "versioned entity must exist after per-version burn")
	assert.Equal(t, "pkg:npm/invariant@2.2.4", versionedEntity.CanonicalURI)
	assert.Equal(t, profile.EntityPackage, versionedEntity.Type)

	// The unversioned root must NOT have been created as a side effect.
	_, err = s.FindEntityByURI(context.Background(), "pkg:npm/invariant")
	assert.ErrorIs(t, err, store.ErrNotFound,
		"per-version burn must not touch the unversioned root entity")

	// The burn record lives on the versioned entity.
	burn, err := s.GetBurn(context.Background(), versionedEntity.ID)
	require.NoError(t, err)
	assert.Contains(t, burn.Reason, "orphaned tag")
}

// TestFunctional_PerVersionBurn_DoesNotBurnOtherVersions verifies
// that burning v2.2.4 leaves v2.2.3 unburned. The two versions are
// independent identities; burning one must not propagate.
//
// Note: this test uses two BURNS (not one burn + one posture set)
// because Plan-A posture canonicalization (2026-04-21) routes
// `posture set X@V` to the unversioned entity — posture set no
// longer materializes a versioned entity row. Burn is still
// per-version-entity. Two burns exercise the per-version non-
// propagation invariant without depending on which storage model
// each verb uses.
func TestFunctional_PerVersionBurn_DoesNotBurnOtherVersions(t *testing.T) {
	globals := testGlobals(t)

	// Burn v2.2.4 and also create a row for v2.2.3 via a second
	// burn we'll immediately withdraw, so the entity exists without
	// an active burn.
	require.NoError(t, (&BurnAddCmd{
		Target: "pkg:npm/invariant@2.2.4",
		Reason: "orphaned tag",
	}).Run(globals))
	require.NoError(t, (&BurnAddCmd{
		Target: "pkg:npm/invariant@2.2.3",
		Reason: "placeholder — will be withdrawn",
	}).Run(globals))
	require.NoError(t, (&BurnRemoveCmd{
		Target: "pkg:npm/invariant@2.2.3",
		Reason: "entity creation only",
	}).Run(globals))

	s, err := store.OpenSQLite(t.Context(), globals.DBPath)
	require.NoError(t, err)
	defer s.Close()

	e24, err := s.FindEntityByURI(context.Background(), "pkg:npm/invariant@2.2.4")
	require.NoError(t, err)
	e23, err := s.FindEntityByURI(context.Background(), "pkg:npm/invariant@2.2.3")
	require.NoError(t, err)
	assert.NotEqual(t, e24.ID, e23.ID, "different versions must have different entity IDs")

	// v2.2.4 is burned; v2.2.3 was only created as a placeholder.
	_, err = s.GetBurn(context.Background(), e24.ID)
	require.NoError(t, err, "v2.2.4 burn must be retrievable")
	_, err = s.GetBurn(context.Background(), e23.ID)
	assert.ErrorIs(t, err, store.ErrNotFound,
		"v2.2.3 must NOT be burned — per-version burns are non-propagating")
}

// TestFunctional_PostureSet_RationaleFromFile verifies the M5
// file-form end-to-end: a multi-line rationale stored in a file
// lands in the DB verbatim (minus the editor-added trailing
// newline). Demonstrates the "agent writes file, passes path" flow
// that replaces bash heredoc invocations.
func TestFunctional_PostureSet_RationaleFromFile(t *testing.T) {
	globals := testGlobals(t)

	rationaleBody := "First-party Go-team module.\n\n" +
		"Load-bearing positives:\n" +
		"- sum.golang.org transparency log anchors publish path\n" +
		"- Ed25519 + SHA-256 throughout\n" +
		"- zero medium+ security findings\n\n" +
		"Caveats: one low (CreateFromVCS arg-injection; caller validation required)."
	rationalePath := filepath.Join(t.TempDir(), "rationale.md")
	require.NoError(t, os.WriteFile(rationalePath, []byte(rationaleBody+"\n"), 0o600))

	cmd := &PostureSetCmd{
		Target:        "pkg:golang/golang.org/x/mod@v0.35.0",
		Tier:          "vetted-frozen",
		RationaleFile: rationalePath,
	}
	require.NoError(t, cmd.Run(globals))

	s, err := store.OpenSQLite(t.Context(), globals.DBPath)
	require.NoError(t, err)
	defer s.Close()

	// Plan-A storage: `posture set X@V` routes the write to the
	// UNVERSIONED entity with the posture row's version column
	// populated. Look up by the stripped URI.
	entity, err := s.FindEntityByURI(context.Background(), "pkg:golang/golang.org/x/mod")
	require.NoError(t, err)
	postures, err := s.GetPostures(context.Background(), entity.ID)
	require.NoError(t, err)
	require.Len(t, postures, 1)
	assert.Equal(t, rationaleBody, postures[0].Rationale,
		"rationale should match the file contents verbatim (minus trailing newline)")
	assert.Equal(t, "v0.35.0", postures[0].Version)
}

// TestFunctional_PostureSet_RationaleConflict verifies the loud-
// failure case: both --rationale and --rationale-file set. The
// command errors before writing anything.
func TestFunctional_PostureSet_RationaleConflict(t *testing.T) {
	globals := testGlobals(t)

	path := filepath.Join(t.TempDir(), "rationale.txt")
	require.NoError(t, os.WriteFile(path, []byte("from file"), 0o600))

	cmd := &PostureSetCmd{
		Target:        "pkg:npm/lodash",
		Tier:          "vetted-frozen",
		Rationale:     "from flag",
		RationaleFile: path,
	}
	err := cmd.Run(globals)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "both set")

	// Nothing should have been written. The entity row MAY exist
	// (ensureEntity ran earlier? no — the rationale check runs
	// before ensureEntity now) but we check absence of posture rows.
	s, err := store.OpenSQLite(t.Context(), globals.DBPath)
	require.NoError(t, err)
	defer s.Close()
	_, findErr := s.FindEntityByURI(context.Background(), "pkg:npm/lodash")
	assert.ErrorIs(t, findErr, store.ErrNotFound,
		"conflict error must fire before any store write")
}

// TestFunctional_BurnAdd_ReasonFromFile covers the burn-side M5
// wiring — same pattern as posture, different flag names.
func TestFunctional_BurnAdd_ReasonFromFile(t *testing.T) {
	globals := testGlobals(t)

	reason := "Multi-paragraph explanation.\n\nThe orphan tag points\nto an unreachable commit."
	path := filepath.Join(t.TempDir(), "reason.txt")
	require.NoError(t, os.WriteFile(path, []byte(reason+"\n"), 0o600))

	cmd := &BurnAddCmd{
		Target:     "pkg:npm/invariant@2.2.4",
		ReasonFile: path,
	}
	require.NoError(t, cmd.Run(globals))

	s, err := store.OpenSQLite(t.Context(), globals.DBPath)
	require.NoError(t, err)
	defer s.Close()

	entity, err := s.FindEntityByURI(context.Background(), "pkg:npm/invariant@2.2.4")
	require.NoError(t, err)
	burn, err := s.GetBurn(context.Background(), entity.ID)
	require.NoError(t, err)
	assert.Equal(t, reason, burn.Reason)
}

// TestFunctional_BurnAddRemoveCycle covers the M4 happy path:
// add → remove → verify removed → add again → verify reactivated.
// This exercises the soft-delete semantics and the upsert-clears-
// withdrawal side-effect from SetBurn.
func TestFunctional_BurnAddRemoveCycle(t *testing.T) {
	globals := testGlobals(t)

	// Burn v2.2.4 for one reason.
	require.NoError(t, (&BurnAddCmd{
		Target: "pkg:npm/invariant@2.2.4",
		Reason: "orphaned tag",
	}).Run(globals))

	s, err := store.OpenSQLite(t.Context(), globals.DBPath)
	require.NoError(t, err)
	defer s.Close()
	entity, err := s.FindEntityByURI(t.Context(), "pkg:npm/invariant@2.2.4")
	require.NoError(t, err)

	// GetBurn surfaces the active burn.
	_, err = s.GetBurn(t.Context(), entity.ID)
	require.NoError(t, err, "burn must be retrievable while active")

	// Withdraw.
	require.NoError(t, (&BurnRemoveCmd{
		Target: "pkg:npm/invariant@2.2.4",
		Reason: "false positive; tag was fine",
	}).Run(globals))

	// GetBurn now returns ErrNotFound — the row is still there but
	// soft-deleted.
	_, err = s.GetBurn(t.Context(), entity.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "withdrawn burn must not surface as an active burn")

	// ListBurns excludes withdrawn rows.
	burns, err := s.ListBurns(t.Context())
	require.NoError(t, err)
	assert.Empty(t, burns, "ListBurns must exclude withdrawn rows")

	// Re-burn: SetBurn's upsert clears the withdrawal state.
	require.NoError(t, (&BurnAddCmd{
		Target: "pkg:npm/invariant@2.2.4",
		Reason: "confirmed malicious after all",
	}).Run(globals))
	reburn, err := s.GetBurn(t.Context(), entity.ID)
	require.NoError(t, err, "re-add must reactivate the burn")
	assert.Equal(t, "confirmed malicious after all", reburn.Reason)
}

// TestFunctional_PostureUnset covers the posture undo path. Set a
// posture, unset it, verify it's gone from both GetPosture and
// GetPostures. Then set a new one and confirm the upsert clears
// the withdrawal state.
func TestFunctional_PostureUnset(t *testing.T) {
	globals := testGlobals(t)

	require.NoError(t, (&PostureSetCmd{
		Target:    "pkg:npm/lodash@4.17.21",
		Tier:      "vetted-frozen",
		Rationale: "audited",
	}).Run(globals))

	s, err := store.OpenSQLite(t.Context(), globals.DBPath)
	require.NoError(t, err)
	defer s.Close()
	// Plan-A: posture set X@V routes to the unversioned entity with
	// version column populated.
	entity, err := s.FindEntityByURI(t.Context(), "pkg:npm/lodash")
	require.NoError(t, err)

	// Active posture present.
	_, err = s.GetPosture(t.Context(), entity.ID, "4.17.21")
	require.NoError(t, err)

	// Unset.
	require.NoError(t, (&PostureUnsetCmd{
		Target: "pkg:npm/lodash@4.17.21",
		Reason: "reassessment pending after CVE disclosure",
	}).Run(globals))

	// GetPosture now returns ErrNotFound.
	_, err = s.GetPosture(t.Context(), entity.ID, "4.17.21")
	assert.ErrorIs(t, err, store.ErrNotFound, "withdrawn posture must not surface as active")

	// GetPostures (active list) is empty.
	postures, err := s.GetPostures(t.Context(), entity.ID)
	require.NoError(t, err)
	assert.Empty(t, postures)

	// Re-set: SetPosture's upsert clears withdrawal.
	require.NoError(t, (&PostureSetCmd{
		Target:    "pkg:npm/lodash@4.17.21",
		Tier:      "trusted-for-now",
		Rationale: "re-evaluated; tier lowered pending review",
	}).Run(globals))
	p, err := s.GetPosture(t.Context(), entity.ID, "4.17.21")
	require.NoError(t, err)
	assert.Equal(t, profile.PostureTier("trusted-for-now"), p.Tier)
}

// TestFunctional_PostureUnset_NoExistingPosture verifies the error
// when nothing exists to withdraw. Caller gets a specific message
// rather than a silent no-op.
func TestFunctional_PostureUnset_NoExistingPosture(t *testing.T) {
	globals := testGlobals(t)

	cmd := &PostureUnsetCmd{Target: "pkg:npm/never-set", Reason: "probing"}
	err := cmd.Run(globals)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nothing to unset")
}

// TestFunctional_BurnRemove_NoExistingBurn same as above for burns.
func TestFunctional_BurnRemove_NoExistingBurn(t *testing.T) {
	globals := testGlobals(t)

	cmd := &BurnRemoveCmd{Target: "pkg:npm/never-burned", Reason: "probing"}
	err := cmd.Run(globals)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nothing to withdraw")
}

// TestFunctional_BurnAdd_DryRun verifies --dry-run writes nothing.
func TestFunctional_BurnAdd_DryRun(t *testing.T) {
	globals := testGlobals(t)

	require.NoError(t, (&BurnAddCmd{
		Target: "pkg:npm/test-dryrun",
		Reason: "preview only",
		DryRun: true,
	}).Run(globals))

	s, err := store.OpenSQLite(t.Context(), globals.DBPath)
	require.NoError(t, err)
	defer s.Close()
	_, err = s.FindEntityByURI(t.Context(), "pkg:npm/test-dryrun")
	assert.ErrorIs(t, err, store.ErrNotFound,
		"--dry-run must not create entity rows")
}

// TestFunctional_PostureSet_DryRun verifies --dry-run writes nothing.
func TestFunctional_PostureSet_DryRun(t *testing.T) {
	globals := testGlobals(t)

	require.NoError(t, (&PostureSetCmd{
		Target:    "pkg:npm/test-dryrun-posture@1.0",
		Tier:      "vetted-frozen",
		Rationale: "preview only",
		DryRun:    true,
	}).Run(globals))

	s, err := store.OpenSQLite(t.Context(), globals.DBPath)
	require.NoError(t, err)
	defer s.Close()
	_, err = s.FindEntityByURI(t.Context(), "pkg:npm/test-dryrun-posture@1.0")
	assert.ErrorIs(t, err, store.ErrNotFound,
		"--dry-run must not create entity rows")
}

// TestExitCode_UsageError_PostureSet covers EX_USAGE (64) routing:
// a conflict between --version and URI @V is a usage error, not a
// runtime error.
func TestExitCode_UsageError_PostureSet(t *testing.T) {
	globals := testGlobals(t)

	cmd := &PostureSetCmd{
		Target:    "pkg:npm/lodash@4.17.21",
		Tier:      "vetted-frozen",
		Rationale: "audited",
		Version:   "4.18.0", // disagrees
	}
	err := cmd.Run(globals)
	require.Error(t, err)
	assert.Equal(t, 64, exitCodeFor(err),
		"--version/@V conflict must surface as EX_USAGE (64)")
}

// TestFunctional_PostureSet_VersionedURI_InheritsVersion verifies
// the M1 inheritance path end-to-end: posture set on a @V URI
// without --version produces a stored Posture with Version = @V.
func TestFunctional_PostureSet_VersionedURI_InheritsVersion(t *testing.T) {
	globals := testGlobals(t)

	cmd := &PostureSetCmd{
		Target:    "pkg:npm/lodash@4.17.21",
		Tier:      "vetted-frozen",
		Rationale: "audited",
	}
	require.NoError(t, cmd.Run(globals))

	s, err := store.OpenSQLite(t.Context(), globals.DBPath)
	require.NoError(t, err)
	defer s.Close()

	// Plan-A: posture set X@V routes to the unversioned entity with
	// version column populated. The test asserts the canonical
	// storage form — entity at pkg:npm/lodash, posture.Version="4.17.21".
	entity, err := s.FindEntityByURI(context.Background(), "pkg:npm/lodash")
	require.NoError(t, err)
	postures, err := s.GetPostures(context.Background(), entity.ID)
	require.NoError(t, err)
	require.Len(t, postures, 1)
	assert.Equal(t, "4.17.21", postures[0].Version,
		"stored posture version must match the URI @V")
	assert.Equal(t, profile.PostureTier("vetted-frozen"), postures[0].Tier)
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

func TestFunctional_PostureListEmpty(t *testing.T) {
	globals := testGlobals(t)

	listCmd := &PostureListCmd{}
	require.NoError(t, listCmd.Run(globals))
}

func TestFunctional_PostureListWithEntries(t *testing.T) {
	globals := testGlobals(t)

	for _, target := range []string{"pkg:npm/alpha", "pkg:npm/beta"} {
		cmd := &PostureSetCmd{
			Target: target, Tier: "trusted-for-now",
			Rationale: "looks fine", Version: "1.0.0",
		}
		require.NoError(t, cmd.Run(globals))
	}

	listCmd := &PostureListCmd{}
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

// TestFunctional_AnalyzeNpm_EndToEnd exercises the full Phase A
// flow in one shot:
//
//  1. A target "pkg:npm/express" reaches AnalyzeCmd.Run.
//  2. ResolveTarget classifies it as pkg-scheme + ecosystem=npm.
//  3. Entity is created with EntityPackage, Ecosystem=npm, URL="".
//  4. A.5's resolveNpmRepo hits the httptest npm registry, pulls
//     repository.url, normalizes it, stamps the github URL on the
//     entity.
//  5. Both the real npm collector (talking to httptest) and a
//     mock github-ish collector run; each emits signals into the
//     store.
//  6. Audit log records the analyze action.
//
// If ANY layer is broken, this test fails. This is the single
// proof-of-coherence for Phase A — every intervening unit test
// covers a slice; this one covers the whole stack.
func TestFunctional_AnalyzeNpm_EndToEnd(t *testing.T) {
	// Not t.Parallel: we're serializing to keep the httptest lifecycle
	// tight; parallelism adds no value at this test's cost.

	// npm mock — multiplexes the registry endpoint (/<name>) and
	// the downloads endpoint (/downloads/point/last-week/<name>)
	// on the same server. Real npm splits these across two hosts
	// (registry.npmjs.org + api.npmjs.org); our test client is
	// configured with a single base URL that covers both.
	npmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/downloads/") {
			fmt.Fprint(w, `{"downloads":28500000,"start":"2026-04-13","end":"2026-04-20","package":"express"}`)
			return
		}
		fmt.Fprint(w, `{
		  "name": "express",
		  "dist-tags": {"latest": "4.18.2"},
		  "time": {
		    "created": "2010-12-29T19:38:25.450Z",
		    "4.18.2": "2022-10-08T19:08:35.000Z"
		  },
		  "maintainers": [
		    {"name": "dougwilson", "email": "doug@somethingdoug.com"}
		  ],
		  "versions": {
		    "4.18.2": {"scripts": {}, "dist": {"attestations": null}}
		  },
		  "repository": {
		    "type": "git",
		    "url": "git+https://github.com/expressjs/express.git"
		  }
		}`)
	}))
	defer npmSrv.Close()

	// Real npm collector, pointed at the httptest server via the
	// dependency-injection entry NewCollectorWithClient.
	realNpmCollector := npmregistry.NewCollectorWithClient(
		npmregistry.NewClientWithBaseURL(npmSrv.URL))

	// Mock github-ish collector emitting two canned signals —
	// stands in for the real github + git collectors without HTTP or
	// local clone plumbing. We're testing analyze orchestration, not
	// github collection behavior (which has its own thorough tests).
	mockGH := &mockCollector{
		name: "github-mock",
		signals: []profile.Signal{
			{Type: "stars", Group: profile.SignalGroupCriticality, Source: "github-mock",
				ForgeryResistance: profile.ForgeryMediumDeclining,
				Value:             json.RawMessage(`{"count":63000}`),
				CollectedAt:       time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour)},
			{Type: "last_commit", Group: profile.SignalGroupVitality, Source: "github-mock",
				ForgeryResistance: profile.ForgeryMediumDeclining,
				Value:             json.RawMessage(`{"days_ago":14}`),
				CollectedAt:       time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour)},
		},
	}

	dir := t.TempDir()
	globals := &Globals{
		DBPath:         filepath.Join(dir, "test.db"),
		Collectors:     []signal.Collector{realNpmCollector, mockGH},
		AuditFilePath:  filepath.Join(dir, "audit.log"),
		NpmRegistryURL: npmSrv.URL,
	}

	cmd := &AnalyzeCmd{Target: "pkg:npm/express", Refresh: true}
	require.NoError(t, cmd.Run(globals))

	// ---- Verify persisted state. ----

	s, err := store.OpenSQLite(t.Context(), globals.DBPath)
	require.NoError(t, err)
	defer s.Close()

	// Entity: correct URI, type, ecosystem, resolved URL.
	entity, err := s.FindEntityByURI(context.Background(), "pkg:npm/express")
	require.NoError(t, err)
	assert.Equal(t, profile.EntityPackage, entity.Type,
		"npm target must yield EntityPackage")
	assert.Equal(t, "npm", entity.Ecosystem)
	assert.Equal(t, "express", entity.ShortName)
	assert.Equal(t, "https://github.com/expressjs/express", entity.URL,
		"A.5 should stamp the normalized github URL on the entity")

	// Signals: both npm and github-mock rows present.
	signals, err := s.GetSignals(context.Background(), entity.ID)
	require.NoError(t, err)

	// Map to source → types for readable assertions.
	bySource := map[string][]string{}
	for _, sig := range signals {
		bySource[sig.Source] = append(bySource[sig.Source], sig.Type)
	}
	assert.Contains(t, bySource, "npm-registry", "npm collector signals must be stored")
	assert.Contains(t, bySource["npm-registry"], "last_publish",
		"npm collector should emit last_publish")
	assert.Contains(t, bySource, "github-mock", "github-mock signals must be stored")
	assert.Contains(t, bySource["github-mock"], "stars")
	assert.Contains(t, bySource["github-mock"], "last_commit")

	// Audit log: analyze action recorded for the entity.
	auditFile, err := os.ReadFile(globals.AuditFilePath)
	require.NoError(t, err)
	assert.Contains(t, string(auditFile), `"action":"analyze"`)
	assert.Contains(t, string(auditFile), entity.ID,
		"audit log should carry the entity's UUID")
}

// TestFunctional_AnalyzeNpm_NoRepoDeclared exercises A.5's
// graceful-degradation path: an npm package whose registry entry
// doesn't declare a repository URL. The entity should be created
// with empty URL, A.5 should silently return (empty is not an
// error), the analyze invocation should succeed, and the npm
// collector should still emit signals.
//
// NOT a test of collectorsFor's skip-github-when-URL-empty
// contract: because globals.Collectors is injected, the production
// collectorsFor never runs. That contract is pinned separately by
// TestCollectorsFor_NpmEntityWithoutURL_ReturnsOnlyNpm in
// collectors_test.go, which exercises collectorsFor directly.
func TestFunctional_AnalyzeNpm_NoRepoDeclared(t *testing.T) {
	npmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/downloads/") {
			fmt.Fprint(w, `{"downloads":1,"start":"a","end":"b","package":"orphan"}`)
			return
		}
		fmt.Fprint(w, `{
		  "name": "orphan",
		  "dist-tags": {"latest": "1.0.0"},
		  "time": {"1.0.0": "2024-01-01T00:00:00Z"},
		  "versions": {"1.0.0": {"scripts": {}, "dist": {}}}
		}`)
	}))
	defer npmSrv.Close()

	npmCollector := npmregistry.NewCollectorWithClient(
		npmregistry.NewClientWithBaseURL(npmSrv.URL))

	dir := t.TempDir()
	globals := &Globals{
		DBPath:         filepath.Join(dir, "test.db"),
		Collectors:     []signal.Collector{npmCollector},
		AuditFilePath:  filepath.Join(dir, "audit.log"),
		NpmRegistryURL: npmSrv.URL,
	}

	cmd := &AnalyzeCmd{Target: "pkg:npm/orphan", Refresh: true}
	require.NoError(t, cmd.Run(globals),
		"absent repository.url is not an error — just leaves URL empty")

	s, err := store.OpenSQLite(t.Context(), globals.DBPath)
	require.NoError(t, err)
	defer s.Close()

	entity, err := s.FindEntityByURI(context.Background(), "pkg:npm/orphan")
	require.NoError(t, err)
	assert.Empty(t, entity.URL,
		"entity URL should stay empty when registry declares no github repo")

	// npm signal still landed — A.5 failing or returning empty
	// doesn't block collection.
	signals, err := s.GetSignals(context.Background(), entity.ID)
	require.NoError(t, err)
	require.NotEmpty(t, signals)
}

// TestFunctional_Analyze_JSONStdoutIsPureJSON locks in the review's
// highest-ROI CLI fix: in --json mode, stdout must contain ONLY the
// JSON payload so a caller piping to `jq` parses cleanly. Progress
// lines (collector summaries, "Collecting signals for…") go to
// stderr; the payload alone lands on stdout. A regression that
// routes progress to stdout breaks every machine consumer.
func TestFunctional_Analyze_JSONStdoutIsPureJSON(t *testing.T) {
	t.Parallel()

	var stdoutBuf, stderrBuf bytes.Buffer
	globals := testGlobals(t, newMockCollector())

	cmd := &AnalyzeCmd{
		Target:  "owner/repo",
		Refresh: true,
		JSON:    true,
		Stdout:  &stdoutBuf,
		Stderr:  &stderrBuf,
	}
	require.NoError(t, cmd.Run(globals))

	// Stdout must parse as JSON top-to-bottom — no preamble.
	var parsed AnalysisDisplay
	require.NoError(t, json.Unmarshal(stdoutBuf.Bytes(), &parsed),
		"stdout in --json mode must be parseable JSON; got %q", stdoutBuf.String())
	assert.Equal(t, "repo:github/owner/repo", parsed.Entity.CanonicalURI)

	// Stderr must carry the progress chatter.
	stderrStr := stderrBuf.String()
	assert.Contains(t, stderrStr, "Collecting signals for:",
		"progress line must be on stderr, not stdout")
	assert.Contains(t, stderrStr, "[mock]",
		"per-collector summary must be on stderr")

	// Explicit negative: progress text must NOT appear on stdout.
	assert.NotContains(t, stdoutBuf.String(), "Collecting signals for:",
		"stdout in --json mode must stay pure JSON — no progress contamination")
}

// TestFunctional_Analyze_NoCacheMessagesOnStderr covers the cached-
// miss branch: when analyze has nothing to report (no cache, no
// --refresh), it exits 0 with the "try --refresh" instructions on
// stderr and an EMPTY stdout. Scripts that expect `signatory analyze
// foo --json` to either emit JSON or emit nothing get the right
// behavior.
func TestFunctional_Analyze_NoCacheMessagesOnStderr(t *testing.T) {
	t.Parallel()

	var stdoutBuf, stderrBuf bytes.Buffer
	globals := testGlobals(t, newMockCollector())

	cmd := &AnalyzeCmd{
		Target: "never-analyzed-org/never-analyzed-repo",
		Stdout: &stdoutBuf,
		Stderr: &stderrBuf,
	}
	require.NoError(t, cmd.Run(globals))

	assert.Empty(t, stdoutBuf.String(),
		"no cached data + no --refresh → nothing to report on stdout")
	assert.Contains(t, stderrBuf.String(), "No cached data for:",
		"diagnostic explaining why stdout is empty belongs on stderr")
	assert.Contains(t, stderrBuf.String(), "Run with --refresh",
		"instructional message belongs on stderr")
}

// TestFunctional_AnalyzeNpm_ScopedPackage confirms a scoped package
// name flows through ResolveTarget, entity creation, and the npm
// collector without losing the @scope/ prefix. Canonical URI stays
// pkg:npm/@types/node; ShortName is @types/node (not "node"); the
// npm collector hits /@types/node on the registry.
func TestFunctional_AnalyzeNpm_ScopedPackage(t *testing.T) {
	var seenRegistryPath string
	npmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/downloads/") {
			fmt.Fprint(w, `{"downloads":1,"start":"a","end":"b","package":"@types/node"}`)
			return
		}
		seenRegistryPath = r.URL.Path
		fmt.Fprint(w, `{
		  "name": "@types/node",
		  "dist-tags": {"latest": "20.0.0"},
		  "time": {"20.0.0": "2024-01-01T00:00:00Z"},
		  "versions": {"20.0.0": {"scripts": {}, "dist": {}}}
		}`)
	}))
	defer npmSrv.Close()

	npmCollector := npmregistry.NewCollectorWithClient(
		npmregistry.NewClientWithBaseURL(npmSrv.URL))

	dir := t.TempDir()
	globals := &Globals{
		DBPath:         filepath.Join(dir, "test.db"),
		Collectors:     []signal.Collector{npmCollector},
		AuditFilePath:  filepath.Join(dir, "audit.log"),
		NpmRegistryURL: npmSrv.URL,
	}

	cmd := &AnalyzeCmd{Target: "pkg:npm/@types/node", Refresh: true}
	require.NoError(t, cmd.Run(globals))

	assert.Equal(t, "/@types/node", seenRegistryPath,
		"scoped package request path must preserve the @scope/name form")

	s, err := store.OpenSQLite(t.Context(), globals.DBPath)
	require.NoError(t, err)
	defer s.Close()

	entity, err := s.FindEntityByURI(context.Background(), "pkg:npm/@types/node")
	require.NoError(t, err)
	assert.Equal(t, "@types/node", entity.ShortName,
		"scope must be preserved on entity.ShortName — '@types/node', not 'node'")
	assert.Equal(t, "npm", entity.Ecosystem)
}
