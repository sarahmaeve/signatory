package pipeline_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/pipeline"
)

// TestRace_ConcurrentDeposits verifies that many goroutines can deposit
// messages into the same session without data races or lost writes.
func TestRace_ConcurrentDeposits(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	s, err := pipeline.OpenStore(context.Background(), db)
	require.NoError(t, err)

	ctx := context.Background()
	sess, err := s.CreateSession(ctx, "repo:github/race/deposits", "")
	require.NoError(t, err)

	const workers = 20
	// Use valid roles from the server's enum. Store-level doesn't
	// enforce the enum, but keeping test data realistic avoids
	// masking issues if validation is ever added at the store layer.
	storeRoles := []string{"security", "provenance", "synthesist", "orchestrator"}
	var wg sync.WaitGroup
	errs := make(chan error, workers)

	for i := range workers {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_, depositErr := s.DepositMessage(ctx, &pipeline.Message{
				SessionID: sess.ID,
				Role:      storeRoles[n%len(storeRoles)],
				MsgType:   "output",
				Content:   fmt.Sprintf("message from worker %d", n),
			})
			if depositErr != nil {
				errs <- depositErr
			}
		}(i)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("deposit error: %v", err)
	}

	// Verify all messages arrived.
	msgs, err := s.GetMessages(ctx, pipeline.MessageFilter{SessionID: sess.ID})
	require.NoError(t, err)
	assert.Len(t, msgs, workers, "expected all %d deposits to persist", workers)
}

// TestRace_ConcurrentSessionCreation verifies that creating many
// sessions concurrently does not cause UUID collisions or write errors.
func TestRace_ConcurrentSessionCreation(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	s, err := pipeline.OpenStore(context.Background(), db)
	require.NoError(t, err)

	ctx := context.Background()
	const workers = 20
	var wg sync.WaitGroup
	sessions := make(chan *pipeline.Session, workers)
	errs := make(chan error, workers)

	for i := range workers {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			sess, createErr := s.CreateSession(ctx, fmt.Sprintf("repo:github/race/sess-%d", n), "")
			if createErr != nil {
				errs <- createErr
				return
			}
			sessions <- sess
		}(i)
	}
	wg.Wait()
	close(sessions)
	close(errs)

	for err := range errs {
		t.Errorf("create session error: %v", err)
	}

	// Verify all sessions exist and have unique IDs.
	seen := make(map[string]bool)
	for sess := range sessions {
		if seen[sess.ID] {
			t.Errorf("duplicate session ID: %s", sess.ID)
		}
		seen[sess.ID] = true
	}
	assert.Len(t, seen, workers)
}

// TestRace_DepositDuringDelete verifies that depositing a message
// while the session is being deleted does not panic or corrupt state.
// The deposit should either succeed (if it runs before the delete)
// or fail with a foreign key or "no such session" error.
func TestRace_DepositDuringDelete(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	s, err := pipeline.OpenStore(context.Background(), db)
	require.NoError(t, err)

	ctx := context.Background()
	sess, err := s.CreateSession(ctx, "repo:github/race/deposit-delete", "")
	require.NoError(t, err)

	// Seed some messages so delete has work to do.
	for i := range 5 {
		_, err := s.DepositMessage(ctx, &pipeline.Message{
			SessionID: sess.ID,
			Role:      "security",
			MsgType:   "handoff",
			Content:   fmt.Sprintf("seed message %d", i),
		})
		require.NoError(t, err)
	}

	var wg sync.WaitGroup

	// Goroutine 1: delete the session.
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = s.DeleteSession(ctx, sess.ID)
	}()

	// Goroutine 2: try to deposit messages concurrently.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range 10 {
			_, _ = s.DepositMessage(ctx, &pipeline.Message{
				SessionID: sess.ID,
				Role:      "provenance",
				MsgType:   "output",
				Content:   fmt.Sprintf("concurrent deposit %d", i),
			})
		}
	}()

	wg.Wait()
	// Success = no panic, no data race detected by -race.
}

// TestRace_ConcurrentDeletes verifies that two goroutines deleting
// the same session simultaneously do not cause a panic or data race.
func TestRace_ConcurrentDeletes(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	s, err := pipeline.OpenStore(context.Background(), db)
	require.NoError(t, err)

	ctx := context.Background()
	sess, err := s.CreateSession(ctx, "repo:github/race/double-delete", "")
	require.NoError(t, err)

	// Seed some messages.
	for i := range 3 {
		_, err := s.DepositMessage(ctx, &pipeline.Message{
			SessionID: sess.ID,
			Role:      "security",
			MsgType:   "handoff",
			Content:   fmt.Sprintf("seed %d", i),
		})
		require.NoError(t, err)
	}

	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.DeleteSession(ctx, sess.ID)
		}()
	}
	wg.Wait()

	// Session should be gone regardless of which delete won.
	_, err = s.GetSession(ctx, sess.ID)
	assert.Error(t, err, "session should be deleted")
}

// TestRace_HTTPConcurrentOperations hammers the HTTP server with
// concurrent create, deposit, read, and delete operations to surface
// any data races in the handler layer.
func TestRace_HTTPConcurrentOperations(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	s, err := pipeline.OpenStore(context.Background(), db)
	require.NoError(t, err)

	srv := pipeline.NewServer(s, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	client := ts.Client()

	// Create a shared session for concurrent operations.
	resp, err := client.Post(ts.URL+"/api/sessions",
		"application/json",
		strings.NewReader(`{"target":"repo:github/race/http"}`))
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var sess pipeline.Session
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&sess))
	resp.Body.Close()

	const workers = 10
	var wg sync.WaitGroup

	// Concurrent deposits. Use valid roles from the server's enum.
	roles := []string{"security", "provenance", "synthesist", "orchestrator"}
	for i := range workers {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			role := roles[n%len(roles)]
			body := fmt.Sprintf(
				`{"role":%q,"msg_type":"output","content":"http message %d"}`, role, n)
			r, postErr := client.Post(
				ts.URL+"/api/sessions/"+sess.ID+"/messages",
				"application/json",
				strings.NewReader(body))
			if postErr != nil {
				t.Errorf("deposit %d: %v", n, postErr)
				return
			}
			if r.StatusCode != http.StatusCreated {
				t.Errorf("deposit %d: expected 201, got %d", n, r.StatusCode)
			}
			r.Body.Close()
		}(i)
	}

	// Concurrent reads.
	for i := range workers {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			r, getErr := client.Get(
				ts.URL + "/api/sessions/" + sess.ID + "/messages")
			if getErr != nil {
				t.Errorf("read %d: %v", n, getErr)
				return
			}
			r.Body.Close()
		}(i)
	}

	// Concurrent session listings.
	for i := range workers {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			r, listErr := client.Get(ts.URL + "/api/sessions")
			if listErr != nil {
				t.Errorf("list %d: %v", n, listErr)
				return
			}
			r.Body.Close()
		}(i)
	}

	// Concurrent session creations.
	for i := range workers {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"target":"repo:github/race/http-concurrent-%d"}`, n)
			r, createErr := client.Post(
				ts.URL+"/api/sessions",
				"application/json",
				strings.NewReader(body))
			if createErr != nil {
				t.Errorf("create %d: %v", n, createErr)
				return
			}
			r.Body.Close()
		}(i)
	}

	wg.Wait()
}

// TestRace_ReadsDuringWrite verifies that concurrent reads and writes
// to the same session do not produce data races. A reader might see
// a partial set of messages (before or after a write commits), but
// the data must be consistent — no torn reads.
func TestRace_ReadsDuringWrite(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	s, err := pipeline.OpenStore(context.Background(), db)
	require.NoError(t, err)

	ctx := context.Background()
	sess, err := s.CreateSession(ctx, "repo:github/race/read-write", "")
	require.NoError(t, err)

	const writers = 10
	const readers = 10
	var wg sync.WaitGroup

	// Writers.
	for i := range writers {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_, _ = s.DepositMessage(ctx, &pipeline.Message{
				SessionID: sess.ID,
				Role:      "security",
				MsgType:   "output",
				Content:   fmt.Sprintf("write %d", n),
			})
		}(i)
	}

	// Readers.
	for range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			msgs, readErr := s.GetMessages(ctx, pipeline.MessageFilter{SessionID: sess.ID})
			if readErr != nil {
				t.Errorf("read error: %v", readErr)
				return
			}
			// Each message must be internally consistent.
			for _, m := range msgs {
				if m.SessionID != sess.ID {
					t.Errorf("torn read: message session_id=%q, expected=%q", m.SessionID, sess.ID)
				}
			}
		}()
	}

	wg.Wait()
}

// TestRace_UpdateStatusDuringDeposit verifies that updating session
// status while messages are being deposited does not race.
func TestRace_UpdateStatusDuringDeposit(t *testing.T) {
	t.Parallel()
	db := openTestDB(t)
	s, err := pipeline.OpenStore(context.Background(), db)
	require.NoError(t, err)

	ctx := context.Background()
	sess, err := s.CreateSession(ctx, "repo:github/race/status-deposit", "")
	require.NoError(t, err)

	var wg sync.WaitGroup

	// Deposit messages.
	for i := range 10 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_, _ = s.DepositMessage(ctx, &pipeline.Message{
				SessionID: sess.ID,
				Role:      "security",
				MsgType:   "output",
				Content:   fmt.Sprintf("msg %d", n),
			})
		}(i)
	}

	// Update status concurrently.
	statuses := []string{"active", "complete", "failed", "active", "complete"}
	for _, status := range statuses {
		wg.Add(1)
		go func(st string) {
			defer wg.Done()
			_ = s.UpdateSessionStatus(ctx, sess.ID, st)
		}(status)
	}

	wg.Wait()
}
