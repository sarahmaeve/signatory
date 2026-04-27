// Command dogfood-metrics is signatory's local OTLP/HTTP/JSON
// receiver for /analyze dogfood telemetry. It listens for OTEL
// traces and logs from Claude Code, writes them to disk in
// OTLP-JSON format, and (in slice 3, planned) generates per-session
// reports correlating the OTEL stream with PreToolUse hook events.
//
// See design/agent-otel.md for the architecture, the verification
// rounds that informed it, and the rationale for writing this
// ourselves rather than adopting otelcol-contrib.
//
// Subcommands:
//
//	dogfood-metrics serve          start the OTLP/HTTP receiver
//	dogfood-metrics report <id>    generate per-session report (slice 3)
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		runServe(os.Args[2:])
	case "report":
		// Slice 3 territory; surface a clear deferral message rather
		// than a generic "unknown command" so callers running ahead
		// of the slice timeline get an immediately-useful answer.
		fmt.Fprintln(os.Stderr, "report: not implemented yet — see design/agent-otel.md slice 3")
		os.Exit(2)
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "dogfood-metrics: signatory's OTLP/HTTP receiver for /analyze telemetry")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "Usage:")
	fmt.Fprintln(os.Stderr, "  dogfood-metrics serve [-addr :4318] [-out-dir dogfood-metrics/raw]")
	fmt.Fprintln(os.Stderr, "  dogfood-metrics report <session-id>     (not implemented yet)")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "See design/agent-otel.md for architecture.")
}

func runServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", ":4318", "HTTP listen address")
	outDir := fs.String("out-dir", "dogfood-metrics/raw", "directory for raw OTLP-JSON files")
	if err := fs.Parse(args); err != nil {
		log.Fatalf("parse flags: %v", err)
	}

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		log.Fatalf("mkdir %s: %v", *outDir, err)
	}

	rcv := &Receiver{OutDir: *outDir}
	log.Printf("dogfood-metrics: listening on %s, writing to %s", *addr, *outDir)
	srv := &http.Server{
		Addr:    *addr,
		Handler: rcv,
		// Modest read-header timeout — OTLP/HTTP requests are small
		// JSON bodies that complete quickly. A longer timeout would
		// let a slow-loris client tie up handler goroutines waiting
		// for headers that never finish.
		ReadHeaderTimeout: 10 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("listen: %v", err)
	}
}
