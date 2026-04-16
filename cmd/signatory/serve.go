package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/sarahmaeve/signatory/internal/pipeline"
	"github.com/sarahmaeve/signatory/internal/store"
)

// ServeCmd starts the pipeline message service — a local HTTP server
// that orchestrators and analyst agents use to exchange handoffs,
// output, and feedback without /tmp files or context-window pressure.
//
// The server binds to 127.0.0.1 only (localhost). It uses the same
// SQLite database as the main signatory store, adding its own
// pipeline_sessions and pipeline_messages tables.
type ServeCmd struct {
	Port int `help:"Port to listen on." default:"21517"`
}

func (cmd *ServeCmd) Run(globals *Globals) error {
	ctx, cancel := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer cancel()

	dbPath, err := store.ResolvePath(globals.DBPath)
	if err != nil {
		return fmt.Errorf("resolve database path: %w", err)
	}
	s, err := store.OpenSQLite(ctx, dbPath)
	if err != nil {
		return fmt.Errorf("open signatory store: %w", err)
	}
	defer func() { _ = s.Close() }()

	ps, err := pipeline.OpenStore(ctx, s.DB())
	if err != nil {
		return fmt.Errorf("open pipeline store: %w", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	srv := pipeline.NewServer(ps, logger)
	return srv.ListenAndServe(ctx, cmd.Port)
}
