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

// TestShowAnalysesTool_VanityResolution: pkg:golang/golang.org/x/mod
// must walk to the equivalent repo:github/golang/mod entity via the
// alternate-URI walk in store.LookupEntityID. Pre-2026-05-07 the
// MCP tool sat on a normalize-then-pass path that missed this
// equivalence and returned CodeNotFound for a target that summary
// resolved fine. Regression pin: this test fails if the resolution
// reverts to direct FindEntityByURI.
func TestShowAnalysesTool_VanityResolution(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	// Seed at the canonical github form. The vanity input must
	// walk through pkg:golang/... → pkg:go/... → repo:github/golang/mod
	// to reach this entity.
	seedEntity(t, s, "repo:github/golang/mod", "mod")
	_ = seedAnalystOutput(t, s, "https://github.com/golang/mod")

	tool := &ShowAnalysesTool{Store: s}
	resp := tool.Handle(context.Background(),
		json.RawMessage(`{"target":"golang.org/x/mod"}`))

	require.Equal(t, "ok", resp.Status,
		"vanity input must reach the github form via LookupEntity; "+
			"pre-fix returned CodeNotFound (%+v)", resp.Error)

	data, ok := resp.Data.(map[string]any)
	require.True(t, ok)
	analyses, ok := data["analyses"].([]store.AnalystOutputSummary)
	require.True(t, ok)
	require.Len(t, analyses, 1,
		"the analysis seeded under repo:github/golang/mod must surface for the vanity query")
	assert.Equal(t, "repo:github/golang/mod", analyses[0].EntityURI,
		"surface the canonical entity URI, not the input vanity form")
}

// TestShowAnalysesTool_MalformedInput: input that profile.ResolveTarget
// rejects must surface as CodeSchemaViolation rather than silently
// looking like CodeNotFound. Mirrors the show_synthesis precedent —
// the typed distinction is useful to LLM consumers.
func TestShowAnalysesTool_MalformedInput(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	tool := &ShowAnalysesTool{Store: s}

	resp := tool.Handle(context.Background(),
		json.RawMessage(`{"target":"not a valid uri or shorthand at all !!!"}`))

	require.Equal(t, "error", resp.Status)
	assert.Equal(t, mcp.CodeSchemaViolation, resp.Error.Code,
		"unparseable input is caller-shape, not missing-data: "+
			"distinguish CodeSchemaViolation from CodeNotFound")
}

// Name() is covered by the registration contract test in cmd/signatory.
