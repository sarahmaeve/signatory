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
	data, ok := resp.Data.(map[string]any)
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
	data, ok := resp.Data.(map[string]any)
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

// TestSignalsTool_CodebergURL_ResolvesToCanonical pins the multi-forge
// generalization of the target normalizer. Pre-fix, the tool ran
// profile.NormalizeGitHubRepoInput on the input — that function's
// permissive prefix-strip ate the "https://" and produced
// owner="codeberg.org", name="forgejo", canonical URI
// "repo:github/codeberg.org/forgejo". The downstream FindEntityByURI
// then missed the codeberg entity (stored at the correct
// "repo:codeberg/forgejo/forgejo") and returned CodeNotFound — even
// when the entity existed. Same misclassification ResolveTarget's
// rejectUnrecognizedForgeURL gate already prevents on the analyze path.
//
// Post-fix, the tool uses profile.ResolveTarget which dispatches off
// the forgeHostPrefix table and produces the correct canonical URI;
// the lookup hits the seeded entity and the response is OK.
func TestSignalsTool_CodebergURL_ResolvesToCanonical(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	e := seedEntity(t, s, "repo:codeberg/forgejo/forgejo", "forgejo/forgejo")
	seedSignal(t, s, e.ID)

	tool := &SignalsTool{Store: s}
	resp := tool.Handle(context.Background(), json.RawMessage(`{"target":"https://codeberg.org/forgejo/forgejo"}`))

	require.Equal(t, "ok", resp.Status,
		"codeberg URL must resolve to the canonical codeberg URI; old NormalizeGitHubRepoInput path produced repo:github/codeberg.org/forgejo and missed the entity")
	data, ok := resp.Data.(map[string]any)
	require.True(t, ok)
	signals, ok := data["signals"].([]signalRecord)
	require.True(t, ok)
	assert.Len(t, signals, 1)
}

// TestSignalsTool_GitLabURL_ResolvesToCanonical — same shape for
// GitLab. A second forge proves the fix is generic, not codeberg-only.
func TestSignalsTool_GitLabURL_ResolvesToCanonical(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	e := seedEntity(t, s, "repo:gitlab/gitlab-org/gitlab", "gitlab-org/gitlab")
	seedSignal(t, s, e.ID)

	tool := &SignalsTool{Store: s}
	resp := tool.Handle(context.Background(), json.RawMessage(`{"target":"https://gitlab.com/gitlab-org/gitlab"}`))

	require.Equal(t, "ok", resp.Status)
	data, ok := resp.Data.(map[string]any)
	require.True(t, ok)
	signals, ok := data["signals"].([]signalRecord)
	require.True(t, ok)
	assert.Len(t, signals, 1)
}

// TestSignalsTool_MalformedTarget_SchemaViolation pins the contract
// shift the ResolveTarget switch introduces: input that doesn't
// resolve as ANY known canonical URI form (registry URL, forge
// shorthand, vanity import path, scheme-prefixed canonical URI)
// returns CodeSchemaViolation, not CodeNotFound. Pre-fix the
// NormalizeGitHubRepoInput failure fell through to "use raw target
// as canonical URI" and the lookup miss produced CodeNotFound —
// conflating malformed-input with absent-data. Matches summary tool's
// existing precedent (summary.go:64-68).
func TestSignalsTool_MalformedTarget_SchemaViolation(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	tool := &SignalsTool{Store: s}

	resp := tool.Handle(context.Background(), json.RawMessage(`{"target":"not a valid target"}`))
	assert.Equal(t, "error", resp.Status)
	assert.Equal(t, mcp.CodeSchemaViolation, resp.Error.Code,
		"unparseable target must classify as schema violation so the LLM caller knows the input shape was wrong, not the data was missing")
}
