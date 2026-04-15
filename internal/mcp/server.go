// Package mcp: Server struct — the top-level MCP server.
//
// Usage:
//
//	srv := mcp.NewServer()
//	srv.Register(myTool)
//	srv.RegisterResource(myResource)
//	if err := srv.Serve(ctx, os.Stdin, os.Stdout); err != nil {
//	    log.Fatal(err)
//	}
//
// Serve reads newline-delimited JSON-RPC 2.0 messages from r, dispatches
// them, and writes responses to w. It returns when:
//   - The reader signals EOF (client closed stdin — normal shutdown for stdio)
//   - ctx is cancelled (caller-initiated shutdown)
//   - An unrecoverable read/write error occurs
//
// Goroutine model: Serve runs a single read loop on the calling goroutine
// and dispatches each request to a handler goroutine. A WaitGroup tracks
// in-flight handlers so Close / context cancellation can drain them
// before returning. Responses are serialized through a mutex-guarded
// writer to prevent interleaved output.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
)

// Server is the MCP protocol server. Zero value is not usable; create
// with NewServer.
type Server struct {
	// version is the server version surfaced in the initialize
	// handshake's serverInfo.version and in Response.Metadata.ServerVersion.
	// Injected at construction so production stamps the real build
	// version (via debug.ReadBuildInfo in main) and tests inject their
	// own fixed value. This is a struct field, not a package-level var,
	// to avoid the race-under-t.Parallel hazard the HandoffCmd refactor
	// established as the house pattern.
	version string

	mu          sync.RWMutex
	tools       map[string]Tool
	toolSchemas map[string]*inputSchema // parsed schema per tool
	resources   map[string]Resource

	shake handshake

	// writeMu guards all writes to the codec's underlying writer.
	// Separate from mu so handler goroutines can write without holding
	// the registry read lock.
	writeMu sync.Mutex

	// wg tracks in-flight handler goroutines for graceful drain.
	wg sync.WaitGroup
}

// NewServer creates a ready-to-use Server with the given version
// string. The version is echoed in the initialize serverInfo and
// in every Response.Metadata.ServerVersion.
func NewServer(version string) *Server {
	if version == "" {
		version = "0.0.0-unset"
	}
	return &Server{
		version:     version,
		tools:       make(map[string]Tool),
		toolSchemas: make(map[string]*inputSchema),
		resources:   make(map[string]Resource),
		shake:       handshake{version: version},
	}
}

// Version returns the version this Server was constructed with.
// Primarily for testing and for handler code paths that need to stamp
// the same version into custom responses.
func (s *Server) Version() string {
	return s.version
}

// Register adds a Tool to the server's registry. The tool's InputSchema
// is parsed and cached; if parsing fails or the schema doesn't declare
// additionalProperties:false, Register panics — schema validity is a
// programmer error discovered at startup, not a runtime error. The
// strict-reject posture is a documented invariant of the Tool contract
// (see interfaces.go); enforcing it at Register time prevents a new
// tool author from silently producing a permissive schema that slips
// unknown fields through validation.
// Register is not safe to call after Serve has started.
func (s *Server) Register(t Tool) {
	schema, err := parseInputSchema(t.InputSchema())
	if err != nil {
		panic("mcp: invalid InputSchema for tool " + t.Name() + ": " + err.Error())
	}
	if !schema.strictReject {
		panic("mcp: tool " + t.Name() +
			" InputSchema must set additionalProperties:false (strict-reject is required by the Tool contract)")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tools[t.Name()] = t
	s.toolSchemas[t.Name()] = schema
}

// RegisterResource adds a Resource to the server's registry.
// Not safe to call after Serve has started.
func (s *Server) RegisterResource(r Resource) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resources[r.URIPattern()] = r
}

// ClientInfo returns the clientInfo captured during the initialize
// handshake, for use in audit logging. Returns a zero value before
// the handshake completes.
func (s *Server) ClientInfo() clientInfo {
	return s.shake.ClientInfo()
}

// Serve reads JSON-RPC messages from r, dispatches them, and writes
// responses to w until r signals EOF or ctx is cancelled.
//
// On ctx cancellation: new incoming requests are rejected with a
// generic internal error; in-flight handlers are given a derived
// context that is also cancelled, allowing them to abort. Serve waits
// for all in-flight handlers to finish before returning.
//
// Serve returns nil on clean EOF, ctx.Err() on cancellation, and an
// I/O error on unrecoverable transport failure.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	c := newCodec(r, w)

	for {
		// Check context before blocking on read.
		select {
		case <-ctx.Done():
			s.wg.Wait()
			return ctx.Err()
		default:
		}

		req, err := c.readRequest()
		if err != nil {
			if errors.Is(err, io.EOF) {
				// Clean shutdown: client closed stdin.
				s.wg.Wait()
				return nil
			}
			// Parse or framing error — send error response and continue.
			// We may not have a valid id; send null id per spec.
			var rpcErr *rpcError
			if errors.As(err, &rpcErr) {
				s.writeMu.Lock()
				_ = c.writeError(json.RawMessage("null"), rpcErr.Code, rpcErr.Message, nil)
				s.writeMu.Unlock()
				continue
			}
			// Unrecoverable transport error.
			s.wg.Wait()
			return err
		}

		// Notifications: dispatch synchronously (no response) and continue.
		if req.isNotification() {
			_, _ = s.dispatch(ctx, req)
			continue
		}

		// initialize is a lifecycle gate per MCP 2025-11-25: every
		// subsequent request depends on the state transition it
		// performs. We dispatch it synchronously in the read loop so
		// that by the time we read the next frame, the state is
		// guaranteed visible to its handler. Async dispatch here would
		// let a pipelining client see "not initialized" errors arrive
		// before the initialize success — correct per spec but
		// confusing, and it loses the notifications/initialized state
		// transition if that notification beats the initialize
		// goroutine. initialize is fast and one-shot; there is no
		// parallelism to preserve.
		if req.Method == "initialize" {
			result, rpcErr := s.dispatch(ctx, req)
			s.writeMu.Lock()
			if rpcErr != nil {
				_ = c.writeError(req.ID, rpcErr.Code, rpcErr.Message, rpcErr.Data)
			} else if result != nil {
				_ = c.writeResult(req.ID, result)
			}
			s.writeMu.Unlock()
			continue
		}

		// Requests: dispatch in a goroutine so the read loop stays
		// responsive. The goroutine inherits ctx so cancellation
		// propagates to the handler.
		s.wg.Add(1)
		go func(req *rpcRequest) {
			defer s.wg.Done()
			result, rpcErr := s.dispatch(ctx, req)

			s.writeMu.Lock()
			defer s.writeMu.Unlock()

			if rpcErr != nil {
				_ = c.writeError(req.ID, rpcErr.Code, rpcErr.Message, rpcErr.Data)
				return
			}
			if result != nil {
				_ = c.writeResult(req.ID, result)
			}
			// result == nil AND rpcErr == nil → no response (notification
			// handled as a request? shouldn't happen, but safe to skip).
		}(req)
	}
}

// Close signals in-flight handlers to stop and waits for them to drain.
// After Close, Serve will return on its next iteration. Close is
// intended for test teardown; in production the ctx cancellation path
// is sufficient.
func (s *Server) Close() {
	s.wg.Wait()
}
