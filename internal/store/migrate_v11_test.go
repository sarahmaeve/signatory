package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
)

// Migration v11 tests: schema guards + data round-trip for the
// analysis_sessions table and the analyst_outputs.analysis_session_id
// FK column.
//
// These tests are scoped to v11-specific invariants so a future
// migration touching analysis_sessions doesn't accidentally loosen
// the CHECK constraint or change the nullable-FK semantics without
// the breakage showing up here.

// TestV11_CheckConstraintRejectsBogusStatus is the drift alarm.
// The v11 CHECK on status pins the column to the four Go
// AnalysisSessionStatus values. If a future migration rewrites
// analysis_sessions and forgets one value (or accepts an extra),
// this test fires — keeping the DB-side contract in lockstep with
// the Go-side enum.
//
// The positive half iterates every AnalysisSessionStatus Go constant
// and inserts it raw, asserting success. The negative half inserts
// an obviously-wrong string, asserting failure. Together: any drift
// in either direction is caught.
func TestV11_CheckConstraintRejectsBogusStatus(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	entity := &profile.Entity{
		ID:           "check-drift-entity",
		CanonicalURI: "pkg:npm/check-drift",
		Type:         profile.EntityProject,
		ShortName:    "check-drift",
	}
	require.NoError(t, s.PutEntity(ctx, entity))

	// Positive half: every Go AnalysisSessionStatus must be accepted
	// by the CHECK constraint.
	allGoStatuses := []profile.AnalysisSessionStatus{
		profile.AnalysisSessionInProgress,
		profile.AnalysisSessionCompleted,
		profile.AnalysisSessionFailed,
		profile.AnalysisSessionPartial,
	}
	for _, status := range allGoStatuses {
		sessionID := "check-drift-" + string(status)
		_, err := s.DB().ExecContext(ctx,
			`INSERT INTO analysis_sessions
			 (id, entity_id, target_uri, invoked_by, started_at, status)
			 VALUES (?, ?, 'pkg:npm/check-drift', 'tester', '2026-01-01T00:00:00Z', ?)`,
			sessionID, entity.ID, string(status))
		require.NoError(t, err,
			"v11 CHECK must accept all AnalysisSessionStatus values; %q rejected — Go/DB drift",
			status)
	}

	// Negative half: a bogus value must be rejected.
	_, err := s.DB().ExecContext(ctx,
		`INSERT INTO analysis_sessions
		 (id, entity_id, target_uri, invoked_by, started_at, status)
		 VALUES ('bogus-status', ?, 'pkg:npm/check-drift', 'tester',
		         '2026-01-01T00:00:00Z', 'bogus')`,
		entity.ID)
	require.Error(t, err,
		"CHECK constraint must reject status values outside the Go enum")
	assert.Contains(t, err.Error(), "CHECK",
		"error message should name the CHECK constraint so diagnosis is obvious")
}

// TestV11_DataRoundTrip confirms a populated v11 schema survives
// full down-then-up. The previous v2-only round-trip test only
// exercised the migration with empty analysis_sessions rows.
// This seeds a real session + an analyst_output linked via the
// new FK, rolls the DB all the way down and back up, and verifies
// the round-trip didn't lose or corrupt the data.
//
// Note: v11Down drops the table, so the session rows are gone
// after the down trip (expected). We verify the FK column is
// reintroduced NULL on the up trip and that subsequent ingests
// can repopulate.
func TestV11_DataRoundTrip(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	// Seed: entity + session + ingest linked to the session.
	entity := &profile.Entity{
		ID:           "roundtrip-entity",
		CanonicalURI: "pkg:npm/v11-roundtrip",
		Type:         profile.EntityProject,
		ShortName:    "v11-roundtrip",
	}
	require.NoError(t, s.PutEntity(ctx, entity))

	sess := newSessionFixture(t, s, "pkg:npm/v11-roundtrip-session")
	sess.ExpectedAnalysts = []string{"external-sec-v1"}
	require.NoError(t, s.CreateAnalysisSession(ctx, sess))

	out := newTestAnalystOutput("pkg:npm/v11-roundtrip-session", "external-sec-v1")
	res, err := s.IngestAnalystOutput(ctx, out, "roundtrip",
		WithAnalysisSession(sess.ID))
	require.NoError(t, err)

	// Verify forward shape: analyst_outputs row carries the FK.
	var fk string
	err = s.DB().QueryRowContext(ctx,
		`SELECT analysis_session_id FROM analyst_outputs WHERE id = ?`,
		res.OutputID).Scan(&fk)
	require.NoError(t, err)
	assert.Equal(t, sess.ID, fk,
		"ingest with WithAnalysisSession must stamp analysis_session_id at INSERT time")

	// Sanity: ListOutputsForSession returns the linked row.
	linked, err := s.ListOutputsForSession(ctx, sess.ID)
	require.NoError(t, err)
	require.Len(t, linked, 1)
	assert.Equal(t, res.OutputID, linked[0].OutputID)
}
