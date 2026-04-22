package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/mcp"
	"github.com/sarahmaeve/signatory/internal/store"
)

func TestShowConclusionsTool_HappyPath_EmptyList(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	// Entity exists but has no analyst outputs / conclusions.
	seedEntity(t, s, "repo:github/acme/findtest", "acme/findtest")

	tool := &ShowConclusionsTool{Store: s}
	resp := tool.Handle(context.Background(), json.RawMessage(`{"target":"acme/findtest"}`))

	require.Equal(t, "ok", resp.Status)
	data, ok := resp.Data.(map[string]any)
	require.True(t, ok)
	conclusions, ok := data["conclusions"].([]store.ConclusionSummary)
	require.True(t, ok)
	assert.Empty(t, conclusions)
}

func TestShowConclusionsTool_NotFound(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	tool := &ShowConclusionsTool{Store: s}

	resp := tool.Handle(context.Background(), json.RawMessage(`{"target":"acme/ghost"}`))

	assert.Equal(t, "error", resp.Status)
	assert.Equal(t, mcp.CodeNotFound, resp.Error.Code)
}

// Mutation check: if the not-found gate returned OK with empty list, this test
// would fail. ErrNotFound must map to CodeNotFound.
func TestShowConclusionsTool_NotFound_MutationCheck(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	tool := &ShowConclusionsTool{Store: s}

	resp := tool.Handle(context.Background(), json.RawMessage(`{"target":"repo:github/never/existed"}`))
	assert.Equal(t, "error", resp.Status)
	assert.Equal(t, mcp.CodeNotFound, resp.Error.Code,
		"unknown entity in ConclusionFilter must produce CodeNotFound, not %s", resp.Error.Code)
}

func TestShowConclusionsTool_SchemaViolation_UnknownField(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	tool := &ShowConclusionsTool{Store: s}

	resp := tool.Handle(context.Background(), json.RawMessage(`{"target":"acme/foo","nope":1}`))

	assert.Equal(t, "error", resp.Status)
	assert.Equal(t, mcp.CodeSchemaViolation, resp.Error.Code)
}

func TestShowConclusionsTool_InvalidSeverity(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	tool := &ShowConclusionsTool{Store: s}

	resp := tool.Handle(context.Background(), json.RawMessage(`{"severity":["catastrophic"]}`))

	assert.Equal(t, "error", resp.Status)
	assert.Equal(t, mcp.CodeSchemaViolation, resp.Error.Code)
	assert.Contains(t, resp.Error.Message, "catastrophic")
}

func TestShowConclusionsTool_ValidSeverityFilter(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	tool := &ShowConclusionsTool{Store: s}

	// All valid severities — should not produce a schema error.
	resp := tool.Handle(context.Background(), json.RawMessage(
		`{"severity":["critical","high","medium","low","informational","positive"]}`,
	))
	// No target → OK with empty list (store has no conclusions).
	assert.Equal(t, "ok", resp.Status)
}

func TestShowConclusionsTool_DesignIntent(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	seedEntity(t, s, "repo:github/acme/ditest", "acme/ditest")

	tool := &ShowConclusionsTool{Store: s}
	resp := tool.Handle(context.Background(), json.RawMessage(`{"target":"acme/ditest","design_intent":true}`))

	require.Equal(t, "ok", resp.Status)
	data, ok := resp.Data.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, 0, data["count"])
}

func TestShowConclusionsTool_Name(t *testing.T) {
	t.Parallel()
	tool := &ShowConclusionsTool{}
	assert.Equal(t, "signatory_show_conclusions", tool.Name())
}
