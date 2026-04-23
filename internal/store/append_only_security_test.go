package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
)

// TestSecurity_AppendOnly_SignalsBlocksMutation verifies that the signals
// table rejects UPDATE and DELETE statements at the schema level. Without
// schema enforcement the append-only invariant documented at sqlite.go:8
// is convention only — any future code path, errant query, or attacker
// with database access could rewrite signal history. Issue #79.
func TestSecurity_AppendOnly_SignalsBlocksMutation(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	entity := testEntity("ent-1", "pkg:npm/example", "example", now)
	require.NoError(t, s.PutEntity(ctx, entity))

	require.NoError(t, s.AppendSignals(ctx, []profile.Signal{{
		ID: "sig-1", EntityID: "ent-1", Type: "stars",
		Group: profile.SignalGroupCriticality, Source: "github",
		ForgeryResistance: profile.ForgeryMediumDeclining,
		Value:             json.RawMessage(`{"count":42}`),
		CollectedAt:       now, ExpiresAt: now.Add(time.Hour),
	}}))

	_, err := s.db.ExecContext(ctx,
		`UPDATE signals SET value = ? WHERE id = ?`, `{"count":9999}`, "sig-1")
	require.Error(t, err, "UPDATE on signals must be blocked at the schema level")

	_, err = s.db.ExecContext(ctx,
		`DELETE FROM signals WHERE id = ?`, "sig-1")
	require.Error(t, err, "DELETE on signals must be blocked at the schema level")

	var value string
	require.NoError(t, s.db.QueryRowContext(ctx,
		`SELECT value FROM signals WHERE id = ?`, "sig-1").Scan(&value))
	assert.Equal(t, `{"count":42}`, value, "signal value must be unchanged after blocked UPDATE")
}

// TestSecurity_AppendOnly_DependencyObservationsBlocksMutation verifies
// that the dependency_observations table rejects UPDATE and DELETE.
// Dependency surveys must be tamper-evident so a malicious update
// cannot rewrite which versions were in use at survey time. Issue #79.
func TestSecurity_AppendOnly_DependencyObservationsBlocksMutation(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	project := testEntity("proj-1", "repo:github/acme/myapp", "myapp", now)
	project.Type = profile.EntityProject
	dep := testEntity("dep-1", "pkg:npm/lodash", "lodash", now)
	require.NoError(t, s.PutEntity(ctx, project))
	require.NoError(t, s.PutEntity(ctx, dep))

	require.NoError(t, s.AppendDependencyObservations(ctx, []profile.DependencyObservation{{
		ID: "obs-1", ProjectID: "proj-1", EntityID: "dep-1",
		Version: "4.17.21", Direct: true, ObservedAt: now, SurveyID: "s1",
	}}))

	_, err := s.db.ExecContext(ctx,
		`UPDATE dependency_observations SET version = ? WHERE id = ?`, "0.0.0", "obs-1")
	require.Error(t, err, "UPDATE on dependency_observations must be blocked")

	_, err = s.db.ExecContext(ctx,
		`DELETE FROM dependency_observations WHERE id = ?`, "obs-1")
	require.Error(t, err, "DELETE on dependency_observations must be blocked")

	var version string
	require.NoError(t, s.db.QueryRowContext(ctx,
		`SELECT version FROM dependency_observations WHERE id = ?`, "obs-1").Scan(&version))
	assert.Equal(t, "4.17.21", version, "observation version must be unchanged after blocked UPDATE")
}

// TestSecurity_AppendOnly_SignalResolutionsBlocksMutation verifies that
// the signal_resolutions table rejects UPDATE and DELETE. Resolutions
// are the conflict-resolution audit trail for signal disagreements;
// rewriting them would let an attacker change which signals were kept
// vs superseded after the fact. Issue #79.
func TestSecurity_AppendOnly_SignalResolutionsBlocksMutation(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	entity := testEntity("ent-1", "pkg:npm/example", "example", now)
	require.NoError(t, s.PutEntity(ctx, entity))

	require.NoError(t, s.AppendSignals(ctx, []profile.Signal{
		{ID: "kept", EntityID: "ent-1", Type: "stars",
			Group: profile.SignalGroupCriticality, Source: "github",
			ForgeryResistance: profile.ForgeryMediumDeclining,
			Value:             json.RawMessage(`{}`),
			CollectedAt:       now, ExpiresAt: now.Add(time.Hour)},
		{ID: "superseded", EntityID: "ent-1", Type: "stars",
			Group: profile.SignalGroupCriticality, Source: "github",
			ForgeryResistance: profile.ForgeryMediumDeclining,
			Value:             json.RawMessage(`{}`),
			CollectedAt:       now, ExpiresAt: now.Add(time.Hour)},
	}))

	require.NoError(t, s.AppendResolution(ctx, &profile.SignalResolution{
		ID: "res-1", EntityID: "ent-1", SignalType: "stars",
		KeptSignalID: "kept", SupersededSignalID: "superseded",
		Action: "keep_previous", ResolvedBy: "team:sarah", ResolvedAt: now,
	}))

	_, err := s.db.ExecContext(ctx,
		`UPDATE signal_resolutions SET action = ? WHERE id = ?`, "tampered", "res-1")
	require.Error(t, err, "UPDATE on signal_resolutions must be blocked")

	_, err = s.db.ExecContext(ctx,
		`DELETE FROM signal_resolutions WHERE id = ?`, "res-1")
	require.Error(t, err, "DELETE on signal_resolutions must be blocked")

	var action string
	require.NoError(t, s.db.QueryRowContext(ctx,
		`SELECT action FROM signal_resolutions WHERE id = ?`, "res-1").Scan(&action))
	assert.Equal(t, "keep_previous", action, "resolution action must be unchanged after blocked UPDATE")
}

// TestSecurity_AppendOnly_AuditLogBlocksMutation verifies that the
// audit_log table rejects UPDATE and DELETE. The audit log is the most
// sensitive append-only table — its forensic value depends entirely on
// the inability to silently rewrite past entries. The package doc at
// audit/logger.go:7-12 promises survival under database compromise; that
// promise is meaningless without schema-level enforcement. Issue #79.
func TestSecurity_AppendOnly_AuditLogBlocksMutation(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	require.NoError(t, s.AppendAuditEntry(ctx, &profile.AuditEntry{
		ID: "audit-1", Timestamp: now, Actor: "team:sarah",
		Action: "set_posture", Detail: `{"version":"1.0.0","tier":"vetted-frozen"}`,
	}))

	_, err := s.db.ExecContext(ctx,
		`UPDATE audit_log SET detail = ? WHERE id = ?`, `{"tampered":true}`, "audit-1")
	require.Error(t, err, "UPDATE on audit_log must be blocked at the schema level")

	_, err = s.db.ExecContext(ctx,
		`DELETE FROM audit_log WHERE id = ?`, "audit-1")
	require.Error(t, err, "DELETE on audit_log must be blocked at the schema level")

	var detail string
	require.NoError(t, s.db.QueryRowContext(ctx,
		`SELECT detail FROM audit_log WHERE id = ?`, "audit-1").Scan(&detail))
	assert.Equal(t, `{"version":"1.0.0","tier":"vetted-frozen"}`, detail,
		"audit detail must be unchanged after blocked UPDATE")
}

// --- citations.parent_kind CHECK constraint (F002) --------------------------
//
// The citations table uses a polymorphic-FK pattern: parent_kind +
// parent_id together name the row being cited, but SQLite has no
// way to express a polymorphic foreign key. F002 from
// design/analysis/signatory-security-v1.json called out that the
// schema imposed NO check on parent_kind, so the Go layer's
// discipline (only ever inserting one of three literal constants)
// was the only thing keeping malformed or orphan citations out of
// the table. A stray tx.Exec with a typo'd parent_kind string
// would land silently and produce rows that every later query
// would fail to match.
//
// Migration v9 adds a CHECK constraint that pins parent_kind to
// the three production values: 'conclusion', 'positive_absence',
// 'observation'. The fourth value the schema comment historically
// mentioned ('methodology_pattern') was never inserted and the
// MethodologyPattern type has no Citations field — it was a
// stale comment claiming a relationship that doesn't exist, not
// a legitimate value being used somewhere.
//
// The pre-v5 legacy value 'finding' is also rejected post-v9; v9's
// Up migration includes a one-time UPDATE that rewrites any extant
// 'finding' rows to 'conclusion' before installing the CHECK, so
// the rebuild's INSERT INTO citations_new doesn't fail on legacy
// data.

// TestSecurity_CitationsCheckParentKind_RejectsUnknown verifies that
// an arbitrary unknown parent_kind value (simulating a future typo
// or corrupted insert) is rejected at the schema layer, not just by
// the Go code.
//
// Revert proof: remove the CHECK clause from migrationV9Up's
// citations_new CREATE TABLE; this test fails because the INSERT
// with parent_kind='xxx' succeeds.
func TestSecurity_CitationsCheckParentKind_RejectsUnknown(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO citations (id, parent_kind, parent_id, seq)
		 VALUES (?, ?, ?, ?)`,
		"cite-xxx", "xxx", "parent-1", 0)
	require.Error(t, err,
		"INSERT with unknown parent_kind must be rejected by the CHECK constraint")
	assert.Contains(t, err.Error(), "CHECK",
		"error should name the CHECK constraint so future debugging points at the schema invariant, not a Go-layer bug")
}

// TestSecurity_CitationsCheckParentKind_RejectsLegacyFinding pins
// that the pre-v5 'finding' value is no longer accepted. Guards
// against a future refactor that tries to accept both names for
// backward compatibility — the whole point of v5 was to make
// 'conclusion' the one true name.
//
// Revert proof: add 'finding' to the CHECK list; this test fails
// because the INSERT succeeds.
func TestSecurity_CitationsCheckParentKind_RejectsLegacyFinding(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO citations (id, parent_kind, parent_id, seq)
		 VALUES (?, ?, ?, ?)`,
		"cite-legacy", "finding", "parent-1", 0)
	require.Error(t, err,
		"INSERT with pre-v5 legacy 'finding' parent_kind must be rejected — the rename was total")
	assert.Contains(t, err.Error(), "CHECK")
}

// TestSecurity_CitationsCheckParentKind_AcceptsValid is the
// positive-path companion: the three production values must all
// pass the CHECK. Without this companion, the implementation could
// regress to an over-restrictive CHECK (e.g., only 'conclusion')
// and the rejection tests above would still pass — a false-secure
// mode the Go layer would trip on during real ingest.
func TestSecurity_CitationsCheckParentKind_AcceptsValid(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	for i, kind := range []string{"conclusion", "positive_absence", "observation"} {
		_, err := s.db.ExecContext(ctx,
			`INSERT INTO citations (id, parent_kind, parent_id, seq)
			 VALUES (?, ?, ?, ?)`,
			"cite-valid-"+kind, kind, "parent-x", i)
		assert.NoErrorf(t, err,
			"INSERT with legitimate parent_kind %q must succeed", kind)
	}
}

// TestSecurity_Citations_AppendOnlyTriggersSurviveV9 is the
// regression guard for the v9 trigger-ceremony bug discovered
// during dogfood install: the v9 rebuild would drop+recreate
// the citations table, losing the v3-installed append-only
// triggers unless v9 explicitly reinstalls them on the new table.
//
// A v9 that forgot to reinstall the triggers would still pass all
// CHECK-constraint tests because those only exercise INSERT. This
// test fires on UPDATE and DELETE, which are exactly what the
// triggers block — and which would silently start succeeding if
// the triggers didn't survive.
//
// Revert proof: delete the two `CREATE TRIGGER citations_no_...`
// stanzas at the end of migrationV9Up; this test fails because
// UPDATE/DELETE now succeed against a citations table that lost
// its append-only invariant after the v9 rebuild.
func TestSecurity_Citations_AppendOnlyTriggersSurviveV9(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	// Insert a legitimate citation so there's a row to try to
	// mutate. Goes through direct SQL (not insertCitations) because
	// this test is specifically about the schema-level enforcement,
	// not the Go path.
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO citations (id, parent_kind, parent_id, seq)
		 VALUES (?, ?, ?, ?)`,
		"cite-trigger-test", "conclusion", "parent-1", 0)
	require.NoError(t, err)

	_, err = s.db.ExecContext(ctx,
		`UPDATE citations SET parent_id = ? WHERE id = ?`, "tampered", "cite-trigger-test")
	require.Error(t, err, "UPDATE on citations must be blocked by the append-only trigger after v9")
	assert.Contains(t, err.Error(), "append-only",
		"error should name the append-only invariant")

	_, err = s.db.ExecContext(ctx,
		`DELETE FROM citations WHERE id = ?`, "cite-trigger-test")
	require.Error(t, err, "DELETE on citations must be blocked by the append-only trigger after v9")
	assert.Contains(t, err.Error(), "append-only")
}

// TestMigration_V9_CleansLegacyFindingOnRebuild is the regression
// guard for the v9 bug discovered during dogfood install: the
// cleanup UPDATE (pre-v5 'finding' → post-v5 'conclusion') tripped
// the citations append-only trigger and aborted the whole v9
// transaction, leaving real-world DBs stuck at v8.
//
// Test shape: migrate fresh DB to v9, back off to v8, insert a
// legacy 'finding' row directly (legal at v8 schema — no CHECK,
// and INSERT isn't trigger-blocked), then re-migrate to v9. v9
// must succeed and the row must come out the other side rewritten
// to 'conclusion'.
//
// Revert proof: remove the two `DROP TRIGGER IF EXISTS` lines at
// the start of migrationV9Up; this test fails with "constraint
// failed: citations are append-only" when v9's cleanup UPDATE hits
// the still-active citations_no_update trigger.
func TestMigration_V9_CleansLegacyFindingOnRebuild(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	// Back off to v8 so the CHECK is gone and we can insert a
	// 'finding' row. v9Down reinstalls the append-only triggers,
	// but INSERT isn't trigger-blocked (only UPDATE/DELETE are).
	// Walk down step-by-step from the latest version — currently
	// v10, previously v9. The loop keeps this test robust when new
	// migrations land above v9 without changing v9's cleanup
	// semantics, which is what this regression is protecting.
	for i := len(migrations); i > 8; i-- {
		require.NoError(t, migrateDown(ctx, s.db, ""),
			"rollback from v%d must succeed to reach v8", i)
	}
	version, err := getCurrentVersion(ctx, s.db)
	require.NoError(t, err)
	require.Equal(t, 8, version, "fixture: must be at v8 before inserting legacy data")

	// Insert a citation with the pre-v5 'finding' parent_kind.
	// This represents a row that was written before v5's
	// Finding→Conclusion rename — exactly the shape v9's cleanup
	// UPDATE exists to handle.
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO citations (id, parent_kind, parent_id, seq)
		 VALUES (?, ?, ?, ?)`,
		"cite-legacy", "finding", "parent-ancient", 0)
	require.NoError(t, err, "INSERT with pre-v5 parent_kind must succeed at v8 schema")

	// Re-migrate forward. v9's cleanup rewrites the 'finding' row
	// before the CHECK rebuild fires, working around the citations
	// append-only trigger via explicit DROP/CREATE around the
	// UPDATE. Pre-fix this failed with "citations are append-only".
	// Re-migrate lands us on whatever latest is (v10+ as new
	// migrations arrive); the important post-condition is that the
	// legacy row survived v9's rewrite.
	require.NoError(t, migrate(ctx, s.db, ""),
		"v9 must succeed even with pre-v5 legacy data in citations")

	version, err = getCurrentVersion(ctx, s.db)
	require.NoError(t, err)
	require.Equal(t, len(migrations), version,
		"should land at latest after full re-migration")

	var kind string
	require.NoError(t, s.db.QueryRowContext(ctx,
		`SELECT parent_kind FROM citations WHERE id = ?`, "cite-legacy").
		Scan(&kind))
	assert.Equal(t, "conclusion", kind,
		"legacy 'finding' row must be rewritten to 'conclusion' by v9's cleanup UPDATE")
}
