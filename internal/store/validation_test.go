package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
)

var fixedTime = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// --- Issue #39: Empty string validation ---

func TestValidation_PutEntity_EmptyID(t *testing.T) {
	s := newTestDB(t)
	entity := &profile.Entity{ID: "", Type: profile.EntityPackage, Name: "test"}
	err := s.PutEntity(context.Background(), entity)
	assert.Error(t, err, "should reject empty ID")
}

func TestValidation_PutEntity_EmptyName(t *testing.T) {
	s := newTestDB(t)
	entity := &profile.Entity{ID: "test-id", Type: profile.EntityPackage, Name: ""}
	err := s.PutEntity(context.Background(), entity)
	assert.Error(t, err, "should reject empty name")
}

func TestValidation_PutEntity_EmptyType(t *testing.T) {
	s := newTestDB(t)
	entity := &profile.Entity{ID: "test-id", Type: "", Name: "test"}
	err := s.PutEntity(context.Background(), entity)
	assert.Error(t, err, "should reject empty type")
}

func TestValidation_SetPosture_EmptyEntityID(t *testing.T) {
	s := newTestDB(t)
	posture := &profile.Posture{EntityID: "", Tier: profile.PostureTrustedForNow, Rationale: "test"}
	err := s.SetPosture(context.Background(), posture)
	assert.Error(t, err, "should reject empty entity ID")
}

func TestValidation_SetPosture_EmptyTier(t *testing.T) {
	s := newTestDB(t)
	posture := &profile.Posture{EntityID: "test", Tier: "", Rationale: "test"}
	err := s.SetPosture(context.Background(), posture)
	assert.Error(t, err, "should reject empty tier")
}

func TestValidation_SetBurn_EmptyEntityID(t *testing.T) {
	s := newTestDB(t)
	burn := &profile.Burn{EntityID: "", Reason: "test"}
	err := s.SetBurn(context.Background(), burn)
	assert.Error(t, err, "should reject empty entity ID")
}

func TestValidation_SetBurn_EmptyReason(t *testing.T) {
	s := newTestDB(t)
	burn := &profile.Burn{EntityID: "test", Reason: ""}
	err := s.SetBurn(context.Background(), burn)
	assert.Error(t, err, "should reject empty reason")
}

// --- Issue #45: FK violation context ---

func TestValidation_PutSignals_ForeignKeyViolation(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	signals := []profile.Signal{{
		ID: "sig-orphan", EntityID: "nonexistent-entity", Type: "test",
		Group: profile.SignalGroupVitality, Source: "test",
		ForgeryResistance: profile.ForgeryHigh,
		Value:             []byte(`{}`),
		CollectedAt:       fixedTime,
		ExpiresAt:         fixedTime.Add(time.Hour),
	}}

	err := s.PutSignals(ctx, signals)
	assert.Error(t, err, "should reject signal referencing nonexistent entity")
}

// --- Issue #34: Database file permissions ---

func TestSecurity_DatabaseFilePermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	s, err := OpenSQLite(path)
	require.NoError(t, err)
	defer s.Close()

	info, err := os.Stat(path)
	require.NoError(t, err)

	perm := info.Mode().Perm()
	assert.Equal(t, os.FileMode(0600), perm,
		"database file should be 0600 (owner read/write only), got %o", perm)
}
