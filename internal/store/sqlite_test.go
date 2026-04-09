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
		ID:        "pkg:npm:express",
		Type:      profile.EntityPackage,
		Name:      "express",
		Ecosystem: "npm",
		URL:       "https://github.com/expressjs/express",
		CreatedAt: now,
		UpdatedAt: now,
	}

	require.NoError(t, s.PutEntity(ctx, entity))

	got, err := s.GetEntity(ctx, "pkg:npm:express")
	require.NoError(t, err)
	assert.Equal(t, entity.ID, got.ID)
	assert.Equal(t, entity.Type, got.Type)
	assert.Equal(t, entity.Name, got.Name)
	assert.Equal(t, entity.Ecosystem, got.Ecosystem)
	assert.Equal(t, entity.URL, got.URL)
	assert.Equal(t, entity.CreatedAt.Unix(), got.CreatedAt.Unix())
}

func TestEntityUpdate(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	entity := &profile.Entity{
		ID:        "pkg:npm:express",
		Type:      profile.EntityPackage,
		Name:      "express",
		CreatedAt: now,
		UpdatedAt: now,
	}
	require.NoError(t, s.PutEntity(ctx, entity))

	entity.URL = "https://github.com/expressjs/express"
	entity.UpdatedAt = now.Add(time.Hour)
	require.NoError(t, s.PutEntity(ctx, entity))

	got, err := s.GetEntity(ctx, "pkg:npm:express")
	require.NoError(t, err)
	assert.Equal(t, "https://github.com/expressjs/express", got.URL)
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

func TestFindEntity(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	entity := &profile.Entity{
		ID:        "pkg:npm:express",
		Type:      profile.EntityPackage,
		Name:      "express",
		Ecosystem: "npm",
		CreatedAt: now,
		UpdatedAt: now,
	}
	require.NoError(t, s.PutEntity(ctx, entity))

	got, err := s.FindEntity(ctx, "express", profile.EntityPackage)
	require.NoError(t, err)
	assert.Equal(t, "pkg:npm:express", got.ID)
}

func TestFindEntity_NotFound(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	_, err := s.FindEntity(ctx, "express", profile.EntityPackage)
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestFindEntity_WrongType(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	entity := &profile.Entity{
		ID:        "pkg:npm:express",
		Type:      profile.EntityPackage,
		Name:      "express",
		CreatedAt: now,
		UpdatedAt: now,
	}
	require.NoError(t, s.PutEntity(ctx, entity))

	_, err := s.FindEntity(ctx, "express", profile.EntityIdentity)
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestSignalRoundTrip(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	entity := &profile.Entity{
		ID: "pkg:npm:express", Type: profile.EntityPackage,
		Name: "express", CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, s.PutEntity(ctx, entity))

	signals := []profile.Signal{
		{
			ID:                "sig-1",
			EntityID:          "pkg:npm:express",
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
			EntityID:          "pkg:npm:express",
			Type:              "stars",
			Group:             profile.SignalGroupCriticality,
			Source:            "github",
			ForgeryResistance: profile.ForgeryMediumDeclining,
			Value:             json.RawMessage(`{"count":65000}`),
			CollectedAt:       now,
			ExpiresAt:         now.Add(24 * time.Hour),
		},
	}

	require.NoError(t, s.PutSignals(ctx, signals))

	got, err := s.GetSignals(ctx, "pkg:npm:express")
	require.NoError(t, err)
	assert.Len(t, got, 2)
}

func TestGetSignalsByGroup(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	entity := &profile.Entity{
		ID: "pkg:npm:express", Type: profile.EntityPackage,
		Name: "express", CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, s.PutEntity(ctx, entity))

	signals := []profile.Signal{
		{ID: "sig-1", EntityID: "pkg:npm:express", Type: "last_commit",
			Group: profile.SignalGroupVitality, Source: "github",
			ForgeryResistance: profile.ForgeryHigh,
			Value:             json.RawMessage(`{}`), CollectedAt: now, ExpiresAt: now.Add(time.Hour)},
		{ID: "sig-2", EntityID: "pkg:npm:express", Type: "stars",
			Group: profile.SignalGroupCriticality, Source: "github",
			ForgeryResistance: profile.ForgeryMediumDeclining,
			Value:             json.RawMessage(`{}`), CollectedAt: now, ExpiresAt: now.Add(time.Hour)},
	}
	require.NoError(t, s.PutSignals(ctx, signals))

	got, err := s.GetSignalsByGroup(ctx, "pkg:npm:express", profile.SignalGroupVitality)
	require.NoError(t, err)
	assert.Len(t, got, 1)
	assert.Equal(t, "last_commit", got[0].Type)
}

func TestSignalUpdate(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	entity := &profile.Entity{
		ID: "pkg:npm:express", Type: profile.EntityPackage,
		Name: "express", CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, s.PutEntity(ctx, entity))

	sig := []profile.Signal{{
		ID: "sig-1", EntityID: "pkg:npm:express", Type: "stars",
		Group: profile.SignalGroupCriticality, Source: "github",
		ForgeryResistance: profile.ForgeryMediumDeclining,
		Value:             json.RawMessage(`{"count":1000}`), CollectedAt: now, ExpiresAt: now.Add(time.Hour),
	}}
	require.NoError(t, s.PutSignals(ctx, sig))

	sig[0].Value = json.RawMessage(`{"count":2000}`)
	sig[0].CollectedAt = now.Add(time.Hour)
	require.NoError(t, s.PutSignals(ctx, sig))

	got, err := s.GetSignals(ctx, "pkg:npm:express")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.JSONEq(t, `{"count":2000}`, string(got[0].Value))
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

	entity := &profile.Entity{
		ID: "pkg:npm:express", Type: profile.EntityPackage,
		Name: "express", CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, s.PutEntity(ctx, entity))

	posture := &profile.Posture{
		EntityID:  "pkg:npm:express",
		Tier:      profile.PostureTrustedForNow,
		Version:   "4.18.2",
		Rationale: "Strong vitality, no anomalies",
		SetBy:     "sarah",
		SetAt:     now,
	}
	require.NoError(t, s.SetPosture(ctx, posture))

	got, err := s.GetPosture(ctx, "pkg:npm:express")
	require.NoError(t, err)
	assert.Equal(t, profile.PostureTrustedForNow, got.Tier)
	assert.Equal(t, "4.18.2", got.Version)
	assert.Equal(t, "Strong vitality, no anomalies", got.Rationale)
	assert.Equal(t, "sarah", got.SetBy)
}

func TestPostureUpdate(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	entity := &profile.Entity{
		ID: "pkg:npm:express", Type: profile.EntityPackage,
		Name: "express", CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, s.PutEntity(ctx, entity))

	p1 := &profile.Posture{
		EntityID: "pkg:npm:express", Tier: profile.PostureUnexamined,
		Rationale: "Haven't looked yet", SetBy: "sarah", SetAt: now,
	}
	require.NoError(t, s.SetPosture(ctx, p1))

	p2 := &profile.Posture{
		EntityID: "pkg:npm:express", Tier: profile.PostureTrustedForNow,
		Version: "4.18.2", Rationale: "Reviewed, looks good", SetBy: "sarah",
		SetAt: now.Add(time.Hour),
	}
	require.NoError(t, s.SetPosture(ctx, p2))

	got, err := s.GetPosture(ctx, "pkg:npm:express")
	require.NoError(t, err)
	assert.Equal(t, profile.PostureTrustedForNow, got.Tier)
	assert.Equal(t, "Reviewed, looks good", got.Rationale)
}

func TestGetPosture_NotFound(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	_, err := s.GetPosture(ctx, "nonexistent")
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

	entity := &profile.Entity{
		ID: "pkg:npm:bad-pkg", Type: profile.EntityPackage,
		Name: "bad-pkg", CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, s.PutEntity(ctx, entity))

	burn := &profile.Burn{
		EntityID: "pkg:npm:bad-pkg",
		Reason:   "Maintainer account compromised",
		Source:   profile.BurnSourceLocal,
		BurnedAt: now,
		BurnedBy: "sarah",
	}
	require.NoError(t, s.SetBurn(ctx, burn))

	got, err := s.GetBurn(ctx, "pkg:npm:bad-pkg")
	require.NoError(t, err)
	assert.Equal(t, "Maintainer account compromised", got.Reason)
	assert.Equal(t, profile.BurnSourceLocal, got.Source)
	assert.Equal(t, "sarah", got.BurnedBy)
}

func TestBurnInheritedWithSourceOrg(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	entity := &profile.Entity{
		ID: "pkg:npm:bad-pkg", Type: profile.EntityPackage,
		Name: "bad-pkg", CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, s.PutEntity(ctx, entity))

	burn := &profile.Burn{
		EntityID:  "pkg:npm:bad-pkg",
		Reason:    "Supply chain attack confirmed",
		Source:    profile.BurnSourceInherited,
		SourceOrg: "security-team",
		BurnedAt:  now,
		BurnedBy:  "auto-sync",
	}
	require.NoError(t, s.SetBurn(ctx, burn))

	got, err := s.GetBurn(ctx, "pkg:npm:bad-pkg")
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
		entity := &profile.Entity{
			ID: "pkg:npm:" + name, Type: profile.EntityPackage,
			Name: name, CreatedAt: now, UpdatedAt: now,
		}
		require.NoError(t, s.PutEntity(ctx, entity))
		burn := &profile.Burn{
			EntityID: "pkg:npm:" + name, Reason: "compromised",
			Source: profile.BurnSourceLocal, BurnedAt: now, BurnedBy: "sarah",
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

	entity := &profile.Entity{
		ID: "pkg:npm:express", Type: profile.EntityPackage,
		Name: "express", CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, s.PutEntity(ctx, entity))

	complexValue := `{"nested":{"array":[1,2,3],"bool":true,"null":null},"string":"hello"}`
	signals := []profile.Signal{{
		ID: "sig-complex", EntityID: "pkg:npm:express", Type: "complex",
		Group: profile.SignalGroupVitality, Source: "test",
		ForgeryResistance: profile.ForgeryHigh,
		Value:             json.RawMessage(complexValue), CollectedAt: now, ExpiresAt: now.Add(time.Hour),
	}}
	require.NoError(t, s.PutSignals(ctx, signals))

	got, err := s.GetSignals(ctx, "pkg:npm:express")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.JSONEq(t, complexValue, string(got[0].Value))
}

func TestConcurrentAccess(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	// WAL mode should allow concurrent reads and writes.
	done := make(chan error, 10)
	for i := 0; i < 10; i++ {
		go func(n int) {
			id := fmt.Sprintf("pkg:npm:pkg-%d", n)
			entity := &profile.Entity{
				ID: id, Type: profile.EntityPackage,
				Name: fmt.Sprintf("pkg-%d", n), CreatedAt: now, UpdatedAt: now,
			}
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
