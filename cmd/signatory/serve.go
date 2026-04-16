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

// ServeCmd starts the pipeline message service — a local HTTPS server
// that orchestrators and analyst agents use to exchange handoffs,
// output, and feedback without /tmp files or context-window pressure.
//
// The server binds to 127.0.0.1 only. TLS is required for agent
// access: Claude Code's WebFetch tool forces HTTPS and rejects
// self-signed certs, so a locally-trusted cert (e.g., from mkcert)
// must be provided. One-time setup:
//
//	brew install mkcert
//	mkcert -install
//	mkdir -p ~/.signatory/certs
//	cd ~/.signatory/certs && mkcert 127.0.0.1 localhost
//	# Add to shell profile:
//	export NODE_EXTRA_CA_CERTS="$(mkcert -CAROOT)/rootCA.pem"
//
// Without TLS (plain HTTP, for debugging only via curl), pass
// --no-tls. Agents cannot reach the plain HTTP variant.
type ServeCmd struct {
	Port    int    `help:"Port to listen on." default:"21517"`
	TLSCert string `help:"Path to TLS certificate (PEM)." default:"~/.signatory/certs/127.0.0.1+1.pem" type:"path"`
	TLSKey  string `help:"Path to TLS private key (PEM)." default:"~/.signatory/certs/127.0.0.1+1-key.pem" type:"path"`
	NoTLS   bool   `help:"Serve plain HTTP instead of HTTPS. Debugging only — agents cannot reach plain HTTP." name:"no-tls"`
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

	certFile := cmd.TLSCert
	keyFile := cmd.TLSKey
	if cmd.NoTLS {
		certFile = ""
		keyFile = ""
		fmt.Fprintln(os.Stderr, "# WARNING: --no-tls serves plain HTTP; agents cannot reach this service via WebFetch.")
	} else {
		// Verify cert files exist before starting.
		if _, err := os.Stat(certFile); err != nil {
			return fmt.Errorf("TLS cert not found at %s: run `mkcert 127.0.0.1 localhost` in %s (see `signatory serve --help`)",
				certFile, "~/.signatory/certs/")
		}
		if _, err := os.Stat(keyFile); err != nil {
			return fmt.Errorf("TLS key not found at %s", keyFile)
		}
	}

	srv := pipeline.NewServer(ps, logger)
	return srv.ListenAndServe(ctx, cmd.Port, certFile, keyFile)
}
