package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSecurity_CorruptedTimestampReturnsError verifies that malformed
// timestamps in the database produce an error rather than silently
// returning zero-value times (issue #31).
func TestSecurity_CorruptedTimestampReturnsError(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	// Insert an entity with a valid timestamp first.
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO entities (id, type, name, ecosystem, url, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"corrupt-entity", "package", "corrupt", "", "",
		"not-a-valid-date", time.Now().UTC().Format(time.RFC3339))
	require.NoError(t, err)

	// Reading this entity should return an error, NOT a zero-value time.
	entity, err := s.GetEntity(ctx, "corrupt-entity")
	if err == nil {
		// If no error, the time should NOT be zero (which would mean
		// the parse error was silently swallowed).
		assert.False(t, entity.CreatedAt.IsZero(),
			"corrupted timestamp was silently converted to zero time — data corruption hidden")
	}
	// If err != nil, that's the correct behavior — we detected corruption.
}

// TestSecurity_CorruptedBurnTimestampReturnsError verifies burn timestamps.
func TestSecurity_CorruptedBurnTimestampReturnsError(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	// Insert entity first.
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO entities (id, type, name, ecosystem, url, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"burn-entity", "package", "burned", "", "",
		time.Now().UTC().Format(time.RFC3339), time.Now().UTC().Format(time.RFC3339))
	require.NoError(t, err)

	// Insert a burn with a corrupted timestamp.
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO burns (entity_id, reason, source, source_org, burned_at, burned_by)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"burn-entity", "compromised", "local", "", "CORRUPTED", "sarah")
	require.NoError(t, err)

	burn, err := s.GetBurn(ctx, "burn-entity")
	if err == nil {
		assert.False(t, burn.BurnedAt.IsZero(),
			"corrupted burn timestamp was silently converted to zero time — "+
				"downstream logic checking 'was this burn recent?' will make wrong decisions")
	}
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

	_, err = s.GetPosture(ctx, "nonexistent")
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

// Ensure the db field is accessible for raw SQL in tests.
func rawDB(s *SQLite) *sql.DB {
	return s.db
}
