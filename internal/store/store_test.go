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

// mockStore implements the Store interface for contract testing.
type mockStore struct {
	entities map[string]*profile.Entity
	signals  map[string][]profile.Signal
	postures map[string]*profile.Posture
	burns    map[string]*profile.Burn

	// Track calls
	getCalled        int
	putCalled        int
	findCalled       int
	getSignalsCalled int
	putSignalsCalled int
	getByGroupCalled int
	getPostureCalled int
	setPostureCalled int
	getBurnCalled    int
	setBurnCalled    int
	listBurnsCalled  int
	closeCalled      int
}

func newMockStore() *mockStore {
	return &mockStore{
		entities: make(map[string]*profile.Entity),
		signals:  make(map[string][]profile.Signal),
		postures: make(map[string]*profile.Posture),
		burns:    make(map[string]*profile.Burn),
	}
}

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

func (m *mockStore) FindEntity(ctx context.Context, name string, entityType profile.EntityType) (*profile.Entity, error) {
	m.findCalled++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, e := range m.entities {
		if e.Name == name && e.Type == entityType {
			return e, nil
		}
	}
	return nil, errors.New("entity not found")
}

func (m *mockStore) GetSignals(ctx context.Context, entityID string) ([]profile.Signal, error) {
	m.getSignalsCalled++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return m.signals[entityID], nil
}

func (m *mockStore) PutSignals(ctx context.Context, signals []profile.Signal) error {
	m.putSignalsCalled++
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

func (m *mockStore) GetPosture(ctx context.Context, entityID string) (*profile.Posture, error) {
	m.getPostureCalled++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p, ok := m.postures[entityID]
	if !ok {
		return nil, errors.New("posture not found")
	}
	return p, nil
}

func (m *mockStore) SetPosture(ctx context.Context, posture *profile.Posture) error {
	m.setPostureCalled++
	if err := ctx.Err(); err != nil {
		return err
	}
	if posture == nil {
		return errors.New("posture must not be nil")
	}
	m.postures[posture.EntityID] = posture
	return nil
}

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

func (m *mockStore) Close() error {
	m.closeCalled++
	return nil
}

// Compile-time interface check.
var _ Store = (*mockStore)(nil)

// --- Contract Tests ---

func TestStore_EntityRoundTrip(t *testing.T) {
	t.Parallel()

	s := newMockStore()
	ctx := context.Background()

	entity := &profile.Entity{
		ID:        "ent-1",
		Type:      profile.EntityPackage,
		Name:      "lodash",
		Ecosystem: "npm",
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
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

func TestStore_FindEntity(t *testing.T) {
	t.Parallel()

	s := newMockStore()
	ctx := context.Background()

	entity := &profile.Entity{
		ID:   "ent-1",
		Type: profile.EntityPackage,
		Name: "express",
	}
	require.NoError(t, s.PutEntity(ctx, entity))

	found, err := s.FindEntity(ctx, "express", profile.EntityPackage)
	require.NoError(t, err)
	assert.Equal(t, entity, found)
	assert.Equal(t, 1, s.findCalled)
}

func TestStore_FindEntity_WrongType(t *testing.T) {
	t.Parallel()

	s := newMockStore()
	ctx := context.Background()

	entity := &profile.Entity{
		ID:   "ent-1",
		Type: profile.EntityPackage,
		Name: "express",
	}
	require.NoError(t, s.PutEntity(ctx, entity))

	_, err := s.FindEntity(ctx, "express", profile.EntityProject)
	assert.Error(t, err)
}

func TestStore_PutEntity_NilEntity(t *testing.T) {
	t.Parallel()

	s := newMockStore()
	err := s.PutEntity(context.Background(), nil)
	assert.Error(t, err)
}

func TestStore_SignalRoundTrip(t *testing.T) {
	t.Parallel()

	s := newMockStore()
	ctx := context.Background()

	signals := []profile.Signal{
		{ID: "sig-1", EntityID: "ent-1", Group: profile.SignalGroupVitality, Type: "commits"},
		{ID: "sig-2", EntityID: "ent-1", Group: profile.SignalGroupHygiene, Type: "has-tests"},
	}

	err := s.PutSignals(ctx, signals)
	require.NoError(t, err)

	got, err := s.GetSignals(ctx, "ent-1")
	require.NoError(t, err)
	assert.Len(t, got, 2)
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

	require.NoError(t, s.PutSignals(ctx, signals))

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

func TestStore_PostureRoundTrip(t *testing.T) {
	t.Parallel()

	s := newMockStore()
	ctx := context.Background()

	posture := &profile.Posture{
		EntityID:  "ent-1",
		Tier:      profile.PostureVettedFrozen,
		Version:   "1.0.0",
		Rationale: "audited",
		SetBy:     "alice",
		SetAt:     time.Now().UTC(),
	}

	err := s.SetPosture(ctx, posture)
	require.NoError(t, err)

	got, err := s.GetPosture(ctx, "ent-1")
	require.NoError(t, err)
	assert.Equal(t, posture, got)
}

func TestStore_GetPosture_NotFound(t *testing.T) {
	t.Parallel()

	s := newMockStore()
	_, err := s.GetPosture(context.Background(), "nonexistent")
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
		BurnedBy: "security-team",
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

	_, err = s.FindEntity(ctx, "name", profile.EntityPackage)
	assert.ErrorIs(t, err, context.Canceled)

	_, err = s.GetSignals(ctx, "ent-1")
	assert.ErrorIs(t, err, context.Canceled)

	err = s.PutSignals(ctx, []profile.Signal{})
	assert.ErrorIs(t, err, context.Canceled)

	_, err = s.GetSignalsByGroup(ctx, "ent-1", profile.SignalGroupVitality)
	assert.ErrorIs(t, err, context.Canceled)

	_, err = s.GetPosture(ctx, "ent-1")
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

func TestStore_PostureOverwrite(t *testing.T) {
	t.Parallel()

	s := newMockStore()
	ctx := context.Background()

	posture1 := &profile.Posture{
		EntityID:  "ent-1",
		Tier:      profile.PostureUnexamined,
		Rationale: "initial",
		SetBy:     "alice",
	}
	posture2 := &profile.Posture{
		EntityID:  "ent-1",
		Tier:      profile.PostureVettedFrozen,
		Rationale: "reviewed and approved",
		SetBy:     "bob",
	}

	require.NoError(t, s.SetPosture(ctx, posture1))
	require.NoError(t, s.SetPosture(ctx, posture2))

	got, err := s.GetPosture(ctx, "ent-1")
	require.NoError(t, err)
	assert.Equal(t, profile.PostureVettedFrozen, got.Tier)
	assert.Equal(t, "bob", got.SetBy)
}
