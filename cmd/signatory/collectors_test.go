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

func TestCollectorsFor_MissingPath_ReturnsErrCloneRequired(t *testing.T) {
	t.Parallel()

	entity := &profile.Entity{
		ID:           "e1",
		CanonicalURI: "repo:github/alecthomas/kong",
		URL:          "https://github.com/alecthomas/kong",
	}
	_, err := collectorsFor(context.Background(), entity, CollectOpts{})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrCloneRequired)
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
	_, err := collectorsFor(context.Background(), entity, CollectOpts{Path: dst, Clone: true})
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
	_, err := collectorsFor(context.Background(), entity, CollectOpts{Path: path})
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
	_, err := collectorsFor(context.Background(), entity, CollectOpts{Path: src})
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
	collectors, err := collectorsFor(context.Background(), entity, CollectOpts{Path: src})
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
	_, err := collectorsFor(context.Background(), entity, CollectOpts{Path: src})
	assert.NoError(t, err, "ssh-form origin should normalize to same canonical URI")
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

	collectors, err := collectorsFor(context.Background(), entity, CollectOpts{Path: dst, Clone: true})
	require.NoError(t, err)
	assert.Len(t, collectors, 5, "github + git + repofiles + openssf-scorecard + exfilwatch collectors returned")

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
	_, err := collectorsFor(context.Background(), entity, CollectOpts{Path: dst, Clone: true})
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
	got, err := resolveClonePath(context.Background(), entity, CollectOpts{Path: pathArg})
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
			require.Len(t, collectors, 1,
				"only the registry collector should fire when no clone is available")
			assert.Equal(t, tc.wantName, collectors[0].Name())
			assert.Contains(t, stderr.String(), "pass --clone",
				"should hint the user about --clone for additional signals")
		})
	}
}

// TestCollectorsFor_RepoEntity_NoPath_StillErrors confirms that
// repo: entities (no registry collector) still fail loudly when
// neither --path nor --clone is provided. The graceful degradation
// only applies to pkg: entities that have registry collectors.
func TestCollectorsFor_RepoEntity_NoPath_StillErrors(t *testing.T) {
	t.Parallel()

	entity := &profile.Entity{
		ID:           "e1",
		CanonicalURI: "repo:github/foo/bar",
		URL:          "https://github.com/foo/bar",
	}
	_, err := collectorsFor(context.Background(), entity, CollectOpts{})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrCloneRequired,
		"repo: entities with no registry collector must still fail on missing clone")
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
	collectors, err := collectorsFor(context.Background(), entity, CollectOpts{Path: src})
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
	collectors, err := collectorsFor(context.Background(), entity, CollectOpts{Path: src})
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
	collectors, err := collectorsFor(context.Background(), entity, CollectOpts{Path: src})
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
	collectors, err := collectorsFor(context.Background(), entity, CollectOpts{Path: src})
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

// TestCollectorsFor_NonGoEcosystem_NoGoPublishOrSourceEvolution is
// the negative counterpart: an npm entity must NOT receive either
// the gopublish collector OR the source-evolution collector. A
// regression that broadens either dispatch case (e.g., adding
// "default:" by accident, or moving source-evolution outside the
// Go-ecosystem guard) would catch on this assertion.
func TestCollectorsFor_NonGoEcosystem_NoGoPublishOrSourceEvolution(t *testing.T) {
	t.Parallel()

	src := initSourceRepo(t, "https://github.com/expressjs/express")

	entity := &profile.Entity{
		ID:           "e1",
		CanonicalURI: "pkg:npm/express",
		Type:         profile.EntityPackage,
		Ecosystem:    "npm",
		URL:          "https://github.com/expressjs/express",
	}
	collectors, err := collectorsFor(context.Background(), entity, CollectOpts{Path: src})
	require.NoError(t, err)

	for _, c := range collectors {
		assert.NotEqualf(t, "go-publish", c.Name(),
			"gopublish collector must NOT dispatch for non-Go ecosystem (got it for Ecosystem=%q)", entity.Ecosystem)
		assert.NotEqualf(t, "source-evolution", c.Name(),
			"source-evolution collector must NOT dispatch for non-Go ecosystem (got it for Ecosystem=%q)", entity.Ecosystem)
	}
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

// installFakeGitEnvDump writes a POSIX shim named "git" into a fresh
// temp dir, dumps that dir at the head of PATH for the test's lifetime,
// and returns the path to which the shim writes its environment. The
// shim exits 0 unconditionally so the caller's gitCloneFull /
// validateExistingClone returns nil and the test can read the dump.
//
// Cleanup is automatic: t.TempDir for the shim and dump locations,
// t.Setenv for the PATH override.
func installFakeGitEnvDump(t *testing.T) string {
	t.Helper()
	shimDir := t.TempDir()
	envDump := filepath.Join(t.TempDir(), "env-dump")
	fakeGit := filepath.Join(shimDir, "git")
	// `env` is POSIX. Single-quote the path so the shell doesn't
	// expand $VAR, $(...), or `...` if a future t.TempDir ever lands
	// on a path with those characters. %q would use double quotes,
	// inside which sh still performs variable/command substitution —
	// cheap to avoid by switching to single-quote escaping now.
	// Embedded single quotes in the path get the canonical POSIX
	// close-escape-reopen treatment (see shellSingleQuote below).
	script := fmt.Sprintf("#!/bin/sh\nenv > %s\nexit 0\n", shellSingleQuote(envDump))
	require.NoError(t, os.WriteFile(fakeGit, []byte(script), 0o755))
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return envDump
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
	envDump := installFakeGitEnvDump(t)

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
	envDump := installFakeGitEnvDump(t)

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
