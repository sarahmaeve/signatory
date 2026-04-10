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
