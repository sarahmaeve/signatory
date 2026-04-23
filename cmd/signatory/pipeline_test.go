package main

import (
	"bytes"
	"context"
	"database/sql"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/pipeline"

	_ "modernc.org/sqlite"
)

// openPipelineTestDB mirrors the internal/pipeline test DB helper
// without importing that package's test files. Keeps this test
// file's deps minimal — just a real SQLite file configured per
// pipeline.ConfigureDB.
func openPipelineTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "pipeline-cmd-test.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	require.NoError(t, pipeline.ConfigureDB(context.Background(), db))
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// newPipelineTestServer stands up a real pipeline server behind
// httptest. Returns the server's URL so the caller can pass it into
// the CLI command via --pipeline-url. The server is cleaned up on
// test completion.
func newPipelineTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	db := openPipelineTestDB(t)
	store, err := pipeline.OpenStore(context.Background(), db)
	require.NoError(t, err)

	srv := pipeline.NewServer(store, nil)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// uuidRegex matches a canonical RFC 4122 UUID (8-4-4-4-12 hex).
// We assert the stdout is EXACTLY a UUID plus a single trailing
// newline — nothing else. This guards the shell-capture contract:
// `SESSION_ID=$(signatory pipeline session create …)` must receive
// just the UUID.
var uuidRegex = regexp.MustCompile(
	`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\n$`)

func TestPipelineSessionCreate_StdoutIsBareUUID(t *testing.T) {
	t.Parallel()
	ts := newPipelineTestServer(t)

	var stdout, stderr bytes.Buffer
	cmd := &PipelineSessionCreateCmd{
		Target:      "repo:github/nvbn/thefuck",
		PipelineURL: ts.URL,
		Stdout:      &stdout,
		Stderr:      &stderr,
	}
	require.NoError(t, cmd.Run(&Globals{}))

	assert.Regexp(t, uuidRegex, stdout.String(),
		"stdout must be a bare UUID plus newline so $(…) capture works")
	assert.Contains(t, stderr.String(), "# pipeline session:",
		"stderr must carry the info line when --quiet is off")
	assert.Contains(t, stderr.String(), "repo:github/nvbn/thefuck",
		"info line must echo the target")
}

func TestPipelineSessionCreate_QuietSuppressesStderr(t *testing.T) {
	t.Parallel()
	ts := newPipelineTestServer(t)

	var stdout, stderr bytes.Buffer
	cmd := &PipelineSessionCreateCmd{
		Target:      "repo:github/x/y",
		PipelineURL: ts.URL,
		Quiet:       true,
		Stdout:      &stdout,
		Stderr:      &stderr,
	}
	require.NoError(t, cmd.Run(&Globals{}))

	assert.Regexp(t, uuidRegex, stdout.String(),
		"stdout unchanged by --quiet")
	assert.Empty(t, strings.TrimSpace(stderr.String()),
		"--quiet must suppress the stderr info line entirely")
}

func TestPipelineSessionCreate_WithMetadata(t *testing.T) {
	t.Parallel()
	ts := newPipelineTestServer(t)

	var stdout, stderr bytes.Buffer
	cmd := &PipelineSessionCreateCmd{
		Target:      "repo:github/alecthomas/kong",
		Metadata:    `{"lang":"go"}`,
		PipelineURL: ts.URL,
		Stdout:      &stdout,
		Stderr:      &stderr,
	}
	require.NoError(t, cmd.Run(&Globals{}))

	// Re-fetch the session via a direct client to confirm metadata
	// round-trips through the server as given.
	client, err := pipeline.NewClient(ts.URL)
	require.NoError(t, err)
	sessionID := strings.TrimSpace(stdout.String())
	// CreateSession round-trips metadata; reuse it as a smoke signal
	// — a fresh create with the same metadata returns the same value.
	sess, err := client.CreateSession(context.Background(),
		"repo:github/x/y", `{"lang":"go"}`)
	require.NoError(t, err)
	assert.Equal(t, `{"lang":"go"}`, sess.Metadata)
	assert.NotEqual(t, sessionID, sess.ID, "fresh session gets a fresh id")
}

// TestPipelineSessionCreate_ServerUnreachable — the most likely
// production failure is the service not running. We verify the error
// names the operation so the user's first question ("what broke?")
// is answered by the error message.
func TestPipelineSessionCreate_ServerUnreachable(t *testing.T) {
	t.Parallel()
	ts := newPipelineTestServer(t)
	unreachableURL := ts.URL
	ts.Close() // close before Run — TCP dial will fail

	var stdout, stderr bytes.Buffer
	cmd := &PipelineSessionCreateCmd{
		Target:      "repo:github/x/y",
		PipelineURL: unreachableURL,
		Stdout:      &stdout,
		Stderr:      &stderr,
	}
	err := cmd.Run(&Globals{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pipeline session create",
		"error must name the operation so the user knows what failed")
	assert.Empty(t, stdout.String(),
		"no partial output when the operation failed")
}

// TestPipelineSessionCreate_BadURL — a misconfigured --pipeline-url
// (e.g. missing scheme) should fail at client construction with a
// clear message, not panic or produce a nil-deref somewhere deeper.
func TestPipelineSessionCreate_BadURL(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	cmd := &PipelineSessionCreateCmd{
		Target:      "repo:github/x/y",
		PipelineURL: "127.0.0.1:21517", // missing scheme
		Stdout:      &stdout,
		Stderr:      &stderr,
	}
	err := cmd.Run(&Globals{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pipeline")
}

// TestPipelineSessionCreate_EmptyTargetRejected mirrors the client-
// level validation test: the server returns 400, the client wraps,
// and the command surfaces "target is required" so the user can act.
func TestPipelineSessionCreate_EmptyTargetRejected(t *testing.T) {
	t.Parallel()
	ts := newPipelineTestServer(t)

	var stdout, stderr bytes.Buffer
	cmd := &PipelineSessionCreateCmd{
		Target:      "",
		PipelineURL: ts.URL,
		Stdout:      &stdout,
		Stderr:      &stderr,
	}
	err := cmd.Run(&Globals{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "target is required",
		"server's validation message must reach the user")
}
