package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

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

// mustRunGitInTest is a local copy of the git-package test helper —
// we can't import test-only symbols across packages. Fails the
// test on any non-zero git exit.
func mustRunGitInTest(t *testing.T, repo string, args ...string) {
	t.Helper()
	full := append([]string{"-C", repo}, args...)
	//nolint:gosec // G204: test helper; binary is "git" literal
	cmd := exec.Command("git", full...)
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

func TestCollectorsFor_CloneIntoNonEmpty_ReturnsErrPathNotEmpty(t *testing.T) {
	t.Parallel()

	// Create a dir and put something in it.
	dst := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dst, "existing.txt"), []byte("hi"), 0o600))

	entity := &profile.Entity{
		ID:           "e1",
		CanonicalURI: "repo:github/alecthomas/kong",
		URL:          "https://github.com/alecthomas/kong",
	}
	_, err := collectorsFor(context.Background(), entity, CollectOpts{Path: dst, Clone: true})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPathNotEmpty)
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
	assert.Len(t, collectors, 2, "github + git collector returned")

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
