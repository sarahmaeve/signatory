package tools

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/exchange"
	"github.com/sarahmaeve/signatory/internal/mcp"
	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/store"
)

// seedSynthesis ingests a synthesis AnalystOutput against an entity
// and returns the new row's output_id. Tests use this to populate
// the store with deterministic synthesis content before driving the
// tool. The session linkage required by ingest is built inline so
// callers don't have to thread session IDs separately.
func seedSynthesis(t *testing.T, s *store.SQLite, target, summary string) (outputID, entityID string) {
	t.Helper()
	ctx := context.Background()

	// Create the entity if it doesn't exist (PutEntity is idempotent
	// on canonical_uri; if a prior call seeded the same URI, this is
	// effectively a no-op).
	e, err := s.FindEntityByURI(ctx, target)
	if err != nil {
		e = &profile.Entity{
			ID:           profile.NewEntityID(),
			CanonicalURI: target,
			Type:         profile.EntityProject,
			ShortName:    "test-entity",
		}
		require.NoError(t, s.PutEntity(ctx, e))
	}

	// Synthesis ingest requires an analysis_session linkage post-Bug-1.
	sess := &profile.AnalysisSession{
		ID:        profile.NewEntityID(),
		EntityID:  e.ID,
		TargetURI: target,
		InvokedBy: "test",
		StartedAt: timeNowUTC(),
		Status:    profile.AnalysisSessionInProgress,
	}
	require.NoError(t, s.CreateAnalysisSession(ctx, sess))

	out := &exchange.AnalystOutput{
		Attribution: exchange.AgentAttribution{
			AnalystID: "signatory-synthesis-v1",
			// Model and InvokedAt server-stamped at ingest.
		},
		Target: target,
		SynthesisSupplement: &exchange.SynthesisSupplement{
			ProposedPosture: exchange.ProposedPosture{
				Tier:             exchange.ProposedTierTrustedForNow,
				RationaleSummary: "test rationale: " + summary,
			},
			Reasoning: "test reasoning paragraph",
			Summary:   summary,
		},
	}
	res, err := s.IngestAnalystOutput(ctx, out, "test-source",
		store.WithAnalysisSession(sess.ID))
	require.NoError(t, err)
	return res.OutputID, e.ID
}

// seedAnalystOutput ingests a non-synthesis (Layer-2) analyst output
// for "this output_id is a Layer-2 row" tests.
func seedAnalystOutput(t *testing.T, s *store.SQLite, target string) string {
	t.Helper()
	out := &exchange.AnalystOutput{
		Attribution: exchange.AgentAttribution{
			AnalystID: "signatory-security-v1",
			// Model and InvokedAt server-stamped at ingest.
		},
		Target: target,
		Conclusions: []exchange.Conclusion{
			{
				ID: "F001", Verdict: "ok", Rationale: "fine",
				Severity: exchange.Severity{Default: exchange.SeverityLow},
				Category: "category",
				Citations: []exchange.Citation{
					{Path: "src/x.go", LineStart: ptrIntForTest(1)},
				},
			},
		},
	}
	res, err := s.IngestAnalystOutput(context.Background(), out, "test-source")
	require.NoError(t, err)
	return res.OutputID
}

// timeNowUTC returns time.Now().UTC() — small wrapper so test fixtures
// don't sprinkle the import + the .UTC() call. Defined here to avoid
// a name collision with the package-level timeNow that the tools
// package may grow later.
func timeNowUTC() time.Time { return time.Now().UTC() }

// ptrIntForTest returns a *int holding v. Local to this file so it
// doesn't collide with similarly-named helpers elsewhere.
func ptrIntForTest(v int) *int { return &v }

// TestShowSynthesisTool_ByOutputID_HappyPath: pass output_id of an
// ingested synthesis row, get the response with Supplement populated.
// Most direct path through the tool.
func TestShowSynthesisTool_ByOutputID_HappyPath(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	outputID, _ := seedSynthesis(t, s, "pkg:npm/by-id-test", "happy-path summary")

	tool := &ShowSynthesisTool{Store: s}
	resp := tool.Handle(context.Background(),
		json.RawMessage(`{"output_id":"`+outputID+`"}`))
	require.Equal(t, "ok", resp.Status, "happy path must return ok, got %+v", resp.Error)

	got, ok := resp.Data.(*ShowSynthesisResponse)
	require.True(t, ok, "response data must be *ShowSynthesisResponse, got %T", resp.Data)
	assert.Equal(t, outputID, got.OutputID)
	assert.Equal(t, "pkg:npm/by-id-test", got.Entity.CanonicalURI)
	require.NotNil(t, got.Supplement)
	assert.Equal(t, "happy-path summary", got.Supplement.Summary)
	assert.Equal(t, "signatory-synthesis-v1", got.Attribution.AnalystID)
}

// TestShowSynthesisTool_ByOutputID_NotFound: a UUID that doesn't
// match any row surfaces the uniform "specified synthesis does not
// exist" error.
func TestShowSynthesisTool_ByOutputID_NotFound(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	tool := &ShowSynthesisTool{Store: s}
	resp := tool.Handle(context.Background(),
		json.RawMessage(`{"output_id":"00000000-0000-0000-0000-000000000000"}`))
	require.Equal(t, "error", resp.Status)
	require.NotNil(t, resp.Error)
	assert.Equal(t, mcp.CodeNotFound, resp.Error.Code)
	assert.Equal(t, "specified synthesis does not exist", resp.Error.Message,
		"the not-found message is uniform across all not-found subcases")
}

// TestShowSynthesisTool_ByOutputID_NotASynthesis: pass an output_id
// that exists but is a Layer-2 analyst row (no SynthesisSupplement).
// Same not-found message; the metadata's `analyst_id` field tells
// the agent it's looking at the wrong layer.
func TestShowSynthesisTool_ByOutputID_NotASynthesis(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	outputID := seedAnalystOutput(t, s, "pkg:npm/layer-2-only")

	tool := &ShowSynthesisTool{Store: s}
	resp := tool.Handle(context.Background(),
		json.RawMessage(`{"output_id":"`+outputID+`"}`))
	require.Equal(t, "error", resp.Status)
	assert.Equal(t, mcp.CodeNotFound, resp.Error.Code)
	assert.Equal(t, "specified synthesis does not exist", resp.Error.Message)
	// Discriminating detail in metadata: agents that want to
	// understand "why not" can read the analyst_id and see this is
	// a Layer-2 row.
	details, ok := resp.Error.Details.(map[string]string)
	require.True(t, ok, "Details must be map[string]string for the non-synthesis case")
	require.Contains(t, details, "analyst_id",
		"non-synthesis case must include the row's analyst_id in metadata so the agent can understand it's a Layer-2 row")
}

// TestShowSynthesisTool_ByTarget_HappyPath: pass target URI, get the
// latest synthesis on the resolved entity. Verifies the
// LookupEntity → GetLatestSynthesisForEntity composition.
func TestShowSynthesisTool_ByTarget_HappyPath(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	outputID, _ := seedSynthesis(t, s, "pkg:npm/by-target-test", "target-path summary")

	tool := &ShowSynthesisTool{Store: s}
	resp := tool.Handle(context.Background(),
		json.RawMessage(`{"target":"pkg:npm/by-target-test"}`))
	require.Equal(t, "ok", resp.Status, "%+v", resp.Error)

	got := resp.Data.(*ShowSynthesisResponse)
	assert.Equal(t, outputID, got.OutputID)
	assert.Equal(t, "target-path summary", got.Supplement.Summary)
}

// TestShowSynthesisTool_ByTarget_NoSynthesisOnEntity: entity exists
// but has no synthesis row. Uniform not-found message; metadata
// surfaces the canonical URI plus a hint pointing the agent to
// signatory_show_analyses.
func TestShowSynthesisTool_ByTarget_NoSynthesisOnEntity(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	// Seed entity + a Layer-2 analyst output (so the entity is
	// reachable via LookupEntity), but no synthesis row.
	_ = seedAnalystOutput(t, s, "pkg:npm/has-only-layer-2")

	tool := &ShowSynthesisTool{Store: s}
	resp := tool.Handle(context.Background(),
		json.RawMessage(`{"target":"pkg:npm/has-only-layer-2"}`))
	require.Equal(t, "error", resp.Status)
	assert.Equal(t, mcp.CodeNotFound, resp.Error.Code)
	assert.Equal(t, "specified synthesis does not exist", resp.Error.Message)
	details, ok := resp.Error.Details.(map[string]string)
	require.True(t, ok, "Details must be map[string]string for the no-synthesis target case")
	require.Contains(t, details, "canonical_uri",
		"target with-no-synthesis case includes the canonical_uri so the agent knows which entity it found")
	require.Contains(t, details, "hint",
		"hint must point the agent at the right next-action tool (signatory_show_analyses)")
}

// TestShowSynthesisTool_ByTarget_UnresolvableTarget: a malformed
// input string is a schema violation, not a not-found.
func TestShowSynthesisTool_ByTarget_UnresolvableTarget(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	tool := &ShowSynthesisTool{Store: s}
	resp := tool.Handle(context.Background(),
		json.RawMessage(`{"target":"not-a-valid-uri-or-shorthand"}`))
	require.Equal(t, "error", resp.Status)
	// Could be CodeSchemaViolation (malformed) or CodeNotFound
	// (well-formed but unknown entity). The test pins the contract:
	// "not-a-valid-uri-or-shorthand" doesn't parse as any URI/shorthand,
	// so it's schema-class.
	assert.Equal(t, mcp.CodeSchemaViolation, resp.Error.Code,
		"input that ResolveTarget rejects is caller-side malformed input, not a missing-data condition")
}

// TestShowSynthesisTool_ByTarget_UsesAlternates: seed entity at
// canonical lowercase URI; query with a non-canonical alternate
// form. LookupEntity's alternates walk must bridge the lookup so
// the synthesis surfaces correctly. Mirrors the survey/summary
// integration coverage.
func TestShowSynthesisTool_ByTarget_UsesAlternates(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	// Canonical lowercase URI carries the synthesis.
	outputID, _ := seedSynthesis(t, s,
		"repo:github/burntsushi/toml",
		"alternates-walk summary")

	// Query with the mixed-case form. LookupEntity case-folds via
	// CanonicalRepoURI to find the canonical row, then
	// GetLatestSynthesisForEntity surfaces the synthesis.
	tool := &ShowSynthesisTool{Store: s}
	resp := tool.Handle(context.Background(),
		json.RawMessage(`{"target":"BurntSushi/toml"}`))
	require.Equal(t, "ok", resp.Status, "%+v", resp.Error)

	got := resp.Data.(*ShowSynthesisResponse)
	assert.Equal(t, outputID, got.OutputID)
	assert.Equal(t, "repo:github/burntsushi/toml", got.Entity.CanonicalURI,
		"the response surfaces the canonical entity URI, not the input alternate")
}

// TestShowSynthesisTool_BothTargetAndOutputID: both fields set is
// a schema violation — the input shape is "exactly one."
func TestShowSynthesisTool_BothTargetAndOutputID(t *testing.T) {
	t.Parallel()
	tool := &ShowSynthesisTool{Store: openTestStore(t)}
	resp := tool.Handle(context.Background(),
		json.RawMessage(`{"target":"pkg:npm/x","output_id":"00000000-0000-0000-0000-000000000000"}`))
	require.Equal(t, "error", resp.Status)
	assert.Equal(t, mcp.CodeSchemaViolation, resp.Error.Code)
	assert.Contains(t, resp.Error.Message, "mutually exclusive")
}

// TestShowSynthesisTool_NeitherTargetNorOutputID: empty input is a
// schema violation — the input shape is "exactly one."
func TestShowSynthesisTool_NeitherTargetNorOutputID(t *testing.T) {
	t.Parallel()
	tool := &ShowSynthesisTool{Store: openTestStore(t)}
	resp := tool.Handle(context.Background(), json.RawMessage(`{}`))
	require.Equal(t, "error", resp.Status)
	assert.Equal(t, mcp.CodeSchemaViolation, resp.Error.Code)
	assert.Contains(t, resp.Error.Message, "exactly one")
}

// TestShowSynthesisTool_RejectsUnknownFields: the strict-decode
// convention every other tool uses applies here too — typos surface
// immediately rather than being silently dropped.
func TestShowSynthesisTool_RejectsUnknownFields(t *testing.T) {
	t.Parallel()
	tool := &ShowSynthesisTool{Store: openTestStore(t)}
	resp := tool.Handle(context.Background(),
		json.RawMessage(`{"target":"pkg:npm/x","mispelled_flag":true}`))
	require.Equal(t, "error", resp.Status)
	assert.Equal(t, mcp.CodeSchemaViolation, resp.Error.Code)
}
