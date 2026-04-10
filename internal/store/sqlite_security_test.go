package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
)

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
