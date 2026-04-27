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
//	dogfood-metrics serve                      start the OTLP/HTTP receiver
//	dogfood-metrics hook --event <name>        process a Claude Code hook event from stdin
//	dogfood-metrics report <session-id>        render per-session markdown report
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
	case "hook":
		runHookCmd(os.Args[2:])
	case "report":
		runReportCmd(os.Args[2:])
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
	fmt.Fprintln(os.Stderr, "  dogfood-metrics hook  --event <name> [-out-dir dogfood-metrics/raw]")
	fmt.Fprintln(os.Stderr, "  dogfood-metrics report [-in-dir dogfood-metrics/raw] [-out-dir dogfood-metrics/sessions] <session-id>")
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

// runHookCmd is the entry point for the `hook` subcommand. Invoked
// per Claude Code hook event (PreToolUse, PostToolUse, etc.). Reads
// the event JSON from stdin, classifies the tool call, appends a
// JSONL line to the per-session file in -out-dir.
//
// CRITICAL: this MUST exit 0 even on internal failure. Per round-3
// verification of the dogfood-telemetry plan, exit code 2 from a
// PreToolUse hook BLOCKS the tool call. Observe-only telemetry
// must never block — even a write failure or malformed input gets
// recorded as a side note and exits 0.
func runHookCmd(args []string) {
	fs := flag.NewFlagSet("hook", flag.ExitOnError)
	outDir := fs.String("out-dir", "dogfood-metrics/raw", "directory for per-session hook JSONL files")
	event := fs.String("event", "PreToolUse", "hook event name (PreToolUse, PostToolUse, ...)")
	if err := fs.Parse(args); err != nil {
		// flag.ExitOnError already exited, but defense in depth.
		os.Exit(0)
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		// Log to stderr (Claude Code may surface it to the user)
		// but still exit 0 — a missing dogfood dir must not block
		// real work.
		fmt.Fprintf(os.Stderr, "dogfood-hook: mkdir %s: %v\n", *outDir, err)
		os.Exit(0)
	}
	if err := runHook(os.Stdin, *outDir, *event, time.Now()); err != nil {
		fmt.Fprintf(os.Stderr, "dogfood-hook: %v\n", err)
	}
	os.Exit(0)
}

// runReportCmd is the entry point for the `report` subcommand.
// Reads `<in-dir>/traces.jsonl` plus `<in-dir>/hooks-<id>.jsonl`,
// joins on session_id, writes the rendered markdown to
// `<out-dir>/<id>/report.md`. Errors (missing session,
// filesystem failures) surface as exit 1.
func runReportCmd(args []string) {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	inDir := fs.String("in-dir", "dogfood-metrics/raw", "directory containing traces.jsonl and hooks-<session-id>.jsonl")
	outDir := fs.String("out-dir", "dogfood-metrics/sessions", "directory to write per-session reports under")
	if err := fs.Parse(args); err != nil {
		log.Fatalf("parse flags: %v", err)
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "report: exactly one positional argument required (session id)")
		fmt.Fprintln(os.Stderr, "Usage: dogfood-metrics report <session-id> [-in-dir <dir>] [-out-dir <dir>]")
		os.Exit(2)
	}
	sessionID := fs.Arg(0)
	if err := runReport(sessionID, *inDir, *outDir); err != nil {
		log.Fatalf("report: %v", err)
	}
	fmt.Fprintf(os.Stderr, "report: wrote %s/%s/report.md\n", *outDir, sessionID)
}
