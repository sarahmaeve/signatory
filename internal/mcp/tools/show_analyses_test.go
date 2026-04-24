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

func TestShowAnalysesTool_HappyPath_EmptyList(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	// Entity exists but has no analyst outputs.
	seedEntity(t, s, "repo:github/acme/showtest", "acme/showtest")

	tool := &ShowAnalysesTool{Store: s}
	resp := tool.Handle(context.Background(), json.RawMessage(`{"target":"acme/showtest"}`))

	require.Equal(t, "ok", resp.Status)
	data, ok := resp.Data.(map[string]any)
	require.True(t, ok)
	analyses, ok := data["analyses"].([]store.AnalystOutputSummary)
	require.True(t, ok)
	assert.Empty(t, analyses)
	assert.Equal(t, 0, data["count"])
}

func TestShowAnalysesTool_NotFound_TargetUnknown(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	tool := &ShowAnalysesTool{Store: s}

	resp := tool.Handle(context.Background(), json.RawMessage(`{"target":"acme/ghost"}`))

	assert.Equal(t, "error", resp.Status)
	assert.Equal(t, mcp.CodeNotFound, resp.Error.Code)
}

// Mutation check: if errors.Is(err, store.ErrNotFound) returned OK with empty
// list instead of CodeNotFound, this test would fail. The distinction between
// "target doesn't exist" and "target has no analyses" is semantically important.
func TestShowAnalysesTool_NotFound_MutationCheck(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	tool := &ShowAnalysesTool{Store: s}

	// A repo URI that has never been ingested.
	resp := tool.Handle(context.Background(), json.RawMessage(`{"target":"repo:github/nobody/nothing"}`))
	assert.Equal(t, "error", resp.Status,
		"unknown entity must return error, not ok")
	assert.Equal(t, mcp.CodeNotFound, resp.Error.Code,
		"unknown entity must return CodeNotFound, not %s", resp.Error.Code)
}

func TestShowAnalysesTool_SchemaViolation_UnknownField(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	tool := &ShowAnalysesTool{Store: s}

	resp := tool.Handle(context.Background(), json.RawMessage(`{"target":"acme/foo","bad_field":"x"}`))

	assert.Equal(t, "error", resp.Status)
	assert.Equal(t, mcp.CodeSchemaViolation, resp.Error.Code)
}

func TestShowAnalysesTool_SchemaViolation_BadSince(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	tool := &ShowAnalysesTool{Store: s}

	resp := tool.Handle(context.Background(), json.RawMessage(`{"since":"not-a-date"}`))

	assert.Equal(t, "error", resp.Status)
	assert.Equal(t, mcp.CodeSchemaViolation, resp.Error.Code)
	assert.Contains(t, resp.Error.Message, "since")
}

func TestShowAnalysesTool_NoTarget_EmptyStore(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	tool := &ShowAnalysesTool{Store: s}

	// No target — should return OK with empty list.
	resp := tool.Handle(context.Background(), json.RawMessage(`{}`))

	require.Equal(t, "ok", resp.Status)
	data, ok := resp.Data.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, 0, data["count"])
}

func TestShowAnalysesTool_DefaultLimit(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	tool := &ShowAnalysesTool{Store: s}

	// No limit specified → should use default of 20 without error.
	resp := tool.Handle(context.Background(), json.RawMessage(`{}`))
	assert.Equal(t, "ok", resp.Status)
}

// Name() is covered by the registration contract test in cmd/signatory.
