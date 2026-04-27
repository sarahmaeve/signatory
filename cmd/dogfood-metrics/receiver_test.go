package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Sample OTLP-JSON bodies — the minimum shape the OTLP JSON Mapping
// spec requires for a valid ExportTraceServiceRequest /
// ExportLogsServiceRequest. Compact-form (no whitespace) so the
// "appended verbatim" assertion stays unambiguous.
const sampleTracesBody = `{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"claude-code"}},{"key":"session.id","value":{"stringValue":"sess-test"}}]},"scopeSpans":[{"spans":[{"name":"claude_code.tool","spanId":"0102030405060708","traceId":"0102030405060708090a0b0c0d0e0f10","startTimeUnixNano":"1714250000000000000","endTimeUnixNano":"1714250001000000000","attributes":[{"key":"tool.name","value":{"stringValue":"Bash"}}]}]}]}]}`

const sampleLogsBody = `{"resourceLogs":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"claude-code"}}]},"scopeLogs":[{"logRecords":[{"timeUnixNano":"1714250000000000000","body":{"stringValue":"test"}}]}]}]}`

// TestReceiver_AcceptsTraces — happy path for /v1/traces. POST one
// OTLP-JSON body, expect 202 + a single JSONL line on disk that
// matches the input.
func TestReceiver_AcceptsTraces(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	rcv := &Receiver{OutDir: dir}

	req := httptest.NewRequest(http.MethodPost, "/v1/traces", strings.NewReader(sampleTracesBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	rcv.ServeHTTP(w, req)

	assert.Equal(t, http.StatusAccepted, w.Code, "OTLP/HTTP success is 202 Accepted")
	assert.Equal(t, sampleTracesBody+"\n", readFile(t, filepath.Join(dir, "traces.jsonl")))
}

// TestReceiver_AcceptsLogs — same shape as traces but for /v1/logs.
// Confirms the path → output-file routing.
func TestReceiver_AcceptsLogs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	rcv := &Receiver{OutDir: dir}

	req := httptest.NewRequest(http.MethodPost, "/v1/logs", strings.NewReader(sampleLogsBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	rcv.ServeHTTP(w, req)

	assert.Equal(t, http.StatusAccepted, w.Code)
	assert.Equal(t, sampleLogsBody+"\n", readFile(t, filepath.Join(dir, "logs.jsonl")))
}

// TestReceiver_DecompressesGzip — round-4 verification flagged that
// Claude Code's compression behavior is undocumented (no
// OTEL_EXPORTER_OTLP_COMPRESSION mention in the docs), so the
// receiver MUST handle gzipped bodies. POST a gzipped OTLP-JSON
// body with Content-Encoding: gzip, expect the decompressed body
// on disk.
func TestReceiver_DecompressesGzip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	rcv := &Receiver{OutDir: dir}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, err := gz.Write([]byte(sampleTracesBody))
	require.NoError(t, err)
	require.NoError(t, gz.Close())

	req := httptest.NewRequest(http.MethodPost, "/v1/traces", &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	w := httptest.NewRecorder()
	rcv.ServeHTTP(w, req)

	assert.Equal(t, http.StatusAccepted, w.Code)
	assert.Equal(t, sampleTracesBody+"\n", readFile(t, filepath.Join(dir, "traces.jsonl")),
		"gzipped body must be decompressed before writing to disk")
}

// TestReceiver_RejectsInvalidJSON — malformed JSON in the body
// produces 400 Bad Request and does NOT write to disk. The
// receiver compacts each body via json.Compact before persisting,
// so invalid JSON fails fast at the boundary.
func TestReceiver_RejectsInvalidJSON(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	rcv := &Receiver{OutDir: dir}

	req := httptest.NewRequest(http.MethodPost, "/v1/traces", strings.NewReader(`{"unterminated":`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	rcv.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	_, err := os.Stat(filepath.Join(dir, "traces.jsonl"))
	assert.True(t, os.IsNotExist(err), "no file should be written on bad input")
}

// TestReceiver_RejectsUnknownPath — paths outside our router (e.g.,
// /v1/metrics, which we don't model in v0.1) return 404. Defenses
// the file system against attacker-controlled path components
// finding their way into output filenames.
func TestReceiver_RejectsUnknownPath(t *testing.T) {
	t.Parallel()
	cases := []string{"/", "/v1/metrics", "/v1/traces/extra", "/healthz", "/../etc/passwd"}
	for _, path := range cases {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			rcv := &Receiver{OutDir: t.TempDir()}
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(sampleTracesBody))
			w := httptest.NewRecorder()
			rcv.ServeHTTP(w, req)
			assert.Equal(t, http.StatusNotFound, w.Code, "path %q should be 404", path)
		})
	}
}

// TestReceiver_RejectsNonPOST — OTLP/HTTP only defines POST for the
// signal endpoints. GET/PUT/DELETE return 405 Method Not Allowed
// with the Allow header populated.
func TestReceiver_RejectsNonPOST(t *testing.T) {
	t.Parallel()
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			rcv := &Receiver{OutDir: t.TempDir()}
			req := httptest.NewRequest(method, "/v1/traces", nil)
			w := httptest.NewRecorder()
			rcv.ServeHTTP(w, req)
			assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
			assert.Equal(t, "POST", w.Header().Get("Allow"))
		})
	}
}

// TestReceiver_AppendsMultipleRequests — three sequential POSTs
// produce three lines in the output file, each individually valid
// JSON. Pins the JSONL contract (one JSON value per line).
func TestReceiver_AppendsMultipleRequests(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	rcv := &Receiver{OutDir: dir}

	for range 3 {
		req := httptest.NewRequest(http.MethodPost, "/v1/traces", strings.NewReader(sampleTracesBody))
		w := httptest.NewRecorder()
		rcv.ServeHTTP(w, req)
		require.Equal(t, http.StatusAccepted, w.Code)
	}

	contents := readFile(t, filepath.Join(dir, "traces.jsonl"))
	lines := strings.Split(strings.TrimRight(contents, "\n"), "\n")
	assert.Len(t, lines, 3, "three POSTs should produce three JSONL lines")
	for i, line := range lines {
		var parsed map[string]any
		assert.NoError(t, json.Unmarshal([]byte(line), &parsed), "line %d should be valid JSON", i)
	}
}

// TestReceiver_CompactsPrettyPrintedJSON — input bodies with
// whitespace (newlines, indentation) get compacted before writing
// so the on-disk JSONL contract holds (each line = one JSON value,
// no embedded newlines). Without this, a pretty-printed input would
// produce a multi-line entry that JSONL parsers reject.
func TestReceiver_CompactsPrettyPrintedJSON(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	rcv := &Receiver{OutDir: dir}

	pretty := "{\n  \"resourceSpans\": [\n    {\"resource\": {\"attributes\": []}, \"scopeSpans\": []}\n  ]\n}"
	req := httptest.NewRequest(http.MethodPost, "/v1/traces", strings.NewReader(pretty))
	w := httptest.NewRecorder()
	rcv.ServeHTTP(w, req)
	require.Equal(t, http.StatusAccepted, w.Code)

	contents := readFile(t, filepath.Join(dir, "traces.jsonl"))
	assert.Equal(t, 1, strings.Count(contents, "\n"), "compacted body should be single line")
	var parsed map[string]any
	assert.NoError(t, json.Unmarshal([]byte(strings.TrimRight(contents, "\n")), &parsed))
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // G304: test reads from t.TempDir
	require.NoError(t, err)
	return string(b)
}
