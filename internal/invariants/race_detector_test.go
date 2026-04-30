package invariants

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// raceWiredSurfaces enumerates the project's local test gauntlet
// (`make test`) which must invoke `go test` with the `-race` flag.
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
// If `-race` is removed from `make test`, those tests silently
// degrade to smoke checks: they still pass, but no longer catch
// the bugs they were written to catch. This invariant fails fast
// in that scenario so the degradation is visible at commit time
// rather than after a real race lands.
//
// Historical note. An earlier version of this invariant (added in
// commit 6631f3a, 2026-04-30) also asserted that
// .github/workflows/ci.yml ran `-race`. CI no longer does so —
// see the comment in ci.yml: "we no longer perform test -race on
// github as its capacity has collapsed" (commit 02d7ed2). The CI
// surface was dropped from this invariant when that decision was
// made permanent; the Makefile surface is unchanged because the
// pipeline-test reliance on `-race` is unchanged and `make test`
// is the local gauntlet that delivers it. Pipeline-test reliance
// on `-race` is now honor-system for CI runs and asserted at the
// local gauntlet level here.
//
// Substring search (not Makefile parsing) is intentional, but the
// matcher anchors on the syntactic form of an actual command
// rather than the bare substring `go test -race`. The bare
// substring would match prose comments and `## help`-style
// annotations.
//
// Per-surface anchor: the Makefile uses a tab-prefixed
// `\tgo test -race`. The leading tab means the substring must be
// on a recipe line (Make recipes are tab-prefixed by syntax)
// rather than in a `## help` comment or a prose docstring.
//
// Pre-commit hook is deliberately NOT in this list. The Makefile
// docstring claims the hook runs `test -race` but the actual
// .githooks/pre-commit only runs `go test ./...`. That drift exists
// independently of this invariant; surface it if you're touching
// the hook, but don't load it into this guard or the test would
// fail today for a reason unrelated to the regression it guards
// against.
var raceWiredSurfaces = []struct {
	name    string
	path    string
	matcher string
}{
	{
		name:    "makefile",
		path:    "Makefile",
		matcher: "\tgo test -race",
	},
}

// TestRaceDetectorWiredIntoCI asserts each surface in
// raceWiredSurfaces contains its per-surface anchor substring (see
// the var-block doc for why the `\t` prefix on the matcher is
// load-bearing).
//
// Note on the name: the `IntoCI` suffix predates the 2026 removal
// of `-race` from the GitHub Actions workflow. The test now guards
// the Makefile (`make test`) only — the historical-note paragraph
// in the var-block doc explains the change. The function name is
// preserved because it is referenced by name in pipeline-test
// docstrings (internal/pipeline/race_test.go,
// internal/pipeline/adversarial_test.go); rename is a follow-up.
//
// Revert proof: drop `-race` from the Makefile's `test:` recipe;
// the makefile subtest fails with the surface name, path, and
// required matcher in the error message.
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
