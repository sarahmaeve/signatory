package source

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal"
)

// fakeSignalStore is a hand-built SignalStore for tests. Holds
// signals keyed by entity ID; an optional callErr is returned from
// every GetLatestSignals call to exercise the error-wrapping path.
type fakeSignalStore struct {
	signals map[string][]profile.Signal
	callErr error
}

func (f *fakeSignalStore) GetLatestSignals(_ context.Context, entityID string) ([]profile.Signal, error) {
	if f.callErr != nil {
		return nil, f.callErr
	}
	return f.signals[entityID], nil
}

// validPinTableJSON returns a JSON-encoded pin table value with one
// pin and the listed module path. Used by multiple tests.
func validPinTableJSON(t *testing.T, modulePath string) []byte {
	t.Helper()
	value := map[string]any{
		"module_path":             modulePath,
		"version_count_total":     3,
		"version_count_processed": 3,
		"pins": []map[string]any{
			{
				"version":      "v0.1.0",
				"sha":          "abc1234567890123456789012345678901234567",
				"source":       "proxy.golang.org",
				"published_at": "2026-04-15T10:00:00Z",
			},
		},
		"missing_origin_versions": []string{},
		"fetch_failed_versions":   []string{},
	}
	bytes, err := json.Marshal(value)
	require.NoError(t, err)
	return bytes
}

func mkSignal(t *testing.T, entityID, sigType string, value []byte) profile.Signal {
	t.Helper()
	return profile.Signal{
		ID:                "test:" + entityID + ":" + sigType,
		EntityID:          entityID,
		Type:              sigType,
		Group:             profile.SignalGroupPublication,
		Source:            "go-publish",
		ForgeryResistance: profile.ForgeryVeryHigh,
		Value:             value,
		CollectedAt:       time.Now().UTC(),
		ExpiresAt:         time.Now().UTC().Add(24 * time.Hour),
	}
}

func mkEntity(id string) *profile.Entity {
	return &profile.Entity{ID: id, CanonicalURI: "pkg:golang/example.com/" + id}
}

// ============================================================
// In-run CollectionResult lookup
// ============================================================

func TestVersionPinTable_FromInRunResult_Returns(t *testing.T) {
	t.Parallel()

	entity := mkEntity("ent-foo")
	inRun := &signal.CollectionResult{}
	inRun.Collected = append(inRun.Collected, signal.SignalOrAbsence{
		Signal: ptr(mkSignal(t, entity.ID, "version_pin_table",
			validPinTableJSON(t, "example.com/foo"))),
	})

	src := NewPinSource(inRun, nil)
	pt, err := src.VersionPinTable(context.Background(), entity)
	require.NoError(t, err)
	assert.Equal(t, "example.com/foo", pt.ModulePath)
	assert.Equal(t, 3, pt.VersionCountTotal)
	require.Len(t, pt.Pins, 1)
	assert.Equal(t, "v0.1.0", pt.Pins[0].Version)
	assert.Equal(t, "proxy.golang.org", pt.Pins[0].Source)
	expectedTime, _ := time.Parse(time.RFC3339, "2026-04-15T10:00:00Z")
	assert.Equal(t, expectedTime, pt.Pins[0].PublishedAt)
}

func TestVersionPinTable_InRunPreferredOverStore(t *testing.T) {
	t.Parallel()

	entity := mkEntity("ent-foo")

	inRun := &signal.CollectionResult{}
	inRun.Collected = append(inRun.Collected, signal.SignalOrAbsence{
		Signal: ptr(mkSignal(t, entity.ID, "version_pin_table",
			validPinTableJSON(t, "from-in-run"))),
	})

	store := &fakeSignalStore{
		signals: map[string][]profile.Signal{
			entity.ID: {mkSignal(t, entity.ID, "version_pin_table",
				validPinTableJSON(t, "from-store"))},
		},
	}

	src := NewPinSource(inRun, store)
	pt, err := src.VersionPinTable(context.Background(), entity)
	require.NoError(t, err)
	assert.Equal(t, "from-in-run", pt.ModulePath, "in-run should be preferred over store")
}

func TestVersionPinTable_InRunHasOtherSignalsButNotPinTable_FallsThroughToStore(t *testing.T) {
	t.Parallel()

	entity := mkEntity("ent-foo")

	inRun := &signal.CollectionResult{}
	inRun.Collected = append(inRun.Collected, signal.SignalOrAbsence{
		Signal: ptr(mkSignal(t, entity.ID, "version_count", []byte(`{"count":5}`))),
	})

	store := &fakeSignalStore{
		signals: map[string][]profile.Signal{
			entity.ID: {mkSignal(t, entity.ID, "version_pin_table",
				validPinTableJSON(t, "from-store"))},
		},
	}

	src := NewPinSource(inRun, store)
	pt, err := src.VersionPinTable(context.Background(), entity)
	require.NoError(t, err)
	assert.Equal(t, "from-store", pt.ModulePath)
}

func TestVersionPinTable_InRunPinTableForDifferentEntity_FallsThroughToStore(t *testing.T) {
	t.Parallel()

	entity := mkEntity("ent-foo")

	inRun := &signal.CollectionResult{}
	// Pin table exists in the in-run result but for a different entity.
	inRun.Collected = append(inRun.Collected, signal.SignalOrAbsence{
		Signal: ptr(mkSignal(t, "ent-bar", "version_pin_table",
			validPinTableJSON(t, "wrong-entity"))),
	})

	store := &fakeSignalStore{
		signals: map[string][]profile.Signal{
			entity.ID: {mkSignal(t, entity.ID, "version_pin_table",
				validPinTableJSON(t, "from-store"))},
		},
	}

	src := NewPinSource(inRun, store)
	pt, err := src.VersionPinTable(context.Background(), entity)
	require.NoError(t, err)
	assert.Equal(t, "from-store", pt.ModulePath)
}

// ============================================================
// Store fallback
// ============================================================

func TestVersionPinTable_FromStore_Returns(t *testing.T) {
	t.Parallel()

	entity := mkEntity("ent-foo")
	store := &fakeSignalStore{
		signals: map[string][]profile.Signal{
			entity.ID: {mkSignal(t, entity.ID, "version_pin_table",
				validPinTableJSON(t, "example.com/foo"))},
		},
	}

	src := NewPinSource(nil, store)
	pt, err := src.VersionPinTable(context.Background(), entity)
	require.NoError(t, err)
	assert.Equal(t, "example.com/foo", pt.ModulePath)
}

func TestVersionPinTable_StoreError_Wrapped(t *testing.T) {
	t.Parallel()

	entity := mkEntity("ent-foo")
	wantErr := errors.New("store boom")
	store := &fakeSignalStore{callErr: wantErr}

	src := NewPinSource(nil, store)
	_, err := src.VersionPinTable(context.Background(), entity)
	require.Error(t, err)
	assert.ErrorIs(t, err, wantErr)
}

func TestVersionPinTable_StoreHasUnrelatedSignalsOnly_NotAvailable(t *testing.T) {
	t.Parallel()

	entity := mkEntity("ent-foo")
	store := &fakeSignalStore{
		signals: map[string][]profile.Signal{
			entity.ID: {mkSignal(t, entity.ID, "version_count", []byte(`{"count":5}`))},
		},
	}

	src := NewPinSource(nil, store)
	_, err := src.VersionPinTable(context.Background(), entity)
	assert.ErrorIs(t, err, ErrPinTableNotAvailable)
}

// ============================================================
// Absence / error cases
// ============================================================

func TestVersionPinTable_NoInRunNoStore_NotAvailable(t *testing.T) {
	t.Parallel()

	entity := mkEntity("ent-foo")
	src := NewPinSource(nil, nil)
	_, err := src.VersionPinTable(context.Background(), entity)
	assert.ErrorIs(t, err, ErrPinTableNotAvailable)
}

func TestVersionPinTable_EmptyInRunEmptyStore_NotAvailable(t *testing.T) {
	t.Parallel()

	entity := mkEntity("ent-foo")
	src := NewPinSource(&signal.CollectionResult{}, &fakeSignalStore{})
	_, err := src.VersionPinTable(context.Background(), entity)
	assert.ErrorIs(t, err, ErrPinTableNotAvailable)
}

func TestVersionPinTable_NilEntity_Errors(t *testing.T) {
	t.Parallel()

	src := NewPinSource(nil, nil)
	_, err := src.VersionPinTable(context.Background(), nil)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrPinTableNotAvailable, "nil entity is a different error class")
}

func TestVersionPinTable_MalformedInRunJSON_PropagatesError(t *testing.T) {
	t.Parallel()

	entity := mkEntity("ent-foo")
	inRun := &signal.CollectionResult{}
	inRun.Collected = append(inRun.Collected, signal.SignalOrAbsence{
		Signal: ptr(mkSignal(t, entity.ID, "version_pin_table", []byte(`{"module_path": broken`))),
	})

	src := NewPinSource(inRun, nil)
	_, err := src.VersionPinTable(context.Background(), entity)
	require.Error(t, err)
	// Not a not-available error — this is a parse failure.
	assert.NotErrorIs(t, err, ErrPinTableNotAvailable)
}

func TestVersionPinTable_MalformedStoreJSON_PropagatesError(t *testing.T) {
	t.Parallel()

	entity := mkEntity("ent-foo")
	store := &fakeSignalStore{
		signals: map[string][]profile.Signal{
			entity.ID: {mkSignal(t, entity.ID, "version_pin_table", []byte(`{"module_path": broken`))},
		},
	}

	src := NewPinSource(nil, store)
	_, err := src.VersionPinTable(context.Background(), entity)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrPinTableNotAvailable)
}

// ptr is a small generic helper for taking the address of a value
// in literal initialization. signal.SignalOrAbsence.Signal is a
// pointer field; tests need to pass &literal which can't be done
// inline.
func ptr[T any](v T) *T { return &v }
