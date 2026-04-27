package pypi

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/manifest"
)

// writePyProject writes a pyproject.toml with the given content
// in a fresh temp dir and returns its absolute path. Mirrors
// writeRequirements from the requirements.txt tests so the same
// pattern applies across both formats.
func writePyProject(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "pyproject.toml")
	require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
	return p
}

func TestParsePyProject_PEP621_Basic(t *testing.T) {
	t.Parallel()
	path := writePyProject(t, `[project]
name = "example-pkg"
version = "1.0.0"
requires-python = ">=3.10"
dependencies = [
    "requests==2.31.0",
    "click>=8.0",
]
`)
	info, deps, err := parsePyProject(path)
	require.NoError(t, err)

	assert.Equal(t, "example-pkg", info.Name)
	assert.Equal(t, ">=3.10", info.EcoVersion)
	assert.Equal(t, "pypi", info.Ecosystem)
	assert.True(t, filepath.IsAbs(info.ManifestPath))

	require.Len(t, deps, 2)
	for _, d := range deps {
		assert.True(t, d.Direct, "every PEP 621 dep is Direct")
		assert.Equal(t, "pypi", d.Ecosystem)
	}
	names := []string{deps[0].Name, deps[1].Name}
	assert.ElementsMatch(t, []string{"requests", "click"}, names)
}

func TestParsePyProject_PEP621_OptionalDependencies(t *testing.T) {
	t.Parallel()
	path := writePyProject(t, `[project]
name = "example-pkg"
dependencies = ["requests==2.31.0"]

[project.optional-dependencies]
dev = ["pytest>=8.0"]
test = ["pytest>=8.0", "coverage"]
docs = ["sphinx>=7.0"]
`)
	_, deps, err := parsePyProject(path)
	require.NoError(t, err)

	// All groups flatten together. dev's pytest and test's pytest
	// both surface — no cross-group dedup (each is a separate
	// declaration; survey's downstream layers can dedupe).
	require.Len(t, deps, 5)
	names := make([]string, len(deps))
	for i, d := range deps {
		names[i] = d.Name
		assert.True(t, d.Direct, "optional-dependency is still Direct from the project's perspective")
	}
	assert.ElementsMatch(t,
		[]string{"requests", "pytest", "pytest", "coverage", "sphinx"},
		names,
	)
}

func TestParsePyProject_PEP621_ExtrasAndPEP503Normalization(t *testing.T) {
	t.Parallel()
	path := writePyProject(t, `[project]
name = "example-pkg"
dependencies = [
    "requests[security]==2.31.0",
    "Python-Dotenv==1.0.0",
]
`)
	_, deps, err := parsePyProject(path)
	require.NoError(t, err)
	require.Len(t, deps, 2)

	byCanonical := make(map[string]manifest.Dep, len(deps))
	for _, d := range deps {
		byCanonical[d.CanonicalURI] = d
	}

	require.Contains(t, byCanonical, "pkg:pypi/requests")
	require.Contains(t, byCanonical, "pkg:pypi/python-dotenv")

	assert.Equal(t, "requests[security]", byCanonical["pkg:pypi/requests"].Name,
		"extras preserved in Name; stripped from CanonicalURI")
	assert.Equal(t, "Python-Dotenv", byCanonical["pkg:pypi/python-dotenv"].Name,
		"verbatim casing preserved in Name; PEP 503 normalization applied to CanonicalURI")
}

func TestParsePyProject_PEP621_EnvironmentMarkerStripped(t *testing.T) {
	t.Parallel()
	path := writePyProject(t, `[project]
name = "example-pkg"
dependencies = [
    'pywin32==306 ; sys_platform == "win32"',
]
`)
	_, deps, err := parsePyProject(path)
	require.NoError(t, err)
	require.Len(t, deps, 1)
	assert.Equal(t, "pywin32", deps[0].Name)
	assert.Equal(t, "==306", deps[0].Version,
		"environment marker stripped, version preserved")
}

func TestParsePyProject_PEP621_EmptyDependencies(t *testing.T) {
	t.Parallel()
	path := writePyProject(t, `[project]
name = "example-pkg"
dependencies = []
`)
	info, deps, err := parsePyProject(path)
	require.NoError(t, err)
	assert.Equal(t, "example-pkg", info.Name)
	assert.Empty(t, deps)
}

func TestParsePyProject_NoModernFormat(t *testing.T) {
	t.Parallel()
	// pyproject.toml with only [build-system] — neither PEP 621
	// [project] nor PEP 735 [dependency-groups] present. Returns
	// the package-private sentinel that signals fallthrough to
	// Commit 6's Poetry parser.
	path := writePyProject(t, `[build-system]
requires = ["setuptools>=61.0"]
build-backend = "setuptools.build_meta"
`)
	_, _, err := parsePyProject(path)
	require.Error(t, err)
	assert.True(t, errors.Is(err, errNoModernFormat),
		"must surface the package-private fallthrough sentinel, not a different error")
}

func TestParsePyProject_FileTooLarge(t *testing.T) {
	t.Parallel()
	// File at maxPyProjectBytes + 1 must reject before decoding —
	// the cap is the front-line defense against malicious input
	// per the BurntSushi/toml synthesis recommendation.
	dir := t.TempDir()
	path := filepath.Join(dir, "pyproject.toml")

	// Build content that exceeds the cap. Padding with comments
	// keeps it valid TOML in case some future cap-handling path
	// tries to decode anyway.
	padding := make([]byte, maxPyProjectBytes+1)
	for i := range padding {
		padding[i] = '#'
	}
	require.NoError(t, os.WriteFile(path, padding, 0o600))

	_, _, err := parsePyProject(path)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrFileTooLarge,
		"size-cap error must use the package's exported sentinel; mirrors requirements.go")
}

func TestParsePyProject_AtSizeCap(t *testing.T) {
	t.Parallel()
	// File at exactly maxPyProjectBytes must parse normally — the
	// cap rejects strictly above, not at. This locks in the
	// boundary so a future fencepost slip (>= vs >) cannot slip
	// through undetected. Companion to TestParsePyProject_FileTooLarge.
	dir := t.TempDir()
	path := filepath.Join(dir, "pyproject.toml")

	header := []byte("[project]\nname = \"atcap\"\n")
	// Pad with '#' bytes — the run after the final newline is
	// treated as one EOF-terminated comment by the TOML parser.
	pad := make([]byte, maxPyProjectBytes-len(header))
	for i := range pad {
		pad[i] = '#'
	}
	content := append(header, pad...) //nolint:gocritic // intentional fresh slice; do not extend `header`
	require.Equal(t, maxPyProjectBytes, len(content),
		"test setup: content must be exactly at cap")
	require.NoError(t, os.WriteFile(path, content, 0o600))

	info, _, err := parsePyProject(path)
	require.NoError(t, err, "file at exact cap must parse, not be rejected")
	assert.Equal(t, "atcap", info.Name)
}

func TestParsePyProject_FileNotFound(t *testing.T) {
	t.Parallel()
	_, _, err := parsePyProject("/does/not/exist/pyproject.toml")
	require.Error(t, err)
}

func TestParsePyProject_MalformedTOML(t *testing.T) {
	t.Parallel()
	path := writePyProject(t, `[project
name = "broken — no closing bracket on the table header"
`)
	_, _, err := parsePyProject(path)
	require.Error(t, err)
}

func TestParsePyProject_PEP621_DependenciesArrayWithComments(t *testing.T) {
	// Real-world pyproject.toml files frequently have inline
	// comments inside the dependencies array. BurntSushi/toml
	// handles them at the TOML layer; the strings reaching us
	// are clean. This test pins the behavior.
	t.Parallel()
	path := writePyProject(t, `[project]
name = "example-pkg"
dependencies = [
    "requests==2.31.0",  # the latest stable
    # placeholder for click — pinned tightly to avoid CLI surprises
    "click==8.1.0",
]
`)
	_, deps, err := parsePyProject(path)
	require.NoError(t, err)
	require.Len(t, deps, 2)
}
