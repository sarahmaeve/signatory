package pypi

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParse_DispatchesRequirementsTxt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "requirements.txt")
	require.NoError(t, os.WriteFile(path, []byte("requests==2.31.0\nclick==8.0.0\n"), 0o600))

	info, deps, err := Parse(path)
	require.NoError(t, err)

	// ProjectInfo: ManifestPath absolute, Ecosystem="pypi", Name and
	// EcoVersion empty (requirements.txt has no project identity).
	absPath, absErr := filepath.Abs(path)
	require.NoError(t, absErr)
	assert.Equal(t, absPath, info.ManifestPath, "ManifestPath must be absolute")
	assert.Equal(t, "pypi", info.Ecosystem)
	assert.Empty(t, info.Name, "requirements.txt declares no project name")
	assert.Empty(t, info.EcoVersion, "requirements.txt declares no toolchain version")

	// Deps surface verbatim from ParseRequirements — sanity-check
	// that routing works and we get the same shape as the underlying
	// parser, not that we re-test ParseRequirements's full behavior.
	require.Len(t, deps, 2)
	names := []string{deps[0].Name, deps[1].Name}
	assert.Contains(t, names, "requests")
	assert.Contains(t, names, "click")
}

func TestParse_PyProjectTOMLReturnsNotYetSupported(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "pyproject.toml")
	require.NoError(t, os.WriteFile(path, []byte("[project]\nname = \"x\"\n"), 0o600))

	info, deps, err := Parse(path)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPyProjectTOMLNotYetSupported,
		"caller-facing sentinel lets survey render a clear message")
	assert.Empty(t, info.ManifestPath, "no partial ProjectInfo on error")
	assert.Empty(t, deps, "no partial deps on error")
}

func TestParse_SetupPyReturnsNotParseable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "setup.py")
	require.NoError(t, os.WriteFile(path, []byte("from setuptools import setup\nsetup(name='x')\n"), 0o600))

	_, _, err := Parse(path)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrSetupPyNotParseable)
	assert.Contains(t, err.Error(), "pyproject.toml",
		"error message should redirect the user to a parseable alternative")
	assert.Contains(t, err.Error(), "requirements.txt")
}

func TestParse_UnrecognizedFilename(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "Pipfile")
	require.NoError(t, os.WriteFile(path, []byte("[packages]\n"), 0o600))

	_, _, err := Parse(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Pipfile",
		"error should name the unrecognized file so the user can correct it")
}

func TestParse_PropagatesParseRequirementsError(t *testing.T) {
	// When ParseRequirements fails (e.g., a malicious -r), Parse
	// must surface the same error type — survey relies on the
	// sentinel to render a useful message.
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "requirements.txt")
	require.NoError(t, os.WriteFile(path, []byte("-r /etc/passwd\n"), 0o600))

	_, _, err := Parse(path)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrIncludeOutOfScope)
}

func TestParse_RequirementsTxtAtRelativePath(t *testing.T) {
	// Caller may pass a relative path; ManifestPath must still be
	// absolute. This protects callers that use ManifestPath as a
	// store key — relative-path drift across cwd would fragment.
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "requirements.txt"), []byte("requests\n"), 0o600))

	wd, err := os.Getwd()
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.Chdir(wd) })
	require.NoError(t, os.Chdir(dir))

	info, _, err := Parse("requirements.txt")
	require.NoError(t, err)
	assert.True(t, filepath.IsAbs(info.ManifestPath),
		"ManifestPath must be absolute even when caller passes a relative path")
}
