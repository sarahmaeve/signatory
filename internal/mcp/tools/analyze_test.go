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
	"github.com/sarahmaeve/signatory/internal/store"
)

func openTestStore(t *testing.T) *store.SQLite {
	t.Helper()
	db, err := store.OpenSQLite(context.Background(), t.TempDir()+"/test.db")
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	return db
}

// seedEntity inserts a minimal entity into the store.
func seedEntity(t *testing.T, s store.Store, uri, shortName string) *profile.Entity {
	t.Helper()
	e := &profile.Entity{
		ID:           profile.NewEntityID(),
		CanonicalURI: uri,
		Type:         profile.EntityProject,
		ShortName:    shortName,
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}
	require.NoError(t, s.PutEntity(context.Background(), e))
	return e
}

// seedSignal inserts a single signal for an entity.
func seedSignal(t *testing.T, s store.Store, entityID string) {
	t.Helper()
	sig := profile.Signal{
		ID:                "test-sig-" + entityID[:8],
		EntityID:          entityID,
		Type:              "stars",
		Group:             profile.SignalGroupCriticality,
		Source:            "github",
		ForgeryResistance: profile.ForgeryMediumDeclining,
		Value:             json.RawMessage(`{"count": 42}`),
		CollectedAt:       time.Now().UTC(),
		ExpiresAt:         time.Now().Add(24 * time.Hour).UTC(),
	}
	require.NoError(t, s.AppendSignals(context.Background(), []profile.Signal{sig}))
}

func TestAnalyzeTool_HappyPath(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	e := seedEntity(t, s, "repo:github/acme/myrepo", "acme/myrepo")
	seedSignal(t, s, e.ID)

	tool := &AnalyzeTool{Store: s}
	resp := tool.Handle(context.Background(), json.RawMessage(`{"target":"acme/myrepo"}`))

	require.Equal(t, "ok", resp.Status)
	require.NotNil(t, resp.Data)
	assert.True(t, resp.Metadata.CacheHit)

	data, ok := resp.Data.(analyzeData)
	require.True(t, ok, "expected analyzeData, got %T", resp.Data)
	assert.Equal(t, "repo:github/acme/myrepo", data.Entity.CanonicalURI)
	assert.Equal(t, "acme/myrepo", data.Entity.ShortName)
	assert.NotEmpty(t, data.ForgeryResistance)
	assert.NotNil(t, data.Anomalies)
}

// Mutation check: if we pass the wrong store field (nil), Handle panics or
// returns internal error — not OK. This test would fail if Handle returned
// OK without hitting the store.
func TestAnalyzeTool_HappyPath_RequiresStore(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	e := seedEntity(t, s, "repo:github/acme/myrepo2", "acme/myrepo2")
	seedSignal(t, s, e.ID)

	tool := &AnalyzeTool{Store: s}
	resp := tool.Handle(context.Background(), json.RawMessage(`{"target":"acme/myrepo2"}`))
	// Must be OK — entity and signals exist.
	assert.Equal(t, "ok", resp.Status)

	// Mutation: change target so it won't be found.
	resp2 := tool.Handle(context.Background(), json.RawMessage(`{"target":"acme/nonexistent"}`))
	assert.Equal(t, "error", resp2.Status)
	assert.Equal(t, mcp.CodeCacheMissRequiresRefresh, resp2.Error.Code)
}

func TestAnalyzeTool_SchemaViolation_UnknownField(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	tool := &AnalyzeTool{Store: s}

	resp := tool.Handle(context.Background(), json.RawMessage(`{"target":"acme/foo","unknown_field":true}`))

	assert.Equal(t, "error", resp.Status)
	assert.Equal(t, mcp.CodeSchemaViolation, resp.Error.Code)
}

func TestAnalyzeTool_SchemaViolation_MissingTarget(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	tool := &AnalyzeTool{Store: s}

	resp := tool.Handle(context.Background(), json.RawMessage(`{}`))

	assert.Equal(t, "error", resp.Status)
	assert.Equal(t, mcp.CodeSchemaViolation, resp.Error.Code)
	assert.Contains(t, resp.Error.Message, "target")
}

func TestAnalyzeTool_CacheMiss_NoRefresh(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	tool := &AnalyzeTool{Store: s}

	resp := tool.Handle(context.Background(), json.RawMessage(`{"target":"acme/unknown"}`))

	assert.Equal(t, "error", resp.Status)
	assert.Equal(t, mcp.CodeCacheMissRequiresRefresh, resp.Error.Code)
}

// Mutation test: verify that the not-found check is the thing producing
// CodeCacheMissRequiresRefresh. If we commented out the errors.Is(err,
// store.ErrNotFound) branch, this test would fail (the error code would change).
func TestAnalyzeTool_CacheMiss_MutationCheck(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	tool := &AnalyzeTool{Store: s}

	// Entity does not exist → should be cache miss.
	resp := tool.Handle(context.Background(), json.RawMessage(`{"target":"owner/nonexistent-repo"}`))
	assert.Equal(t, "error", resp.Status, "expected error status for unknown entity")
	// Must be cache-miss, not a generic not-found or internal error.
	assert.Equal(t, mcp.CodeCacheMissRequiresRefresh, resp.Error.Code,
		"cache miss must produce CodeCacheMissRequiresRefresh, not %s", resp.Error.Code)
}

func TestAnalyzeTool_InvalidDepth(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	tool := &AnalyzeTool{Store: s}

	resp := tool.Handle(context.Background(), json.RawMessage(`{"target":"acme/foo","depth":"deep"}`))

	assert.Equal(t, "error", resp.Status)
	assert.Equal(t, mcp.CodeSchemaViolation, resp.Error.Code)
}

func TestAnalyzeTool_EntityExistsNoSignals(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	// Entity exists but no signals seeded.
	seedEntity(t, s, "repo:github/acme/empty", "acme/empty")

	tool := &AnalyzeTool{Store: s}
	resp := tool.Handle(context.Background(), json.RawMessage(`{"target":"acme/empty"}`))

	// No signals → cache miss.
	assert.Equal(t, "error", resp.Status)
	assert.Equal(t, mcp.CodeCacheMissRequiresRefresh, resp.Error.Code)
}

// TestDominantForgeryResistance exercises the helper directly, including the
// unknown-value skip behaviour added in the TDD fix.
func TestDominantForgeryResistance(t *testing.T) {
	t.Parallel()

	const unknownFR = profile.ForgeryResistance("fabricated-unknown")

	cases := []struct {
		name    string
		signals []profile.Signal
		want    string
	}{
		{
			name:    "empty signals",
			signals: []profile.Signal{},
			want:    "",
		},
		{
			name: "all known returns minimum",
			signals: []profile.Signal{
				{ForgeryResistance: profile.ForgeryVeryHigh},
				{ForgeryResistance: profile.ForgeryHigh},
				{ForgeryResistance: profile.ForgeryMediumDeclining},
			},
			want: string(profile.ForgeryMediumDeclining),
		},
		{
			name: "known high plus unknown returns high not empty",
			signals: []profile.Signal{
				{ForgeryResistance: profile.ForgeryHigh},
				{ForgeryResistance: unknownFR},
			},
			want: string(profile.ForgeryHigh),
		},
		{
			name: "all unknown returns empty",
			signals: []profile.Signal{
				{ForgeryResistance: unknownFR},
				{ForgeryResistance: profile.ForgeryResistance("another-unknown")},
			},
			want: "",
		},
		{
			name: "high and low returns low",
			signals: []profile.Signal{
				{ForgeryResistance: profile.ForgeryHigh},
				{ForgeryResistance: profile.ForgeryLowDeclining},
			},
			want: string(profile.ForgeryLowDeclining),
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := dominantForgeryResistance(tc.signals)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestAnalyzeTool_Name(t *testing.T) {
	t.Parallel()
	tool := &AnalyzeTool{}
	assert.Equal(t, "signatory_analyze", tool.Name())
}

func TestAnalyzeTool_InputSchemaValid(t *testing.T) {
	t.Parallel()
	tool := &AnalyzeTool{}
	schema := tool.InputSchema()
	assert.True(t, json.Valid(schema), "InputSchema must be valid JSON")
}
