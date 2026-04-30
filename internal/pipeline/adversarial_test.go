package pipeline_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/pipeline"

	_ "modernc.org/sqlite"
)

// ---------------------------------------------------------------------------
// Store-level adversarial tests
// ---------------------------------------------------------------------------

func TestStore_LargeMessageContent(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	sess, err := s.CreateSession(ctx, "repo:github/large/payload", "")
	require.NoError(t, err)

	// 2 MB of content — well beyond typical handoff size.
	bigContent := strings.Repeat("A", 2*1024*1024)

	msg, err := s.DepositMessage(ctx, &pipeline.Message{
		SessionID: sess.ID,
		Role:      "security",
		MsgType:   "output",
		Content:   bigContent,
	})
	require.NoError(t, err)
	assert.Equal(t, len(bigContent), len(msg.Content))

	// Round-trip: content survives storage and retrieval intact.
	msgs, err := s.GetMessages(ctx, pipeline.MessageFilter{SessionID: sess.ID})
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	assert.Equal(t, len(bigContent), len(msgs[0].Content))
	assert.Equal(t, bigContent, msgs[0].Content)
}

func TestStore_EmptySessionMessages(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	sess, err := s.CreateSession(ctx, "repo:github/empty/session", "")
	require.NoError(t, err)

	msgs, err := s.GetMessages(ctx, pipeline.MessageFilter{SessionID: sess.ID})
	require.NoError(t, err)
	// Should return nil slice (no rows appended), not an error.
	assert.Empty(t, msgs)
}

func TestStore_DuplicateTargetDifferentSessions(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	target := "repo:github/duplicate/target"
	sess1, err := s.CreateSession(ctx, target, `{"run":1}`)
	require.NoError(t, err)
	sess2, err := s.CreateSession(ctx, target, `{"run":2}`)
	require.NoError(t, err)

	// Different IDs, same target — both valid.
	assert.NotEqual(t, sess1.ID, sess2.ID)
	assert.Equal(t, target, sess1.Target)
	assert.Equal(t, target, sess2.Target)

	// Both retrievable independently.
	got1, err := s.GetSession(ctx, sess1.ID)
	require.NoError(t, err)
	assert.Equal(t, `{"run":1}`, got1.Metadata)

	got2, err := s.GetSession(ctx, sess2.ID)
	require.NoError(t, err)
	assert.Equal(t, `{"run":2}`, got2.Metadata)
}

func TestStore_MessageOrdering(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	sess, err := s.CreateSession(ctx, "repo:github/order/test", "")
	require.NoError(t, err)

	// Deposit messages with deterministic content to verify order.
	// SQLite AUTOINCREMENT guarantees ascending IDs, so even if
	// created_at has identical timestamps the ORDER BY id ASC
	// clause in GetMessages provides stable ordering.
	contents := []string{"first", "second", "third", "fourth", "fifth"}
	for _, c := range contents {
		_, err := s.DepositMessage(ctx, &pipeline.Message{
			SessionID: sess.ID,
			Role:      "orchestrator",
			MsgType:   "status",
			Content:   c,
		})
		require.NoError(t, err)
	}

	msgs, err := s.GetMessages(ctx, pipeline.MessageFilter{SessionID: sess.ID})
	require.NoError(t, err)
	require.Len(t, msgs, len(contents))

	for i, want := range contents {
		assert.Equal(t, want, msgs[i].Content, "message %d out of order", i)
	}

	// IDs should be strictly ascending.
	for i := 1; i < len(msgs); i++ {
		assert.Greater(t, msgs[i].ID, msgs[i-1].ID,
			"IDs not ascending at index %d", i)
	}
}

func TestStore_ConcurrentDeposits(t *testing.T) {
	t.Parallel()
	// openTestDB calls ConfigureDB (WAL + busy_timeout + MaxOpenConns=1)
	// to match production SQLite configuration. Without these pragmas,
	// concurrent goroutines hitting database/sql's connection pool
	// trigger SQLITE_BUSY — see TestStore_ConcurrentDeposits_NoPragmas
	// for documentation of that failure mode.
	db := openTestDB(t)
	s, err := pipeline.OpenStore(context.Background(), db)
	require.NoError(t, err)
	ctx := context.Background()

	sess, err := s.CreateSession(ctx, "repo:github/concurrent/test", "")
	require.NoError(t, err)

	const goroutines = 10
	const msgsPerGoroutine = 20

	var wg sync.WaitGroup
	errs := make(chan error, goroutines*msgsPerGoroutine)

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(gID int) {
			defer wg.Done()
			for m := 0; m < msgsPerGoroutine; m++ {
				_, err := s.DepositMessage(ctx, &pipeline.Message{
					SessionID: sess.ID,
					Role:      "security",
					MsgType:   "output",
					Content:   fmt.Sprintf("g%d-m%d", gID, m),
				})
				if err != nil {
					errs <- err
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		require.NoError(t, err, "concurrent deposit failed")
	}

	msgs, err := s.GetMessages(ctx, pipeline.MessageFilter{SessionID: sess.ID})
	require.NoError(t, err)
	assert.Len(t, msgs, goroutines*msgsPerGoroutine)

	// All IDs should be unique.
	seen := make(map[int64]bool, len(msgs))
	for _, msg := range msgs {
		assert.False(t, seen[msg.ID], "duplicate message ID: %d", msg.ID)
		seen[msg.ID] = true
	}
}

// TestStore_ConcurrentDeposits_NoPragmas documents that without proper
// SQLite pragmas (WAL mode, busy_timeout, MaxOpenConns=1), concurrent
// writes via database/sql's default connection pool produce SQLITE_BUSY
// errors. This is NOT a test that should pass — it documents a real
// failure mode that OpenStore callers must guard against by configuring
// the *sql.DB before passing it in.
//
// NOTE: openTestStore now calls ConfigureDB, so this test deliberately
// opens a raw *sql.DB without ConfigureDB to preserve the documented
// failure mode.
func TestStore_ConcurrentDeposits_NoPragmas(t *testing.T) {
	t.Parallel()
	// Deliberately skip ConfigureDB to demonstrate the failure mode.
	dbPath := filepath.Join(t.TempDir(), "nopragma-test.db")
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })

	s, err := pipeline.OpenStore(context.Background(), db)
	require.NoError(t, err)
	ctx := context.Background()

	sess, err := s.CreateSession(ctx, "repo:github/concurrent/nopragma", "")
	require.NoError(t, err)

	const goroutines = 8
	var wg sync.WaitGroup
	var busyCount int64
	var mu sync.Mutex

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(gID int) {
			defer wg.Done()
			for m := 0; m < 10; m++ {
				_, err := s.DepositMessage(ctx, &pipeline.Message{
					SessionID: sess.ID,
					Role:      "security",
					MsgType:   "output",
					Content:   fmt.Sprintf("g%d-m%d", gID, m),
				})
				if err != nil {
					mu.Lock()
					busyCount++
					mu.Unlock()
				}
			}
		}(g)
	}
	wg.Wait()

	// We expect SQLITE_BUSY errors. If zero errors, the DB driver
	// changed behavior — still fine, but surprising.
	t.Logf("SQLITE_BUSY errors without pragmas: %d/%d deposits", busyCount, goroutines*10)
}

func TestStore_UnicodeAndSpecialCharacters(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	sess, err := s.CreateSession(ctx, "repo:github/unicode/test", "")
	require.NoError(t, err)

	tests := []struct {
		name    string
		content string
	}{
		{"emoji", "Security analysis complete \U0001f512\U0001f50d"},
		{"cjk", "\u4fe1\u983c\u5206\u6790 - trust analysis"},
		{"newlines", "line1\nline2\nline3"},
		{"tabs_and_carriage_returns", "col1\tcol2\r\nrow1\trow2"},
		{"null_bytes", "before\x00after"},
		{"backslashes_and_quotes", `path: C:\Users\test "quoted"`},
		{"zero_width_chars", "trust\u200banalysis\u200b"},
		{"mixed_scripts", "\u0410\u043d\u0430\u043b\u0438\u0437 \u5206\u6790 Analysis"},
		{"long_emoji_sequence", "\U0001f468\u200d\U0001f469\u200d\U0001f467\u200d\U0001f466 family emoji"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg, err := s.DepositMessage(ctx, &pipeline.Message{
				SessionID: sess.ID,
				Role:      "security",
				MsgType:   "output",
				Content:   tt.content,
			})
			require.NoError(t, err)
			assert.Equal(t, tt.content, msg.Content)

			// Verify round-trip through GetLatestMessage.
			latest, err := s.GetLatestMessage(ctx, pipeline.MessageFilter{
				SessionID: sess.ID,
			})
			require.NoError(t, err)
			assert.Equal(t, tt.content, latest.Content)
		})
	}
}

// TestStore_DeleteSessionWhileDepositingRace exercises a delete
// running concurrently with a stream of deposits. We don't care
// which writes won — we care that neither goroutine panics nor
// corrupts the database, and that the delete eventually committed.
//
// Race-detector reliance: per-call deposit errors are tracked but
// not asserted against (FK violations are an expected outcome once
// the delete commits). The validators are (a) no panic, (b) no
// DATA RACE under `go test -race` (see
// internal/invariants/TestRaceDetectorWiredIntoCI), and (c) the
// session is gone post-Wait — asserted below. Earlier versions of
// this test ended with `_ = err` on the GetSession call, leaving
// (c) unchecked; a regression where DeleteSession silently no-op'd
// while the deposit goroutine held the connection would have
// passed.
func TestStore_DeleteSessionWhileDepositingRace(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	s, err := pipeline.OpenStore(context.Background(), db)
	require.NoError(t, err)
	ctx := context.Background()

	sess, err := s.CreateSession(ctx, "repo:github/race/delete", "")
	require.NoError(t, err)

	// Pre-populate some messages so delete has work to do.
	for i := 0; i < 5; i++ {
		_, err := s.DepositMessage(ctx, &pipeline.Message{
			SessionID: sess.ID,
			Role:      "security",
			MsgType:   "output",
			Content:   fmt.Sprintf("pre-msg-%d", i),
		})
		require.NoError(t, err)
	}

	// Concurrently deposit and delete. We don't care which wins —
	// we care that neither panics nor corrupts the database.
	var wg sync.WaitGroup

	// Goroutine 1: keep depositing messages.
	depositErrors := make(chan error, 100)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			_, err := s.DepositMessage(ctx, &pipeline.Message{
				SessionID: sess.ID,
				Role:      "security",
				MsgType:   "output",
				Content:   fmt.Sprintf("race-msg-%d", i),
			})
			if err != nil {
				depositErrors <- err
			}
		}
		close(depositErrors)
	}()

	// Goroutine 2: delete the session.
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = s.DeleteSession(ctx, sess.ID)
	}()

	wg.Wait()

	// End-state: the delete must have committed by now (its
	// goroutine finished, and with MaxOpenConns=1 the operation
	// is atomic on the single connection). The session must be
	// gone — GetSession should return an error.
	_, err = s.GetSession(ctx, sess.ID)
	assert.Error(t, err, "session must be deleted after the delete goroutine returns")
}

func TestStore_GetMessages_AllFilterCombinations(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	sess, err := s.CreateSession(ctx, "repo:github/filter/test", "")
	require.NoError(t, err)

	// Deposit a matrix of role x type messages.
	roles := []string{"security", "provenance"}
	types := []string{"handoff", "output"}

	for _, role := range roles {
		for _, msgType := range types {
			_, err := s.DepositMessage(ctx, &pipeline.Message{
				SessionID: sess.ID,
				Role:      role,
				MsgType:   msgType,
				Content:   fmt.Sprintf("%s-%s", role, msgType),
			})
			require.NoError(t, err)
		}
	}

	tests := []struct {
		name     string
		filter   pipeline.MessageFilter
		expected int
	}{
		{
			name:     "no_filter_returns_all",
			filter:   pipeline.MessageFilter{SessionID: sess.ID},
			expected: 4,
		},
		{
			name:     "role_only_security",
			filter:   pipeline.MessageFilter{SessionID: sess.ID, Role: "security"},
			expected: 2,
		},
		{
			name:     "role_only_provenance",
			filter:   pipeline.MessageFilter{SessionID: sess.ID, Role: "provenance"},
			expected: 2,
		},
		{
			name:     "type_only_handoff",
			filter:   pipeline.MessageFilter{SessionID: sess.ID, MsgType: "handoff"},
			expected: 2,
		},
		{
			name:     "type_only_output",
			filter:   pipeline.MessageFilter{SessionID: sess.ID, MsgType: "output"},
			expected: 2,
		},
		{
			name:     "both_security_handoff",
			filter:   pipeline.MessageFilter{SessionID: sess.ID, Role: "security", MsgType: "handoff"},
			expected: 1,
		},
		{
			name:     "both_provenance_output",
			filter:   pipeline.MessageFilter{SessionID: sess.ID, Role: "provenance", MsgType: "output"},
			expected: 1,
		},
		{
			name:     "nonexistent_role",
			filter:   pipeline.MessageFilter{SessionID: sess.ID, Role: "synthesist"},
			expected: 0,
		},
		{
			name:     "nonexistent_type",
			filter:   pipeline.MessageFilter{SessionID: sess.ID, MsgType: "feedback"},
			expected: 0,
		},
		{
			name:     "nonexistent_role_and_type",
			filter:   pipeline.MessageFilter{SessionID: sess.ID, Role: "synthesist", MsgType: "feedback"},
			expected: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msgs, err := s.GetMessages(ctx, tt.filter)
			require.NoError(t, err)
			assert.Len(t, msgs, tt.expected)
		})
	}
}

func TestStore_GetSession_NotFound(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	_, err := s.GetSession(context.Background(), "does-not-exist-uuid")
	require.Error(t, err)
	// store.GetSession wraps sql.ErrNoRows with %w; check the
	// sentinel via errors.Is rather than the wrapper prefix
	// substring. A reword of the prefix shouldn't fail this test.
	assert.ErrorIs(t, err, sql.ErrNoRows)
}

func TestStore_DeleteSession_Nonexistent(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	// Deleting a nonexistent session should not error — the DELETE
	// statements are no-ops and the transaction commits successfully.
	err := s.DeleteSession(context.Background(), "nonexistent-session-id")
	assert.NoError(t, err)
}

func TestStore_GetLatestMessage_EmptySession(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	sess, err := s.CreateSession(ctx, "repo:github/empty/latest", "")
	require.NoError(t, err)

	_, err = s.GetLatestMessage(ctx, pipeline.MessageFilter{SessionID: sess.ID})
	require.Error(t, err, "GetLatestMessage on empty session should error")
	// store.GetLatestMessage wraps sql.ErrNoRows with %w; check the
	// sentinel via errors.Is rather than the wrapper substring. A
	// reword of the wrapper shouldn't fail this test.
	assert.ErrorIs(t, err, sql.ErrNoRows)
}

func TestStore_CreateSession_EmptyMetadata(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	sess, err := s.CreateSession(ctx, "repo:github/no/metadata", "")
	require.NoError(t, err)

	got, err := s.GetSession(ctx, sess.ID)
	require.NoError(t, err)
	assert.Empty(t, got.Metadata)
}

func TestStore_Message_EmptyMetadata(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	ctx := context.Background()

	sess, err := s.CreateSession(ctx, "repo:github/msg/nometa", "")
	require.NoError(t, err)

	msg, err := s.DepositMessage(ctx, &pipeline.Message{
		SessionID: sess.ID,
		Role:      "security",
		MsgType:   "handoff",
		Content:   "test content",
		// Metadata deliberately empty
	})
	require.NoError(t, err)
	assert.Empty(t, msg.Metadata, "returned message preserves empty metadata")

	msgs, err := s.GetMessages(ctx, pipeline.MessageFilter{SessionID: sess.ID})
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	assert.Empty(t, msgs[0].Metadata, "round-tripped message preserves empty metadata")
}

// ---------------------------------------------------------------------------
// HTTP-level adversarial tests
// ---------------------------------------------------------------------------

func TestHTTP_LargeMessageContent(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	defer ts.Close()
	client := ts.Client()

	// Create session.
	resp, err := doPost(t, client, ts.URL+"/api/sessions",
		strings.NewReader(`{"target":"repo:github/large/http"}`))
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var sess pipeline.Session
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&sess))
	resp.Body.Close()

	// 1 MB content via HTTP. We construct valid JSON with an escaped
	// large string — using json.Marshal to avoid manual escaping issues.
	bigContent := strings.Repeat("X", 1024*1024)
	reqBody, err := json.Marshal(map[string]string{
		"role":     "security",
		"msg_type": "output",
		"content":  bigContent,
	})
	require.NoError(t, err)

	resp, err = doPost(t, client,
		ts.URL+"/api/sessions/"+sess.ID+"/messages",
		strings.NewReader(string(reqBody)))
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var msg pipeline.Message
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&msg))
	resp.Body.Close()
	assert.Equal(t, len(bigContent), len(msg.Content))
}

func TestHTTP_EmptySessionReturnsEmptyArray(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	defer ts.Close()
	client := ts.Client()

	// Create session, deposit nothing.
	resp, err := doPost(t, client, ts.URL+"/api/sessions",
		strings.NewReader(`{"target":"repo:github/empty/http"}`))
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var sess pipeline.Session
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&sess))
	resp.Body.Close()

	// GET messages should return [] not null.
	resp, err = doGet(t, client, ts.URL+"/api/sessions/"+sess.ID+"/messages")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.NoError(t, err)

	// The handler normalizes nil to []. Verify JSON is an empty array.
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))
	var msgs []pipeline.Message
	require.NoError(t, json.Unmarshal(body, &msgs))
	assert.Len(t, msgs, 0)
	// Additionally verify the raw JSON is literally an empty array, not null.
	trimmed := strings.TrimSpace(string(body))
	assert.Equal(t, "[]", trimmed, "expected JSON empty array, got: %s", trimmed)
}

func TestHTTP_ListSessionsEmptyReturnsEmptyArray(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	defer ts.Close()
	client := ts.Client()

	resp, err := doGet(t, client, ts.URL+"/api/sessions")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.NoError(t, err)
	trimmed := strings.TrimSpace(string(body))
	assert.Equal(t, "[]", trimmed, "empty session list should be [], got: %s", trimmed)
}

func TestHTTP_DuplicateTargetSessions(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	defer ts.Close()
	client := ts.Client()

	target := "repo:github/duplicate/target"
	createSession := func() pipeline.Session {
		resp, err := doPost(t, client, ts.URL+"/api/sessions",
			strings.NewReader(fmt.Sprintf(`{"target":%q}`, target)))
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, resp.StatusCode)
		var sess pipeline.Session
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&sess))
		resp.Body.Close()
		return sess
	}

	s1 := createSession()
	s2 := createSession()
	assert.NotEqual(t, s1.ID, s2.ID, "duplicate target should produce different session IDs")
	assert.Equal(t, target, s1.Target)
	assert.Equal(t, target, s2.Target)
}

func TestHTTP_MessageOrderingViaHTTP(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	defer ts.Close()
	client := ts.Client()

	resp, err := doPost(t, client, ts.URL+"/api/sessions",
		strings.NewReader(`{"target":"repo:github/order/http"}`))
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var sess pipeline.Session
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&sess))
	resp.Body.Close()

	contents := []string{"first", "second", "third", "fourth", "fifth"}
	for _, c := range contents {
		body, err := json.Marshal(map[string]string{
			"role":     "security",
			"msg_type": "status",
			"content":  c,
		})
		require.NoError(t, err)
		resp, err := doPost(t, client,
			ts.URL+"/api/sessions/"+sess.ID+"/messages",
			strings.NewReader(string(body)))
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, resp.StatusCode)
		resp.Body.Close()
	}

	resp, err = doGet(t, client, ts.URL+"/api/sessions/"+sess.ID+"/messages")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var msgs []pipeline.Message
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&msgs))
	resp.Body.Close()
	require.Len(t, msgs, len(contents))

	for i, want := range contents {
		assert.Equal(t, want, msgs[i].Content, "HTTP message %d out of order", i)
	}
}

func TestHTTP_MalformedJSON(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	defer ts.Close()
	client := ts.Client()

	tests := []struct {
		name   string
		path   string
		body   string
		expect int
	}{
		{
			name:   "truncated_json_session",
			path:   "/api/sessions",
			body:   `{"target": "repo:github/foo`,
			expect: http.StatusBadRequest,
		},
		{
			name:   "completely_invalid_json_session",
			path:   "/api/sessions",
			body:   `not json at all`,
			expect: http.StatusBadRequest,
		},
		{
			name:   "empty_body_session",
			path:   "/api/sessions",
			body:   ``,
			expect: http.StatusBadRequest,
		},
		{
			name:   "array_instead_of_object_session",
			path:   "/api/sessions",
			body:   `[{"target":"test"}]`,
			expect: http.StatusBadRequest,
		},
		{
			name:   "wrong_type_target_is_number",
			path:   "/api/sessions",
			body:   `{"target": 12345}`,
			expect: http.StatusBadRequest,
		},
		{
			name:   "null_target",
			path:   "/api/sessions",
			body:   `{"target": null}`,
			expect: http.StatusBadRequest,
		},
		{
			name:   "extra_fields_accepted",
			path:   "/api/sessions",
			body:   `{"target":"test","unknown_field":"ignored"}`,
			expect: http.StatusCreated, // extra fields are silently ignored
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := doPost(t, client, ts.URL+tt.path,
				strings.NewReader(tt.body))
			require.NoError(t, err)
			assert.Equal(t, tt.expect, resp.StatusCode, "body: %s", tt.body)
			resp.Body.Close()
		})
	}
}

func TestHTTP_MalformedJSON_DepositMessage(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	defer ts.Close()
	client := ts.Client()

	// Create a valid session first.
	resp, err := doPost(t, client, ts.URL+"/api/sessions",
		strings.NewReader(`{"target":"repo:github/malformed/deposit"}`))
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var sess pipeline.Session
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&sess))
	resp.Body.Close()

	msgURL := ts.URL + "/api/sessions/" + sess.ID + "/messages"

	tests := []struct {
		name   string
		body   string
		expect int
	}{
		{
			name:   "truncated_json",
			body:   `{"role":"security","msg_type":"handoff","content":"trun`,
			expect: http.StatusBadRequest,
		},
		{
			name:   "missing_role",
			body:   `{"msg_type":"handoff","content":"test"}`,
			expect: http.StatusBadRequest,
		},
		{
			name:   "missing_msg_type",
			body:   `{"role":"security","content":"test"}`,
			expect: http.StatusBadRequest,
		},
		{
			name:   "missing_content",
			body:   `{"role":"security","msg_type":"handoff"}`,
			expect: http.StatusBadRequest,
		},
		{
			name:   "empty_string_content",
			body:   `{"role":"security","msg_type":"handoff","content":""}`,
			expect: http.StatusBadRequest,
		},
		{
			name:   "content_is_number",
			body:   `{"role":"security","msg_type":"handoff","content":42}`,
			expect: http.StatusBadRequest,
		},
		{
			name:   "empty_body",
			body:   ``,
			expect: http.StatusBadRequest,
		},
		{
			name:   "null_body",
			body:   `null`,
			expect: http.StatusBadRequest,
		},
		{
			name:   "extra_fields_ignored",
			body:   `{"role":"security","msg_type":"handoff","content":"ok","extra":"ignored"}`,
			expect: http.StatusCreated,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := doPost(t, client, msgURL,
				strings.NewReader(tt.body))
			require.NoError(t, err)
			assert.Equal(t, tt.expect, resp.StatusCode, "body: %s", tt.body)
			resp.Body.Close()
		})
	}
}

func TestHTTP_UnicodeContentRoundTrip(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	defer ts.Close()
	client := ts.Client()

	resp, err := doPost(t, client, ts.URL+"/api/sessions",
		strings.NewReader(`{"target":"repo:github/unicode/http"}`))
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var sess pipeline.Session
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&sess))
	resp.Body.Close()

	msgURL := ts.URL + "/api/sessions/" + sess.ID + "/messages"

	tests := []struct {
		name    string
		content string
	}{
		{"emoji", "\U0001f512 Security lock \U0001f50d search"},
		{"cjk_characters", "\u4fe1\u983c\u5206\u6790\u5b8c\u4e86"},
		{"cyrillic", "\u0410\u043d\u0430\u043b\u0438\u0437 \u0431\u0435\u0437\u043e\u043f\u0430\u0441\u043d\u043e\u0441\u0442\u0438"},
		{"newlines_in_json", "line1\nline2\nline3"},
		{"tabs", "col1\tcol2"},
		{"backslash", `back\slash and "quotes"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Use json.Marshal for correct JSON escaping of content.
			body, err := json.Marshal(map[string]string{
				"role":     "security",
				"msg_type": "output",
				"content":  tt.content,
			})
			require.NoError(t, err)

			resp, err := doPost(t, client, msgURL,
				strings.NewReader(string(body)))
			require.NoError(t, err)
			require.Equal(t, http.StatusCreated, resp.StatusCode)
			var msg pipeline.Message
			require.NoError(t, json.NewDecoder(resp.Body).Decode(&msg))
			resp.Body.Close()
			assert.Equal(t, tt.content, msg.Content)

			// Verify round-trip via raw format.
			resp, err = doGet(t, client, msgURL+"/latest?role=security&type=output&format=raw")
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, resp.StatusCode)
			rawBody, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			require.NoError(t, err)
			assert.Equal(t, tt.content, string(rawBody))
		})
	}
}

func TestHTTP_MethodMismatches(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	defer ts.Close()
	client := ts.Client()

	tests := []struct {
		name   string
		method string
		path   string
	}{
		// Note: GET /api/sessions is valid (list handler), so no mismatch there.
		{"POST_on_get_session", http.MethodPost, "/api/sessions/some-id"},
		{"PUT_on_create_session", http.MethodPut, "/api/sessions"},
		{"PATCH_on_session", http.MethodPatch, "/api/sessions/some-id"},
		{"DELETE_on_messages", http.MethodDelete, "/api/sessions/some-id/messages"},
		{"PUT_on_messages", http.MethodPut, "/api/sessions/some-id/messages"},
		{"POST_on_latest", http.MethodPost, "/api/sessions/some-id/messages/latest"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := doRequest(t, client, tt.method, ts.URL+tt.path, nil)
			require.NoError(t, err)

			// Go 1.22+ method-scoped routing returns 405 Method Not Allowed
			// for registered paths with wrong methods. For unregistered
			// method+path combos, it returns 404.
			assert.True(t,
				resp.StatusCode == http.StatusMethodNotAllowed || resp.StatusCode == http.StatusNotFound,
				"expected 405 or 404, got %d for %s %s", resp.StatusCode, tt.method, tt.path)
			resp.Body.Close()
		})
	}
}

func TestHTTP_GetMessages_FilterCombinationsViaHTTP(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	defer ts.Close()
	client := ts.Client()

	// Create session and deposit a matrix of messages.
	resp, err := doPost(t, client, ts.URL+"/api/sessions",
		strings.NewReader(`{"target":"repo:github/filter/http"}`))
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var sess pipeline.Session
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&sess))
	resp.Body.Close()

	msgURL := ts.URL + "/api/sessions/" + sess.ID + "/messages"

	// Deposit: security-handoff, security-output, provenance-handoff, provenance-output.
	for _, role := range []string{"security", "provenance"} {
		for _, msgType := range []string{"handoff", "output"} {
			body, _ := json.Marshal(map[string]string{
				"role":     role,
				"msg_type": msgType,
				"content":  fmt.Sprintf("%s-%s-content", role, msgType),
			})
			resp, err := doPost(t, client, msgURL,
				strings.NewReader(string(body)))
			require.NoError(t, err)
			require.Equal(t, http.StatusCreated, resp.StatusCode)
			resp.Body.Close()
		}
	}

	tests := []struct {
		name     string
		query    string
		expected int
	}{
		{"no_filter", "", 4},
		{"role_only", "?role=security", 2},
		{"type_only", "?type=handoff", 2},
		{"role_and_type", "?role=security&type=handoff", 1},
		{"nonexistent_role", "?role=synthesist", 0},
		{"nonexistent_type", "?type=feedback", 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := doGet(t, client, msgURL+tt.query)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, resp.StatusCode)
			var msgs []pipeline.Message
			require.NoError(t, json.NewDecoder(resp.Body).Decode(&msgs))
			resp.Body.Close()
			assert.Len(t, msgs, tt.expected)
		})
	}
}

func TestHTTP_RawFormatMultipleMessages(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	defer ts.Close()
	client := ts.Client()

	resp, err := doPost(t, client, ts.URL+"/api/sessions",
		strings.NewReader(`{"target":"repo:github/raw/multi"}`))
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var sess pipeline.Session
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&sess))
	resp.Body.Close()

	msgURL := ts.URL + "/api/sessions/" + sess.ID + "/messages"

	// Deposit two messages with same role and type.
	for _, content := range []string{"first raw", "second raw"} {
		body, _ := json.Marshal(map[string]string{
			"role":     "security",
			"msg_type": "handoff",
			"content":  content,
		})
		resp, err := doPost(t, client, msgURL,
			strings.NewReader(string(body)))
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, resp.StatusCode)
		resp.Body.Close()
	}

	// Raw format with multiple matching messages should fall through
	// to JSON array (raw only applies when exactly 1 message matches).
	resp, err = doGet(t, client, msgURL+"?role=security&type=handoff&format=raw")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"),
		"multiple messages with format=raw should return JSON, not text")
	var msgs []pipeline.Message
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&msgs))
	resp.Body.Close()
	assert.Len(t, msgs, 2)
}

func TestHTTP_ConcurrentDepositsViaHTTP(t *testing.T) {
	t.Parallel()
	// Use pragma-equipped DB to match production configuration.
	// Without WAL + busy_timeout + MaxOpenConns=1, SQLite returns
	// SQLITE_BUSY under concurrent HTTP writes (see the no-pragma
	// store-level test for documentation of that failure mode).
	db := openTestDB(t)
	s, err := pipeline.OpenStore(context.Background(), db)
	require.NoError(t, err)
	srv := pipeline.NewServer(s, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := ts.Client()

	resp, err := doPost(t, client, ts.URL+"/api/sessions",
		strings.NewReader(`{"target":"repo:github/concurrent/http"}`))
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var sess pipeline.Session
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&sess))
	resp.Body.Close()

	msgURL := ts.URL + "/api/sessions/" + sess.ID + "/messages"

	const goroutines = 8
	const msgsPerGoroutine = 10

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(gID int) {
			defer wg.Done()
			for m := 0; m < msgsPerGoroutine; m++ {
				body, _ := json.Marshal(map[string]string{
					"role":     "security",
					"msg_type": "output",
					"content":  fmt.Sprintf("g%d-m%d", gID, m),
				})
				resp, err := doPost(t, client, msgURL,
					strings.NewReader(string(body)))
				assert.NoError(t, err)
				if resp != nil {
					assert.Equal(t, http.StatusCreated, resp.StatusCode)
					resp.Body.Close()
				}
			}
		}(g)
	}
	wg.Wait()

	// Verify all messages were deposited.
	resp, err = doGet(t, client, msgURL)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var msgs []pipeline.Message
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&msgs))
	resp.Body.Close()
	assert.Len(t, msgs, goroutines*msgsPerGoroutine)
}

// TestHTTP_DepositToNonexistentSession asserts the server's
// response when a deposit references a session that does not exist.
//
// Behavior chain: openTestDB → ConfigureDB → `PRAGMA foreign_keys=ON`,
// so the FK constraint declared in the schema IS enforced at runtime.
// On a missing parent row, SQLite returns SQLITE_CONSTRAINT_FOREIGNKEY,
// store.DepositMessage maps that to ErrSessionNotFound via
// isForeignKeyFailure(), and server.go's deposit handler renders that
// as 400 BadRequest with a body naming the missing session — the same
// shape used for other client-input errors on this endpoint (invalid
// role, missing content, etc.). See internal/pipeline/server.go,
// deposit handler, ~line 258.
//
// Earlier this test was log-only (`t.Logf` with no assertion) and its
// comment claimed FK enforcement was off. Both were stale — the
// pragma was added when ConfigureDB landed. A regression that mapped
// the FK failure to 500 instead of 400 (or 201, masking the missing
// parent) would have passed under the old shape.
func TestHTTP_DepositToNonexistentSession(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	defer ts.Close()
	client := ts.Client()

	body, _ := json.Marshal(map[string]string{
		"role":     "security",
		"msg_type": "handoff",
		"content":  "orphaned message",
	})
	resp, err := doPost(t, client,
		ts.URL+"/api/sessions/nonexistent-session-id/messages",
		strings.NewReader(string(body)))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equalf(t, http.StatusBadRequest, resp.StatusCode,
		"deposit to nonexistent session must return 400 (FK enforcement "+
			"on + ErrSessionNotFound mapping in server.go); got %d",
		resp.StatusCode)
}

// TestHTTP_DeleteSession_WhileGettingMessages exercises a delete
// running concurrently with a stream of message reads at the HTTP
// surface. HTTP-layer twin of TestRace_DepositDuringDelete and
// TestStore_DeleteSessionWhileDepositingRace.
//
// Race-detector reliance: per-call errors are tolerated (some
// reads will land after the delete and return 404 — acceptable).
// The validators are (a) no panic, (b) no DATA RACE under
// `go test -race` (see
// internal/invariants/TestRaceDetectorWiredIntoCI), and (c) the
// session is gone post-Wait — asserted below by GET-ing the
// session URL and asserting 404. Without (c), a regression where
// DELETE silently no-op'd while the read goroutines kept the
// connection busy would pass.
func TestHTTP_DeleteSession_WhileGettingMessages(t *testing.T) {
	t.Parallel()
	// Use pragma-equipped DB to avoid SQLITE_BUSY during concurrent ops.
	db := openTestDB(t)
	s, err := pipeline.OpenStore(context.Background(), db)
	require.NoError(t, err)
	srv := pipeline.NewServer(s, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := ts.Client()

	resp, err := doPost(t, client, ts.URL+"/api/sessions",
		strings.NewReader(`{"target":"repo:github/race/http"}`))
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var sess pipeline.Session
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&sess))
	resp.Body.Close()

	sessURL := ts.URL + "/api/sessions/" + sess.ID
	msgURL := sessURL + "/messages"

	// Deposit a few messages.
	for i := 0; i < 10; i++ {
		body, _ := json.Marshal(map[string]string{
			"role":     "security",
			"msg_type": "output",
			"content":  fmt.Sprintf("message-%d", i),
		})
		resp, err := doPost(t, client, msgURL,
			strings.NewReader(string(body)))
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, resp.StatusCode)
		resp.Body.Close()
	}

	// Concurrently read and delete. Neither should panic.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			resp, err := doGet(t, client, msgURL)
			if err == nil {
				resp.Body.Close()
			}
		}
	}()
	go func() {
		defer wg.Done()
		resp, err := doRequest(t, client, http.MethodDelete, sessURL, nil)
		if err == nil {
			resp.Body.Close()
		}
	}()
	wg.Wait()

	// End-state: the delete goroutine has returned, so the delete
	// committed. GET on the session URL must now return 404.
	finalResp, err := doGet(t, client, sessURL)
	require.NoError(t, err)
	defer finalResp.Body.Close()
	assert.Equalf(t, http.StatusNotFound, finalResp.StatusCode,
		"session must be gone after the delete goroutine finished; "+
			"got %d (expected 404)", finalResp.StatusCode)
}

// TestHTTP_GetSessionNotFoundVsDeleteNotFound verifies the difference in
// behavior: GET returns 404 for missing session, DELETE returns 204 (no-op).
func TestHTTP_GetSessionNotFoundVsDeleteNotFound(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	defer ts.Close()
	client := ts.Client()

	// GET missing session = 404
	resp, err := doGet(t, client, ts.URL+"/api/sessions/nonexistent")
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	resp.Body.Close()

	// DELETE missing session = 204 (the delete queries are no-ops)
	resp, err = doRequest(t, client, http.MethodDelete, ts.URL+"/api/sessions/nonexistent", nil)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNoContent, resp.StatusCode)
	resp.Body.Close()
}

// TestHTTP_ResponseBodiesAreJSONSafe asserts that both the deposit
// response body AND the subsequent list response body are always
// valid JSON documents with NO raw ASCII control characters
// (U+0000–U+001F) outside of the tolerated whitespace set (\t \n \r)
// anywhere in the string — including inside string-typed fields.
//
// Why this invariant is load-bearing: bash orchestrators pipe
// server responses through `jq` to extract fields like `.id`.
// jq rejects any JSON string whose content has unescaped control
// characters (per RFC 8259 §7). If the server ever emits raw
// newlines or tabs inside a JSON string field — e.g., by string-
// concatenating response bodies instead of using encoding/json —
// downstream `jq` pipelines break and the orchestrator has to
// retry via python3 or manual parsing. The /analyze skill's
// pipeline deposit flow relies on this invariant.
//
// Payload shape: handoff-like content packed with every character
// class that has historically caused problems for shell + JSON
// pipelines — newlines, tabs, carriage returns, backticks,
// backslashes, double quotes, dollar signs, unicode non-ASCII,
// and every control-char byte in the U+0000–U+001F range
// (the ones JSON requires escaping for).
func TestHTTP_ResponseBodiesAreJSONSafe(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	defer ts.Close()
	client := ts.Client()

	// Create session.
	resp, err := doPost(t, client, ts.URL+"/api/sessions",
		strings.NewReader(`{"target":"repo:github/json/probe"}`))
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var sess pipeline.Session
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&sess))
	resp.Body.Close()

	// Build content that historically stressed downstream JSON
	// consumers. Each character class maps to a real shell-pipeline
	// failure we've seen:
	//
	//   - "\n" literal in a handoff body: jq parse error at line N
	//   - backticks / $ in a handoff body: shell re-expansion bugs
	//   - backslash + quote: double-escape confusion
	//   - ASCII control chars 0x00-0x1F: RFC 8259 §7 violation
	//     when unescaped inside a string
	var controlChars strings.Builder
	for b := byte(0x00); b <= 0x1f; b++ {
		controlChars.WriteByte(b)
	}
	content := strings.Join([]string{
		"# Handoff with every annoying character class",
		"line with `backticks` and $VAR and \"quotes\"",
		"tabbed\tcontent\there",
		"line\r\nwith CRLF",
		"backslash\\escape\\path",
		"unicode: résumé, Ω, 日本語, 🦀",
		"control chars below:",
		controlChars.String(),
	}, "\n")

	// Deposit via HTTP — json.Marshal gets the outbound escape right.
	reqBody, err := json.Marshal(map[string]string{
		"role":     "security",
		"msg_type": "handoff",
		"content":  content,
	})
	require.NoError(t, err)

	depositResp, err := doPost(t, client,
		ts.URL+"/api/sessions/"+sess.ID+"/messages",
		strings.NewReader(string(reqBody)))
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, depositResp.StatusCode)
	depositBody, err := io.ReadAll(depositResp.Body)
	require.NoError(t, err)
	depositResp.Body.Close()

	// Invariant 1: the deposit response body must be valid JSON.
	// If encoding/json.NewEncoder is being used server-side (correct),
	// this always holds. If someone switches to Fprintf-with-raw-string
	// formatting, this will start failing.
	assertResponseBodyJSONSafe(t, "deposit response", depositBody)

	// Spot-check the roundtrip: decode the deposit body and confirm
	// Content survived verbatim — including every annoying byte.
	var depositMsg pipeline.Message
	require.NoError(t, json.Unmarshal(depositBody, &depositMsg))
	assert.Equal(t, content, depositMsg.Content,
		"deposit response .content must round-trip every input byte")

	// Invariant 2: the list response body must also be valid JSON,
	// same control-char rule. Deposits go through the store, so
	// this is a second chance for a bad serializer to slip in.
	listResp, err := doGet(t, client, ts.URL+"/api/sessions/"+sess.ID+"/messages")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, listResp.StatusCode)
	listBody, err := io.ReadAll(listResp.Body)
	require.NoError(t, err)
	listResp.Body.Close()

	assertResponseBodyJSONSafe(t, "list response", listBody)

	// Roundtrip through the list response too — same content,
	// same bytes.
	var listed []pipeline.Message
	require.NoError(t, json.Unmarshal(listBody, &listed))
	require.Len(t, listed, 1, "list should return the one deposit")
	assert.Equal(t, content, listed[0].Content,
		"list response message.content must round-trip every input byte")

	// Invariant 3: raw-format access (used by analyst WebFetch
	// retrievals) returns the CONTENT verbatim — not a JSON
	// document. The content bytes may contain control chars
	// (that's the point of raw format), but the response body
	// itself isn't required to be JSON.
	rawResp, err := doGet(t, client,
		ts.URL+"/api/sessions/"+sess.ID+"/messages?role=security&type=handoff&format=raw")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rawResp.StatusCode)
	rawBody, err := io.ReadAll(rawResp.Body)
	require.NoError(t, err)
	rawResp.Body.Close()

	assert.Equal(t, content, string(rawBody),
		"raw format must return content bytes verbatim without JSON wrapping")
}

// assertResponseBodyJSONSafe asserts that body is valid JSON AND
// contains no raw ASCII control characters outside the tolerated
// whitespace set (\t \n \r) anywhere in its bytes — including
// inside JSON string fields.
//
// The byte scan is the load-bearing check, NOT json.Unmarshal.
// Go's encoding/json is lenient on decode: it accepts raw control
// characters (U+0000–U+001F) inside JSON string values without
// error, even though RFC 8259 §7 requires them to be escaped. A
// server emitting a raw 0x01 byte inside a string field would
// produce a body that Unmarshal accepts but `jq` rejects.
//
// Both checks stay because they cover different failure modes:
//
//   - json.Unmarshal catches structural breakage (malformed
//     envelopes, bad escape sequences, truncated values).
//   - The byte scan catches RFC 8259 §7 violations the lenient
//     decoder ignores.
//
// Do NOT remove the byte scan as "redundant with Unmarshal". It
// is not redundant — it's the actual RFC 8259 guard for the
// downstream `jq` pipeline failure mode this invariant exists to
// prevent.
//
// Helper kept local to this test since it's an assertion idiom
// specific to the JSON-safety invariant rather than a general-
// purpose check.
func assertResponseBodyJSONSafe(t *testing.T, label string, body []byte) {
	t.Helper()

	// Structural check: catches malformed envelopes and bad
	// escape sequences. Does NOT catch raw control bytes in
	// string fields (Go's decoder is lenient on those, per
	// encoding/json's documented behavior).
	var parsed any
	require.NoError(t, json.Unmarshal(body, &parsed),
		"%s must be valid JSON", label)

	// RFC 8259 §7 guard: only \t \n \r tolerated below 0x20.
	// This is the check downstream `jq` consumers care about — a
	// raw 0x01 inside a string field would be silently accepted
	// by encoding/json but would break the orchestrator pipeline.
	for i, b := range body {
		if b < 0x20 && b != '\t' && b != '\n' && b != '\r' {
			t.Fatalf("%s: raw control byte 0x%02x at offset %d; server must emit JSON-escaped form",
				label, b, i)
		}
	}
}

// TestHTTP_ContentTypeJSON verifies all JSON responses have correct Content-Type.
func TestHTTP_ContentTypeJSON(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	defer ts.Close()
	client := ts.Client()

	// List sessions (empty).
	resp, err := doGet(t, client, ts.URL+"/api/sessions")
	require.NoError(t, err)
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))
	resp.Body.Close()

	// Create session.
	resp, err = doPost(t, client, ts.URL+"/api/sessions",
		strings.NewReader(`{"target":"repo:github/content-type/test"}`))
	require.NoError(t, err)
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))
	var sess pipeline.Session
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&sess))
	resp.Body.Close()

	// Get session.
	resp, err = doGet(t, client, ts.URL+"/api/sessions/"+sess.ID)
	require.NoError(t, err)
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))
	resp.Body.Close()

	// Error response should also be JSON.
	resp, err = doPost(t, client, ts.URL+"/api/sessions",
		strings.NewReader(`{}`))
	require.NoError(t, err)
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))
	resp.Body.Close()
}

// TestHTTP_NewServerNilLogger verifies the server handles a nil logger
// without panicking (it falls back to slog.Default).
func TestHTTP_NewServerNilLogger(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	s, err := pipeline.OpenStore(context.Background(), db)
	require.NoError(t, err)

	// Should not panic.
	srv := pipeline.NewServer(s, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Verify it works.
	resp, err := doGet(t, ts.Client(), ts.URL+"/api/sessions")
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
}
