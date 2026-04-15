package mcp

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCodec_ReadRequest_HappyPath verifies that a valid JSON-RPC request
// is decoded correctly with all fields preserved.
func TestCodec_ReadRequest_HappyPath(t *testing.T) {
	t.Parallel()
	line := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}` + "\n"
	c := newCodec(strings.NewReader(line), io.Discard)

	req, err := c.readRequest()
	require.NoError(t, err)
	assert.Equal(t, "2.0", req.JSONRPC)
	assert.Equal(t, "tools/list", req.Method)
	assert.Equal(t, json.RawMessage(`1`), req.ID)
	assert.False(t, req.isNotification())
}

// TestCodec_ReadRequest_Notification verifies that a message without an
// id field is recognised as a notification.
func TestCodec_ReadRequest_Notification(t *testing.T) {
	t.Parallel()
	line := `{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n"
	c := newCodec(strings.NewReader(line), io.Discard)

	req, err := c.readRequest()
	require.NoError(t, err)
	assert.True(t, req.isNotification())
	assert.Equal(t, "notifications/initialized", req.Method)
}

// TestCodec_ReadRequest_ParseError verifies that malformed JSON returns
// an rpcError with the parse-error code (-32700), not a Go error.
func TestCodec_ReadRequest_ParseError(t *testing.T) {
	t.Parallel()
	line := `{"jsonrpc":"2.0","id":1,"method":` + "\n" // truncated JSON
	c := newCodec(strings.NewReader(line), io.Discard)

	_, err := c.readRequest()
	require.Error(t, err)
	var rpcErr *rpcError
	require.ErrorAs(t, err, &rpcErr)
	assert.Equal(t, codeParseError, rpcErr.Code)
}

// TestCodec_ReadRequest_WrongVersion verifies that a message with
// jsonrpc != "2.0" is rejected with -32600 (Invalid Request).
func TestCodec_ReadRequest_WrongVersion(t *testing.T) {
	t.Parallel()
	line := `{"jsonrpc":"1.0","id":1,"method":"foo"}` + "\n"
	c := newCodec(strings.NewReader(line), io.Discard)

	_, err := c.readRequest()
	require.Error(t, err)
	var rpcErr *rpcError
	require.ErrorAs(t, err, &rpcErr)
	assert.Equal(t, codeInvalidRequest, rpcErr.Code)
}

// TestCodec_ReadRequest_MissingMethod verifies that a message with no
// method field is rejected with -32600.
func TestCodec_ReadRequest_MissingMethod(t *testing.T) {
	t.Parallel()
	line := `{"jsonrpc":"2.0","id":1}` + "\n"
	c := newCodec(strings.NewReader(line), io.Discard)

	_, err := c.readRequest()
	require.Error(t, err)
	var rpcErr *rpcError
	require.ErrorAs(t, err, &rpcErr)
	assert.Equal(t, codeInvalidRequest, rpcErr.Code)
}

// TestCodec_ReadRequest_EOF verifies that a closed reader returns io.EOF,
// not another error type.
func TestCodec_ReadRequest_EOF(t *testing.T) {
	t.Parallel()
	c := newCodec(strings.NewReader(""), io.Discard)
	_, err := c.readRequest()
	assert.ErrorIs(t, err, io.EOF)
}

// TestCodec_WriteResult verifies that writeResult produces valid JSON-RPC
// with the correct id and result fields.
func TestCodec_WriteResult(t *testing.T) {
	t.Parallel()
	var buf strings.Builder
	c := newCodec(strings.NewReader(""), &buf)

	err := c.writeResult(json.RawMessage(`42`), map[string]string{"status": "ok"})
	require.NoError(t, err)

	var out struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  json.RawMessage `json:"result"`
		Error   *rpcError       `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &out))
	assert.Equal(t, "2.0", out.JSONRPC)
	assert.Equal(t, json.RawMessage(`42`), out.ID)
	assert.Nil(t, out.Error)
	assert.Contains(t, string(out.Result), "ok")
}

// TestCodec_WriteError verifies that writeError produces a JSON-RPC error
// response with the specified code and message.
func TestCodec_WriteError(t *testing.T) {
	t.Parallel()
	var buf strings.Builder
	c := newCodec(strings.NewReader(""), &buf)

	err := c.writeError(json.RawMessage(`"abc"`), codeMethodNotFound, "method not found: foo", nil)
	require.NoError(t, err)

	var out struct {
		JSONRPC string    `json:"jsonrpc"`
		ID      any       `json:"id"`
		Error   *rpcError `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &out))
	assert.Equal(t, "2.0", out.JSONRPC)
	require.NotNil(t, out.Error)
	assert.Equal(t, codeMethodNotFound, out.Error.Code)
	assert.Contains(t, out.Error.Message, "method not found")
}

// TestCodec_IDCorrelation verifies that the response id matches the
// request id exactly, for both integer and string ids.
func TestCodec_IDCorrelation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		id   string
	}{
		{"integer id", `1`},
		{"string id", `"req-abc-123"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			line := `{"jsonrpc":"2.0","id":` + tt.id + `,"method":"tools/list"}` + "\n"
			r := strings.NewReader(line)
			var buf strings.Builder
			c := newCodec(r, &buf)

			req, err := c.readRequest()
			require.NoError(t, err)
			require.NoError(t, c.writeResult(req.ID, map[string]int{"count": 0}))

			var out struct {
				ID json.RawMessage `json:"id"`
			}
			require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &out))
			assert.Equal(t, json.RawMessage(tt.id), out.ID)
		})
	}
}

// TestCodec_MaxLineBytes_LargeMessage verifies that a frame just under
// the size limit is accepted. Exercises the DoS-prevention cap's upper
// boundary without tripping it.
func TestCodec_MaxLineBytes_LargeMessage(t *testing.T) {
	t.Parallel()
	// A string that, wrapped in the JSON envelope, lands comfortably
	// under maxLineBytes. Half the limit leaves headroom for the
	// envelope bytes and the trailing newline.
	bigValue := strings.Repeat("x", maxLineBytes/2)
	line := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"x":"` + bigValue + `"}}` + "\n"
	require.Less(t, len(line), maxLineBytes,
		"fixture must fit within frame limit or the test isn't exercising the intended path")

	c := newCodec(strings.NewReader(line), io.Discard)
	req, err := c.readRequest()
	require.NoError(t, err)
	assert.Equal(t, "tools/list", req.Method)
}

// TestCodec_ReadRequest_OversizeFrame_ReturnsRPCError verifies that a
// frame exceeding maxLineBytes surfaces as a recoverable *rpcError
// (code -32700), not a transport-level error. This is the regression
// test for the bufio.Scanner ErrTooLong path that used to terminate
// the server.
func TestCodec_ReadRequest_OversizeFrame_ReturnsRPCError(t *testing.T) {
	t.Parallel()
	// One byte over the limit — content doesn't matter, the codec
	// rejects at the framing layer before parsing.
	oversize := strings.Repeat("x", maxLineBytes+1) + "\n"
	c := newCodec(strings.NewReader(oversize), io.Discard)

	_, err := c.readRequest()
	require.Error(t, err)

	var rpcErr *rpcError
	require.ErrorAs(t, err, &rpcErr,
		"oversize frame must surface as an rpcError so Serve can respond and continue")
	assert.Equal(t, codeParseError, rpcErr.Code)
	assert.Contains(t, rpcErr.Message, "exceeds",
		"error message should describe the size violation")
}

// TestCodec_ReadRequest_OversizeFrame_RecoversForNextFrame verifies
// that the codec re-aligns on the next frame boundary after rejecting
// an oversize frame. If this test fails, the server will be unable to
// keep serving after a single oversize message — exactly the bug H2
// set out to fix.
func TestCodec_ReadRequest_OversizeFrame_RecoversForNextFrame(t *testing.T) {
	t.Parallel()
	oversize := strings.Repeat("x", maxLineBytes+1) + "\n"
	valid := `{"jsonrpc":"2.0","id":42,"method":"tools/list"}` + "\n"
	c := newCodec(strings.NewReader(oversize+valid), io.Discard)

	// First frame: oversize → rpcError.
	_, err := c.readRequest()
	var rpcErr *rpcError
	require.ErrorAs(t, err, &rpcErr)
	require.Equal(t, codeParseError, rpcErr.Code)

	// Second frame: valid → must parse cleanly. This is the recovery
	// property — the drain step consumed the rest of the oversize
	// frame so ReadSlice is positioned at the start of this line.
	req, err := c.readRequest()
	require.NoError(t, err, "codec must recover and parse the frame following an oversize one")
	assert.Equal(t, "tools/list", req.Method)
	assert.Equal(t, json.RawMessage(`42`), req.ID,
		"the recovered frame's id must match the second frame, not the discarded first")
}

// TestCodec_ReadRequest_DrainCapExceeded_ReturnsFatalError verifies
// that a never-terminating frame (more than one frame-limit plus one
// drain-cap worth of bytes, no newline) returns a plain error, NOT an
// rpcError. Serve treats plain errors as unrecoverable and disconnects
// — which is the intended outcome when a client's framing has gone
// badly enough that we can't confidently re-align on a frame boundary.
//
// Size reasoning: the first ReadSlice in readLine consumes up to
// maxLineBytes before drainLine starts accounting. drainLine then
// runs up to maxDrainBytes more. So the input must exceed
// maxLineBytes + maxDrainBytes for the cap to trip before EOF.
func TestCodec_ReadRequest_DrainCapExceeded_ReturnsFatalError(t *testing.T) {
	t.Parallel()
	noTermination := strings.Repeat("x", maxLineBytes+maxDrainBytes+1)
	c := newCodec(strings.NewReader(noTermination), io.Discard)

	_, err := c.readRequest()
	require.Error(t, err)

	var rpcErr *rpcError
	assert.False(t, errors.As(err, &rpcErr),
		"drain-cap exceeded must NOT be an rpcError — Serve relies on the non-rpcError branch to disconnect")
	assert.Contains(t, err.Error(), "drain cap exceeded")
}

// TestCodec_ReadRequest_EOF_Truncated verifies that a partial frame
// followed by EOF (no trailing newline) surfaces as a parse-error
// rpcError rather than as a clean EOF. This matters because a
// truncated frame is a framing violation, not a normal shutdown —
// the client either crashed mid-write or the stdio pipe was severed,
// and masking that as EOF would hide the failure from audit logs.
func TestCodec_ReadRequest_EOF_Truncated(t *testing.T) {
	t.Parallel()
	// Valid-looking JSON prefix with no trailing newline.
	partial := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	c := newCodec(strings.NewReader(partial), io.Discard)

	_, err := c.readRequest()
	require.Error(t, err)
	assert.NotErrorIs(t, err, io.EOF,
		"truncated frames must not masquerade as clean shutdown")

	var rpcErr *rpcError
	require.ErrorAs(t, err, &rpcErr)
	assert.Equal(t, codeParseError, rpcErr.Code)
	assert.Contains(t, rpcErr.Message, "unexpected EOF")
}
