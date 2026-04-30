package invariants

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// raceWiredSurfaces enumerates the project's two enforced test
// surfaces — `make test` and the GitHub Actions CI workflow — that
// must invoke `go test` with the `-race` flag.
//
// Why this is an invariant. A swath of pipeline tests
// (internal/pipeline/race_test.go and
// TestStore_DeleteSessionWhileDepositingRace in adversarial_test.go)
// deliberately exercise concurrent goroutines against the store and
// HTTP handler layer with the race detector as the *primary*
// validator of correctness. Each such test documents this reliance
// in its docstring; several swallow per-call errors or stop short
// of asserting end-state, because the failure mode they exist to
// surface IS a data race rather than a logical-error outcome.
//
// If `-race` is removed from CI to save minutes, those tests
// silently degrade to smoke checks: they still pass, but no longer
// catch the bugs they were written to catch. This invariant fails
// fast in that scenario so the degradation is visible at PR time
// rather than after a real race lands.
//
// Substring search (not YAML/Makefile parsing) is intentional, but
// each surface's matcher anchors on the syntactic form of an actual
// command rather than the bare substring `go test -race`. The bare
// substring would match prose comments and YAML step `name:`
// fields — an early version of this test (2026-04-30) would have
// passed even if the YAML `run:` line was deleted, because three
// other unrelated `go test -race` substrings (header comment,
// quoted comment, step name) survived.
//
// Per-surface anchors:
//
//   - ci.yml uses `run: go test -race`. The `run:` prefix means
//     the substring must be on the actual GitHub Actions shell-
//     command line, not in a comment or a step name. Multi-line
//     run blocks (`run: |` followed by indented lines) would NOT
//     match — accepted as a benign-refactor cost; if the workflow
//     ever goes multi-line, update the matcher in the same commit.
//   - Makefile uses a tab-prefixed `\tgo test -race`. The leading
//     tab means the substring must be on a recipe line (Make
//     recipes are tab-prefixed by syntax) rather than in a
//     `## help` comment or a prose docstring.
//
// Pre-commit hook is deliberately NOT in this list. The Makefile
// docstring claims the hook runs `test -race` but the actual
// .githooks/pre-commit only runs `go test ./...`. That drift exists
// independently of this invariant; surface it if you're touching
// the hook, but don't load it into this guard or the test would
// fail today for a reason unrelated to the CI regression it
// guards against.
var raceWiredSurfaces = []struct {
	name    string
	path    string
	matcher string
}{
	{
		name:    "ci_workflow",
		path:    filepath.Join(".github", "workflows", "ci.yml"),
		matcher: "run: go test -race",
	},
	{
		name:    "makefile",
		path:    "Makefile",
		matcher: "\tgo test -race",
	},
}

// TestRaceDetectorWiredIntoCI asserts each surface in
// raceWiredSurfaces contains its per-surface anchor substring (see
// the var-block doc for why `run:` and `\t` prefixes are
// load-bearing).
//
// Revert proof: drop `-race` from either the CI workflow's `run:`
// step or the Makefile's `test:` recipe; the corresponding subtest
// fails with the surface name, path, and required matcher in the
// error message. Critically, deleting only the `run:` line in
// ci.yml while leaving the step `name: go test -race` in place
// ALSO fails — that scenario was the gap a bare-substring matcher
// would have missed.
func TestRaceDetectorWiredIntoCI(t *testing.T) {
	root := findModuleRoot(t)

	for _, surface := range raceWiredSurfaces {
		t.Run(surface.name, func(t *testing.T) {
			full := filepath.Join(root, surface.path)
			data, err := os.ReadFile(full) //nolint:gosec // G304: build-time fixture under module root
			require.NoError(t, err, "%s not found at %s", surface.name, full)

			assert.Truef(t, strings.Contains(string(data), surface.matcher),
				"%s (%s) must invoke `go test` with the -race flag — "+
					"several pipeline tests rely on the race detector "+
					"as their primary validator (see "+
					"internal/pipeline/race_test.go and the docstring "+
					"on TestRaceDetectorWiredIntoCI). Required matcher: "+
					"%q. Restore the -race flag on an actual command "+
					"line (not a comment / step name); do not silence "+
					"this test.",
				surface.name, surface.path, surface.matcher)
		})
	}
}
