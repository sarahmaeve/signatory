package git

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/sarahmaeve/signatory/internal/gitenv"
	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// initRepo creates a minimal git working-tree clone at tmp. Shared
// by every integration test in this package. Configures user
// identity and disables GPG signing at the repo level so the test
// is robust against a host system where the operator has enabled
// commit.gpgsign globally.
//
// Returns the repo root; the caller uses it as the collector path.
func initRepo(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	mustRunGit(t, tmp, "init", "-b", "main", "-q")
	// Repo-local config. Each line cannot leak to the host gitconfig.
	mustRunGit(t, tmp, "config", "user.email", "test@example.invalid")
	mustRunGit(t, tmp, "config", "user.name", "Test User")
	mustRunGit(t, tmp, "config", "commit.gpgsign", "false")
	mustRunGit(t, tmp, "config", "tag.gpgSign", "false")
	return tmp
}

// mustRunGit runs `git -C repo <args...>` and fails the test on any
// non-zero exit. Used for setup steps where any failure is a bug
// in the test, not an assertion target.
//
// Uses gitenv.SafeEnv() to strip GIT_DIR, GIT_CONFIG_*, and the
// other config-injection / redirection env vars from the inherited
// environment. Without that, an ambient GIT_DIR (set by a git hook
// or IDE integration) would cause `git -C <tempdir>` to still
// resolve writes against the inherited config path — which for a
// git worktree is the shared main repo's config. The postmortem
// for the 2026-04-24 worktree corruption traced to exactly that
// mechanism; every exec.Command("git", ...) in tests must set
// cmd.Env = gitenv.SafeEnv() before running.
func mustRunGit(t *testing.T, repo string, args ...string) {
	t.Helper()
	full := append([]string{"-C", repo}, args...)
	// t.Context() ties the subprocess lifetime to the test's — if
	// the test times out or is cancelled, pending git invocations
	// get killed instead of orphaned. Go 1.24+.
	//nolint:gosec // G204: test helper; binary is "git" literal, args are test-controlled
	cmd := exec.CommandContext(t.Context(), "git", full...)
	cmd.Env = gitenv.SafeEnv()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, stderr.String())
	}
}

// commitEmpty creates one empty commit with the given message. Used
// as the test's primitive for "advance the repo by a commit without
// introducing file changes."
func commitEmpty(t *testing.T, repo, msg string) {
	t.Helper()
	mustRunGit(t, repo, "commit", "--allow-empty", "-m", msg)
}

func TestCollector_NoClone_ReturnsErrNoClone(t *testing.T) {
	t.Parallel()

	c := NewCollector("/does/not/exist")
	_, err := c.Collect(context.Background(), &profile.Entity{ID: "e1"})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNoClone)
}

func TestCollector_EmptyPath_ReturnsErrNoClone(t *testing.T) {
	t.Parallel()

	c := NewCollector("")
	_, err := c.Collect(context.Background(), &profile.Entity{ID: "e1"})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNoClone)
}

func TestCollector_PathExistsButNotAGitRepo_ReturnsErrNoClone(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir() // exists, but has no .git
	c := NewCollector(tmp)
	_, err := c.Collect(context.Background(), &profile.Entity{ID: "e1"})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNoClone)
}

func TestCollector_EmptyRepo_RecordsAbsence(t *testing.T) {
	// A freshly-initialized repo has no commits. The collector
	// should treat this as a definitive absence — the commit
	// window is empty because nothing's there, not because of
	// an upstream error.
	t.Parallel()

	repo := initRepo(t)
	c := NewCollector(repo)

	result, err := c.Collect(context.Background(), &profile.Entity{ID: "empty-repo"})
	require.NoError(t, err)
	require.NotNil(t, result)

	// Both signing signals should be present as absences.
	signalsByType := indexByType(result)
	for _, typeName := range []string{
		"per_developer_commit_signing_ratio",
		"web_flow_signing_ratio",
	} {
		entry, ok := signalsByType[typeName]
		require.True(t, ok, "expected absence for %q", typeName)
		assert.Nil(t, entry.Signal, "%q should be recorded as absence, not signal", typeName)
		require.NotNil(t, entry.Absence, "%q absence must carry an Absence record", typeName)
	}

	// No failures — absence is distinct from failure.
	assert.Empty(t, result.Failures, "empty repo is an absence case, not a failure case")
}

func TestCollector_UnsignedCommits_AllZeroRatios(t *testing.T) {
	// Three commits, none signed. Expect both signals emitted,
	// both ratios 0, all three commits accounted for as unsigned.
	t.Parallel()

	repo := initRepo(t)
	commitEmpty(t, repo, "first")
	commitEmpty(t, repo, "second")
	commitEmpty(t, repo, "third")

	c := NewCollector(repo)
	result, err := c.Collect(context.Background(), &profile.Entity{ID: "unsigned-repo"})
	require.NoError(t, err)

	perDev := findSignal(t, result, "per_developer_commit_signing_ratio")
	webFlow := findSignal(t, result, "web_flow_signing_ratio")

	perDevVal := unmarshalValue(t, perDev)
	assert.Equal(t, float64(3), perDevVal["total_commits"])
	assert.Equal(t, float64(0), perDevVal["per_developer_signed"])
	assert.Equal(t, float64(0), perDevVal["web_flow_signed"])
	assert.Equal(t, float64(3), perDevVal["unsigned"])
	assert.Equal(t, float64(0), perDevVal["ratio"])

	webFlowVal := unmarshalValue(t, webFlow)
	assert.Equal(t, float64(3), webFlowVal["total_commits"])
	assert.Equal(t, float64(0), webFlowVal["ratio"])
}

func TestCollector_WindowExcludesOldCommits(t *testing.T) {
	// A commit whose date sits outside the collector's window must
	// not appear in the signing-ratio computation. We use a tiny
	// window (1 second) and a commit backdated by far more than
	// that via GIT_COMMITTER_DATE to exercise the filter.
	t.Parallel()

	repo := initRepo(t)

	// Backdated commit — outside any reasonable window.
	backdate := "2020-01-01T00:00:00Z"
	//nolint:gosec // G204: test helper
	oldCmd := exec.CommandContext(t.Context(), "git", "-C", repo, "commit", "--allow-empty", "-m", "ancient")
	// Start from gitenv.SafeEnv() — strip dangerous inherited
	// vars — then append the backdating overrides. Inheriting
	// from cmd.Environ() would leak GIT_DIR / GIT_CONFIG_* and
	// risk writes against the shared worktree config.
	oldCmd.Env = append(gitenv.SafeEnv(),
		"GIT_AUTHOR_DATE="+backdate,
		"GIT_COMMITTER_DATE="+backdate,
	)
	require.NoError(t, oldCmd.Run())

	// A window narrow enough to exclude the ancient commit.
	c := NewCollector(repo).WithWindow(24 * time.Hour)
	result, err := c.Collect(context.Background(), &profile.Entity{ID: "windowed"})
	require.NoError(t, err)

	// The ancient commit is outside the window, and there are no
	// commits inside → absence, not a signal with ratio=0.
	signalsByType := indexByType(result)
	entry := signalsByType["per_developer_commit_signing_ratio"]
	require.NotNil(t, entry.Absence,
		"narrow-window run with only out-of-window commits should record absence")
	assert.Contains(t, entry.Absence.Reason, "window",
		"absence reason should name the window")
}

func TestCollector_ContextCancelled_PropagatesError(t *testing.T) {
	// Cancelling the context before Collect runs should surface
	// as a failure on the signing signals rather than a panic.
	t.Parallel()

	repo := initRepo(t)
	commitEmpty(t, repo, "one")

	c := NewCollector(repo)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel BEFORE calling Collect

	result, err := c.Collect(ctx, &profile.Entity{ID: "cancelled"})
	require.NoError(t, err, "Collect itself should not error on context cancel; individual signals fail")

	// The per_developer_commit_signing_ratio collector uses runGit,
	// which bails fast on ctx.Err(); its signing failure is the
	// canonical outcome we're checking for. Without this assertion
	// an unrelated future failure (e.g., a panic in another collector)
	// would silently satisfy a "len(Failures) > 0" check and mask
	// a regression in the cancellation-propagation path itself.
	require.NotEmpty(t, result.Failures,
		"cancelled context must produce at least one failure")

	signingFailed := false
	sawCancellationShape := false
	for _, f := range result.Failures {
		if f.SignalType == "per_developer_commit_signing_ratio" {
			signingFailed = true
		}
		// CollectionError.Reason is a sanitized string (see
		// internal/signal/errors.go), not a wrapped error, so
		// errors.Is is unavailable. The runGit error for a
		// pre-cancelled ctx is "git [...]: context canceled"
		// (from context.Canceled.Error()); the sanitize pass
		// only replaces the clone path with "<clone>", leaving
		// the "context canceled" substring intact.
		if strings.Contains(strings.ToLower(f.Reason), "canceled") ||
			strings.Contains(strings.ToLower(f.Reason), "cancelled") {
			sawCancellationShape = true
		}
	}
	assert.True(t, signingFailed,
		"per_developer_commit_signing_ratio (runGit-backed) must fail on a pre-cancelled context")
	assert.True(t, sawCancellationShape,
		"at least one failure's Reason must name cancellation; "+
			"an unrelated failure would indicate the cancellation "+
			"wasn't actually the cause")
}

// ---- helpers (test-only) ----

// indexByType returns a map keyed on signal type for easy assertion
// against a CollectionResult. Each entry may carry either a Signal
// (successful collection) or an Absence (definitive "not there"),
// never both.
func indexByType(result *signal.CollectionResult) map[string]signal.SignalOrAbsence {
	out := map[string]signal.SignalOrAbsence{}
	for _, entry := range result.Collected {
		switch {
		case entry.Signal != nil:
			out[entry.Signal.Type] = entry
		case entry.Absence != nil:
			out[entry.Absence.SignalType] = entry
		}
	}
	return out
}

// findSignal asserts the result contains a successful Signal for
// the given type and returns it. Fails the test if the type is
// missing or recorded as an absence.
func findSignal(t *testing.T, result *signal.CollectionResult, typeName string) *profile.Signal {
	t.Helper()
	for _, entry := range result.Collected {
		if entry.Signal != nil && entry.Signal.Type == typeName {
			return entry.Signal
		}
	}
	t.Fatalf("no successful signal of type %q in result (collected=%d failures=%d)",
		typeName, len(result.Collected), len(result.Failures))
	return nil
}

// unmarshalValue decodes a Signal.Value into a generic map for
// easy field-level assertion.
func unmarshalValue(t *testing.T, s *profile.Signal) map[string]any {
	t.Helper()
	var out map[string]any
	require.NoError(t, json.Unmarshal(s.Value, &out))
	return out
}

// Sanity check that ErrNoClone is a sentinel, not a moving target —
// callers use errors.Is on it and the invariant is that the value is
// stable across the package's lifetime.
func TestErrNoClone_IsSentinel(t *testing.T) {
	t.Parallel()
	assert.True(t, errors.Is(ErrNoClone, ErrNoClone))
	assert.Equal(t, "git collector: no local clone at path", ErrNoClone.Error())
}

// TestCommitSigningFormat_ContainsRequiredSeparators guards the
// load-bearing invariant that the git-log format string contains
// the placeholder tokens the parser depends on. git evaluates
// `%x1f` / `%x1e` inside format strings as the literal bytes
// 0x1F / 0x1E at output time — the format string Go ships contains
// the textual placeholders, not the bytes themselves.
func TestCommitSigningFormat_ContainsRequiredSeparators(t *testing.T) {
	t.Parallel()
	assert.Contains(t, commitSigningFormat, "%x1f", "format must use git's %x1f token as field separator")
	assert.Contains(t, commitSigningFormat, "%x1e", "format must use git's %x1e token as record terminator")
	// 6 placeholders ({%H, %aN, %aE, %G?, %GS, %GK}) → 5 field separators between them.
	fieldSepCount := bytes.Count([]byte(commitSigningFormat), []byte("%x1f"))
	assert.Equal(t, 5, fieldSepCount, "expected 5 %%x1f tokens between 6 field placeholders")
	recordTermCount := bytes.Count([]byte(commitSigningFormat), []byte("%x1e"))
	assert.Equal(t, 1, recordTermCount, "expected exactly one %%x1e record terminator")
}
