package deltas

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/store"
)

// fakeStore is an in-memory ComputerStore for hermetic Computer
// tests. signalsByEntity drives GetSignals; entityByURI drives
// FindEntityByURI. Tests seed either or both.
type fakeStore struct {
	entityByURI     map[string]*profile.Entity
	signalsByEntity map[string][]profile.Signal
}

func (f *fakeStore) FindEntityByURI(_ context.Context, canonicalURI string) (*profile.Entity, error) {
	if e, ok := f.entityByURI[canonicalURI]; ok {
		return e, nil
	}
	return nil, store.ErrNotFound
}

func (f *fakeStore) GetSignals(_ context.Context, entityID string) ([]profile.Signal, error) {
	return f.signalsByEntity[entityID], nil
}

// mustMarshal panics on error — test-only helper that keeps the
// scenario seeding readable.
func mustMarshal(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}

// seededFakeStore returns a fakeStore preloaded with a two-observation
// signal history on one entity. Useful for the bulk of the tests
// below.
func seededFakeStore(t *testing.T) *fakeStore {
	t.Helper()
	t1 := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 5, 12, 15, 0, 0, 0, time.UTC)
	entity := &profile.Entity{ID: "e1", CanonicalURI: "pkg:npm/example"}
	signals := []profile.Signal{
		{
			ID: "s1", EntityID: "e1", Type: "stars", Source: "github",
			Group: profile.SignalGroupCriticality, CollectedAt: t1,
			Value: mustMarshal(t, map[string]any{"count": 100}),
		},
		{
			ID: "s2", EntityID: "e1", Type: "stars", Source: "github",
			Group: profile.SignalGroupCriticality, CollectedAt: t2,
			Value: mustMarshal(t, map[string]any{"count": 150}),
		},
		{
			ID: "s3", EntityID: "e1", Type: "forks", Source: "github",
			Group: profile.SignalGroupCriticality, CollectedAt: t1,
			Value: mustMarshal(t, map[string]any{"count": 10}),
		},
		{
			ID: "s4", EntityID: "e1", Type: "forks", Source: "github",
			Group: profile.SignalGroupCriticality, CollectedAt: t2,
			Value: mustMarshal(t, map[string]any{"count": 12}),
		},
	}
	return &fakeStore{
		entityByURI:     map[string]*profile.Entity{"pkg:npm/example": entity},
		signalsByEntity: map[string][]profile.Signal{"e1": signals},
	}
}

// TestComputer_NotFound: target absent from store → ErrEntityNotFound.
func TestComputer_NotFound(t *testing.T) {
	t.Parallel()
	c := New(&fakeStore{entityByURI: map[string]*profile.Entity{}})
	_, err := c.Compute(context.Background(), Params{
		Target: "pkg:npm/nonexistent",
		Window: TimeWindow{All: true},
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrEntityNotFound)
}

// TestComputer_HappyPath: two observations of one signal type
// produce one transition with one pair-diff carrying the actual
// scalar change. Asserts not just "a diff was produced" but the
// specific Before/After values, so a regression in
// buildSignalDeltas (wrong ordering, swapped pair endpoints,
// stale value) breaks the test.
func TestComputer_HappyPath(t *testing.T) {
	t.Parallel()
	c := New(seededFakeStore(t))
	got, err := c.Compute(context.Background(), Params{
		Target: "pkg:npm/example",
		Window: TimeWindow{All: true},
	})
	require.NoError(t, err)

	assert.Equal(t, "pkg:npm/example", got.Target)
	require.Len(t, got.Groups, 2, "two signal types → two groups")

	// Groups are sorted by (signal_group, type, source); both have
	// the same group, so type sort applies: forks < stars.
	assert.Equal(t, "forks", got.Groups[0].Type)
	assert.Equal(t, "stars", got.Groups[1].Type)

	// stars: count went 100 → 150. The pair-diff must surface the
	// scalar change with Before/After in the right direction
	// (chronologically: earlier observation is "Before").
	starsDiff := got.Groups[1].PairDiffs[0]
	require.Contains(t, starsDiff.Changed, "count",
		"the count field's transition must surface as a Changed entry")
	change := starsDiff.Changed["count"]
	assert.Equal(t, float64(100), change.Before,
		"earlier observation's count is the Before value")
	assert.Equal(t, float64(150), change.After,
		"later observation's count is the After value")

	// forks: count went 10 → 12 — same shape check, lighter touch.
	forksDiff := got.Groups[0].PairDiffs[0]
	require.Contains(t, forksDiff.Changed, "count")
	assert.Equal(t, float64(10), forksDiff.Changed["count"].Before)
	assert.Equal(t, float64(12), forksDiff.Changed["count"].After)
}

// TestComputer_FilterByType: --type narrows to one signal.
func TestComputer_FilterByType(t *testing.T) {
	t.Parallel()
	c := New(seededFakeStore(t))
	got, err := c.Compute(context.Background(), Params{
		Target: "pkg:npm/example",
		Window: TimeWindow{All: true},
		Type:   "stars",
	})
	require.NoError(t, err)
	require.Len(t, got.Groups, 1)
	assert.Equal(t, "stars", got.Groups[0].Type)
}

// TestComputer_FilterBySource: --source narrows. Use a seeded store
// with two sources to exercise the filter.
func TestComputer_FilterBySource(t *testing.T) {
	t.Parallel()
	t1 := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 5, 12, 15, 0, 0, 0, time.UTC)
	fs := &fakeStore{
		entityByURI: map[string]*profile.Entity{
			"pkg:npm/example": {ID: "e1", CanonicalURI: "pkg:npm/example"},
		},
		signalsByEntity: map[string][]profile.Signal{
			"e1": {
				{
					ID: "a1", EntityID: "e1", Type: "stars", Source: "github",
					Group: profile.SignalGroupCriticality, CollectedAt: t1,
					Value: mustMarshal(t, map[string]any{"count": 1}),
				},
				{
					ID: "a2", EntityID: "e1", Type: "stars", Source: "github",
					Group: profile.SignalGroupCriticality, CollectedAt: t2,
					Value: mustMarshal(t, map[string]any{"count": 2}),
				},
				{
					ID: "b1", EntityID: "e1", Type: "stars", Source: "gitlab",
					Group: profile.SignalGroupCriticality, CollectedAt: t1,
					Value: mustMarshal(t, map[string]any{"count": 5}),
				},
				{
					ID: "b2", EntityID: "e1", Type: "stars", Source: "gitlab",
					Group: profile.SignalGroupCriticality, CollectedAt: t2,
					Value: mustMarshal(t, map[string]any{"count": 7}),
				},
			},
		},
	}
	c := New(fs)
	got, err := c.Compute(context.Background(), Params{
		Target: "pkg:npm/example",
		Window: TimeWindow{All: true},
		Source: "gitlab",
	})
	require.NoError(t, err)
	require.Len(t, got.Groups, 1)
	assert.Equal(t, "gitlab", got.Groups[0].Source)
}

// TestComputer_WindowRange: range narrows to a subset.
func TestComputer_WindowRange(t *testing.T) {
	t.Parallel()
	c := New(seededFakeStore(t))
	// Range that admits only t2 (2026-05-12).
	got, err := c.Compute(context.Background(), Params{
		Target: "pkg:npm/example",
		Window: TimeWindow{
			RangeStart: time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC),
			RangeEnd:   time.Date(2026, 5, 15, 0, 0, 0, 0, time.UTC),
		},
	})
	require.NoError(t, err)
	for _, g := range got.Groups {
		assert.Len(t, g.Observations, 1,
			"range excludes t1; only t2 survives")
		assert.Empty(t, g.PairDiffs, "single observation → no diffs")
	}
}

// TestComputer_TargetResolution: a non-canonical input form (a
// github URL) must be resolved to the canonical URI before lookup.
// The fake store only contains the canonical URI key, so the test
// fails if resolveTarget is a no-op (the lookup would miss and we'd
// get ErrEntityNotFound instead of a successful Compute).
//
// This is the load-bearing assertion that the resolver is actually
// invoked. Full resolver coverage lives in profile/target_test.go.
func TestComputer_TargetResolution(t *testing.T) {
	t.Parallel()
	t1 := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 5, 12, 15, 0, 0, 0, time.UTC)
	canonical := "repo:github/alecthomas/kong"
	fs := &fakeStore{
		entityByURI: map[string]*profile.Entity{
			canonical: {ID: "e1", CanonicalURI: canonical},
		},
		signalsByEntity: map[string][]profile.Signal{
			"e1": {
				{
					ID: "s1", EntityID: "e1", Type: "stars", Source: "github",
					Group: profile.SignalGroupCriticality, CollectedAt: t1,
					Value: mustMarshal(t, map[string]any{"count": 1}),
				},
				{
					ID: "s2", EntityID: "e1", Type: "stars", Source: "github",
					Group: profile.SignalGroupCriticality, CollectedAt: t2,
					Value: mustMarshal(t, map[string]any{"count": 2}),
				},
			},
		},
	}
	c := New(fs)
	got, err := c.Compute(context.Background(), Params{
		Target: "https://github.com/alecthomas/kong", // NOT canonical
		Window: TimeWindow{All: true},
	})
	require.NoError(t, err,
		"github URL must resolve to repo:github/... before lookup; "+
			"a NotFound here means resolveTarget did not transform the input")
	assert.Equal(t, canonical, got.Target,
		"Target on RenderInput is the canonical URI, not the raw input")
}

// TestComputer_StoreErrorPropagation: non-NotFound errors from the
// store propagate rather than being swallowed.
func TestComputer_StoreErrorPropagation(t *testing.T) {
	t.Parallel()
	customErr := errors.New("database is on fire")
	fs := &errFakeStore{err: customErr}
	c := New(fs)
	_, err := c.Compute(context.Background(), Params{
		Target: "pkg:npm/example",
		Window: TimeWindow{All: true},
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, customErr)
	assert.NotErrorIs(t, err, ErrEntityNotFound,
		"arbitrary errors must not be misclassified as not-found")
}

// depsFakeStore returns a fakeStore preloaded with two observations
// of a *_dependencies signal on one entity: a prior `direct` array
// and a current one that adds exactly one entry. Models the real
// JSON round-trip (counts arrive as float64, arrays as []any) so the
// test exercises the same value shapes Compute sees in production.
func depsFakeStore(t *testing.T, uri, sigType, src string,
	priorDirect, currentDirect []string) *fakeStore {
	t.Helper()
	t1 := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 5, 12, 15, 0, 0, 0, time.UTC)
	mk := func(direct []string) json.RawMessage {
		return mustMarshal(t, map[string]any{
			"direct_count":   len(direct),
			"indirect_count": 0,
			"total_count":    len(direct),
			"direct":         direct,
		})
	}
	entity := &profile.Entity{ID: "e1", CanonicalURI: uri}
	signals := []profile.Signal{
		{
			ID: "d1", EntityID: "e1", Type: sigType, Source: src,
			Group: profile.SignalGroupGovernance, CollectedAt: t1,
			Value: mk(priorDirect),
		},
		{
			ID: "d2", EntityID: "e1", Type: sigType, Source: src,
			Group: profile.SignalGroupGovernance, CollectedAt: t2,
			Value: mk(currentDirect),
		},
	}
	return &fakeStore{
		entityByURI:     map[string]*profile.Entity{uri: entity},
		signalsByEntity: map[string][]profile.Signal{"e1": signals},
	}
}

// assertDependencyAdded pins that a two-observation *_dependencies
// history surfaces the newly-added dependency as a clean ElementAdded
// through the full Compute path, with the direct_count scalar as the
// reliable backstop. This is the end-to-end-through-Compute proof the
// live dogfood could not produce (real packages did not change deps
// between observations); TestDiff_* pins the same at the Diff layer.
func assertDependencyAdded(t *testing.T, fs *fakeStore, uri, sigType string,
	wantBefore, wantAfter int, wantAdded string) {
	t.Helper()
	got, err := New(fs).Compute(context.Background(), Params{
		Target: uri,
		Window: TimeWindow{All: true},
		Type:   sigType,
	})
	require.NoError(t, err)
	require.Len(t, got.Groups, 1)
	assert.Equal(t, sigType, got.Groups[0].Type)
	require.Len(t, got.Groups[0].PairDiffs, 1,
		"two observations → exactly one pair-diff")

	diff := got.Groups[0].PairDiffs[0]

	// Backstop: the count scalar always moves when the set changes.
	require.Contains(t, diff.Changed, "direct_count")
	assert.Equal(t, float64(wantBefore), diff.Changed["direct_count"].Before)
	assert.Equal(t, float64(wantAfter), diff.Changed["direct_count"].After)

	// The new dependency surfaces by name via set-diff.
	require.Contains(t, diff.Changed, "direct")
	arrayChange := diff.Changed["direct"]
	assert.Equal(t, ChangeKindArray, arrayChange.Kind)
	require.Len(t, arrayChange.Elements, 1)
	assert.Equal(t, ElementAdded, arrayChange.Elements[0].Kind)
	assert.Equal(t, wantAdded, arrayChange.Elements[0].After)
}

// TestComputer_NpmDependencyAdded: a new npm dependency surfaces as an
// ElementAdded through the full Compute path.
func TestComputer_NpmDependencyAdded(t *testing.T) {
	t.Parallel()
	fs := depsFakeStore(t, "pkg:npm/example", "npm_dependencies", "npm-registry",
		[]string{"express", "lodash"},
		[]string{"express", "left-pad", "lodash"})
	assertDependencyAdded(t, fs, "pkg:npm/example", "npm_dependencies", 2, 3, "left-pad")
}

// TestComputer_CargoDependencyAdded: same proof for the cargo signal,
// confirming the byte-identical value shape flows through Compute
// identically across ecosystems.
func TestComputer_CargoDependencyAdded(t *testing.T) {
	t.Parallel()
	fs := depsFakeStore(t, "pkg:cargo/example", "cargo_dependencies", "cargo-registry",
		[]string{"libc", "mio"},
		[]string{"libc", "mio", "tokio-macros"})
	assertDependencyAdded(t, fs, "pkg:cargo/example", "cargo_dependencies", 2, 3, "tokio-macros")
}

// TestComputer_MavenDependencyAdded: same proof for the maven signal.
// direct entries are groupId:artifactId coordinates; the byte-
// identical value shape flows through Compute identically.
func TestComputer_MavenDependencyAdded(t *testing.T) {
	t.Parallel()
	fs := depsFakeStore(t, "pkg:maven/com.example/thing", "maven_dependencies", "maven-registry",
		[]string{"com.google.guava:guava", "org.slf4j:slf4j-api"},
		[]string{"com.google.guava:guava", "com.h2database:h2", "org.slf4j:slf4j-api"})
	assertDependencyAdded(t, fs, "pkg:maven/com.example/thing", "maven_dependencies", 2, 3, "com.h2database:h2")
}

// TestComputer_GemDependencyAdded: same proof for the gem signal,
// confirming the byte-identical value shape flows through Compute
// identically for the fifth ecosystem.
func TestComputer_GemDependencyAdded(t *testing.T) {
	t.Parallel()
	fs := depsFakeStore(t, "pkg:gem/example", "gem_dependencies", "gem-registry",
		[]string{"actionpack", "activesupport"},
		[]string{"actionpack", "activesupport", "railties"})
	assertDependencyAdded(t, fs, "pkg:gem/example", "gem_dependencies", 2, 3, "railties")
}

// TestComputer_PyPIDependencyAdded: same proof for the pypi signal,
// confirming the byte-identical value shape flows through Compute
// identically for the sixth ecosystem. Entries are PEP 503-normalized
// dependency names.
func TestComputer_PyPIDependencyAdded(t *testing.T) {
	t.Parallel()
	fs := depsFakeStore(t, "pkg:pypi/example", "pypi_dependencies", "pypi-registry",
		[]string{"certifi", "urllib3"},
		[]string{"certifi", "charset-normalizer", "urllib3"})
	assertDependencyAdded(t, fs, "pkg:pypi/example", "pypi_dependencies", 2, 3, "charset-normalizer")
}

// TestComputer_GoDependencyAdded: parity proof for the pre-existing
// go signal. Now that parseGoModDeps sorts+dedupes its direct list,
// go_dependencies flows through Compute through the same set-diff
// path as the five registry ecosystems. Entries are module paths.
func TestComputer_GoDependencyAdded(t *testing.T) {
	t.Parallel()
	fs := depsFakeStore(t, "repo:github/example/proj", "go_dependencies", "github",
		[]string{"github.com/pkg/errors", "github.com/stretchr/testify"},
		[]string{"github.com/pkg/errors", "github.com/spf13/cobra", "github.com/stretchr/testify"})
	assertDependencyAdded(t, fs, "repo:github/example/proj", "go_dependencies", 2, 3, "github.com/spf13/cobra")
}

// errFakeStore returns its configured error from every method.
type errFakeStore struct {
	err error
}

func (e *errFakeStore) FindEntityByURI(_ context.Context, _ string) (*profile.Entity, error) {
	return nil, e.err
}
func (e *errFakeStore) GetSignals(_ context.Context, _ string) ([]profile.Signal, error) {
	return nil, e.err
}
