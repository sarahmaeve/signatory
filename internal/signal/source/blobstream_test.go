package source

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/gitenv"
	"github.com/sarahmaeve/signatory/internal/signal/source/golang"
)

// runGit runs `git -C dir args...` via gitenv.NewCmd and fails the
// test on any non-zero exit. Same shape as the helper in
// internal/signal/git/collector_test.go; duplicated here to keep
// the source package self-contained.
//
// Use this for LOCAL porcelain only — init, config, add, commit,
// rev-parse, etc. For clone-shaped operations (clone, fetch, push,
// pull), use runGitClone instead — those may fork ssh/credential-
// helper grandchildren that need NewCloneCmd's WaitDelay
// discipline. See gitenv package docs for the boundary rationale.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	full := append([]string{"-C", dir}, args...)
	cmd := gitenv.NewCmd(t.Context(), full...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, stderr.String())
	}
}

// runGitClone runs `git clone args...` via gitenv.NewCloneCmd. The
// distinction from runGit matters: even local file:// clone can
// spawn credential helpers in some configurations, and clone is
// the canonical site listed in the gitenv package doc as requiring
// NewCloneCmd's WaitDelay discipline.
//
// Note: no -C dir prefix — clone takes <source> <dest> directly.
func runGitClone(t *testing.T, args ...string) {
	t.Helper()
	full := append([]string{"clone"}, args...)
	cmd := gitenv.NewCloneCmd(t.Context(), full...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %v: %v: %s", full, err, stderr.String())
	}
}

// captureGitOutput runs git and returns stdout. Used to look up
// HEAD's SHA for tests that need a real commit hash.
func captureGitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-C", dir}, args...)
	cmd := gitenv.NewCmd(t.Context(), full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, stderr.String())
	}
	return strings.TrimSpace(stdout.String())
}

// writeFile writes content to dir/relPath, creating intermediate
// directories as needed. Used by initRepoForBlobStream to set up
// the test fixture's content tree.
func writeFile(t *testing.T, dir, relPath, content string) {
	t.Helper()
	full := filepath.Join(dir, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", relPath, err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", relPath, err)
	}
}

// initRepoForBlobStream creates a git repo with a controlled file
// layout: two .go files at root, one _test.go (filtered out), one
// vendored .go (filtered out), and a non-Go README. Returns the
// repo's clone path and HEAD's commit SHA.
func initRepoForBlobStream(t *testing.T) (clonePath, headSHA string) {
	t.Helper()
	tmp := t.TempDir()
	runGit(t, tmp, "init", "-b", "main", "-q")
	runGit(t, tmp, "config", "user.email", "test@example.invalid")
	runGit(t, tmp, "config", "user.name", "Test User")
	runGit(t, tmp, "config", "commit.gpgsign", "false")

	writeFile(t, tmp, "main.go", "package main\n\nfunc main() {}\n")
	writeFile(t, tmp, "lib.go", "package main\n\nfunc Hello() string { return \"hi\" }\n")
	writeFile(t, tmp, "main_test.go", "package main\n\n// would-be test file\n")
	writeFile(t, tmp, "vendor/dep/dep.go", "package dep\n\n// vendored\n")
	writeFile(t, tmp, "internal/util/util.go", "package util\n\n// internal helper\n")
	writeFile(t, tmp, "README.md", "Documentation\n")

	runGit(t, tmp, "add", ".")
	runGit(t, tmp, "commit", "-m", "initial")

	headSHA = captureGitOutput(t, tmp, "rev-parse", "HEAD")
	return tmp, headSHA
}

// blobSHAFor returns the blob SHA for relPath at HEAD via
// `git rev-parse HEAD:<path>`. Used by tests that read individual
// blobs and need their SHAs.
func blobSHAFor(t *testing.T, clonePath, relPath string) string {
	t.Helper()
	return captureGitOutput(t, clonePath, "rev-parse", "HEAD:"+relPath)
}

// ============================================================
// NewBlobStreamer
// ============================================================

func TestBlobStreamer_NoClonePath_ReturnsErrNoClone(t *testing.T) {
	t.Parallel()
	_, err := NewBlobStreamer("")
	assert.ErrorIs(t, err, ErrNoClone)
}

func TestBlobStreamer_NewBlobStreamer_StartsSubprocess(t *testing.T) {
	t.Parallel()
	clonePath, _ := initRepoForBlobStream(t)
	bs, err := NewBlobStreamer(clonePath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = bs.Close() })
}

// ============================================================
// ReadBlob
// ============================================================

func TestBlobStreamer_ReadBlob_KnownBlob_ReturnsContent(t *testing.T) {
	t.Parallel()
	clonePath, _ := initRepoForBlobStream(t)
	bs, err := NewBlobStreamer(clonePath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = bs.Close() })

	mainSHA := blobSHAFor(t, clonePath, "main.go")
	content, err := bs.ReadBlob(t.Context(), mainSHA)
	require.NoError(t, err)
	assert.Equal(t, "package main\n\nfunc main() {}\n", string(content))
}

func TestBlobStreamer_ReadBlob_MissingSHA_ReturnsErrSHAMissingFromClone(t *testing.T) {
	t.Parallel()
	clonePath, _ := initRepoForBlobStream(t)
	bs, err := NewBlobStreamer(clonePath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = bs.Close() })

	// 40-hex SHA that won't exist in this clone.
	const fakeSHA = "0000000000000000000000000000000000000000"
	_, err = bs.ReadBlob(t.Context(), fakeSHA)
	assert.ErrorIs(t, err, ErrSHAMissingFromClone)
}

func TestBlobStreamer_ReadBlob_MultipleSequential_AllSucceed(t *testing.T) {
	t.Parallel()
	clonePath, _ := initRepoForBlobStream(t)
	bs, err := NewBlobStreamer(clonePath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = bs.Close() })

	// Read several blobs in sequence to validate the persistent
	// subprocess handles repeated requests cleanly.
	mainSHA := blobSHAFor(t, clonePath, "main.go")
	libSHA := blobSHAFor(t, clonePath, "lib.go")
	readmeSHA := blobSHAFor(t, clonePath, "README.md")

	main, err := bs.ReadBlob(t.Context(), mainSHA)
	require.NoError(t, err)
	lib, err := bs.ReadBlob(t.Context(), libSHA)
	require.NoError(t, err)
	readme, err := bs.ReadBlob(t.Context(), readmeSHA)
	require.NoError(t, err)

	assert.Contains(t, string(main), "func main()")
	assert.Contains(t, string(lib), "func Hello()")
	assert.Equal(t, "Documentation\n", string(readme))
}

func TestBlobStreamer_ReadBlob_AfterClose_ReturnsClosed(t *testing.T) {
	t.Parallel()
	clonePath, _ := initRepoForBlobStream(t)
	bs, err := NewBlobStreamer(clonePath)
	require.NoError(t, err)
	require.NoError(t, bs.Close())

	mainSHA := blobSHAFor(t, clonePath, "main.go")
	_, err = bs.ReadBlob(t.Context(), mainSHA)
	assert.ErrorIs(t, err, ErrBlobStreamerClosed)
}

// ============================================================
// ListTreeBlobs
// ============================================================

func TestBlobStreamer_ListTreeBlobs_ReturnsAllBlobs(t *testing.T) {
	t.Parallel()
	clonePath, headSHA := initRepoForBlobStream(t)
	bs, err := NewBlobStreamer(clonePath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = bs.Close() })

	blobs, err := bs.ListTreeBlobs(t.Context(), headSHA)
	require.NoError(t, err)

	paths := make([]string, len(blobs))
	for i, b := range blobs {
		paths[i] = b.Path
	}
	assert.ElementsMatch(t, []string{
		"README.md",
		"internal/util/util.go",
		"lib.go",
		"main.go",
		"main_test.go",
		"vendor/dep/dep.go",
	}, paths)
}

func TestBlobStreamer_ListTreeBlobs_BlobsHaveValidSHAs(t *testing.T) {
	t.Parallel()
	clonePath, headSHA := initRepoForBlobStream(t)
	bs, err := NewBlobStreamer(clonePath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = bs.Close() })

	blobs, err := bs.ListTreeBlobs(t.Context(), headSHA)
	require.NoError(t, err)

	// Each blob's SHA should be a 40-character hex string and
	// resolvable via cat-file.
	for _, blob := range blobs {
		assert.Len(t, blob.SHA, 40, "blob SHA should be 40 hex chars: %+v", blob)
		_, readErr := bs.ReadBlob(t.Context(), blob.SHA)
		assert.NoError(t, readErr, "ReadBlob should succeed for blob from ListTreeBlobs: %+v", blob)
	}
}

func TestBlobStreamer_ListTreeBlobs_MissingSHA_ReturnsErrSHAMissingFromClone(t *testing.T) {
	t.Parallel()
	clonePath, _ := initRepoForBlobStream(t)
	bs, err := NewBlobStreamer(clonePath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = bs.Close() })

	const fakeSHA = "0000000000000000000000000000000000000000"
	_, err = bs.ListTreeBlobs(t.Context(), fakeSHA)
	assert.ErrorIs(t, err, ErrSHAMissingFromClone)
}

// ============================================================
// EnumerateGoFiles
// ============================================================

func TestBlobStreamer_EnumerateGoFiles_OnlyReturnsGoSources(t *testing.T) {
	t.Parallel()
	clonePath, headSHA := initRepoForBlobStream(t)
	bs, err := NewBlobStreamer(clonePath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = bs.Close() })

	var paths []string
	contents := make(map[string]string)
	for sf, ferr := range bs.EnumerateGoFiles(t.Context(), headSHA) {
		require.NoError(t, ferr)
		paths = append(paths, sf.Path)
		contents[sf.Path] = string(sf.Content)
	}

	// main.go and lib.go (top-level) + internal/util/util.go.
	// Excluded: README.md (not .go), main_test.go (test),
	// vendor/dep/dep.go (vendored).
	assert.ElementsMatch(t, []string{
		"main.go",
		"lib.go",
		"internal/util/util.go",
	}, paths)

	assert.Contains(t, contents["main.go"], "func main()")
	assert.Contains(t, contents["lib.go"], "func Hello()")
	assert.Contains(t, contents["internal/util/util.go"], "internal helper")
}

func TestBlobStreamer_EnumerateGoFiles_MissingSHA_YieldsErrorAndStops(t *testing.T) {
	t.Parallel()
	clonePath, _ := initRepoForBlobStream(t)
	bs, err := NewBlobStreamer(clonePath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = bs.Close() })

	const fakeSHA = "0000000000000000000000000000000000000000"
	var yielded []golang.SourceFile
	var firstErr error
	for sf, ferr := range bs.EnumerateGoFiles(t.Context(), fakeSHA) {
		yielded = append(yielded, sf)
		if firstErr == nil {
			firstErr = ferr
		}
	}
	require.Len(t, yielded, 1, "should yield exactly one (zero, error) pair before stopping")
	assert.ErrorIs(t, firstErr, ErrSHAMissingFromClone)
}

func TestBlobStreamer_EnumerateGoFiles_StopsOnYieldFalse(t *testing.T) {
	t.Parallel()
	clonePath, headSHA := initRepoForBlobStream(t)
	bs, err := NewBlobStreamer(clonePath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = bs.Close() })

	count := 0
	for range bs.EnumerateGoFiles(t.Context(), headSHA) {
		count++
		if count == 1 {
			break
		}
	}
	assert.Equal(t, 1, count, "iteration should stop after first yield-false")
}

// ============================================================
// allow-fetch (commit 10)
// ============================================================

// setupFetchableFixture creates a "missing SHA, recoverable via
// fetch" scenario by:
//
//  1. Initializing a source repo with one commit, cloning it to
//     local (which gets the full pack of objects to that point).
//  2. Adding a NEW commit to source AFTER the clone. Local has no
//     knowledge of this commit or its blob.
//  3. Returning local's path plus the new commit's blob SHA, which
//     is genuinely not in local's object DB and is recoverable via
//     `git fetch origin`.
//
// Why not "delete an object from .git/objects": git clone packs
// every object into a single packfile, so deleting an unpacked
// loose object file leaves the SHA still readable from the pack.
// "Source advances after clone" is the simpler design that works
// without packfile surgery.
func setupFetchableFixture(t *testing.T) (localPath, missingBlobSHA, expectedContent string) {
	t.Helper()

	source := t.TempDir()
	runGit(t, source, "init", "-b", "main", "-q")
	runGit(t, source, "config", "user.email", "test@example.invalid")
	runGit(t, source, "config", "user.name", "Test")
	runGit(t, source, "config", "commit.gpgsign", "false")

	writeFile(t, source, "before-clone.txt", "captured by clone\n")
	runGit(t, source, "add", ".")
	runGit(t, source, "commit", "-m", "first")

	// Full clone: local has every object source knows about so far.
	localPath = t.TempDir()
	runGitClone(t, "--quiet", source, localPath)

	// Source advances. The new commit's blob is NOT in local's
	// object DB — local was cloned before this commit existed.
	expectedContent = "added after clone — recoverable via fetch\n"
	writeFile(t, source, "after-clone.txt", expectedContent)
	runGit(t, source, "add", ".")
	runGit(t, source, "commit", "-m", "second")
	missingBlobSHA = captureGitOutput(t, source, "rev-parse", "HEAD:after-clone.txt")

	return localPath, missingBlobSHA, expectedContent
}

func TestBlobStreamer_AllowFetch_DefaultIsFalse(t *testing.T) {
	t.Parallel()
	clonePath, _ := initRepoForBlobStream(t)
	bs, err := NewBlobStreamer(clonePath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = bs.Close() })
	assert.False(t, bs.allowFetch)
}

func TestBlobStreamer_AllowFetch_OptionEnables(t *testing.T) {
	t.Parallel()
	clonePath, _ := initRepoForBlobStream(t)
	bs, err := NewBlobStreamer(clonePath, WithAllowFetch(true))
	require.NoError(t, err)
	t.Cleanup(func() { _ = bs.Close() })
	assert.True(t, bs.allowFetch)
}

func TestBlobStreamer_ReadBlob_NoFetchByDefault_StillReturnsErrSHAMissingFromClone(t *testing.T) {
	t.Parallel()
	localPath, missingSHA, _ := setupFetchableFixture(t)
	bs, err := NewBlobStreamer(localPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = bs.Close() })

	_, err = bs.ReadBlob(t.Context(), missingSHA)
	assert.ErrorIs(t, err, ErrSHAMissingFromClone)
	assert.False(t, bs.fetched, "default no-fetch should not invoke fetch")
}

func TestBlobStreamer_ReadBlob_AllowFetch_RecoversMissingSHA(t *testing.T) {
	t.Parallel()
	localPath, missingSHA, expectedContent := setupFetchableFixture(t)
	bs, err := NewBlobStreamer(localPath, WithAllowFetch(true))
	require.NoError(t, err)
	t.Cleanup(func() { _ = bs.Close() })

	content, err := bs.ReadBlob(t.Context(), missingSHA)
	require.NoError(t, err)
	assert.Equal(t, expectedContent, string(content))
	assert.True(t, bs.fetched, "ensureFetched should have run and succeeded")
}

func TestBlobStreamer_ReadBlob_AllowFetch_FetchRunsAtMostOnce(t *testing.T) {
	t.Parallel()
	localPath, missingSHA, _ := setupFetchableFixture(t)
	bs, err := NewBlobStreamer(localPath, WithAllowFetch(true))
	require.NoError(t, err)
	t.Cleanup(func() { _ = bs.Close() })

	// First call: missing locally, fetched, recovered.
	_, err = bs.ReadBlob(t.Context(), missingSHA)
	require.NoError(t, err)
	assert.True(t, bs.fetched)

	// Second call to a STILL-missing SHA (fake hash that origin
	// also doesn't have): should fail with ErrSHAMissingFromClone
	// without re-fetching. ensureFetched short-circuits because
	// b.fetched is already true.
	const fakeSHA = "0000000000000000000000000000000000000000"
	_, err = bs.ReadBlob(t.Context(), fakeSHA)
	assert.ErrorIs(t, err, ErrSHAMissingFromClone)
}

func TestBlobStreamer_ListTreeBlobs_AllowFetch_RecoversMissingSHA(t *testing.T) {
	t.Parallel()

	// Same fixture pattern as setupFetchableFixture: source
	// advances after the clone, so local doesn't have the second
	// commit's tree object. ls-tree on the second commit's SHA
	// fails initially; allow-fetch recovers.
	source := t.TempDir()
	runGit(t, source, "init", "-b", "main", "-q")
	runGit(t, source, "config", "user.email", "test@example.invalid")
	runGit(t, source, "config", "user.name", "Test")
	runGit(t, source, "config", "commit.gpgsign", "false")
	writeFile(t, source, "first.txt", "first\n")
	runGit(t, source, "add", ".")
	runGit(t, source, "commit", "-m", "first")

	localPath := t.TempDir()
	runGitClone(t, "--quiet", source, localPath)

	// Advance source — local doesn't have this commit yet.
	writeFile(t, source, "second.txt", "second\n")
	runGit(t, source, "add", ".")
	runGit(t, source, "commit", "-m", "second")
	newCommitSHA := captureGitOutput(t, source, "rev-parse", "HEAD")

	bs, err := NewBlobStreamer(localPath, WithAllowFetch(true))
	require.NoError(t, err)
	t.Cleanup(func() { _ = bs.Close() })

	// Without fetch this would be ErrSHAMissingFromClone; allow-
	// fetch should run `git fetch origin` and recover.
	blobs, err := bs.ListTreeBlobs(t.Context(), newCommitSHA)
	require.NoError(t, err)
	assert.NotEmpty(t, blobs)
	// Should include both files now that fetch brought the new commit.
	paths := make([]string, len(blobs))
	for i, b := range blobs {
		paths[i] = b.Path
	}
	assert.ElementsMatch(t, []string{"first.txt", "second.txt"}, paths)
	assert.True(t, bs.fetched)
}

// ============================================================
// Close
// ============================================================

func TestBlobStreamer_Close_AllowsDoubleClose(t *testing.T) {
	t.Parallel()
	clonePath, _ := initRepoForBlobStream(t)
	bs, err := NewBlobStreamer(clonePath)
	require.NoError(t, err)
	require.NoError(t, bs.Close())
	require.NoError(t, bs.Close(), "second Close should be a no-op")
}

// ============================================================
// isGoSourceFile (package-private helper, tested directly because
// the filtering rules are load-bearing for matrix correctness)
// ============================================================

func TestIsGoSourceFile(t *testing.T) {
	t.Parallel()
	cases := []struct {
		path string
		want bool
	}{
		{"main.go", true},
		{"lib/foo.go", true},
		{"internal/util/util.go", true},
		{"deeply/nested/path/file.go", true},
		// Non-.go.
		{"README.md", false},
		{"go.mod", false},
		{"go.sum", false},
		{"main", false},
		// Test files.
		{"main_test.go", false},
		{"lib/foo_test.go", false},
		{"deeply/nested/foo_test.go", false},
		// Vendored.
		{"vendor", false},
		{"vendor/x.go", false},
		{"vendor/foo/bar.go", false},
		{"some/path/vendor/x.go", false},
		// Non-vendor "vendor" prefix in name (false negative
		// acceptable; vendoring is the dominant interpretation).
		{"vendortools.go", true}, // not a vendor dir
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, isGoSourceFile(tc.path), "path=%q", tc.path)
		})
	}
}

// ============================================================
// isMissingObjectMessage
// ============================================================

func TestIsMissingObjectMessage(t *testing.T) {
	t.Parallel()
	cases := []struct {
		stderr string
		want   bool
	}{
		{"fatal: Not a valid object name 0000000\n", true},
		{"fatal: bad object 0000000\n", true},
		{"fatal: ambiguous argument: unknown revision\n", true},
		{"fatal: invalid object name '0000000'.\n", true},
		{"fatal: not a tree object\n", true},
		// Negative cases.
		{"", false},
		{"fatal: not a git repository\n", false},
		{"warning: something else\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.stderr, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, isMissingObjectMessage(tc.stderr))
		})
	}
}
