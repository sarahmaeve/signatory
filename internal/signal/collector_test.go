package signal

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
)

// mockCollector is a test double for the Collector interface.
type mockCollector struct {
	name    string
	signals []profile.Signal
	err     error
	// called tracks whether Collect was invoked.
	called bool
	// receivedCtx captures the context passed to Collect.
	receivedCtx context.Context
	// receivedEntity captures the entity passed to Collect.
	receivedEntity *profile.Entity
}

func (m *mockCollector) Name() string { return m.name }

func (m *mockCollector) Collect(ctx context.Context, entity *profile.Entity) ([]profile.Signal, error) {
	m.called = true
	m.receivedCtx = ctx
	m.receivedEntity = entity
	if m.err != nil {
		return nil, m.err
	}
	return m.signals, nil
}

// Compile-time interface check.
var _ Collector = (*mockCollector)(nil)

func TestCollector_MockSatisfiesInterface(t *testing.T) {
	t.Parallel()

	var c Collector = &mockCollector{name: "test"}
	assert.Equal(t, "test", c.Name())
}

func TestCollector_NameReturnsIdentifier(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		mockName string
	}{
		{"GitHub", "github"},
		{"NPMRegistry", "npm-registry"},
		{"OpenSSFScorecard", "openssf-scorecard"},
		{"EmptyName", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := &mockCollector{name: tc.mockName}
			assert.Equal(t, tc.mockName, c.Name())
		})
	}
}

func TestCollector_CollectReturnsSignals(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	expectedSignals := []profile.Signal{
		{
			ID:       "sig-1",
			EntityID: "ent-1",
			Type:     "commit-frequency",
			Group:    profile.SignalGroupVitality,
		},
		{
			ID:       "sig-2",
			EntityID: "ent-1",
			Type:     "has-codeowners",
			Group:    profile.SignalGroupGovernance,
		},
	}

	entity := &profile.Entity{
		ID:        "ent-1",
		Type:      profile.EntityPackage,
		Name:      "test-pkg",
		CreatedAt: now,
		UpdatedAt: now,
	}

	collector := &mockCollector{
		name:    "github",
		signals: expectedSignals,
	}

	ctx := context.Background()
	signals, err := collector.Collect(ctx, entity)

	require.NoError(t, err)
	assert.True(t, collector.called)
	assert.Equal(t, entity, collector.receivedEntity)
	assert.Equal(t, expectedSignals, signals)
	assert.Len(t, signals, 2)
}

func TestCollector_CollectReturnsError(t *testing.T) {
	t.Parallel()

	expectedErr := errors.New("API rate limit exceeded")
	collector := &mockCollector{
		name: "github",
		err:  expectedErr,
	}

	entity := &profile.Entity{ID: "ent-1", Name: "test"}
	signals, err := collector.Collect(context.Background(), entity)

	assert.Nil(t, signals)
	assert.ErrorIs(t, err, expectedErr)
	assert.True(t, collector.called)
}

func TestCollector_CollectReturnsEmptySlice(t *testing.T) {
	t.Parallel()

	collector := &mockCollector{
		name:    "scorecard",
		signals: []profile.Signal{},
	}

	signals, err := collector.Collect(context.Background(), &profile.Entity{ID: "ent-1"})

	require.NoError(t, err)
	assert.NotNil(t, signals)
	assert.Empty(t, signals)
}

func TestCollector_CollectReturnsNilSlice(t *testing.T) {
	t.Parallel()

	collector := &mockCollector{
		name:    "empty-source",
		signals: nil,
	}

	signals, err := collector.Collect(context.Background(), &profile.Entity{ID: "ent-1"})

	require.NoError(t, err)
	assert.Nil(t, signals)
}

func TestCollector_CollectReceivesContext(t *testing.T) {
	t.Parallel()

	// Document the expectation that context is passed through,
	// allowing implementations to respect cancellation.
	collector := &mockCollector{name: "ctx-test"}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	entity := &profile.Entity{ID: "ent-1"}
	_, _ = collector.Collect(ctx, entity)

	assert.Equal(t, ctx, collector.receivedCtx, "context must be passed through to the collector")
}

func TestCollector_ContextCancellationExpectation(t *testing.T) {
	t.Parallel()

	// This test documents the expectation that Collector implementations
	// should check ctx.Done() and return early if cancelled.
	// We test with a collector that simulates checking context cancellation.
	cancelledCollector := &contextAwareCollector{name: "cancellation-aware"}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately.

	_, err := cancelledCollector.Collect(ctx, &profile.Entity{ID: "ent-1"})
	assert.ErrorIs(t, err, context.Canceled)
}

// contextAwareCollector is a mock that respects context cancellation.
type contextAwareCollector struct {
	name string
}

func (c *contextAwareCollector) Name() string { return c.name }

func (c *contextAwareCollector) Collect(ctx context.Context, entity *profile.Entity) ([]profile.Signal, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
		return nil, nil
	}
}

var _ Collector = (*contextAwareCollector)(nil)

func TestCollector_NilEntity(t *testing.T) {
	t.Parallel()

	// Ensure the interface does not panic with a nil entity --
	// the contract allows passing nil; implementations should handle it.
	collector := &mockCollector{name: "nil-entity"}

	signals, err := collector.Collect(context.Background(), nil)
	require.NoError(t, err)
	assert.Nil(t, signals)
	assert.Nil(t, collector.receivedEntity)
}
