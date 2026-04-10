package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
)

// mockStore is an in-memory contract test for the Store interface. It
// doesn't have to be production-quality — it just has to exercise the
// interface semantics so we can verify that SQLite and any future
// implementations behave consistently.
//
// Append-only semantics are enforced where the real store enforces them:
// signals, dependency observations, audit entries, and resolutions all
// accumulate; no method replaces or removes existing records.
type mockStore struct {
	entities     map[string]*profile.Entity                 // keyed by ID
	signals      map[string][]profile.Signal                // keyed by entity ID
	postures     map[postureKey]*profile.Posture            // keyed by (entity_id, version)
	burns        map[string]*profile.Burn                   // keyed by entity ID
	observations map[string][]profile.DependencyObservation // keyed by project ID
	auditEntries []profile.AuditEntry
	resolutions  []profile.SignalResolution
	teams        map[string]*profile.TeamIdentity

	// Call counters (handy for asserting wiring in higher-level tests).
	getCalled        int
	putCalled        int
	findByURICalled  int
	getSignalsCalled int
	getLatestCalled  int
	appendSigCalled  int
	getByGroupCalled int
	getPostureCalled int
	getPosturesCount int
	setPostureCalled int
	getBurnCalled    int
	setBurnCalled    int
	listBurnsCalled  int
	closeCalled      int
}

type postureKey struct {
	entityID string
	version  string
}

func newMockStore() *mockStore {
	return &mockStore{
		entities:     make(map[string]*profile.Entity),
		signals:      make(map[string][]profile.Signal),
		postures:     make(map[postureKey]*profile.Posture),
		burns:        make(map[string]*profile.Burn),
		observations: make(map[string][]profile.DependencyObservation),
		teams:        make(map[string]*profile.TeamIdentity),
	}
}

// --- Entity operations ---

func (m *mockStore) GetEntity(ctx context.Context, id string) (*profile.Entity, error) {
	m.getCalled++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	e, ok := m.entities[id]
	if !ok {
		return nil, errors.New("entity not found")
	}
	return e, nil
}

func (m *mockStore) FindEntityByURI(ctx context.Context, canonicalURI string) (*profile.Entity, error) {
	m.findByURICalled++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, e := range m.entities {
		if e.CanonicalURI == canonicalURI {
			return e, nil
		}
	}
	return nil, errors.New("entity not found")
}

func (m *mockStore) PutEntity(ctx context.Context, entity *profile.Entity) error {
	m.putCalled++
	if err := ctx.Err(); err != nil {
		return err
	}
	if entity == nil {
		return errors.New("entity must not be nil")
	}
	m.entities[entity.ID] = entity
	return nil
}

// --- Signal operations ---

func (m *mockStore) GetSignals(ctx context.Context, entityID string) ([]profile.Signal, error) {
	m.getSignalsCalled++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return m.signals[entityID], nil
}

func (m *mockStore) GetLatestSignals(ctx context.Context, entityID string) ([]profile.Signal, error) {
	m.getLatestCalled++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Build the set of superseded signal IDs.
	superseded := make(map[string]bool)
	for _, r := range m.resolutions {
		superseded[r.SupersededSignalID] = true
	}
	// Pick latest per (type, source) ignoring superseded.
	type key struct{ t, src string }
	latest := make(map[key]profile.Signal)
	for _, sig := range m.signals[entityID] {
		if superseded[sig.ID] {
			continue
		}
		k := key{sig.Type, sig.Source}
		if existing, ok := latest[k]; !ok || sig.CollectedAt.After(existing.CollectedAt) {
			latest[k] = sig
		}
	}
	out := make([]profile.Signal, 0, len(latest))
	for _, sig := range latest {
		out = append(out, sig)
	}
	return out, nil
}

func (m *mockStore) AppendSignals(ctx context.Context, signals []profile.Signal) error {
	m.appendSigCalled++
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, s := range signals {
		m.signals[s.EntityID] = append(m.signals[s.EntityID], s)
	}
	return nil
}

func (m *mockStore) GetSignalsByGroup(ctx context.Context, entityID string, group profile.SignalGroup) ([]profile.Signal, error) {
	m.getByGroupCalled++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var result []profile.Signal
	for _, s := range m.signals[entityID] {
		if s.Group == group {
			result = append(result, s)
		}
	}
	return result, nil
}

// --- Posture operations ---

func (m *mockStore) GetPosture(ctx context.Context, entityID string, version string) (*profile.Posture, error) {
	m.getPostureCalled++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p, ok := m.postures[postureKey{entityID, version}]
	if !ok {
		return nil, errors.New("posture not found")
	}
	return p, nil
}

func (m *mockStore) GetPostures(ctx context.Context, entityID string) ([]profile.Posture, error) {
	m.getPosturesCount++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var out []profile.Posture
	for k, p := range m.postures {
		if k.entityID == entityID {
			out = append(out, *p)
		}
	}
	return out, nil
}

func (m *mockStore) SetPosture(ctx context.Context, posture *profile.Posture) error {
	m.setPostureCalled++
	if err := ctx.Err(); err != nil {
		return err
	}
	if posture == nil {
		return errors.New("posture must not be nil")
	}
	m.postures[postureKey{posture.EntityID, posture.Version}] = posture
	return nil
}

// --- Burn operations ---

func (m *mockStore) GetBurn(ctx context.Context, entityID string) (*profile.Burn, error) {
	m.getBurnCalled++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b, ok := m.burns[entityID]
	if !ok {
		return nil, errors.New("burn not found")
	}
	return b, nil
}

func (m *mockStore) SetBurn(ctx context.Context, burn *profile.Burn) error {
	m.setBurnCalled++
	if err := ctx.Err(); err != nil {
		return err
	}
	if burn == nil {
		return errors.New("burn must not be nil")
	}
	m.burns[burn.EntityID] = burn
	return nil
}

func (m *mockStore) ListBurns(ctx context.Context) ([]profile.Burn, error) {
	m.listBurnsCalled++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var result []profile.Burn
	for _, b := range m.burns {
		result = append(result, *b)
	}
	return result, nil
}

// --- Dependency observations ---

func (m *mockStore) AppendDependencyObservations(ctx context.Context, obs []profile.DependencyObservation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, o := range obs {
		m.observations[o.ProjectID] = append(m.observations[o.ProjectID], o)
	}
	return nil
}

func (m *mockStore) GetLatestDependencies(ctx context.Context, projectID string) ([]profile.DependencyObservation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	all := m.observations[projectID]
	if len(all) == 0 {
		return nil, nil
	}
	// Find the most recent survey_id.
	latestSurvey := all[0].SurveyID
	latestTime := all[0].ObservedAt
	for _, o := range all {
		if o.ObservedAt.After(latestTime) {
			latestSurvey = o.SurveyID
			latestTime = o.ObservedAt
		}
	}
	var out []profile.DependencyObservation
	for _, o := range all {
		if o.SurveyID == latestSurvey {
			out = append(out, o)
		}
	}
	return out, nil
}

// --- Audit log ---

func (m *mockStore) AppendAuditEntry(ctx context.Context, entry *profile.AuditEntry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if entry == nil {
		return errors.New("audit entry must not be nil")
	}
	m.auditEntries = append(m.auditEntries, *entry)
	return nil
}

// --- Signal resolutions ---

func (m *mockStore) AppendResolution(ctx context.Context, r *profile.SignalResolution) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if r == nil {
		return errors.New("resolution must not be nil")
	}
	m.resolutions = append(m.resolutions, *r)
	return nil
}

// --- Team identities ---

func (m *mockStore) GetTeamIdentity(ctx context.Context, id string) (*profile.TeamIdentity, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	t, ok := m.teams[id]
	if !ok {
		return nil, errors.New("team identity not found")
	}
	return t, nil
}

func (m *mockStore) PutTeamIdentity(ctx context.Context, t *profile.TeamIdentity) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if t == nil {
		return errors.New("team identity must not be nil")
	}
	m.teams[t.ID] = t
	return nil
}

// --- Close ---

func (m *mockStore) Close() error {
	m.closeCalled++
	return nil
}

// Compile-time interface check.
var _ Store = (*mockStore)(nil)

// --- Contract Tests ---
//
// These tests target the interface contract, not any specific
// implementation. They use the in-memory mockStore above; corresponding
// tests against the SQLite implementation live in sqlite_test.go.

func TestStore_EntityRoundTrip(t *testing.T) {
	t.Parallel()

	s := newMockStore()
	ctx := context.Background()

	entity := &profile.Entity{
		ID:           "ent-1",
		CanonicalURI: "pkg:npm/lodash",
		Type:         profile.EntityPackage,
		ShortName:    "lodash",
		Description:  "Lodash utility library",
		Ecosystem:    "npm",
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}

	err := s.PutEntity(ctx, entity)
	require.NoError(t, err)
	assert.Equal(t, 1, s.putCalled)

	got, err := s.GetEntity(ctx, "ent-1")
	require.NoError(t, err)
	assert.Equal(t, entity, got)
	assert.Equal(t, 1, s.getCalled)
}

func TestStore_GetEntity_NotFound(t *testing.T) {
	t.Parallel()

	s := newMockStore()
	_, err := s.GetEntity(context.Background(), "nonexistent")
	assert.Error(t, err)
}

func TestStore_FindEntityByURI(t *testing.T) {
	t.Parallel()

	s := newMockStore()
	ctx := context.Background()

	entity := &profile.Entity{
		ID:           "ent-1",
		CanonicalURI: "pkg:npm/express",
		Type:         profile.EntityPackage,
		ShortName:    "express",
	}
	require.NoError(t, s.PutEntity(ctx, entity))

	found, err := s.FindEntityByURI(ctx, "pkg:npm/express")
	require.NoError(t, err)
	assert.Equal(t, entity, found)
	assert.Equal(t, 1, s.findByURICalled)
}

func TestStore_FindEntityByURI_NotFound(t *testing.T) {
	t.Parallel()

	s := newMockStore()
	ctx := context.Background()

	entity := &profile.Entity{
		ID:           "ent-1",
		CanonicalURI: "pkg:npm/express",
		Type:         profile.EntityPackage,
		ShortName:    "express",
	}
	require.NoError(t, s.PutEntity(ctx, entity))

	_, err := s.FindEntityByURI(ctx, "pkg:npm/nonexistent")
	assert.Error(t, err)
}

func TestStore_PutEntity_NilEntity(t *testing.T) {
	t.Parallel()

	s := newMockStore()
	err := s.PutEntity(context.Background(), nil)
	assert.Error(t, err)
}

func TestStore_AppendSignals_RoundTrip(t *testing.T) {
	t.Parallel()

	s := newMockStore()
	ctx := context.Background()

	signals := []profile.Signal{
		{ID: "sig-1", EntityID: "ent-1", Group: profile.SignalGroupVitality, Type: "commits"},
		{ID: "sig-2", EntityID: "ent-1", Group: profile.SignalGroupHygiene, Type: "has-tests"},
	}

	err := s.AppendSignals(ctx, signals)
	require.NoError(t, err)

	got, err := s.GetSignals(ctx, "ent-1")
	require.NoError(t, err)
	assert.Len(t, got, 2)
}

// TestStore_AppendSignals_IsAppendOnly verifies that a second call does
// not replace earlier signals — it adds to them. This is the core
// append-only semantic from the v2 design.
func TestStore_AppendSignals_IsAppendOnly(t *testing.T) {
	t.Parallel()

	s := newMockStore()
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Second)

	require.NoError(t, s.AppendSignals(ctx, []profile.Signal{
		{ID: "sig-1", EntityID: "ent-1", Type: "stars", Source: "github", CollectedAt: now},
	}))
	require.NoError(t, s.AppendSignals(ctx, []profile.Signal{
		{ID: "sig-2", EntityID: "ent-1", Type: "stars", Source: "github", CollectedAt: now.Add(time.Hour)},
	}))

	all, err := s.GetSignals(ctx, "ent-1")
	require.NoError(t, err)
	assert.Len(t, all, 2, "both signals should be preserved — append-only")
}

func TestStore_GetLatestSignals_FiltersSuperseded(t *testing.T) {
	t.Parallel()

	s := newMockStore()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	require.NoError(t, s.AppendSignals(ctx, []profile.Signal{
		{ID: "old", EntityID: "ent-1", Type: "stars", Source: "github", CollectedAt: now.Add(-time.Hour)},
		{ID: "new", EntityID: "ent-1", Type: "stars", Source: "github", CollectedAt: now},
	}))

	// Supersede "new" with a resolution — "old" should be the latest.
	require.NoError(t, s.AppendResolution(ctx, &profile.SignalResolution{
		ID:                 "res-1",
		EntityID:           "ent-1",
		SignalType:         "stars",
		KeptSignalID:       "old",
		SupersededSignalID: "new",
		Action:             "keep_previous",
	}))

	latest, err := s.GetLatestSignals(ctx, "ent-1")
	require.NoError(t, err)
	require.Len(t, latest, 1)
	assert.Equal(t, "old", latest[0].ID)
}

func TestStore_GetSignalsByGroup(t *testing.T) {
	t.Parallel()

	s := newMockStore()
	ctx := context.Background()

	signals := []profile.Signal{
		{ID: "sig-1", EntityID: "ent-1", Group: profile.SignalGroupVitality, Type: "commits"},
		{ID: "sig-2", EntityID: "ent-1", Group: profile.SignalGroupHygiene, Type: "has-tests"},
		{ID: "sig-3", EntityID: "ent-1", Group: profile.SignalGroupVitality, Type: "releases"},
	}

	require.NoError(t, s.AppendSignals(ctx, signals))

	vitalitySignals, err := s.GetSignalsByGroup(ctx, "ent-1", profile.SignalGroupVitality)
	require.NoError(t, err)
	assert.Len(t, vitalitySignals, 2)
	for _, sig := range vitalitySignals {
		assert.Equal(t, profile.SignalGroupVitality, sig.Group)
	}
}

func TestStore_GetSignals_NoSignals(t *testing.T) {
	t.Parallel()

	s := newMockStore()
	signals, err := s.GetSignals(context.Background(), "nonexistent-entity")
	require.NoError(t, err)
	assert.Empty(t, signals)
}

func TestStore_VersionedPostureRoundTrip(t *testing.T) {
	t.Parallel()

	s := newMockStore()
	ctx := context.Background()

	posture := &profile.Posture{
		EntityID:  "ent-1",
		Tier:      profile.PostureVettedFrozen,
		Version:   "1.0.0",
		Rationale: "audited",
		SetBy:     "team:alice+claude",
		SetAt:     time.Now().UTC(),
	}

	err := s.SetPosture(ctx, posture)
	require.NoError(t, err)

	got, err := s.GetPosture(ctx, "ent-1", "1.0.0")
	require.NoError(t, err)
	assert.Equal(t, posture, got)
}

// TestStore_PostureVersionsCoexist verifies that two versions of the
// same entity keep independent postures — the core benefit of versioned
// postures in v2.
func TestStore_PostureVersionsCoexist(t *testing.T) {
	t.Parallel()

	s := newMockStore()
	ctx := context.Background()

	v1 := &profile.Posture{
		EntityID: "ent-1", Version: "v1.15.0",
		Tier: profile.PostureVettedFrozen, Rationale: "audited",
		SetBy: "team:sarah", SetAt: time.Now().UTC(),
	}
	v2 := &profile.Posture{
		EntityID: "ent-1", Version: "v1.16.0",
		Tier: profile.PostureUnexamined, Rationale: "new release",
		SetBy: "team:sarah", SetAt: time.Now().UTC(),
	}
	require.NoError(t, s.SetPosture(ctx, v1))
	require.NoError(t, s.SetPosture(ctx, v2))

	gotV1, err := s.GetPosture(ctx, "ent-1", "v1.15.0")
	require.NoError(t, err)
	assert.Equal(t, profile.PostureVettedFrozen, gotV1.Tier)

	gotV2, err := s.GetPosture(ctx, "ent-1", "v1.16.0")
	require.NoError(t, err)
	assert.Equal(t, profile.PostureUnexamined, gotV2.Tier)
}

func TestStore_GetPostures_All(t *testing.T) {
	t.Parallel()

	s := newMockStore()
	ctx := context.Background()

	require.NoError(t, s.SetPosture(ctx, &profile.Posture{
		EntityID: "ent-1", Version: "v1.15.0", Tier: profile.PostureVettedFrozen,
		Rationale: "audited", SetBy: "team:sarah", SetAt: time.Now().UTC(),
	}))
	require.NoError(t, s.SetPosture(ctx, &profile.Posture{
		EntityID: "ent-1", Version: "v1.16.0", Tier: profile.PostureTrustedForNow,
		Rationale: "minor diff", SetBy: "team:sarah", SetAt: time.Now().UTC(),
	}))

	all, err := s.GetPostures(ctx, "ent-1")
	require.NoError(t, err)
	assert.Len(t, all, 2)
}

func TestStore_GetPosture_NotFound(t *testing.T) {
	t.Parallel()

	s := newMockStore()
	_, err := s.GetPosture(context.Background(), "nonexistent", "")
	assert.Error(t, err)
}

func TestStore_SetPosture_NilPosture(t *testing.T) {
	t.Parallel()

	s := newMockStore()
	err := s.SetPosture(context.Background(), nil)
	assert.Error(t, err)
}

func TestStore_BurnRoundTrip(t *testing.T) {
	t.Parallel()

	s := newMockStore()
	ctx := context.Background()

	burn := &profile.Burn{
		EntityID: "ent-1",
		Reason:   "malware",
		Source:   profile.BurnSourceLocal,
		BurnedAt: time.Now().UTC(),
		BurnedBy: "team:security+claude",
	}

	err := s.SetBurn(ctx, burn)
	require.NoError(t, err)

	got, err := s.GetBurn(ctx, "ent-1")
	require.NoError(t, err)
	assert.Equal(t, burn, got)
}

func TestStore_GetBurn_NotFound(t *testing.T) {
	t.Parallel()

	s := newMockStore()
	_, err := s.GetBurn(context.Background(), "nonexistent")
	assert.Error(t, err)
}

func TestStore_SetBurn_NilBurn(t *testing.T) {
	t.Parallel()

	s := newMockStore()
	err := s.SetBurn(context.Background(), nil)
	assert.Error(t, err)
}

func TestStore_ListBurns(t *testing.T) {
	t.Parallel()

	s := newMockStore()
	ctx := context.Background()

	burn1 := &profile.Burn{EntityID: "ent-1", Reason: "malware", Source: profile.BurnSourceLocal, BurnedBy: "alice"}
	burn2 := &profile.Burn{EntityID: "ent-2", Reason: "backdoor", Source: profile.BurnSourceInherited, BurnedBy: "bob"}

	require.NoError(t, s.SetBurn(ctx, burn1))
	require.NoError(t, s.SetBurn(ctx, burn2))

	burns, err := s.ListBurns(ctx)
	require.NoError(t, err)
	assert.Len(t, burns, 2)
}

func TestStore_ListBurns_Empty(t *testing.T) {
	t.Parallel()

	s := newMockStore()
	burns, err := s.ListBurns(context.Background())
	require.NoError(t, err)
	assert.Empty(t, burns)
}

func TestStore_AppendDependencyObservations_Accumulate(t *testing.T) {
	t.Parallel()

	s := newMockStore()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	survey1 := []profile.DependencyObservation{
		{ID: "obs-1", ProjectID: "proj", EntityID: "dep-1", Version: "1.0.0", Direct: true, ObservedAt: now, SurveyID: "s1"},
		{ID: "obs-2", ProjectID: "proj", EntityID: "dep-2", Version: "2.0.0", Direct: false, ObservedAt: now, SurveyID: "s1"},
	}
	require.NoError(t, s.AppendDependencyObservations(ctx, survey1))

	survey2 := []profile.DependencyObservation{
		{ID: "obs-3", ProjectID: "proj", EntityID: "dep-1", Version: "1.1.0", Direct: true, ObservedAt: now.Add(time.Hour), SurveyID: "s2"},
	}
	require.NoError(t, s.AppendDependencyObservations(ctx, survey2))

	latest, err := s.GetLatestDependencies(ctx, "proj")
	require.NoError(t, err)
	require.Len(t, latest, 1, "only s2 survey should be returned by GetLatestDependencies")
	assert.Equal(t, "1.1.0", latest[0].Version)
}

func TestStore_AppendAuditEntry(t *testing.T) {
	t.Parallel()

	s := newMockStore()
	ctx := context.Background()

	entry := &profile.AuditEntry{
		ID:        "audit-1",
		Timestamp: time.Now().UTC(),
		Actor:     "team:sarah+claude",
		Action:    "set_posture",
		EntityID:  "ent-1",
		Detail:    `{"version":"1.0.0","tier":"vetted-frozen"}`,
	}
	err := s.AppendAuditEntry(ctx, entry)
	require.NoError(t, err)
	assert.Len(t, s.auditEntries, 1)
}

func TestStore_TeamIdentityRoundTrip(t *testing.T) {
	t.Parallel()

	s := newMockStore()
	ctx := context.Background()

	team := &profile.TeamIdentity{
		ID:        "team-1",
		Name:      "sarah+claude-opus-4.6",
		CreatedAt: time.Now().UTC(),
	}
	require.NoError(t, s.PutTeamIdentity(ctx, team))

	got, err := s.GetTeamIdentity(ctx, "team-1")
	require.NoError(t, err)
	assert.Equal(t, team, got)
}

func TestStore_Close(t *testing.T) {
	t.Parallel()

	s := newMockStore()
	err := s.Close()
	require.NoError(t, err)
	assert.Equal(t, 1, s.closeCalled)
}

func TestStore_ContextCancellation(t *testing.T) {
	t.Parallel()

	s := newMockStore()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := s.GetEntity(ctx, "ent-1")
	assert.ErrorIs(t, err, context.Canceled)

	err = s.PutEntity(ctx, &profile.Entity{ID: "ent-1"})
	assert.ErrorIs(t, err, context.Canceled)

	_, err = s.FindEntityByURI(ctx, "pkg:npm/x")
	assert.ErrorIs(t, err, context.Canceled)

	_, err = s.GetSignals(ctx, "ent-1")
	assert.ErrorIs(t, err, context.Canceled)

	_, err = s.GetLatestSignals(ctx, "ent-1")
	assert.ErrorIs(t, err, context.Canceled)

	err = s.AppendSignals(ctx, []profile.Signal{})
	assert.ErrorIs(t, err, context.Canceled)

	_, err = s.GetSignalsByGroup(ctx, "ent-1", profile.SignalGroupVitality)
	assert.ErrorIs(t, err, context.Canceled)

	_, err = s.GetPosture(ctx, "ent-1", "")
	assert.ErrorIs(t, err, context.Canceled)

	_, err = s.GetPostures(ctx, "ent-1")
	assert.ErrorIs(t, err, context.Canceled)

	err = s.SetPosture(ctx, &profile.Posture{EntityID: "ent-1"})
	assert.ErrorIs(t, err, context.Canceled)

	_, err = s.GetBurn(ctx, "ent-1")
	assert.ErrorIs(t, err, context.Canceled)

	err = s.SetBurn(ctx, &profile.Burn{EntityID: "ent-1"})
	assert.ErrorIs(t, err, context.Canceled)

	_, err = s.ListBurns(ctx)
	assert.ErrorIs(t, err, context.Canceled)
}

// TestStore_PostureOverwriteSameVersion verifies that re-setting posture
// for the exact same (entity_id, version) pair replaces the earlier
// row. This is intentional — revising a decision with new rationale is
// a normal edit, not a conflict.
func TestStore_PostureOverwriteSameVersion(t *testing.T) {
	t.Parallel()

	s := newMockStore()
	ctx := context.Background()

	posture1 := &profile.Posture{
		EntityID:  "ent-1",
		Version:   "v1.0.0",
		Tier:      profile.PostureUnexamined,
		Rationale: "initial",
		SetBy:     "alice",
	}
	posture2 := &profile.Posture{
		EntityID:  "ent-1",
		Version:   "v1.0.0",
		Tier:      profile.PostureVettedFrozen,
		Rationale: "reviewed and approved",
		SetBy:     "bob",
	}

	require.NoError(t, s.SetPosture(ctx, posture1))
	require.NoError(t, s.SetPosture(ctx, posture2))

	got, err := s.GetPosture(ctx, "ent-1", "v1.0.0")
	require.NoError(t, err)
	assert.Equal(t, profile.PostureVettedFrozen, got.Tier)
	assert.Equal(t, "bob", got.SetBy)
}
