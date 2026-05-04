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
	assert.Contains(t, err.Error(), "package.json",
		"error should mention npm alongside go")
	assert.Contains(t, err.Error(), "pyproject.toml",
		"error should mention pyproject.toml now that Detect recognizes Python projects")
}

func TestDetect_FindsPyProjectTOML(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	pyProjectPath := filepath.Join(dir, "pyproject.toml")
	require.NoError(t, os.WriteFile(pyProjectPath, []byte("[project]\nname = \"x\"\n"), 0o600))

	path, ecosystem, err := Detect(dir)
	require.NoError(t, err)
	assert.Equal(t, "pypi", ecosystem)
	assert.Equal(t, "pyproject.toml", filepath.Base(path))
}

func TestDetect_FindsSetupPy(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	setupPyPath := filepath.Join(dir, "setup.py")
	require.NoError(t, os.WriteFile(setupPyPath, []byte("from setuptools import setup\nsetup(name='x')\n"), 0o600))

	path, ecosystem, err := Detect(dir)
	require.NoError(t, err)
	assert.Equal(t, "pypi", ecosystem)
	assert.Equal(t, "setup.py", filepath.Base(path))
}

func TestDetect_FindsRequirementsTxt(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	reqPath := filepath.Join(dir, "requirements.txt")
	require.NoError(t, os.WriteFile(reqPath, []byte("requests==2.31.0\n"), 0o600))

	path, ecosystem, err := Detect(dir)
	require.NoError(t, err)
	assert.Equal(t, "pypi", ecosystem)
	assert.Equal(t, "requirements.txt", filepath.Base(path))
}

// TestDetect_PyProjectTOMLWinsOverSetupPy documents that within the
// PyPI ecosystem, the modern PEP 621 manifest is preferred over the
// legacy setup.py when both exist. This isn't an arbitrary tie-break:
// pyproject.toml carries declarative metadata that's safe to parse,
// whereas setup.py is Python source that can't be read deterministically
// without execution.
func TestDetect_PyProjectTOMLWinsOverSetupPy(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte("[project]\nname = \"x\"\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "setup.py"), []byte("setup()\n"), 0o600))

	path, ecosystem, err := Detect(dir)
	require.NoError(t, err)
	assert.Equal(t, "pypi", ecosystem)
	assert.Equal(t, "pyproject.toml", filepath.Base(path),
		"pyproject.toml is the modern PEP 621 source of truth and must win over legacy setup.py")
}

// TestDetect_PyProjectTOMLWinsOverRequirementsTxt documents that a
// project manifest beats a deps-only file when both are present.
// requirements.txt carries no project identity and is often a
// derived artifact (pip-compile output, dev-only); falling through
// to it when pyproject.toml exists would be a regression.
func TestDetect_PyProjectTOMLWinsOverRequirementsTxt(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte("[project]\nname = \"x\"\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "requirements.txt"), []byte("requests\n"), 0o600))

	path, ecosystem, err := Detect(dir)
	require.NoError(t, err)
	assert.Equal(t, "pypi", ecosystem)
	assert.Equal(t, "pyproject.toml", filepath.Base(path),
		"a project manifest must win over a deps-only requirements.txt")
}

func TestDetect_FindsPackageJSON(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	pkgJSONPath := filepath.Join(dir, "package.json")
	require.NoError(t, os.WriteFile(pkgJSONPath, []byte(`{"name":"x"}`), 0o600))

	path, ecosystem, err := Detect(dir)
	require.NoError(t, err)
	assert.Equal(t, "npm", ecosystem)
	assert.Equal(t, "package.json", filepath.Base(path))
}

// TestDetect_GoModWinsInPolyglotRoot documents the first-match-wins
// behavior for projects that contain BOTH manifests in the root.
// v0.1 treats that as a rare-enough edge case to handle implicitly
// via the candidates-list order (go.mod first); callers with real
// polyglot projects can pass --manifest explicitly.
func TestDetect_GoModWinsInPolyglotRoot(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"x"}`), 0o600))

	_, ecosystem, err := Detect(dir)
	require.NoError(t, err)
	assert.Equal(t, "go", ecosystem,
		"go.mod is earlier in the candidates list and wins over package.json in polyglot roots")
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

func TestDetect_FindsPomXML(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	pomPath := filepath.Join(dir, "pom.xml")
	require.NoError(t, os.WriteFile(pomPath, []byte(`<?xml version="1.0"?>
<project><groupId>com.example</groupId><artifactId>x</artifactId></project>`), 0o600))

	path, ecosystem, err := Detect(dir)
	require.NoError(t, err)
	assert.Equal(t, "maven", ecosystem)
	assert.Equal(t, "pom.xml", filepath.Base(path))
}

func TestDetect_NonexistentDir(t *testing.T) {
	t.Parallel()

	_, _, err := Detect("/absolutely/does/not/exist/anywhere")
	require.Error(t, err)
}
