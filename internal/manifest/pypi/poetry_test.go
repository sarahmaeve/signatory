package pypi

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- Value-shape tests ----------------------------------------------------

// TestParsePyProject_Poetry_StringValueDep exercises Poetry's
// most common dep form: bare-string version spec.
func TestParsePyProject_Poetry_StringValueDep(t *testing.T) {
	t.Parallel()
	path := writePyProject(t, `[tool.poetry]
name = "x"

[tool.poetry.dependencies]
python = "^3.10"
requests = "^2.31.0"
`)
	_, deps, err := parsePyProject(path)
	require.NoError(t, err)
	require.Len(t, deps, 1, "python is the runtime pin, not a dep; only requests surfaces")

	d := deps[0]
	assert.Equal(t, "requests", d.Name)
	assert.Equal(t, "^2.31.0", d.Version,
		"Poetry's caret syntax preserved verbatim; no PEP 440 translation")
	assert.Equal(t, "pkg:pypi/requests", d.CanonicalURI)
	assert.Equal(t, "pypi", d.Ecosystem)
	assert.True(t, d.Direct)
}

// TestParsePyProject_Poetry_TableValueWithVersion covers the
// extended form where the value is a table containing a version key.
func TestParsePyProject_Poetry_TableValueWithVersion(t *testing.T) {
	t.Parallel()
	path := writePyProject(t, `[tool.poetry]
name = "x"

[tool.poetry.dependencies]
requests = { version = "^2.31.0" }
`)
	_, deps, err := parsePyProject(path)
	require.NoError(t, err)
	require.Len(t, deps, 1)
	assert.Equal(t, "requests", deps[0].Name)
	assert.Equal(t, "^2.31.0", deps[0].Version)
	assert.Equal(t, "pkg:pypi/requests", deps[0].CanonicalURI)
}

// TestParsePyProject_Poetry_TableValueWithExtras covers
// `requests = { version = "^2.31.0", extras = ["security"] }`.
// Extras are encoded into Name (matching the requirements.txt
// convention `requests[security]`) and stripped from CanonicalURI.
func TestParsePyProject_Poetry_TableValueWithExtras(t *testing.T) {
	t.Parallel()
	path := writePyProject(t, `[tool.poetry]
name = "x"

[tool.poetry.dependencies]
requests = { version = "^2.31.0", extras = ["security", "socks"] }
`)
	_, deps, err := parsePyProject(path)
	require.NoError(t, err)
	require.Len(t, deps, 1)
	assert.Equal(t, "requests[security,socks]", deps[0].Name,
		"extras encoded into Name to match requirements.txt convention")
	assert.Equal(t, "^2.31.0", deps[0].Version)
	assert.Equal(t, "pkg:pypi/requests", deps[0].CanonicalURI,
		"extras stripped from CanonicalURI; package identity is the bare name")
}

// TestParsePyProject_Poetry_GitDependency: VCS source → pypi-local.
func TestParsePyProject_Poetry_GitDependency(t *testing.T) {
	t.Parallel()
	path := writePyProject(t, `[tool.poetry]
name = "x"

[tool.poetry.dependencies]
mylib = { git = "https://github.com/foo/bar.git", branch = "main" }
`)
	_, deps, err := parsePyProject(path)
	require.NoError(t, err)
	require.Len(t, deps, 1)
	assert.Equal(t, "pypi-local", deps[0].Ecosystem)
	assert.Empty(t, deps[0].CanonicalURI,
		"non-registry source: no canonical URI to stamp into store")
	assert.Equal(t, "mylib", deps[0].Name,
		"key preserved as Name so operator can identify the dep")
}

// TestParsePyProject_Poetry_PathDependency: local path → pypi-local.
func TestParsePyProject_Poetry_PathDependency(t *testing.T) {
	t.Parallel()
	path := writePyProject(t, `[tool.poetry]
name = "x"

[tool.poetry.dependencies]
mylib = { path = "../mylib" }
`)
	_, deps, err := parsePyProject(path)
	require.NoError(t, err)
	require.Len(t, deps, 1)
	assert.Equal(t, "pypi-local", deps[0].Ecosystem)
	assert.Empty(t, deps[0].CanonicalURI)
}

// TestParsePyProject_Poetry_URLDependency: explicit URL → pypi-local.
func TestParsePyProject_Poetry_URLDependency(t *testing.T) {
	t.Parallel()
	path := writePyProject(t, `[tool.poetry]
name = "x"

[tool.poetry.dependencies]
mylib = { url = "https://example.com/mylib-1.0.tar.gz" }
`)
	_, deps, err := parsePyProject(path)
	require.NoError(t, err)
	require.Len(t, deps, 1)
	assert.Equal(t, "pypi-local", deps[0].Ecosystem)
	assert.Empty(t, deps[0].CanonicalURI)
}

// TestParsePyProject_Poetry_FiltersPythonRuntimePin documents
// that python in [tool.poetry.dependencies] populates EcoVersion
// (when PEP 621 absent) and is NEVER a Dep, regardless of value shape.
func TestParsePyProject_Poetry_FiltersPythonRuntimePin(t *testing.T) {
	t.Parallel()

	// String value form.
	t.Run("string value", func(t *testing.T) {
		t.Parallel()
		path := writePyProject(t, `[tool.poetry]
name = "x"

[tool.poetry.dependencies]
python = "^3.10"
`)
		info, deps, err := parsePyProject(path)
		require.NoError(t, err)
		assert.Empty(t, deps, "python is never a dep")
		assert.Equal(t, "^3.10", info.EcoVersion,
			"python populates EcoVersion when PEP 621 absent")
	})

	// Table value form.
	t.Run("table value", func(t *testing.T) {
		t.Parallel()
		path := writePyProject(t, `[tool.poetry]
name = "x"

[tool.poetry.dependencies]
python = { version = "^3.10" }
`)
		info, deps, err := parsePyProject(path)
		require.NoError(t, err)
		assert.Empty(t, deps)
		assert.Equal(t, "^3.10", info.EcoVersion,
			"python in table form also extracts to EcoVersion")
	})
}

// --- Location tests -------------------------------------------------------

// TestParsePyProject_Poetry_LegacyDevDependencies covers the
// pre-Poetry-1.2 dev-deps shape that Textualize/rich uses.
func TestParsePyProject_Poetry_LegacyDevDependencies(t *testing.T) {
	t.Parallel()
	path := writePyProject(t, `[tool.poetry]
name = "x"

[tool.poetry.dependencies]
python = "^3.10"
requests = "^2.31.0"

[tool.poetry.dev-dependencies]
pytest = "^8.0"
black = "^24.0"
`)
	_, deps, err := parsePyProject(path)
	require.NoError(t, err)
	require.Len(t, deps, 3, "1 main + 2 dev (legacy form); python excluded")

	names := []string{deps[0].Name, deps[1].Name, deps[2].Name}
	assert.ElementsMatch(t, []string{"requests", "pytest", "black"}, names)

	for _, d := range deps {
		assert.True(t, d.Direct, "every Poetry dep is Direct")
	}
}

// TestParsePyProject_Poetry_ModernGroupDependencies covers the
// Poetry-1.2+ group syntax that Poetry-the-repo uses.
func TestParsePyProject_Poetry_ModernGroupDependencies(t *testing.T) {
	t.Parallel()
	path := writePyProject(t, `[tool.poetry]
name = "x"

[tool.poetry.dependencies]
python = "^3.10"

[tool.poetry.group.dev.dependencies]
pytest = "^8.0"

[tool.poetry.group.test.dependencies]
coverage = "^7.0"
`)
	_, deps, err := parsePyProject(path)
	require.NoError(t, err)
	require.Len(t, deps, 2, "groups: 1 dev + 1 test; main has only python pin")

	names := []string{deps[0].Name, deps[1].Name}
	assert.ElementsMatch(t, []string{"pytest", "coverage"}, names)
}

// TestParsePyProject_Poetry_LegacyAndModernGroupsCoexist documents
// that a file CAN declare both [tool.poetry.dev-dependencies] and
// [tool.poetry.group.*.dependencies]. Both surface; no de-dup.
func TestParsePyProject_Poetry_LegacyAndModernGroupsCoexist(t *testing.T) {
	t.Parallel()
	path := writePyProject(t, `[tool.poetry]
name = "x"

[tool.poetry.dependencies]
python = "^3.10"

[tool.poetry.dev-dependencies]
pytest = "^8.0"

[tool.poetry.group.test.dependencies]
coverage = "^7.0"
`)
	_, deps, err := parsePyProject(path)
	require.NoError(t, err)
	require.Len(t, deps, 2)
	names := []string{deps[0].Name, deps[1].Name}
	assert.ElementsMatch(t, []string{"pytest", "coverage"}, names)
}

// --- Merge-rule tests -----------------------------------------------------

// TestParsePyProject_PEP621NameWinsOverPoetry asserts that when
// both [project].name and [tool.poetry].name are present, PEP 621's
// value flows into ProjectInfo.Name. PEP 621 is the standard;
// [tool.poetry] is the dialect.
func TestParsePyProject_PEP621NameWinsOverPoetry(t *testing.T) {
	t.Parallel()
	path := writePyProject(t, `[project]
name = "from-pep621"
dependencies = []

[tool.poetry]
name = "from-poetry"
`)
	info, _, err := parsePyProject(path)
	require.NoError(t, err)
	assert.Equal(t, "from-pep621", info.Name,
		"PEP 621 wins as the merge rule")
}

// TestParsePyProject_PoetryNameUsedWhenPEP621Absent asserts the
// fallback path: when [project] is absent, [tool.poetry].name
// populates ProjectInfo.Name.
func TestParsePyProject_PoetryNameUsedWhenPEP621Absent(t *testing.T) {
	t.Parallel()
	path := writePyProject(t, `[tool.poetry]
name = "from-poetry"
`)
	info, _, err := parsePyProject(path)
	require.NoError(t, err)
	assert.Equal(t, "from-poetry", info.Name,
		"when PEP 621 absent, Poetry's name fills the gap")
}

// TestParsePyProject_PEP621RequiresPythonWinsOverPoetryPython
// asserts the same merge rule for EcoVersion.
func TestParsePyProject_PEP621RequiresPythonWinsOverPoetryPython(t *testing.T) {
	t.Parallel()
	path := writePyProject(t, `[project]
name = "x"
requires-python = ">=3.11"

[tool.poetry.dependencies]
python = "^3.10"
`)
	info, _, err := parsePyProject(path)
	require.NoError(t, err)
	assert.Equal(t, ">=3.11", info.EcoVersion,
		"PEP 621 requires-python wins over Poetry's python pin")
}

// --- Three-shape integration tests against fixtures -----------------------

// TestParsePyProject_PoetryShape_PureLegacy uses the testdata
// fixture matching Textualize/rich's shape: pure Poetry,
// [tool.poetry.dev-dependencies] (legacy form).
func TestParsePyProject_PoetryShape_PureLegacy(t *testing.T) {
	t.Parallel()
	info, deps, err := parsePyProject("testdata/poetry-pure-legacy/pyproject.toml")
	require.NoError(t, err)

	assert.Equal(t, "pure-legacy-pkg", info.Name,
		"name from [tool.poetry] when PEP 621 absent")
	assert.Equal(t, "^3.10", info.EcoVersion,
		"EcoVersion from [tool.poetry.dependencies].python")
	assert.Equal(t, "pypi", info.Ecosystem)

	// Fixture's documented dep count.
	require.Len(t, deps, 4)
	for _, d := range deps {
		assert.True(t, d.Direct)
	}
}

// TestParsePyProject_PoetryShape_PureModern uses the testdata
// fixture for pure Poetry with [tool.poetry.group.*.dependencies]
// (modern groups, no PEP 621). No confirmed real-world target;
// fixture-only smoke coverage.
func TestParsePyProject_PoetryShape_PureModern(t *testing.T) {
	t.Parallel()
	info, deps, err := parsePyProject("testdata/poetry-pure-modern/pyproject.toml")
	require.NoError(t, err)

	assert.Equal(t, "pure-modern-pkg", info.Name)
	assert.Equal(t, "^3.10", info.EcoVersion)
	require.Len(t, deps, 4)
}

// TestParsePyProject_PoetryShape_Hybrid uses the testdata fixture
// matching Poetry-the-repo's shape: PEP 621 [project] for primary
// metadata, [tool.poetry.group.*.dependencies] for dev/test groups.
// This is the case that broke v1/v2's "fallback" framing.
func TestParsePyProject_PoetryShape_Hybrid(t *testing.T) {
	t.Parallel()
	info, deps, err := parsePyProject("testdata/poetry-hybrid/pyproject.toml")
	require.NoError(t, err)

	// PEP 621 wins for metadata.
	assert.Equal(t, "hybrid-pkg", info.Name)
	assert.Equal(t, ">=3.10", info.EcoVersion)

	// Deps from BOTH PEP 621 [project].dependencies AND
	// [tool.poetry.group.*.dependencies] surface.
	require.Len(t, deps, 4,
		"hybrid case: [project] main deps + Poetry group dev/test all surface; v1/v2 would have missed Poetry deps")
}

// --- All-three-tables coexistence test ------------------------------------

// TestParsePyProject_AllThreeTablesPresent asserts that a file
// declaring [project], [dependency-groups], AND [tool.poetry] gets
// deps from all three handlers, no de-dup. This is the maximalist
// case the v3 architecture exists to handle.
func TestParsePyProject_AllThreeTablesPresent(t *testing.T) {
	t.Parallel()
	path := writePyProject(t, `[project]
name = "max"
dependencies = ["requests==2.31.0"]

[dependency-groups]
dev = ["ruff"]

[tool.poetry.group.test.dependencies]
pytest = "^8.0"
`)
	info, deps, err := parsePyProject(path)
	require.NoError(t, err)

	assert.Equal(t, "max", info.Name)
	require.Len(t, deps, 3)
	names := make([]string, len(deps))
	for i, d := range deps {
		names[i] = d.Name
	}
	assert.ElementsMatch(t,
		[]string{"requests", "ruff", "pytest"},
		names,
		"deps from PEP 621 + PEP 735 + Poetry all surface")
}

// --- Revised errNoModernFormat semantics ----------------------------------

// TestParsePyProject_NoneOfThreeTables asserts that a file with
// none of [project], [dependency-groups], [tool.poetry] still
// surfaces errNoModernFormat. The sentinel's meaning revised in
// v3: "all three table sets absent."
func TestParsePyProject_NoneOfThreeTables(t *testing.T) {
	t.Parallel()
	path := writePyProject(t, `[build-system]
requires = ["setuptools>=61.0"]
`)
	_, _, err := parsePyProject(path)
	require.Error(t, err)
	assert.ErrorIs(t, err, errNoModernFormat)
}

// TestParsePyProject_OnlyToolPoetry_ReturnsDeps asserts that a
// pure Poetry file (no [project], no [dependency-groups]) parses
// successfully — this is the gap Commit 6 closes.
func TestParsePyProject_OnlyToolPoetry_ReturnsDeps(t *testing.T) {
	t.Parallel()
	path := writePyProject(t, `[tool.poetry]
name = "x"

[tool.poetry.dependencies]
python = "^3.10"
requests = "^2.31.0"
`)
	info, deps, err := parsePyProject(path)
	require.NoError(t, err,
		"pure Poetry file is now successfully parsed (Commit 6); errNoModernFormat fires only when ALL three are absent")
	assert.Equal(t, "x", info.Name)
	require.Len(t, deps, 1)
}
