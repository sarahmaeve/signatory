package main

// analyze_clone_test.go — RED-phase TDD tests for the idempotent
// clone-or-refresh evolution of `signatory analyze --clone`.
//
// CONTRACT UNDER TEST (not yet implemented — these tests are RED):
//
//  1. `--clone` without `--refresh` → UsageError naming both flags.
//  2. `--clone --refresh` with `--path` missing → default path to
//     filestore/clones/<short-name> relative to CWD.
//  3. `--clone --refresh --path=dest` where dest is missing/empty →
//     fresh full clone into dest.
//  4. `--clone --refresh --path=dest` where dest holds a valid full
//     clone of the target → git fetch (no re-clone).
//  5. `--clone --refresh --path=dest` where dest holds a shallow clone
//     → git fetch --unshallow (no re-clone).
//  6. `--clone --refresh --path=dest` where dest's origin mismatches
//     the target → loud fail, no git operations.
//  7. `--refresh --path=dest` (no --clone) where dest holds a shallow
//     clone → loud fail pointing user toward --clone.
//
// SAFETY CONSTRAINTS (see task description for the full rationale):
//
//   - Every git subprocess goes through gitenv.NewCmd / gitenv.NewCloneCmd,
//     never exec.Command("git", ...) directly. Tests that run real git
//     use mustRunGitInTest (defined in collectors_test.go).
//   - No real remote URLs are ever fetched. Clone/fetch operations in
//     tests use file:// or local filesystem paths.
//   - Every filesystem path is rooted in t.TempDir().
//   - Fixture repos disable commit.gpgsign and tag.gpgSign.
//
// PARALLEL DISCIPLINE:
//
//   - Tests that use t.Setenv (PATH-shim based) are intentionally NOT
//     parallel (Go panics if you mix t.Parallel + t.Setenv).
//   - All other tests use t.Parallel().

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---- Test helpers specific to this file -----------------------------------

// initSourceRepoWithCommits creates a source git repo with n commits
// (n ≥ 1) and sets a synthetic remote origin URL. Returns the
// absolute path to the repo, usable as a `git clone file:///path dest`
// argument.
//
// A multi-commit repo is required for shallow-clone tests: a
// --depth=1 clone of a single-commit repo still has a complete object
// graph (no truncation), so .git/shallow may or may not exist. Two or
// more commits guarantee that a --depth=1 clone truly truncates.
//
// All git operations go through mustRunGitInTest (gitenv.NewCmd
// underneath) to inherit env-strip discipline.
func initSourceRepoWithCommits(t *testing.T, n int, origin string) string {
	t.Helper()
	if n < 1 {
		t.Fatal("initSourceRepoWithCommits: n must be >= 1")
	}
	dir := t.TempDir()
	mustRunGitInTest(t, dir, "init", "-b", "main", "-q")
	mustRunGitInTest(t, dir, "config", "user.email", "test@example.invalid")
	mustRunGitInTest(t, dir, "config", "user.name", "Test")
	mustRunGitInTest(t, dir, "config", "commit.gpgsign", "false")
	mustRunGitInTest(t, dir, "config", "tag.gpgSign", "false")
	for i := range n {
		mustRunGitInTest(t, dir, "commit", "--allow-empty", "-m", "commit "+string(rune('0'+i)))
	}
	if origin != "" {
		mustRunGitInTest(t, dir, "remote", "add", "origin", origin)
	}
	return dir
}

// initFullClone creates a full (non-shallow) local clone of src into
// a newly-created subdirectory under a t.TempDir() parent. Returns
// the absolute path of the clone. After creation the clone's origin
// is set to syntheticOrigin (a synthetic github URL) so
// validateExistingClone can parse it.
//
// The actual clone uses `git clone file:///src dest` — local
// filesystem, no network. syntheticOrigin is written via
// `git remote set-url origin` and is never fetched; it exists only so
// the origin-validation logic sees a parseable github URL.
func initFullClone(t *testing.T, src, syntheticOrigin string) string {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "clone")
	// Use file:// scheme for local-filesystem clone. Absolute path
	// required; file:///path works on POSIX.
	mustRunGitInTest(t, ".", "clone", "file://"+src, dest)
	if syntheticOrigin != "" {
		mustRunGitInTest(t, dest, "remote", "set-url", "origin", syntheticOrigin)
	}
	return dest
}

// initShallowClone creates a shallow (--depth=1) local clone of src
// into a newly-created subdirectory under a t.TempDir() parent.
// Verifies that .git/shallow exists afterward (a shallow clone that
// doesn't produce the shallow marker file would silently make Test 5
// vacuous). After creation, the clone's origin is changed to
// syntheticOrigin.
func initShallowClone(t *testing.T, src, syntheticOrigin string) string {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "shallow-clone")
	mustRunGitInTest(t, ".", "clone", "--depth=1", "file://"+src, dest)
	// Confirm the shallow marker was created. If it wasn't, the test
	// would be checking the wrong scenario.
	require.FileExists(t, filepath.Join(dest, ".git", "shallow"),
		"shallow clone must produce .git/shallow — source repo needs ≥2 commits for --depth=1 to truncate")
	if syntheticOrigin != "" {
		mustRunGitInTest(t, dest, "remote", "set-url", "origin", syntheticOrigin)
	}
	return dest
}

// gitCallRecord captures a single git invocation by the RunGit seam.
type gitCallRecord struct {
	Workdir string
	Args    []string
}

// gitCallRecorder records all RunGit calls into a slice. Thread-safe
// (tests use t.Parallel) — each test constructs its own recorder so
// there is no shared state between tests.
type gitCallRecorder struct {
	mu    sync.Mutex
	calls []gitCallRecord
}

func (r *gitCallRecorder) record(ctx context.Context, workdir string, args ...string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := make([]string, len(args))
	copy(cp, args)
	r.calls = append(r.calls, gitCallRecord{Workdir: workdir, Args: cp})
	return nil
}

// calledWith reports whether any recorded call had the given first arg
// (the git subcommand). Thread-safe.
func (r *gitCallRecorder) calledWith(subcommand string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.calls {
		if len(c.Args) > 0 && c.Args[0] == subcommand {
			return true
		}
	}
	return false
}

// calledWithArgs reports whether any recorded call's args slice
// contains all of the supplied strings (order-independent, subset
// check). Used to assert "fetch --unshallow" without caring about
// additional args.
func (r *gitCallRecorder) calledWithArgs(required ...string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.calls {
		matched := 0
		for _, req := range required {
			if slices.Contains(c.Args, req) {
				matched++
			}
		}
		if matched == len(required) {
			return true
		}
	}
	return false
}

// ---- Tests ----------------------------------------------------------------

// TestAnalyzeClone_UsageErrorWhenCloneWithoutRefresh asserts that
// `--clone` without `--refresh` returns a UsageError naming both flags.
//
// TODAY: --clone without --refresh is a silent no-op (the refresh branch
// is never taken, Clone is ignored). The test expects a UsageError with
// "--clone" and "--refresh" in the message — behavior not yet
// implemented.
//
// RED assertion: errors.Is(err, ErrUsage) must be true.
func TestAnalyzeClone_UsageErrorWhenCloneWithoutRefresh(t *testing.T) {
	t.Parallel()

	globals := testGlobals(t) // isolated DB; no network collectors injected

	cmd := &AnalyzeCmd{
		Target:  "https://github.com/acme/widget",
		Clone:   true,
		Refresh: false, // the triggering condition
	}
	err := cmd.Run(globals)

	// Must return an error — not nil (today it returns nil silently).
	require.Error(t, err, "--clone without --refresh must return a UsageError; today it returns nil")
	assert.True(t, errors.Is(err, ErrUsage),
		"error must wrap ErrUsage (via NewUsageError); got: %v", err)
	assert.Contains(t, err.Error(), "--clone",
		"UsageError message must name --clone so the user knows which flag triggered it")
	assert.Contains(t, err.Error(), "--refresh",
		"UsageError message must name --refresh so the user knows the required companion flag")
}

// TestAnalyzeClone_DefaultsPathToFilestore_WhenPathUnset asserts that
// `--clone --refresh` with no `--path` defaults the destination to
// filestore/clones/<short-name>.
//
// Asserts on the side-effect that's actually testable in isolation: by
// the time AnalyzeCmd.Run returns, cmd.Path has been mutated to
// filestore/clones/<short-name>. Going through Run also exercises every
// guard the default sits behind (usage check, ResolveTarget). Mock
// collectors are injected via globals.Collectors so collectorsFor is
// bypassed entirely — no real network/git work happens, and we can
// assert on the path mutation that DID happen.
//
// The short-name for `https://github.com/acme/widget` is "widget"
// (last path segment, no .git suffix). The expected cmd.Path therefore
// ends in filepath.Join("filestore", "clones", "widget").
func TestAnalyzeClone_DefaultsPathToFilestore_WhenPathUnset(t *testing.T) {
	t.Parallel()

	mock := newMockCollector()
	globals := testGlobals(t, mock)

	cmd := &AnalyzeCmd{
		Target:  "https://github.com/acme/widget",
		Refresh: true,
		Clone:   true,
		Path:    "", // deliberately unset — should default
	}
	err := cmd.Run(globals)
	require.NoError(t, err, "--clone --refresh with no --path must succeed by defaulting cmd.Path")

	// cmd.Path must have been mutated to filestore/clones/widget.
	want := filepath.Join("filestore", "clones", "widget")
	assert.Truef(t,
		strings.HasSuffix(cmd.Path, want) ||
			strings.HasSuffix(cmd.Path, filepath.FromSlash("filestore/clones/widget")),
		"cmd.Path must default to filestore/clones/widget when --clone is set without --path; got %q", cmd.Path)
}

// TestAnalyzeClone_FreshClone_WhenPathMissing asserts that the --clone
// branch performs a fresh full clone when --path is missing. Tests
// collectorsFor directly (matching TestCollectorsFor_CloneHappyPath in
// collectors_test.go) — going through AnalyzeCmd.Run would also fire
// the github API collector, which fails on a synthetic target URL.
//
// The injected RunGit redirects the clone to a local fixture source
// repo so .git is actually created (no network).
func TestAnalyzeClone_FreshClone_WhenPathMissing(t *testing.T) {
	t.Parallel()

	src := initSourceRepo(t, "")
	dest := filepath.Join(t.TempDir(), "not-yet-created")

	recorder := &gitCallRecorder{}

	entity := &profile.Entity{
		ID:           "e1",
		CanonicalURI: "repo:github/acme/widget",
		URL:          "https://github.com/acme/widget",
	}

	_, err := collectorsFor(t.Context(), entity, CollectOpts{
		Path:  dest,
		Clone: true,
		RunGit: func(ctx context.Context, workdir string, args ...string) error {
			recorder.record(ctx, workdir, args...) //nolint:errcheck // recorder returns nil
			// Redirect the synthetic github URL to the local fixture so
			// the actual clone succeeds without network.
			if len(args) >= 1 && args[0] == "clone" {
				return gitCloneFull(ctx, src, dest)
			}
			return nil
		},
	})
	require.NoError(t, err, "--clone --refresh --path=dest (missing) must succeed")

	// Recorder must have seen exactly the clone subcommand.
	assert.True(t, recorder.calledWith("clone"),
		"missing dest must trigger a clone; RunGit must be called with 'clone'")
	assert.False(t, recorder.calledWith("fetch"),
		"missing dest must not trigger a fetch")

	// .git must exist at dest.
	assert.DirExists(t, filepath.Join(dest, ".git"),
		"fresh clone must create a .git directory at dest")
	// Must be a full clone: .git/shallow must NOT exist.
	assert.NoFileExists(t, filepath.Join(dest, ".git", "shallow"),
		"fresh clone must not be shallow (--clone always produces a full clone)")
}

// TestAnalyzeClone_RefreshesExistingValidClone asserts that the --clone
// branch fetches (not re-clones) when dest already holds a valid full
// clone of the target. Tests collectorsFor directly so the assertion
// is scoped to the clone-resolution contract — Run would also fire the
// github API collector against the synthetic target.
//
// Setup:
//   - Source repo at src (local fixture, multiple commits).
//   - Full clone of source at dest, with origin set to synthetic github URL.
//   - RunGit injected to capture calls.
func TestAnalyzeClone_RefreshesExistingValidClone(t *testing.T) {
	t.Parallel()

	src := initSourceRepoWithCommits(t, 2, "") // 2 commits, no origin needed
	dest := initFullClone(t, src, "https://github.com/acme/widget")

	recorder := &gitCallRecorder{}

	entity := &profile.Entity{
		ID:           "e1",
		CanonicalURI: "repo:github/acme/widget",
		URL:          "https://github.com/acme/widget",
	}

	_, err := collectorsFor(t.Context(), entity, CollectOpts{
		Path:   dest,
		Clone:  true,
		RunGit: recorder.record,
	})
	require.NoError(t, err,
		"--clone on an existing valid full clone must succeed (refresh via fetch, not re-clone)")

	assert.False(t, recorder.calledWith("clone"),
		"existing valid clone must not trigger a re-clone; RunGit must not be called with 'clone'")
	assert.True(t, recorder.calledWith("fetch"),
		"existing valid clone must trigger a git fetch to refresh; RunGit must be called with 'fetch'")
}

// TestAnalyzeClone_RefusesOriginMismatch asserts that --clone with an
// existing dest whose origin doesn't match the target returns
// ErrOriginMismatch without performing any git operations. Tests
// collectorsFor directly so we can assert no RunGit calls.
func TestAnalyzeClone_RefusesOriginMismatch(t *testing.T) {
	t.Parallel()

	src := initSourceRepoWithCommits(t, 1, "")
	// Clone and set origin to a DIFFERENT repo than the target.
	dest := initFullClone(t, src, "https://github.com/wrong/repo")

	recorder := &gitCallRecorder{}

	entity := &profile.Entity{
		ID:           "e1",
		CanonicalURI: "repo:github/acme/widget",
		URL:          "https://github.com/acme/widget", // target != origin
	}

	_, err := collectorsFor(t.Context(), entity, CollectOpts{
		Path:   dest,
		Clone:  true,
		RunGit: recorder.record,
	})

	require.Error(t, err, "origin mismatch must return an error")
	assert.True(t, errors.Is(err, ErrOriginMismatch),
		"error must be (or wrap) ErrOriginMismatch; got: %v", err)

	// No git clone/fetch operations must have been performed.
	assert.Empty(t, recorder.calls,
		"RunGit must not be invoked when origin mismatch is detected before any git operation")
}

// TestAnalyzeClone_UnshallowsExistingShallowClone asserts that
// `--clone --refresh --path=dest` where dest holds a shallow clone
// of the target calls `git fetch --unshallow` (or equivalent) and
// leaves no .git/shallow afterward.
//
// Setup:
//   - Source repo at src with ≥2 commits (required for a meaningful
//     shallow clone).
//   - Shallow clone (--depth=1) at dest (verified .git/shallow exists).
//   - dest's origin set to synthetic github URL.
//   - RunGit injected to (a) capture calls and (b) perform the real
//     unshallow when called with "fetch --unshallow".
//
// TODAY: --clone with non-empty dest returns ErrPathNotEmpty.
//
// RED assertions:
//   - err == nil
//   - RunGit was called with args containing "fetch" and "--unshallow"
//   - .git/shallow no longer exists
//   - RunGit was NOT called with "clone" (no re-clone)
//
// TestAnalyzeClone_UnshallowsExistingShallowClone asserts that --clone
// upgrades a shallow clone in place via `git fetch --unshallow`.
// Tests collectorsFor directly — Run would also fire the github API
// collector against the synthetic target.
//
// The injected RunGit performs the real unshallow against the local
// source so the filesystem assertion (.git/shallow gone) reflects
// reality. Origin URL is bounced file://↔https://github.com to satisfy
// validateCloneOrigin (parses synthetic) and the real fetch (needs a
// reachable URL) without leaving the test in either state.
func TestAnalyzeClone_UnshallowsExistingShallowClone(t *testing.T) {
	t.Parallel()

	// Source needs ≥2 commits for --depth=1 to produce a true
	// shallow (truncated) clone.
	src := initSourceRepoWithCommits(t, 3, "")
	dest := initShallowClone(t, src, "https://github.com/acme/widget")

	// Verify shallow state before the test runs.
	require.FileExists(t, filepath.Join(dest, ".git", "shallow"),
		"test setup: shallow clone must have .git/shallow before collectorsFor")

	recorder := &gitCallRecorder{}

	entity := &profile.Entity{
		ID:           "e1",
		CanonicalURI: "repo:github/acme/widget",
		URL:          "https://github.com/acme/widget",
	}

	_, err := collectorsFor(t.Context(), entity, CollectOpts{
		Path:  dest,
		Clone: true,
		RunGit: func(ctx context.Context, workdir string, args ...string) error {
			recorder.record(ctx, workdir, args...) //nolint:errcheck // recorder always returns nil
			// Perform the real unshallow when that's what's requested,
			// so the filesystem assertion afterward reflects reality.
			if len(args) >= 2 && args[0] == "fetch" {
				if slices.Contains(args, "--unshallow") {
					mustRunGitInTest(t, dest, "remote", "set-url", "origin", "file://"+src)
					mustRunGitInTest(t, dest, "fetch", "--unshallow")
					mustRunGitInTest(t, dest, "remote", "set-url", "origin", "https://github.com/acme/widget")
					return nil
				}
				mustRunGitInTest(t, dest, "remote", "set-url", "origin", "file://"+src)
				mustRunGitInTest(t, dest, "fetch")
				mustRunGitInTest(t, dest, "remote", "set-url", "origin", "https://github.com/acme/widget")
			}
			return nil
		},
	})
	require.NoError(t, err,
		"--clone on a shallow clone must unshallow and succeed")

	// Must NOT have re-cloned.
	assert.False(t, recorder.calledWith("clone"),
		"shallow clone upgrade must NOT trigger a re-clone (preserve existing working tree)")

	// Must have called fetch --unshallow.
	assert.True(t, recorder.calledWithArgs("fetch", "--unshallow"),
		"shallow clone upgrade must call 'git fetch --unshallow' (or equivalent with those args)")

	// .git/shallow must be gone after unshallow.
	assert.NoFileExists(t, filepath.Join(dest, ".git", "shallow"),
		"after unshallow, .git/shallow must no longer exist")
}

// TestAnalyze_PathOnlyRefusesShallowClone asserts that
// `--refresh --path=dest` (no --clone) where dest holds a shallow
// clone returns an error referencing "shallow" and pointing toward
// --clone.
//
// This is the non-clone path: the user passed an existing clone but
// didn't ask for --clone. Today validateExistingClone checks .git
// existence and origin, but does NOT check for .git/shallow. The new
// behavior is to also reject shallow clones in this path.
//
// RED assertion: error returned (today: nil from validateExistingClone).
// Error wraps ErrShallowClone OR error message contains "shallow" and
// references "--clone".
func TestAnalyze_PathOnlyRefusesShallowClone(t *testing.T) {
	t.Parallel()

	src := initSourceRepoWithCommits(t, 2, "")
	dest := initShallowClone(t, src, "https://github.com/acme/widget")

	globals := testGlobals(t)

	cmd := &AnalyzeCmd{
		Target:  "https://github.com/acme/widget",
		Refresh: true,
		Clone:   false, // --path only, no --clone
		Path:    dest,
	}
	err := cmd.Run(globals)

	// Must return an error — today returns nil (origin matches, no
	// shallow check exists).
	require.Error(t, err,
		"--path pointing at a shallow clone must return an error (not proceed with degraded signals); today returns nil")

	// Error must either wrap ErrShallowClone or contain both "shallow"
	// and "--clone" so the user knows the remedy.
	eitherCondition := errors.Is(err, ErrShallowClone) ||
		(strings.Contains(err.Error(), "shallow") && strings.Contains(err.Error(), "--clone"))
	assert.True(t, eitherCondition,
		"error must wrap ErrShallowClone OR contain 'shallow' and '--clone' to guide remediation; got: %v", err)
}

// TestAnalyzeClone_SentinelErrors_Stable pins ErrShallowClone
// (added in this RED phase) as a stable sentinel, consistent with
// TestCollectorsFor_SentinelErrors in collectors_test.go.
func TestAnalyzeClone_SentinelErrors_Stable(t *testing.T) {
	t.Parallel()

	assert.True(t, errors.Is(ErrShallowClone, ErrShallowClone),
		"ErrShallowClone must be a stable sentinel comparable via errors.Is")
}
