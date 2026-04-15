package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/store"
)

// TestSecurity_EnsureEntity_FailsClosedOnUnparseableInput verifies that
// ensureEntity refuses to create entities from input that is neither a
// parseable GitHub repo nor a valid canonical URI. Issue #78: the prior
// fallback at posture.go:249-263 silently treated arbitrary text as a
// canonical URI, allowing log injection, lookalike fragmentation, and
// stored display-spoofing primitives.
//
// Each row also asserts that no entity was persisted by the rejected
// call — failing closed means no side effects.
func TestSecurity_EnsureEntity_FailsClosedOnUnparseableInput(t *testing.T) {
	tests := []struct {
		name   string
		target string
	}{
		{"unknown scheme", "evil:payload"},
		{"control char NUL", "pkg:npm/foo\x00bar"},
		{"control char newline", "pkg:npm/foo\nbar"},
		{"non-ASCII Cyrillic lookalike", "pkg:npm/lod\u0430sh"}, // Cyrillic а
		{"plain garbage with no scheme", "garbage with spaces"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := store.OpenSQLite(t.Context(), filepath.Join(t.TempDir(), "test.db"))
			require.NoError(t, err)
			defer s.Close()

			ctx := context.Background()
			entity, err := ensureEntity(ctx, s, tt.target)
			require.Error(t, err, "ensureEntity must fail closed on unparseable input")
			assert.Nil(t, entity, "no entity should be returned on failure")

			// Confirm nothing was persisted under the bad target.
			_, err = s.FindEntityByURI(ctx, tt.target)
			require.Error(t, err, "no entity should exist for the rejected target")
			assert.ErrorIs(t, err, store.ErrNotFound)
		})
	}
}
