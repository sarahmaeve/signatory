package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPIDFile_WriteReadRoundTrip — happy path for the PID
// file helpers. Write then read should give the same value.
func TestPIDFile_WriteReadRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, ".receiver.pid")

	require.NoError(t, writePIDFileFor(path, 12345))
	got, ok := readPIDFile(path)
	require.True(t, ok, "PID file should be readable")
	assert.Equal(t, 12345, got)
}

// TestReadPIDFile_MissingFile — missing PID file returns
// (0, false), not an error. Callers (notably stopDaemon) use
// the bool to distinguish "no daemon recorded" from "PID file
// is corrupt."
func TestReadPIDFile_MissingFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, ".receiver.pid")

	pid, ok := readPIDFile(path)
	assert.False(t, ok)
	assert.Equal(t, 0, pid)
}

// TestReadPIDFile_GarbageContents — non-numeric PID file
// content also returns (0, false). Treats corrupt files the
// same as missing — restart cleans up.
func TestReadPIDFile_GarbageContents(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, ".receiver.pid")
	require.NoError(t, os.WriteFile(path, []byte("not a number\n"), 0o644))

	pid, ok := readPIDFile(path)
	assert.False(t, ok)
	assert.Equal(t, 0, pid)
}

// TestReadPIDFile_TrimsWhitespace — PID files can have
// trailing newlines (we write one) plus incidental
// whitespace from manual edits. Trim them transparently.
func TestReadPIDFile_TrimsWhitespace(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, ".receiver.pid")
	require.NoError(t, os.WriteFile(path, []byte("  42\n\n"), 0o644))

	pid, ok := readPIDFile(path)
	assert.True(t, ok)
	assert.Equal(t, 42, pid)
}

// TestIsProcessAlive_LiveProcess — spawn a real subprocess
// (sleep 30) and confirm isProcessAlive sees it. Then kill it,
// reap via Wait, and confirm we see it's gone.
//
// Wait MUST be called to reap the child — without it, the
// process becomes a zombie that signal(0) still reports as
// alive (zombies are still in the process table). Production's
// daemon doesn't hit this because Release() detaches the child
// to init, which reaps zombies promptly.
func TestIsProcessAlive_LiveProcess(t *testing.T) {
	t.Parallel()

	cmd := exec.Command("sleep", "30")
	require.NoError(t, cmd.Start())
	pid := cmd.Process.Pid

	assert.True(t, isProcessAlive(pid), "spawned process should be alive")

	require.NoError(t, cmd.Process.Kill())
	// cmd.Wait returns the kill-signal as an error; that's
	// normal Go exec behavior, not a test failure. The
	// important thing is that Wait reaps the zombie so the
	// next isProcessAlive call sees the truth.
	_ = cmd.Wait()

	assert.False(t, isProcessAlive(pid), "reaped process should be gone")
}

// TestIsProcessAlive_NeverExisted — PID 999999 (well above
// macOS/Linux PID range for any session) should be reported
// dead. Negative and zero PIDs are also dead by definition.
func TestIsProcessAlive_NeverExisted(t *testing.T) {
	t.Parallel()
	assert.False(t, isProcessAlive(999999))
	assert.False(t, isProcessAlive(0))
	assert.False(t, isProcessAlive(-1))
}

// TestPortFromAddr — :4318 → 4318, host:port → port,
// malformed → error.
func TestPortFromAddr(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in      string
		want    int
		wantErr bool
	}{
		{":4318", 4318, false},
		{"127.0.0.1:9090", 9090, false},
		{"localhost:8080", 8080, false},
		{"no-port-here", 0, true},
		{"", 0, true},
	}
	for _, tc := range cases {
		got, err := portFromAddr(tc.in)
		if tc.wantErr {
			assert.Error(t, err, "input %q should error", tc.in)
		} else {
			assert.NoError(t, err, "input %q should parse", tc.in)
			assert.Equal(t, tc.want, got, "input %q port", tc.in)
		}
	}
}

// TestStopDaemon_NotRunning — stop against an empty out-dir
// returns errNotRunning so restart's best-effort logic can
// distinguish it.
func TestStopDaemon_NotRunning(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	err := stopDaemon(dir)
	assert.ErrorIs(t, err, errNotRunning)
}

// TestStopDaemon_StalePIDFile — stop against a PID file
// pointing at a dead process: cleans up the file, returns a
// not-running-shape error so restart's filter still works.
func TestStopDaemon_StalePIDFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	pidPath := filepath.Join(dir, ".receiver.pid")
	// PID 999999 is well past any real PID we'd encounter
	require.NoError(t, writePIDFileFor(pidPath, 999999))

	err := stopDaemon(dir)
	assert.ErrorIs(t, err, errNotRunning,
		"stale PID file should surface as not-running for restart's filter")

	// And the stale file should be cleaned up.
	_, err = os.Stat(pidPath)
	assert.True(t, os.IsNotExist(err), "stale PID file should be removed")
}

// TestStopDaemon_LiveProcess — write a PID file for a real
// spawned subprocess, verify stopDaemon kills it and removes
// the file.
//
// We need to reap the child concurrently: stopDaemon checks
// isProcessAlive in a polling loop, and a zombie child still
// shows alive. A goroutine doing cmd.Wait reaps the moment the
// process exits, so stopDaemon's loop sees the truth and
// returns promptly. Production doesn't hit this because the
// daemon is detached via Release() and init reaps it.
func TestStopDaemon_LiveProcess(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	pidPath := filepath.Join(dir, ".receiver.pid")

	cmd := exec.Command("sleep", "30")
	require.NoError(t, cmd.Start())
	pid := cmd.Process.Pid

	// Reap-on-exit goroutine. Without this, sleep dies on
	// SIGTERM but lingers as a zombie and stopDaemon's loop
	// waits until daemonStopTimeout fires.
	reaped := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(reaped)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-reaped
	})

	require.NoError(t, writePIDFileFor(pidPath, pid))
	require.True(t, isProcessAlive(pid), "spawn precondition")

	require.NoError(t, stopDaemon(dir))

	// Give the reap goroutine a moment to observe the exit.
	select {
	case <-reaped:
	case <-time.After(2 * time.Second):
		t.Fatal("reap goroutine did not see process exit")
	}

	assert.False(t, isProcessAlive(pid), "sleep subprocess should be dead after stopDaemon")
	_, err := os.Stat(pidPath)
	assert.True(t, os.IsNotExist(err), "PID file should be removed")
}
