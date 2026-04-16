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
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/pipeline"

	_ "modernc.org/sqlite"
)

// ---------------------------------------------------------------------------
// 1. MaxBytesReader enforcement
// ---------------------------------------------------------------------------

func TestHardening_MaxBytesReader_SessionEndpoint(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	defer ts.Close()

	// 4 KB limit for session creation. Build a body that exceeds it.
	bigMetadata := strings.Repeat("x", 5*1024) // 5 KB of metadata
	body := fmt.Sprintf(`{"target":"test","metadata":"%s"}`, bigMetadata)

	resp, err := doPost(t, ts.Client(), ts.URL+"/api/sessions",
		strings.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.NotEqual(t, http.StatusCreated, resp.StatusCode,
		"oversized session body should not succeed")
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHardening_MaxBytesReader_MessageEndpoint(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	defer ts.Close()
	client := ts.Client()

	// Create a session first.
	resp, err := doPost(t, client, ts.URL+"/api/sessions",
		strings.NewReader(`{"target":"test"}`))
	require.NoError(t, err)
	var sess pipeline.Session
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&sess))
	resp.Body.Close()

	// 10 MB limit for messages. Build a body exceeding it.
	bigContent := strings.Repeat("z", 11*1024*1024) // 11 MB
	body := fmt.Sprintf(`{"role":"security","msg_type":"handoff","content":"%s"}`, bigContent)

	resp, err = doPost(t, client,
		ts.URL+"/api/sessions/"+sess.ID+"/messages",
		strings.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.NotEqual(t, http.StatusCreated, resp.StatusCode,
		"oversized message body should not succeed")
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

// ---------------------------------------------------------------------------
// 2. Error sanitization — no internal details leak to clients
// ---------------------------------------------------------------------------

func TestHardening_ErrorSanitization(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	defer ts.Close()
	client := ts.Client()

	// Deposit a message to a nonexistent session. With FK enforcement on,
	// this triggers a FOREIGN KEY constraint error inside SQLite. The
	// HTTP response must not leak internal details.
	body := `{"role":"security","msg_type":"handoff","content":"trigger FK violation"}`
	resp, err := doPost(t, client,
		ts.URL+"/api/sessions/nonexistent-session-id/messages",
		strings.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	bodyStr := string(respBody)

	// Must be a 500 (internal error), not a 201 or 400.
	assert.Equal(t, http.StatusInternalServerError, resp.StatusCode)

	// Must NOT leak SQLite internals.
	forbiddenTokens := []string{
		"sqlite", "SQLITE",
		"FOREIGN KEY", "foreign key",
		"constraint",
		".db", ".sqlite",
		"goroutine", "runtime.",
		"/Users/", "/home/", "/tmp/",
	}
	for _, tok := range forbiddenTokens {
		assert.NotContains(t, bodyStr, tok,
			"response body must not contain %q", tok)
	}

	// Must contain the generic error message.
	assert.Contains(t, bodyStr, "internal error")
}

// ---------------------------------------------------------------------------
// 3. Session count limit
// ---------------------------------------------------------------------------

func TestHardening_SessionCountLimit(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	defer ts.Close()
	client := ts.Client()

	// Helper to create a session and return its ID and status code.
	createSession := func(idx int) (string, int) {
		body := fmt.Sprintf(`{"target":"target-%d"}`, idx)
		resp, err := doPost(t, client, ts.URL+"/api/sessions",
			strings.NewReader(body))
		require.NoError(t, err)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			return "", resp.StatusCode
		}
		var sess pipeline.Session
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&sess))
		return sess.ID, resp.StatusCode
	}

	// Fill up to the limit (100 sessions).
	ids := make([]string, 0, 100)
	for i := range 100 {
		id, status := createSession(i)
		require.Equalf(t, http.StatusCreated, status,
			"session %d should succeed (under limit)", i)
		ids = append(ids, id)
	}

	// Next creation should be rejected with 503.
	_, status := createSession(100)
	assert.Equal(t, http.StatusServiceUnavailable, status,
		"session 101 should be rejected (at limit)")

	// Delete one session.
	resp, err := doRequest(t, client, http.MethodDelete,
		ts.URL+"/api/sessions/"+ids[0], nil)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusNoContent, resp.StatusCode)

	// Now creation should work again.
	_, status = createSession(101)
	assert.Equal(t, http.StatusCreated, status,
		"session creation should succeed after deleting one")
}

// ---------------------------------------------------------------------------
// 4. Enum validation
// ---------------------------------------------------------------------------

func TestHardening_EnumValidation_InvalidRole(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	defer ts.Close()
	client := ts.Client()

	sess := createTestSession(t, client, ts.URL)

	resp, err := doPost(t, client,
		ts.URL+"/api/sessions/"+sess+"/messages",
		strings.NewReader(`{"role":"hacker","msg_type":"handoff","content":"x"}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHardening_EnumValidation_InvalidMsgType(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	defer ts.Close()
	client := ts.Client()

	sess := createTestSession(t, client, ts.URL)

	resp, err := doPost(t, client,
		ts.URL+"/api/sessions/"+sess+"/messages",
		strings.NewReader(`{"role":"security","msg_type":"exploit","content":"x"}`))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestHardening_EnumValidation_AllValidValues(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	defer ts.Close()
	client := ts.Client()

	sess := createTestSession(t, client, ts.URL)

	roles := []string{"security", "provenance", "synthesist", "orchestrator"}
	msgTypes := []string{"handoff", "output", "feedback", "template", "status"}

	for _, role := range roles {
		for _, mt := range msgTypes {
			t.Run(role+"/"+mt, func(t *testing.T) {
				body := fmt.Sprintf(
					`{"role":%q,"msg_type":%q,"content":"test %s %s"}`,
					role, mt, role, mt)
				resp, err := doPost(t, client,
					ts.URL+"/api/sessions/"+sess+"/messages",
					strings.NewReader(body))
				require.NoError(t, err)
				defer resp.Body.Close()
				assert.Equal(t, http.StatusCreated, resp.StatusCode,
					"valid combo %s/%s should succeed", role, mt)
			})
		}
	}
}

// ---------------------------------------------------------------------------
// 5. errors.Is paths — proper 404 for missing resources
// ---------------------------------------------------------------------------

func TestHardening_NotFound_NonexistentSession(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	defer ts.Close()

	resp, err := doGet(t, ts.Client(), ts.URL+"/api/sessions/does-not-exist")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusNotFound, resp.StatusCode,
		"nonexistent session should return 404, not 500")
}

func TestHardening_NotFound_LatestMessageEmptySession(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	defer ts.Close()
	client := ts.Client()

	sess := createTestSession(t, client, ts.URL)

	resp, err := doGet(t, client, ts.URL+"/api/sessions/"+sess+"/messages/latest")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusNotFound, resp.StatusCode,
		"latest message on empty session should return 404, not 500")
}

// ---------------------------------------------------------------------------
// 6. ConfigureDB — concurrent safety
// ---------------------------------------------------------------------------

func TestHardening_ConfigureDB_ConcurrentDeposits(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// Helper: open a store with or without ConfigureDB, run N concurrent
	// deposits, return the count of successes and failures.
	runConcurrent := func(t *testing.T, configure bool) (successes, failures int64) {
		t.Helper()
		dbPath := filepath.Join(t.TempDir(), "test.db")
		db, err := sql.Open("sqlite", dbPath)
		require.NoError(t, err)
		t.Cleanup(func() { db.Close() })

		if configure {
			require.NoError(t, pipeline.ConfigureDB(ctx, db))
		}

		store, err := pipeline.OpenStore(ctx, db)
		require.NoError(t, err)

		sess, err := store.CreateSession(ctx, "test-concurrent", "")
		require.NoError(t, err)

		const workers = 10
		var sOK, sFail atomic.Int64
		var wg sync.WaitGroup
		wg.Add(workers)
		for i := range workers {
			go func(i int) {
				defer wg.Done()
				_, err := store.DepositMessage(ctx, &pipeline.Message{
					SessionID: sess.ID,
					Role:      "security",
					MsgType:   "handoff",
					Content:   fmt.Sprintf("concurrent msg %d", i),
				})
				if err != nil {
					sFail.Add(1)
				} else {
					sOK.Add(1)
				}
			}(i)
		}
		wg.Wait()
		return sOK.Load(), sFail.Load()
	}

	t.Run("without_ConfigureDB", func(t *testing.T) {
		t.Parallel()
		_, failures := runConcurrent(t, false)
		// Without ConfigureDB we expect some failures due to database-is-locked
		// errors (multiple connections, no WAL, no busy timeout). If all succeed
		// that's also fine — the important thing is the "with" case always works.
		t.Logf("without ConfigureDB: failures=%d", failures)
	})

	t.Run("with_ConfigureDB", func(t *testing.T) {
		t.Parallel()
		successes, failures := runConcurrent(t, true)
		assert.Equal(t, int64(10), successes,
			"all deposits should succeed with ConfigureDB")
		assert.Equal(t, int64(0), failures,
			"no deposit should fail with ConfigureDB")
	})
}

// ---------------------------------------------------------------------------
// 7. Large content round-trip (5 MB)
// ---------------------------------------------------------------------------

func TestHardening_LargeContentRoundTrip(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	defer ts.Close()
	client := ts.Client()

	sess := createTestSession(t, client, ts.URL)

	// Build a 5 MB content string.
	largeContent := strings.Repeat("A", 5*1024*1024)

	body, err := json.Marshal(map[string]string{
		"role":     "security",
		"msg_type": "output",
		"content":  largeContent,
	})
	require.NoError(t, err)

	resp, err := doPost(t, client,
		ts.URL+"/api/sessions/"+sess+"/messages",
		strings.NewReader(string(body)))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	// Retrieve via raw format and verify exact round-trip.
	resp2, err := doGet(t, client,
		ts.URL+"/api/sessions/"+sess+"/messages/latest?role=security&type=output&format=raw")
	require.NoError(t, err)
	defer resp2.Body.Close()
	require.Equal(t, http.StatusOK, resp2.StatusCode)

	got, err := io.ReadAll(resp2.Body)
	require.NoError(t, err)
	assert.Equal(t, len(largeContent), len(string(got)),
		"round-tripped content length must match")
	assert.Equal(t, largeContent, string(got),
		"round-tripped content must match exactly")
}

// ---------------------------------------------------------------------------
// 8. Unicode content — emoji, CJK, null bytes
// ---------------------------------------------------------------------------

func TestHardening_UnicodeContentRoundTrip(t *testing.T) {
	t.Parallel()
	ts := newTestServer(t)
	defer ts.Close()
	client := ts.Client()

	sess := createTestSession(t, client, ts.URL)

	testCases := []struct {
		name    string
		content string
	}{
		{"emoji", "Hello 🔒🛡️🚀 security analysis complete ✅"},
		{"CJK", "安全分析完了。依存関係は信頼できます。软件供应链"},
		{"mixed_scripts", "Ελληνικά العربية हिन्दी 日本語 🎉"},
		{"null_bytes", "before\x00after\x00end"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(map[string]string{
				"role":     "synthesist",
				"msg_type": "output",
				"content":  tc.content,
			})
			require.NoError(t, err)

			resp, err := doPost(t, client,
				ts.URL+"/api/sessions/"+sess+"/messages",
				strings.NewReader(string(body)))
			require.NoError(t, err)
			defer resp.Body.Close()
			require.Equal(t, http.StatusCreated, resp.StatusCode,
				"deposit of %s content should succeed", tc.name)

			// Retrieve via JSON (latest) and verify round-trip.
			var deposited pipeline.Message
			require.NoError(t, json.NewDecoder(resp.Body).Decode(&deposited))
			assert.Equal(t, tc.content, deposited.Content,
				"deposited content should match for %s", tc.name)

			// Also verify via GET latest (JSON decode).
			resp2, err := doGet(t, client,
				ts.URL+"/api/sessions/"+sess+
					"/messages/latest?role=synthesist&type=output")
			require.NoError(t, err)
			defer resp2.Body.Close()
			require.Equal(t, http.StatusOK, resp2.StatusCode)

			var latest pipeline.Message
			require.NoError(t, json.NewDecoder(resp2.Body).Decode(&latest))
			assert.Equal(t, tc.content, latest.Content,
				"round-tripped content must match for %s", tc.name)
		})
	}
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// createTestSession creates a session and returns its ID.
func createTestSession(t *testing.T, client *http.Client, baseURL string) string {
	t.Helper()
	resp, err := doPost(t, client, baseURL+"/api/sessions",
		strings.NewReader(`{"target":"hardening-test"}`))
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var sess pipeline.Session
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&sess))
	resp.Body.Close()
	return sess.ID
}

// newTestServerWithDB is like newTestServer but also returns the underlying
// httptest.Server for tests that need the handler directly.
func newTestServerWithHandler(t *testing.T) (*httptest.Server, http.Handler) {
	t.Helper()
	db := openTestDB(t)
	s, err := pipeline.OpenStore(context.Background(), db)
	require.NoError(t, err)
	srv := pipeline.NewServer(s, nil)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, srv.Handler()
}
