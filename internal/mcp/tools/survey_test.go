package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/sarahmaeve/signatory/internal/mcp"
)

func TestSurveyTool_EmptyManifestPath(t *testing.T) {
	t.Parallel()
	tool := &SurveyTool{}

	resp := tool.Handle(context.Background(), json.RawMessage(`{}`))

	assert.Equal(t, "error", resp.Status)
	assert.Equal(t, mcp.CodeSchemaViolation, resp.Error.Code)
	assert.Contains(t, resp.Error.Message, "manifest_path")
}

// Mutation check: if the empty manifest_path check returned OK instead of an
// error, this test would fail — the contract is that v0.1 requires the path.
func TestSurveyTool_EmptyManifestPath_MutationCheck(t *testing.T) {
	t.Parallel()
	tool := &SurveyTool{}

	resp := tool.Handle(context.Background(), json.RawMessage(`{}`))
	assert.Equal(t, "error", resp.Status,
		"empty manifest_path must produce an error, not ok")
	assert.Equal(t, mcp.CodeSchemaViolation, resp.Error.Code,
		"empty manifest_path must produce CodeSchemaViolation, not %s", resp.Error.Code)
}

func TestSurveyTool_SchemaViolation_UnknownField(t *testing.T) {
	t.Parallel()
	tool := &SurveyTool{}

	resp := tool.Handle(context.Background(), json.RawMessage(`{"manifest_path":"go.mod","unknown":true}`))

	assert.Equal(t, "error", resp.Status)
	assert.Equal(t, mcp.CodeSchemaViolation, resp.Error.Code)
}

func TestSurveyTool_GoMod_NotImplemented(t *testing.T) {
	t.Parallel()
	tool := &SurveyTool{}

	resp := tool.Handle(context.Background(), json.RawMessage(`{"manifest_path":"/app/go.mod"}`))

	assert.Equal(t, "error", resp.Status)
	assert.Equal(t, mcp.CodeNotFound, resp.Error.Code)
	assert.Contains(t, resp.Error.Message, "go")
}

func TestSurveyTool_CargoToml_NotImplemented(t *testing.T) {
	t.Parallel()
	tool := &SurveyTool{}

	resp := tool.Handle(context.Background(), json.RawMessage(`{"manifest_path":"/app/Cargo.toml"}`))

	assert.Equal(t, "error", resp.Status)
	assert.Equal(t, mcp.CodeNotFound, resp.Error.Code)
	assert.Contains(t, resp.Error.Message, "not yet implemented")
}

func TestSurveyTool_PackageJSON_NotImplemented(t *testing.T) {
	t.Parallel()
	tool := &SurveyTool{}

	resp := tool.Handle(context.Background(), json.RawMessage(`{"manifest_path":"./package.json"}`))

	assert.Equal(t, "error", resp.Status)
	assert.Equal(t, mcp.CodeNotFound, resp.Error.Code)
}

func TestSurveyTool_Name(t *testing.T) {
	t.Parallel()
	tool := &SurveyTool{}
	assert.Equal(t, "signatory_survey", tool.Name())
}

func TestSurveyTool_InputSchemaValid(t *testing.T) {
	t.Parallel()
	tool := &SurveyTool{}
	assert.True(t, json.Valid(tool.InputSchema()))
}
