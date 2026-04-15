package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/sarahmaeve/signatory/internal/mcp"
	"github.com/sarahmaeve/signatory/internal/mcp/resources"
	"github.com/sarahmaeve/signatory/internal/mcp/tools"
	"github.com/sarahmaeve/signatory/internal/store"
)

// MCPCmd serves signatory as a Model Context Protocol server over stdio.
//
// Transport: newline-delimited JSON-RPC 2.0 on stdin/stdout per the MCP
// 2025-11-25 specification. All logging MUST go to stderr to avoid
// corrupting the protocol stream on stdout.
//
// Lifecycle: the server runs until stdin signals EOF (host process
// closed the pipe — normal shutdown for a stdio MCP server), SIGINT /
// SIGTERM is delivered, or an unrecoverable transport error occurs.
type MCPCmd struct{}

// Run starts the MCP server. It wires the read-only Phase 1 tool and
// resource surface to a shared store, then blocks on Serve until the
// transport closes or a termination signal is received.
//
// The version stamped into handshake.serverInfo and into every
// Response.Metadata.ServerVersion comes from the process-wide `version`
// variable set by the build (ldflags) or "dev" in unpinned builds —
// the same value `signatory version` prints.
func (cmd *MCPCmd) Run(globals *Globals) error {
	// Parent context: cancelled on SIGINT/SIGTERM so an in-flight
	// dispatch that honours ctx can abort cleanly. Also threaded into
	// OpenSQLite so startup cancellation aborts migrations.
	ctx, cancel := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Open the concrete SQLite store. Two resources (PostureResource,
	// UnexaminedResource) need *store.SQLite directly — they call DB()
	// for aggregation queries that don't fit the Store interface. The
	// rest take the interface, which *SQLite satisfies.
	dbPath, err := store.ResolvePath(globals.DBPath)
	if err != nil {
		return fmt.Errorf("resolve database path: %w", err)
	}
	s, err := store.OpenSQLite(ctx, dbPath)
	if err != nil {
		return fmt.Errorf("open signatory store: %w", err)
	}
	defer func() { _ = s.Close() }()

	// Construct the server with the build-injected version. An empty
	// version would default to "0.0.0-unset" inside NewServer, so we
	// forward whatever main.go has (dev builds pass "dev").
	srv := mcp.NewServer(version)

	// Register read-only resources (Phase 1).
	srv.RegisterResource(&resources.ConfigResource{
		DBPath:  dbPath,
		Version: version,
	})
	srv.RegisterResource(&resources.SignalTypesResource{})
	srv.RegisterResource(&resources.BurnsResource{Store: s})
	srv.RegisterResource(&resources.PostureResource{Store: s})
	srv.RegisterResource(&resources.UnexaminedResource{Store: s})
	srv.RegisterResource(&resources.AnalysesResource{Store: s})

	// Register read-only tools (Phase 1). Tools that mutate state (burn,
	// posture, ingest, survey-with-write) land in a later phase after
	// the confirmation-metadata path and audit logging are wired in.
	srv.Register(&tools.AnalyzeTool{Store: s})
	srv.Register(&tools.DetailTool{Store: s})
	srv.Register(&tools.SignalsTool{Store: s})
	srv.Register(&tools.ShowAnalysesTool{Store: s})
	srv.Register(&tools.ShowFindingsTool{Store: s})
	srv.Register(&tools.ShowMethodologyTool{Store: s})
	// SurveyTool is a pure read-only dispatcher in Phase 1: it wraps
	// AnalyzeTool across a project's dep tree. No store field required.
	srv.Register(&tools.SurveyTool{})

	// Diagnostic line on stderr so a human watching the session knows
	// the server is up. stdout is reserved for protocol traffic.
	fmt.Fprintf(os.Stderr,
		"signatory mcp: listening on stdio (version=%s, db=%s)\n",
		version, dbPath)

	// Serve blocks until EOF, ctx cancellation, or unrecoverable I/O.
	// A nil return means the client closed stdin cleanly.
	if err := srv.Serve(ctx, os.Stdin, os.Stdout); err != nil {
		// ctx.Err() on signal-driven shutdown is expected, not fatal.
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("mcp serve: %w", err)
	}
	return nil
}
