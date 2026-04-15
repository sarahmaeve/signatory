package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/mcp"
)

func TestSignalsTool_HappyPath(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	e := seedEntity(t, s, "repo:github/acme/sigtest", "acme/sigtest")
	seedSignal(t, s, e.ID)

	tool := &SignalsTool{Store: s}
	resp := tool.Handle(context.Background(), json.RawMessage(`{"target":"acme/sigtest"}`))

	require.Equal(t, "ok", resp.Status)
	data, ok := resp.Data.(map[string]interface{})
	require.True(t, ok)
	signals, ok := data["signals"].([]signalRecord)
	require.True(t, ok)
	assert.Len(t, signals, 1)
	assert.Equal(t, "stars", signals[0].Type)
	assert.Equal(t, "criticality", signals[0].Group)
}

func TestSignalsTool_SchemaViolation_UnknownField(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	tool := &SignalsTool{Store: s}

	resp := tool.Handle(context.Background(), json.RawMessage(`{"target":"acme/foo","bogus":1}`))

	assert.Equal(t, "error", resp.Status)
	assert.Equal(t, mcp.CodeSchemaViolation, resp.Error.Code)
}

func TestSignalsTool_NotFound(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	tool := &SignalsTool{Store: s}

	resp := tool.Handle(context.Background(), json.RawMessage(`{"target":"acme/ghost"}`))

	assert.Equal(t, "error", resp.Status)
	assert.Equal(t, mcp.CodeNotFound, resp.Error.Code)
}

// Mutation check: if errors.Is(err, store.ErrNotFound) was replaced with nil,
// the code would return internal_error or panic instead of not_found.
// This test ensures the not_found path is exercised correctly.
func TestSignalsTool_NotFound_MutationCheck(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	tool := &SignalsTool{Store: s}

	resp := tool.Handle(context.Background(), json.RawMessage(`{"target":"owner/definitely-not-there"}`))
	assert.Equal(t, "error", resp.Status)
	// Must be not_found, not schema_violation or internal_error.
	assert.Equal(t, mcp.CodeNotFound, resp.Error.Code,
		"entity not found must produce CodeNotFound, not %s", resp.Error.Code)
}

func TestSignalsTool_EmptySignals(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	// Entity exists but no signals.
	seedEntity(t, s, "repo:github/acme/nosigs", "acme/nosigs")

	tool := &SignalsTool{Store: s}
	resp := tool.Handle(context.Background(), json.RawMessage(`{"target":"acme/nosigs"}`))

	// Should be OK with empty slice.
	require.Equal(t, "ok", resp.Status)
	data, ok := resp.Data.(map[string]interface{})
	require.True(t, ok)
	signals, ok := data["signals"].([]signalRecord)
	require.True(t, ok)
	assert.Empty(t, signals)
}

func TestSignalsTool_MissingTarget(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	tool := &SignalsTool{Store: s}

	resp := tool.Handle(context.Background(), json.RawMessage(`{}`))
	assert.Equal(t, "error", resp.Status)
	assert.Equal(t, mcp.CodeSchemaViolation, resp.Error.Code)
}
