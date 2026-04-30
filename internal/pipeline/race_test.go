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
//
// Race-detector reliance: this test deliberately swallows per-call
// errors because either outcome (deposit succeeds, deposit FKs out)
// is acceptable. The validators are (a) no panic, (b) no DATA RACE
// reported by `go test -race`, and (c) the session is gone
// post-Wait. (a) is implicit; (b) is only checked when -race is on
// (see internal/invariants/TestRaceDetectorWiredIntoCI which guards
// that flag in CI); (c) is asserted below.
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

	// End-state: regardless of which goroutine won which write, the
	// delete eventually committed and the session must be gone.
	// Without this assertion, a regression where DeleteSession
	// silently no-op'd while the deposit goroutine kept the
	// connection busy would still pass under -race.
	_, getErr := s.GetSession(ctx, sess.ID)
	assert.Error(t, getErr, "session should be deleted after the delete goroutine finished")
}

// TestRace_ConcurrentDeletes verifies that two goroutines deleting
// the same session simultaneously do not cause a panic or data race.
//
// Race-detector reliance: the concurrent deletes can interleave in
// many orders; the test swallows per-call errors because all
// outcomes (one wins, both no-op the second time, etc.) are
// acceptable. The validators are (a) no panic, (b) no DATA RACE
// under `go test -race` (see
// internal/invariants/TestRaceDetectorWiredIntoCI), and (c) the
// session is gone post-Wait. (c) is asserted below.
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
// concurrent create, deposit, read, and list operations to surface
// data races in the handler layer.
//
// Race-detector reliance plus end-state assertions. Earlier this
// test ended at `wg.Wait()` and asserted nothing about outcomes —
// "Success = no panic, no data race detected by -race." That made
// `-race` the *only* validator: a regression that lost half the
// concurrent writes (or returned duplicate session IDs) would have
// passed. This version mirrors the discipline of
// TestStore_ConcurrentDeposits in adversarial_test.go and adds:
//
//  1. Per-request status-code checks for read and list paths
//     (previously only the err return was inspected).
//  2. After Wait: GetMessages on the shared session must return
//     exactly `workers` messages, all with unique IDs (lost-write
//     and duplicate-ID smoke).
//  3. After Wait: every concurrent CreateSession call must have
//     produced a unique non-empty session ID (uniqueness smoke
//     for the create path).
//
// `-race` (asserted by internal/invariants/TestRaceDetectorWiredIntoCI)
// remains the validator for memory-unsafe sharing in handler state
// — the new assertions catch lost-write and duplicate-ID
// regressions that don't manifest as a memory race.
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
	resp, err := doPost(t, client, ts.URL+"/api/sessions",
		strings.NewReader(`{"target":"repo:github/race/http"}`))
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var sess pipeline.Session
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&sess))
	resp.Body.Close()

	const workers = 10
	var wg sync.WaitGroup

	// Channel buffered to workers — every successful CreateSession
	// goroutine ships its returned ID for post-Wait uniqueness
	// checks. Buffer size matches the producer count so sends never
	// block (a blocked send would deadlock under -race's stricter
	// scheduling).
	createdIDs := make(chan string, workers)

	// Concurrent deposits. Use valid roles from the server's enum.
	roles := []string{"security", "provenance", "synthesist", "orchestrator"}
	for i := range workers {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			role := roles[n%len(roles)]
			body := fmt.Sprintf(
				`{"role":%q,"msg_type":"output","content":"http message %d"}`, role, n)
			r, postErr := doPost(t, client,
				ts.URL+"/api/sessions/"+sess.ID+"/messages",
				strings.NewReader(body))
			if postErr != nil {
				t.Errorf("deposit %d: %v", n, postErr)
				return
			}
			defer r.Body.Close()
			if r.StatusCode != http.StatusCreated {
				t.Errorf("deposit %d: expected 201, got %d", n, r.StatusCode)
			}
		}(i)
	}

	// Concurrent reads.
	for i := range workers {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			r, getErr := doGet(t, client,
				ts.URL+"/api/sessions/"+sess.ID+"/messages")
			if getErr != nil {
				t.Errorf("read %d: %v", n, getErr)
				return
			}
			defer r.Body.Close()
			if r.StatusCode != http.StatusOK {
				t.Errorf("read %d: expected 200, got %d", n, r.StatusCode)
			}
		}(i)
	}

	// Concurrent session listings.
	for i := range workers {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			r, listErr := doGet(t, client, ts.URL+"/api/sessions")
			if listErr != nil {
				t.Errorf("list %d: %v", n, listErr)
				return
			}
			defer r.Body.Close()
			if r.StatusCode != http.StatusOK {
				t.Errorf("list %d: expected 200, got %d", n, r.StatusCode)
			}
		}(i)
	}

	// Concurrent session creations.
	for i := range workers {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			body := fmt.Sprintf(`{"target":"repo:github/race/http-concurrent-%d"}`, n)
			r, createErr := doPost(t, client,
				ts.URL+"/api/sessions",
				strings.NewReader(body))
			if createErr != nil {
				t.Errorf("create %d: %v", n, createErr)
				return
			}
			defer r.Body.Close()
			if r.StatusCode != http.StatusCreated {
				t.Errorf("create %d: expected 201, got %d", n, r.StatusCode)
				return
			}
			var created pipeline.Session
			if decErr := json.NewDecoder(r.Body).Decode(&created); decErr != nil {
				t.Errorf("create %d: decode response: %v", n, decErr)
				return
			}
			createdIDs <- created.ID
		}(i)
	}

	wg.Wait()
	close(createdIDs)

	// End-state assertion 1: every concurrent deposit landed in the
	// shared session exactly once. A regression where the handler
	// dropped writes under contention (e.g., a connection-pool
	// deadlock that returned 201 without committing) would surface
	// here as a Len mismatch even when -race finds no memory race.
	// Reads through the same Store the server is using — the HTTP
	// surface adds nothing for this assertion and a direct read is
	// faster.
	msgs, err := s.GetMessages(context.Background(), pipeline.MessageFilter{SessionID: sess.ID})
	require.NoError(t, err)
	assert.Lenf(t, msgs, workers,
		"expected all %d concurrent HTTP deposits to persist (lost-write smoke)", workers)
	seenMsgID := make(map[int64]bool, len(msgs))
	for _, m := range msgs {
		assert.Falsef(t, seenMsgID[m.ID], "duplicate message ID under contention: %d", m.ID)
		seenMsgID[m.ID] = true
	}

	// End-state assertion 2: every concurrent CreateSession returned
	// a unique non-empty session ID. Catches a regression where two
	// concurrent creates collided on a UUID, or where the handler
	// returned an empty/zero ID under contention.
	seenSessID := make(map[string]bool)
	for id := range createdIDs {
		assert.NotEmpty(t, id, "create returned an empty session ID under contention")
		assert.Falsef(t, seenSessID[id], "duplicate session ID under contention: %s", id)
		seenSessID[id] = true
	}
	assert.Lenf(t, seenSessID, workers,
		"expected %d unique session IDs from concurrent CreateSession calls", workers)
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
//
// Race-detector reliance: the test swallows per-call errors because
// status updates and deposits can interleave in any order — any
// outcome is acceptable so long as no goroutine panics and no
// memory race is reported. The validators are (a) no panic,
// (b) no DATA RACE under `go test -race` (see
// internal/invariants/TestRaceDetectorWiredIntoCI), and (c) the
// session's final status is one of the values an update goroutine
// wrote (asserted below).
//
// (c) is load-bearing only if the asserted set EXCLUDES the
// session's creation default ("active"). An earlier version
// included "active" in the set, which made the assertion vacuous:
// if every UpdateSessionStatus call silently failed to commit,
// status would remain "active" from creation, "active" was in the
// set, and the assertion passed under the exact regression it was
// written to catch. The current set ("complete" / "failed" only)
// can only be satisfied if at least one update committed —
// otherwise the assertion fails on the lingering creation default.
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

	// Update status concurrently. Deliberately exclude "active"
	// (the creation default) — see end-state assertion below for
	// why that exclusion is load-bearing.
	statuses := []string{"complete", "failed", "complete", "failed", "complete"}
	for _, status := range statuses {
		wg.Add(1)
		go func(st string) {
			defer wg.Done()
			_ = s.UpdateSessionStatus(ctx, sess.ID, st)
		}(status)
	}

	wg.Wait()

	// End-state: the session's final status must be one of the
	// values an update goroutine wrote. Because "active" is the
	// creation default and is NOT in `statuses`, this assertion
	// fails if every update silently no-op'd (status would still
	// be "active", which isn't in the set). That's the regression
	// the assertion exists to catch: a torn write or a bug where
	// UpdateSessionStatus reported success without committing.
	got, err := s.GetSession(ctx, sess.ID)
	require.NoError(t, err)
	assert.Containsf(t, statuses, got.Status,
		"session status %q is not in the set of values updates wrote: %v "+
			"(if status is still %q, every update silently failed to commit)",
		got.Status, statuses, "active")
}
