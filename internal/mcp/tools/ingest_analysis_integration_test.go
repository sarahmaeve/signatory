package tools

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/mcp"
	"github.com/sarahmaeve/signatory/internal/store"
)

// TestIngestAnalysisTool_StdioRoundTrip exercises signatory_ingest_analysis
// through the real MCP server: handshake, tools/call, envelope unwrap,
// and SQLite verification. Complementary to the unit tests in
// ingest_analysis_test.go, which call Handle() directly.
//
// Catches bugs in the dispatch → schema validation → handler →
// response-envelope path that unit tests bypass:
//
//   - Tool registration failure (wrong Name(), unregistered at startup).
//   - InputSchema JSON that doesn't parse or doesn't enforce required
//     fields via the validator shim.
//   - Response-envelope serialization (content[0].text shape per the
//     MCP 2025-11-25 spec).
//   - Initialize-handshake ordering: a tools/call before handshake
//     should be rejected with "server not yet initialized".
//
// Not covered here (lives in ingest_analysis_test.go): idempotency,
// schema-violation messages, counts correctness, source-field
// round-trip. This test focuses on the transport.
func TestIngestAnalysisTool_StdioRoundTrip(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	srv := mcp.NewServer("0.1.0-test")
	srv.Register(&IngestAnalysisTool{Store: s})

	// Wire the server to io.Pipe pairs — same shape as a real
	// stdio MCP client connecting to `signatory mcp`.
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()

	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- srv.Serve(ctx, stdinR, stdoutW)
		//nolint:errcheck // best-effort close; Serve already returned
		stdoutW.Close()
	}()

	stopServer := func() {
		cancel()
		//nolint:errcheck // best-effort close; pipe errors irrelevant to test outcome
		stdinW.Close()
		select {
		case <-serveDone:
		case <-time.After(5 * time.Second):
			t.Error("server did not stop within 5 seconds")
		}
	}
	t.Cleanup(stopServer)

	enc := json.NewEncoder(stdinW)
	dec := json.NewDecoder(stdoutR)

	// --- 1. Initialize handshake. -------------------------------------
	require.NoError(t, enc.Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-11-25",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"clientInfo":      map[string]any{"name": "integration-test", "version": "0.0.1"},
		},
	}))
	var initResp map[string]json.RawMessage
	require.NoError(t, dec.Decode(&initResp))
	require.NotNil(t, initResp["result"], "initialize should return a result, got: %s", initResp["error"])

	// Complete the handshake lifecycle by sending initialized
	// notification. Without it, tools/call would be rejected.
	require.NoError(t, enc.Encode(map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
		"params":  nil,
	}))

	// --- 2. Call signatory_ingest_analysis with a real payload. -------
	analystOutput := map[string]any{
		"attribution": map[string]any{
			"analyst_id": "stdio-integration",
			// model and invoked_at server-stamped at ingest;
			// caller-supplied values are rejected by the validator.
		},
		"target":      "pkg:test/stdio-widget",
		"conclusions": []any{}, // empty conclusions is valid
	}

	require.NoError(t, enc.Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "signatory_ingest_analysis",
			"arguments": map[string]any{
				"analyst_output": analystOutput,
				"source":         "mcp:stdio-integration",
			},
		},
	}))

	var callResp map[string]json.RawMessage
	require.NoError(t, dec.Decode(&callResp))
	require.NotNil(t, callResp["result"], "tools/call should return a result, got: %s", callResp["error"])

	// --- 3. Unwrap the MCP content envelope per spec. -----------------
	//
	// tools/call returns { content: [{type, text}], isError }. The
	// handler's Response is JSON-encoded into content[0].text. That
	// two-layer envelope is load-bearing for MCP-spec compliance, and
	// a bug there (wrong field name, missing field) would silently
	// degrade the real MCP client experience.
	var mcpResult struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	require.NoError(t, json.Unmarshal(callResp["result"], &mcpResult))
	require.Len(t, mcpResult.Content, 1, "expected exactly one content item")
	assert.Equal(t, "text", mcpResult.Content[0].Type)
	require.False(t, mcpResult.IsError,
		"tool returned isError=true, envelope text: %s", mcpResult.Content[0].Text)

	// --- 4. Unwrap signatory's Response envelope inside content.text. -
	var envelope struct {
		Status string          `json:"status"`
		Data   json.RawMessage `json:"data"`
		Error  *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}
	require.NoError(t, json.Unmarshal([]byte(mcpResult.Content[0].Text), &envelope))
	require.Equal(t, "ok", envelope.Status,
		"expected ok status, got error: %+v", envelope.Error)

	var data ingestAnalysisData
	require.NoError(t, json.Unmarshal(envelope.Data, &data))
	assert.NotEmpty(t, data.OutputID, "output_id must be returned")
	assert.NotEmpty(t, data.EntityID, "entity_id must be returned")
	assert.False(t, data.Idempotent, "first ingest should not be idempotent")

	// --- 5. Confirm SQLite has the row under the ID we got back. ------
	//
	// The whole point of this test is that the dispatch actually
	// caused a write. Checking the store directly (not via another
	// MCP call) guarantees we're not observing a hallucinated
	// response.
	filter := store.AnalystOutputFilter{EntityID: data.EntityID}
	summaries, err := s.ListAnalystOutputs(context.Background(), filter)
	require.NoError(t, err)
	require.Len(t, summaries, 1, "exactly one stored analyst output expected")
	assert.Equal(t, data.OutputID, summaries[0].OutputID)
	assert.Equal(t, "mcp:stdio-integration", summaries[0].SourcePath,
		"source field should round-trip through the full stdio path")
}

// TestIngestAnalysisTool_StdioSchemaViolation verifies that a
// malformed payload sent over the real stdio transport surfaces as
// an envelope-level error response — not a panic, not a protocol
// crash, not a silent success. The handler emits the error; the
// dispatch wraps it with isError=true; the client can read it and
// retry.
func TestIngestAnalysisTool_StdioSchemaViolation(t *testing.T) {
	t.Parallel()

	s := openTestStore(t)
	srv := mcp.NewServer("0.1.0-test")
	srv.Register(&IngestAnalysisTool{Store: s})

	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()

	ctx, cancel := context.WithCancel(context.Background())
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- srv.Serve(ctx, stdinR, stdoutW)
		//nolint:errcheck // best-effort close; Serve already returned
		stdoutW.Close()
	}()
	t.Cleanup(func() {
		cancel()
		//nolint:errcheck // best-effort close
		stdinW.Close()
		select {
		case <-serveDone:
		case <-time.After(5 * time.Second):
			t.Error("server did not stop")
		}
	})

	enc := json.NewEncoder(stdinW)
	dec := json.NewDecoder(stdoutR)

	// Handshake.
	require.NoError(t, enc.Encode(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-11-25",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"clientInfo":      map[string]any{"name": "test", "version": "0.0.1"},
		},
	}))
	var initResp map[string]json.RawMessage
	require.NoError(t, dec.Decode(&initResp))
	require.NoError(t, enc.Encode(map[string]any{
		"jsonrpc": "2.0", "method": "notifications/initialized", "params": nil,
	}))

	// tools/call with an analyst_output missing required Target.
	require.NoError(t, enc.Encode(map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{
			"name": "signatory_ingest_analysis",
			"arguments": map[string]any{
				"analyst_output": map[string]any{
					"attribution": map[string]any{
						"analyst_id": "x",
						// model and invoked_at server-stamped at ingest.
					},
					// target omitted on purpose.
				},
			},
		},
	}))

	var callResp map[string]json.RawMessage
	require.NoError(t, dec.Decode(&callResp))
	require.NotNil(t, callResp["result"])

	var mcpResult struct {
		Content []struct{ Type, Text string } `json:"content"`
		IsError bool                          `json:"isError"`
	}
	require.NoError(t, json.Unmarshal(callResp["result"], &mcpResult))
	assert.True(t, mcpResult.IsError,
		"schema violation should surface as isError=true")

	var envelope struct {
		Status string `json:"status"`
		Error  *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}
	require.NoError(t, json.Unmarshal([]byte(mcpResult.Content[0].Text), &envelope))
	assert.Equal(t, "error", envelope.Status)
	require.NotNil(t, envelope.Error)
	assert.Contains(t, envelope.Error.Message, "target",
		"schema error should name the missing field")

	// The store must NOT have a row — violation aborts before write.
	summaries, err := s.ListAnalystOutputs(context.Background(),
		store.AnalystOutputFilter{})
	require.NoError(t, err)
	assert.Empty(t, summaries, "no row should be written on schema violation")
}
