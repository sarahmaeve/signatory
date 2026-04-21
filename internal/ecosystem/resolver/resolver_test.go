package resolver

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubResolver is a test-only implementation used to exercise the
// registry's dispatch behavior without touching the network.
type stubResolver struct {
	source DeclaredSource
	err    error
	calls  int
}

func (s *stubResolver) ResolveSource(_ context.Context, _ string) (DeclaredSource, error) {
	s.calls++
	if s.err != nil {
		return DeclaredSource{}, s.err
	}
	return s.source, nil
}

// TestRegistry_Resolve_Dispatches covers the happy path: a registered
// resolver is called and its result is returned verbatim.
func TestRegistry_Resolve_Dispatches(t *testing.T) {
	t.Parallel()

	stub := &stubResolver{source: DeclaredSource{
		URI: "repo:github/foo/bar",
		URL: "https://github.com/foo/bar",
	}}
	r := NewRegistry()
	r.Register("example", stub)

	got, err := r.Resolve(context.Background(), "example", "some-pkg")
	require.NoError(t, err)
	assert.Equal(t, 1, stub.calls)
	assert.Equal(t, "repo:github/foo/bar", got.URI)
}

// TestRegistry_Resolve_UnknownEcosystemReturnsErrNoResolver verifies
// the signal callers branch on when an ecosystem isn't yet supported.
func TestRegistry_Resolve_UnknownEcosystemReturnsErrNoResolver(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	_, err := r.Resolve(context.Background(), "unsupported", "anything")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNoResolver)
	assert.Contains(t, err.Error(), `"unsupported"`,
		"error must name the missing ecosystem so the caller can report it precisely")
}

// TestRegistry_Resolve_EmptyRegistryReturnsErrNoResolver guards the
// zero-value case — an uninitialized registry doesn't panic.
func TestRegistry_Resolve_EmptyRegistryReturnsErrNoResolver(t *testing.T) {
	t.Parallel()

	var r Registry
	_, err := r.Resolve(context.Background(), "npm", "express")
	require.ErrorIs(t, err, ErrNoResolver)
}

// TestRegistry_Resolve_PropagatesResolverError verifies that a
// resolver's error surfaces to the caller unwrapped — the registry
// doesn't add noise between the underlying failure and what the
// caller sees.
func TestRegistry_Resolve_PropagatesResolverError(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("registry unreachable")
	stub := &stubResolver{err: sentinel}
	r := NewRegistry()
	r.Register("example", stub)

	_, err := r.Resolve(context.Background(), "example", "x")
	require.Error(t, err)
	assert.ErrorIs(t, err, sentinel)
}

// TestRegistry_Register_Overwrites verifies that a second Register
// call for the same ecosystem replaces the prior resolver — this is
// how tests swap a stubbed resolver for a production one without
// creating a new registry instance.
func TestRegistry_Register_Overwrites(t *testing.T) {
	t.Parallel()

	first := &stubResolver{source: DeclaredSource{URI: "first"}}
	second := &stubResolver{source: DeclaredSource{URI: "second"}}
	r := NewRegistry()
	r.Register("example", first)
	r.Register("example", second)

	got, err := r.Resolve(context.Background(), "example", "x")
	require.NoError(t, err)
	assert.Equal(t, "second", got.URI)
	assert.Zero(t, first.calls, "the replaced resolver must not be called")
}

// TestRegistry_Ecosystems_SortedReturnsRegisteredKeys verifies the
// help-diagnostic surface.
func TestRegistry_Ecosystems_SortedReturnsRegisteredKeys(t *testing.T) {
	t.Parallel()

	r := NewRegistry()
	r.Register("npm", &stubResolver{})
	r.Register("go", &stubResolver{})
	r.Register("pypi", &stubResolver{})

	assert.Equal(t, []string{"go", "npm", "pypi"}, r.Ecosystems())
}

// TestDefault_HasShippedResolvers verifies that the package-level
// Default registry is non-empty by the time callers see it — the
// sibling init() functions registered at least npm and go.
func TestDefault_HasShippedResolvers(t *testing.T) {
	t.Parallel()

	got := Default.Ecosystems()
	assert.Contains(t, got, "npm")
	assert.Contains(t, got, "go")
}
