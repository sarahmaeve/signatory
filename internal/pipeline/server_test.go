package pipeline_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/pipeline"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	db := openTestDB(t)
	s, err := pipeline.OpenStore(context.Background(), db)
	require.NoError(t, err)
	srv := pipeline.NewServer(s, nil)
	return httptest.NewServer(srv.Handler())
}

// TestHTTP_EndToEnd_SmokeTest exercises the full lifecycle: create
// session → deposit handoff → retrieve as JSON → retrieve as raw
// text → deposit feedback → get latest → delete session.
func TestHTTP_EndToEnd_SmokeTest(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	defer ts.Close()
	client := ts.Client()

	// --- Create session ---
	resp, err := doPost(t, client, ts.URL+"/api/sessions",
		strings.NewReader(`{"target":"repo:github/spf13/cobra","metadata":"{\"lang\":\"go\"}"}`))
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	var sess pipeline.Session
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&sess))
	resp.Body.Close()
	assert.NotEmpty(t, sess.ID)
	assert.Equal(t, "repo:github/spf13/cobra", sess.Target)
	assert.Equal(t, "active", sess.Status)

	sessionURL := ts.URL + "/api/sessions/" + sess.ID

	// --- List sessions ---
	resp, err = doGet(t, client, ts.URL+"/api/sessions")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var sessions []pipeline.Session
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&sessions))
	resp.Body.Close()
	require.Len(t, sessions, 1)
	assert.Equal(t, sess.ID, sessions[0].ID)

	// --- Deposit security handoff ---
	handoffContent := "# Security review for cobra\n\nThis is the full handoff."
	resp, err = doPost(t, client, sessionURL+"/messages",
		strings.NewReader(`{"role":"security","msg_type":"handoff","content":"`+
			strings.ReplaceAll(handoffContent, "\n", "\\n")+`"}`))
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var msg pipeline.Message
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&msg))
	resp.Body.Close()
	assert.Equal(t, "security", msg.Role)
	assert.Equal(t, "handoff", msg.MsgType)
	assert.Equal(t, handoffContent, msg.Content)

	// --- Deposit provenance handoff ---
	resp, err = doPost(t, client, sessionURL+"/messages",
		strings.NewReader(`{"role":"provenance","msg_type":"handoff","content":"# Provenance handoff"}`))
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	resp.Body.Close()

	// --- Retrieve as JSON (filtered by role + type) ---
	resp, err = doGet(t, client, sessionURL+"/messages?role=security&type=handoff")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var msgs []pipeline.Message
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&msgs))
	resp.Body.Close()
	require.Len(t, msgs, 1)
	assert.Equal(t, handoffContent, msgs[0].Content)

	// --- Retrieve as raw text (what WebFetch agents get) ---
	resp, err = doGet(t, client, sessionURL+"/messages?role=security&type=handoff&format=raw")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/plain; charset=utf-8", resp.Header.Get("Content-Type"))
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.NoError(t, err)
	assert.Equal(t, handoffContent, string(body))

	// --- Deposit feedback ---
	resp, err = doPost(t, client, sessionURL+"/messages",
		strings.NewReader(`{"role":"security","msg_type":"feedback","content":"absence A001 missing description"}`))
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	resp.Body.Close()

	// --- Get latest feedback ---
	resp, err = doGet(t, client, sessionURL+"/messages/latest?role=security&type=feedback")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var latestMsg pipeline.Message
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&latestMsg))
	resp.Body.Close()
	assert.Equal(t, "absence A001 missing description", latestMsg.Content)

	// --- Get latest as raw ---
	resp, err = doGet(t, client, sessionURL+"/messages/latest?role=security&type=feedback&format=raw")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err = io.ReadAll(resp.Body)
	resp.Body.Close()
	require.NoError(t, err)
	assert.Equal(t, "absence A001 missing description", string(body))

	// --- Verify all messages for session ---
	resp, err = doGet(t, client, sessionURL+"/messages")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&msgs))
	resp.Body.Close()
	assert.Len(t, msgs, 3) // 2 handoffs + 1 feedback

	// --- Delete session ---
	resp, err = doRequest(t, client, http.MethodDelete, sessionURL, nil)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
	resp.Body.Close()

	// --- Verify deletion ---
	resp, err = doGet(t, client, sessionURL)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	resp.Body.Close()
}

// TestHTTP_SessionIsolation verifies that two concurrent sessions
// don't leak messages into each other.
func TestHTTP_SessionIsolation(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	defer ts.Close()
	client := ts.Client()

	// Create two sessions.
	createSession := func(target string) string {
		resp, err := doPost(t, client, ts.URL+"/api/sessions",
			strings.NewReader(`{"target":"`+target+`"}`))
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, resp.StatusCode)
		var sess pipeline.Session
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&sess))
		resp.Body.Close()
		return sess.ID
	}
	s1 := createSession("repo:github/alpha/one")
	s2 := createSession("repo:github/beta/two")

	// Deposit in each.
	deposit := func(sessionID, content string) {
		resp, err := doPost(t, client,
			ts.URL+"/api/sessions/"+sessionID+"/messages",
			strings.NewReader(`{"role":"security","msg_type":"handoff","content":"`+content+`"}`))
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, resp.StatusCode)
		resp.Body.Close()
	}
	deposit(s1, "alpha content")
	deposit(s2, "beta content")

	// Each session only sees its own.
	getContent := func(sessionID string) string {
		resp, err := doGet(t, client,
			ts.URL+"/api/sessions/"+sessionID+"/messages?role=security&type=handoff&format=raw")
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		require.NoError(t, err)
		return string(body)
	}
	assert.Equal(t, "alpha content", getContent(s1))
	assert.Equal(t, "beta content", getContent(s2))
}

// TestHTTP_ValidationErrors exercises error paths.
func TestHTTP_ValidationErrors(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	defer ts.Close()
	client := ts.Client()

	// Create session missing target.
	resp, err := doPost(t, client, ts.URL+"/api/sessions",
		strings.NewReader(`{}`))
	require.NoError(t, err)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp.Body.Close()

	// Deposit message missing role.
	resp, err = doPost(t, client, ts.URL+"/api/sessions",
		strings.NewReader(`{"target":"test"}`))
	require.NoError(t, err)
	var sess pipeline.Session
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&sess))
	resp.Body.Close()

	resp, err = doPost(t, client,
		ts.URL+"/api/sessions/"+sess.ID+"/messages",
		strings.NewReader(`{"msg_type":"handoff","content":"test"}`))
	require.NoError(t, err)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp.Body.Close()

	// Deposit message missing content.
	resp, err = doPost(t, client,
		ts.URL+"/api/sessions/"+sess.ID+"/messages",
		strings.NewReader(`{"role":"security","msg_type":"handoff","content":""}`))
	require.NoError(t, err)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp.Body.Close()

	// Get non-existent session.
	resp, err = doGet(t, client, ts.URL+"/api/sessions/nonexistent")
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	resp.Body.Close()

	// Get latest from empty session.
	resp, err = doGet(t, client,
		ts.URL+"/api/sessions/"+sess.ID+"/messages/latest?role=security&type=feedback")
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	resp.Body.Close()
}
