package main

import (
	"context"
	"database/sql"
	"io"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/pipeline"

	_ "modernc.org/sqlite"
)

// newHandoffDepositTestServer stands up a real pipeline server
// behind httptest for the handoff --deposit-to tests. Separate from
// pipeline_test.go's helper because each test file gets its own test
// DB to avoid cross-test contention.
func newHandoffDepositTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "handoff-deposit.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	require.NoError(t, pipeline.ConfigureDB(context.Background(), db))
	t.Cleanup(func() { _ = db.Close() })

	store, err := pipeline.OpenStore(context.Background(), db)
	require.NoError(t, err)

	srv := pipeline.NewServer(store, nil)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// createDepositTestSession creates a session on the given pipeline
// server and returns its id, ready for a handoff --deposit-to call.
func createDepositTestSession(t *testing.T, serverURL, target string) string {
	t.Helper()
	client, err := pipeline.NewClient(serverURL)
	require.NoError(t, err)
	sess, err := client.CreateSession(context.Background(), target, "")
	require.NoError(t, err)
	return sess.ID
}

// TestHandoff_DepositTo_HappyPath covers the primary new path:
// --deposit-to posts the rendered handoff into the session as a
// 'handoff' message with the correct role, and the server can read
// it back identically. Replaces the former skill pattern of
// `signatory handoff ...` into a shell variable + `curl -X POST`
// (retired in the same branch that introduced this verb).
func TestHandoff_DepositTo_HappyPath(t *testing.T) {
	ts := newHandoffDepositTestServer(t)
	sessionID := createDepositTestSession(t, ts.URL, "https://github.com/nvbn/thefuck")

	cmd := &HandoffCmd{
		Role:        "security",
		Target:      "https://github.com/nvbn/thefuck",
		Path:        "/tmp/thefuck-clone",
		Language:    "python",
		Intake:      "does this leak credentials?",
		DepositTo:   sessionID,
		PipelineURL: ts.URL,
		Quiet:       true,
	}
	require.NoError(t, cmd.Run(&Globals{}))

	// Confirm the message landed. The pipeline.Client surface has no
	// Get method yet (v0.1 only needs CreateSession + DepositMessage),
	// so we assert the side effect via a raw HTTP GET against the
	// format=raw endpoint — the exact endpoint the skill's subagents
	// hit via WebFetch to retrieve their handoff.
	resp, err := ts.Client().Get(ts.URL + "/api/sessions/" + sessionID +
		"/messages?role=security&type=handoff&format=raw")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, 200, resp.StatusCode)

	bodyBytes, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	rendered := string(bodyBytes)

	// The rendered handoff body must have landed verbatim — including
	// the placeholders we filled (target name, path, intake). The
	// whole point of --deposit-to is that these survive the round-trip
	// without shell-escape corruption, so assert on a representative
	// set of substitutions.
	assert.Contains(t, rendered, "thefuck",
		"rendered handoff must carry the target name into the deposited body")
	assert.Contains(t, rendered, "/tmp/thefuck-clone",
		"TARGET_PATH must be substituted in the deposited body")
	assert.Contains(t, rendered, "does this leak credentials?",
		"INTAKE_QUESTION must be substituted in the deposited body")
}

func TestHandoff_DepositTo_ConflictsWithOutput(t *testing.T) {
	t.Parallel()
	// No server needed — the conflict is caught before any network
	// work happens.
	cmd := &HandoffCmd{
		Role:        "security",
		Target:      "https://github.com/nvbn/thefuck",
		Path:        "/tmp/thefuck",
		Language:    "python",
		DepositTo:   "some-session-id",
		Output:      filepath.Join(t.TempDir(), "out.md"),
		PipelineURL: "http://127.0.0.1:1", // not used; the conflict fires first
		Quiet:       true,
	}
	err := cmd.Run(&Globals{})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUsage,
		"two-destination misuse is user input, must surface as EX_USAGE")
	assert.Contains(t, err.Error(), "mutually exclusive")
}

// TestHandoff_DepositTo_StdoutStaysEmpty guards the single-destination
// rule from the stdout side: when --deposit-to is set and --output is
// not, nothing goes to stdout. The skill captures stdout for shell
// interpolation in other commands; leaking a handoff body here would
// break callers downstream if they did `$(signatory handoff ... --deposit-to …)`.
func TestHandoff_DepositTo_StdoutStaysEmpty(t *testing.T) {
	ts := newHandoffDepositTestServer(t)
	sessionID := createDepositTestSession(t, ts.URL, "https://github.com/nvbn/thefuck")

	cmd := &HandoffCmd{
		Role:        "security",
		Target:      "https://github.com/nvbn/thefuck",
		Path:        "/tmp/thefuck",
		Language:    "python",
		DepositTo:   sessionID,
		PipelineURL: ts.URL,
		Quiet:       true,
	}
	stdout := captureStdout(t, func() {
		require.NoError(t, cmd.Run(&Globals{}))
	})
	assert.Empty(t, stdout,
		"stdout must be empty when --deposit-to is the destination")
}

// TestHandoff_DepositTo_StderrReportsDeposit verifies the operator-
// visible feedback: when --quiet is off, stderr gets a
// "# deposited: session=X role=Y bytes=N" line. Without this, a
// successful deposit is indistinguishable from a silent no-op.
func TestHandoff_DepositTo_StderrReportsDeposit(t *testing.T) {
	ts := newHandoffDepositTestServer(t)
	sessionID := createDepositTestSession(t, ts.URL, "https://github.com/nvbn/thefuck")

	cmd := &HandoffCmd{
		Role:        "security",
		Target:      "https://github.com/nvbn/thefuck",
		Path:        "/tmp/thefuck",
		Language:    "python",
		DepositTo:   sessionID,
		PipelineURL: ts.URL,
		// Quiet intentionally off.
	}
	stderr := captureStderr(t, func() {
		require.NoError(t, cmd.Run(&Globals{}))
	})
	assert.Contains(t, stderr, "# deposited:",
		"stderr must carry the deposit report when --quiet is off")
	assert.Contains(t, stderr, "session="+sessionID,
		"deposit report must name the session")
	assert.Contains(t, stderr, "role=security",
		"deposit report must name the role")
	assert.Contains(t, stderr, "bytes=",
		"deposit report must include the byte count")
}

// TestHandoff_DepositTo_Quiet suppresses the deposit report along
// with everything else.
func TestHandoff_DepositTo_Quiet(t *testing.T) {
	ts := newHandoffDepositTestServer(t)
	sessionID := createDepositTestSession(t, ts.URL, "https://github.com/nvbn/thefuck")

	cmd := &HandoffCmd{
		Role:        "security",
		Target:      "https://github.com/nvbn/thefuck",
		Path:        "/tmp/thefuck",
		Language:    "python",
		DepositTo:   sessionID,
		PipelineURL: ts.URL,
		Quiet:       true,
	}
	stderr := captureStderr(t, func() {
		require.NoError(t, cmd.Run(&Globals{}))
	})
	assert.NotContains(t, stderr, "# deposited:",
		"--quiet must suppress the deposit report")
}

// TestHandoff_DepositTo_UnknownSession covers the most common
// production failure: a typo'd session id, or a session that's been
// deleted. The store-level FK constraint catches the bad reference
// and is translated into pipeline.ErrSessionNotFound; the server
// maps that sentinel into an error body that names the bogus
// session id so the user can correct their input.
//
// What this test pins end-to-end: (1) the command errors rather
// than silently succeeding, (2) the error text names the deposit
// operation, and (3) it surfaces the session id in the actionable
// "session %q not found" phrasing — not an opaque "internal error."
func TestHandoff_DepositTo_UnknownSession(t *testing.T) {
	ts := newHandoffDepositTestServer(t)

	const bogusID = "00000000-0000-0000-0000-000000000000"
	cmd := &HandoffCmd{
		Role:        "security",
		Target:      "https://github.com/nvbn/thefuck",
		Path:        "/tmp/thefuck",
		Language:    "python",
		DepositTo:   bogusID, // never created
		PipelineURL: ts.URL,
		Quiet:       true,
	}
	err := cmd.Run(&Globals{})
	require.Error(t, err,
		"deposit against a nonexistent session must fail")
	assert.Contains(t, err.Error(), "handoff deposit",
		"error must name the operation so the user knows what failed")
	assert.Contains(t, err.Error(), bogusID,
		"error must carry the bogus session id so the user sees their typo")
	assert.Contains(t, err.Error(), "not found",
		"error must be 'not found', not 'internal error' — caller needs actionable text")
}

// TestHandoff_DepositTo_ServerUnreachable checks that a network-level
// failure (the service isn't running) produces a clear error with the
// operation named.
func TestHandoff_DepositTo_ServerUnreachable(t *testing.T) {
	ts := newHandoffDepositTestServer(t)
	// Create a session against the live server, THEN close it so the
	// session id is well-formed but the server is gone.
	sessionID := createDepositTestSession(t, ts.URL, "https://github.com/nvbn/thefuck")
	unreachableURL := ts.URL
	ts.Close()

	cmd := &HandoffCmd{
		Role:        "security",
		Target:      "https://github.com/nvbn/thefuck",
		Path:        "/tmp/thefuck",
		Language:    "python",
		DepositTo:   sessionID,
		PipelineURL: unreachableURL,
		Quiet:       true,
	}
	err := cmd.Run(&Globals{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "handoff deposit",
		"error must name the operation so the user knows what failed")
}

// TestHandoff_DepositTo_BadPipelineURL catches the "misconfigured
// --pipeline-url" case at client construction.
func TestHandoff_DepositTo_BadPipelineURL(t *testing.T) {
	t.Parallel()
	cmd := &HandoffCmd{
		Role:        "security",
		Target:      "https://github.com/nvbn/thefuck",
		Path:        "/tmp/thefuck",
		Language:    "python",
		DepositTo:   "00000000-0000-0000-0000-000000000000",
		PipelineURL: "not-a-url", // no scheme
		Quiet:       true,
	}
	err := cmd.Run(&Globals{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pipeline",
		"error must mention pipeline so the user knows --pipeline-url was the problem")
}

// TestHandoff_DepositTo_Role covers a non-security role path to
// prove the role threaded into DepositMessage comes from cmd.Role,
// not a hardcoded constant. Provenance is the smallest non-security
// role to exercise (needs --ecosystem; no --language dependency).
//
// Asserts role routing by fetching back through the role=provenance
// filter — the server's handleGetMessages query returns the message
// body only when the role filter matches what was stored. The body
// must also contain a provenance-specific substitution so we're not
// mistakenly seeing any-old handoff come back.
func TestHandoff_DepositTo_Role(t *testing.T) {
	ts := newHandoffDepositTestServer(t)
	sessionID := createDepositTestSession(t, ts.URL, "https://github.com/nvbn/thefuck")

	cmd := &HandoffCmd{
		Role:        "provenance",
		Target:      "https://github.com/nvbn/thefuck",
		Path:        "/tmp/thefuck",
		Ecosystem:   "pypi",
		DepositTo:   sessionID,
		PipelineURL: ts.URL,
		Quiet:       true,
	}
	require.NoError(t, cmd.Run(&Globals{}))

	resp, err := ts.Client().Get(ts.URL + "/api/sessions/" + sessionID +
		"/messages?role=provenance&type=handoff&format=raw")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)

	bodyBytes, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	rendered := string(bodyBytes)
	assert.Contains(t, rendered, "thefuck",
		"fetched-under-role=provenance body must be the rendered handoff, not empty")
	assert.Contains(t, rendered, "pypi",
		"provenance handoff renders the ecosystem into the body")
}
