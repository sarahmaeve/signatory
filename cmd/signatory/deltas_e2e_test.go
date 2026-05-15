package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testDBPath is the path to the generated sample DB, relative to
// the package directory. See internal/deltas/testdata/generate for
// the generator script.
const testDBPath = "../../internal/deltas/testdata/sample.db"

// deltasTestGlobals points at the committed sample.db and returns a
// Globals suitable for invoking DeltasCmd.Run in tests. The store
// is read-only in practice; the verb opens, reads, closes.
func deltasTestGlobals(t *testing.T) *Globals {
	t.Helper()
	abs, err := filepath.Abs(testDBPath)
	require.NoError(t, err)
	return &Globals{DBPath: abs}
}

// runDeltas builds a DeltasCmd with the given target and flags,
// invokes Run, and returns stdout output for assertion.
func runDeltas(t *testing.T, target string, mod func(*DeltasCmd)) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd := &DeltasCmd{
		Target: target,
		All:    true, // default for these tests; test cases override via mod
		Stdout: &stdout,
		Stderr: &stderr,
	}
	if mod != nil {
		mod(cmd)
	}
	require.NoError(t, cmd.Run(deltasTestGlobals(t)))
	return stdout.String()
}

// TestDeltas_E2E_AxiosTrustedPublishingLost runs the verb against
// the seeded axios entity. The trusted_publishing signal must show
// the present→absent transition with publisher fields removed.
func TestDeltas_E2E_AxiosTrustedPublishingLost(t *testing.T) {
	got := runDeltas(t, "pkg:npm/axios", nil)

	assert.Contains(t, got, "Deltas for pkg:npm/axios",
		"header names the target")
	assert.Contains(t, got, "trusted_publishing",
		"the signal type appears")
	assert.Contains(t, got, "CHANGED", "transition marker present")
	assert.Contains(t, got, "present:",
		"present field surfaces in the diff")
	// Removed fields should appear.
	for _, removed := range []string{"publisher_kind", "source_repository", "workflow"} {
		assert.Contains(t, got, removed,
			"removed publisher field %q must surface", removed)
	}
}

// TestDeltas_E2E_TanStackUnpublishGap runs the verb against the
// seeded @tanstack/react-router entity. Two signals will surface
// because the entity has both version_unpublish_observed and
// commit_publish_cadence_divergence scenarios seeded against it.
func TestDeltas_E2E_TanStackUnpublishGap(t *testing.T) {
	got := runDeltas(t, "pkg:npm/@tanstack/react-router", nil)

	// Both seeded signals must surface (--all + no filter).
	assert.Contains(t, got, "version_unpublish_observed",
		"unpublish signal must appear")
	assert.Contains(t, got, "commit_publish_cadence_divergence",
		"cadence signal must appear")

	// The unpublish-gap scalar transition.
	assert.Contains(t, got, "unpublished_count")
	assert.Contains(t, got, "0 → 2")

	// Cadence transition.
	assert.Contains(t, got, "shape:")
	assert.Contains(t, got, "synchronized")
	assert.Contains(t, got, "active-repo-paused-publishes")
}

// TestDeltas_E2E_FilterByType narrows to one signal even when the
// entity has multiple. --type filter dropping the cadence row.
func TestDeltas_E2E_FilterByType(t *testing.T) {
	got := runDeltas(t, "pkg:npm/@tanstack/react-router", func(c *DeltasCmd) {
		c.Type = "version_unpublish_observed"
	})

	assert.Contains(t, got, "version_unpublish_observed",
		"the filtered-in signal must appear")
	assert.NotContains(t, got, "commit_publish_cadence_divergence",
		"the filtered-out signal must NOT appear")
}

// TestDeltas_E2E_WorkflowRefChange exercises the sketch-5 detection
// axis: workflow_refs array changes position-0 between observations.
func TestDeltas_E2E_WorkflowRefChange(t *testing.T) {
	got := runDeltas(t, "pkg:pypi/sample-careful-variant", nil)

	assert.Contains(t, got, "attestation_consistency")
	assert.Contains(t, got, "workflow_refs")
	assert.Contains(t, got, "release-v2.yml",
		"the new workflow name from current observation must surface")
	assert.Contains(t, got, "workflow_ref_transitions",
		"the transitions count change must surface")
	assert.Contains(t, got, "0 → 1",
		"workflow_ref_transitions scalar transition")
}

// TestDeltas_E2E_BotPublisherAppears exercises stable-key alignment
// on an array-of-objects: the publisher_account_class.logins array
// gains one entry (evil-publisher-bot, class=service-account).
func TestDeltas_E2E_BotPublisherAppears(t *testing.T) {
	got := runDeltas(t, "pkg:pypi/sample-bot-target", nil)

	assert.Contains(t, got, "publisher_account_class")
	assert.Contains(t, got, "evil-publisher-bot",
		"the added publisher login (stable-key alignment) must surface")
	assert.Contains(t, got, "non_human_count",
		"the non_human_count scalar change must surface")
	assert.Contains(t, got, "0 → 1")
}

// TestDeltas_E2E_VersionBurstFlips: boolean transition surfaces.
func TestDeltas_E2E_VersionBurstFlips(t *testing.T) {
	got := runDeltas(t, "pkg:npm/burst-shape-sample", nil)

	assert.Contains(t, got, "version_publish_burst")
	assert.Contains(t, got, "burst_detected")
	assert.Contains(t, got, "false → true",
		"boolean transition for burst_detected")
}

// TestDeltas_E2E_MaintainerChurn: different-length primitive array
// (set-diff handling) — alice and bob unchanged, newcomer added.
func TestDeltas_E2E_MaintainerChurn(t *testing.T) {
	got := runDeltas(t, "pkg:npm/maintainer-churn-sample", nil)

	assert.Contains(t, got, "maintainer_count")
	assert.Contains(t, got, "count")
	assert.Contains(t, got, "2 → 3", "maintainer count scalar")
	assert.Contains(t, got, "newcomer",
		"the added maintainer must surface as an added entry")
}

// TestDeltas_E2E_NpmDependenciesAdded: a new npm dependency surfaces
// through the real `signatory deltas` command — the dependency-drift
// transition the live dogfood could not produce against an unchanging
// real package. `direct` is a different-length primitive array, so it
// goes through the same set-diff path as maintainer logins.
func TestDeltas_E2E_NpmDependenciesAdded(t *testing.T) {
	got := runDeltas(t, "pkg:npm/dependency-added-sample", nil)

	assert.Contains(t, got, "Deltas for pkg:npm/dependency-added-sample",
		"header names the target")
	assert.Contains(t, got, "npm_dependencies",
		"the dependency signal type appears")
	assert.Contains(t, got, "direct_count")
	assert.Contains(t, got, "2 → 3", "direct_count scalar backstop")
	assert.Contains(t, got, "left-pad",
		"the newly-added dependency must surface as an added entry")
}

// TestDeltas_E2E_CargoDependenciesAdded: same proof for the cargo
// signal, confirming the byte-identical value shape renders an
// identical CLI transition across ecosystems.
func TestDeltas_E2E_CargoDependenciesAdded(t *testing.T) {
	got := runDeltas(t, "pkg:cargo/dependency-added-sample", nil)

	assert.Contains(t, got, "cargo_dependencies",
		"the dependency signal type appears")
	assert.Contains(t, got, "2 → 3", "direct_count scalar backstop")
	assert.Contains(t, got, "tokio-macros",
		"the newly-added crate must surface as an added entry")
}

// TestDeltas_E2E_MavenDependenciesAdded: same proof for the maven
// signal. The added entry is a groupId:artifactId coordinate,
// confirming the byte-identical value shape renders an identical CLI
// transition for the Maven ecosystem.
func TestDeltas_E2E_MavenDependenciesAdded(t *testing.T) {
	got := runDeltas(t, "pkg:maven/com.example/dependency-added-sample", nil)

	assert.Contains(t, got, "maven_dependencies",
		"the dependency signal type appears")
	assert.Contains(t, got, "2 → 3", "direct_count scalar backstop")
	assert.Contains(t, got, "com.h2database:h2",
		"the newly-added coordinate must surface as an added entry")
}

// TestDeltas_E2E_GemDependenciesAdded: same proof for the gem signal,
// confirming the byte-identical value shape renders an identical CLI
// transition for the Ruby ecosystem.
func TestDeltas_E2E_GemDependenciesAdded(t *testing.T) {
	got := runDeltas(t, "pkg:gem/dependency-added-sample", nil)

	assert.Contains(t, got, "gem_dependencies",
		"the dependency signal type appears")
	assert.Contains(t, got, "2 → 3", "direct_count scalar backstop")
	assert.Contains(t, got, "railties",
		"the newly-added runtime dependency must surface as an added entry")
}

// TestDeltas_E2E_PyPIDependenciesAdded: same proof for the pypi
// signal, confirming the byte-identical value shape renders an
// identical CLI transition for the Python ecosystem.
func TestDeltas_E2E_PyPIDependenciesAdded(t *testing.T) {
	got := runDeltas(t, "pkg:pypi/dependency-added-sample", nil)

	assert.Contains(t, got, "pypi_dependencies",
		"the dependency signal type appears")
	assert.Contains(t, got, "2 → 3", "direct_count scalar backstop")
	assert.Contains(t, got, "charset-normalizer",
		"the newly-added dependency must surface as an added entry")
}

// TestDeltas_E2E_GoDependenciesAdded: parity proof for the pre-
// existing go signal. Unlike the registry ecosystems, go_dependencies
// carries a real indirect_count: it stays constant (5) across the two
// observations while the direct list gains one module path, so only
// direct_count and total_count move. Confirms go renders an
// equivalent CLI transition now that parseGoModDeps sorts+dedupes.
func TestDeltas_E2E_GoDependenciesAdded(t *testing.T) {
	got := runDeltas(t, "repo:github/example/go-dependency-added-sample", nil)

	assert.Contains(t, got, "go_dependencies",
		"the dependency signal type appears")
	assert.Contains(t, got, "2 → 3", "direct_count scalar backstop")
	assert.Contains(t, got, "github.com/spf13/cobra",
		"the newly-added module path must surface as an added entry")
}

// TestDeltas_E2E_JSON exercises the structured JSON output path
// against a real scenario. Decodes the output and asserts on the
// top-level shape.
func TestDeltas_E2E_JSON(t *testing.T) {
	out := runDeltas(t, "pkg:npm/axios", func(c *DeltasCmd) {
		c.JSON = true
	})

	var decoded map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &decoded))

	assert.Equal(t, "pkg:npm/axios", decoded["target"])

	window, ok := decoded["window"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "all", window["kind"])

	groups, ok := decoded["groups"].([]any)
	require.True(t, ok)
	require.NotEmpty(t, groups)

	group := groups[0].(map[string]any)
	assert.Equal(t, "trusted_publishing", group["type"])
	assert.Equal(t, "npm-registry", group["source"])
}

// TestDeltas_E2E_TargetNotFound surfaces a clean error when the
// store has no entity for the requested URI.
func TestDeltas_E2E_TargetNotFound(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := &DeltasCmd{
		Target: "pkg:npm/never-seen-by-store",
		All:    true,
		Stdout: &stdout,
		Stderr: &stderr,
	}
	err := cmd.Run(deltasTestGlobals(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found in store",
		"missing-target error should be clear about why")
}

// TestDeltas_E2E_LastFlag exercises the --last per-group limit.
// Each seeded entity has exactly 2 observations per signal type,
// so --last 1 trims to the most recent observation only; no pair-
// diffs are computable (only one observation left); signal appears
// as "no change" under --include-unchanged.
func TestDeltas_E2E_LastFlag(t *testing.T) {
	got := runDeltas(t, "pkg:npm/axios", func(c *DeltasCmd) {
		c.All = false
		c.Last = 1
		c.IncludeUnchanged = true
	})
	assert.Contains(t, got, "trusted_publishing")
	assert.Contains(t, got, "no change",
		"single-observation group has no pair-diffs to surface")
}
