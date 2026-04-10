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

// TestAnalyze_CorruptedEntityErrorNotSwallowed verifies that a
// non-ErrNotFound error from FindEntityByURI is surfaced to the user,
// not silently ignored. (Review 3, C1)
func TestAnalyze_CorruptedEntityErrorNotSwallowed(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := store.OpenSQLite(dbPath)
	require.NoError(t, err)

	// Insert an entity with a corrupted timestamp so the entity read
	// will return a parse error (not ErrNotFound).
	_, err = s.DB().ExecContext(context.Background(),
		`INSERT INTO entities (id, canonical_uri, type, short_name, description, ecosystem, url, created_at, updated_at)
		 VALUES ('corrupt-id', 'repo:github/corrupt/corrupt', 'project', 'corrupt/corrupt', '', '', 'https://github.com/corrupt/corrupt', 'INVALID', '2026-01-01T00:00:00Z')`)
	require.NoError(t, err)
	s.Close()

	// Now try to analyze — the error should propagate, not be swallowed.
	mock := &mockCollector{
		name: "mock",
		signals: []profile.Signal{{
			ID:                "mock-sig",
			Type:              "stars",
			Group:             profile.SignalGroupCriticality,
			Source:            "mock",
			ForgeryResistance: profile.ForgeryMediumDeclining,
			Value:             json.RawMessage(`{"count":1}`),
			CollectedAt:       time.Now(), ExpiresAt: time.Now().Add(time.Hour),
		}},
	}
	globals := &Globals{
		DBPath:        dbPath,
		Collectors:    []signal.Collector{mock},
		AuditFilePath: filepath.Join(t.TempDir(), "audit.log"),
	}

	cmd := &AnalyzeCmd{Target: "corrupt/corrupt", Refresh: true}
	err = cmd.Run(globals)

	// The error should NOT be nil — the corrupted timestamp should
	// be surfaced, not silently ignored.
	assert.Error(t, err, "corrupted entity read should produce an error, not be silently swallowed")
}

// TestAnalyze_DisplayProfileErrorNotSwallowed verifies that corrupted
// posture timestamps are surfaced when displaying a profile.
// (Review 3, H4)
func TestAnalyze_DisplayProfileErrorNotSwallowed(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := store.OpenSQLite(dbPath)
	require.NoError(t, err)

	ctx := context.Background()

	// Insert a valid entity with v2 schema.
	entity := &profile.Entity{
		ID:           "display-test",
		CanonicalURI: "repo:github/display/test",
		Type:         profile.EntityProject,
		ShortName:    "display/test",
		URL:          "https://github.com/display/test",
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}
	require.NoError(t, s.PutEntity(ctx, entity))

	// Insert a signal so there's cached data.
	require.NoError(t, s.AppendSignals(ctx, []profile.Signal{{
		ID: "sig-1", EntityID: "display-test", Type: "stars",
		Group: profile.SignalGroupCriticality, Source: "test",
		ForgeryResistance: profile.ForgeryHigh,
		Value:             json.RawMessage(`{}`),
		CollectedAt:       time.Now().UTC(),
		ExpiresAt:         time.Now().UTC().Add(time.Hour),
	}}))

	// Insert a posture with corrupted timestamp via raw SQL.
	_, err = s.DB().ExecContext(ctx,
		`INSERT INTO postures (entity_id, version, tier, rationale, set_by, set_at)
		 VALUES ('display-test', '', 'trusted-for-now', 'test', 'team:sarah', 'CORRUPTED')`)
	require.NoError(t, err)
	s.Close()

	// Analyze without refresh — should read from cache and hit the
	// corrupted posture.
	globals := &Globals{
		DBPath:        dbPath,
		AuditFilePath: filepath.Join(t.TempDir(), "audit.log"),
	}
	cmd := &AnalyzeCmd{Target: "display/test", Refresh: false}
	err = cmd.Run(globals)

	assert.Error(t, err, "corrupted posture timestamp should produce an error, not be silently ignored")
}
