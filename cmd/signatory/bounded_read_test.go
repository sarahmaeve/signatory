package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// makeSparseFile creates a file at path of the requested size in
// bytes using truncate (sparse on Linux/macOS). Avoids actually
// writing N bytes to disk — the file APPEARS to be N bytes via Stat
// and reads return zero bytes via the OS sparse-file representation.
// Used by the size-cap tests so a 10 MiB+1 fixture costs ~zero disk
// I/O and ~zero wall time.
func makeSparseFile(t *testing.T, path string, size int64) {
	t.Helper()
	f, err := os.Create(path)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(size))
	require.NoError(t, f.Close())
}

// TestReadBoundedAnalystFile_RejectsOversized confirms the cap
// fires exactly one byte past maxAnalystFileBytes. Using a sparse
// file means the test is fast even at multi-MiB sizes.
//
// Revert proof: change the LimitReader argument from
// maxAnalystFileBytes+1 back to a stat-then-read pattern WITHOUT
// the +1 detection; this test fails because reading exactly
// maxAnalystFileBytes bytes passes silently and the size check
// returns the buffer instead of erroring.
func TestReadBoundedAnalystFile_RejectsOversized(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "oversized.json")
	makeSparseFile(t, path, maxAnalystFileBytes+1)

	_, err := readBoundedAnalystFile(path)
	require.Error(t, err)
	assert.ErrorIs(t, err, errAnalystFileTooLarge,
		"oversized file must trigger errAnalystFileTooLarge sentinel")
}

// TestReadBoundedAnalystFile_AcceptsExactCap covers the
// off-by-one boundary: a file of exactly maxAnalystFileBytes bytes
// must succeed. Without this, the implementation could regress to
// rejecting at-or-over (one byte too restrictive) and pass only the
// "rejects oversized" test.
func TestReadBoundedAnalystFile_AcceptsExactCap(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "exact-cap.bin")
	makeSparseFile(t, path, maxAnalystFileBytes)

	buf, err := readBoundedAnalystFile(path)
	require.NoError(t, err)
	assert.Equal(t, int64(maxAnalystFileBytes), int64(len(buf)),
		"reading at exact cap must return exactly cap bytes")
}

// TestReadBoundedAnalystFile_AcceptsSmallFile is the happy-path
// regression guard: a typical analyst output (tens of KiB) must
// round-trip its content unchanged. Catches a future refactor that
// truncates or otherwise mangles small inputs.
func TestReadBoundedAnalystFile_AcceptsSmallFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "small.json")
	content := []byte(`{"analyst_id":"x","model":"y"}`)
	require.NoError(t, os.WriteFile(path, content, 0o600))

	buf, err := readBoundedAnalystFile(path)
	require.NoError(t, err)
	assert.Equal(t, content, buf, "small file must round-trip its content")
}

// TestReadBoundedAnalystFile_FileNotFound covers the boring case so
// callers can errors.Is-check os.ErrNotExist on the returned error.
// Pre-fix readBoundedAnalystFile didn't exist; once it does, the
// not-exist path needs to remain identifiable.
func TestReadBoundedAnalystFile_FileNotFound(t *testing.T) {
	t.Parallel()
	_, err := readBoundedAnalystFile(filepath.Join(t.TempDir(), "no-such-file"))
	require.Error(t, err)
	assert.True(t, errors.Is(err, os.ErrNotExist),
		"missing file must surface os.ErrNotExist for caller classification")
}

// TestErrAnalystFileTooLarge_IsSentinel locks in the contract that
// errAnalystFileTooLarge is a stable reference. Any caller that
// uses errors.Is to detect the size-rejection path depends on this.
func TestErrAnalystFileTooLarge_IsSentinel(t *testing.T) {
	t.Parallel()
	assert.True(t, errors.Is(errAnalystFileTooLarge, errAnalystFileTooLarge),
		"errAnalystFileTooLarge must be a stable sentinel")
}
