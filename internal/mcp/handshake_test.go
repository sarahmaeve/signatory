package mcp

// Race-detection tests for the handshake type. These are designed
// to fail under `go test -race ./internal/mcp` before the fix lands,
// and to pass cleanly afterwards.
//
// The race, precisely: handshake.state and handshake.client are
// written from the Serve read loop (initialize + notifications/
// initialized are dispatched synchronously there) and read from
// request-handler goroutines (tools/list, tools/call, resources/list,
// resources/read spawn goroutines that call isOperational). No
// mutex, atomic, or channel sits between the writers and readers.
//
// Go's memory model gives `go func()` a happens-before edge from
// "code up to spawn" → "goroutine start" only. It gives no edge
// between a LATER main-loop write and the spawned goroutine's
// reads. So the race is structural — the happens-before graph is
// missing an edge — and the detector flags it every run regardless
// of wall-clock interleaving.
//
// Two levels of test here: one unit-focused (TestHandshake_*) that
// directly exercises the type without Serve's goroutine model,
// and one integration-level test in server_test.go that exercises
// the race through the production-shaped pipeline.

import (
	"runtime"
	"sync/atomic"
	"testing"
)

// TestHandshake_StateAccessIsRaceFree spawns a reader goroutine
// that hammers isOperational() while the main test goroutine
// transitions the lifecycle via handleInitialize →
// handleInitializedNotification. Under -race the detector fires
// on the unsynchronized handshake.state field.
//
// Revert proof: delete the mutex from the handshake struct (or
// downgrade the Lock/Unlock to no-ops). This test then races
// under `go test -race ./internal/mcp` with a report naming
// handshake.state and the two methods.
//
// Note on t.Parallel: deliberately omitted. Race tests are more
// meaningful when they run without concurrent unrelated activity
// in the same test binary; any race surfaced is unambiguously
// from this test's own code.
func TestHandshake_StateAccessIsRaceFree(t *testing.T) {
	h := &handshake{version: "test"}

	var stop atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		for !stop.Load() {
			_ = h.isOperational()
		}
	}()

	// Writer: main goroutine performs the two lifecycle transitions.
	// Each is a single state write; together they cover both
	// transition points.
	if _, err := h.handleInitialize(nil); err != nil {
		t.Fatalf("handleInitialize: %v", err)
	}
	// Yield so the reader goroutine has a real chance to run
	// between the two writes, widening the window for the race
	// detector. The detector fires on happens-before violations
	// regardless of actual scheduling, but a yield is cheap
	// insurance against skipped goroutine scheduling on a busy
	// runtime.
	runtime.Gosched()
	h.handleInitializedNotification()
	runtime.Gosched()

	stop.Store(true)
	<-done
}

// TestHandshake_ClientInfoIsRaceFree covers the parallel race on
// handshake.client. Today ClientInfo() is only called from tests,
// but the field is documented as "exposed for audit logging"
// (handshake.go:41) — as soon as a handler goroutine stamps
// "source: mcp/<name>/<version>" on an audit record, it races
// with the handleInitialize write. Closing the race now, while
// we have a mutex in hand, is cheaper than fixing it a second
// time when audit wiring lands.
//
// Revert proof: remove the read-lock from ClientInfo(); under
// -race the detector fires on handshake.client.
func TestHandshake_ClientInfoIsRaceFree(t *testing.T) {
	h := &handshake{version: "test"}

	var stop atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		for !stop.Load() {
			_ = h.ClientInfo()
		}
	}()

	// Writer: handleInitialize writes h.client along with h.state.
	params := []byte(`{"clientInfo":{"name":"race-client","version":"9.9.9"}}`)
	if _, err := h.handleInitialize(params); err != nil {
		t.Fatalf("handleInitialize: %v", err)
	}
	runtime.Gosched()

	stop.Store(true)
	<-done
}
