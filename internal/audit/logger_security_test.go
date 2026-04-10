package audit

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSecurity_AuditLog_RefusesSymlink verifies that the audit logger
// will not follow a symlink at the audit.log path. Issue #81: an attacker
// who can plant a symlink redirects audit writes to an arbitrary file,
// turning the audit logger into a "write JSON line to arbitrary file"
// primitive when combined with attacker-influenced Actor/Action/Detail
// fields. The fix is O_NOFOLLOW on Unix plus a defensive Lstat check.
//
// The test asserts the security property that matters: the file the
// symlink points at MUST NOT be modified by an audit log write. The
// dual-write design means Logger.Log returns nil even when the file
// writer fails (database write succeeds, file write is best-effort),
// so the assertion is on the sentinel file's content, not on the
// returned error.
func TestSecurity_AuditLog_RefusesSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation on Windows requires admin privileges")
	}

	dir := t.TempDir()

	// Sentinel file the symlink will point at. If the symlink is
	// followed, the audit writer will append a JSON line to this file.
	sentinel := filepath.Join(dir, "sentinel.txt")
	const sentinelContent = "ORIGINAL — must not be modified by audit logger\n"
	require.NoError(t, os.WriteFile(sentinel, []byte(sentinelContent), 0600))

	// Plant the symlink at the path the audit logger will try to open.
	logPath := filepath.Join(dir, "audit.log")
	require.NoError(t, os.Symlink(sentinel, logPath))

	store := &fakeStore{}
	logger := New(store, logPath)

	// LogAction — the dual-write design means this returns nil even
	// if the file writer rejects the symlink (database write succeeds,
	// file write is best-effort). The security property is that the
	// sentinel file is not modified, NOT that an error is returned.
	err := logger.LogAction(context.Background(),
		"team:attacker", "set_posture", "ent-1",
		`{"injected":"payload"}`)
	require.NoError(t, err, "Log returns nil because the database write succeeded")

	// Database write must still have happened — defense in depth at the
	// file layer must not break the in-database audit record.
	require.Len(t, store.entries, 1, "database audit entry must be written even when the file write is refused")

	// THE CRITICAL ASSERTION: the sentinel file must be byte-identical to
	// what we wrote. If the symlink was followed, the audit JSON line
	// would have been appended (O_APPEND), and the content would now be
	// sentinelContent + JSON.
	got, err := os.ReadFile(sentinel)
	require.NoError(t, err)
	assert.Equal(t, sentinelContent, string(got),
		"sentinel file must not be modified — if this fails, the symlink was followed and the audit logger is a write-anywhere primitive")
}
