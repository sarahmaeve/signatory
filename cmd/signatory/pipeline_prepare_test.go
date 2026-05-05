package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/certs"

	_ "modernc.org/sqlite"
)

// TestPipelinePrepare_CertsCheckFails verifies that a failed certs
// preflight stops the pipeline immediately with a structured error
// containing both the failure message and the remediation hint.
// This is the first gate — no network or store work should happen
// when the TLS environment is broken.
func TestPipelinePrepare_CertsCheckFails(t *testing.T) {
	t.Parallel()

	ts := newPipelineTestServer(t)
	globals := testGlobals(t)

	var stdout, stderr bytes.Buffer
	cmd := &PipelinePrepareCmd{
		Target:      "https://github.com/JedWatson/classnames",
		PipelineURL: ts.URL,
		Stdout:      &stdout,
		Stderr:      &stderr,
		CertsChecker: func() certs.CheckResult {
			return certs.CheckResult{
				OK:      false,
				Message: "NODE_EXTRA_CA_CERTS is not set",
				Fix:     "run `signatory certs init --write-profile`",
			}
		},
	}

	err := cmd.Run(globals)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "NODE_EXTRA_CA_CERTS",
		"error must surface the certs check failure message")
	assert.Contains(t, err.Error(), "fix:",
		"error must include the remediation hint")
	assert.Empty(t, stdout.String(),
		"no JSON output when pipeline fails in preflight")
}

// TestPipelinePrepare_ServiceUnreachable verifies that an unreachable
// pipeline service produces a clear error naming the operation, and
// no partial output.
func TestPipelinePrepare_ServiceUnreachable(t *testing.T) {
	t.Parallel()

	ts := newPipelineTestServer(t)
	unreachableURL := ts.URL
	ts.Close() // close before Run — TCP dial will fail

	globals := testGlobals(t)

	var stdout, stderr bytes.Buffer
	cmd := &PipelinePrepareCmd{
		Target:      "https://github.com/JedWatson/classnames",
		PipelineURL: unreachableURL,
		Stdout:      &stdout,
		Stderr:      &stderr,
		CertsChecker: func() certs.CheckResult {
			return certs.CheckResult{OK: true}
		},
	}

	err := cmd.Run(globals)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pipeline session create",
		"error must name the failing operation")
	assert.Empty(t, stdout.String(),
		"no JSON output when pipeline fails")
}

// TestPipelinePrepare_OutputIsValidManifest is the happy-path
// integration test: given a resolvable target, a running pipeline
// server, and a real store, prepare creates both sessions, renders
// and deposits handoffs, refreshes signals, and returns a JSON
// manifest with every field the orchestrator needs.
func TestPipelinePrepare_OutputIsValidManifest(t *testing.T) {
	t.Parallel()

	ts := newPipelineTestServer(t)
	globals := testGlobals(t, newMockCollector())
	cloneDir := filepath.Join(t.TempDir(), "clones")

	var stdout, stderr bytes.Buffer
	cmd := &PipelinePrepareCmd{
		Target: "https://github.com/JedWatson/classnames",
		ExpectedAnalysts: []string{
			"signatory-security-v1",
			"signatory-provenance-v1",
			"signatory-synthesis-v1",
		},
		CloneDir:    cloneDir,
		PipelineURL: ts.URL,
		Stdout:      &stdout,
		Stderr:      &stderr,
		CertsChecker: func() certs.CheckResult {
			return certs.CheckResult{OK: true, Message: "test: certs OK"}
		},
		RunGitClone: func(_ context.Context, _, dest, _ string) error {
			return os.MkdirAll(dest, 0o755)
		},
		PrecheckSource: &fakePrecheckSource{
			Files:    []string{"package.json", "README.md", "index.js"},
			Language: "JavaScript",
		},
	}

	require.NoError(t, cmd.Run(globals))

	var manifest PrepareManifest
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &manifest),
		"stdout must be valid JSON; got: %s", stdout.String())

	// Session IDs are UUIDs.
	assert.Regexp(t, uuidRegex, manifest.SessionID+"\n",
		"session_id must be a UUID")
	assert.Regexp(t, uuidRegex, manifest.AnalysisSessionID+"\n",
		"analysis_session_id must be a UUID")
	// The two must be distinct — confusing them was the synthesis
	// bug from dogfood session cef3c5ab.
	assert.NotEqual(t, manifest.SessionID, manifest.AnalysisSessionID,
		"pipeline session ID and analysis session ID must differ")

	// Target metadata matches input.
	assert.Equal(t, "https://github.com/JedWatson/classnames", manifest.Target)
	assert.Equal(t, "classnames", manifest.TargetName)
	assert.NotEmpty(t, manifest.TargetURL)

	// Clone path is under the clone directory.
	assert.Contains(t, manifest.ClonePath, "classnames")

	// Both handoffs deposited.
	assert.Equal(t, []string{"security", "provenance"}, manifest.HandoffsDeposited)

	// Signals refreshed via mock collectors.
	assert.True(t, manifest.SignalsRefreshed)

	// Terminal status.
	assert.Equal(t, "ready", manifest.Status)
}

// TestPipelinePrepare_HandoffsRetrievable verifies that the deposited
// handoffs are actually in the pipeline session and can be retrieved
// via the same URL pattern agents use. This catches a class of bug
// where the deposit succeeds but the content is empty or misrouted.
func TestPipelinePrepare_HandoffsRetrievable(t *testing.T) {
	t.Parallel()

	ts := newPipelineTestServer(t)
	globals := testGlobals(t, newMockCollector())
	cloneDir := filepath.Join(t.TempDir(), "clones")

	var stdout bytes.Buffer
	cmd := &PipelinePrepareCmd{
		Target:      "https://github.com/JedWatson/classnames",
		CloneDir:    cloneDir,
		PipelineURL: ts.URL,
		Stdout:      &stdout,
		Stderr:      &bytes.Buffer{},
		CertsChecker: func() certs.CheckResult {
			return certs.CheckResult{OK: true}
		},
		RunGitClone: func(_ context.Context, _, dest, _ string) error {
			return os.MkdirAll(dest, 0o755)
		},
		PrecheckSource: &fakePrecheckSource{
			Files:    []string{"package.json"},
			Language: "JavaScript",
		},
	}
	require.NoError(t, cmd.Run(globals))

	var manifest PrepareManifest
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &manifest))

	// Retrieve both handoffs via HTTP GET, matching the URL pattern
	// agents use at dispatch time (format=raw returns plain text).
	for _, role := range []string{"security", "provenance"} {
		url := ts.URL + "/api/sessions/" + manifest.SessionID +
			"/messages?role=" + role + "&type=handoff&format=raw"
		resp, err := http.Get(url) //nolint:gosec // G107: test URL from httptest
		require.NoError(t, err)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode,
			"%s handoff GET failed: %s", role, string(body))
		assert.NotEmpty(t, body,
			"%s handoff content must not be empty", role)
	}
}

// TestPipelinePrepare_BadTarget verifies that an unresolvable target
// fails early (before session creation) with an error naming the
// target.
func TestPipelinePrepare_BadTarget(t *testing.T) {
	t.Parallel()

	ts := newPipelineTestServer(t)
	globals := testGlobals(t)

	var stdout, stderr bytes.Buffer
	cmd := &PipelinePrepareCmd{
		Target:      "not-a-valid-target-!!!",
		PipelineURL: ts.URL,
		Stdout:      &stdout,
		Stderr:      &stderr,
		CertsChecker: func() certs.CheckResult {
			return certs.CheckResult{OK: true}
		},
	}

	err := cmd.Run(globals)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not-a-valid-target-!!!",
		"error must name the unresolvable target")
	assert.Empty(t, stdout.String(),
		"no JSON output on target resolution failure")
}
