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

func TestShowFindingsTool_HappyPath_EmptyList(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	// Entity exists but has no analyst outputs / findings.
	seedEntity(t, s, "repo:github/acme/findtest", "acme/findtest")

	tool := &ShowFindingsTool{Store: s}
	resp := tool.Handle(context.Background(), json.RawMessage(`{"target":"acme/findtest"}`))

	require.Equal(t, "ok", resp.Status)
	data, ok := resp.Data.(map[string]interface{})
	require.True(t, ok)
	findings, ok := data["findings"].([]store.FindingSummary)
	require.True(t, ok)
	assert.Empty(t, findings)
}

func TestShowFindingsTool_NotFound(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	tool := &ShowFindingsTool{Store: s}

	resp := tool.Handle(context.Background(), json.RawMessage(`{"target":"acme/ghost"}`))

	assert.Equal(t, "error", resp.Status)
	assert.Equal(t, mcp.CodeNotFound, resp.Error.Code)
}

// Mutation check: if the not-found gate returned OK with empty list, this test
// would fail. ErrNotFound must map to CodeNotFound.
func TestShowFindingsTool_NotFound_MutationCheck(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	tool := &ShowFindingsTool{Store: s}

	resp := tool.Handle(context.Background(), json.RawMessage(`{"target":"repo:github/never/existed"}`))
	assert.Equal(t, "error", resp.Status)
	assert.Equal(t, mcp.CodeNotFound, resp.Error.Code,
		"unknown entity in FindingFilter must produce CodeNotFound, not %s", resp.Error.Code)
}

func TestShowFindingsTool_SchemaViolation_UnknownField(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	tool := &ShowFindingsTool{Store: s}

	resp := tool.Handle(context.Background(), json.RawMessage(`{"target":"acme/foo","nope":1}`))

	assert.Equal(t, "error", resp.Status)
	assert.Equal(t, mcp.CodeSchemaViolation, resp.Error.Code)
}

func TestShowFindingsTool_InvalidSeverity(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	tool := &ShowFindingsTool{Store: s}

	resp := tool.Handle(context.Background(), json.RawMessage(`{"severity":["catastrophic"]}`))

	assert.Equal(t, "error", resp.Status)
	assert.Equal(t, mcp.CodeSchemaViolation, resp.Error.Code)
	assert.Contains(t, resp.Error.Message, "catastrophic")
}

func TestShowFindingsTool_ValidSeverityFilter(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	tool := &ShowFindingsTool{Store: s}

	// All valid severities — should not produce a schema error.
	resp := tool.Handle(context.Background(), json.RawMessage(
		`{"severity":["critical","high","medium","low","informational","positive"]}`,
	))
	// No target → OK with empty list (store has no findings).
	assert.Equal(t, "ok", resp.Status)
}

func TestShowFindingsTool_DesignIntent(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	seedEntity(t, s, "repo:github/acme/ditest", "acme/ditest")

	tool := &ShowFindingsTool{Store: s}
	resp := tool.Handle(context.Background(), json.RawMessage(`{"target":"acme/ditest","design_intent":true}`))

	require.Equal(t, "ok", resp.Status)
	data, ok := resp.Data.(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, 0, data["count"])
}

func TestShowFindingsTool_Name(t *testing.T) {
	t.Parallel()
	tool := &ShowFindingsTool{}
	assert.Equal(t, "signatory_show_findings", tool.Name())
}
