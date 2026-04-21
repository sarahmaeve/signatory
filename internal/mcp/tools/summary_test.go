package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/mcp"
	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/summary"
)

// TestSummaryTool_HappyPath exercises the tool end-to-end against a
// real *store.SQLite seeded with an entity and one posture. The
// returned payload must deserialize into summary.Summary with the
// populated fields surfaced correctly.
func TestSummaryTool_HappyPath(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	e := seedEntity(t, s, "pkg:npm/express", "express")
	require.NoError(t, s.SetPosture(context.Background(), &profile.Posture{
		EntityID:  e.ID,
		Tier:      profile.PostureTier("trusted-for-now"),
		Rationale: "audited 2026-04-21",
		SetBy:     "team:test",
	}))

	tool := &SummaryTool{Store: s}
	resp := tool.Handle(context.Background(), json.RawMessage(`{"target":"pkg:npm/express"}`))
	require.Equal(t, "ok", resp.Status)

	// MCP serializes Data through json.Marshal later; here we have
	// the concrete *summary.Summary in resp.Data.
	got, ok := resp.Data.(*summary.Summary)
	require.True(t, ok, "response data must be *summary.Summary, got %T", resp.Data)
	assert.Equal(t, "pkg:npm/express", got.CanonicalURI)
	assert.Equal(t, "express", got.ShortName)
	require.NotNil(t, got.Posture)
	assert.Equal(t, "trusted-for-now", got.Posture.Tier)
}

// TestSummaryTool_NotFound verifies that unknown targets surface
// CodeNotFound with the canonical URI in the error metadata. The
// error message echoes the URI so callers see which target was
// missing.
func TestSummaryTool_NotFound(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	tool := &SummaryTool{Store: s}
	resp := tool.Handle(context.Background(), json.RawMessage(`{"target":"pkg:npm/doesnotexist"}`))
	require.Equal(t, "error", resp.Status)
	require.NotNil(t, resp.Error)
	assert.Equal(t, mcp.CodeNotFound, resp.Error.Code)
	assert.Contains(t, resp.Error.Message, "pkg:npm/doesnotexist")
}

// TestSummaryTool_RejectsMissingTarget enforces the input-schema
// contract: target is required.
func TestSummaryTool_RejectsMissingTarget(t *testing.T) {
	t.Parallel()

	tool := &SummaryTool{}
	resp := tool.Handle(context.Background(), json.RawMessage(`{}`))
	require.Equal(t, "error", resp.Status)
	require.NotNil(t, resp.Error)
	assert.Equal(t, mcp.CodeSchemaViolation, resp.Error.Code)
}

// TestSummaryTool_RejectsUnknownFields verifies strict-decode on
// unknown fields — matches the convention the other tools use so
// typos surface immediately rather than being silently dropped.
func TestSummaryTool_RejectsUnknownFields(t *testing.T) {
	t.Parallel()

	tool := &SummaryTool{}
	resp := tool.Handle(context.Background(),
		json.RawMessage(`{"target":"pkg:npm/x","misspelled_field":true}`))
	require.Equal(t, "error", resp.Status)
	assert.Equal(t, mcp.CodeSchemaViolation, resp.Error.Code)
}
