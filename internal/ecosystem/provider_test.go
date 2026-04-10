package ecosystem

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
