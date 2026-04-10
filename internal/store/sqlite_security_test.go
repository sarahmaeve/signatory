package store

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
)

// TestSecurity_DBFile_AtomicallyCreatedWith0600 verifies that the
// SQLite database file is created with mode 0600 *atomically*, with
// no window during which it could be world-readable. Issue #83: the
// previous OpenSQLite flow was sql.Open → db.Ping → os.Chmod, where
// db.Ping creates the file with the process's default umask (typically
// 0644) and the subsequent os.Chmod narrows it to 0600. Between those
// two calls, another user on a multi-user system could read the file
// — and the parent directory's restrictive 0700 permissions only
// close that window if the parent was created by this MkdirAll call;
// MkdirAll does not narrow the perms of an existing parent dir.
//
// The TOCTOU window is unobservable from a normal black-box test
// because the chmod runs synchronously before OpenSQLite returns. To
// make the bug observable, we inject a chmodFunc that captures the
// file's permissions immediately before delegating to the real
// os.Chmod. The captured mode is what an attacker reading the file
// during the window would have seen.
//
// Pre-fix: chmodFunc captures whatever sql.Open's underlying open(2)
// produced (driver-dependent, typically 0644 or 0666 with default
// umask). The test fails because the captured mode != 0600.
//
// Post-fix: OpenSQLite pre-creates the file via os.OpenFile with
// mode 0600 before handing the path to sql.Open. By the time chmodFunc
// runs, the file is already at 0600. The chmod becomes defense in
// depth (covers existing files that had looser perms when OpenSQLite
// was first called).
func TestSecurity_DBFile_AtomicallyCreatedWith0600(t *testing.T) {
	// Pre-create a parent directory with deliberately loose perms.
	// MkdirAll inside OpenSQLite will not narrow these — that's the
	// precondition the TOCTOU window relies on.
	dir := t.TempDir()
	parent := filepath.Join(dir, "loose-parent")
	require.NoError(t, os.MkdirAll(parent, 0755))

	// Hook chmodFunc so we can see what mode the database file has at
	// the moment os.Chmod would normally narrow it. Restore in defer
	// regardless of test outcome — global state must not leak.
	var capturedMode os.FileMode
	origChmodFunc := chmodFunc
	chmodFunc = func(name string, mode os.FileMode) error {
		info, err := os.Stat(name)
		if err == nil {
			capturedMode = info.Mode().Perm()
		}
		return origChmodFunc(name, mode)
	}
	defer func() { chmodFunc = origChmodFunc }()

	path := filepath.Join(parent, "test.db")
	s, err := OpenSQLite(path)
	require.NoError(t, err)
	defer s.Close()

	// THE CRITICAL ASSERTION: the database file must have been at 0600
	// before the chmod ran. If it wasn't, the file was world-readable
	// for some non-zero window, and an attacker on a multi-user system
	// could have read it.
	assert.Equal(t, os.FileMode(0600), capturedMode,
		"DB file must be 0600 BEFORE chmod runs — closing the TOCTOU window during which the file would be world-readable on multi-user systems with loose parent dir perms")
}

// TestSecurity_GetLatestSignals_IsolatesEntities verifies that the
// signal_resolutions filter inside GetLatestSignals is entity-scoped.
// Issue #91: the previous query had `id NOT IN (SELECT
// superseded_signal_id FROM signal_resolutions)` with no entity_id
// filter on the subquery, so a resolution belonging to entity A could
// silently hide signals for entity B if the IDs collided.
//
// The current github/absence collectors generate IDs that include the
// entity ID as a substring, so cross-entity collisions are effectively
// impossible through the application layer today. But the query's
// correctness should not depend on the collector's ID-generation
// scheme — it should be structurally entity-scoped at the SQL level.
//
// This test exercises the query directly with hand-crafted IDs that
// bypass the collector's coincidental defense, demonstrating the bug
// even though it isn't currently reachable from production code paths.
func TestSecurity_GetLatestSignals_IsolatesEntities(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	// Two entities, each with one signal of the same type.
	entA := testEntity("ent-A", "pkg:npm/a-pkg", "a-pkg", now)
	entB := testEntity("ent-B", "pkg:npm/b-pkg", "b-pkg", now)
	require.NoError(t, s.PutEntity(ctx, entA))
	require.NoError(t, s.PutEntity(ctx, entB))

	// Hand-crafted signal IDs that don't follow the github collector's
	// {source}:{entity_id}:{type}:{ts} convention. The IDs are unique
	// (signals.id is a global PRIMARY KEY) but the strings happen to
	// not encode the entity, simulating a future collector with a
	// different ID scheme — or an external signal ingest pipeline.
	require.NoError(t, s.AppendSignals(ctx, []profile.Signal{
		{
			ID: "sig-A-keeper", EntityID: "ent-A", Type: "stars",
			Group: profile.SignalGroupCriticality, Source: "github",
			ForgeryResistance: profile.ForgeryMediumDeclining,
			Value:             json.RawMessage(`{"count":1}`),
			CollectedAt:       now, ExpiresAt: now.Add(time.Hour),
		},
		{
			ID: "sig-B-victim", EntityID: "ent-B", Type: "stars",
			Group: profile.SignalGroupCriticality, Source: "github",
			ForgeryResistance: profile.ForgeryMediumDeclining,
			Value:             json.RawMessage(`{"count":2}`),
			CollectedAt:       now, ExpiresAt: now.Add(time.Hour),
		},
	}))

	// A malformed resolution: it claims to belong to ent-A but its
	// SupersededSignalID points at ent-B's signal. AppendResolution
	// does not validate cross-entity consistency (the FK only requires
	// that the signal IDs exist, not that they belong to the resolution's
	// entity). So this insert succeeds — and the global subquery in
	// the unfixed GetLatestSignals would then hide ent-B's signal from
	// GetLatestSignals(ent-B).
	require.NoError(t, s.AppendResolution(ctx, &profile.SignalResolution{
		ID:                 "res-cross",
		EntityID:           "ent-A",
		SignalType:         "stars",
		KeptSignalID:       "sig-A-keeper",
		SupersededSignalID: "sig-B-victim",
		Action:             "keep_previous",
		ResolvedBy:         "team:attacker",
		ResolvedAt:         now,
	}))

	// THE CRITICAL ASSERTION: ent-B's signal must still be visible in
	// its own latest-signals view. If this returns 0 signals, the
	// resolution from ent-A has silently hidden it — which is the bug.
	latestB, err := s.GetLatestSignals(ctx, "ent-B")
	require.NoError(t, err)
	require.Len(t, latestB, 1, "ent-B's signal must not be hidden by a resolution belonging to ent-A")
	assert.Equal(t, "sig-B-victim", latestB[0].ID)
	assert.Equal(t, "ent-B", latestB[0].EntityID)
}

// TestSecurity_PutEntity_RejectsMalformedCanonicalURI verifies that
// PutEntity refuses to persist entities whose CanonicalURI contains
// dangerous characters, unknown schemes, excessive length, or other
// shapes that could be used for log injection, lookalike fragmentation,
// or display spoofing. This is the persistence-boundary defense for
// issue #78 — even if a CLI command, library caller, or future code
// path forgets to validate input, the store rejects the bad data.
//
// Each row also asserts that no entity row was written by the rejected
// PutEntity call. The validation must happen before the INSERT.
func TestSecurity_PutEntity_RejectsMalformedCanonicalURI(t *testing.T) {
	tests := []struct {
		name string
		uri  string
	}{
		{"unknown scheme", "evil:payload"},
		{"no scheme at all", "foo/bar"},
		{"scheme only with no body", "pkg:"},
		{"control char NUL", "pkg:npm/foo\x00bar"},
		{"control char newline", "pkg:npm/foo\nbar"},
		{"control char tab", "pkg:npm/foo\tbar"},
		{"DEL char", "pkg:npm/foo\x7fbar"},
		{"non-ASCII Cyrillic lookalike", "pkg:npm/lod\u0430sh"}, // Cyrillic а
		{"leading whitespace", " pkg:npm/express"},
		{"trailing newline", "pkg:npm/express\n"},
		{"too long", "pkg:npm/" + strings.Repeat("x", 600)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestDB(t)
			ctx := context.Background()
			now := time.Now().UTC().Truncate(time.Second)

			entity := &profile.Entity{
				ID:           "ent-bad",
				CanonicalURI: tt.uri,
				Type:         profile.EntityPackage,
				ShortName:    "bad-entity",
				CreatedAt:    now,
				UpdatedAt:    now,
			}
			err := s.PutEntity(ctx, entity)
			require.Error(t, err, "PutEntity must reject malformed canonical URI")

			// The bad row must NOT have landed in the entities table —
			// validation has to happen before the INSERT, not after.
			_, err = s.GetEntity(ctx, "ent-bad")
			require.Error(t, err, "rejected entity must not be readable back")
			assert.ErrorIs(t, err, ErrNotFound)
		})
	}
}

// TestSecurity_GetEntity_CorruptedTimestampReturnsError verifies that a
// malformed created_at column produces an error rather than silently
// substituting a placeholder time. The previous version of this test
// (issue #80) wrapped its assertion in `if err == nil { ... }`, which
// meant the test passed against a regression that returned time.Now().
// This version uses require.Error so any non-error path fails the test.
func TestSecurity_GetEntity_CorruptedTimestampReturnsError(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO entities (id, canonical_uri, type, short_name, description, ecosystem, url, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"corrupt-entity", "pkg:npm/corrupt", "package", "corrupt", "", "", "",
		"not-a-valid-date", time.Now().UTC().Format(time.RFC3339))
	require.NoError(t, err)

	_, err = s.GetEntity(ctx, "corrupt-entity")
	require.Error(t, err, "GetEntity must return an error on corrupted created_at")
	assert.NotErrorIs(t, err, ErrNotFound, "corruption must not be reported as not-found")
}

// TestSecurity_GetBurn_CorruptedTimestampReturnsError verifies that a
// malformed burned_at column in the burns table produces an error rather
// than silently substituting a placeholder time. A burn rendering as
// time.Now() would be interpreted as a fresh ban; a burn rendering as
// the zero time would be interpreted as ancient history. Both are wrong
// and could lead an LLM agent or human reviewer to act on bogus data.
func TestSecurity_GetBurn_CorruptedTimestampReturnsError(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339)

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO entities (id, canonical_uri, type, short_name, description, ecosystem, url, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"burn-entity", "pkg:npm/burned", "package", "burned", "", "", "", now, now)
	require.NoError(t, err)

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO burns (entity_id, reason, source, source_org, burned_at, burned_by)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"burn-entity", "compromised", "local", "", "CORRUPTED", "sarah")
	require.NoError(t, err)

	_, err = s.GetBurn(ctx, "burn-entity")
	require.Error(t, err, "GetBurn must return an error on corrupted burned_at")
	assert.NotErrorIs(t, err, ErrNotFound, "corruption must not be reported as not-found")
}

// TestSecurity_ListBurns_CorruptedTimestampReturnsError covers the second
// burns timestamp parse path. ListBurns is independent from GetBurn (it
// scans rows in a loop instead of a single row), and it could regress
// separately, so it needs its own regression gate. The error must
// propagate out of the loop — it must not be swallowed and silently
// produce a partial result with valid rows only.
func TestSecurity_ListBurns_CorruptedTimestampReturnsError(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Format(time.RFC3339)

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO entities (id, canonical_uri, type, short_name, description, ecosystem, url, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"burn-entity", "pkg:npm/burned", "package", "burned", "", "", "", now, now)
	require.NoError(t, err)

	_, err = s.db.ExecContext(ctx,
		`INSERT INTO burns (entity_id, reason, source, source_org, burned_at, burned_by)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"burn-entity", "compromised", "local", "", "CORRUPTED", "sarah")
	require.NoError(t, err)

	_, err = s.ListBurns(ctx)
	require.Error(t, err, "ListBurns must return an error on corrupted burned_at")
}

// TestSecurity_GetTeamIdentity_CorruptedTimestampReturnsError covers the
// team_identities created_at parse path. Team identities anchor the
// audit log's actor field; a team identity rendering as time.Now() or
// the zero time would corrupt downstream "when did this team start
// operating?" logic and could let a forged team identity blend in.
func TestSecurity_GetTeamIdentity_CorruptedTimestampReturnsError(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO team_identities (id, name, created_at, halted_at, revoked_at, revoke_reason)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"team-corrupt", "sarah+claude", "INVALID-TIMESTAMP", nil, nil, nil)
	require.NoError(t, err)

	_, err = s.GetTeamIdentity(ctx, "team-corrupt")
	require.Error(t, err, "GetTeamIdentity must return an error on corrupted created_at")
	assert.NotErrorIs(t, err, ErrNotFound, "corruption must not be reported as not-found")
}

// TestSecurity_ErrorsIsUsedForSentinels verifies that error comparison
// works correctly even when errors are wrapped (issue #32).
func TestSecurity_ErrorsIsUsedForSentinels(t *testing.T) {
	// This test verifies the contract: callers should be able to use
	// errors.Is to check for ErrNotFound even if the store wraps it.
	// Currently the store returns bare sentinels, but this test ensures
	// the pattern works regardless.

	s := newTestDB(t)
	ctx := context.Background()

	_, err := s.GetEntity(ctx, "nonexistent")
	require.Error(t, err)

	// errors.Is must work — not just ==
	assert.ErrorIs(t, err, ErrNotFound,
		"GetEntity should return an error matchable via errors.Is(err, ErrNotFound)")

	_, err = s.GetPosture(ctx, "nonexistent", "")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound)

	_, err = s.GetBurn(ctx, "nonexistent")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound)

	// Nil input errors should also be matchable.
	err = s.PutEntity(ctx, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNilInput)

	err = s.SetPosture(ctx, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNilInput)

	err = s.SetBurn(ctx, nil)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNilInput)
}
