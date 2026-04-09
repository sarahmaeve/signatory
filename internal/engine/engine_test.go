package engine

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/ecosystem"
	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal"
	"github.com/sarahmaeve/signatory/internal/store"
)

// --- Mocks ---

type mockStore struct{}

func (m *mockStore) GetEntity(ctx context.Context, id string) (*profile.Entity, error) {
	return nil, nil
}
func (m *mockStore) PutEntity(ctx context.Context, entity *profile.Entity) error { return nil }
func (m *mockStore) FindEntity(ctx context.Context, name string, entityType profile.EntityType) (*profile.Entity, error) {
	return nil, nil
}
func (m *mockStore) GetSignals(ctx context.Context, entityID string) ([]profile.Signal, error) {
	return nil, nil
}
func (m *mockStore) PutSignals(ctx context.Context, signals []profile.Signal) error { return nil }
func (m *mockStore) GetSignalsByGroup(ctx context.Context, entityID string, group profile.SignalGroup) ([]profile.Signal, error) {
	return nil, nil
}
func (m *mockStore) GetPosture(ctx context.Context, entityID string) (*profile.Posture, error) {
	return nil, nil
}
func (m *mockStore) SetPosture(ctx context.Context, posture *profile.Posture) error { return nil }
func (m *mockStore) GetBurn(ctx context.Context, entityID string) (*profile.Burn, error) {
	return nil, nil
}
func (m *mockStore) SetBurn(ctx context.Context, burn *profile.Burn) error { return nil }
func (m *mockStore) ListBurns(ctx context.Context) ([]profile.Burn, error) {
	return nil, nil
}
func (m *mockStore) Close() error { return nil }

var _ store.Store = (*mockStore)(nil)

type mockCollector struct {
	name string
}

func (m *mockCollector) Name() string { return m.name }
func (m *mockCollector) Collect(ctx context.Context, entity *profile.Entity) (*signal.CollectionResult, error) {
	return &signal.CollectionResult{}, nil
}

var _ signal.Collector = (*mockCollector)(nil)

type mockProvider struct {
	name string
}

func (m *mockProvider) Name() string                                              { return m.name }
func (m *mockProvider) DetectManifest(dir string) (string, bool)                  { return "", false }
func (m *mockProvider) ParseManifest(path string) ([]ecosystem.Dependency, error) { return nil, nil }
func (m *mockProvider) ResolveRepo(ctx context.Context, packageName string) (string, error) {
	return "", nil
}

var _ ecosystem.Provider = (*mockProvider)(nil)

// --- Tests ---

func TestNew_WithValidInputs(t *testing.T) {
	t.Parallel()

	s := &mockStore{}
	collectors := []signal.Collector{
		&mockCollector{name: "github"},
		&mockCollector{name: "npm"},
	}
	ecosystems := []ecosystem.Provider{
		&mockProvider{name: "npm"},
		&mockProvider{name: "pypi"},
	}

	engine := New(s, collectors, ecosystems)

	require.NotNil(t, engine)
	assert.Equal(t, s, engine.store)
	assert.Len(t, engine.collectors, 2)
	assert.Len(t, engine.ecosystems, 2)
}

func TestNew_WithNilStore(t *testing.T) {
	t.Parallel()

	// The constructor does not validate inputs, so nil is accepted.
	// This documents the current behavior and tests that it does not panic.
	engine := New(nil, nil, nil)
	require.NotNil(t, engine)
	assert.Nil(t, engine.store)
	assert.Nil(t, engine.collectors)
	assert.Nil(t, engine.ecosystems)
}

func TestNew_WithEmptySlices(t *testing.T) {
	t.Parallel()

	s := &mockStore{}
	engine := New(s, []signal.Collector{}, []ecosystem.Provider{})

	require.NotNil(t, engine)
	assert.Equal(t, s, engine.store)
	assert.NotNil(t, engine.collectors)
	assert.Empty(t, engine.collectors)
	assert.NotNil(t, engine.ecosystems)
	assert.Empty(t, engine.ecosystems)
}

func TestNew_WithNilCollectorsAndProviders(t *testing.T) {
	t.Parallel()

	s := &mockStore{}
	engine := New(s, nil, nil)

	require.NotNil(t, engine)
	assert.Equal(t, s, engine.store)
	assert.Nil(t, engine.collectors)
	assert.Nil(t, engine.ecosystems)
}

func TestNew_HoldsReferenceToStore(t *testing.T) {
	t.Parallel()

	s := &mockStore{}
	engine := New(s, nil, nil)

	// Verify the engine holds the same reference, not a copy.
	assert.Same(t, s, engine.store)
}

func TestNew_HoldsReferencesToCollectors(t *testing.T) {
	t.Parallel()

	c1 := &mockCollector{name: "github"}
	c2 := &mockCollector{name: "npm"}
	collectors := []signal.Collector{c1, c2}

	engine := New(&mockStore{}, collectors, nil)

	require.Len(t, engine.collectors, 2)
	assert.Same(t, c1, engine.collectors[0])
	assert.Same(t, c2, engine.collectors[1])
}

func TestNew_HoldsReferencesToEcosystems(t *testing.T) {
	t.Parallel()

	p1 := &mockProvider{name: "npm"}
	p2 := &mockProvider{name: "pypi"}
	ecosystems := []ecosystem.Provider{p1, p2}

	engine := New(&mockStore{}, nil, ecosystems)

	require.Len(t, engine.ecosystems, 2)
	assert.Same(t, p1, engine.ecosystems[0])
	assert.Same(t, p2, engine.ecosystems[1])
}

func TestNew_SingleCollectorAndProvider(t *testing.T) {
	t.Parallel()

	s := &mockStore{}
	c := &mockCollector{name: "solo-collector"}
	p := &mockProvider{name: "solo-provider"}

	engine := New(s, []signal.Collector{c}, []ecosystem.Provider{p})

	require.NotNil(t, engine)
	assert.Len(t, engine.collectors, 1)
	assert.Len(t, engine.ecosystems, 1)
	assert.Equal(t, "solo-collector", engine.collectors[0].Name())
	assert.Equal(t, "solo-provider", engine.ecosystems[0].Name())
}
