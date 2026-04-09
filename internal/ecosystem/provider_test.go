package ecosystem

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockProvider is a test double for the Provider interface.
type mockProvider struct {
	name string

	manifestPath  string
	manifestFound bool

	deps     []Dependency
	parseErr error

	repoURL    string
	resolveErr error

	// Track calls for verification.
	detectCalled  bool
	parseCalled   bool
	resolveCalled bool
}

func (m *mockProvider) Name() string { return m.name }

func (m *mockProvider) DetectManifest(dir string) (string, bool) {
	m.detectCalled = true
	return m.manifestPath, m.manifestFound
}

func (m *mockProvider) ParseManifest(path string) ([]Dependency, error) {
	m.parseCalled = true
	if m.parseErr != nil {
		return nil, m.parseErr
	}
	return m.deps, nil
}

func (m *mockProvider) ResolveRepo(ctx context.Context, packageName string) (string, error) {
	m.resolveCalled = true
	if m.resolveErr != nil {
		return "", m.resolveErr
	}
	return m.repoURL, nil
}

// Compile-time interface check.
var _ Provider = (*mockProvider)(nil)

func TestProvider_MockSatisfiesInterface(t *testing.T) {
	t.Parallel()

	var p Provider = &mockProvider{name: "npm"}
	assert.Equal(t, "npm", p.Name())
}

func TestProvider_DetectManifest_Found(t *testing.T) {
	t.Parallel()

	p := &mockProvider{
		name:          "npm",
		manifestPath:  "/project/package.json",
		manifestFound: true,
	}

	path, found := p.DetectManifest("/project")
	assert.True(t, found)
	assert.Equal(t, "/project/package.json", path)
	assert.True(t, p.detectCalled)
}

func TestProvider_DetectManifest_NotFound(t *testing.T) {
	t.Parallel()

	p := &mockProvider{
		name:          "pypi",
		manifestPath:  "",
		manifestFound: false,
	}

	path, found := p.DetectManifest("/project")
	assert.False(t, found)
	assert.Empty(t, path)
}

func TestProvider_ParseManifest_Success(t *testing.T) {
	t.Parallel()

	deps := []Dependency{
		{Name: "lodash", Version: "4.17.21", Pinned: true, Direct: true, RepoURL: "https://github.com/lodash/lodash"},
		{Name: "express", Version: "^4.18.0", Pinned: false, Direct: true},
		{Name: "debug", Version: "4.3.4", Pinned: true, Direct: false},
	}

	p := &mockProvider{name: "npm", deps: deps}

	result, err := p.ParseManifest("/project/package.json")
	require.NoError(t, err)
	assert.Len(t, result, 3)
	assert.Equal(t, deps, result)
	assert.True(t, p.parseCalled)
}

func TestProvider_ParseManifest_Error(t *testing.T) {
	t.Parallel()

	p := &mockProvider{
		name:     "npm",
		parseErr: errors.New("malformed JSON in package.json"),
	}

	deps, err := p.ParseManifest("/project/package.json")
	assert.Error(t, err)
	assert.Nil(t, deps)
	assert.Contains(t, err.Error(), "malformed JSON")
}

func TestProvider_ParseManifest_EmptyDeps(t *testing.T) {
	t.Parallel()

	p := &mockProvider{name: "npm", deps: []Dependency{}}

	deps, err := p.ParseManifest("/project/package.json")
	require.NoError(t, err)
	assert.NotNil(t, deps)
	assert.Empty(t, deps)
}

func TestProvider_ResolveRepo_Success(t *testing.T) {
	t.Parallel()

	p := &mockProvider{
		name:    "npm",
		repoURL: "https://github.com/lodash/lodash",
	}

	url, err := p.ResolveRepo(context.Background(), "lodash")
	require.NoError(t, err)
	assert.Equal(t, "https://github.com/lodash/lodash", url)
	assert.True(t, p.resolveCalled)
}

func TestProvider_ResolveRepo_Error(t *testing.T) {
	t.Parallel()

	p := &mockProvider{
		name:       "npm",
		resolveErr: errors.New("package not found"),
	}

	url, err := p.ResolveRepo(context.Background(), "nonexistent-pkg")
	assert.Error(t, err)
	assert.Empty(t, url)
}

func TestProvider_ResolveRepo_ContextCancellation(t *testing.T) {
	t.Parallel()

	// A context-aware mock to verify implementations should respect cancellation.
	ctxProvider := &contextAwareProvider{name: "ctx-npm"}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := ctxProvider.ResolveRepo(ctx, "any-package")
	assert.ErrorIs(t, err, context.Canceled)
}

// contextAwareProvider is a mock that respects context cancellation.
type contextAwareProvider struct {
	name string
}

func (c *contextAwareProvider) Name() string { return c.name }

func (c *contextAwareProvider) DetectManifest(dir string) (string, bool) { return "", false }

func (c *contextAwareProvider) ParseManifest(path string) ([]Dependency, error) { return nil, nil }

func (c *contextAwareProvider) ResolveRepo(ctx context.Context, packageName string) (string, error) {
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	default:
		return "", nil
	}
}

var _ Provider = (*contextAwareProvider)(nil)

func TestDependencyJSONRoundTrip(t *testing.T) {
	t.Parallel()

	dep := Dependency{
		Name:    "express",
		Version: "4.18.2",
		Pinned:  true,
		Direct:  true,
		RepoURL: "https://github.com/expressjs/express",
	}

	data, err := json.Marshal(dep)
	require.NoError(t, err)

	var decoded Dependency
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)

	assert.Equal(t, dep, decoded)
}

func TestDependencyJSON_OmitsEmptyRepoURL(t *testing.T) {
	t.Parallel()

	dep := Dependency{
		Name:    "debug",
		Version: "4.3.4",
		Pinned:  true,
		Direct:  false,
	}

	data, err := json.Marshal(dep)
	require.NoError(t, err)

	var raw map[string]json.RawMessage
	err = json.Unmarshal(data, &raw)
	require.NoError(t, err)

	_, hasRepoURL := raw["repo_url"]
	assert.False(t, hasRepoURL, "empty repo_url should be omitted from JSON")
}

func TestDependencyJSON_BooleanFields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		dep    Dependency
		pinned bool
		direct bool
	}{
		{
			name:   "PinnedDirect",
			dep:    Dependency{Name: "a", Pinned: true, Direct: true},
			pinned: true,
			direct: true,
		},
		{
			name:   "UnpinnedTransitive",
			dep:    Dependency{Name: "b", Pinned: false, Direct: false},
			pinned: false,
			direct: false,
		},
		{
			name:   "PinnedTransitive",
			dep:    Dependency{Name: "c", Pinned: true, Direct: false},
			pinned: true,
			direct: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			data, err := json.Marshal(tc.dep)
			require.NoError(t, err)

			var decoded Dependency
			err = json.Unmarshal(data, &decoded)
			require.NoError(t, err)

			assert.Equal(t, tc.pinned, decoded.Pinned)
			assert.Equal(t, tc.direct, decoded.Direct)
		})
	}
}

func TestDependencyJSON_ZeroValue(t *testing.T) {
	t.Parallel()

	var dep Dependency
	data, err := json.Marshal(dep)
	require.NoError(t, err)

	var decoded Dependency
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)

	assert.Equal(t, dep, decoded)
}
