package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/sarahmaeve/signatory/internal/gitenv"
	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for the clone-isolation defense (Fix #2 from
// design/analysis/cve-2025-41390.md). The gitenv `-c` override prefix
// (Fix #1) neutralizes every dangerous directive in our enumerated
// catalog at every invocation. Fix #2 layers on top of that with the
// structural defense TruffleHog adopted post-CVE: clone the operator-
// supplied directory to a fresh tempdir before any collector touches
// it. The resulting clone has a minted `.git/config` (only `core` +
// `[remote "origin"]` defaults) and only the default sample-only
// `.git/hooks/` — no copy of the operator's potentially attacker-
// supplied versions.
//
// Test strategy. The `-c` catalog already covers every dangerous
// directive our command set reaches today, so a "did the shim run?"
// reproduction can't distinguish Fix #1 from Fix #2. Instead these
// tests assert the STRUCTURAL property: the path collectors operate
// on differs from the operator's path, has a minted config, and lacks
// the operator's executable hooks. Combined with Fix #1's per-call
// override, defense in depth.

// TestCloneToTempIsolated_StripsAttackerControlledConfig is the
// load-bearing structural test. Plants a marker config key that
// CANNOT trigger exec (so it's safe even without Fix #1) but CAN
// be queried via `git config --get`, plus an executable post-checkout
// hook. After cloneToTempIsolated, assert neither the marker nor the
// hook carry over to the isolated path.
//
// The marker-key approach is the right shape because:
//
//   - It exercises the same code path an attacker would use to ship
//     ANY directive, dangerous or not. A future git release that
//     adds a new exec-shaped directive name still gets neutralized
//     by the structural clone — Fix #1 would need a catalog update,
//     but Fix #2 is correct by construction.
//   - It doesn't depend on host gpg / signature verification, so
//     the test is hermetic.
//   - It matches the "deny by default" framing: the operator can put
//     ANYTHING in their .git/config and collectors won't see it.
//
// Revert proof: change cloneToTempIsolated to return srcPath
// unchanged (or short-circuit when srcPath has any specific shape);
// every assertion below fails because the marker key is reachable
// via git-config or the hook file is present.
func TestCloneToTempIsolated_StripsAttackerControlledConfig(t *testing.T) {
	t.Parallel()

	src := initSourceRepo(t, "https://github.com/legit/repo")

	// Custom config key that proves "the operator's config did NOT
	// carry over." Not in Fix #1's catalog because it's not exec-
	// shaped — it's just a marker for the structural test.
	mustRunGitInTest(t, src, "config", "fix2.marker", "leaked")
	cfgPath := filepath.Join(src, ".git", "config")
	cfg, err := os.ReadFile(cfgPath)
	require.NoError(t, err)
	cfg = fmt.Appendf(cfg, "\n[fix2]\n\tcustom = sneaky\n")
	require.NoError(t, os.WriteFile(cfgPath, cfg, 0o644))

	// Plant an executable post-checkout hook in the source's
	// .git/hooks/. `git clone <local> <dest>` does NOT copy
	// .git/hooks/ — destination gets fresh sample-only hooks. The
	// hook never runs (clone uses dest's hooks, which are samples)
	// and the file never appears in dest at all.
	hookPath := filepath.Join(src, ".git", "hooks", "post-checkout")
	require.NoError(t, os.WriteFile(hookPath,
		[]byte("#!/bin/sh\necho clone-time-attack\nexit 0\n"), 0o755))

	tempPath, cleanup, err := cloneToTempIsolated(t.Context(), src)
	require.NoError(t, err, "cloneToTempIsolated must succeed against a valid local clone")
	require.NotEmpty(t, tempPath, "tempPath must be non-empty on success")
	require.NotNil(t, cleanup, "cleanup must be non-nil on success")
	t.Cleanup(func() { _ = cleanup() })

	// Distinct path: the structural property Fix #2 exists to
	// guarantee.
	assert.NotEqual(t, src, tempPath,
		"cloneToTempIsolated must return a distinct path from the operator's input")

	// Sanity: tempPath has a .git/ — it really is a clone, not a
	// random directory.
	gitInfo, err := os.Stat(filepath.Join(tempPath, ".git"))
	require.NoError(t, err, "tempPath must contain a .git directory")
	assert.True(t, gitInfo.IsDir(), ".git in tempPath must be a directory (not a worktree-link file)")

	// Marker config keys must NOT carry over. `git config --get`
	// exits 1 when the key is absent — non-nil err is the green
	// signal here.
	for _, key := range []string{"fix2.marker", "fix2.custom"} {
		err := runGitConfigGet(t, tempPath, key)
		assert.Errorf(t, err,
			"key %q must not carry over to the isolated clone — fresh .git/config has only minted defaults", key)
	}

	// The operator's executable post-checkout hook must NOT appear
	// in the destination's .git/hooks/. Only the .sample files git
	// installs from its templates should be present.
	_, err = os.Stat(filepath.Join(tempPath, ".git", "hooks", "post-checkout"))
	assert.Truef(t, os.IsNotExist(err),
		"post-checkout hook must NOT carry over to the isolated clone (got stat err=%v)", err)
}

// TestCloneToTempIsolated_CleanupRemovesTempdir locks the cleanup
// callback contract: invoking it must remove the tempdir from disk so
// production /tmp doesn't accumulate per-analyze leakage.
//
// Revert proof: change cleanup to a no-op; this test fails because
// tempPath still exists after cleanup.
func TestCloneToTempIsolated_CleanupRemovesTempdir(t *testing.T) {
	t.Parallel()

	src := initSourceRepo(t, "")

	tempPath, cleanup, err := cloneToTempIsolated(t.Context(), src)
	require.NoError(t, err)
	require.DirExists(t, tempPath,
		"tempPath must exist before cleanup")

	require.NoError(t, cleanup(),
		"cleanup must succeed on a freshly-created tempdir")

	_, err = os.Stat(tempPath)
	assert.Truef(t, os.IsNotExist(err),
		"cleanup() must remove the tempdir; got stat err=%v", err)
}

// TestCloneToTempIsolated_PreservesOperatorOrigin pins the
// behavioral contract for --allow-fetch users (and any other future
// caller that relies on `git fetch origin` reaching the real
// upstream). After cloneToTempIsolated, the temp clone's origin
// must point at the SAME URL the operator's clone's origin pointed
// at — typically github — NOT at the operator's filesystem path
// (which is what `git clone <local> <dest>` defaults to).
//
// Without this, source-evolution's --allow-fetch path degrades:
// it would `git fetch origin` against the operator's local path,
// which is the SAME object store the temp clone hardlinked from,
// so any SHA missing from the operator's clone is also missing
// from the fetch target. Recovery fails silently.
//
// Revert proof: remove the origin-rewriting subprocess in
// cloneToTempIsolated; this test fails because the temp clone's
// origin equals the operator's path instead of the github URL.
func TestCloneToTempIsolated_PreservesOperatorOrigin(t *testing.T) {
	t.Parallel()

	const expectedOrigin = "https://github.com/legit/repo"
	src := initSourceRepo(t, expectedOrigin)

	tempPath, cleanup, err := cloneToTempIsolated(t.Context(), src)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cleanup() })

	// Read the temp clone's origin URL via plumbing — same shape
	// validateCloneOrigin uses in production.
	cmd := gitenv.NewCmd(t.Context(), "-C", tempPath, "remote", "get-url", "origin")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	require.NoErrorf(t, cmd.Run(),
		"reading temp clone's origin must succeed; stderr=%s", stderr.String())

	got := bytes.TrimSpace(stdout.Bytes())
	assert.Equalf(t, expectedOrigin, string(got),
		"temp clone's origin must be rewritten back to operator's origin URL "+
			"(typically github); leaving it as the operator's local path silently "+
			"breaks --allow-fetch recovery (see source-evolution BlobStreamer)")
}

// TestCloneToTempIsolated_NoOriginInSource_StillSucceeds covers the
// edge case the production validation chain rules out but
// cloneToTempIsolated must handle gracefully on its own: an
// origin-less source repo (test fixtures sometimes init without
// `git remote add origin`). The temp clone keeps the local-path
// origin git's clone wrote, and the helper does not error.
//
// Revert proof: change cloneToTempIsolated to fail-loud on missing
// origin; this test fails because the helper returns an error
// instead of (tempPath, cleanup, nil).
func TestCloneToTempIsolated_NoOriginInSource_StillSucceeds(t *testing.T) {
	t.Parallel()

	src := initSourceRepo(t, "") // no `git remote add origin`

	tempPath, cleanup, err := cloneToTempIsolated(t.Context(), src)
	require.NoError(t, err,
		"cloneToTempIsolated must tolerate an origin-less source repo")
	require.NotEmpty(t, tempPath)
	require.NotNil(t, cleanup)
	t.Cleanup(func() { _ = cleanup() })
}

// TestCloneToTempIsolated_PropagatesGitFailure pins error handling.
// Pointing at a non-existent source must return an error and not
// leak a partially-constructed tempdir. This is the failure mode for
// upstream-validation-skipped callers — the broader resolveClonePath
// wraps cloneToTempIsolated with origin/shape checks, but the helper
// itself must be safe to call standalone.
//
// Revert proof: change cloneToTempIsolated to return nil on git
// errors; this test fails because no error surfaces.
func TestCloneToTempIsolated_PropagatesGitFailure(t *testing.T) {
	t.Parallel()

	bogus := filepath.Join(t.TempDir(), "does-not-exist")

	tempPath, cleanup, err := cloneToTempIsolated(t.Context(), bogus)
	require.Error(t, err,
		"cloneToTempIsolated against a non-existent source must error")
	assert.Empty(t, tempPath,
		"tempPath must be empty on error so the caller can't accidentally use it")
	assert.Nil(t, cleanup,
		"cleanup must be nil on error so the caller can't double-call it")
}

// runGitConfigGet runs `git -C path config --get key` and returns the
// resulting error (nil if the key exists, non-nil if it doesn't).
// Helper for the marker-absence assertions above.
func runGitConfigGet(t *testing.T, path, key string) error {
	t.Helper()
	cmd := gitenv.NewCmd(t.Context(), "-C", path, "config", "--get", key)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	return cmd.Run()
}

// TestResolveClonePath_PathOnly_ReturnsIsolatedTempPath proves the
// integration: resolveClonePath(--path) returns the tempdir from
// cloneToTempIsolated, NOT the operator's input path. This is the
// orchestrator-level shape that drives the file-vector defense for
// the read-only --path flow (collectCommitSigning, repofiles,
// exfilwatch, source-evolution all see the sanitized path).
//
// Revert proof: change resolveClonePath to skip the
// cloneToTempIsolated step; this test fails because the returned
// path equals the operator's path.
func TestResolveClonePath_PathOnly_ReturnsIsolatedTempPath(t *testing.T) {
	t.Parallel()

	src := initSourceRepo(t, "https://github.com/alecthomas/kong")
	mustRunGitInTest(t, src, "config", "fix2.path-only", "leaked")

	entity := &profile.Entity{
		ID:           "e1",
		CanonicalURI: "repo:github/alecthomas/kong",
		URL:          "https://github.com/alecthomas/kong",
	}

	var cleanups []func() error
	t.Cleanup(func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			_ = cleanups[i]()
		}
	})

	got, err := resolveClonePath(t.Context(), entity, CollectOpts{
		Path:     src,
		Cleanups: &cleanups,
	})
	require.NoError(t, err)
	assert.NotEqual(t, src, got,
		"resolveClonePath must return the isolated tempdir, not the operator's --path")

	// Marker key must not appear in the isolated clone.
	require.Error(t, runGitConfigGet(t, got, "fix2.path-only"),
		"the operator's .git/config must NOT reach collectors via the resolveClonePath return value")

	// At least one cleanup got registered (the tempdir-removal).
	require.NotEmpty(t, cleanups,
		"resolveClonePath must register the tempdir-cleanup with opts.Cleanups when isolation runs")
}

// TestResolveClonePath_CloneAndPath_ReturnsIsolatedTempPath covers the
// `--path --clone` flow: the operator's clone is refreshed (existing
// behavior), then cloneToTempIsolated produces the path collectors
// see. Same structural property — distinct path, fresh config —
// as the --path-only case.
//
// Revert proof: same as TestResolveClonePath_PathOnly — the test
// fails if resolveClonePath returns the operator's path on the
// --clone branch too.
func TestResolveClonePath_CloneAndPath_ReturnsIsolatedTempPath(t *testing.T) {
	t.Parallel()

	srcRemote := initSourceRepo(t, "")
	dst := filepath.Join(t.TempDir(), "operator-clone")

	entity := &profile.Entity{
		ID:           "e1",
		CanonicalURI: "repo:github/alecthomas/kong",
		URL:          srcRemote, // file path is acceptable to safeGitCloneURL
	}

	var cleanups []func() error
	t.Cleanup(func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			_ = cleanups[i]()
		}
	})

	got, err := resolveClonePath(t.Context(), entity, CollectOpts{
		Path:     dst,
		Clone:    true,
		Cleanups: &cleanups,
	})
	require.NoError(t, err)
	assert.NotEqual(t, dst, got,
		"resolveClonePath --clone must return the isolated tempdir, not the operator's --path destination")
	require.DirExists(t, got, "isolated tempdir must exist on disk")
	require.DirExists(t, dst, "operator's --path destination must still exist (clone went there)")
	require.NotEmpty(t, cleanups,
		"resolveClonePath must register cleanup on the --clone branch too")
}
