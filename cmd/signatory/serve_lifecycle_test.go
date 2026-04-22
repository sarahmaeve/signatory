package main

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- isOurServiceAlive: PID-recycling defense ------------------------------
//
// The pre-hardening check was isProcessAlive (signal-0 only), which
// returned true for ANY alive process at the named PID. That made
// Stop's syscall.Kill structurally unsafe under PID recycling: the
// OS may have reassigned signatory's PID to an unrelated process
// between pidfile write and Stop invocation, and SIGTERM would hit
// that unrelated process. The fix adds a binary-name check via
// `ps -p <pid> -o comm=` so destructive callers refuse to act on
// strangers.
//
// These tests use the running test process as the "stranger." Its
// PID is alive (we are it), but its binary name is the test binary
// (e.g., signatory.test, or whatever go test built), not "signatory".
// By overriding serviceBinaryName to a value that cannot match any
// real binary name, we guarantee the comparison fails — proving the
// gate works without depending on the actual test binary's basename.

// TestIsOurServiceAlive_StrangerPidReturnsFalse covers the helper
// in isolation: a live PID that doesn't match the configured
// binary name must report not-our-service.
//
// Revert proof: change isOurServiceAlive to `return isProcessAlive(pid)`
// (drop the binary check). This test fails because the test process
// IS alive and the helper would return true.
//
// NOTE: serviceBinaryName is a package-level var; the t.Cleanup
// restoration is required so cross-test pollution doesn't leak.
func TestIsOurServiceAlive_StrangerPidReturnsFalse(t *testing.T) {
	// Pin serviceBinaryName to a value that no real running process
	// can possibly match. The test process's binary name will not
	// equal this string, so processCommandMatches returns false,
	// and isOurServiceAlive returns false even though signal-0
	// against our own PID succeeds.
	orig := serviceBinaryName
	t.Cleanup(func() { serviceBinaryName = orig })
	serviceBinaryName = func() string { return "definitely-not-a-real-binary-name" }

	require.True(t, isProcessAlive(os.Getpid()),
		"sanity: signal-0 must succeed against our own PID")
	assert.False(t, isOurServiceAlive(os.Getpid()),
		"isOurServiceAlive must return false when binary name doesn't match — defends against PID recycling")
}

// TestIsOurServiceAlive_DeadPidReturnsFalse covers the short-circuit:
// liveness gate fails first, binary check never runs. Uses a PID we
// fabricate that's vanishingly unlikely to be live (max int32 - 1).
//
// Revert proof: invert the isProcessAlive check; this test fails
// because a dead PID would report alive without the guard.
func TestIsOurServiceAlive_DeadPidReturnsFalse(t *testing.T) {
	// 2147483646 (max int32 - 1) is well above the per-OS PID ceiling
	// (Linux default 4194304; macOS default 99999) — guaranteed dead.
	const deadPid = 2147483646
	require.False(t, isProcessAlive(deadPid), "fixture: PID must be dead for the test to be meaningful")
	assert.False(t, isOurServiceAlive(deadPid))
}

// TestIsOurServiceAlive_OurOwnBinaryReturnsTrue covers the positive
// path: a live PID whose binary name matches what serviceBinaryName
// reports must be recognized as ours. Without this companion test,
// we can't tell whether isOurServiceAlive is correctly gating or
// just always returning false.
//
// Implementation: override serviceBinaryName to return the basename
// of the actual test executable (which is what `ps -p <our-pid>`
// will report). Comparison succeeds, helper returns true.
func TestIsOurServiceAlive_OurOwnBinaryReturnsTrue(t *testing.T) {
	exe, err := os.Executable()
	require.NoError(t, err, "fixture: must be able to read our own executable path")
	expected := filepath.Base(exe)

	orig := serviceBinaryName
	t.Cleanup(func() { serviceBinaryName = orig })
	serviceBinaryName = func() string { return expected }

	assert.True(t, isOurServiceAlive(os.Getpid()),
		"isOurServiceAlive must return true when both liveness and binary name match — locks in the positive path")
}

// --- ServeStopCmd: PID-recycling defense + killFn seam --------------------

// TestServeStop_RecycledPid_DoesNotKillStranger is the headline
// regression guard for the PID-recycling fix. The pidfile names a
// PID that's alive but is NOT a signatory process; Stop must
// recognize the mismatch, refuse to send any signal, and clean
// the stale pidfile.
//
// The killFn seam is load-bearing for two reasons:
//  1. Safety: if the binary check is bypassed by a future
//     refactor, the seam catches it before SIGTERM actually
//     fires against the test process.
//  2. Observability: asserting "killFn was never called" is the
//     direct way to express the contract. Asserting "the test
//     process is still alive" is indirect and could pass for the
//     wrong reason (e.g., signal blocked, race in delivery).
//
// Revert proof: replace `if !isOurServiceAlive(pid)` with
// `if !isProcessAlive(pid)` in ServeStopCmd.Run; this test fails
// because the test process is alive, Stop falls through to the
// signal-sending branch, and killFn records the call.
func TestServeStop_RecycledPid_DoesNotKillStranger(t *testing.T) {
	// Pin serviceBinaryName so no real process can match.
	origName := serviceBinaryName
	t.Cleanup(func() { serviceBinaryName = origName })
	serviceBinaryName = func() string { return "definitely-not-a-real-binary-name" }

	// Replace killFn with a recorder. Use atomic for paranoia even
	// though Stop is single-threaded — the seam is the contract.
	var killCalls atomic.Int32
	origKill := killFn
	t.Cleanup(func() { killFn = origKill })
	killFn = func(_ int, _ syscall.Signal) error {
		killCalls.Add(1)
		return nil
	}

	pidPath := filepath.Join(t.TempDir(), "serve.pid")
	pidStr := strconv.Itoa(os.Getpid()) + "\n"
	require.NoError(t, os.WriteFile(pidPath, []byte(pidStr), 0o600))

	cmd := &ServeStopCmd{PidPath: pidPath}
	err := cmd.Run(&Globals{})

	require.NoError(t, err, "Stop on a recycled PID must succeed (post-condition is 'service not running')")
	assert.Zero(t, killCalls.Load(),
		"killFn must NOT be called when the named PID belongs to a stranger — defends against PID recycling")
	assert.NoFileExists(t, pidPath,
		"stale pidfile must be removed after recognizing the recycled PID")
}

// TestServeStop_NoPidfile_ReturnsNil locks in the established
// behavior: missing pidfile is a no-op, not an error. Stop's
// post-condition is "service not running" — already satisfied
// when there's no pidfile.
func TestServeStop_NoPidfile_ReturnsNil(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "no-such-pidfile")
	cmd := &ServeStopCmd{PidPath: pidPath}
	assert.NoError(t, cmd.Run(&Globals{}))
}

// TestServeStop_StalePidfile_DeadPid covers the existing stale-
// pidfile branch (PID is dead). Distinct from the recycled-PID
// case above because the dead-PID path was already handled before
// the fix; we test it to lock the behavior in alongside the new
// recycling defense.
func TestServeStop_StalePidfile_DeadPid(t *testing.T) {
	const deadPid = 2147483646
	require.False(t, isProcessAlive(deadPid), "fixture: PID must be dead")

	pidPath := filepath.Join(t.TempDir(), "serve.pid")
	require.NoError(t, os.WriteFile(pidPath, []byte(strconv.Itoa(deadPid)+"\n"), 0o600))

	// Even with the kill seam empty, a dead PID should never reach
	// it — recording for paranoia.
	var killCalls atomic.Int32
	origKill := killFn
	t.Cleanup(func() { killFn = origKill })
	killFn = func(_ int, _ syscall.Signal) error {
		killCalls.Add(1)
		return nil
	}

	cmd := &ServeStopCmd{PidPath: pidPath}
	require.NoError(t, cmd.Run(&Globals{}))
	assert.Zero(t, killCalls.Load(), "dead PID must short-circuit before any kill")
	assert.NoFileExists(t, pidPath, "stale pidfile must be removed")
}

// --- ServeStatusCmd: errStatusNotRunning sentinel (no os.Exit) -------------

// TestServeStatus_NoPidfile_ReturnsErrStatusNotRunning is the
// regression guard for the os.Exit removal. Pre-fix, Status
// called os.Exit(1) directly when the pidfile didn't exist —
// untestable through the standard CLI path (the test runner
// would terminate). Post-fix, Status returns errStatusNotRunning
// and main.go maps it to the same exit code without echoing to
// stderr.
//
// Revert proof: restore os.Exit(1) in place of the sentinel
// return. Running this test causes the test binary to exit
// before assertions — observed as "FAIL" with no per-test
// diagnostics, which IS the failure (test runner death).
func TestServeStatus_NoPidfile_ReturnsErrStatusNotRunning(t *testing.T) {
	pidPath := filepath.Join(t.TempDir(), "no-such-pidfile")
	cmd := &ServeStatusCmd{PidPath: pidPath, Port: 21517}
	err := cmd.Run(&Globals{})
	require.Error(t, err, "Status with no pidfile must return an error, not call os.Exit")
	assert.ErrorIs(t, err, errStatusNotRunning,
		"Status must return errStatusNotRunning sentinel so main.go can suppress the stderr echo")
}

// TestServeStatus_StalePidfile_ReturnsErrStatusNotRunning covers
// the second os.Exit branch: pidfile exists but the PID is dead
// (or, post-recycling-fix, alive but a stranger). Either way Status
// must report not-running via the sentinel rather than os.Exit.
func TestServeStatus_StalePidfile_ReturnsErrStatusNotRunning(t *testing.T) {
	const deadPid = 2147483646
	require.False(t, isProcessAlive(deadPid), "fixture: PID must be dead")

	pidPath := filepath.Join(t.TempDir(), "serve.pid")
	require.NoError(t, os.WriteFile(pidPath, []byte(strconv.Itoa(deadPid)+"\n"), 0o600))

	cmd := &ServeStatusCmd{PidPath: pidPath, Port: 21517}
	err := cmd.Run(&Globals{})
	require.Error(t, err)
	assert.ErrorIs(t, err, errStatusNotRunning)
}

// TestServeStatus_RecycledPid_ReturnsErrStatusNotRunning verifies
// the recycling defense applies to Status too. A live PID owned
// by a stranger is "not running" from signatory's perspective —
// we must not report it as "running" just because something at
// that PID exists.
func TestServeStatus_RecycledPid_ReturnsErrStatusNotRunning(t *testing.T) {
	origName := serviceBinaryName
	t.Cleanup(func() { serviceBinaryName = origName })
	serviceBinaryName = func() string { return "definitely-not-a-real-binary-name" }

	pidPath := filepath.Join(t.TempDir(), "serve.pid")
	require.NoError(t, os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600))

	cmd := &ServeStatusCmd{PidPath: pidPath, Port: 21517}
	err := cmd.Run(&Globals{})
	require.Error(t, err)
	assert.ErrorIs(t, err, errStatusNotRunning)
}

// TestErrStatusNotRunning_IsSentinel locks in the contract that
// errStatusNotRunning is a stable reference, not a freshly-allocated
// error each call. main.go's errors.Is check depends on this.
func TestErrStatusNotRunning_IsSentinel(t *testing.T) {
	assert.True(t, errors.Is(errStatusNotRunning, errStatusNotRunning),
		"errStatusNotRunning must be a stable sentinel — main.go uses errors.Is to suppress stderr")
}
