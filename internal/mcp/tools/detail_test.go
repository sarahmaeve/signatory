package tools

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/mcp"
	"github.com/sarahmaeve/signatory/internal/profile"
)

func TestDetailTool_HappyPath(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	e := seedEntity(t, s, "repo:github/acme/detailtest", "acme/detailtest")
	seedSignal(t, s, e.ID) // seeds a criticality signal

	tool := &DetailTool{Store: s}
	resp := tool.Handle(context.Background(), json.RawMessage(`{"target":"acme/detailtest","signal_group":"criticality"}`))

	require.Equal(t, "ok", resp.Status)
	data, ok := resp.Data.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "criticality", data["signal_group"])
	sigs, ok := data["signals"].([]signalRecord)
	require.True(t, ok)
	assert.Len(t, sigs, 1)
}

func TestDetailTool_InvalidSignalGroup(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	seedEntity(t, s, "repo:github/acme/detailtest2", "acme/detailtest2")

	tool := &DetailTool{Store: s}
	resp := tool.Handle(context.Background(), json.RawMessage(`{"target":"acme/detailtest2","signal_group":"fakegroup"}`))

	assert.Equal(t, "error", resp.Status)
	assert.Equal(t, mcp.CodeSchemaViolation, resp.Error.Code)
	assert.Contains(t, resp.Error.Message, "signal_group")
}

// Mutation check: if the signal_group enum validation were removed, an
// invalid group would not return schema_violation — it would either silently
// return empty results or panic. This test verifies the gate is active.
func TestDetailTool_InvalidSignalGroup_MutationCheck(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	tool := &DetailTool{Store: s}

	resp := tool.Handle(context.Background(), json.RawMessage(`{"target":"acme/anything","signal_group":"nonexistent"}`))
	// The enum validation must fire before any store lookup.
	assert.Equal(t, "error", resp.Status)
	assert.Equal(t, mcp.CodeSchemaViolation, resp.Error.Code,
		"invalid signal_group must produce CodeSchemaViolation, not %s", resp.Error.Code)
}

func TestDetailTool_NotFound(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	tool := &DetailTool{Store: s}

	resp := tool.Handle(context.Background(), json.RawMessage(`{"target":"acme/missing","signal_group":"vitality"}`))

	assert.Equal(t, "error", resp.Status)
	assert.Equal(t, mcp.CodeNotFound, resp.Error.Code)
}

func TestDetailTool_SchemaViolation_UnknownField(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	tool := &DetailTool{Store: s}

	resp := tool.Handle(context.Background(), json.RawMessage(`{"target":"acme/foo","signal_group":"vitality","extra":true}`))

	assert.Equal(t, "error", resp.Status)
	assert.Equal(t, mcp.CodeSchemaViolation, resp.Error.Code)
}

func TestDetailTool_EmptyGroup(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	// Entity with only a criticality signal — querying vitality returns empty.
	e := seedEntity(t, s, "repo:github/acme/detailempty", "acme/detailempty")
	seedSignal(t, s, e.ID)

	tool := &DetailTool{Store: s}
	resp := tool.Handle(context.Background(), json.RawMessage(`{"target":"acme/detailempty","signal_group":"vitality"}`))

	require.Equal(t, "ok", resp.Status)
	data, ok := resp.Data.(map[string]any)
	require.True(t, ok)
	sigs, ok := data["signals"].([]signalRecord)
	require.True(t, ok)
	assert.Empty(t, sigs)
}

func TestDetailTool_AllValidGroups(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	e := seedEntity(t, s, "repo:github/acme/groupstest", "acme/groupstest")
	// Seed signals for several groups.
	for _, g := range []profile.SignalGroup{
		profile.SignalGroupVitality,
		profile.SignalGroupGovernance,
		profile.SignalGroupHygiene,
	} {
		now := time.Now().UTC()
		sig := profile.Signal{
			ID:                "sig-" + string(g) + "-" + e.ID[:6],
			EntityID:          e.ID,
			Type:              string(g) + "_metric",
			Group:             g,
			Source:            "test",
			ForgeryResistance: profile.ForgeryHigh,
			Value:             json.RawMessage(`{}`),
			CollectedAt:       now,
			ExpiresAt:         now.Add(24 * time.Hour),
		}
		require.NoError(t, s.AppendSignals(context.Background(), []profile.Signal{sig}))
	}

	tool := &DetailTool{Store: s}
	for _, grp := range []string{"vitality", "governance", "publication", "hygiene", "criticality", "history"} {
		resp := tool.Handle(context.Background(), json.RawMessage(`{"target":"acme/groupstest","signal_group":"`+grp+`"}`))
		assert.Equal(t, "ok", resp.Status, "group %s should return ok", grp)
	}
}

// TestDetailTool_CodebergURL_ResolvesToCanonical pins the multi-forge
// generalization for detail. Same rationale as
// TestSignalsTool_CodebergURL_ResolvesToCanonical — a codeberg URL
// reaching this tool used to misclassify through
// NormalizeGitHubRepoInput as repo:github/codeberg.org/forgejo and
// miss the actual entity at repo:codeberg/forgejo/forgejo.
func TestDetailTool_CodebergURL_ResolvesToCanonical(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	e := seedEntity(t, s, "repo:codeberg/forgejo/forgejo", "forgejo/forgejo")
	sig := profile.Signal{
		ID:                "test-detail-cb-" + e.ID[:8],
		EntityID:          e.ID,
		Type:              "stars",
		Group:             profile.SignalGroupCriticality,
		Source:            "github",
		ForgeryResistance: profile.ForgeryMediumDeclining,
		Value:             json.RawMessage(`{"count": 42}`),
		CollectedAt:       time.Now().UTC(),
		ExpiresAt:         time.Now().Add(24 * time.Hour).UTC(),
	}
	require.NoError(t, s.AppendSignals(context.Background(), []profile.Signal{sig}))

	tool := &DetailTool{Store: s}
	resp := tool.Handle(context.Background(),
		json.RawMessage(`{"target":"https://codeberg.org/forgejo/forgejo","signal_group":"criticality"}`))

	require.Equal(t, "ok", resp.Status,
		"codeberg URL must resolve to repo:codeberg/forgejo/forgejo, not the buggy NormalizeGitHubRepoInput rendering")
}

// TestDetailTool_MalformedTarget_SchemaViolation — same contract shift
// as TestSignalsTool_MalformedTarget_SchemaViolation. After the
// ResolveTarget switch, unparseable input is a schema violation rather
// than a confusing CodeNotFound on a synthesized-from-garbage URI.
func TestDetailTool_MalformedTarget_SchemaViolation(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	tool := &DetailTool{Store: s}

	resp := tool.Handle(context.Background(),
		json.RawMessage(`{"target":"not a valid target","signal_group":"criticality"}`))
	assert.Equal(t, "error", resp.Status)
	assert.Equal(t, mcp.CodeSchemaViolation, resp.Error.Code)
}
