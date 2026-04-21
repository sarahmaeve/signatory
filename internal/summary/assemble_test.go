package summary

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/exchange"
	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/store"
)

// fakeStore is an in-memory implementation of AssemblerStore for
// hermetic testing. Each method's behavior is driven by its own
// struct field, so tests can exercise individual paths without
// touching SQLite.
type fakeStore struct {
	entity       *profile.Entity
	entityErr    error
	postures     []profile.Posture
	posturesErr  error
	burn         *profile.Burn
	burnErr      error
	outputs      []store.AnalystOutputSummary
	outputsErr   error
	severityByID map[string]SeverityCounts
	relatedURIs  []string
	relatedErr   error
}

func (f *fakeStore) FindEntityByURI(_ context.Context, _ string) (*profile.Entity, error) {
	if f.entityErr != nil {
		return nil, f.entityErr
	}
	return f.entity, nil
}
func (f *fakeStore) GetPostures(_ context.Context, _ string) ([]profile.Posture, error) {
	return f.postures, f.posturesErr
}
func (f *fakeStore) GetBurn(_ context.Context, _ string) (*profile.Burn, error) {
	if f.burnErr != nil {
		return nil, f.burnErr
	}
	return f.burn, nil
}
func (f *fakeStore) ListAnalystOutputs(_ context.Context, _ store.AnalystOutputFilter) ([]store.AnalystOutputSummary, error) {
	return f.outputs, f.outputsErr
}
func (f *fakeStore) SeverityCounts(_ context.Context, outputID string) (SeverityCounts, error) {
	return f.severityByID[outputID], nil
}
func (f *fakeStore) ListRelatedURIs(_ context.Context, _ string) ([]string, error) {
	return f.relatedURIs, f.relatedErr
}

// TestAssemble_FullyPopulated covers the happy path: entity exists,
// has a posture, has an active burn, has analyses across two
// analysts, and has related URIs. Every field on Summary should
// show up.
func TestAssemble_FullyPopulated(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 4, 21, 12, 0, 0, 0, time.UTC)
	f := &fakeStore{
		entity: &profile.Entity{
			ID:           "ent-1",
			CanonicalURI: "pkg:npm/express@4.18.2",
			Type:         profile.EntityPackage,
			ShortName:    "express",
			URL:          "",
		},
		postures: []profile.Posture{
			{EntityID: "ent-1", Tier: profile.PostureTier("vetted-frozen"), Version: "4.18.2", Rationale: "audited", SetBy: "team:sarah", SetAt: now},
		},
		burn: &profile.Burn{
			EntityID: "ent-1", Reason: "supply-chain compromise", BurnedBy: "team:sarah", BurnedAt: now,
		},
		outputs: []store.AnalystOutputSummary{
			{
				OutputID: "out-sec", AnalystID: "external-sec-v1",
				Model: "claude-opus-4", Round: 1,
				IngestedAt:       now.Format(time.RFC3339),
				TargetCommit:     "abc123",
				CollectedFromURI: "repo:github/expressjs/express",
				ConclusionsCount: 5, PositiveAbsenceCount: 2, ObservationCount: 1, PatternCount: 0,
			},
			{
				OutputID: "out-prov", AnalystID: "signatory-provenance",
				Round:            1,
				IngestedAt:       now.Format(time.RFC3339),
				ConclusionsCount: 8, PositiveAbsenceCount: 3, ObservationCount: 2, PatternCount: 1,
			},
		},
		severityByID: map[string]SeverityCounts{
			"out-sec":  {exchange.SeverityHigh: 1, exchange.SeverityMedium: 2, exchange.SeverityPositive: 2},
			"out-prov": {exchange.SeverityMedium: 3, exchange.SeverityLow: 2, exchange.SeverityPositive: 3},
		},
		relatedURIs: []string{"repo:github/expressjs/express"},
	}
	s, err := New(f).Assemble(context.Background(), "pkg:npm/express@4.18.2")
	require.NoError(t, err)
	require.NotNil(t, s)

	assert.Equal(t, "pkg:npm/express@4.18.2", s.CanonicalURI)
	assert.Equal(t, "express", s.ShortName)
	assert.Equal(t, "package", s.EntityType)
	require.NotNil(t, s.Posture)
	assert.Equal(t, "vetted-frozen", s.Posture.Tier)
	assert.Equal(t, "4.18.2", s.Posture.Version)
	require.NotNil(t, s.Burn)
	assert.Equal(t, "supply-chain compromise", s.Burn.Reason)
	require.Len(t, s.Analyses, 2)
	assert.Equal(t, "external-sec-v1", s.Analyses[0].AnalystID)
	assert.Equal(t, "repo:github/expressjs/express", s.Analyses[0].CollectedFromURI)
	assert.Equal(t, 1, s.Analyses[0].SeverityCounts[exchange.SeverityHigh])
	assert.Equal(t, []string{"repo:github/expressjs/express"}, s.RelatedURIs)
}

// TestAssemble_NotFound maps store.ErrNotFound to
// ErrEntityNotFound with the target URI in the error message.
func TestAssemble_NotFound(t *testing.T) {
	t.Parallel()

	f := &fakeStore{entityErr: store.ErrNotFound}
	_, err := New(f).Assemble(context.Background(), "pkg:npm/ghost")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEntityNotFound)
	assert.Contains(t, err.Error(), "pkg:npm/ghost")
}

// TestAssemble_NoPostureNoBurn covers the common "entity exists
// but is unexamined" case: no posture, no burn, nil analyses.
// Summary is populated but has all three optional fields empty.
func TestAssemble_NoPostureNoBurn(t *testing.T) {
	t.Parallel()

	f := &fakeStore{
		entity: &profile.Entity{
			ID:           "ent-empty",
			CanonicalURI: "pkg:npm/unexamined",
			Type:         profile.EntityPackage,
			ShortName:    "unexamined",
		},
		burnErr: store.ErrNotFound, // "no active burn" is normal
	}
	s, err := New(f).Assemble(context.Background(), "pkg:npm/unexamined")
	require.NoError(t, err)
	assert.Nil(t, s.Posture)
	assert.Nil(t, s.Burn)
	assert.Empty(t, s.Analyses)
	assert.Empty(t, s.RelatedURIs)
}

// TestAssemble_StripsSelfFromRelatedURIs verifies that the entity's
// own URI is dropped from RelatedURIs — related means "other
// identities linked to this one," not "this one itself."
func TestAssemble_StripsSelfFromRelatedURIs(t *testing.T) {
	t.Parallel()

	f := &fakeStore{
		entity: &profile.Entity{
			ID:           "ent-x",
			CanonicalURI: "pkg:npm/x",
			Type:         profile.EntityPackage,
			ShortName:    "x",
		},
		burnErr:     store.ErrNotFound,
		relatedURIs: []string{"repo:github/foo/x", "pkg:npm/x", "pkg:npm/x", "repo:github/foo/x"},
	}
	s, err := New(f).Assemble(context.Background(), "pkg:npm/x")
	require.NoError(t, err)
	assert.Equal(t, []string{"repo:github/foo/x"}, s.RelatedURIs,
		"self-URI and duplicates must be stripped")
}

// TestAssemble_PostureErrorPropagates ensures we don't silently drop
// a posture-lookup failure — the summary is supposed to be
// trustworthy, so a DB hiccup on one of its pieces must surface.
func TestAssemble_PostureErrorPropagates(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("posture-lookup failed")
	f := &fakeStore{
		entity: &profile.Entity{
			ID:           "ent",
			CanonicalURI: "pkg:npm/y",
			Type:         profile.EntityPackage,
			ShortName:    "y",
		},
		posturesErr: sentinel,
	}
	_, err := New(f).Assemble(context.Background(), "pkg:npm/y")
	require.Error(t, err)
	assert.ErrorIs(t, err, sentinel)
}
