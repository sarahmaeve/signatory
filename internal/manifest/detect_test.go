package manifest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDetect_FindsGoMod(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	goModPath := filepath.Join(dir, "go.mod")
	require.NoError(t, os.WriteFile(goModPath, []byte("module example\n"), 0o600))

	path, ecosystem, err := Detect(dir)
	require.NoError(t, err)
	assert.Equal(t, "go", ecosystem)
	// filepath.EvalSymlinks may normalize paths on macOS (/tmp →
	// /private/tmp); compare using filepath.Base to stay platform-
	// portable.
	assert.Equal(t, "go.mod", filepath.Base(path))
}

func TestDetect_NoManifest(t *testing.T) {
	t.Parallel()

	dir := t.TempDir() // empty
	path, ecosystem, err := Detect(dir)
	require.Error(t, err)
	assert.Empty(t, path)
	assert.Empty(t, ecosystem)
	assert.Contains(t, err.Error(), "no recognized manifest")
	assert.Contains(t, err.Error(), "go.mod",
		"error should list candidate filenames so users know what to add")
}

func TestDetect_DirInsteadOfFile(t *testing.T) {
	// If something weird is at the candidate path — say, a directory
	// named "go.mod" — Detect must not match it. Guards against a
	// future developer who types `mkdir go.mod` by accident.
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "go.mod"), 0o755))

	_, _, err := Detect(dir)
	require.Error(t, err, "a directory named go.mod must not count as a manifest")
}

func TestDetect_NonexistentDir(t *testing.T) {
	t.Parallel()

	_, _, err := Detect("/absolutely/does/not/exist/anywhere")
	require.Error(t, err)
}
