package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/exchange"
	"github.com/sarahmaeve/signatory/internal/store"
)

// minimalValidAnalystOutputJSON returns the bytes of a minimal
// v1-schema AnalystOutput that passes exchange.Validate and is
// ingestible — shared fixture for tests that need a valid
// payload without caring about the specific conclusions.
//
// Target uses pkg: scheme so the store auto-creates a cargo-style
// entity without requiring github URL normalization.
func minimalValidAnalystOutputJSON(t *testing.T) []byte {
	t.Helper()
	out := exchange.AnalystOutput{
		Attribution: exchange.AgentAttribution{
			AnalystID: "test-analyst",
			Model:     "test-model",
			InvokedAt: "2026-04-17T12:00:00Z",
		},
		Target:      "pkg:test/widget",
		Conclusions: nil, // empty is valid
	}
	data, err := json.Marshal(out)
	require.NoError(t, err)
	return data
}

// wrapIngestPayload returns the input JSON the tool expects —
// the analyst_output nested under an envelope.
func wrapIngestPayload(t *testing.T, analystOutput []byte, source string) json.RawMessage {
	t.Helper()
	env := map[string]any{"analyst_output": json.RawMessage(analystOutput)}
	if source != "" {
		env["source"] = source
	}
	data, err := json.Marshal(env)
	require.NoError(t, err)
	return data
}

func TestIngestAnalysisTool_HappyPath(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	tool := &IngestAnalysisTool{Store: s}
	payload := minimalValidAnalystOutputJSON(t)

	resp := tool.Handle(context.Background(), wrapIngestPayload(t, payload, ""))
	require.Equal(t, "ok", resp.Status, "expected ok, got %+v", resp.Error)

	data, ok := resp.Data.(ingestAnalysisData)
	require.True(t, ok, "expected ingestAnalysisData, got %T", resp.Data)
	assert.NotEmpty(t, data.OutputID)
	assert.NotEmpty(t, data.EntityID)
	assert.False(t, data.Idempotent, "first ingest is not idempotent")
}

func TestIngestAnalysisTool_Idempotent(t *testing.T) {
	// Re-ingesting the same payload should return the same IDs
	// with idempotent=true, not error out and not double-write.
	t.Parallel()
	s := openTestStore(t)
	tool := &IngestAnalysisTool{Store: s}
	payload := minimalValidAnalystOutputJSON(t)

	first := tool.Handle(context.Background(), wrapIngestPayload(t, payload, ""))
	require.Equal(t, "ok", first.Status)
	firstData := first.Data.(ingestAnalysisData)
	assert.False(t, firstData.Idempotent)

	second := tool.Handle(context.Background(), wrapIngestPayload(t, payload, "different-source"))
	require.Equal(t, "ok", second.Status)
	secondData := second.Data.(ingestAnalysisData)
	assert.True(t, secondData.Idempotent, "content-identical re-ingest must be idempotent")
	assert.Equal(t, firstData.OutputID, secondData.OutputID, "idempotent ingest returns same output ID")
	assert.Equal(t, firstData.EntityID, secondData.EntityID, "idempotent ingest returns same entity ID")

	// And only one analyst_outputs row in the store.
	filter := store.AnalystOutputFilter{EntityID: firstData.EntityID}
	summaries, err := s.ListAnalystOutputs(context.Background(), filter)
	require.NoError(t, err)
	assert.Len(t, summaries, 1, "idempotent re-ingest must not create a duplicate row")
}

func TestIngestAnalysisTool_MissingAnalystOutput_SchemaViolation(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	tool := &IngestAnalysisTool{Store: s}

	resp := tool.Handle(context.Background(), json.RawMessage(`{}`))
	require.Equal(t, "error", resp.Status)
	require.NotNil(t, resp.Error)
	assert.Equal(t, string(mcpSchemaViolationCode()), resp.Error.Code)
	assert.Contains(t, resp.Error.Message, "analyst_output is required")
}

func TestIngestAnalysisTool_MalformedJSON_SchemaViolation(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	tool := &IngestAnalysisTool{Store: s}

	// Valid envelope shape, invalid inner JSON — analyst_output is
	// a JSON string instead of an object.
	resp := tool.Handle(context.Background(), json.RawMessage(`{"analyst_output": "not-an-object"}`))
	require.Equal(t, "error", resp.Status)
	require.NotNil(t, resp.Error)
	assert.Contains(t, resp.Error.Message, "not valid v1 JSON")
}

func TestIngestAnalysisTool_ValidationFailure_NamesField(t *testing.T) {
	// Payload parses as JSON and unmarshals as AnalystOutput, but
	// fails Validate because the Target is missing. The error
	// message should name the missing field so the caller can fix
	// and retry in one turn.
	t.Parallel()
	s := openTestStore(t)
	tool := &IngestAnalysisTool{Store: s}

	// Minimal payload missing Target.
	payload, err := json.Marshal(exchange.AnalystOutput{
		Attribution: exchange.AgentAttribution{
			AnalystID: "x",
			Model:     "y",
			InvokedAt: "2026-04-17T12:00:00Z",
		},
		// Target deliberately omitted.
	})
	require.NoError(t, err)

	resp := tool.Handle(context.Background(), wrapIngestPayload(t, payload, ""))
	require.Equal(t, "error", resp.Status)
	require.NotNil(t, resp.Error)
	assert.Contains(t, resp.Error.Message, "schema validation")
	assert.Contains(t, resp.Error.Message, "target", "error should name the missing target field")
}

func TestIngestAnalysisTool_UnknownFieldsRejected(t *testing.T) {
	// DisallowUnknownFields should prevent a caller from sneaking
	// typos past the schema. Catches things like
	// {"analystt_output": ...} (a typo) early instead of silently
	// treating it as an empty payload.
	t.Parallel()
	s := openTestStore(t)
	tool := &IngestAnalysisTool{Store: s}

	resp := tool.Handle(context.Background(),
		json.RawMessage(`{"analystt_output": {}}`))
	require.Equal(t, "error", resp.Status)
	require.NotNil(t, resp.Error)
}

func TestIngestAnalysisTool_CustomSource_Respected(t *testing.T) {
	// The `source` field lands on the store row as source_path. We
	// verify that by listing the ingested output and checking its
	// recorded source matches what we sent.
	t.Parallel()
	s := openTestStore(t)
	tool := &IngestAnalysisTool{Store: s}

	payload := minimalValidAnalystOutputJSON(t)
	const customSource = "mcp:security-analyst:session-abc"

	resp := tool.Handle(context.Background(), wrapIngestPayload(t, payload, customSource))
	require.Equal(t, "ok", resp.Status)
	data := resp.Data.(ingestAnalysisData)

	filter := store.AnalystOutputFilter{EntityID: data.EntityID}
	summaries, err := s.ListAnalystOutputs(context.Background(), filter)
	require.NoError(t, err)
	require.Len(t, summaries, 1)
	assert.Equal(t, customSource, summaries[0].SourcePath,
		"source field should flow through to analyst_outputs.source_path")
}

func TestIngestAnalysisTool_Counts_MatchPayload(t *testing.T) {
	// The Counts payload in the response should reflect the rows
	// that were actually ingested. Build a payload with known
	// numbers of each entity type and assert.
	t.Parallel()
	s := openTestStore(t)
	tool := &IngestAnalysisTool{Store: s}

	lineStart1 := 42
	out := exchange.AnalystOutput{
		Attribution: exchange.AgentAttribution{
			AnalystID: "test",
			Model:     "test",
			InvokedAt: "2026-04-17T12:00:00Z",
		},
		Target: "pkg:test/widget",
		Conclusions: []exchange.Conclusion{
			{
				ID:        "F001",
				Verdict:   "first",
				Rationale: "because",
				Category:  "network",
				Severity:  exchange.Severity{Default: exchange.SeverityLow},
				Citations: []exchange.Citation{
					{Path: "main.go", LineStart: &lineStart1},
				},
			},
			{
				ID:        "F002",
				Verdict:   "second",
				Rationale: "also because",
				Category:  "network",
				Severity:  exchange.Severity{Default: exchange.SeverityMedium},
				Citations: []exchange.Citation{
					{Path: "util.go", LineStart: &lineStart1},
				},
			},
		},
		Observations: []exchange.Observation{
			{ID: "O001", Title: "obs", Body: "body", Category: "trust_model"},
		},
	}
	payload, err := json.Marshal(out)
	require.NoError(t, err)

	resp := tool.Handle(context.Background(), wrapIngestPayload(t, payload, ""))
	require.Equal(t, "ok", resp.Status, "got error: %+v", resp.Error)
	data := resp.Data.(ingestAnalysisData)

	assert.Equal(t, 2, data.Counts.Conclusions)
	assert.Equal(t, 1, data.Counts.Observations)
	assert.Equal(t, 0, data.Counts.PositiveAbsences)
	assert.Equal(t, 0, data.Counts.MethodologyPatterns)
}

// mcpSchemaViolationCode returns the error-code string used by
// mcp.CodeSchemaViolation so tests don't have to know the string
// literal. Import avoidance: the helper is local rather than
// exporting an internal constant from the mcp package.
func mcpSchemaViolationCode() errorCode {
	// CodeSchemaViolation is a string-typed constant. We import
	// it via the same alias the tool package uses.
	return errorCode("schema_violation")
}

type errorCode string
