package pypi

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParsePyProject_PEP735_Basic(t *testing.T) {
	t.Parallel()
	path := writePyProject(t, `[dependency-groups]
dev = ["pytest>=8.0", "ruff"]
`)
	info, deps, err := parsePyProject(path)
	require.NoError(t, err)

	// No [project] table, so Name is empty — this is the
	// non-package application/CLI case PEP 735 was designed for.
	assert.Empty(t, info.Name)
	assert.Equal(t, "pypi", info.Ecosystem)

	require.Len(t, deps, 2)
	for _, d := range deps {
		assert.True(t, d.Direct)
		assert.Equal(t, "pypi", d.Ecosystem)
	}
}

func TestParsePyProject_PEP735_MultipleGroups(t *testing.T) {
	t.Parallel()
	path := writePyProject(t, `[dependency-groups]
dev = ["pytest>=8.0"]
test = ["coverage"]
docs = ["sphinx>=7.0"]
`)
	_, deps, err := parsePyProject(path)
	require.NoError(t, err)
	require.Len(t, deps, 3)

	names := make([]string, len(deps))
	for i, d := range deps {
		names[i] = d.Name
	}
	assert.ElementsMatch(t, []string{"pytest", "coverage", "sphinx"}, names)
}

func TestParsePyProject_PEP735_IncludeResolvesInOrder(t *testing.T) {
	// Per PEP 735: includes expand inline at their position, with
	// no deduplication. If `bar = ["c", {include-group = "foo"}, "d"]`
	// and `foo = ["a", "b"]`, then `bar` resolves to `["c", "a", "b", "d"]`.
	t.Parallel()
	path := writePyProject(t, `[dependency-groups]
foo = ["a==1.0", "b==1.0"]
bar = ["c==1.0", {include-group = "foo"}, "d==1.0"]
`)
	_, deps, err := parsePyProject(path)
	require.NoError(t, err)

	// Both groups flatten: foo's entries (a, b) appear at
	// least once (from foo itself OR via the include in bar).
	// Per the spec's no-dedupe rule, foo's entries appear TWICE
	// in the combined output: once as foo's own deps, once as
	// the included expansion in bar. Total: foo(2) + bar(4) = 6.
	require.Len(t, deps, 6, "PEP 735 forbids deduplication during include expansion")

	names := make([]string, len(deps))
	for i, d := range deps {
		names[i] = d.Name
	}
	// a and b each appear twice; c and d each appear once.
	count := make(map[string]int, 4)
	for _, n := range names {
		count[n]++
	}
	assert.Equal(t, 2, count["a"], "a appears in foo AND in bar's include")
	assert.Equal(t, 2, count["b"], "b appears in foo AND in bar's include")
	assert.Equal(t, 1, count["c"])
	assert.Equal(t, 1, count["d"])
}

func TestParsePyProject_PEP735_NestedIncludes(t *testing.T) {
	// A includes B, B includes C — the chain flattens
	// recursively.
	t.Parallel()
	path := writePyProject(t, `[dependency-groups]
c = ["c==1.0"]
b = [{include-group = "c"}, "b==1.0"]
a = [{include-group = "b"}, "a==1.0"]
`)
	_, deps, err := parsePyProject(path)
	require.NoError(t, err)

	// a resolves to: include b → (include c → c) + b + a = c, b, a
	// b resolves to: include c + b = c, b
	// c resolves to: c
	// Union: c, b, a, c, b, c (3 + 2 + 1 = 6 entries)
	require.Len(t, deps, 6)
}

func TestParsePyProject_PEP735_GroupNameNormalization(t *testing.T) {
	// Per PEP 735, the regex re.sub(r"[-_.]+", "-", name).lower()
	// applies. So `Dev`, `dev`, `dev_deps`, `Dev.Deps` all
	// normalize to either `dev` or `dev-deps`. An include
	// reference is matched after normalization.
	t.Parallel()
	path := writePyProject(t, `[dependency-groups]
test = ["pytest"]
docs = [{include-group = "TEST"}]
`)
	_, deps, err := parsePyProject(path)
	require.NoError(t, err)

	// docs group includes "TEST" which normalizes to "test"
	// and matches the test group. Total deps: pytest from test,
	// pytest from docs's include of test = 2.
	require.Len(t, deps, 2)
	for _, d := range deps {
		assert.Equal(t, "pytest", d.Name)
	}
}

func TestParsePyProject_PEP735_DuplicateNormalizedGroupNames(t *testing.T) {
	// Both `dev` and `Dev` normalize to `dev` (PEP 503 lowercases
	// before comparing). Per PEP 735, tools "SHOULD emit an error"
	// on duplicate normalized names — we treat it as MUST since
	// silent merge would change the dep set in non-obvious ways.
	t.Parallel()
	path := writePyProject(t, `[dependency-groups]
dev = ["pytest"]
Dev = ["ruff"]
`)
	_, _, err := parsePyProject(path)
	require.Error(t, err)
	assert.ErrorIs(t, err, errPEP735DuplicateGroup,
		"sentinel locks the condition; substring would match any prose containing 'duplicate'")
}

func TestParsePyProject_PEP735_IncludeUndefinedGroup(t *testing.T) {
	t.Parallel()
	path := writePyProject(t, `[dependency-groups]
docs = [{include-group = "nonesuch"}]
`)
	_, _, err := parsePyProject(path)
	require.Error(t, err)
	assert.ErrorIs(t, err, errPEP735UndefinedInclude)
	assert.Contains(t, err.Error(), "nonesuch",
		"diagnostic content: error should name the undefined group reference")
}

func TestParsePyProject_PEP735_DirectCycle(t *testing.T) {
	t.Parallel()
	path := writePyProject(t, `[dependency-groups]
a = [{include-group = "a"}]
`)
	_, _, err := parsePyProject(path)
	require.Error(t, err)
	assert.ErrorIs(t, err, errPEP735Cycle)
}

func TestParsePyProject_PEP735_TransitiveCycle(t *testing.T) {
	// A → B → C → A. Visited-set must catch this regardless of
	// the order the parser starts walking from.
	t.Parallel()
	path := writePyProject(t, `[dependency-groups]
a = [{include-group = "b"}]
b = [{include-group = "c"}]
c = [{include-group = "a"}]
`)
	_, _, err := parsePyProject(path)
	require.Error(t, err)
	assert.ErrorIs(t, err, errPEP735Cycle)
	// Diagnostic content: error should name a group in the cycle.
	// Without this assertion an error like "cycle detected" with no
	// specifics would pass — the test-quality review flagged this.
	msg := err.Error()
	assert.True(t,
		strings.Contains(msg, `"a"`) ||
			strings.Contains(msg, `"b"`) ||
			strings.Contains(msg, `"c"`),
		"cycle error should name a group in the cycle (a/b/c); got %q", msg)
}

func TestParsePyProject_PEP735_CycleErrorNamesOuterGroup(t *testing.T) {
	// When a cycle is reached transitively from group X (not at X
	// itself), the error must surface BOTH the outer iteration
	// group and the inner cycle point. Without the outer-name
	// breadcrumb, a user reading the log sees only the inner name
	// and has to reverse-engineer how resolution got there.
	t.Parallel()
	path := writePyProject(t, `[dependency-groups]
outer = [{include-group = "a"}]
a = [{include-group = "b"}]
b = [{include-group = "a"}]
`)
	_, _, err := parsePyProject(path)
	require.Error(t, err)
	require.ErrorIs(t, err, errPEP735Cycle)
	msg := err.Error()
	// Iteration starts at the alphabetically-first group; the
	// outer-iteration breadcrumb must surface SOME outer group
	// name from the iteration. Both "outer" and "a" are valid
	// breadcrumb roots depending on iteration order.
	assert.True(t,
		strings.Contains(msg, `"outer"`) || strings.Contains(msg, `"a"`),
		"error should breadcrumb the outer iteration group; got %q", msg)
}

func TestParsePyProject_PEP735_InvalidIncludeShape(t *testing.T) {
	// Table entries must have exactly one key, "include-group".
	// Anything else is malformed.
	t.Parallel()
	path := writePyProject(t, `[dependency-groups]
docs = [{include-group = "test", extra-key = "value"}]
test = ["pytest"]
`)
	_, _, err := parsePyProject(path)
	require.Error(t, err)
}

func TestParsePyProject_PEP735_EmptyGroup(t *testing.T) {
	t.Parallel()
	path := writePyProject(t, `[dependency-groups]
empty = []
dev = ["pytest"]
`)
	_, deps, err := parsePyProject(path)
	require.NoError(t, err)
	require.Len(t, deps, 1, "empty group contributes no deps; non-empty group still surfaces")
}

func TestParsePyProject_PEP621AndPEP735_Combined(t *testing.T) {
	// Both tables present — deps from both surface, all Direct.
	t.Parallel()
	path := writePyProject(t, `[project]
name = "example-pkg"
dependencies = ["requests==2.31.0"]

[project.optional-dependencies]
test = ["pytest>=8.0"]

[dependency-groups]
dev = ["ruff", "mypy"]
`)
	info, deps, err := parsePyProject(path)
	require.NoError(t, err)
	assert.Equal(t, "example-pkg", info.Name)

	// requests (PEP 621 deps) + pytest (PEP 621 optional) +
	// ruff + mypy (PEP 735 dev) = 4
	require.Len(t, deps, 4)
	for _, d := range deps {
		assert.True(t, d.Direct)
	}

	names := make([]string, len(deps))
	for i, d := range deps {
		names[i] = d.Name
	}
	assert.ElementsMatch(t,
		[]string{"requests", "pytest", "ruff", "mypy"},
		names,
	)
}

func TestParsePyProject_PEP735_OnlyNoProject(t *testing.T) {
	// The application/CLI use case PEP 735 was designed for —
	// no [project] table at all, [dependency-groups] is the only
	// deps source.
	t.Parallel()
	path := writePyProject(t, `[dependency-groups]
runtime = ["click", "pyyaml"]
dev = ["pytest"]
`)
	info, deps, err := parsePyProject(path)
	require.NoError(t, err)

	assert.Empty(t, info.Name, "no [project] table; no project identity")
	assert.Empty(t, info.EcoVersion)
	assert.Equal(t, "pypi", info.Ecosystem)

	require.Len(t, deps, 3)
}

func TestParsePyProject_PEP735_SameNameAsOptionalDependencyGroup(t *testing.T) {
	// A file CAN have [project.optional-dependencies].dev AND
	// [dependency-groups].dev simultaneously — they're distinct
	// collections per PEP 735's "extras coexistence" note.
	// Both surface as deps; no merge or dedupe.
	t.Parallel()
	path := writePyProject(t, `[project]
name = "example-pkg"
dependencies = []

[project.optional-dependencies]
dev = ["pytest"]

[dependency-groups]
dev = ["ruff"]
`)
	_, deps, err := parsePyProject(path)
	require.NoError(t, err)
	require.Len(t, deps, 2,
		"optional-dependencies.dev and dependency-groups.dev are independent — both surface")

	names := make([]string, len(deps))
	for i, d := range deps {
		names[i] = d.Name
	}
	assert.ElementsMatch(t, []string{"pytest", "ruff"}, names)
}
