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

func TestShowMethodologyTool_HappyPath_EmptyList(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	// Entity exists but no analyst outputs / patterns.
	seedEntity(t, s, "repo:github/acme/methtest", "acme/methtest")

	tool := &ShowMethodologyTool{Store: s}
	resp := tool.Handle(context.Background(), json.RawMessage(`{"target":"acme/methtest"}`))

	require.Equal(t, "ok", resp.Status)
	data, ok := resp.Data.(map[string]any)
	require.True(t, ok)
	patterns, ok := data["patterns"].([]store.MethodologyPatternSummary)
	require.True(t, ok)
	assert.Empty(t, patterns)
}

func TestShowMethodologyTool_NotFound(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	tool := &ShowMethodologyTool{Store: s}

	resp := tool.Handle(context.Background(), json.RawMessage(`{"target":"acme/ghost"}`))

	assert.Equal(t, "error", resp.Status)
	assert.Equal(t, mcp.CodeNotFound, resp.Error.Code)
}

// Mutation check: ErrNotFound from the store must produce CodeNotFound, not OK.
func TestShowMethodologyTool_NotFound_MutationCheck(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	tool := &ShowMethodologyTool{Store: s}

	resp := tool.Handle(context.Background(), json.RawMessage(`{"target":"repo:github/no/entity"}`))
	assert.Equal(t, "error", resp.Status)
	assert.Equal(t, mcp.CodeNotFound, resp.Error.Code,
		"unknown entity must return CodeNotFound, not %s", resp.Error.Code)
}

func TestShowMethodologyTool_SchemaViolation_UnknownField(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	tool := &ShowMethodologyTool{Store: s}

	resp := tool.Handle(context.Background(), json.RawMessage(`{"target":"acme/foo","extra":1}`))

	assert.Equal(t, "error", resp.Status)
	assert.Equal(t, mcp.CodeSchemaViolation, resp.Error.Code)
}

func TestShowMethodologyTool_InvalidHitOnTarget(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	tool := &ShowMethodologyTool{Store: s}

	resp := tool.Handle(context.Background(), json.RawMessage(`{"hit_on_target":"maybe"}`))

	assert.Equal(t, "error", resp.Status)
	assert.Equal(t, mcp.CodeSchemaViolation, resp.Error.Code)
	assert.Contains(t, resp.Error.Message, "hit_on_target")
}

// Mutation check: if the hit_on_target enum check were removed, "maybe" would
// pass through and the store would return results ignoring the filter. This
// test verifies the strict-reject gate is active.
func TestShowMethodologyTool_InvalidHitOnTarget_MutationCheck(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	tool := &ShowMethodologyTool{Store: s}

	resp := tool.Handle(context.Background(), json.RawMessage(`{"hit_on_target":"bogus"}`))
	assert.Equal(t, "error", resp.Status,
		"invalid hit_on_target must produce an error")
	assert.Equal(t, mcp.CodeSchemaViolation, resp.Error.Code,
		"invalid hit_on_target must produce CodeSchemaViolation, not %s", resp.Error.Code)
}

func TestShowMethodologyTool_ValidHitOnTarget_Hit(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	tool := &ShowMethodologyTool{Store: s}

	resp := tool.Handle(context.Background(), json.RawMessage(`{"hit_on_target":"hit"}`))
	assert.Equal(t, "ok", resp.Status)
}

func TestShowMethodologyTool_ValidHitOnTarget_Miss(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	tool := &ShowMethodologyTool{Store: s}

	resp := tool.Handle(context.Background(), json.RawMessage(`{"hit_on_target":"miss"}`))
	assert.Equal(t, "ok", resp.Status)
}

// Name() is covered by the registration contract test in cmd/signatory.
