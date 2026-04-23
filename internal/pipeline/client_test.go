package pipeline_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/pipeline"
)

// newTestClientServer stands up a real pipeline.Server wrapped in
// httptest (plain HTTP, random port) and returns a Client pointed at
// it. The http:// scheme exercises the NewClient path that skips TLS
// trust config — the production TLS path is covered elsewhere
// (end-to-end, by manual / dogfood runs), not in these HTTP-shape
// tests.
func newTestClientServer(t *testing.T) (*pipeline.Client, *httptest.Server) {
	t.Helper()
	db := openTestDB(t)
	store, err := pipeline.OpenStore(context.Background(), db)
	require.NoError(t, err)

	srv := pipeline.NewServer(store, nil)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	client, err := pipeline.NewClient(ts.URL)
	require.NoError(t, err)
	return client, ts
}

func TestClient_CreateSession_HappyPath(t *testing.T) {
	t.Parallel()
	client, _ := newTestClientServer(t)

	sess, err := client.CreateSession(context.Background(),
		"repo:github/nvbn/thefuck", "")
	require.NoError(t, err)
	assert.NotEmpty(t, sess.ID, "server must assign a session ID")
	assert.Equal(t, "repo:github/nvbn/thefuck", sess.Target)
	assert.Equal(t, "active", sess.Status)
	assert.False(t, sess.CreatedAt.IsZero(), "server must set CreatedAt")
}

func TestClient_CreateSession_WithMetadata(t *testing.T) {
	t.Parallel()
	client, _ := newTestClientServer(t)

	sess, err := client.CreateSession(context.Background(),
		"repo:github/alecthomas/kong", `{"lang":"go"}`)
	require.NoError(t, err)
	assert.Equal(t, `{"lang":"go"}`, sess.Metadata)
}

// TestClient_CreateSession_EmptyTargetRejected covers the primary
// validation error surface: the server rejects empty targets with a
// 400 + {"error":"target is required"}. The client must surface that
// server message verbatim (wrapped with status code) so the CLI caller
// can print it without additional decoding.
func TestClient_CreateSession_EmptyTargetRejected(t *testing.T) {
	t.Parallel()
	client, _ := newTestClientServer(t)

	sess, err := client.CreateSession(context.Background(), "", "")
	assert.Nil(t, sess)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "400", "status code surfaces")
	assert.Contains(t, err.Error(), "target is required",
		"server error message surfaces verbatim")
}

// TestClient_CreateSession_ServerUnreachable exercises the error path
// when the server isn't there (TCP-level failure). We stand up a real
// server to borrow its URL, then close it before calling. The client
// must return a descriptive error, not a nil panic or a silent zero
// Session.
func TestClient_CreateSession_ServerUnreachable(t *testing.T) {
	t.Parallel()
	_, ts := newTestClientServer(t)
	unreachableURL := ts.URL // capture BEFORE closing
	ts.Close()               // TCP port now refuses connections

	client, err := pipeline.NewClient(unreachableURL)
	require.NoError(t, err)

	sess, err := client.CreateSession(context.Background(),
		"repo:github/x/y", "")
	assert.Nil(t, sess)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "create session",
		"error must name the operation that failed")
}

// TestNewClient_UnsupportedScheme rejects URLs with schemes other than
// http/https at construction time — a typo like `127.0.0.1:21517`
// (missing scheme) or `tcp://…` gets caught here rather than deep in
// a request that would produce an obscure HTTP error.
func TestNewClient_UnsupportedScheme(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		url  string
	}{
		{"missing scheme", "127.0.0.1:21517"},
		{"ftp scheme", "ftp://127.0.0.1:21517"},
		{"empty", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := pipeline.NewClient(tc.url)
			require.Error(t, err)
		})
	}
}

// TestNewClient_TrailingSlashTolerated covers a UX quirk: callers may
// pass "https://127.0.0.1:21517/" (with trailing slash) by mistake.
// The client strips trailing slashes from baseURL so request-path
// construction doesn't produce "//api/sessions".
func TestNewClient_TrailingSlashTolerated(t *testing.T) {
	t.Parallel()
	_, ts := newTestClientServer(t)

	// Strip the scheme+host+port out and re-attach with a trailing
	// slash. httptest.Server.URL has no trailing slash; we add one.
	client, err := pipeline.NewClient(ts.URL + "/")
	require.NoError(t, err)

	sess, err := client.CreateSession(context.Background(),
		"repo:github/x/y", "")
	require.NoError(t, err)
	assert.NotEmpty(t, sess.ID)
}

// TestClient_ContextCancellation verifies the client honors ctx
// cancellation — a skill that gets Ctrl-C mid-deposit should see
// its HTTP call abort rather than block on the 60s timeout.
func TestClient_ContextCancellation(t *testing.T) {
	t.Parallel()
	client, _ := newTestClientServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled

	sess, err := client.CreateSession(ctx, "repo:github/x/y", "")
	assert.Nil(t, sess)
	require.Error(t, err)
	// The error will be context.Canceled wrapped in http.Client's error
	// chain; the exact prefix varies by Go version, so we check for
	// the substring.
	assert.True(t,
		strings.Contains(err.Error(), "context canceled") ||
			strings.Contains(err.Error(), "context deadline exceeded"),
		"cancellation must propagate; got: %v", err)
}

// TestDefaultURL pins the production default so a future edit that
// silently drops the port or flips the scheme triggers a test
// failure. Trust architecture (design/tls-trust.md) commits to
// https:// and port 21517; this is the guardrail.
func TestDefaultURL(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "https://127.0.0.1:21517", pipeline.DefaultURL)
	assert.True(t, strings.HasPrefix(pipeline.DefaultURL, "https://"),
		"trust architecture requires HTTPS")
}

// createClientTestSession is the pipeline.Client-flavored sibling of
// hardening_test.go's createTestSession (which takes an *http.Client
// for the raw-HTTP hardening tests). Creates a session via the typed
// client surface and returns its id.
func createClientTestSession(t *testing.T, client *pipeline.Client, target string) string {
	t.Helper()
	sess, err := client.CreateSession(context.Background(), target, "")
	require.NoError(t, err)
	require.NotEmpty(t, sess.ID)
	return sess.ID
}

func TestClient_DepositMessage_HappyPath(t *testing.T) {
	t.Parallel()
	client, _ := newTestClientServer(t)
	sessionID := createClientTestSession(t, client, "repo:github/nvbn/thefuck")

	// Content deliberately carries literal newlines and a quote — the
	// whole reason --deposit-to exists is to avoid shell-level JSON
	// escaping of exactly these characters.
	content := "line one\nline \"two\"\nline three\n"
	msg, err := client.DepositMessage(context.Background(),
		sessionID, "security", "handoff", content, "")
	require.NoError(t, err)

	assert.NotZero(t, msg.ID, "server must assign a message ID")
	assert.Equal(t, sessionID, msg.SessionID)
	assert.Equal(t, "security", msg.Role)
	assert.Equal(t, "handoff", msg.MsgType)
	assert.Equal(t, content, msg.Content, "literal newlines and quotes round-trip unchanged")
	assert.False(t, msg.CreatedAt.IsZero(), "server must set CreatedAt")
}

// TestClient_DepositMessage_EmptySessionID catches caller mistakes
// at the client boundary rather than letting them reach the server
// as a request to "/api/sessions//messages" (which would 404 with
// an obscure error).
func TestClient_DepositMessage_EmptySessionID(t *testing.T) {
	t.Parallel()
	client, _ := newTestClientServer(t)

	msg, err := client.DepositMessage(context.Background(),
		"", "security", "handoff", "content", "")
	assert.Nil(t, msg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "session id is required")
}

func TestClient_DepositMessage_InvalidRole(t *testing.T) {
	t.Parallel()
	client, _ := newTestClientServer(t)
	sessionID := createClientTestSession(t, client, "repo:github/x/y")

	msg, err := client.DepositMessage(context.Background(),
		sessionID, "not-a-role", "handoff", "body", "")
	assert.Nil(t, msg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid role",
		"server's role-validation message must surface to the caller")
}

func TestClient_DepositMessage_InvalidMsgType(t *testing.T) {
	t.Parallel()
	client, _ := newTestClientServer(t)
	sessionID := createClientTestSession(t, client, "repo:github/x/y")

	msg, err := client.DepositMessage(context.Background(),
		sessionID, "security", "not-a-type", "body", "")
	assert.Nil(t, msg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid msg_type")
}

func TestClient_DepositMessage_EmptyContent(t *testing.T) {
	t.Parallel()
	client, _ := newTestClientServer(t)
	sessionID := createClientTestSession(t, client, "repo:github/x/y")

	msg, err := client.DepositMessage(context.Background(),
		sessionID, "security", "handoff", "", "")
	assert.Nil(t, msg)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "content is required")
}

// TestClient_DepositMessage_LargeBody exercises a body near the
// server's upper bound to confirm the default timeout + drain-on-close
// path handle realistic handoff sizes. The server cap is 10 MB; we
// send 1 MB, which comfortably exceeds any real handoff template.
func TestClient_DepositMessage_LargeBody(t *testing.T) {
	t.Parallel()
	client, _ := newTestClientServer(t)
	sessionID := createClientTestSession(t, client, "repo:github/x/y")

	content := strings.Repeat("x", 1024*1024) // 1 MB
	msg, err := client.DepositMessage(context.Background(),
		sessionID, "security", "handoff", content, "")
	require.NoError(t, err)
	assert.Equal(t, len(content), len(msg.Content), "full body round-trips")
}
