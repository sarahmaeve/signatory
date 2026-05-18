package source

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// initRepoForDiffStat creates a git repo with two commits whose
// diff exercises DiffStat in well-defined ways:
//
//	commit 1 (sha1): files A.txt, B.txt with known content
//	commit 2 (sha2): A.txt modified (lines added/removed),
//	                 B.txt removed,
//	                 C.txt added,
//	                 binary.bin added (binary file → no line count).
//
// Returns the repo path and the two commit SHAs.
func initRepoForDiffStat(t *testing.T) (clonePath, sha1, sha2 string) {
	t.Helper()
	tmp := t.TempDir()
	runGit(t, tmp, "init", "-b", "main", "-q")
	runGit(t, tmp, "config", "user.email", "test@example.invalid")
	runGit(t, tmp, "config", "user.name", "Test User")
	runGit(t, tmp, "config", "commit.gpgsign", "false")

	// First commit: A.txt with 3 lines, B.txt with 2 lines.
	writeFile(t, tmp, "A.txt", "alpha\nbeta\ngamma\n")
	writeFile(t, tmp, "B.txt", "one\ntwo\n")
	runGit(t, tmp, "add", ".")
	runGit(t, tmp, "commit", "-m", "first")
	sha1 = captureGitOutput(t, tmp, "rev-parse", "HEAD")

	// Second commit: modify A.txt (replace last line, add two new),
	// remove B.txt, add C.txt with 4 lines, add a binary file.
	writeFile(t, tmp, "A.txt", "alpha\nbeta\nDELTA\nepsilon\nzeta\n")
	runGit(t, tmp, "rm", "-q", "B.txt")
	writeFile(t, tmp, "C.txt", "first\nsecond\nthird\nfourth\n")
	// Binary file: bytes that contain a NUL so git classifies as binary.
	binaryContent := string([]byte{0x00, 0x01, 0x02, 0x03, 0xFF, 0xFE, 0x00, 0xAA})
	writeFile(t, tmp, "binary.bin", binaryContent)
	runGit(t, tmp, "add", ".")
	runGit(t, tmp, "commit", "-m", "second")
	sha2 = captureGitOutput(t, tmp, "rev-parse", "HEAD")

	return tmp, sha1, sha2
}

func TestDiffStat_SameSHA_ZeroStats(t *testing.T) {
	t.Parallel()
	clonePath, sha1, _ := initRepoForDiffStat(t)
	bs, err := NewBlobStreamer(t.Context(), clonePath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = bs.Close() })

	stat, err := bs.DiffStat(t.Context(), sha1, sha1)
	require.NoError(t, err)
	assert.Equal(t, DiffStat{}, stat)
}

func TestDiffStat_TwoCommits_FullBreakdown(t *testing.T) {
	t.Parallel()
	clonePath, sha1, sha2 := initRepoForDiffStat(t)
	bs, err := NewBlobStreamer(t.Context(), clonePath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = bs.Close() })

	stat, err := bs.DiffStat(t.Context(), sha1, sha2)
	require.NoError(t, err)

	// File counts:
	//   A.txt modified  → FilesChanged += 1
	//   B.txt deleted   → FilesRemoved += 1
	//   C.txt added     → FilesAdded += 1
	//   binary.bin add  → FilesAdded += 1
	assert.Equal(t, 2, stat.FilesAdded, "C.txt + binary.bin")
	assert.Equal(t, 1, stat.FilesChanged, "A.txt modified")
	assert.Equal(t, 1, stat.FilesRemoved, "B.txt deleted")

	// Line counts (text files only; binary excluded):
	//   A.txt: was 3 lines [alpha,beta,gamma], now 5 [alpha,beta,DELTA,epsilon,zeta]
	//          → 1 line replaced (gamma -> DELTA) + 2 new (epsilon, zeta)
	//          → numstat reports +3 -1
	//   B.txt: was 2 lines, removed → -2
	//   C.txt: 4 new lines → +4
	//   binary.bin: not counted
	assert.Equal(t, 7, stat.LinesAdded, "A:+3, C:+4")
	assert.Equal(t, 3, stat.LinesRemoved, "A:-1, B:-2")
}

func TestDiffStat_OnlyFileAdded(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	runGit(t, tmp, "init", "-b", "main", "-q")
	runGit(t, tmp, "config", "user.email", "test@example.invalid")
	runGit(t, tmp, "config", "user.name", "Test")
	runGit(t, tmp, "config", "commit.gpgsign", "false")

	writeFile(t, tmp, "first.txt", "hello\n")
	runGit(t, tmp, "add", ".")
	runGit(t, tmp, "commit", "-m", "first")
	sha1 := captureGitOutput(t, tmp, "rev-parse", "HEAD")

	writeFile(t, tmp, "second.txt", "world\nagain\n")
	runGit(t, tmp, "add", ".")
	runGit(t, tmp, "commit", "-m", "second")
	sha2 := captureGitOutput(t, tmp, "rev-parse", "HEAD")

	bs, err := NewBlobStreamer(t.Context(), tmp)
	require.NoError(t, err)
	t.Cleanup(func() { _ = bs.Close() })

	stat, err := bs.DiffStat(t.Context(), sha1, sha2)
	require.NoError(t, err)
	assert.Equal(t, 1, stat.FilesAdded)
	assert.Equal(t, 0, stat.FilesChanged)
	assert.Equal(t, 0, stat.FilesRemoved)
	assert.Equal(t, 2, stat.LinesAdded)
	assert.Equal(t, 0, stat.LinesRemoved)
}

func TestDiffStat_OnlyFileRemoved(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	runGit(t, tmp, "init", "-b", "main", "-q")
	runGit(t, tmp, "config", "user.email", "test@example.invalid")
	runGit(t, tmp, "config", "user.name", "Test")
	runGit(t, tmp, "config", "commit.gpgsign", "false")

	writeFile(t, tmp, "doomed.txt", "line1\nline2\nline3\n")
	runGit(t, tmp, "add", ".")
	runGit(t, tmp, "commit", "-m", "first")
	sha1 := captureGitOutput(t, tmp, "rev-parse", "HEAD")

	runGit(t, tmp, "rm", "-q", "doomed.txt")
	runGit(t, tmp, "commit", "-m", "second")
	sha2 := captureGitOutput(t, tmp, "rev-parse", "HEAD")

	bs, err := NewBlobStreamer(t.Context(), tmp)
	require.NoError(t, err)
	t.Cleanup(func() { _ = bs.Close() })

	stat, err := bs.DiffStat(t.Context(), sha1, sha2)
	require.NoError(t, err)
	assert.Equal(t, 0, stat.FilesAdded)
	assert.Equal(t, 0, stat.FilesChanged)
	assert.Equal(t, 1, stat.FilesRemoved)
	assert.Equal(t, 0, stat.LinesAdded)
	assert.Equal(t, 3, stat.LinesRemoved)
}

func TestDiffStat_BinaryFileAdded_FilesCountedNoLineCount(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	runGit(t, tmp, "init", "-b", "main", "-q")
	runGit(t, tmp, "config", "user.email", "test@example.invalid")
	runGit(t, tmp, "config", "user.name", "Test")
	runGit(t, tmp, "config", "commit.gpgsign", "false")

	writeFile(t, tmp, "stable.txt", "stable\n")
	runGit(t, tmp, "add", ".")
	runGit(t, tmp, "commit", "-m", "first")
	sha1 := captureGitOutput(t, tmp, "rev-parse", "HEAD")

	binary := string([]byte{0x00, 0xFF, 0x00, 0xFF, 0x00, 0xFF})
	writeFile(t, tmp, "data.bin", binary)
	runGit(t, tmp, "add", ".")
	runGit(t, tmp, "commit", "-m", "added binary")
	sha2 := captureGitOutput(t, tmp, "rev-parse", "HEAD")

	bs, err := NewBlobStreamer(t.Context(), tmp)
	require.NoError(t, err)
	t.Cleanup(func() { _ = bs.Close() })

	stat, err := bs.DiffStat(t.Context(), sha1, sha2)
	require.NoError(t, err)
	assert.Equal(t, 1, stat.FilesAdded, "data.bin counted as added")
	assert.Equal(t, 0, stat.LinesAdded, "binary contributes no line count")
	assert.Equal(t, 0, stat.LinesRemoved)
}

func TestDiffStat_MissingSHA_ReturnsErrSHAMissingFromClone(t *testing.T) {
	t.Parallel()
	clonePath, sha1, _ := initRepoForDiffStat(t)
	bs, err := NewBlobStreamer(t.Context(), clonePath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = bs.Close() })

	const fakeSHA = "0000000000000000000000000000000000000000"
	_, err = bs.DiffStat(t.Context(), sha1, fakeSHA)
	assert.ErrorIs(t, err, ErrSHAMissingFromClone)

	_, err = bs.DiffStat(t.Context(), fakeSHA, sha1)
	assert.ErrorIs(t, err, ErrSHAMissingFromClone)
}

func TestDiffStat_AfterClose_StillUsable(t *testing.T) {
	t.Parallel()
	// DiffStat doesn't use the persistent cat-file subprocess, so
	// it remains operable after Close. Documented behavior — the
	// matrix assembler may DiffStat versions even after the
	// streamer's blob-read phase has ended. (The two diff
	// subprocesses are one-shot, gitenv.NewCmd-bounded by ctx.)
	clonePath, sha1, sha2 := initRepoForDiffStat(t)
	bs, err := NewBlobStreamer(t.Context(), clonePath)
	require.NoError(t, err)
	require.NoError(t, bs.Close())

	stat, err := bs.DiffStat(t.Context(), sha1, sha2)
	require.NoError(t, err)
	assert.Greater(t, stat.FilesAdded+stat.FilesChanged+stat.FilesRemoved, 0)
}
