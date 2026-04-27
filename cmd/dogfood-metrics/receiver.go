package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

// maxBodyBytes bounds incoming OTLP-JSON request bodies. Realistic
// /v1/traces and /v1/logs payloads from Claude Code are kilobytes
// to low MB; 16 MiB is generous slack and a hard cap to prevent
// OOM from a misbehaving or malicious sender.
const maxBodyBytes = 16 * 1024 * 1024

// Receiver implements net/http.Handler for OTLP/HTTP/JSON ingest.
// It accepts POST /v1/traces and POST /v1/logs, decompresses gzip
// when present, validates each body is well-formed JSON, and
// appends one compacted JSON line per request to traces.jsonl or
// logs.jsonl in OutDir. Returns 202 Accepted on success per the
// OTLP/HTTP spec.
//
// Why we wrote this instead of adopting otelcol-contrib: see
// design/agent-otel.md "Architectural decision: write our own
// collector." Short version — vetting the upstream collector +
// dep tree is out of proportion to what we need (a small HTTP
// server that records OTLP-JSON to disk).
//
// Concurrency: a single Receiver may be shared across many
// goroutines (the http.Server dispatches each request in its own
// goroutine). File appends are sequenced via OS-level append-only
// open semantics — each write is atomic up to the OS write-buffer
// boundary, which exceeds any realistic OTLP-JSON line. Two
// concurrent writers don't interleave bytes within a line.
type Receiver struct {
	OutDir string
}

// ServeHTTP routes by URL path to the appropriate signal file.
// Unknown paths return 404; non-POST methods return 405 with the
// Allow header populated.
func (r *Receiver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	var filename string
	switch req.URL.Path {
	case "/v1/traces":
		filename = "traces.jsonl"
	case "/v1/logs":
		filename = "logs.jsonl"
	default:
		http.NotFound(w, req)
		return
	}

	if req.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := readBody(req)
	if err != nil {
		http.Error(w, fmt.Sprintf("read body: %v", err), http.StatusBadRequest)
		return
	}

	// Validate + compact in one pass. json.Compact rejects malformed
	// JSON (returning the parse error) and produces a single-line
	// representation with no whitespace — preserving the JSONL
	// invariant that every line is exactly one JSON value.
	var compact bytes.Buffer
	if err := json.Compact(&compact, body); err != nil {
		http.Error(w, fmt.Sprintf("invalid JSON: %v", err), http.StatusBadRequest)
		return
	}

	if err := appendLine(filepath.Join(r.OutDir, filename), compact.Bytes()); err != nil {
		// 500-class because this is an internal failure (filesystem
		// problem), not a client-fixable input issue.
		http.Error(w, fmt.Sprintf("write: %v", err), http.StatusInternalServerError)
		return
	}

	// OTLP/HTTP success status per spec.
	w.WriteHeader(http.StatusAccepted)
}

// readBody reads up to maxBodyBytes from req.Body, transparently
// decompressing if Content-Encoding: gzip is set. Round-4
// verification of the dogfood-telemetry plan flagged that Claude
// Code's compression behavior is undocumented, so the receiver
// handles gzip defensively per request rather than assuming a
// fixed encoding.
func readBody(req *http.Request) ([]byte, error) {
	var src io.Reader = req.Body
	if req.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(req.Body)
		if err != nil {
			return nil, fmt.Errorf("gzip reader: %w", err)
		}
		defer gz.Close() //nolint:errcheck // close on read-only reader; err is not actionable
		src = gz
	}
	// Bound the body so a misbehaving sender streaming an
	// unbounded payload can't exhaust memory. LimitReader caps the
	// number of bytes io.ReadAll will consume.
	return io.ReadAll(io.LimitReader(src, maxBodyBytes+1))
}

// appendLine opens path in append-create-write mode and writes
// data followed by a single newline. Used for the per-signal
// JSONL files (one OTLP-JSON value per line).
func appendLine(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644) //nolint:gosec // G304: path is constructed from receiver-controlled OutDir + a small allowlist of signal filenames
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // file close after write; err already surfaced via Write
	if _, err := f.Write(data); err != nil {
		return err
	}
	if _, err := f.Write([]byte{'\n'}); err != nil {
		return err
	}
	return nil
}
