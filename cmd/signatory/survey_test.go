package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/store"
	"github.com/sarahmaeve/signatory/internal/survey"
)

// writeTestManifest creates a temp dir containing a go.mod with
// the given content and returns the directory path. Callers use
// it as either the CWD or the parent of --manifest.
func writeTestManifest(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte(content), 0o600))
	return dir
}

// seedSurveyEntity inserts an entity into the store.
func seedSurveyEntity(t *testing.T, s store.Store, uri string) *profile.Entity {
	t.Helper()
	e := &profile.Entity{
		ID:           profile.NewEntityID(),
		CanonicalURI: uri,
		Type:         profile.EntityProject,
		ShortName:    filepath.Base(uri),
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}
	require.NoError(t, s.PutEntity(context.Background(), e))
	return e
}

func seedSurveyPosture(t *testing.T, s store.Store, entityID, version string, tier profile.PostureTier, rationale string) {
	t.Helper()
	require.NoError(t, s.SetPosture(context.Background(), &profile.Posture{
		EntityID:  entityID,
		Tier:      tier,
		Version:   version,
		Rationale: rationale,
		SetBy:     "test",
		SetAt:     time.Now().UTC(),
	}))
}

func seedSurveyBurn(t *testing.T, s store.Store, entityID, reason string) {
	t.Helper()
	require.NoError(t, s.SetBurn(context.Background(), &profile.Burn{
		EntityID: entityID,
		Reason:   reason,
		Source:   profile.BurnSourceLocal,
		BurnedAt: time.Now().UTC(),
		BurnedBy: "test",
	}))
}

// runSurvey executes SurveyCmd.Run with Stdout/Stderr pointed at
// test-local buffers. Returns the captured stdout + stderr
// alongside any error Run returned. No global os.Stdout
// redirection — each test owns its own buffers, so parallel
// tests are race-free.
func runSurvey(t *testing.T, cmd *SurveyCmd, globals *Globals) (stdout, stderr string, err error) {
	t.Helper()
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err = cmd.Run(globals)
	return out.String(), errBuf.String(), err
}

// TestSurvey_Human_AllNotInStore — baseline: empty store, every
// dep reports not-in-store, direct deps surface as action items,
// no indirect-deps list by default.
func TestSurvey_Human_AllNotInStore(t *testing.T) {
	t.Parallel()

	dir := writeTestManifest(t, `module github.com/example/survey-functional

go 1.25.1

require (
	github.com/alecthomas/kong v1.15.0
	gopkg.in/yaml.v3 v3.0.1
)
`)

	globals := testGlobals(t)
	cmd := &SurveyCmd{Manifest: filepath.Join(dir, "go.mod")}

	out, _, err := runSurvey(t, cmd, globals)
	require.NoError(t, err)

	// Project header.
	assert.Contains(t, out, "Surveying go.mod")
	assert.Contains(t, out, "github.com/example/survey-functional")
	assert.Contains(t, out, "go 1.25.1")

	// Summary: 2 direct, 0 indirect, all not-in-store.
	assert.Contains(t, out, "2 dependencies")
	assert.Contains(t, out, "2 direct")
	assert.Contains(t, out, "not-in-store")

	// Direct-deps table.
	assert.Contains(t, out, "github.com/alecthomas/kong")
	assert.Contains(t, out, "gopkg.in/yaml.v3")

	// not-in-store annotation — clarifies that [·] rows are "no
	// data collected yet," distinct from [?] which is "data but
	// no verdict." See tierSummaryAnnotation for why these two
	// states need to be disambiguated.
	assert.Contains(t, out, "(no data collected yet)",
		"not-in-store tier must carry the 'no data collected yet' clarifier in the summary block")

	// Vet-path footer — names the three categories of next move.
	// Guarded on NeedsReview; these deps are all not-in-store
	// (direct) so the footer fires.
	assert.Contains(t, out, "To vet direct dependencies",
		"footer must steer the user toward the actual vet-path categories")
	assert.Contains(t, out, "/analyze <target>",
		"footer must name /analyze (the Claude skill) as the LLM-backed review path")
	assert.Contains(t, out, "signatory posture set",
		"footer must name posture set as the known-verdict path")
	assert.Contains(t, out, "signatory burn",
		"footer must name burn as the known-bad path")

	// No "Action items" / suggested-commands section. The CLI
	// verbs survey previously pointed at (signatory analyze) only
	// collect signals — they cannot produce the trust verdict
	// that flips a [?] row. See the package-level doc on
	// printSurveyHuman for the dropped-section rationale.
	assert.NotContains(t, out, "Action items",
		"survey output must not steer users to commands that don't deliver postures")
	assert.NotContains(t, out, "signatory analyze ",
		"survey output must not embed `signatory analyze` invocations — they collect signals but don't produce verdicts")
}

// TestSurvey_Human_MixedTiers covers the rich output with each
// tier represented. Every rendering branch fires at least once.
func TestSurvey_Human_MixedTiers(t *testing.T) {
	t.Parallel()

	dir := writeTestManifest(t, `module github.com/example/mixed-tiers

go 1.25.1

require (
	github.com/vetted/lib v1.0.0
	github.com/rejected/lib v1.0.0
	github.com/burned/lib v1.0.0
	github.com/unexamined/lib v1.0.0
)
`)

	globals := testGlobals(t)

	// Open the store to seed it. Close explicitly before running
	// survey so survey sees committed state.
	s, err := store.OpenSQLite(context.Background(), globals.DBPath)
	require.NoError(t, err)

	vettedEnt := seedSurveyEntity(t, s, "repo:github/vetted/lib")
	seedSurveyPosture(t, s, vettedEnt.ID, "v1.0.0", profile.PostureVettedFrozen, "strong signals")

	rejectedEnt := seedSurveyEntity(t, s, "repo:github/rejected/lib")
	seedSurveyPosture(t, s, rejectedEnt.ID, "v1.0.0", profile.PostureRejected, "abandoned project")

	burnedEnt := seedSurveyEntity(t, s, "repo:github/burned/lib")
	seedSurveyBurn(t, s, burnedEnt.ID, "supply-chain compromise 2026-04-15")

	// unexamined/lib: entity present, no posture → TierUnexamined.
	seedSurveyEntity(t, s, "repo:github/unexamined/lib")

	require.NoError(t, s.Close())

	cmd := &SurveyCmd{Manifest: filepath.Join(dir, "go.mod")}
	out, _, err := runSurvey(t, cmd, globals)
	require.NoError(t, err)

	// Each tier appears in the summary block.
	for _, tierName := range []string{
		"vetted-frozen",
		"rejected",
		"burned",
		"unexamined",
	} {
		assert.Contains(t, out, tierName, "summary should list %s", tierName)
	}

	// Burn context rendered.
	assert.Contains(t, out, "supply-chain compromise 2026-04-15")

	// Posture rationales rendered.
	assert.Contains(t, out, "strong signals")
	assert.Contains(t, out, "abandoned project")

	// unexamined annotation — this test's fixture includes
	// unexamined/lib as a direct dep (entity seeded, no posture),
	// so the summary block must carry the "signal data in store;
	// no posture verdict yet" clarifier. Companion to the
	// not-in-store annotation test in AllNotInStore; together
	// they cover both non-resolved tiers.
	assert.Contains(t, out, "(signal data in store; no posture verdict yet)",
		"unexamined tier must carry the 'signal data in store' clarifier — distinguishes it from not-in-store")

	// "Only unexamined deps end up in NeedsReview" — that intent
	// is now covered structurally by internal/survey/survey_test.go
	// (TestSurvey_NeedsReview_*). The Action-items rendering that
	// formerly proved it via stdout was removed, so we no longer
	// re-assert it here through a presentation-layer proxy.
}

// TestSurvey_JSON_OutputShape asserts --json output is a
// parseable Result. Future tooling (web UI, CI gates) relies on
// the schema.
func TestSurvey_JSON_OutputShape(t *testing.T) {
	t.Parallel()

	dir := writeTestManifest(t, `module github.com/example/json-shape

go 1.25.1

require github.com/alecthomas/kong v1.15.0
`)

	globals := testGlobals(t)
	cmd := &SurveyCmd{Manifest: filepath.Join(dir, "go.mod"), JSON: true}

	out, _, err := runSurvey(t, cmd, globals)
	require.NoError(t, err)

	var result survey.Result
	require.NoError(t, json.Unmarshal([]byte(out), &result),
		"--json output must parse as a survey.Result")

	assert.Equal(t, "github.com/example/json-shape", result.Project.Name)
	assert.Equal(t, "go", result.Project.Ecosystem)
	assert.Equal(t, "1.25.1", result.Project.EcoVersion)
	require.Len(t, result.Deps, 1)
	assert.Equal(t, "github.com/alecthomas/kong", result.Deps[0].Dep.Name)
	assert.Equal(t, survey.TierNotInStore, result.Deps[0].Tier)
	assert.Equal(t, 1, result.Summary.Direct)
	assert.Equal(t, 1, result.Summary.ByTier[survey.TierNotInStore])
}

// TestSurvey_AutoDetectCWD exercises the no-flag path: running
// survey without --manifest from a dir containing go.mod. The
// command should detect and parse it.
//
// NOT parallel — this test mutates the process-global CWD.
func TestSurvey_AutoDetectCWD(t *testing.T) {
	dir := writeTestManifest(t, `module github.com/example/auto-detect

go 1.25.1
`)

	origCWD, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() {
		_ = os.Chdir(origCWD)
	})

	globals := testGlobals(t)
	cmd := &SurveyCmd{}

	out, _, err := runSurvey(t, cmd, globals)
	require.NoError(t, err)
	assert.Contains(t, out, "github.com/example/auto-detect")
}

// TestSurvey_NoManifestNoCWDMatch fails cleanly when invoked
// from a directory without a recognized manifest and without
// an explicit --manifest flag.
//
// NOT parallel — mutates the process-global CWD.
func TestSurvey_NoManifestNoCWDMatch(t *testing.T) {
	dir := t.TempDir() // empty

	origCWD, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() {
		_ = os.Chdir(origCWD)
	})

	globals := testGlobals(t)
	cmd := &SurveyCmd{}
	_, _, err = runSurvey(t, cmd, globals)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no recognized manifest")
	assert.Contains(t, err.Error(), "pass --manifest")
}

// TestSurvey_RefreshFlagEmitsWarning — --refresh is accepted but
// emits a stderr note in v0.1. This guards against a silent
// regression if someone wires partial refresh behavior without
// updating the help text.
func TestSurvey_RefreshFlagEmitsWarning(t *testing.T) {
	t.Parallel()

	dir := writeTestManifest(t, `module github.com/example/refresh-warn

go 1.25.1
`)

	globals := testGlobals(t)
	cmd := &SurveyCmd{
		Manifest: filepath.Join(dir, "go.mod"),
		Refresh:  true,
	}

	_, stderr, err := runSurvey(t, cmd, globals)
	require.NoError(t, err)
	assert.Contains(t, stderr, "--refresh is not implemented",
		"should warn when --refresh is passed but ignored")
}

// TestSurvey_All_ListsIndirectDeps covers the --all flag:
// indirect deps get rendered individually instead of collapsed
// into a count.
func TestSurvey_All_ListsIndirectDeps(t *testing.T) {
	t.Parallel()

	dir := writeTestManifest(t, `module github.com/example/all-flag

go 1.25.1

require github.com/alecthomas/kong v1.15.0

require github.com/stretchr/testify v1.11.1 // indirect
`)

	// Default: indirect summarized by count.
	defaultOut, _, err := runSurvey(t, &SurveyCmd{Manifest: filepath.Join(dir, "go.mod")}, testGlobals(t))
	require.NoError(t, err)
	assert.Contains(t, defaultOut, "Indirect dependencies: 1")
	assert.NotContains(t, defaultOut, "github.com/stretchr/testify",
		"default (non-all) output should NOT list indirect deps individually")

	// With --all: indirect listed.
	allOut, _, err := runSurvey(t, &SurveyCmd{Manifest: filepath.Join(dir, "go.mod"), All: true}, testGlobals(t))
	require.NoError(t, err)
	assert.Contains(t, allOut, "github.com/stretchr/testify",
		"--all output should list indirect deps")
}
