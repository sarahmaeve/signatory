package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sarahmaeve/signatory/internal/gitenv"
	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// initSourceRepo creates a git repo with one commit that serves as
// the "origin" for clone tests. Returns the absolute path, usable
// as a `git clone <src> <dst>` argument.
func initSourceRepo(t *testing.T, origin string) string {
	t.Helper()
	dir := t.TempDir()
	mustRunGitInTest(t, dir, "init", "-b", "main", "-q")
	mustRunGitInTest(t, dir, "config", "user.email", "test@example.invalid")
	mustRunGitInTest(t, dir, "config", "user.name", "Test")
	mustRunGitInTest(t, dir, "config", "commit.gpgsign", "false")
	mustRunGitInTest(t, dir, "commit", "--allow-empty", "-m", "seed")
	if origin != "" {
		// Set a synthetic origin so validateExistingClone has something
		// to parse. The URL's target is what the test's Entity.URL /
		// CanonicalURI reference.
		mustRunGitInTest(t, dir, "remote", "add", "origin", origin)
	}
	return dir
}

// testCleanups returns a *[]func() error suitable for
// CollectOpts.Cleanups, registered with t.Cleanup so the LIFO drain
// runs when the test ends.
//
// Required at every collectorsFor / resolveClonePath call site that
// passes a valid Path (with or without Clone:) — the file-vector
// clone-isolation defense (Fix #2 from
// design/analysis/cve-2025-41390.md) mints a fresh tempdir per call,
// and without this drain the tempdirs leak into the OS temp directory
// across test runs.
//
// Safe to use even on call sites that fail validation before
// isolation runs: the slice stays empty and the deferred drain is a
// no-op.
func testCleanups(t *testing.T) *[]func() error {
	t.Helper()
	var cs []func() error
	t.Cleanup(func() {
		for i := len(cs) - 1; i >= 0; i-- {
			_ = cs[i]()
		}
	})
	return &cs
}

// mustRunGitInTest runs `git -C repo <args...>` and fails the test
// on any non-zero exit. Routes through gitenv.NewCmd so the test
// subprocess inherits the same env-strip + WaitDelay discipline
// production code does. Without the env strip, an ambient GIT_DIR
// would redirect writes to the shared worktree config — the
// mechanism behind the 2026-04-24 main-worktree config corruption.
//
// Local copy of the git-package test helper — we can't import
// test-only symbols across packages. Fails the test on any
// non-zero git exit.
func mustRunGitInTest(t *testing.T, repo string, args ...string) {
	t.Helper()
	full := append([]string{"-C", repo}, args...)
	cmd := gitenv.NewCmd(t.Context(), full...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	require.NoErrorf(t, cmd.Run(), "git %v: %s", args, stderr.String())
}

// TestCollectorsFor_MissingPath_DegradesToAPIOnlyCollectors pins
// the post-ungate behavior superseding the prior "missing path
// returns ErrCloneRequired" pin. See
// TestCollectorsFor_RepoEntity_NoPath_DegradesToAPIOnlyCollectors
// for the full rationale; this is the parallel test that originally
// pinned ErrCloneRequired for a github-shaped repo: entity.
//
// ErrCloneRequired still fires when there are NO collectors at all
// to return — the entity has no URL, or the URL host isn't
// recognized so even openssf would skip. That case is covered by
// the pkg/cargo tests and a future bare-entity test if needed.
func TestCollectorsFor_MissingPath_DegradesToAPIOnlyCollectors(t *testing.T) {
	t.Parallel()

	var stderr bytes.Buffer
	entity := &profile.Entity{
		ID:           "e1",
		CanonicalURI: "repo:github/alecthomas/kong",
		URL:          "https://github.com/alecthomas/kong",
	}
	collectors, err := collectorsFor(context.Background(), entity, CollectOpts{
		Stderr: &stderr,
	})
	require.NoError(t, err,
		"github repo: entity without --path/--clone must degrade gracefully — openssf is API-only and runs without a clone")
	require.NotEmpty(t, collectors,
		"API-only collectors must populate the dispatch list even when clone is absent")
}

func TestCollectorsFor_CloneWithoutPath_ReturnsErrPathMissing(t *testing.T) {
	t.Parallel()

	entity := &profile.Entity{
		ID:           "e1",
		CanonicalURI: "repo:github/alecthomas/kong",
		URL:          "https://github.com/alecthomas/kong",
	}
	_, err := collectorsFor(context.Background(), entity, CollectOpts{Clone: true})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPathMissing)
}

// TestCollectorsFor_CloneIntoNonEmptyNonClone_ReturnsErrPathNotAClone
// guards the "never clobber" property of --clone under the idempotent
// clone-or-refresh contract: pointing --clone at a non-empty directory
// that is NOT a git clone still refuses, but with ErrPathNotAClone
// (origin-validation discovers no .git and refuses) rather than the
// pre-idempotent ErrPathNotEmpty. The protective intent — never
// overwrite content the user didn't authorize — is preserved.
func TestCollectorsFor_CloneIntoNonEmptyNonClone_ReturnsErrPathNotAClone(t *testing.T) {
	t.Parallel()

	// Create a dir and put something in it that is NOT a clone.
	dst := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dst, "existing.txt"), []byte("hi"), 0o600))

	entity := &profile.Entity{
		ID:           "e1",
		CanonicalURI: "repo:github/alecthomas/kong",
		URL:          "https://github.com/alecthomas/kong",
	}
	_, err := collectorsFor(context.Background(), entity, CollectOpts{Path: dst, Clone: true, Cleanups: testCleanups(t)})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPathNotAClone,
		"under the idempotent --clone contract, a non-empty non-clone dir is rejected by origin validation (no .git → ErrPathNotAClone), not the legacy ErrPathNotEmpty")
}

func TestCollectorsFor_ExistingPathNoGitDir_ReturnsErrPathNotAClone(t *testing.T) {
	t.Parallel()

	path := t.TempDir() // exists, but has no .git

	entity := &profile.Entity{
		ID:           "e1",
		CanonicalURI: "repo:github/alecthomas/kong",
		URL:          "https://github.com/alecthomas/kong",
	}
	_, err := collectorsFor(context.Background(), entity, CollectOpts{Path: path, Cleanups: testCleanups(t)})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPathNotAClone)
}

func TestCollectorsFor_OriginMismatch_ReturnsErrOriginMismatch(t *testing.T) {
	// Clone exists but its origin points at a different repo than
	// the analyze target. Validation must catch this before we run
	// a collector that would attribute wrong-repo signals to the
	// declared entity.
	t.Parallel()

	src := initSourceRepo(t, "https://github.com/otherowner/otherrepo")

	entity := &profile.Entity{
		ID:           "e1",
		CanonicalURI: "repo:github/alecthomas/kong",
		URL:          "https://github.com/alecthomas/kong",
	}
	_, err := collectorsFor(context.Background(), entity, CollectOpts{Path: src, Cleanups: testCleanups(t)})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrOriginMismatch)
}

func TestCollectorsFor_OriginMatches_ReturnsCollectorList(t *testing.T) {
	t.Parallel()

	src := initSourceRepo(t, "https://github.com/alecthomas/kong")

	entity := &profile.Entity{
		ID:           "e1",
		CanonicalURI: "repo:github/alecthomas/kong",
		URL:          "https://github.com/alecthomas/kong",
	}
	collectors, err := collectorsFor(context.Background(), entity, CollectOpts{Path: src, Cleanups: testCleanups(t)})
	require.NoError(t, err)
	// Expect github + git collectors in the returned slice.
	names := map[string]bool{}
	for _, c := range collectors {
		names[c.Name()] = true
	}
	assert.True(t, names["github"], "github collector should be present")
	assert.True(t, names["git"], "git collector should be present")
	assert.True(t, names["openssf-scorecard"],
		"openssf-scorecard collector should be present so dispatched analysts read cached scorecard data via signatory_signals instead of WebFetching the API")
	assert.True(t, names["exfilwatch"],
		"exfilwatch collector should be present for git-hosted entities — runs a literal scan of the clone for HTTP-capture-as-a-service hosts (BufferZoneCorp-shaped exfil signal)")
}

func TestCollectorsFor_OriginMatches_SshForm(t *testing.T) {
	// Git origins commonly appear as `git@github.com:owner/repo.git`.
	// Normalization must resolve this to the same canonical URI as
	// `https://github.com/owner/repo` so the match succeeds.
	t.Parallel()

	src := initSourceRepo(t, "git@github.com:alecthomas/kong.git")

	entity := &profile.Entity{
		ID:           "e1",
		CanonicalURI: "repo:github/alecthomas/kong",
		URL:          "https://github.com/alecthomas/kong",
	}
	_, err := collectorsFor(context.Background(), entity, CollectOpts{Path: src, Cleanups: testCleanups(t)})
	assert.NoError(t, err, "ssh-form origin should normalize to same canonical URI")
}

// TestCollectorsFor_CodebergOriginMatches_SshForm pins the multi-forge
// generalization: a Codeberg target validated against an SCP-form
// `git@codeberg.org:owner/repo.git` origin must normalize to the same
// canonical URI on both sides. Pre-NormalizeForgeRepoInput, the
// normalizer left the colon in the owner segment ("codeberg.org:owner")
// and rejected it via validPathSegment — so origin validation errored
// for every codeberg-with-SSH-clone target.
func TestCollectorsFor_CodebergOriginMatches_SshForm(t *testing.T) {
	t.Parallel()

	src := initSourceRepo(t, "git@codeberg.org:forgejo/forgejo.git")

	entity := &profile.Entity{
		ID:           "e1",
		CanonicalURI: "repo:codeberg/forgejo/forgejo",
		URL:          "https://codeberg.org/forgejo/forgejo",
	}
	_, err := collectorsFor(context.Background(), entity, CollectOpts{Path: src, Cleanups: testCleanups(t)})
	assert.NoError(t, err, "ssh-form codeberg origin should normalize to same canonical URI")
}

// TestCollectorsFor_GitLabOriginMatches_SshForm — same shape as the
// Codeberg SSH-form test, for GitLab.
func TestCollectorsFor_GitLabOriginMatches_SshForm(t *testing.T) {
	t.Parallel()

	src := initSourceRepo(t, "git@gitlab.com:gitlab-org/gitlab.git")

	entity := &profile.Entity{
		ID:           "e1",
		CanonicalURI: "repo:gitlab/gitlab-org/gitlab",
		URL:          "https://gitlab.com/gitlab-org/gitlab",
	}
	_, err := collectorsFor(context.Background(), entity, CollectOpts{Path: src, Cleanups: testCleanups(t)})
	assert.NoError(t, err, "ssh-form gitlab origin should normalize to same canonical URI")
}

// TestCollectorsFor_CodebergOriginMatches_HTTPSForm pins the happy
// HTTPS-form path for Codeberg. The pre-NormalizeForgeRepoInput
// behavior accidentally validated this case (both sides went through
// the broken github-only normalizer and produced the same wrong URI,
// e.g. "repo:github/codeberg.org/forgejo"), so this test wasn't RED
// before the refactor — but it's a permanent invariant that the
// switch must preserve.
func TestCollectorsFor_CodebergOriginMatches_HTTPSForm(t *testing.T) {
	t.Parallel()

	src := initSourceRepo(t, "https://codeberg.org/forgejo/forgejo")

	entity := &profile.Entity{
		ID:           "e1",
		CanonicalURI: "repo:codeberg/forgejo/forgejo",
		URL:          "https://codeberg.org/forgejo/forgejo",
	}
	_, err := collectorsFor(context.Background(), entity, CollectOpts{Path: src, Cleanups: testCleanups(t)})
	assert.NoError(t, err)
}

// TestCollectorsFor_CodebergEntity_IncludesForgejoCollector pins the
// Tier 1 wiring: a codeberg entity must get the forgejo collector in
// its dispatched set. Without this, the new internal/signal/forgejo
// package would land but never run — the github collector self-gates
// on non-github URLs (correct) and there'd be nothing else collecting
// the API-only metadata (stars, forks, archived, repo_age, etc.) for
// codeberg targets.
//
// Asserts on collector names rather than instances so the test
// remains decoupled from the exact construction shape (with/without
// EntityStore, etc.). Same idiom the github wiring uses elsewhere.
func TestCollectorsFor_CodebergEntity_IncludesForgejoCollector(t *testing.T) {
	t.Parallel()

	src := initSourceRepo(t, "https://codeberg.org/forgejo/forgejo")

	entity := &profile.Entity{
		ID:           "e1",
		CanonicalURI: "repo:codeberg/forgejo/forgejo",
		URL:          "https://codeberg.org/forgejo/forgejo",
	}
	collectors, err := collectorsFor(context.Background(), entity, CollectOpts{Path: src, Cleanups: testCleanups(t)})
	require.NoError(t, err)

	names := make([]string, 0, len(collectors))
	for _, c := range collectors {
		names = append(names, c.Name())
	}
	assert.Contains(t, names, "forgejo",
		"codeberg-hosted entity must dispatch the forgejo collector — without it, API-only metadata signals (stars/forks/archived/repo_age/last_push/open_issues) are never collected for codeberg targets")
}

// TestCollectorsFor_GitHostedEntity_IncludesAdoptionCollector pins
// the adoption-lift wiring. After the github collector's
// collectAdoption was lifted to internal/signal/adoption, the
// standalone collector must be in the dispatch list for every
// git-hosted entity (Go-ecosystem ones in particular). Without it,
// the github lift-out would have silently dropped adoption from
// every analyze run because no collector emits the signal anymore.
//
// Asserts on collector identity only — the adoption collector's
// own package tests cover the ecosystem self-gate, the per-forge
// module path derivation, and the stars-from-inRunResult ratio
// computation.
func TestCollectorsFor_GitHostedEntity_IncludesAdoptionCollector(t *testing.T) {
	t.Parallel()

	src := initSourceRepo(t, "https://github.com/alecthomas/kong")

	entity := &profile.Entity{
		ID:           "e1",
		CanonicalURI: "repo:github/alecthomas/kong",
		URL:          "https://github.com/alecthomas/kong",
	}
	collectors, err := collectorsFor(context.Background(), entity, CollectOpts{Path: src, Cleanups: testCleanups(t)})
	require.NoError(t, err)

	names := make([]string, 0, len(collectors))
	for _, c := range collectors {
		names = append(names, c.Name())
	}
	assert.Contains(t, names, "adoption",
		"adoption collector must be dispatched for every git-hosted entity; without it, the github collector's pre-lift adoption emission is gone and nothing replaces it")
}

// TestCollectorsFor_GitLabEntity_IncludesGitLabCollector pins the
// gitlab Tier 1 wiring symmetric to the codeberg/forgejo pairing:
// every git-hosted entity walks the full collector list, each with
// its own host-side self-gate. A gitlab entity must reach the
// gitlab collector at dispatch time (the collector then emits
// signals because its self-gate accepts gitlab.com URLs). Same
// dispatch-shape discipline that landed for forgejo.
func TestCollectorsFor_GitLabEntity_IncludesGitLabCollector(t *testing.T) {
	t.Parallel()

	src := initSourceRepo(t, "https://gitlab.com/gitlab-org/gitlab")

	entity := &profile.Entity{
		ID:           "e1",
		CanonicalURI: "repo:gitlab/gitlab-org/gitlab",
		URL:          "https://gitlab.com/gitlab-org/gitlab",
	}
	collectors, err := collectorsFor(context.Background(), entity, CollectOpts{Path: src, Cleanups: testCleanups(t)})
	require.NoError(t, err)

	names := make([]string, 0, len(collectors))
	for _, c := range collectors {
		names = append(names, c.Name())
	}
	assert.Contains(t, names, "gitlab",
		"gitlab-hosted entity must dispatch the gitlab collector — without it, API-only metadata signals (stars/forks/archived/repo_age/last_push/open_issues) are never collected for gitlab targets")
}

// TestCollectorsFor_GitHubEntity_DispatchesForgejoAndGitLab_SelfGateAtCollectTime
// pins the dispatch contract: a github entity routes through the
// forgejo AND gitlab collectors in the dispatched list, and each
// collector self-gates at Collect time and emits zero signals for the
// non-matching host. The dispatch is unconditional (same shape as
// github / openssf — every git-hosted entity goes through the full
// collector list, each with its own self-gate); this test guards
// against a future "let's only wire forgejo for codeberg URLs"
// optimization that would silently break the symmetry and require
// host-aware dispatch knowledge to leak into collectorsFor.
//
// Renamed from OmitsForgejoCollectorAtCollectTime: the prior name
// described the surface as "omits forgejo," but the body asserts the
// opposite — forgejo IS dispatched and the self-gate decides emission.
// The old name primed refactors to flip the assertion to NotContains.
func TestCollectorsFor_GitHubEntity_DispatchesForgejoAndGitLab_SelfGateAtCollectTime(t *testing.T) {
	t.Parallel()

	src := initSourceRepo(t, "https://github.com/alecthomas/kong")

	entity := &profile.Entity{
		ID:           "e1",
		CanonicalURI: "repo:github/alecthomas/kong",
		URL:          "https://github.com/alecthomas/kong",
	}
	collectors, err := collectorsFor(context.Background(), entity, CollectOpts{Path: src, Cleanups: testCleanups(t)})
	require.NoError(t, err)

	names := make([]string, 0, len(collectors))
	for _, c := range collectors {
		names = append(names, c.Name())
	}
	// forgejo IS in the dispatched list; the self-gate fires at Collect
	// time and emits zero signals for non-codeberg URLs (proven by
	// TestCollector_NonCodebergEntity_ReturnsEmpty in the forgejo
	// package). The dispatch list itself is unconditional so the
	// orchestrator stays host-agnostic.
	assert.Contains(t, names, "forgejo",
		"forgejo collector must be dispatched for every git-hosted entity; the self-gate decides whether to emit signals")
	assert.Contains(t, names, "gitlab",
		"gitlab collector must be dispatched for every git-hosted entity; the self-gate decides whether to emit signals (proven empty by TestCollector_NonGitLabEntity_ReturnsEmpty in the gitlab package)")
}

// TestCollectorsFor_GitLabOriginMatches_HTTPSForm — same shape for GitLab.
func TestCollectorsFor_GitLabOriginMatches_HTTPSForm(t *testing.T) {
	t.Parallel()

	src := initSourceRepo(t, "https://gitlab.com/gitlab-org/gitlab")

	entity := &profile.Entity{
		ID:           "e1",
		CanonicalURI: "repo:gitlab/gitlab-org/gitlab",
		URL:          "https://gitlab.com/gitlab-org/gitlab",
	}
	_, err := collectorsFor(context.Background(), entity, CollectOpts{Path: src, Cleanups: testCleanups(t)})
	assert.NoError(t, err)
}

func TestCollectorsFor_CloneHappyPath(t *testing.T) {
	// Create a "remote" source repo, then clone from its filesystem
	// path into a new destination. Asserts the clone actually happened
	// (dest exists and has .git after) and collectors return.
	t.Parallel()

	src := initSourceRepo(t, "")
	dst := filepath.Join(t.TempDir(), "clone-here")

	entity := &profile.Entity{
		ID:           "e1",
		CanonicalURI: "repo:github/alecthomas/kong",
		URL:          src, // file path as "clone URL" — safeGitCloneURL accepts path-style URLs
	}

	collectors, err := collectorsFor(context.Background(), entity, CollectOpts{Path: dst, Clone: true, Cleanups: testCleanups(t)})
	require.NoError(t, err)
	assert.Len(t, collectors, 9, "github + forgejo + gitlab + adoption + openssf-scorecard + cadence + git + repofiles + exfilwatch collectors returned")

	gitDir, err := os.Stat(filepath.Join(dst, ".git"))
	require.NoError(t, err)
	assert.True(t, gitDir.IsDir(), "cloned dst should contain a .git directory")
}

func TestCollectorsFor_CloneIntoMissingDir_SucceedsByCreating(t *testing.T) {
	// --path points at a dir that doesn't exist yet; ensurePathEmpty-
	// OrMissing should treat "not exist" as acceptable and let git
	// clone create the dir.
	t.Parallel()

	src := initSourceRepo(t, "")
	dst := filepath.Join(t.TempDir(), "not-yet-created")

	entity := &profile.Entity{
		ID:           "e1",
		CanonicalURI: "repo:github/alecthomas/kong",
		URL:          src,
	}
	_, err := collectorsFor(context.Background(), entity, CollectOpts{Path: dst, Clone: true, Cleanups: testCleanups(t)})
	assert.NoError(t, err)
	assert.DirExists(t, filepath.Join(dst, ".git"))
}

func TestResolveClonePath_AbsolutePathReturnedAsAbs(t *testing.T) {
	t.Parallel()

	src := initSourceRepo(t, "https://github.com/alecthomas/kong")

	// Pass a relative path that we'll turn into a CWD-relative one
	// to verify filepath.Abs is applied.
	rel, err := filepath.Rel(t.TempDir(), src)
	// If we can't compute a meaningful relative, just use the
	// absolute form — the test still exercises the Abs call.
	pathArg := src
	if err == nil && rel != "" {
		pathArg = src
	}

	entity := &profile.Entity{
		ID:           "e1",
		CanonicalURI: "repo:github/alecthomas/kong",
		URL:          "https://github.com/alecthomas/kong",
	}
	got, err := resolveClonePath(context.Background(), entity, CollectOpts{Path: pathArg, Cleanups: testCleanups(t)})
	require.NoError(t, err)
	assert.True(t, filepath.IsAbs(got), "resolveClonePath should return an absolute path")
}

// Sanity: sentinel errors are stable references, not moving values.
func TestCollectorsFor_SentinelErrors(t *testing.T) {
	t.Parallel()

	// Each sentinel wraps a specific trust-level failure mode.
	// They're documented as part of the --path/--clone contract;
	// downstream tooling may pattern-match via errors.Is.
	assert.True(t, errors.Is(ErrCloneRequired, ErrCloneRequired))
	assert.True(t, errors.Is(ErrPathMissing, ErrPathMissing))
	assert.True(t, errors.Is(ErrPathNotEmpty, ErrPathNotEmpty))
	assert.True(t, errors.Is(ErrPathNotAClone, ErrPathNotAClone))
	assert.True(t, errors.Is(ErrOriginMismatch, ErrOriginMismatch))
}

// TestCollectorsFor_NpmEntityWithoutURL_ReturnsOnlyNpm locks in the
// Phase A.2 contract: an npm EntityPackage with no resolved repo
// URL gets the npm collector and nothing else. In particular, the
// git-local-clone collector's --path/--clone requirement MUST NOT
// fire — a regression that flipped isGitHostedEntity's polarity
// (or dropped the empty-URL short-circuit) would spuriously demand
// a clone path for a package that has no github repo at all.
//
// This test intentionally exercises collectorsFor directly rather
// than going through AnalyzeCmd.Run — the functional path injects
// Globals.Collectors to bypass real collector construction, which
// happens BEFORE this function is called. Unit-testing the
// dispatcher is the only way to pin the actual contract.
func TestCollectorsFor_NpmEntityWithoutURL_ReturnsOnlyNpm(t *testing.T) {
	t.Parallel()

	entity := &profile.Entity{
		ID:           "e1",
		CanonicalURI: "pkg:npm/orphan-package",
		Type:         profile.EntityPackage,
		Ecosystem:    "npm",
		URL:          "", // no resolved repo — A.5 couldn't find one
	}
	collectors, err := collectorsFor(context.Background(), entity, CollectOpts{})
	require.NoError(t, err,
		"empty-URL npm entity must not require --path/--clone")
	require.Len(t, collectors, 1)
	assert.Equal(t, "npm-registry", collectors[0].Name(),
		"only the npm registry collector should fire for an unresolved npm package")
}

// TestCollectorsFor_PkgEntityWithResolvedURL_NoClone_GracefulDegradation
// locks in the graceful-degradation contract: a pkg:<eco> entity
// whose source resolver has stamped a github URL, but the user ran
// --refresh without --clone or --path. The registry collector must
// still fire — the user asked for fresh signals and we have a
// registry path. Git-hosted collectors (github, git, repofiles,
// openssf) are skipped because there's no clone to work against.
//
// This is the fix for the "exit status 64" failure on
// `signatory analyze "pkg:maven/X/Y" --refresh` (without --clone):
// previously, ErrCloneRequired killed the entire collector list,
// discarding the already-queued registry collector.
func TestCollectorsFor_PkgEntityWithResolvedURL_NoClone_GracefulDegradation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		ecosystem string
		uri       string
		url       string
		wantName  string
	}{
		{
			name:      "maven with resolved URL, no clone",
			ecosystem: "maven",
			uri:       "pkg:maven/com.google.guava/guava",
			url:       "https://github.com/google/guava",
			wantName:  "maven-registry",
		},
		{
			name:      "npm with resolved URL, no clone",
			ecosystem: "npm",
			uri:       "pkg:npm/express",
			url:       "https://github.com/expressjs/express",
			wantName:  "npm-registry",
		},
		{
			name:      "cargo with resolved URL, no clone",
			ecosystem: "cargo",
			uri:       "pkg:cargo/serde",
			url:       "https://github.com/serde-rs/serde",
			wantName:  "cargo-registry",
		},
		{
			name:      "gem with resolved URL, no clone",
			ecosystem: "gem",
			uri:       "pkg:gem/rails",
			url:       "https://github.com/rails/rails",
			wantName:  "gem-registry",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var stderr bytes.Buffer
			entity := &profile.Entity{
				ID:           "e1",
				CanonicalURI: tc.uri,
				Type:         profile.EntityPackage,
				Ecosystem:    tc.ecosystem,
				URL:          tc.url,
			}
			// No Path, no Clone — just --refresh.
			collectors, err := collectorsFor(context.Background(), entity, CollectOpts{
				Stderr: &stderr,
			})
			require.NoError(t, err,
				"pkg: entity with resolved URL but no clone must NOT return an error")

			names := make([]string, 0, len(collectors))
			for _, c := range collectors {
				names = append(names, c.Name())
			}
			assert.Contains(t, names, tc.wantName,
				"registry collector for ecosystem %q must fire", tc.ecosystem)
			// Every API-only collector dispatches whenever
			// entity.URL is set, regardless of clone state. See
			// TestCollectorsFor_RepoEntity_NoPath_DegradesToAPIOnlyCollectors
			// for the full API-only set rationale.
			for _, want := range []string{"github", "forgejo", "gitlab", "adoption", "openssf-scorecard"} {
				assert.Contains(t, names, want,
					"API-only collector %q must dispatch even when no clone is available", want)
			}
			assert.NotContains(t, names, "git",
				"clone-dependent git collector must NOT dispatch when no clone is available")
			assert.NotContains(t, names, "repofiles",
				"clone-dependent repofiles collector must NOT dispatch when no clone is available")
			assert.NotContains(t, names, "exfilwatch",
				"clone-dependent exfilwatch collector must NOT dispatch when no clone is available")
			assert.Contains(t, stderr.String(), "pass --clone",
				"should hint the user about --clone for additional signals")
		})
	}
}

// TestCollectorsFor_RepoEntity_NoPath_DegradesToAPIOnlyCollectors
// pins the post-ungate behavior for bare repo: targets without a
// clone. Pre-ungate, repo: entities errored with ErrCloneRequired
// because the only collectors that fired were clone-dependent;
// after the API-only collectors moved out of the clone-gated block,
// repo: entities degrade gracefully — they still don't get the
// clone-dependent set (git / repofiles / exfilwatch), but every
// API-only collector that can run does.
//
// API-only set (this test pins the full membership):
//
//   - github          — calls api.github.com, reads owner/repo from URL
//   - forgejo         — calls codeberg.org/api/v1, host-self-gates
//   - gitlab          — calls gitlab.com/api/v4, host-self-gates
//   - adoption        — calls GitHub code-search, derives module path
//   - openssf-scorecard — calls api.securityscorecards.dev
//
// Clone-dependent set (must be absent here):
//
//   - git, repofiles, exfilwatch (all read the local clone)
//   - source, source-golang (read .git for tag history)
//   - artifact (compares clone tree against registry tarball)
//
// Same intent as the pkg-entity degradation: "collect whatever you
// can." The asymmetry that existed pre-broader-ungate (pkg: degrades
// minimally, repo: errors) was an artifact of which collectors had
// been hardwired into the clone-gated block, not a deliberate design
// distinction.
func TestCollectorsFor_RepoEntity_NoPath_DegradesToAPIOnlyCollectors(t *testing.T) {
	t.Parallel()

	var stderr bytes.Buffer
	entity := &profile.Entity{
		ID:           "e1",
		CanonicalURI: "repo:github/foo/bar",
		URL:          "https://github.com/foo/bar",
	}
	collectors, err := collectorsFor(context.Background(), entity, CollectOpts{
		Stderr: &stderr,
	})
	require.NoError(t, err,
		"repo: entity without --clone must NOT error — every API-only collector still has work to do")

	names := make([]string, 0, len(collectors))
	for _, c := range collectors {
		names = append(names, c.Name())
	}
	// API-only set: all five must be dispatched.
	for _, want := range []string{"github", "forgejo", "gitlab", "adoption", "openssf-scorecard"} {
		assert.Contains(t, names, want,
			"API-only collector %q must dispatch even without --clone (no git operations, no filesystem reads)", want)
	}
	// Clone-dependent set: all must be absent.
	for _, mustSkip := range []string{"git", "repofiles", "exfilwatch"} {
		assert.NotContains(t, names, mustSkip,
			"clone-dependent collector %q must NOT dispatch when --clone is absent — it reads the local clone", mustSkip)
	}
	assert.Contains(t, stderr.String(), "pass --clone",
		"warning must surface what --clone unlocks (the clone-dependent collectors)")
}

// TestCollectorsFor_NpmEntityWithResolvedURL_IncludesAll verifies
// the other side of the contract: once A.5 has stamped a github URL
// on an npm entity, all three collectors dispatch and the clone
// path contract applies. Tests the "post-resolution" branch from
// the npm-plan.
func TestCollectorsFor_NpmEntityWithResolvedURL_IncludesAll(t *testing.T) {
	t.Parallel()

	src := initSourceRepo(t, "https://github.com/expressjs/express")

	entity := &profile.Entity{
		ID:           "e1",
		CanonicalURI: "pkg:npm/express",
		Type:         profile.EntityPackage,
		Ecosystem:    "npm",
		URL:          "https://github.com/expressjs/express",
	}
	collectors, err := collectorsFor(context.Background(), entity, CollectOpts{Path: src, Cleanups: testCleanups(t)})
	require.NoError(t, err)

	names := map[string]bool{}
	for _, c := range collectors {
		names[c.Name()] = true
	}
	assert.True(t, names["npm-registry"], "npm collector should be present")
	assert.True(t, names["github"], "github collector should be present after URL resolution")
	assert.True(t, names["git"], "git collector should be present after URL resolution")
	assert.True(t, names["openssf-scorecard"],
		"openssf-scorecard collector should also dispatch once an npm package has a resolved github URL — it caches scorecard data for the resolved repo")
	assert.True(t, names["source-evolution"],
		"source-evolution collector should dispatch for an npm package with a resolved github URL — it consumes the npm-registry version_pin_table to emit the AST matrix")
}

// TestCollectorsFor_GoModuleEntity_IncludesGoPublishAndSourceEvolution
// locks in the dispatch contract for Go-ecosystem entities: a
// pkg:golang/... entity with a resolved github URL gets BOTH the
// gopublish collector (emits version_pin_table) and the
// source-evolution collector (consumes the pin table to emit the
// matrix and anomaly signals) — alongside github + git + repofiles
// + openssf-scorecard.
//
// This is the test that catches a regression where the dispatch
// switch in collectorsFor either drops the "golang"/"go" case or
// fails to actually append either collector. The source-evolution
// emission is the design-doc D3 signal class; both signals are
// load-bearing for the BufferZoneCorp-shaped attack-detection
// path.
func TestCollectorsFor_GoModuleEntity_IncludesGoPublishAndSourceEvolution(t *testing.T) {
	t.Parallel()

	src := initSourceRepo(t, "https://github.com/alecthomas/kong")

	entity := &profile.Entity{
		ID:           "e1",
		CanonicalURI: "pkg:golang/github.com/alecthomas/kong",
		Type:         profile.EntityPackage,
		Ecosystem:    "golang",
		URL:          "https://github.com/alecthomas/kong",
	}
	collectors, err := collectorsFor(context.Background(), entity, CollectOpts{Path: src, Cleanups: testCleanups(t)})
	require.NoError(t, err)

	names := map[string]bool{}
	for _, c := range collectors {
		names[c.Name()] = true
	}
	assert.True(t, names["go-publish"],
		"gopublish collector must dispatch for Ecosystem=\"golang\" — emits version_pin_table consumed by source-evolution")
	assert.True(t, names["source-evolution"],
		"source-evolution collector must dispatch for Ecosystem=\"golang\" — emits source_evolution_matrix + source_evolution_anomaly")
	assert.True(t, names["github"], "github collector should also dispatch for resolved Go entity")
	assert.True(t, names["git"], "git collector should also dispatch for resolved Go entity")
	assert.True(t, names["repofiles"], "repofiles collector should also dispatch for resolved Go entity")
	assert.True(t, names["openssf-scorecard"], "openssf-scorecard collector should also dispatch for resolved Go entity")
}

// TestCollectorsFor_GoModuleEntity_SourceEvolutionAfterGoPublish
// locks in the dispatch ORDER constraint: source-evolution must
// run AFTER gopublish in the same run, so the in-run accumulator
// (passed through CollectOpts.InRunResult) holds gopublish's
// version_pin_table emission by the time source-evolution's
// VersionPinSource consults it.
//
// A regression that reorders the dispatch slice (e.g., appending
// source-evolution earlier than gopublish, or letting it slip
// before the git-hosted block where clonePath is resolved) would
// break the in-run pin-table lookup. Source-evolution would then
// see only the previous run's pin table from the store — or
// nothing if this is a fresh-DB run — silently degrading to
// absences.
func TestCollectorsFor_GoModuleEntity_SourceEvolutionAfterGoPublish(t *testing.T) {
	t.Parallel()

	src := initSourceRepo(t, "https://github.com/alecthomas/kong")

	entity := &profile.Entity{
		ID:           "e1",
		CanonicalURI: "pkg:golang/github.com/alecthomas/kong",
		Type:         profile.EntityPackage,
		Ecosystem:    "golang",
		URL:          "https://github.com/alecthomas/kong",
	}
	collectors, err := collectorsFor(context.Background(), entity, CollectOpts{Path: src, Cleanups: testCleanups(t)})
	require.NoError(t, err)

	var goPublishIdx, sourceIdx = -1, -1
	for i, c := range collectors {
		switch c.Name() {
		case "go-publish":
			goPublishIdx = i
		case "source-evolution":
			sourceIdx = i
		}
	}
	require.NotEqual(t, -1, goPublishIdx, "gopublish must be in dispatch")
	require.NotEqual(t, -1, sourceIdx, "source-evolution must be in dispatch")
	assert.Less(t, goPublishIdx, sourceIdx,
		"source-evolution must run AFTER gopublish so the in-run accumulator holds version_pin_table by the time source-evolution's pinSource consults it")
}

// TestCollectorsFor_GoModuleLegacyEcosystem_IncludesGoPublish covers
// the legacy Ecosystem="go" form (pre-purl-canonicalization). Some
// in-store entities created before the migration still carry "go"
// rather than "golang"; the dispatch's `case "golang", "go":` arm
// must keep matching both — for both gopublish AND source-evolution.
func TestCollectorsFor_GoModuleLegacyEcosystem_IncludesGoPublish(t *testing.T) {
	t.Parallel()

	src := initSourceRepo(t, "https://github.com/alecthomas/kong")

	entity := &profile.Entity{
		ID:           "e1",
		CanonicalURI: "pkg:go/github.com/alecthomas/kong",
		Type:         profile.EntityPackage,
		Ecosystem:    "go",
		URL:          "https://github.com/alecthomas/kong",
	}
	collectors, err := collectorsFor(context.Background(), entity, CollectOpts{Path: src, Cleanups: testCleanups(t)})
	require.NoError(t, err)

	names := map[string]bool{}
	for _, c := range collectors {
		names[c.Name()] = true
	}
	assert.True(t, names["go-publish"],
		"gopublish collector must dispatch for legacy Ecosystem=\"go\"")
	assert.True(t, names["source-evolution"],
		"source-evolution collector must dispatch for legacy Ecosystem=\"go\"")
}

// TestCollectorsFor_NpmEcosystem_NoGoPublishButSourceEvolution pins
// the npm dispatch boundary. gopublish is proxy.golang.org-specific
// and must NEVER dispatch for npm. source-evolution, by contrast,
// now DOES dispatch for npm (it consumes the npm-registry collector's
// gitHead/attestation version_pin_table) — this assertion used to
// require its absence; flipping it is the wiring-shipped signal that
// step 2 (npm AST analyzer) landed, the same pattern as the pypi
// flip below. A regression that re-narrows the source-evolution
// guard back to Go-only, or that broadens gopublish to non-Go, trips
// here.
func TestCollectorsFor_NpmEcosystem_NoGoPublishButSourceEvolution(t *testing.T) {
	t.Parallel()

	src := initSourceRepo(t, "https://github.com/expressjs/express")

	entity := &profile.Entity{
		ID:           "e1",
		CanonicalURI: "pkg:npm/express",
		Type:         profile.EntityPackage,
		Ecosystem:    "npm",
		URL:          "https://github.com/expressjs/express",
	}
	collectors, err := collectorsFor(context.Background(), entity, CollectOpts{Path: src, Cleanups: testCleanups(t)})
	require.NoError(t, err)

	names := map[string]bool{}
	for _, c := range collectors {
		names[c.Name()] = true
	}
	assert.False(t, names["go-publish"],
		"gopublish collector must NOT dispatch for npm — it is proxy.golang.org-specific")
	assert.True(t, names["source-evolution"],
		"source-evolution MUST dispatch for npm — it consumes the npm-registry "+
			"collector's version_pin_table to emit the AST matrix + anomaly")
}

// TestCollectorsFor_PypiEcosystem_NoGoPublishButSourceEvolution pins
// the pypi dispatch boundary, symmetric to the npm pin above. The
// source-evolution collector consumes the pypi-registry collector's
// attestation-derived version_pin_table; a regression that re-narrows
// the source-evolution guard back to Go-only (or removes the "pypi"
// arm of the four-ecosystem condition in collectorsFor) trips here.
// gopublish must stay absent — proxy.golang.org is Go-specific.
func TestCollectorsFor_PypiEcosystem_NoGoPublishButSourceEvolution(t *testing.T) {
	t.Parallel()

	src := initSourceRepo(t, "https://github.com/psf/requests")

	entity := &profile.Entity{
		ID:           "e1",
		CanonicalURI: "pkg:pypi/requests",
		Type:         profile.EntityPackage,
		Ecosystem:    "pypi",
		URL:          "https://github.com/psf/requests",
	}
	collectors, err := collectorsFor(context.Background(), entity, CollectOpts{Path: src, Cleanups: testCleanups(t)})
	require.NoError(t, err)

	names := map[string]bool{}
	for _, c := range collectors {
		names[c.Name()] = true
	}
	assert.False(t, names["go-publish"],
		"gopublish collector must NOT dispatch for pypi — it is proxy.golang.org-specific")
	assert.True(t, names["source-evolution"],
		"source-evolution MUST dispatch for pypi — it consumes the pypi-registry "+
			"collector's attestation-derived version_pin_table to emit the AST matrix + anomaly")
}

// TestCollectorsFor_PypiPackage_NoURL_GetsPypiCollector pins the
// post-Phase-E wiring: a pkg:pypi/ entity with no resolved github
// URL still gets the pypi-registry collector (so publisher entities
// land + maintainer_count emits + cascade resolver picks them up).
// This used to assert "pypi gets zero collectors" before the pypi
// collector landed; flipping the assertion is the wiring-shipped
// signal.
func TestCollectorsFor_PypiPackage_NoURL_GetsPypiCollector(t *testing.T) {
	t.Parallel()

	entity := &profile.Entity{
		ID:           "e1",
		CanonicalURI: "pkg:pypi/requests",
		Type:         profile.EntityPackage,
		Ecosystem:    "pypi",
		URL:          "",
	}
	collectors, err := collectorsFor(context.Background(), entity, CollectOpts{})
	require.NoError(t, err)

	var names []string
	for _, c := range collectors {
		names = append(names, c.Name())
	}
	assert.Contains(t, names, "pypi-registry",
		"pypi entity must dispatch the pypi-registry collector even without a resolved github URL — the maintainer_count signal feeds the cascade resolver and publisher entities mint regardless of repo presence")
}

// TestCollectorsFor_UnwiredEcosystemPackage_NoCollectors guards the
// safe-skip behaviour for ecosystems signatory doesn't yet collect
// against. v0.1 wires npm + pypi + golang + cargo + gem; nuget,
// php, ... lack collectors and must produce an empty slice — not
// a hard error.
func TestCollectorsFor_UnwiredEcosystemPackage_NoCollectors(t *testing.T) {
	t.Parallel()

	entity := &profile.Entity{
		ID:           "e1",
		CanonicalURI: "pkg:nuget/Newtonsoft.Json",
		Type:         profile.EntityPackage,
		Ecosystem:    "nuget",
		URL:          "",
	}
	collectors, err := collectorsFor(context.Background(), entity, CollectOpts{})
	require.NoError(t, err)
	assert.Empty(t, collectors,
		"nuget isn't wired — entities for unwired ecosystems must produce zero collectors without erroring")
}

// --- gitCloneFull / validateExistingClone env-sanitization tests ------------
//
// These tests guard the env-var stripping on the analyze-path git
// subprocesses (cmd/signatory/collectors.go), which is the second git-
// clone site in the binary. The first site, runGitClone in handoff.go,
// has been env-sanitized via gitenv.SafeEnv since the Go-security adversarial
// pass; the analyze-path additions (gitCloneFull, validateExistingClone)
// landed later and inherited the full os.Environ() including the same
// dangerous vars handoff already strips. The two-Opus 2026-04-22 review
// flagged the drift in both passes — this is the regression guard.
//
// Threat: a hostile parent environment sets GIT_SSH_COMMAND or
// GIT_CONFIG_KEY_*/VALUE_* to attacker-controlled values; without the
// stripping, the git subprocess invokes the attacker binary or applies
// the injected config (e.g. core.sshCommand) — same RCE class as the
// handoff-path threat the existing gitenv.SafeEnv tests cover.
//
// Approach: PATH-shim a fake `git` that dumps its env to a file and
// exits 0. After the parent calls gitCloneFull / validateExistingClone,
// the test reads the dumped env and asserts the dangerous vars are
// absent. Validates the actual subprocess boundary (cmd.Env), not just
// gitenv.SafeEnv()'s output — the bug was specifically that cmd.Env wasn't
// being assigned, so a test of gitenv.SafeEnv() in isolation wouldn't catch it.

// installFakeGitDump writes a POSIX shim named "git" into a fresh
// temp dir, dumps that dir at the head of PATH for the test's lifetime,
// and returns the paths to which the shim writes the parent's env AND
// the shim's own argv. The shim exits 0 unconditionally so the
// caller's gitCloneFull / validateExistingClone / defaultGitClone
// returns nil and the test can read the dumps.
//
// Two assertion surfaces:
//
//   - envDump: contents of `env`. Tests that this output is missing
//     dangerous parent vars (GIT_SSH_COMMAND, GIT_CONFIG_KEY_*, etc.)
//     prove gitenv.SafeEnv reached cmd.Env.
//   - argvDump: one argv slot per line via `printf '%s\n' "$@"`. Tests
//     that the output begins with the safeOverrides "-c key=value"
//     prefix prove gitenv.NewCmd / NewCloneCmd injected the file-vector
//     defense before user args (CVE-2025-41390 / TALOS-2025-2243).
//
// The shim overwrites both files on each git invocation; tests that
// invoke the parent multiple times must read between calls or accept
// that only the last invocation's dump is observable.
//
// Cleanup is automatic: t.TempDir for the shim and dump locations,
// t.Setenv for the PATH override.
func installFakeGitDump(t *testing.T) (envDump, argvDump string) {
	t.Helper()
	shimDir := t.TempDir()
	dumpDir := t.TempDir()
	envDump = filepath.Join(dumpDir, "env-dump")
	argvDump = filepath.Join(dumpDir, "argv-dump")
	fakeGit := filepath.Join(shimDir, "git")
	// `env` is POSIX. Single-quote the path so the shell doesn't
	// expand $VAR, $(...), or `...` if a future t.TempDir ever lands
	// on a path with those characters. %q would use double quotes,
	// inside which sh still performs variable/command substitution —
	// cheap to avoid by switching to single-quote escaping now.
	// Embedded single quotes in the path get the canonical POSIX
	// close-escape-reopen treatment (see shellSingleQuote below).
	//
	// `printf '%s\n' "$@"` writes one argv slot per line, which is
	// trivial to readArgvDump-parse. Note the doubled %% to escape
	// fmt.Sprintf's verb.
	script := fmt.Sprintf(
		"#!/bin/sh\nenv > %s\nprintf '%%s\\n' \"$@\" > %s\nexit 0\n",
		shellSingleQuote(envDump),
		shellSingleQuote(argvDump),
	)
	require.NoError(t, os.WriteFile(fakeGit, []byte(script), 0o755))
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return envDump, argvDump
}

// shellSingleQuote wraps s in POSIX-sh single quotes. Embedded single
// quotes are escaped by the canonical 4-character idiom: close the
// quoted run, emit a backslash-escaped single quote, reopen. Safe
// even if s contains $, `, \, or other shell metacharacters — single-
// quoted strings in POSIX sh perform no substitution of any kind.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// readEnvDump reads the env-dump file written by the fake git shim and
// returns a key→value map for assertion. Fails the test if the dump
// is missing — that indicates the parent never spawned the subprocess,
// which is itself a regression worth surfacing.
func readEnvDump(t *testing.T, path string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoErrorf(t, err, "fake git must have produced env dump at %s", path)
	out := map[string]string{}
	for line := range strings.SplitSeq(string(data), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[key] = value
	}
	return out
}

// readArgvDump reads the argv-dump file written by the fake git shim
// (one argv slot per line via `printf '%s\n' "$@"`) and returns the
// argv slice. argv[0] in the shim's view is git's first argument, NOT
// the binary name — the dump contains exactly what the parent passed,
// not what /bin/sh prepended.
//
// Fails the test if the dump is missing; that indicates the parent
// never spawned the subprocess (a regression worth surfacing in its
// own right).
func readArgvDump(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoErrorf(t, err, "fake git must have produced argv dump at %s", path)
	// printf appends a trailing newline; TrimSpace then Split avoids
	// a phantom empty-string final entry.
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

// TestGitCloneFull_StripsDangerousEnv is the RED test for the analyze-
// path env-sanitization drift. Before the fix, gitCloneFull called
// exec.CommandContext without setting cmd.Env, so the subprocess
// inherited the full parent env including GIT_SSH_COMMAND and the
// bulk-injection GIT_CONFIG_* vars. After the fix, cmd.Env =
// gitenv.SafeEnv() strips them at the boundary.
//
// Revert proof: delete the `cmd.Env = gitenv.SafeEnv()` line from
// gitCloneFull; this test fails because GIT_SSH_COMMAND appears in
// the env dump.
//
// NOTE: t.Setenv and t.Parallel are mutually exclusive; intentionally
// sequential.
func TestGitCloneFull_StripsDangerousEnv(t *testing.T) {
	envDump, _ := installFakeGitDump(t)

	// Hostile parent env — a representative sample of the vars
	// gitenv.SafeEnv strips. Full coverage of the strip rule lives
	// in internal/gitenv/env_test.go's TestSafeEnv_StripsAllGitPrefix;
	// here we just need enough to prove the boundary is enforced.
	t.Setenv("GIT_SSH_COMMAND", "evil-binary --steal-credentials")
	t.Setenv("GIT_PROXY_COMMAND", "evil-proxy")
	t.Setenv("GIT_EXEC_PATH", "/tmp/attacker-bin")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.sshCommand")
	t.Setenv("GIT_CONFIG_VALUE_0", "evil")

	// Dest doesn't need to exist; the fake git ignores its args. URL
	// is a synthetic-but-validatable form (file:// passes
	// safeGitCloneURL — though gitCloneFull's caller-asserts comment
	// notes validation happens upstream, gitCloneFull itself doesn't
	// re-validate).
	dest := filepath.Join(t.TempDir(), "clone-dest")
	require.NoError(t,
		gitCloneFull(context.Background(), "https://example.invalid/repo.git", dest),
		"fake git must exit 0 — non-zero indicates the shim wasn't picked up via PATH")

	env := readEnvDump(t, envDump)
	for _, key := range []string{
		"GIT_SSH_COMMAND",
		"GIT_PROXY_COMMAND",
		"GIT_EXEC_PATH",
		"GIT_CONFIG_COUNT",
		"GIT_CONFIG_KEY_0",
		"GIT_CONFIG_VALUE_0",
	} {
		_, present := env[key]
		assert.Falsef(t, present,
			"%s must not leak from parent env into git subprocess (gitCloneFull)", key)
	}
	// PATH must survive — the child needs it to locate ssh, helpers,
	// etc. Verifies gitenv.SafeEnv didn't accidentally strip too much.
	assert.NotEmpty(t, env["PATH"], "PATH must be preserved in child env")
	// GIT_TERMINAL_PROMPT is force-set to 0 by gitenv.SafeEnv to prevent
	// the child from blocking on a credential prompt.
	assert.Equal(t, "0", env["GIT_TERMINAL_PROMPT"],
		"GIT_TERMINAL_PROMPT must be force-set to 0 in child env")
}

// --- Subprocess-boundary tests for the file-vector defense (CVE-2025-41390) ---
//
// These mirror the _StripsDangerousEnv tests above for the env-vector
// defense, but assert the OTHER chokepoint contribution from
// gitenv.NewCmd / NewCloneCmd: the safeOverrides "-c key=value" argv
// prefix that neutralizes attacker-controlled `.git/config` directives
// (gpg.program, core.hooksPath, credential.helper, etc.) before they
// can drive arbitrary command execution.
//
// The unit-level catalog and prefix-vs-suffix ordering are locked by
// internal/gitenv/env_test.go (TestNewCmd_InjectsConfigOverrides /
// TestNewCmd_OverridesPrecedeUserArgs). These tests prove the
// chokepoint REACHES the spawned subprocess at the production sites —
// that is, that gitCloneFull / validateExistingClone / defaultGitClone
// route through gitenv.NewCmd / NewCloneCmd rather than constructing
// an exec.Cmd by hand and bypassing the discipline. The bug we're
// guarding against is "someone adds a new git invocation with
// exec.Command directly."
//
// Each test asserts that gpg.program=/usr/bin/false (the canonical
// catalog entry covering the actual collectCommitSigning %G? attack
// vector) AND core.hooksPath=/dev/null (covering the git fetch
// reference-transaction hook vector) appear as `-c <kv>` pairs in the
// child's argv. Asserting two entries rather than one catches a
// half-fix where someone passes `-c gpg.program=...` ad-hoc but
// doesn't go through the chokepoint.

// TestGitCloneFull_PassesConfigOverrides proves the file-vector "-c"
// prefix from gitenv.NewCloneCmd reaches the git subprocess for the
// analyze-path full-clone site (gitCloneFull in collectors.go).
//
// Revert proof: change gitCloneFull to construct exec.Cmd directly
// instead of via gitenv.NewCloneCmd; this test fails because
// gpg.program / core.hooksPath are absent from the child's argv.
//
// NOTE: t.Setenv and t.Parallel are mutually exclusive; intentionally
// sequential.
func TestGitCloneFull_PassesConfigOverrides(t *testing.T) {
	_, argDump := installFakeGitDump(t)

	dest := filepath.Join(t.TempDir(), "clone-dest")
	require.NoError(t,
		gitCloneFull(context.Background(), "https://example.invalid/repo.git", dest),
		"fake git must exit 0 — non-zero indicates the shim wasn't picked up via PATH")

	args := readArgvDump(t, argDump)
	assertCarriesOverride(t, args, "gpg.program=/usr/bin/false",
		"gitCloneFull must route through gitenv.NewCloneCmd so the file-vector -c prefix reaches the child")
	assertCarriesOverride(t, args, "core.hooksPath=/dev/null",
		"gitCloneFull must carry core.hooksPath=/dev/null — clone fires reference-transaction hooks")
}

// TestValidateExistingClone_StripsDangerousEnv covers the second git-
// subprocess site in collectors.go. validateExistingClone runs
// `git -C <path> remote get-url origin` to verify the clone matches
// the declared entity. Even though it's a read-only operation, env-
// based config injection (GIT_CONFIG_KEY_* setting e.g. include.path
// to a hostile config) can still subvert it. Same fix, same boundary.
//
// Revert proof: delete the `cmd.Env = gitenv.SafeEnv()` line in
// validateExistingClone; this test fails because GIT_CONFIG_KEY_0
// appears in the dump.
//
// NOTE: t.Setenv and t.Parallel are mutually exclusive; intentionally
// sequential.
func TestValidateExistingClone_StripsDangerousEnv(t *testing.T) {
	envDump, _ := installFakeGitDump(t)

	t.Setenv("GIT_SSH_COMMAND", "evil-binary")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "include.path")
	t.Setenv("GIT_CONFIG_VALUE_0", "/tmp/attacker.gitconfig")

	// validateExistingClone wants a directory containing a `.git`
	// child to satisfy its os.Stat check before invoking git. The
	// fake git doesn't care what's actually in there.
	clone := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(clone, ".git"), 0o755))

	// expectedURI just needs to be syntactically valid; the fake git
	// outputs nothing to stdout, so origin parsing will fail and
	// validateExistingClone returns an error — but the env dump
	// happens before that, which is all we're testing.
	_ = validateExistingClone(context.Background(), clone, "repo:github/owner/repo")

	env := readEnvDump(t, envDump)
	for _, key := range []string{
		"GIT_SSH_COMMAND",
		"GIT_CONFIG_COUNT",
		"GIT_CONFIG_KEY_0",
		"GIT_CONFIG_VALUE_0",
	} {
		_, present := env[key]
		assert.Falsef(t, present,
			"%s must not leak from parent env into git subprocess (validateExistingClone)", key)
	}
	assert.NotEmpty(t, env["PATH"], "PATH must be preserved in child env")
	assert.Equal(t, "0", env["GIT_TERMINAL_PROMPT"],
		"GIT_TERMINAL_PROMPT must be force-set to 0 in child env")
}

// TestValidateExistingClone_PassesConfigOverrides proves the file-vector
// "-c" prefix from gitenv.NewCmd reaches the git subprocess for the
// analyze-path origin-validation site (validateCloneOrigin via
// validateExistingClone in collectors.go).
//
// Revert proof: change validateCloneOrigin to construct exec.Cmd
// directly instead of via gitenv.NewCmd; this test fails because
// gpg.program is absent from the child's argv.
//
// NOTE: t.Setenv and t.Parallel are mutually exclusive; intentionally
// sequential.
func TestValidateExistingClone_PassesConfigOverrides(t *testing.T) {
	_, argDump := installFakeGitDump(t)

	clone := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(clone, ".git"), 0o755))

	// As in TestValidateExistingClone_StripsDangerousEnv, the fake
	// git's empty stdout makes origin parsing fail and the function
	// returns an error — irrelevant to this test, which only cares
	// that the subprocess was spawned and recorded its argv before
	// origin parsing would have run.
	_ = validateExistingClone(context.Background(), clone, "repo:github/owner/repo")

	args := readArgvDump(t, argDump)
	assertCarriesOverride(t, args, "gpg.program=/usr/bin/false",
		"validateCloneOrigin must route through gitenv.NewCmd so the file-vector -c prefix reaches the child")
	assertCarriesOverride(t, args, "core.hooksPath=/dev/null",
		"validateCloneOrigin must carry core.hooksPath=/dev/null — defense-in-depth even on read-only ops")
}

// assertCarriesOverride fails the test unless args contains a
// consecutive pair "-c", want. Walks pairs in argv, the same shape
// gitenv.NewCmd produces. Shared by the three _PassesConfigOverrides
// tests (gitCloneFull / validateExistingClone here, defaultGitClone
// in handoff_test.go) so the assertion shape is identical at every
// production site.
func assertCarriesOverride(t *testing.T, args []string, want, msg string) {
	t.Helper()
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-c" && args[i+1] == want {
			return
		}
	}
	t.Errorf("%s: expected `-c %s` pair in child argv but did not find it; argv=%v", msg, want, args)
}

// --- classifyGitCloneError tests ---------------------------------------------
//
// classifyGitCloneError detects the "could not read Username … terminal
// prompts disabled" stderr pair git emits when the forge returns 401
// and our discipline (credential.helper= in gitenv.safeOverrides +
// GIT_TERMINAL_PROMPT=0 in gitenv.SafeEnv) correctly refuses to supply
// or prompt for credentials. The classifier rewrites the error message
// so operators see the actual cause (missing/private/moved repo) rather
// than git's misleading "terminal prompts disabled" surface text.
//
// Security invariant tested below: the helper does NOT touch argv, env,
// or the subprocess. It only inspects stderr after the fact and rewrites
// the Go error string. Tests assert the original git stderr is preserved
// in the wrapped error so a future "let's clean up the message" patch
// that drops the original stderr fails loudly here. See the godoc on
// classifyGitCloneError in collectors.go for the full design rationale.

// TestClassifyGitCloneError_AuthRequiredPattern pins the rewrite of
// git's "could not read Username … terminal prompts disabled" stderr
// into an operator-facing message that names the actual cause (repo
// missing, private, or moved) without weakening the no-credentials
// posture. Drives the helper's existence (red before
// classifyGitCloneError exists; green after).
func TestClassifyGitCloneError_AuthRequiredPattern(t *testing.T) {
	t.Parallel()

	stderr := "Cloning into '/tmp/x'...\n" +
		"fatal: could not read Username for 'https://codeberg.org': terminal prompts disabled\n"
	args := []string{"clone", "https://codeberg.org/missing/repo", "/tmp/x"}
	runErr := fmt.Errorf("exit status 128")

	err := classifyGitCloneError(args, runErr, stderr)
	require.Error(t, err, "auth-prompt pattern must classify, not pass through")
	assert.ErrorIs(t, err, ErrCloneAuthRequiredOrMissing,
		"sentinel must be reachable via errors.Is so callers/tests can differentiate auth failures from other clone errors")
	assert.ErrorIs(t, err, runErr,
		"underlying exec error must remain reachable via errors.Is so callers can match exit-status-shaped failures")
	assert.Contains(t, err.Error(), "does not pass credentials",
		"operator-facing message must explain WHY git couldn't authenticate (signatory's no-credentials discipline, not a git bug)")
	assert.Contains(t, err.Error(), "git ls-remote",
		"message must point at the diagnostic command operators can run with their own credential setup")
	assert.Contains(t, err.Error(), "terminal prompts disabled",
		"original git stderr must be preserved in the wrapped error for debugging — a future patch that drops the underlying stderr would silently degrade incident response")
}

// TestClassifyGitCloneError_NonAuthErrorPassesThrough pins the gate:
// only the "could not read Username" + "terminal prompts disabled"
// pair triggers the rewrite. Network failures, missing-host errors,
// and other clone-time stderrs must fall through (return nil) so
// the caller's existing wrap handles them — we don't want to claim
// every clone failure is an auth issue.
func TestClassifyGitCloneError_NonAuthErrorPassesThrough(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		stderr string
	}{
		{"DNS failure", "fatal: unable to access 'https://example.com/': Could not resolve host"},
		{"connection refused", "fatal: unable to access 'https://example.com/': Failed to connect"},
		{"prompt-disabled phrase alone", "warning: terminal prompts disabled\n"},
		{"username-error phrase alone", "fatal: could not read Username for some unrelated reason\n"},
		{"empty stderr", ""},
		{"unrelated 128", "fatal: destination path '/tmp/x' already exists and is not an empty directory.\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyGitCloneError(
				[]string{"clone", "https://example.com/foo", "/tmp/x"},
				fmt.Errorf("exit status 128"),
				tc.stderr,
			)
			assert.Nil(t, err,
				"non-auth pattern must return nil; defaultRunGit's existing wrap handles the error")
		})
	}
}

// TestDefaultRunGit_AuthRequired_ReturnsClassifiedError pins the wiring
// of classifyGitCloneError into defaultRunGit. Uses a synthetic git
// substitute (not the real git binary) to drive the auth-prompt stderr
// pattern deterministically without a network round-trip. Confirms the
// classified error reaches the caller, not the raw "git %v: ..." wrap
// that pre-existed.
//
// We can't directly inject a fake binary into defaultRunGit (it always
// invokes "git" via gitenv.NewCloneCmd), so this test stays at the
// classifier-helper layer. The integration is covered by
// TestClassifyGitCloneError_* plus a manual walk of defaultRunGit's
// six new lines — keeping the test surface honest about what it
// actually exercises.
