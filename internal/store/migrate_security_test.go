package store

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSecurity_Backup_RefusesToClobberExistingFile verifies that the
// migration backup function does not overwrite a pre-existing file at
// the predicted backup path. Issue #82: backupDatabase opened the
// destination with O_CREATE|O_WRONLY but no O_EXCL, so any file already
// at the predicted path was opened for writing — clobbering arbitrary
// content with the database bytes.
//
// The test pre-creates sentinel files at every backup path the function
// could choose in the next several seconds (covering clock skew between
// the test's now and the function's now). After running backup, every
// sentinel must be byte-identical to its original. The fix is to use
// os.CreateTemp with a unique random suffix so the backup file lands at
// an unguessable path that no sentinel can predict.
func TestSecurity_Backup_RefusesToClobberExistingFile(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	const dbContent = "fake db content for backup test"
	require.NoError(t, os.WriteFile(dbPath, []byte(dbContent), 0600))

	// Pre-create sentinels at every second-precision path the backup
	// function could compute in the next 5 seconds. backupDatabase
	// computes its timestamp via time.Now() inside the call, so we
	// blanket the window between sentinel creation and backup execution.
	const sentinelContent = "ORIGINAL — must not be clobbered by backup"
	now := time.Now().UTC()
	var sentinels []string
	for offset := 0; offset < 5; offset++ {
		ts := now.Add(time.Duration(offset) * time.Second).Format("20060102-150405")
		path := fmt.Sprintf("%s.backup-v0-%s", dbPath, ts)
		require.NoError(t, os.WriteFile(path, []byte(sentinelContent), 0600))
		sentinels = append(sentinels, path)
	}

	// Run the backup (db nil → skip the WAL checkpoint, just copy bytes).
	err := backupDatabase(t.Context(), nil, dbPath, 0)
	require.NoError(t, err, "backup should succeed by writing to a unique path")

	// THE CRITICAL ASSERTION: every sentinel must be byte-identical.
	// If the backup function clobbered any of them, the diff will show
	// the dbContent prefix (with O_TRUNC) or "fake db con..." stamped
	// over the leading bytes (without O_TRUNC).
	for _, sentinel := range sentinels {
		content, readErr := os.ReadFile(sentinel)
		require.NoError(t, readErr, "sentinel %s should still exist", sentinel)
		assert.Equal(t, sentinelContent, string(content),
			"sentinel %s must not be clobbered — if this fails, backupDatabase wrote through to a predictable path",
			filepath.Base(sentinel))
	}
}

// TestSecurity_Backup_RefusesToFollowSymlink verifies that the migration
// backup function does not follow symlinks planted at the predicted
// backup path. Issue #82: combined with the deterministic filename
// (signatory.db.backup-v{N}-{timestamp}), an attacker could plant a
// symlink at the predicted path and redirect the database backup to an
// arbitrary file (e.g., /etc/cron.d/...).
//
// The fix (os.CreateTemp with a random suffix) addresses this not via
// O_NOFOLLOW per se, but by making the destination path unguessable —
// the random suffix means no symlink can be planted in advance.
func TestSecurity_Backup_RefusesToFollowSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation on Windows requires admin privileges")
	}

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	const dbContent = "fake db content for symlink test"
	require.NoError(t, os.WriteFile(dbPath, []byte(dbContent), 0600))

	// Sentinel that the planted symlinks point at.
	sentinel := filepath.Join(dir, "sentinel.txt")
	const sentinelContent = "ORIGINAL — must not be modified by backup write through symlink"
	require.NoError(t, os.WriteFile(sentinel, []byte(sentinelContent), 0600))

	// Plant symlinks at every backup path the function could land at
	// in the next 5 seconds. Each symlink points at the sentinel.
	now := time.Now().UTC()
	for offset := 0; offset < 5; offset++ {
		ts := now.Add(time.Duration(offset) * time.Second).Format("20060102-150405")
		symlinkPath := fmt.Sprintf("%s.backup-v0-%s", dbPath, ts)
		require.NoError(t, os.Symlink(sentinel, symlinkPath))
	}

	err := backupDatabase(t.Context(), nil, dbPath, 0)
	require.NoError(t, err, "backup should succeed by writing to a unique path")

	// THE CRITICAL ASSERTION: the symlink target (sentinel) must not
	// have been modified. If the backup followed any of the planted
	// symlinks, the sentinel would now contain the dbContent (or its
	// leading bytes overwriting the original).
	content, err := os.ReadFile(sentinel)
	require.NoError(t, err)
	assert.Equal(t, sentinelContent, string(content),
		"sentinel must not be modified — if this fails, backupDatabase followed a planted symlink")
}
