package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
)

func newTestDB(t *testing.T) *SQLite {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	s, err := OpenSQLite(path)
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	return s
}

// testEntity constructs a fully-populated v2 entity with the given id
// and canonical URI. Tests should prefer this helper over inline
// literals — v2 PutEntity validates canonical_uri, so every Entity
// literal needs it, and factoring it out keeps the test body focused.
func testEntity(id, uri, shortName string, now time.Time) *profile.Entity {
	return &profile.Entity{
		ID:           id,
		CanonicalURI: uri,
		Type:         profile.EntityPackage,
		ShortName:    shortName,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
}

func TestOpenSQLite_CreatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "deep", "test.db")
	s, err := OpenSQLite(path)
	require.NoError(t, err)
	defer s.Close()

	_, err = os.Stat(path)
	assert.NoError(t, err, "database file should exist")
}

func TestOpenSQLite_IdempotentMigration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	s1, err := OpenSQLite(path)
	require.NoError(t, err)
	s1.Close()

	// Opening the same database again should not fail.
	s2, err := OpenSQLite(path)
	require.NoError(t, err)
	s2.Close()
}

func TestEntityRoundTrip(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	entity := &profile.Entity{
		ID:           "ent-1",
		CanonicalURI: "pkg:npm/express",
		Type:         profile.EntityPackage,
		ShortName:    "express",
		Description:  "Fast, unopinionated web framework for Node.js",
		Ecosystem:    "npm",
		URL:          "https://github.com/expressjs/express",
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	require.NoError(t, s.PutEntity(ctx, entity))

	got, err := s.GetEntity(ctx, "ent-1")
	require.NoError(t, err)
	assert.Equal(t, entity.ID, got.ID)
	assert.Equal(t, entity.CanonicalURI, got.CanonicalURI)
	assert.Equal(t, entity.Type, got.Type)
	assert.Equal(t, entity.ShortName, got.ShortName)
	assert.Equal(t, entity.Description, got.Description)
	assert.Equal(t, entity.Ecosystem, got.Ecosystem)
	assert.Equal(t, entity.URL, got.URL)
	assert.Equal(t, entity.CreatedAt.Unix(), got.CreatedAt.Unix())
}

func TestEntityUpdate(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	entity := testEntity("ent-1", "pkg:npm/express", "express", now)
	require.NoError(t, s.PutEntity(ctx, entity))

	entity.URL = "https://github.com/expressjs/express"
	entity.Description = "now with description"
	entity.UpdatedAt = now.Add(time.Hour)
	require.NoError(t, s.PutEntity(ctx, entity))

	got, err := s.GetEntity(ctx, "ent-1")
	require.NoError(t, err)
	assert.Equal(t, "https://github.com/expressjs/express", got.URL)
	assert.Equal(t, "now with description", got.Description)
}

func TestGetEntity_NotFound(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	_, err := s.GetEntity(ctx, "nonexistent")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestPutEntity_NilReturnsError(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	err := s.PutEntity(ctx, nil)
	assert.ErrorIs(t, err, ErrNilInput)
}

func TestFindEntityByURI(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	entity := testEntity("ent-1", "pkg:npm/express", "express", now)
	require.NoError(t, s.PutEntity(ctx, entity))

	got, err := s.FindEntityByURI(ctx, "pkg:npm/express")
	require.NoError(t, err)
	assert.Equal(t, "ent-1", got.ID)
}

func TestFindEntityByURI_NotFound(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	_, err := s.FindEntityByURI(ctx, "pkg:npm/nonexistent")
	assert.ErrorIs(t, err, ErrNotFound)
}

// TestFindEntityByURI_UniquenessEnforced verifies the unique index on
// canonical_uri prevents two entities from sharing the same URI.
func TestFindEntityByURI_UniquenessEnforced(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	first := testEntity("ent-1", "pkg:npm/express", "express", now)
	require.NoError(t, s.PutEntity(ctx, first))

	duplicate := testEntity("ent-2", "pkg:npm/express", "express-dupe", now)
	err := s.PutEntity(ctx, duplicate)
	assert.Error(t, err, "inserting a second entity with the same canonical_uri should fail")
}

func TestAppendSignals_RoundTrip(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	entity := testEntity("ent-1", "pkg:npm/express", "express", now)
	require.NoError(t, s.PutEntity(ctx, entity))

	signals := []profile.Signal{
		{
			ID:                "sig-1",
			EntityID:          "ent-1",
			Type:              "last_commit",
			Group:             profile.SignalGroupVitality,
			Source:            "github",
			ForgeryResistance: profile.ForgeryHigh,
			Value:             json.RawMessage(`{"date":"2026-04-01"}`),
			CollectedAt:       now,
			ExpiresAt:         now.Add(24 * time.Hour),
		},
		{
			ID:                "sig-2",
			EntityID:          "ent-1",
			Type:              "stars",
			Group:             profile.SignalGroupCriticality,
			Source:            "github",
			ForgeryResistance: profile.ForgeryMediumDeclining,
			Value:             json.RawMessage(`{"count":65000}`),
			CollectedAt:       now,
			ExpiresAt:         now.Add(24 * time.Hour),
		},
	}

	require.NoError(t, s.AppendSignals(ctx, signals))

	got, err := s.GetSignals(ctx, "ent-1")
	require.NoError(t, err)
	assert.Len(t, got, 2)
}

// TestAppendSignals_IsAppendOnly verifies that a second collection adds
// new rows rather than overwriting the first — this is the core
// semantic shift from v1.
func TestAppendSignals_IsAppendOnly(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	entity := testEntity("ent-1", "pkg:npm/express", "express", now)
	require.NoError(t, s.PutEntity(ctx, entity))

	// First collection.
	require.NoError(t, s.AppendSignals(ctx, []profile.Signal{{
		ID: "sig-early", EntityID: "ent-1", Type: "stars",
		Group: profile.SignalGroupCriticality, Source: "github",
		ForgeryResistance: profile.ForgeryMediumDeclining,
		Value:             json.RawMessage(`{"count":1000}`), CollectedAt: now, ExpiresAt: now.Add(time.Hour),
	}}))

	// Second collection an hour later — different ID, same type.
	require.NoError(t, s.AppendSignals(ctx, []profile.Signal{{
		ID: "sig-later", EntityID: "ent-1", Type: "stars",
		Group: profile.SignalGroupCriticality, Source: "github",
		ForgeryResistance: profile.ForgeryMediumDeclining,
		Value:             json.RawMessage(`{"count":1100}`), CollectedAt: now.Add(time.Hour), ExpiresAt: now.Add(2 * time.Hour),
	}}))

	all, err := s.GetSignals(ctx, "ent-1")
	require.NoError(t, err)
	require.Len(t, all, 2, "append-only: both signals must be preserved")
}

// TestAppendSignals_DuplicateIDFails verifies that the store rejects a
// signal whose ID already exists — append-only means append-only.
func TestAppendSignals_DuplicateIDFails(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	entity := testEntity("ent-1", "pkg:npm/express", "express", now)
	require.NoError(t, s.PutEntity(ctx, entity))

	sig := profile.Signal{
		ID: "dup", EntityID: "ent-1", Type: "stars",
		Group: profile.SignalGroupCriticality, Source: "github",
		ForgeryResistance: profile.ForgeryMediumDeclining,
		Value:             json.RawMessage(`{}`), CollectedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	require.NoError(t, s.AppendSignals(ctx, []profile.Signal{sig}))

	err := s.AppendSignals(ctx, []profile.Signal{sig})
	assert.Error(t, err, "re-inserting a signal with the same ID should fail")
}

func TestGetLatestSignals_ReturnsNewestPerTypeSource(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	entity := testEntity("ent-1", "pkg:npm/express", "express", now)
	require.NoError(t, s.PutEntity(ctx, entity))

	require.NoError(t, s.AppendSignals(ctx, []profile.Signal{
		{
			ID: "old-stars", EntityID: "ent-1", Type: "stars",
			Group: profile.SignalGroupCriticality, Source: "github",
			ForgeryResistance: profile.ForgeryMediumDeclining,
			Value:             json.RawMessage(`{"count":1000}`),
			CollectedAt:       now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour),
		},
		{
			ID: "new-stars", EntityID: "ent-1", Type: "stars",
			Group: profile.SignalGroupCriticality, Source: "github",
			ForgeryResistance: profile.ForgeryMediumDeclining,
			Value:             json.RawMessage(`{"count":1100}`),
			CollectedAt:       now, ExpiresAt: now.Add(time.Hour),
		},
	}))

	latest, err := s.GetLatestSignals(ctx, "ent-1")
	require.NoError(t, err)
	require.Len(t, latest, 1, "one signal per (type, source)")
	assert.Equal(t, "new-stars", latest[0].ID)
}

// TestGetLatestSignals_MultipleSourcesCoexist verifies that latest-per-
// (type, source) returns BOTH sources when they differ — federation
// support per the v2 design.
func TestGetLatestSignals_MultipleSourcesCoexist(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	entity := testEntity("ent-1", "pkg:npm/express", "express", now)
	require.NoError(t, s.PutEntity(ctx, entity))

	require.NoError(t, s.AppendSignals(ctx, []profile.Signal{
		{
			ID: "gh-stars", EntityID: "ent-1", Type: "stars",
			Group: profile.SignalGroupCriticality, Source: "github",
			ForgeryResistance: profile.ForgeryMediumDeclining,
			Value:             json.RawMessage(`{"count":1100}`),
			CollectedAt:       now, ExpiresAt: now.Add(time.Hour),
		},
		{
			ID: "peer-stars", EntityID: "ent-1", Type: "stars",
			Group: profile.SignalGroupCriticality, Source: "peer:acme",
			ForgeryResistance: profile.ForgeryMediumDeclining,
			Value:             json.RawMessage(`{"count":1150}`),
			CollectedAt:       now, ExpiresAt: now.Add(time.Hour),
		},
	}))

	latest, err := s.GetLatestSignals(ctx, "ent-1")
	require.NoError(t, err)
	assert.Len(t, latest, 2, "both sources should appear in latest view")
}

// TestGetLatestSignals_ExcludesSuperseded verifies that signals marked
// as superseded by an AppendResolution call are filtered out.
func TestGetLatestSignals_ExcludesSuperseded(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	entity := testEntity("ent-1", "pkg:npm/express", "express", now)
	require.NoError(t, s.PutEntity(ctx, entity))

	require.NoError(t, s.AppendSignals(ctx, []profile.Signal{
		{
			ID: "keeper", EntityID: "ent-1", Type: "stars",
			Group: profile.SignalGroupCriticality, Source: "github",
			ForgeryResistance: profile.ForgeryMediumDeclining,
			Value:             json.RawMessage(`{"count":1000}`),
			CollectedAt:       now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour),
		},
		{
			ID: "rejected", EntityID: "ent-1", Type: "stars",
			Group: profile.SignalGroupCriticality, Source: "github",
			ForgeryResistance: profile.ForgeryMediumDeclining,
			Value:             json.RawMessage(`{"count":1100}`),
			CollectedAt:       now, ExpiresAt: now.Add(time.Hour),
		},
	}))

	require.NoError(t, s.AppendResolution(ctx, &profile.SignalResolution{
		ID:                 "res-1",
		EntityID:           "ent-1",
		SignalType:         "stars",
		KeptSignalID:       "keeper",
		SupersededSignalID: "rejected",
		Action:             "keep_previous",
		ResolvedBy:         "team:sarah",
		ResolvedAt:         now,
	}))

	latest, err := s.GetLatestSignals(ctx, "ent-1")
	require.NoError(t, err)
	require.Len(t, latest, 1)
	assert.Equal(t, "keeper", latest[0].ID, "superseded signal must be filtered out")
}

func TestGetSignalsByGroup(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	entity := testEntity("ent-1", "pkg:npm/express", "express", now)
	require.NoError(t, s.PutEntity(ctx, entity))

	signals := []profile.Signal{
		{ID: "sig-1", EntityID: "ent-1", Type: "last_commit",
			Group: profile.SignalGroupVitality, Source: "github",
			ForgeryResistance: profile.ForgeryHigh,
			Value:             json.RawMessage(`{}`), CollectedAt: now, ExpiresAt: now.Add(time.Hour)},
		{ID: "sig-2", EntityID: "ent-1", Type: "stars",
			Group: profile.SignalGroupCriticality, Source: "github",
			ForgeryResistance: profile.ForgeryMediumDeclining,
			Value:             json.RawMessage(`{}`), CollectedAt: now, ExpiresAt: now.Add(time.Hour)},
	}
	require.NoError(t, s.AppendSignals(ctx, signals))

	got, err := s.GetSignalsByGroup(ctx, "ent-1", profile.SignalGroupVitality)
	require.NoError(t, err)
	assert.Len(t, got, 1)
	assert.Equal(t, "last_commit", got[0].Type)
}

func TestGetSignals_Empty(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	got, err := s.GetSignals(ctx, "nonexistent")
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestPostureRoundTrip(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	entity := testEntity("ent-1", "pkg:npm/express", "express", now)
	require.NoError(t, s.PutEntity(ctx, entity))

	posture := &profile.Posture{
		EntityID:  "ent-1",
		Tier:      profile.PostureTrustedForNow,
		Version:   "4.18.2",
		Rationale: "Strong vitality, no anomalies",
		SetBy:     "team:sarah+claude",
		SetAt:     now,
	}
	require.NoError(t, s.SetPosture(ctx, posture))

	got, err := s.GetPosture(ctx, "ent-1", "4.18.2")
	require.NoError(t, err)
	assert.Equal(t, profile.PostureTrustedForNow, got.Tier)
	assert.Equal(t, "4.18.2", got.Version)
	assert.Equal(t, "Strong vitality, no anomalies", got.Rationale)
	assert.Equal(t, "team:sarah+claude", got.SetBy)
}

// TestPostureVersionsCoexist verifies the v2 core: two versions of the
// same entity get independent posture decisions, neither overwrites
// the other.
func TestPostureVersionsCoexist(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	entity := testEntity("ent-1", "pkg:npm/express", "express", now)
	require.NoError(t, s.PutEntity(ctx, entity))

	v1 := &profile.Posture{
		EntityID: "ent-1", Version: "4.18.2",
		Tier: profile.PostureVettedFrozen, Rationale: "audited",
		SetBy: "team:sarah", SetAt: now,
	}
	v2 := &profile.Posture{
		EntityID: "ent-1", Version: "4.19.0",
		Tier: profile.PostureUnexamined, Rationale: "new release, not reviewed",
		SetBy: "team:sarah", SetAt: now.Add(time.Hour),
	}
	require.NoError(t, s.SetPosture(ctx, v1))
	require.NoError(t, s.SetPosture(ctx, v2))

	gotV1, err := s.GetPosture(ctx, "ent-1", "4.18.2")
	require.NoError(t, err)
	assert.Equal(t, profile.PostureVettedFrozen, gotV1.Tier)

	gotV2, err := s.GetPosture(ctx, "ent-1", "4.19.0")
	require.NoError(t, err)
	assert.Equal(t, profile.PostureUnexamined, gotV2.Tier)
}

func TestGetPostures_AllVersions(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	entity := testEntity("ent-1", "pkg:npm/express", "express", now)
	require.NoError(t, s.PutEntity(ctx, entity))

	require.NoError(t, s.SetPosture(ctx, &profile.Posture{
		EntityID: "ent-1", Version: "4.18.0", Tier: profile.PostureVettedFrozen,
		Rationale: "old audit", SetBy: "team:sarah", SetAt: now.Add(-24 * time.Hour),
	}))
	require.NoError(t, s.SetPosture(ctx, &profile.Posture{
		EntityID: "ent-1", Version: "4.18.2", Tier: profile.PostureVettedFrozen,
		Rationale: "re-audit", SetBy: "team:sarah", SetAt: now,
	}))
	require.NoError(t, s.SetPosture(ctx, &profile.Posture{
		EntityID: "ent-1", Version: "4.19.0", Tier: profile.PostureTrustedForNow,
		Rationale: "minor release review", SetBy: "team:sarah", SetAt: now.Add(time.Hour),
	}))

	all, err := s.GetPostures(ctx, "ent-1")
	require.NoError(t, err)
	require.Len(t, all, 3)
	// Ordered newest-first by set_at.
	assert.Equal(t, "4.19.0", all[0].Version)
	assert.Equal(t, "4.18.2", all[1].Version)
	assert.Equal(t, "4.18.0", all[2].Version)
}

// TestPostureSameVersionOverwrite verifies that setting posture twice
// for the same (entity_id, version) pair replaces the earlier row.
// This is intentional — revising a rationale is a normal edit.
func TestPostureSameVersionOverwrite(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	entity := testEntity("ent-1", "pkg:npm/express", "express", now)
	require.NoError(t, s.PutEntity(ctx, entity))

	p1 := &profile.Posture{
		EntityID: "ent-1", Version: "4.18.2", Tier: profile.PostureUnexamined,
		Rationale: "haven't looked", SetBy: "team:sarah", SetAt: now,
	}
	require.NoError(t, s.SetPosture(ctx, p1))

	p2 := &profile.Posture{
		EntityID: "ent-1", Version: "4.18.2", Tier: profile.PostureVettedFrozen,
		Rationale: "reviewed, looks good", SetBy: "team:sarah", SetAt: now.Add(time.Hour),
	}
	require.NoError(t, s.SetPosture(ctx, p2))

	got, err := s.GetPosture(ctx, "ent-1", "4.18.2")
	require.NoError(t, err)
	assert.Equal(t, profile.PostureVettedFrozen, got.Tier)
	assert.Equal(t, "reviewed, looks good", got.Rationale)
}

func TestGetPosture_NotFound(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	_, err := s.GetPosture(ctx, "nonexistent", "")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestSetPosture_NilReturnsError(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	err := s.SetPosture(ctx, nil)
	assert.ErrorIs(t, err, ErrNilInput)
}

func TestBurnRoundTrip(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	entity := testEntity("ent-bad", "pkg:npm/bad-pkg", "bad-pkg", now)
	require.NoError(t, s.PutEntity(ctx, entity))

	burn := &profile.Burn{
		EntityID: "ent-bad",
		Reason:   "Maintainer account compromised",
		Source:   profile.BurnSourceLocal,
		BurnedAt: now,
		BurnedBy: "team:sarah+claude",
	}
	require.NoError(t, s.SetBurn(ctx, burn))

	got, err := s.GetBurn(ctx, "ent-bad")
	require.NoError(t, err)
	assert.Equal(t, "Maintainer account compromised", got.Reason)
	assert.Equal(t, profile.BurnSourceLocal, got.Source)
	assert.Equal(t, "team:sarah+claude", got.BurnedBy)
}

func TestBurnInheritedWithSourceOrg(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	entity := testEntity("ent-bad", "pkg:npm/bad-pkg", "bad-pkg", now)
	require.NoError(t, s.PutEntity(ctx, entity))

	burn := &profile.Burn{
		EntityID:  "ent-bad",
		Reason:    "Supply chain attack confirmed",
		Source:    profile.BurnSourceInherited,
		SourceOrg: "security-team",
		BurnedAt:  now,
		BurnedBy:  "auto-sync",
	}
	require.NoError(t, s.SetBurn(ctx, burn))

	got, err := s.GetBurn(ctx, "ent-bad")
	require.NoError(t, err)
	assert.Equal(t, profile.BurnSourceInherited, got.Source)
	assert.Equal(t, "security-team", got.SourceOrg)
}

func TestGetBurn_NotFound(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	_, err := s.GetBurn(ctx, "nonexistent")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestSetBurn_NilReturnsError(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	err := s.SetBurn(ctx, nil)
	assert.ErrorIs(t, err, ErrNilInput)
}

func TestListBurns(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	for _, name := range []string{"bad-1", "bad-2", "bad-3"} {
		id := "ent-" + name
		entity := testEntity(id, "pkg:npm/"+name, name, now)
		require.NoError(t, s.PutEntity(ctx, entity))
		burn := &profile.Burn{
			EntityID: id, Reason: "compromised",
			Source: profile.BurnSourceLocal, BurnedAt: now, BurnedBy: "team:sarah",
		}
		require.NoError(t, s.SetBurn(ctx, burn))
	}

	burns, err := s.ListBurns(ctx)
	require.NoError(t, err)
	assert.Len(t, burns, 3)
}

func TestListBurns_Empty(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	burns, err := s.ListBurns(ctx)
	require.NoError(t, err)
	assert.Empty(t, burns)
}

func TestSignalValuePreservesJSON(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	entity := testEntity("ent-1", "pkg:npm/express", "express", now)
	require.NoError(t, s.PutEntity(ctx, entity))

	complexValue := `{"nested":{"array":[1,2,3],"bool":true,"null":null},"string":"hello"}`
	signals := []profile.Signal{{
		ID: "sig-complex", EntityID: "ent-1", Type: "complex",
		Group: profile.SignalGroupVitality, Source: "test",
		ForgeryResistance: profile.ForgeryHigh,
		Value:             json.RawMessage(complexValue), CollectedAt: now, ExpiresAt: now.Add(time.Hour),
	}}
	require.NoError(t, s.AppendSignals(ctx, signals))

	got, err := s.GetSignals(ctx, "ent-1")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.JSONEq(t, complexValue, string(got[0].Value))
}

// --- New v2 method tests ---

func TestAppendDependencyObservations_RoundTrip(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	project := testEntity("proj-1", "repo:github/acme/myapp", "myapp", now)
	project.Type = profile.EntityProject
	dep1 := testEntity("dep-1", "pkg:npm/lodash", "lodash", now)
	dep2 := testEntity("dep-2", "pkg:npm/express", "express", now)
	require.NoError(t, s.PutEntity(ctx, project))
	require.NoError(t, s.PutEntity(ctx, dep1))
	require.NoError(t, s.PutEntity(ctx, dep2))

	obs := []profile.DependencyObservation{
		{ID: "obs-1", ProjectID: "proj-1", EntityID: "dep-1", Version: "4.17.21", Direct: true, ObservedAt: now, SurveyID: "survey-1"},
		{ID: "obs-2", ProjectID: "proj-1", EntityID: "dep-2", Version: "4.18.2", Direct: true, ObservedAt: now, SurveyID: "survey-1"},
	}
	require.NoError(t, s.AppendDependencyObservations(ctx, obs))

	latest, err := s.GetLatestDependencies(ctx, "proj-1")
	require.NoError(t, err)
	assert.Len(t, latest, 2)
}

// TestGetLatestDependencies_ReturnsMostRecentSurvey verifies that only
// observations from the latest survey are returned, not the full history.
func TestGetLatestDependencies_ReturnsMostRecentSurvey(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	project := testEntity("proj-1", "repo:github/acme/myapp", "myapp", now)
	project.Type = profile.EntityProject
	dep := testEntity("dep-1", "pkg:npm/lodash", "lodash", now)
	require.NoError(t, s.PutEntity(ctx, project))
	require.NoError(t, s.PutEntity(ctx, dep))

	require.NoError(t, s.AppendDependencyObservations(ctx, []profile.DependencyObservation{
		{ID: "obs-old", ProjectID: "proj-1", EntityID: "dep-1", Version: "4.17.20", Direct: true, ObservedAt: now.Add(-24 * time.Hour), SurveyID: "s1"},
	}))
	require.NoError(t, s.AppendDependencyObservations(ctx, []profile.DependencyObservation{
		{ID: "obs-new", ProjectID: "proj-1", EntityID: "dep-1", Version: "4.17.21", Direct: true, ObservedAt: now, SurveyID: "s2"},
	}))

	latest, err := s.GetLatestDependencies(ctx, "proj-1")
	require.NoError(t, err)
	require.Len(t, latest, 1, "only the most recent survey should be returned")
	assert.Equal(t, "s2", latest[0].SurveyID)
	assert.Equal(t, "4.17.21", latest[0].Version)
}

func TestGetLatestDependencies_NoObservations(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	obs, err := s.GetLatestDependencies(ctx, "never-surveyed")
	require.NoError(t, err)
	assert.Nil(t, obs)
}

func TestAppendAuditEntry_RoundTrip(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	entry := &profile.AuditEntry{
		ID:        "audit-1",
		Timestamp: now,
		Actor:     "team:sarah+claude-opus-4.6",
		Action:    "set_posture",
		EntityID:  "ent-1",
		Detail:    `{"version":"4.18.2","tier":"vetted-frozen"}`,
	}
	require.NoError(t, s.AppendAuditEntry(ctx, entry))

	// Raw verification via the underlying db.
	var got int
	require.NoError(t, s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM audit_log WHERE id = 'audit-1'`).Scan(&got))
	assert.Equal(t, 1, got)
}

func TestAppendAuditEntry_NilEntityID(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	entry := &profile.AuditEntry{
		ID: "audit-global", Timestamp: time.Now().UTC(),
		Actor: "system", Action: "startup", Detail: `{}`,
	}
	require.NoError(t, s.AppendAuditEntry(ctx, entry))

	var nullCheck int
	require.NoError(t, s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM audit_log WHERE id = 'audit-global' AND entity_id IS NULL`).Scan(&nullCheck))
	assert.Equal(t, 1, nullCheck, "empty EntityID should be stored as SQL NULL")
}

func TestAppendAuditEntry_Nil(t *testing.T) {
	s := newTestDB(t)
	assert.ErrorIs(t, s.AppendAuditEntry(context.Background(), nil), ErrNilInput)
}

func TestTeamIdentityRoundTrip(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	team := &profile.TeamIdentity{
		ID:        "team-1",
		Name:      "sarah+claude-opus-4.6",
		CreatedAt: now,
	}
	require.NoError(t, s.PutTeamIdentity(ctx, team))

	got, err := s.GetTeamIdentity(ctx, "team-1")
	require.NoError(t, err)
	assert.Equal(t, "sarah+claude-opus-4.6", got.Name)
	assert.Nil(t, got.HaltedAt)
	assert.Nil(t, got.RevokedAt)
}

func TestTeamIdentity_HaltAndRevoke(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	halted := now.Add(time.Hour)
	revoked := now.Add(2 * time.Hour)

	team := &profile.TeamIdentity{
		ID: "team-1", Name: "sarah+claude", CreatedAt: now,
		HaltedAt: &halted, RevokedAt: &revoked, RevokeReason: "credential leak",
	}
	require.NoError(t, s.PutTeamIdentity(ctx, team))

	got, err := s.GetTeamIdentity(ctx, "team-1")
	require.NoError(t, err)
	require.NotNil(t, got.HaltedAt)
	require.NotNil(t, got.RevokedAt)
	assert.Equal(t, halted.Unix(), got.HaltedAt.Unix())
	assert.Equal(t, revoked.Unix(), got.RevokedAt.Unix())
	assert.Equal(t, "credential leak", got.RevokeReason)
}

func TestTeamIdentity_NotFound(t *testing.T) {
	s := newTestDB(t)
	_, err := s.GetTeamIdentity(context.Background(), "nope")
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestAppendResolution_RoundTrip(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	entity := testEntity("ent-1", "pkg:npm/express", "express", now)
	require.NoError(t, s.PutEntity(ctx, entity))

	// Two signals must exist for the FK to be satisfied.
	require.NoError(t, s.AppendSignals(ctx, []profile.Signal{
		{ID: "kept", EntityID: "ent-1", Type: "stars", Group: profile.SignalGroupCriticality,
			Source: "github", ForgeryResistance: profile.ForgeryMediumDeclining,
			Value: json.RawMessage(`{}`), CollectedAt: now, ExpiresAt: now.Add(time.Hour)},
		{ID: "superseded", EntityID: "ent-1", Type: "stars", Group: profile.SignalGroupCriticality,
			Source: "github", ForgeryResistance: profile.ForgeryMediumDeclining,
			Value: json.RawMessage(`{}`), CollectedAt: now, ExpiresAt: now.Add(time.Hour)},
	}))

	r := &profile.SignalResolution{
		ID:                 "res-1",
		EntityID:           "ent-1",
		SignalType:         "stars",
		KeptSignalID:       "kept",
		SupersededSignalID: "superseded",
		Action:             "keep_previous",
		ResolvedBy:         "team:sarah",
		ResolvedAt:         now,
	}
	require.NoError(t, s.AppendResolution(ctx, r))

	var count int
	require.NoError(t, s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM signal_resolutions WHERE id = 'res-1'`).Scan(&count))
	assert.Equal(t, 1, count)
}

func TestConcurrentAccess(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	// WAL mode should allow concurrent reads and writes.
	done := make(chan error, 10)
	for i := 0; i < 10; i++ {
		go func(n int) {
			id := fmt.Sprintf("ent-%d", n)
			uri := fmt.Sprintf("pkg:npm/pkg-%d", n)
			entity := testEntity(id, uri, fmt.Sprintf("pkg-%d", n), now)
			if err := s.PutEntity(ctx, entity); err != nil {
				done <- err
				return
			}
			_, err := s.GetEntity(ctx, id)
			done <- err
		}(i)
	}

	for i := 0; i < 10; i++ {
		assert.NoError(t, <-done)
	}
}
