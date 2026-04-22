package mcp

// End-to-end tests for the Server via io.Pipe pairs. Each test writes
// a sequence of JSON-RPC messages to the server's input pipe and reads
// responses from the output pipe.
//
// Test quality: every test is designed to fail if the production code
// path it exercises is broken. See the mutation evidence in the report.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testServerVersion is the canonical version string tests inject into
// NewServer. Used by handshake-assertion and emission-stamping tests
// to distinguish the stamped value from "empty default."
const testServerVersion = "0.1.0-test"

// ---- stub implementations -----------------------------------------------

// echoTool is a minimal Tool that echoes its input back as data.
// Its schema has additionalProperties:false and a required "message" field.
type echoTool struct{}

func (echoTool) Name() string        { return "echo" }
func (echoTool) Description() string { return "Echoes the message field." }
func (echoTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{"message":{"type":"string"}},
		"required":["message"],
		"additionalProperties":false
	}`)
}
func (echoTool) Handle(_ context.Context, input json.RawMessage) *Response {
	var in struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return Err(CodeSchemaViolation, "bad input: "+err.Error(), nil)
	}
	return OK(map[string]string{"echo": in.Message})
}

// failTool always returns an error Response.
type failTool struct{}

func (failTool) Name() string        { return "fail" }
func (failTool) Description() string { return "Always returns an error." }
func (failTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","additionalProperties":false}`)
}
func (failTool) Handle(_ context.Context, _ json.RawMessage) *Response {
	return Err(CodeInternalError, "intentional failure", nil)
}

// staticResource serves a static JSON payload.
type staticResource struct {
	uriPattern string
	data       map[string]string
}

func (r *staticResource) URIPattern() string { return r.uriPattern }
func (r *staticResource) Description() string {
	return "Static test resource at " + r.uriPattern
}
func (r *staticResource) Read(_ context.Context, _ string) *Response {
	return OK(r.data)
}

// queryResource matches "signatory://items{?filter}" and surfaces the
// query parameter in its response.
type queryResource struct{}

func (queryResource) URIPattern() string  { return "signatory://items{?filter}" }
func (queryResource) Description() string { return "Templated resource with query param." }
func (queryResource) Read(_ context.Context, uri string) *Response {
	return OK(map[string]string{"uri": uri})
}

// ---- test helpers -------------------------------------------------------

// session writes JSON-RPC messages to the server and collects responses.
type session struct {
	enc *json.Encoder
	dec *json.Decoder
}

// newSession creates a Server with the provided tools/resources and
// starts it in a goroutine. Returns a session for writing requests and
// a cancel func to stop the server.
func newSession(t *testing.T, setup func(*Server)) (*session, context.CancelFunc) {
	t.Helper()
	srv := NewServer(testServerVersion)
	if setup != nil {
		setup(srv)
	}

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- srv.Serve(ctx, inR, outW)
		// Close the write end of out so the decoder gets EOF if the
		// test is still reading.
		outW.Close()
	}()

	s := &session{
		enc: json.NewEncoder(inW),
		dec: json.NewDecoder(outR),
	}

	// Wrap cancel to also close stdin and drain the server.
	stop := func() {
		cancel()
		inW.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("server did not stop within 5 seconds")
		}
	}

	return s, stop
}

// request sends a JSON-RPC request with the given id and method.
func (s *session) request(id any, method string, params any) {
	_ = s.enc.Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	})
}

// notify sends a JSON-RPC notification (no id).
func (s *session) notify(method string, params any) {
	_ = s.enc.Encode(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	})
}

// readResponse reads the next JSON-RPC response from the server.
func (s *session) readResponse(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	var out map[string]json.RawMessage
	require.NoError(t, s.dec.Decode(&out))
	return out
}

// doHandshake sends initialize + initialized and reads the initialize result.
func (s *session) doHandshake(t *testing.T) {
	t.Helper()
	s.request(1, "initialize", map[string]any{
		"protocolVersion": "2025-11-25",
		"capabilities":    map[string]any{"tools": map[string]any{}, "resources": map[string]any{}},
		"clientInfo":      map[string]any{"name": "test-client", "version": "0.0.1"},
	})
	resp := s.readResponse(t)
	require.NotNil(t, resp["result"], "expected initialize result")
	s.notify("notifications/initialized", nil)
}

// ---- end-to-end tests ---------------------------------------------------

// TestServer_Initialize_HappyPath verifies the full initialize handshake:
// server responds with correct protocolVersion, serverInfo, and capabilities.
func TestServer_Initialize_HappyPath(t *testing.T) {
	t.Parallel()
	s, stop := newSession(t, nil)
	defer stop()

	s.request(1, "initialize", map[string]any{
		"protocolVersion": "2025-11-25",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "claude-code", "version": "1.0"},
	})
	resp := s.readResponse(t)
	require.NotNil(t, resp["result"])
	require.Nil(t, resp["error"])

	var result initializeResult
	require.NoError(t, json.Unmarshal(resp["result"], &result))
	assert.Equal(t, "2025-11-25", result.ProtocolVersion)
	assert.Equal(t, "signatory", result.ServerInfo.Name)
	assert.NotEmpty(t, result.ServerInfo.Version)
	assert.NotNil(t, result.Capabilities.Tools)
	assert.NotNil(t, result.Capabilities.Resources)
}

// TestServer_ToolsList_AfterHandshake verifies that tools/list returns
// all registered tools with correct name/description/inputSchema.
func TestServer_ToolsList_AfterHandshake(t *testing.T) {
	t.Parallel()
	s, stop := newSession(t, func(srv *Server) {
		srv.Register(echoTool{})
		srv.Register(failTool{})
	})
	defer stop()

	s.doHandshake(t)
	s.request(2, "tools/list", nil)
	resp := s.readResponse(t)
	require.NotNil(t, resp["result"])

	var result toolsListResult
	require.NoError(t, json.Unmarshal(resp["result"], &result))
	assert.Len(t, result.Tools, 2)

	byName := map[string]toolEntry{}
	for _, e := range result.Tools {
		byName[e.Name] = e
	}
	assert.Contains(t, byName, "echo")
	assert.Contains(t, byName, "fail")
	assert.Equal(t, "Echoes the message field.", byName["echo"].Description)
	// InputSchema must be present and parseable.
	var schema map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(byName["echo"].InputSchema, &schema))
}

// TestServer_ToolsCall_HappyPath verifies a successful tool invocation:
// isError:false, content[0].text is valid JSON containing status:"ok".
func TestServer_ToolsCall_HappyPath(t *testing.T) {
	t.Parallel()
	s, stop := newSession(t, func(srv *Server) {
		srv.Register(echoTool{})
	})
	defer stop()

	s.doHandshake(t)
	s.request(3, "tools/call", map[string]any{
		"name":      "echo",
		"arguments": map[string]any{"message": "hello world"},
	})
	resp := s.readResponse(t)
	require.NotNil(t, resp["result"], "expected result, got error: %s", resp["error"])

	var result toolsCallResult
	require.NoError(t, json.Unmarshal(resp["result"], &result))
	assert.False(t, result.IsError)
	require.Len(t, result.Content, 1)
	assert.Equal(t, "text", result.Content[0].Type)

	var envelope Response
	require.NoError(t, json.Unmarshal([]byte(result.Content[0].Text), &envelope))
	assert.Equal(t, "ok", envelope.Status)
}

// TestServer_ToolsCall_HandlerError verifies that when a tool handler
// returns an error Response, isError is true and the envelope status is "error".
func TestServer_ToolsCall_HandlerError(t *testing.T) {
	t.Parallel()
	s, stop := newSession(t, func(srv *Server) {
		srv.Register(failTool{})
	})
	defer stop()

	s.doHandshake(t)
	s.request(4, "tools/call", map[string]any{
		"name":      "fail",
		"arguments": map[string]any{},
	})
	resp := s.readResponse(t)
	require.NotNil(t, resp["result"])

	var result toolsCallResult
	require.NoError(t, json.Unmarshal(resp["result"], &result))
	assert.True(t, result.IsError)
	require.Len(t, result.Content, 1)

	var envelope Response
	require.NoError(t, json.Unmarshal([]byte(result.Content[0].Text), &envelope))
	assert.Equal(t, "error", envelope.Status)
}

// TestServer_ToolsCall_UnknownMethod verifies that calling an unregistered
// tool returns a JSON-RPC error (not an isError tool result).
func TestServer_ToolsCall_UnknownTool(t *testing.T) {
	t.Parallel()
	s, stop := newSession(t, nil)
	defer stop()

	s.doHandshake(t)
	s.request(5, "tools/call", map[string]any{
		"name":      "nonexistent_tool",
		"arguments": map[string]any{},
	})
	resp := s.readResponse(t)
	require.NotNil(t, resp["error"], "expected protocol error for unknown tool")
	var rpcErr rpcError
	require.NoError(t, json.Unmarshal(resp["error"], &rpcErr))
	assert.Equal(t, codeInvalidParams, rpcErr.Code)
}

// TestServer_UnknownMethod verifies that an unknown method returns
// JSON-RPC error code -32601 (Method not found).
func TestServer_UnknownMethod(t *testing.T) {
	t.Parallel()
	s, stop := newSession(t, nil)
	defer stop()

	s.doHandshake(t)
	s.request(6, "magic/unknown", nil)
	resp := s.readResponse(t)
	require.NotNil(t, resp["error"])
	var rpcErr rpcError
	require.NoError(t, json.Unmarshal(resp["error"], &rpcErr))
	assert.Equal(t, codeMethodNotFound, rpcErr.Code)
}

// TestServer_MalformedJSON verifies that malformed JSON in the request
// stream results in a parse-error response (-32700) and the server
// continues processing subsequent valid messages.
func TestServer_MalformedJSON(t *testing.T) {
	t.Parallel()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	srv := NewServer(testServerVersion)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	go func() {
		_ = srv.Serve(ctx, inR, outW)
		outW.Close()
	}()

	dec := json.NewDecoder(outR)

	// Send malformed JSON.
	fmt.Fprintf(inW, "not valid json at all\n")
	var out1 map[string]json.RawMessage
	require.NoError(t, dec.Decode(&out1))
	require.NotNil(t, out1["error"])

	var rpcErr rpcError
	require.NoError(t, json.Unmarshal(out1["error"], &rpcErr))
	assert.Equal(t, codeParseError, rpcErr.Code)

	// Server should still be alive for subsequent messages.
	enc := json.NewEncoder(inW)
	_ = enc.Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      99,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-11-25",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "t", "version": "0"},
		},
	})
	var out2 map[string]json.RawMessage
	require.NoError(t, dec.Decode(&out2))
	require.NotNil(t, out2["result"])
	inW.Close()
}

// TestServer_SchemaViolation_UnknownField verifies that a tools/call
// with an unknown field in arguments is rejected with schema_violation
// and isError:true — the strict-reject posture from the architecture doc.
func TestServer_SchemaViolation_UnknownField(t *testing.T) {
	t.Parallel()
	s, stop := newSession(t, func(srv *Server) {
		srv.Register(echoTool{})
	})
	defer stop()

	s.doHandshake(t)
	s.request(7, "tools/call", map[string]any{
		"name": "echo",
		"arguments": map[string]any{
			"message":      "hello",
			"sneaky_extra": "hacked", // unknown field
		},
	})
	resp := s.readResponse(t)
	require.NotNil(t, resp["result"])

	var result toolsCallResult
	require.NoError(t, json.Unmarshal(resp["result"], &result))
	assert.True(t, result.IsError, "schema violation should set isError:true")

	var envelope Response
	require.NoError(t, json.Unmarshal([]byte(result.Content[0].Text), &envelope))
	assert.Equal(t, "error", envelope.Status)
	require.NotNil(t, envelope.Error)
	assert.Equal(t, CodeSchemaViolation, envelope.Error.Code)
	// Error must name the bad field.
	assert.Contains(t, envelope.Error.Message, "sneaky_extra")
}

// TestServer_ResourcesList verifies that resources/list returns all
// registered resources with correct URI, name, and description.
func TestServer_ResourcesList(t *testing.T) {
	t.Parallel()
	s, stop := newSession(t, func(srv *Server) {
		srv.RegisterResource(&staticResource{
			uriPattern: "signatory://posture",
			data:       map[string]string{"total": "42"},
		})
		srv.RegisterResource(&staticResource{
			uriPattern: "signatory://burns",
			data:       map[string]string{"count": "3"},
		})
	})
	defer stop()

	s.doHandshake(t)
	s.request(8, "resources/list", nil)
	resp := s.readResponse(t)
	require.NotNil(t, resp["result"])

	var result resourcesListResult
	require.NoError(t, json.Unmarshal(resp["result"], &result))
	assert.Len(t, result.Resources, 2)

	byURI := map[string]resourceEntry{}
	for _, r := range result.Resources {
		byURI[r.URI] = r
	}
	assert.Contains(t, byURI, "signatory://posture")
	assert.Contains(t, byURI, "signatory://burns")
	assert.Equal(t, "posture", byURI["signatory://posture"].Name)
}

// TestServer_ResourcesRead_StaticURI verifies that resources/read with
// an exact static URI returns the resource contents wrapped in the
// standard envelope.
func TestServer_ResourcesRead_StaticURI(t *testing.T) {
	t.Parallel()
	s, stop := newSession(t, func(srv *Server) {
		srv.RegisterResource(&staticResource{
			uriPattern: "signatory://posture",
			data:       map[string]string{"total": "100"},
		})
	})
	defer stop()

	s.doHandshake(t)
	s.request(9, "resources/read", map[string]any{
		"uri": "signatory://posture",
	})
	resp := s.readResponse(t)
	require.NotNil(t, resp["result"])

	var result resourcesReadResult
	require.NoError(t, json.Unmarshal(resp["result"], &result))
	require.Len(t, result.Contents, 1)
	assert.Equal(t, "signatory://posture", result.Contents[0].URI)

	var envelope Response
	require.NoError(t, json.Unmarshal([]byte(result.Contents[0].Text), &envelope))
	assert.Equal(t, "ok", envelope.Status)
}

// TestServer_ResourcesRead_TemplatedURI verifies that a query-parameterized
// URI like "signatory://analyses?target=repo:x" matches a templated pattern
// "signatory://analyses{?target}".
func TestServer_ResourcesRead_TemplatedURI(t *testing.T) {
	t.Parallel()
	s, stop := newSession(t, func(srv *Server) {
		srv.RegisterResource(queryResource{})
	})
	defer stop()

	s.doHandshake(t)
	s.request(10, "resources/read", map[string]any{
		"uri": "signatory://items?filter=active",
	})
	resp := s.readResponse(t)
	require.NotNil(t, resp["result"], "expected result, got error: %s", resp["error"])

	var result resourcesReadResult
	require.NoError(t, json.Unmarshal(resp["result"], &result))
	require.Len(t, result.Contents, 1)

	var envelope Response
	require.NoError(t, json.Unmarshal([]byte(result.Contents[0].Text), &envelope))
	assert.Equal(t, "ok", envelope.Status)
	// The full URI (with query params) must have been passed to the handler.
	assert.Equal(t, "signatory://items?filter=active", result.Contents[0].URI)
}

// TestServer_ResourcesRead_NotFound verifies that reading an unregistered
// URI returns a JSON-RPC error with code -32002.
func TestServer_ResourcesRead_NotFound(t *testing.T) {
	t.Parallel()
	s, stop := newSession(t, nil)
	defer stop()

	s.doHandshake(t)
	s.request(11, "resources/read", map[string]any{
		"uri": "signatory://nonexistent",
	})
	resp := s.readResponse(t)
	require.NotNil(t, resp["error"])
	var rpcErr rpcError
	require.NoError(t, json.Unmarshal(resp["error"], &rpcErr))
	assert.Equal(t, -32002, rpcErr.Code)
}

// TestServer_ClientInfo verifies that ClientInfo captures the clientInfo
// from the initialize handshake for use in audit logging.
func TestServer_ClientInfo(t *testing.T) {
	t.Parallel()
	srv := NewServer(testServerVersion)
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	go func() {
		_ = srv.Serve(ctx, inR, outW)
		outW.Close()
	}()

	enc := json.NewEncoder(inW)
	dec := json.NewDecoder(outR)

	_ = enc.Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-11-25",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "my-client", "version": "9.9.9"},
		},
	})

	var out map[string]json.RawMessage
	require.NoError(t, dec.Decode(&out))
	require.NotNil(t, out["result"])

	ci := srv.ClientInfo()
	assert.Equal(t, "my-client", ci.Name)
	assert.Equal(t, "9.9.9", ci.Version)
	assert.Equal(t, "mcp/my-client/9.9.9", ci.Source())

	inW.Close()
}

// TestServer_OperationsBeforeHandshake verifies that tools/list called
// before the initialized notification returns an error, not a result.
func TestServer_OperationsBeforeHandshake(t *testing.T) {
	t.Parallel()
	srv := NewServer(testServerVersion)
	srv.Register(echoTool{})
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	go func() {
		_ = srv.Serve(ctx, inR, outW)
		outW.Close()
	}()

	enc := json.NewEncoder(inW)
	dec := json.NewDecoder(outR)

	// tools/list WITHOUT initialize first.
	_ = enc.Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/list",
	})
	var out map[string]json.RawMessage
	require.NoError(t, dec.Decode(&out))
	require.NotNil(t, out["error"], "should get error before handshake")

	inW.Close()
}

// TestServer_ContextCancellation verifies that when the context is
// cancelled and stdin is closed, Serve returns cleanly.
// Note: for stdio transport, context cancellation alone doesn't unblock a
// blocking pipe read — the transport must also be closed (stdin closed).
// The stop() helper in newSession does both. This test exercises the
// context-check-before-read path by closing the pipe after cancellation.
func TestServer_ContextCancellation(t *testing.T) {
	t.Parallel()
	srv := NewServer(testServerVersion)
	inR, inW := io.Pipe()
	_, outW := io.Pipe()
	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)
	go func() {
		done <- srv.Serve(ctx, inR, outW)
		outW.Close()
	}()

	// Cancel the context, then close stdin to unblock the read loop.
	cancel()
	inW.Close() // triggers EOF in the scanner, unblocks Serve

	select {
	case err := <-done:
		// Either ctx.Canceled (caught in the select before the next read)
		// or nil (EOF from inW.Close() caught first). Both are valid
		// clean shutdowns; neither should be an unexpected error.
		if err != nil {
			assert.ErrorIs(t, err, context.Canceled)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not stop after context cancellation + pipe close")
	}
}

// TestServer_SchemaViolation_MissingRequired verifies that a tools/call
// missing a required field is rejected with schema_violation.
func TestServer_SchemaViolation_MissingRequired(t *testing.T) {
	t.Parallel()
	s, stop := newSession(t, func(srv *Server) {
		srv.Register(echoTool{})
	})
	defer stop()

	s.doHandshake(t)
	// echo requires "message" but we send an empty arguments object.
	s.request(12, "tools/call", map[string]any{
		"name":      "echo",
		"arguments": map[string]any{},
	})
	resp := s.readResponse(t)
	require.NotNil(t, resp["result"])

	var result toolsCallResult
	require.NoError(t, json.Unmarshal(resp["result"], &result))
	assert.True(t, result.IsError)

	var envelope Response
	require.NoError(t, json.Unmarshal([]byte(result.Content[0].Text), &envelope))
	assert.Equal(t, CodeSchemaViolation, envelope.Error.Code)
	assert.Contains(t, envelope.Error.Message, "message")
}

// TestServer_ToolResponse_EnvelopeShape verifies that the tools/call
// result uses the exact MCP 2025-11-25 envelope shape:
// {content:[{type:"text",text:"..."}], isError:bool}.
// This is a direct spec-conformance test.
func TestServer_ToolResponse_EnvelopeShape(t *testing.T) {
	t.Parallel()
	s, stop := newSession(t, func(srv *Server) {
		srv.Register(echoTool{})
	})
	defer stop()

	s.doHandshake(t)
	s.request(13, "tools/call", map[string]any{
		"name":      "echo",
		"arguments": map[string]any{"message": "test"},
	})
	resp := s.readResponse(t)
	require.NotNil(t, resp["result"])

	// Decode as generic map to verify exact field names.
	var rawResult map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(resp["result"], &rawResult))
	_, hasContent := rawResult["content"]
	_, hasIsError := rawResult["isError"]
	assert.True(t, hasContent, "result must have 'content' field per MCP spec")
	assert.True(t, hasIsError, "result must have 'isError' field per MCP spec")

	var contentArr []map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rawResult["content"], &contentArr))
	require.Len(t, contentArr, 1)
	_, hasType := contentArr[0]["type"]
	_, hasText := contentArr[0]["text"]
	assert.True(t, hasType, "content item must have 'type' field")
	assert.True(t, hasText, "content item must have 'text' field")

	var typeVal string
	require.NoError(t, json.Unmarshal(contentArr[0]["type"], &typeVal))
	assert.Equal(t, "text", typeVal)
}

// TestHandshake_ClientInfoSource verifies the Source() format for
// audit logging.
func TestHandshake_ClientInfoSource(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		info     clientInfo
		expected string
	}{
		{"full info", clientInfo{Name: "claude-code", Version: "1.2.3"}, "mcp/claude-code/1.2.3"},
		{"empty name", clientInfo{Name: "", Version: "1.0"}, "mcp/unknown/1.0"},
		{"empty version", clientInfo{Name: "test", Version: ""}, "mcp/test/unknown"},
		{"both empty", clientInfo{}, "mcp/unknown/unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.expected, tt.info.Source())
		})
	}
}

// TestURIBase verifies the uriBase helper used in resource matching.
func TestURIBase(t *testing.T) {
	t.Parallel()
	tests := []struct {
		uri      string
		expected string
	}{
		{"signatory://posture", "signatory://posture"},
		{"signatory://analyses?target=repo:x", "signatory://analyses"},
		{"signatory://items{?filter}", "signatory://items"},
		{"signatory://config", "signatory://config"},
	}
	for _, tt := range tests {
		t.Run(tt.uri, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.expected, uriBase(tt.uri))
		})
	}
}

// TestResourceName verifies that resourceName extracts readable display
// names from signatory:// URIs.
func TestResourceName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		pattern  string
		expected string
	}{
		{"signatory://posture", "posture"},
		{"signatory://analyses{?target}", "analyses"},
		{"signatory://signal-types", "signal-types"},
	}
	for _, tt := range tests {
		t.Run(tt.pattern, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.expected, resourceName(tt.pattern))
		})
	}
}

// TestServer_ToolsList_Sorted verifies that tools/list returns tools
// in alphabetical order for deterministic client rendering.
func TestServer_ToolsList_Sorted(t *testing.T) {
	t.Parallel()
	s, stop := newSession(t, func(srv *Server) {
		srv.Register(failTool{}) // "fail" sorts after "echo"
		srv.Register(echoTool{}) // "echo"
	})
	defer stop()

	s.doHandshake(t)
	s.request(14, "tools/list", nil)
	resp := s.readResponse(t)
	require.NotNil(t, resp["result"])

	var result toolsListResult
	require.NoError(t, json.Unmarshal(resp["result"], &result))
	require.Len(t, result.Tools, 2)
	assert.Equal(t, "echo", result.Tools[0].Name, "echo should sort before fail")
	assert.Equal(t, "fail", result.Tools[1].Name)
}

// TestServer_ElapsedMs verifies that the response envelope's elapsed_ms
// field is populated (> 0 is not guaranteed for fast handlers, but it
// should be >= 0 and parseable).
func TestServer_ElapsedMs(t *testing.T) {
	t.Parallel()
	s, stop := newSession(t, func(srv *Server) {
		srv.Register(echoTool{})
	})
	defer stop()

	s.doHandshake(t)
	s.request(15, "tools/call", map[string]any{
		"name":      "echo",
		"arguments": map[string]any{"message": "timing test"},
	})
	resp := s.readResponse(t)

	var callResult toolsCallResult
	require.NoError(t, json.Unmarshal(resp["result"], &callResult))
	require.Len(t, callResult.Content, 1)

	var envelope Response
	require.NoError(t, json.Unmarshal([]byte(callResult.Content[0].Text), &envelope))
	assert.GreaterOrEqual(t, envelope.Metadata.ElapsedMs, int64(0))
	// The dispatch layer stamps ServerVersion from the Server's injected
	// version string (NewServer(testServerVersion)) — handlers leave it
	// empty. Mutation check: change dispatch.go's
	// resp.Metadata.ServerVersion = s.version back to the deleted
	// package-level ServerVersion var and this assertion fails (the
	// identifier no longer exists; build breaks).
	assert.Equal(t, testServerVersion, envelope.Metadata.ServerVersion)
}

// TestServer_ResourcesRead_Prefix verifies that a static resource URI
// matches a request URI that has a query string appended (prefix matching).
func TestServer_ResourcesRead_Prefix(t *testing.T) {
	t.Parallel()
	s, stop := newSession(t, func(srv *Server) {
		srv.RegisterResource(&staticResource{
			uriPattern: "signatory://analyses",
			data:       map[string]string{"kind": "analyses"},
		})
	})
	defer stop()

	s.doHandshake(t)
	s.request(16, "resources/read", map[string]any{
		"uri": "signatory://analyses?target=repo:github/foo/bar",
	})
	resp := s.readResponse(t)
	// Should match via prefix.
	require.Nil(t, resp["error"], "prefix match should succeed")
	require.NotNil(t, resp["result"])
}

// TestServer_MultipleRequests_IDIsolation verifies that responses carry
// the correct id when multiple requests are outstanding. We send two
// requests and verify each response echoes back the right id.
func TestServer_MultipleRequests_IDIsolation(t *testing.T) {
	t.Parallel()
	s, stop := newSession(t, func(srv *Server) {
		srv.Register(echoTool{})
	})
	defer stop()

	s.doHandshake(t)

	s.request("req-alpha", "tools/call", map[string]any{
		"name":      "echo",
		"arguments": map[string]any{"message": "alpha"},
	})
	s.request("req-beta", "tools/call", map[string]any{
		"name":      "echo",
		"arguments": map[string]any{"message": "beta"},
	})

	// Collect both responses.
	byID := map[string]map[string]json.RawMessage{}
	for i := 0; i < 2; i++ {
		resp := s.readResponse(t)
		var id string
		require.NoError(t, json.Unmarshal(resp["id"], &id))
		byID[id] = resp
	}

	assert.Contains(t, byID, "req-alpha")
	assert.Contains(t, byID, "req-beta")

	for _, id := range []string{"req-alpha", "req-beta"} {
		require.NotNil(t, byID[id]["result"], "id %s should have result", id)
	}
}

// ---- M2: Register enforces additionalProperties:false ----------------

// permissiveTool declares no additionalProperties constraint. A Register
// call with this tool must panic — the strict-reject posture is a
// contract of the Tool interface (see interfaces.go) and a silently
// permissive schema would slip unknown fields through validation.
type permissiveTool struct{}

func (permissiveTool) Name() string        { return "permissive" }
func (permissiveTool) Description() string { return "omits additionalProperties:false" }
func (permissiveTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{"x":{"type":"string"}},
		"required":[]
	}`)
}
func (permissiveTool) Handle(_ context.Context, _ json.RawMessage) *Response {
	return OK(nil)
}

// explicitlyPermissiveTool declares additionalProperties:true explicitly.
// Register must panic on this too — "true" is semantically equivalent
// to "omitted" for our purposes.
type explicitlyPermissiveTool struct{}

func (explicitlyPermissiveTool) Name() string        { return "explicit_permissive" }
func (explicitlyPermissiveTool) Description() string { return "explicit additionalProperties:true" }
func (explicitlyPermissiveTool) InputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"properties":{"x":{"type":"string"}},
		"additionalProperties":true
	}`)
}
func (explicitlyPermissiveTool) Handle(_ context.Context, _ json.RawMessage) *Response {
	return OK(nil)
}

// TestServer_Register_PanicsOnPermissiveSchema is the M2 regression:
// a tool whose schema does not declare additionalProperties:false must
// trigger a panic at Register time with a message naming the tool.
// Discovering this at startup (vs. letting a permissive schema slip
// into production) is the whole point of enforcing it here.
func TestServer_Register_PanicsOnPermissiveSchema(t *testing.T) {
	t.Parallel()

	defer func() {
		r := recover()
		require.NotNil(t, r, "Register must panic when additionalProperties:false is missing")
		msg, ok := r.(string)
		require.True(t, ok, "panic value should be a string, got %T", r)
		assert.Contains(t, msg, "permissive",
			"panic message must name the offending tool")
		assert.Contains(t, msg, "additionalProperties:false",
			"panic message must identify the missing invariant")
	}()

	srv := NewServer(testServerVersion)
	srv.Register(permissiveTool{}) // should panic before return
}

// TestServer_Register_PanicsOnExplicitAdditionalPropertiesTrue covers
// the explicit "additionalProperties": true case separately from the
// "omitted" case — both are permissive, both must be rejected.
func TestServer_Register_PanicsOnExplicitAdditionalPropertiesTrue(t *testing.T) {
	t.Parallel()

	defer func() {
		r := recover()
		require.NotNil(t, r, "Register must panic on additionalProperties:true")
	}()

	srv := NewServer(testServerVersion)
	srv.Register(explicitlyPermissiveTool{})
}

// TestServer_Register_SucceedsWithStrictSchema is the positive control
// for the M2 enforcement: echoTool's schema does include
// additionalProperties:false and Register must accept it without
// panicking. If this test ever fails, the M2 check has been
// over-tightened.
func TestServer_Register_SucceedsWithStrictSchema(t *testing.T) {
	t.Parallel()

	// A bare function call is the test: if Register panics, the test
	// itself panics. No recover() needed.
	srv := NewServer(testServerVersion)
	srv.Register(echoTool{})
}

// ---- M4: static URI matching requires a legal boundary ----------------

// TestServer_ResourcesRead_PrefixBoundary is the M4 regression test.
// Before the fix, matchResource used plain HasPrefix, so a short
// pattern could shadow a longer URI — "signatory://ana" would
// silently handle requests for "signatory://analyses". The fix
// requires the URI to equal the pattern exactly, or to continue with
// "?" or "#". This test registers two resources where one pattern is
// a prefix of the other and verifies each URI reaches its intended
// handler.
func TestServer_ResourcesRead_PrefixBoundary(t *testing.T) {
	t.Parallel()
	s, stop := newSession(t, func(srv *Server) {
		// Shorter pattern first — if HasPrefix were still in use, it
		// would win the iteration coin flip some fraction of the time
		// and intercept the longer URI.
		srv.RegisterResource(&staticResource{
			uriPattern: "signatory://ana",
			data:       map[string]string{"kind": "ana"},
		})
		srv.RegisterResource(&staticResource{
			uriPattern: "signatory://analyses",
			data:       map[string]string{"kind": "analyses"},
		})
	})
	defer stop()
	s.doHandshake(t)

	// 1. Exact URI for the shorter pattern → shorter handler.
	s.request(101, "resources/read", map[string]any{"uri": "signatory://ana"})
	resp := s.readResponse(t)
	require.Nil(t, resp["error"], "shorter pattern must handle its own URI")
	assertResourcePayloadKind(t, resp, "ana")

	// 2. Exact URI for the longer pattern → longer handler. If
	//    HasPrefix were still in effect, the shorter pattern could
	//    intercept this.
	s.request(102, "resources/read", map[string]any{"uri": "signatory://analyses"})
	resp = s.readResponse(t)
	require.Nil(t, resp["error"], "longer pattern must handle its own URI")
	assertResourcePayloadKind(t, resp, "analyses")

	// 3. Longer URI with query string → longer handler via the
	//    pattern+"?" branch. Same concern: the shorter pattern must
	//    not win this match.
	s.request(103, "resources/read", map[string]any{
		"uri": "signatory://analyses?target=repo:github/foo/bar",
	})
	resp = s.readResponse(t)
	require.Nil(t, resp["error"], "query-string URI must resolve to longer pattern")
	assertResourcePayloadKind(t, resp, "analyses")

	// 4. A URI that extends the shorter pattern at a non-boundary
	//    character ("ana" + "lyses" with no ?/#) must NOT match the
	//    shorter pattern via its own loop iteration. It still matches
	//    the longer pattern (exact match, step 1 of matchResource),
	//    so we'd see the longer handler either way — the proof is that
	//    step 3 also reaches the longer handler, which it can only
	//    do if the short pattern did not intercept.
	//
	//    To construct a case where only a shorter-prefix-shadow
	//    failure would be observable, register a URI that extends
	//    the short pattern but has no registered longer match — it
	//    should return resource-not-found, not silently match the
	//    short pattern.
	s.request(104, "resources/read", map[string]any{
		"uri": "signatory://ana-other",
	})
	resp = s.readResponse(t)
	require.NotNil(t, resp["error"], "extended URI with no exact match must surface not-found")
}

// TestServer_HandshakeRace_PipelinedToolsListBeforeInitialized is
// the integration-level race test for the handshake fix. It complements
// the unit-level TestHandshake_*IsRaceFree tests in handshake_test.go
// by exercising the race through the production goroutine model —
// a future refactor of dispatch or Serve that silently reintroduces
// unsynchronized access to handshake state would pass the unit test
// (which hits the type directly) but fail this one.
//
// Shape: pipeline N tools/list requests between initialize and
// notifications/initialized. The server's read loop dispatches each
// tools/list in a spawned handler goroutine (server.go:217), which
// reads handshake.state via isOperational at dispatch.go:108. The
// read loop subsequently writes handshake.state when it processes
// the notifications/initialized frame. There is no happens-before
// edge from those writes to the earlier-spawned reads — a structural
// race that `go test -race` flags every run.
//
// Revert proof: remove the mutex from handshake's methods. Under
// `go test -race ./internal/mcp -run TestServer_HandshakeRace`,
// the detector reports a data race on handshake.state with stacks
// pointing at handleInitializedNotification (writer, goroutine 1)
// and isOperational (reader, one of the handler goroutines).
//
// The semantic content of each tools/list response isn't asserted.
// Under the pre-fix code some may observe stateInitialized and
// return "not yet initialized" while others see stateOperational
// and return the tool list — both outcomes are protocol-legal for
// the pipelined client sequence, and neither is the point of the
// test. The race detector is the entire assertion.
//
// Not parallelized: see TestHandshake_StateAccessIsRaceFree for
// rationale.
func TestServer_HandshakeRace_PipelinedToolsListBeforeInitialized(t *testing.T) {
	s, stop := newSession(t, func(srv *Server) {
		srv.Register(echoTool{})
	})
	defer stop()

	s.request(1, "initialize", map[string]any{
		"protocolVersion": "2025-11-25",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "race-client", "version": "0.0.0"},
	})
	// Drain the initialize response so the pipe doesn't stall.
	_ = s.readResponse(t)

	// Pipeline tools/list BEFORE notifications/initialized. Each
	// spawns a handler goroutine that reads handshake.state; the
	// notifications/initialized that follows writes handshake.state.
	// N chosen generous enough that some goroutines are still
	// pending at state-write time on any plausible scheduler, but
	// strictly the race detector doesn't require overlap — just
	// absence of a happens-before edge between the accesses.
	const n = 32
	for i := 2; i <= n+1; i++ {
		s.request(i, "tools/list", nil)
	}

	// State write while N handler goroutines' reads have no
	// happens-before edge to this write.
	s.notify("notifications/initialized", nil)

	// Drain all N responses. Their contents aren't asserted —
	// either "not yet initialized" or a tools list is fine. Drain
	// prevents the server's write side from stalling on pipe
	// backpressure at test shutdown.
	for i := 0; i < n; i++ {
		_ = s.readResponse(t)
	}
}

// assertResourcePayloadKind extracts data.kind from a resources/read
// envelope and asserts it equals want. Factored out because three
// subcases in TestServer_ResourcesRead_PrefixBoundary share the check.
func assertResourcePayloadKind(t *testing.T, resp map[string]json.RawMessage, want string) {
	t.Helper()
	var result struct {
		Contents []struct {
			Text string `json:"text"`
		} `json:"contents"`
	}
	require.NoError(t, json.Unmarshal(resp["result"], &result))
	require.Len(t, result.Contents, 1)

	var env struct {
		Data map[string]string `json:"data"`
	}
	require.NoError(t, json.Unmarshal([]byte(result.Contents[0].Text), &env))
	assert.Equal(t, want, env.Data["kind"],
		"resources/read must reach the handler matching the URI boundary, not a shorter prefix")
}
