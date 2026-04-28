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

	// entityByURI / entityByVersionedBase let tests exercise the
	// LookupEntity walk (alternate-URI fallback, versioned-base scan)
	// without conflating "happy path" entity-driven tests with the
	// route-each-URI-separately behavior. When non-nil, these maps
	// override the entity / entityErr single-value fields.
	entityByURI          map[string]*profile.Entity
	entityByVersionedURI map[string]*profile.Entity
}

func (f *fakeStore) FindEntityByURI(_ context.Context, canonicalURI string) (*profile.Entity, error) {
	if f.entityByURI != nil {
		if e, ok := f.entityByURI[canonicalURI]; ok {
			return e, nil
		}
		return nil, store.ErrNotFound
	}
	if f.entityErr != nil {
		return nil, f.entityErr
	}
	return f.entity, nil
}

func (f *fakeStore) FindEntityByVersionedBaseURI(_ context.Context, baseURI string) (*profile.Entity, error) {
	if f.entityByVersionedURI != nil {
		if e, ok := f.entityByVersionedURI[baseURI]; ok {
			return e, nil
		}
	}
	return nil, store.ErrNotFound
}

// HasPostures returns true when the seeded posture slice is non-
// empty. Drives LookupEntity's weight-aware alternate walk; tests
// that don't care can leave postures nil and get the "thin entity"
// behavior naturally.
func (f *fakeStore) HasPostures(_ context.Context, _ string) (bool, error) {
	return len(f.postures) > 0, nil
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

	// entityByURI present and empty → every FindEntityByURI returns
	// ErrNotFound. This is more direct than entityErr=ErrNotFound now
	// that the assembler delegates to LookupEntity, which walks
	// alternates rather than honoring a single global error.
	f := &fakeStore{entityByURI: map[string]*profile.Entity{}}
	_, err := New(f).Assemble(context.Background(), "pkg:npm/ghost")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEntityNotFound)
	assert.Contains(t, err.Error(), "pkg:npm/ghost")
}

// TestAssemble_VersionedEntityFallback — caller asks about the
// unversioned form `repo:github/stretchr/testify`, but the only
// store row for testify is at `repo:github/stretchr/testify@v1.11.1`
// (the M1 violation surfaced by 2026-04-27 dogfood). The assembler's
// LookupEntity delegation must fall through to the versioned-base
// scan and surface the entity instead of NotFound.
//
// Revert proof: replace LookupEntity in Assemble with a direct
// FindEntityByURI; this test fails because the M1-violation entity
// is invisible to the unversioned base lookup.
func TestAssemble_VersionedEntityFallback(t *testing.T) {
	t.Parallel()

	versioned := &profile.Entity{
		ID:           "ent-tv",
		CanonicalURI: "repo:github/stretchr/testify@v1.11.1",
		Type:         profile.EntityProject,
		ShortName:    "testify",
	}
	f := &fakeStore{
		// Empty entityByURI ⇒ every exact-match misses.
		entityByURI: map[string]*profile.Entity{},
		// Versioned scan returns the testify@v1.11.1 entity for the
		// base URI lookup.
		entityByVersionedURI: map[string]*profile.Entity{
			"repo:github/stretchr/testify": versioned,
		},
		// Real store returns ErrNotFound when no burn exists; mirror
		// that here so the assembler's nil-burn branch fires correctly.
		burnErr: store.ErrNotFound,
	}

	s, err := New(f).Assemble(context.Background(), "repo:github/stretchr/testify")
	require.NoError(t, err, "M1-violation entity must surface via the versioned-base scan")
	assert.Equal(t, "repo:github/stretchr/testify@v1.11.1", s.CanonicalURI,
		"summary's CanonicalURI must reflect the entity actually found, not the input URI")
	assert.Equal(t, "testify", s.ShortName)
}

// TestAssemble_VanityResolution — caller asks about the import-path
// form `pkg:go/golang.org/x/mod`, but the store has it at
// `repo:github/golang/mod` (signal-collection pipelines vanity-
// resolve at ingest time). Cross-scheme alternate-walk must bridge.
func TestAssemble_VanityResolution(t *testing.T) {
	t.Parallel()

	resolved := &profile.Entity{
		ID:           "ent-mod",
		CanonicalURI: "repo:github/golang/mod",
		Type:         profile.EntityProject,
		ShortName:    "mod",
	}
	f := &fakeStore{
		entityByURI: map[string]*profile.Entity{
			"repo:github/golang/mod": resolved,
		},
		burnErr: store.ErrNotFound,
	}

	s, err := New(f).Assemble(context.Background(), "pkg:go/golang.org/x/mod")
	require.NoError(t, err, "vanity-resolved github form must be reachable from import-path lookup")
	assert.Equal(t, "repo:github/golang/mod", s.CanonicalURI)
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
