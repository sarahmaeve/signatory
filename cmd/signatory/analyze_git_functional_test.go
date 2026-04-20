package main

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
	gitcollector "github.com/sarahmaeve/signatory/internal/signal/git"
	"github.com/sarahmaeve/signatory/internal/store"
)

// TestAnalyze_GitCollector_FunctionalTwoSourcesLand exercises the
// new per-target collector-assembly path end-to-end: AnalyzeCmd.Run
// drives a two-collector list (mocked github + real git) against a
// fixture clone, and both source streams must land in the store on
// a single --refresh invocation.
//
// Catches regressions in the collector-loop side of
// cmd/signatory/analyze.go:
//
//   - A mid-loop failure in one collector must not abort the other.
//   - Signals from multiple sources must all be persisted under the
//     same entity UUID (not fragmented into per-source entities).
//   - The "signals from source X" invariant: emitted signals must
//     carry Source == collector.Name() and the latest-signal query
//     must return rows from every source that ran.
//
// Note on scope: this test uses Globals.Collectors to inject a
// mock github collector rather than going through collectorsFor.
// collectorsFor's --path / --clone / origin-validation behaviors
// are unit-tested in collectors_test.go; they do not need to be
// re-exercised through AnalyzeCmd.Run. This test's concern is the
// collector-loop.
func TestAnalyze_GitCollector_FunctionalTwoSourcesLand(t *testing.T) {
	t.Parallel()

	// --- Set up a minimal git clone the git collector can read. ---
	clonePath := initGitFixtureRepo(t, "https://github.com/acme/widget")

	// --- Build the mock github collector emitting one signal. ---
	mockGH := &mockCollector{
		name: "github",
		signals: []profile.Signal{
			{
				Type:              "stars",
				Group:             profile.SignalGroupCriticality,
				Source:            "github",
				ForgeryResistance: profile.ForgeryMediumDeclining,
				Value:             json.RawMessage(`{"count": 42}`),
			},
		},
	}

	// --- Real git collector against the fixture clone. ---
	git := gitcollector.NewCollector(clonePath)

	globals := testGlobals(t, mockGH, git)

	// --- Run analyze --refresh against the target. ---
	//
	// The target URL must resolve to the same canonical URI
	// the fixture clone's origin claims, so entity auto-creation
	// lines up with what collectorsFor would produce in the
	// production path. Ecosystem test tolerance: as long as the
	// two match, the entity UUID is stable across collector
	// emission and later query.
	cmd := &AnalyzeCmd{
		Target:  "https://github.com/acme/widget",
		Refresh: true,
	}
	require.NoError(t, cmd.Run(globals))

	// --- Verify both sources landed in the store. ---
	s, err := store.OpenSQLite(t.Context(), globals.DBPath)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck // close on test exit

	entity, err := s.FindEntityByURI(t.Context(), "repo:github/acme/widget")
	require.NoError(t, err, "entity must have been created during refresh")

	signals, err := s.GetLatestSignals(t.Context(), entity.ID)
	require.NoError(t, err)

	// Partition signals by source; assert both sources contributed.
	bySource := map[string]int{}
	for _, sig := range signals {
		bySource[sig.Source]++
	}
	assert.Greater(t, bySource["github"], 0, "github signals should be present")
	assert.Greater(t, bySource["git"], 0, "git signals should be present")

	// More precise: the git collector emits the seven v0.1 signals
	// (or their absence records if the repo shape doesn't fit).
	// Either a signal or an absence for each type should appear,
	// sourced "git". A complete miss means the collector didn't run.
	expectGitTypes := []string{
		"per_developer_commit_signing_ratio",
		"web_flow_signing_ratio",
		"tag_signing_status",
		"identity_graph_depth",
		"identity_domain_consistency",
		"effective_maintainer_concentration",
		"first_commit_date",
	}
	gitTypesPresent := map[string]bool{}
	for _, sig := range signals {
		if sig.Source != "git" {
			continue
		}
		// Absence records carry Type="absence:<name>"; a real signal
		// carries Type="<name>". Normalize to the underlying name.
		t := sig.Type
		if len(t) > len("absence:") && t[:len("absence:")] == "absence:" {
			t = t[len("absence:"):]
		}
		gitTypesPresent[t] = true
	}
	for _, typeName := range expectGitTypes {
		assert.True(t, gitTypesPresent[typeName],
			"git-source signal %q missing (as signal or absence) — git collector may not have run", typeName)
	}

	// And the mocked github signal must also be there — one
	// collector failing must not mask the other.
	var gotStars bool
	for _, sig := range signals {
		if sig.Type == "stars" && sig.Source == "github" {
			gotStars = true
			break
		}
	}
	assert.True(t, gotStars, "mock github stars signal must have landed")
}

// TestAnalyze_GitCollector_NoPath_WithInjectedCollectors verifies
// that when the test injects collectors via Globals.Collectors,
// the --path / --clone validation in collectorsFor is correctly
// bypassed. A direct-injection path is the main test-injection
// mechanism; it must not be broken by future changes to
// collectorsFor's contract.
func TestAnalyze_GitCollector_NoPath_WithInjectedCollectors(t *testing.T) {
	t.Parallel()

	mock := newMockCollector()
	globals := testGlobals(t, mock)

	cmd := &AnalyzeCmd{
		Target:  "https://github.com/acme/widget",
		Refresh: true,
		// Path deliberately empty; injected collectors should bypass
		// collectorsFor's "--path required" check.
	}
	require.NoError(t, cmd.Run(globals))

	s, err := store.OpenSQLite(t.Context(), globals.DBPath)
	require.NoError(t, err)
	defer s.Close() //nolint:errcheck

	entity, err := s.FindEntityByURI(t.Context(), "repo:github/acme/widget")
	require.NoError(t, err)

	signals, err := s.GetLatestSignals(t.Context(), entity.ID)
	require.NoError(t, err)
	assert.NotEmpty(t, signals, "mock signals should land even without --path")
}

// TestAnalyze_GitCollector_MissingPath_ProductionPath verifies the
// inverse: when Globals.Collectors is empty, collectorsFor runs
// and enforces the --path requirement, failing the run.
//
// This is the "fail loudly" side of the v0.1 Invariant 2 contract:
// without --path and without --clone, analyze of a git-hosted
// entity must error cleanly.
func TestAnalyze_GitCollector_MissingPath_ProductionPath(t *testing.T) {
	t.Parallel()

	globals := testGlobals(t) // no collectors injected → collectorsFor runs

	cmd := &AnalyzeCmd{
		Target:  "https://github.com/acme/widget",
		Refresh: true,
		// Path and Clone both zero-value → collectorsFor returns
		// ErrCloneRequired.
	}
	err := cmd.Run(globals)
	require.Error(t, err, "analyze with no --path should fail loudly")
	assert.ErrorIs(t, err, ErrCloneRequired,
		"error should be the ErrCloneRequired sentinel")
}

// initGitFixtureRepo creates a minimal git clone with one commit
// and a synthetic `origin` matching originURL. Used by
// analyze-level functional tests that need a real clone the git
// collector can read. Mirrors internal/signal/git/collector_test.go's
// initRepo helper; we duplicate here because that helper is
// package-private to the git collector.
func initGitFixtureRepo(t *testing.T, originURL string) string {
	t.Helper()
	dir := t.TempDir()
	runGitInFunctional(t, dir, "init", "-b", "main", "-q")
	runGitInFunctional(t, dir, "config", "user.email", "test@example.invalid")
	runGitInFunctional(t, dir, "config", "user.name", "Test")
	runGitInFunctional(t, dir, "config", "commit.gpgsign", "false")
	runGitInFunctional(t, dir, "config", "tag.gpgSign", "false")
	runGitInFunctional(t, dir, "commit", "--allow-empty", "-m", "seed")
	if originURL != "" {
		runGitInFunctional(t, dir, "remote", "add", "origin", originURL)
	}
	return dir
}

// runGitInFunctional is a local copy of the cross-package git
// helper. We can't import test-only symbols from
// internal/signal/git/, so this small helper lives here.
func runGitInFunctional(t *testing.T, repo string, args ...string) {
	t.Helper()
	full := append([]string{"-C", repo}, args...)
	//nolint:gosec // G204: test helper; binary is "git" literal
	cmd := exec.Command("git", full...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %v in %s: %v: %s", args, repo, err, stderr.String())
	}
}
