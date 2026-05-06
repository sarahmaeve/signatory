package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestServeStatus_JSON_NotRunning verifies that --json returns
// structured output when the service is not running (no pidfile).
func TestServeStatus_JSON_NotRunning(t *testing.T) {
	t.Parallel()

	pidPath := filepath.Join(t.TempDir(), "serve.pid")
	// No pidfile → not running.

	var stdout bytes.Buffer
	cmd := &ServeStatusCmd{
		PidPath: pidPath,
		Port:    21517,
		JSON:    true,
		Stdout:  &stdout,
	}

	err := cmd.Run(nil)
	require.Error(t, err, "status returns error when not running")

	var result ServeStatusResult
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &result),
		"JSON output must be written before error return; got: %s", stdout.String())

	assert.False(t, result.Running)
	assert.Equal(t, 0, result.PID)
	assert.Contains(t, result.Message, "not running")
}

// TestServeStatus_JSON_StalePid verifies that --json handles a stale
// pidfile (PID doesn't exist or isn't ours) correctly.
func TestServeStatus_JSON_StalePid(t *testing.T) {
	t.Parallel()

	pidPath := filepath.Join(t.TempDir(), "serve.pid")
	// Write a PID that almost certainly doesn't belong to signatory.
	// PID 1 on macOS is launchd — alive but not ours.
	require.NoError(t, os.WriteFile(pidPath, []byte("999999999\n"), 0o644))

	var stdout bytes.Buffer
	cmd := &ServeStatusCmd{
		PidPath: pidPath,
		Port:    21517,
		JSON:    true,
		Stdout:  &stdout,
	}

	err := cmd.Run(nil)
	require.Error(t, err, "stale pidfile still means not running")

	var result ServeStatusResult
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &result),
		"JSON output must be written; got: %s", stdout.String())

	assert.False(t, result.Running)
	assert.Contains(t, result.Message, "not running")
}

// TestServeStatus_JSON_Running verifies that --json returns
// structured output when the service IS running. We use the current
// process PID to fake an alive process, and a predictably-unused
// port so the port probe fails (distinguishing "alive but not
// listening" from "fully healthy").
func TestServeStatus_JSON_Running(t *testing.T) {
	t.Parallel()

	pidPath := filepath.Join(t.TempDir(), "serve.pid")
	myPID := os.Getpid()
	require.NoError(t, os.WriteFile(pidPath,
		fmt.Appendf(nil, "%d\n", myPID), 0o644))

	var stdout bytes.Buffer
	cmd := &ServeStatusCmd{
		PidPath: pidPath,
		Port:    0, // port 0 won't be listening → portHealthy = false
		JSON:    true,
		Stdout:  &stdout,
	}

	require.NoError(t, cmd.Run(nil))

	var result ServeStatusResult
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &result),
		"stdout must be valid JSON; got: %s", stdout.String())

	assert.True(t, result.Running)
	assert.Equal(t, myPID, result.PID)
	assert.False(t, result.PortHealthy,
		"port 0 should not be listening")
}
