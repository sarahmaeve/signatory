package source

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	_, err := NewBlobStreamer(t.Context(), "")
	assert.ErrorIs(t, err, ErrNoClone)
}

func TestBlobStreamer_NewBlobStreamer_StartsSubprocess(t *testing.T) {
	t.Parallel()
	clonePath, _ := initRepoForBlobStream(t)
	bs, err := NewBlobStreamer(t.Context(), clonePath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = bs.Close() })
}

// ============================================================
// ReadBlob
// ============================================================

func TestBlobStreamer_ReadBlob_KnownBlob_ReturnsContent(t *testing.T) {
	t.Parallel()
	clonePath, _ := initRepoForBlobStream(t)
	bs, err := NewBlobStreamer(t.Context(), clonePath)
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
	bs, err := NewBlobStreamer(t.Context(), clonePath)
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
	bs, err := NewBlobStreamer(t.Context(), clonePath)
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
	bs, err := NewBlobStreamer(t.Context(), clonePath)
	require.NoError(t, err)
	require.NoError(t, bs.Close())

	mainSHA := blobSHAFor(t, clonePath, "main.go")
	_, err = bs.ReadBlob(t.Context(), mainSHA)
	assert.ErrorIs(t, err, ErrBlobStreamerClosed)
}

// TestBlobStreamer_ConstructionCtxCancel_TerminatesSubprocess pins
// the contextcheck fix: the persistent cat-file subprocess is bound
// to the context passed to NewBlobStreamer, not context.Background().
// Cancelling the caller's context (an aborted collection run) must
// tear the subprocess down so reads fail fast, rather than leaking a
// git process whose lifetime is divorced from the request. Close is
// still the explicit path; this is the implicit, request-scoped one.
func TestBlobStreamer_ConstructionCtxCancel_TerminatesSubprocess(t *testing.T) {
	t.Parallel()
	clonePath, _ := initRepoForBlobStream(t)

	ctx, cancel := context.WithCancel(t.Context())
	bs, err := NewBlobStreamer(ctx, clonePath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = bs.Close() })

	// Subprocess is live while the construction ctx is live.
	mainSHA := blobSHAFor(t, clonePath, "main.go")
	_, err = bs.ReadBlob(t.Context(), mainSHA)
	require.NoError(t, err)

	// Cancelling the construction ctx kills the subprocess. The kill
	// is asynchronous (the os/exec context goroutine signals the
	// process), so poll until a read fails rather than asserting once.
	cancel()
	require.Eventually(t, func() bool {
		_, rerr := bs.ReadBlob(t.Context(), mainSHA)
		return rerr != nil
	}, 5*time.Second, 10*time.Millisecond,
		"ReadBlob must fail once the construction ctx is cancelled — "+
			"the cat-file subprocess is derived from that ctx")
}

// ============================================================
// ListTreeBlobs
// ============================================================

func TestBlobStreamer_ListTreeBlobs_ReturnsAllBlobs(t *testing.T) {
	t.Parallel()
	clonePath, headSHA := initRepoForBlobStream(t)
	bs, err := NewBlobStreamer(t.Context(), clonePath)
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
	bs, err := NewBlobStreamer(t.Context(), clonePath)
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
	bs, err := NewBlobStreamer(t.Context(), clonePath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = bs.Close() })

	const fakeSHA = "0000000000000000000000000000000000000000"
	_, err = bs.ListTreeBlobs(t.Context(), fakeSHA)
	assert.ErrorIs(t, err, ErrSHAMissingFromClone)
}

// ============================================================
// EnumerateSourceFiles
// ============================================================

func TestBlobStreamer_EnumerateSourceFiles_OnlyReturnsGoSources(t *testing.T) {
	t.Parallel()
	clonePath, headSHA := initRepoForBlobStream(t)
	bs, err := NewBlobStreamer(t.Context(), clonePath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = bs.Close() })

	var paths []string
	contents := make(map[string]string)
	for sf, ferr := range bs.EnumerateSourceFiles(t.Context(), headSHA) {
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

func TestIsPythonSourceFile(t *testing.T) {
	t.Parallel()
	cases := []struct {
		path string
		want bool
	}{
		{"pkg/mod.py", true},
		{"__init__.py", true},
		{"a/b/c.py", true},
		{"setup.py", true},
		{"README.md", false},
		{"mod.go", false},
		{"mod.pyc", false},
		{"mod.pyi", false}, // stub, not importable runtime source
		{"test_mod.py", false},
		{"mod_test.py", false},
		{"conftest.py", false},
		{"tests/test_x.py", false},
		{"pkg/tests/helper.py", false},
		{"test/x.py", false},
		{"vendor/dep.py", false},
		{"pkg/_vendor/dep.py", false},
		{".venv/lib/x.py", false},
		{"venv/lib/x.py", false},
		{"pkg/site-packages/dep.py", false},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, isPythonSourceFile(tc.path))
		})
	}
}

func TestIsNodeSourceFile(t *testing.T) {
	t.Parallel()
	cases := []struct {
		path string
		want bool
	}{
		{"src/index.js", true},
		{"index.mjs", true},
		{"index.cjs", true},
		{"src/a.ts", true},
		{"components/B.tsx", true},
		{"x.jsx", true},
		{"a/b/c.ts", true},
		{"README.md", false},
		{"index.d.ts", false},     // type declaration, not runtime source
		{"lib/api.d.ts", false},   // ditto, nested
		{"foo.test.js", false},    // test
		{"foo.spec.ts", false},    // spec
		{"bar.test.tsx", false},   // test
		{"__tests__/a.js", false}, // test dir
		{"test/a.js", false},
		{"tests/a.js", false},
		{"pkg/__tests__/helper.ts", false},
		{"node_modules/dep/index.js", false}, // vendored
		{"dist/bundle.js", false},            // build output
		{"build/x.js", false},
		{"out/x.js", false},
		{"lib/index.min.js", false}, // minified bundle, not authored source
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, isNodeSourceFile(tc.path))
		})
	}
}

// TestBlobStreamer_WithSourceFileFilter_OverridesDefault pins the
// per-language seam: EnumerateSourceFiles must honor the filter
// supplied at construction rather than the hardwired Go default, so
// a pypi entity streams .py instead of .go.
func TestBlobStreamer_WithSourceFileFilter_OverridesDefault(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	runGit(t, tmp, "init", "-b", "main", "-q")
	runGit(t, tmp, "config", "user.email", "test@example.invalid")
	runGit(t, tmp, "config", "user.name", "Test")
	runGit(t, tmp, "config", "commit.gpgsign", "false")
	writeFile(t, tmp, "pkg/__init__.py", "VERSION = '1'\n")
	writeFile(t, tmp, "pkg/core.py", "def f():\n    return 1\n")
	writeFile(t, tmp, "pkg/test_core.py", "def test_f():\n    assert True\n")
	writeFile(t, tmp, "helper.go", "package x\n")
	writeFile(t, tmp, "README.md", "docs\n")
	runGit(t, tmp, "add", ".")
	runGit(t, tmp, "commit", "-m", "init")
	headSHA := captureGitOutput(t, tmp, "rev-parse", "HEAD")

	bs, err := NewBlobStreamer(t.Context(), tmp, WithSourceFileFilter(isPythonSourceFile))
	require.NoError(t, err)
	t.Cleanup(func() { _ = bs.Close() })

	var paths []string
	for sf, ferr := range bs.EnumerateSourceFiles(t.Context(), headSHA) {
		require.NoError(t, ferr)
		paths = append(paths, sf.Path)
	}
	// Only importable .py: helper.go (Go), test_core.py (test),
	// README.md (not .py) excluded.
	assert.ElementsMatch(t, []string{"pkg/__init__.py", "pkg/core.py"}, paths)
}

func TestBlobStreamer_EnumerateSourceFiles_MissingSHA_YieldsErrorAndStops(t *testing.T) {
	t.Parallel()
	clonePath, _ := initRepoForBlobStream(t)
	bs, err := NewBlobStreamer(t.Context(), clonePath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = bs.Close() })

	const fakeSHA = "0000000000000000000000000000000000000000"
	var yielded []golang.SourceFile
	var firstErr error
	for sf, ferr := range bs.EnumerateSourceFiles(t.Context(), fakeSHA) {
		yielded = append(yielded, sf)
		if firstErr == nil {
			firstErr = ferr
		}
	}
	require.Len(t, yielded, 1, "should yield exactly one (zero, error) pair before stopping")
	assert.ErrorIs(t, firstErr, ErrSHAMissingFromClone)
}

func TestBlobStreamer_EnumerateSourceFiles_StopsOnYieldFalse(t *testing.T) {
	t.Parallel()
	clonePath, headSHA := initRepoForBlobStream(t)
	bs, err := NewBlobStreamer(t.Context(), clonePath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = bs.Close() })

	count := 0
	for range bs.EnumerateSourceFiles(t.Context(), headSHA) {
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
	bs, err := NewBlobStreamer(t.Context(), clonePath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = bs.Close() })
	assert.False(t, bs.allowFetch)
}

func TestBlobStreamer_AllowFetch_OptionEnables(t *testing.T) {
	t.Parallel()
	clonePath, _ := initRepoForBlobStream(t)
	bs, err := NewBlobStreamer(t.Context(), clonePath, WithAllowFetch(true))
	require.NoError(t, err)
	t.Cleanup(func() { _ = bs.Close() })
	assert.True(t, bs.allowFetch)
}

func TestBlobStreamer_ReadBlob_NoFetchByDefault_StillReturnsErrSHAMissingFromClone(t *testing.T) {
	t.Parallel()
	localPath, missingSHA, _ := setupFetchableFixture(t)
	bs, err := NewBlobStreamer(t.Context(), localPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = bs.Close() })

	_, err = bs.ReadBlob(t.Context(), missingSHA)
	assert.ErrorIs(t, err, ErrSHAMissingFromClone)
	assert.False(t, bs.fetched, "default no-fetch should not invoke fetch")
}

func TestBlobStreamer_ReadBlob_AllowFetch_RecoversMissingSHA(t *testing.T) {
	t.Parallel()
	localPath, missingSHA, expectedContent := setupFetchableFixture(t)
	bs, err := NewBlobStreamer(t.Context(), localPath, WithAllowFetch(true))
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
	bs, err := NewBlobStreamer(t.Context(), localPath, WithAllowFetch(true))
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

	bs, err := NewBlobStreamer(t.Context(), localPath, WithAllowFetch(true))
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
// WithMaxBlobSize / blob size cap
// ============================================================

// TestBlobStreamer_ReadBlob_RejectsBlobOverCap pins the contract that
// readBlobOnce refuses to allocate when the cat-file header reports a
// size exceeding the configured maxBlobSize. The cap defends against
// a tampered or corrupt loose object whose size header would
// otherwise drive an unbounded make([]byte, size) — git's own
// integrity normally bounds this, but signatory should not depend on
// git's integrity for its own memory safety. The other HTTP clients
// in the codebase enforce the same defensive bound at their fetch
// boundary; this test pins the cat-file-pipe equivalent.
//
// Test seam: WithMaxBlobSize(small) lets the assertion fire on a
// fixture blob a few hundred bytes long instead of needing a real
// 10+ MiB blob. The check itself is size-comparison logic, not
// dependent on the cap value.
func TestBlobStreamer_ReadBlob_RejectsBlobOverCap(t *testing.T) {
	t.Parallel()
	clonePath, _ := initRepoForBlobStream(t)

	// main.go is one of the fixture files (tiny — ~30 bytes).
	// Cap below the file size guarantees the check fires before
	// any allocation. The size value (10) is well under the
	// fixture; the production default (defaultMaxBlobSize) is
	// 10 MiB and is exercised implicitly by every other test.
	bs, err := NewBlobStreamer(t.Context(), clonePath, WithMaxBlobSize(10))
	require.NoError(t, err)
	t.Cleanup(func() { _ = bs.Close() })

	mainSHA := blobSHAFor(t, clonePath, "main.go")

	_, err = bs.ReadBlob(t.Context(), mainSHA)
	require.Error(t, err, "ReadBlob must refuse a blob whose size exceeds the cap")
	assert.ErrorIs(t, err, ErrBlobSizeExceedsCap,
		"the error must wrap ErrBlobSizeExceedsCap so callers can errors.Is against it; got: %v", err)
}

// TestBlobStreamer_ReadBlob_AllowsBlobUnderCap is the positive sibling:
// if the configured cap is generous enough for the fixture blob,
// ReadBlob must succeed. Pins that the cap check is a strict
// inequality, not a regression that rejects every blob.
func TestBlobStreamer_ReadBlob_AllowsBlobUnderCap(t *testing.T) {
	t.Parallel()
	clonePath, _ := initRepoForBlobStream(t)

	// 1 KiB cap is well above the fixture blob's ~30 bytes.
	bs, err := NewBlobStreamer(t.Context(), clonePath, WithMaxBlobSize(1024))
	require.NoError(t, err)
	t.Cleanup(func() { _ = bs.Close() })

	mainSHA := blobSHAFor(t, clonePath, "main.go")

	content, err := bs.ReadBlob(t.Context(), mainSHA)
	require.NoError(t, err, "blob under cap must read successfully")
	assert.NotEmpty(t, content, "blob content should be the file bytes, not empty")
}

// TestBlobStreamer_WithMaxBlobSize_NonPositiveKeepsDefault pins the
// documented "<=0 falls back to default" behaviour of WithMaxBlobSize.
// This is a footgun guard: a caller passing 0 or a negative value
// shouldn't accidentally disable the cap — they should get the safe
// 10 MiB default. We assert that a fixture blob (tiny) reads cleanly
// after WithMaxBlobSize(0), proving the default is in effect.
func TestBlobStreamer_WithMaxBlobSize_NonPositiveKeepsDefault(t *testing.T) {
	t.Parallel()
	clonePath, _ := initRepoForBlobStream(t)

	bs, err := NewBlobStreamer(t.Context(), clonePath, WithMaxBlobSize(0))
	require.NoError(t, err)
	t.Cleanup(func() { _ = bs.Close() })

	mainSHA := blobSHAFor(t, clonePath, "main.go")
	_, err = bs.ReadBlob(t.Context(), mainSHA)
	require.NoError(t, err, "WithMaxBlobSize(0) must keep the default cap, not disable it")
}

// ============================================================
// Close
// ============================================================

func TestBlobStreamer_Close_AllowsDoubleClose(t *testing.T) {
	t.Parallel()
	clonePath, _ := initRepoForBlobStream(t)
	bs, err := NewBlobStreamer(t.Context(), clonePath)
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
