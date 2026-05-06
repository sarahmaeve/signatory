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
			// Model and InvokedAt are server-stamped at ingest;
			// caller-supplied values are rejected by the validator.
			// See exchange.AgentAttribution.validate.
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
			// Model and InvokedAt server-stamped at ingest.
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
			// Model and InvokedAt server-stamped at ingest.
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

// ---- metadata sanity tests ----

// TestIngestAnalysisTool_MetadataShape asserts the tool's static
// metadata is intact: non-empty Name/Description, well-formed
// InputSchema that actually enforces required fields, and the
// description mentions enough context for an LLM caller to route
// to it (the word "ingest" or a reference to analyst outputs).
//
// This catches typos and silent-breakage regressions — an empty
// Description or a malformed schema would slip past every
// handler-level test because they directly construct the input
// rather than parsing the schema.
func TestIngestAnalysisTool_MetadataShape(t *testing.T) {
	t.Parallel()
	tool := &IngestAnalysisTool{}

	// Name: exact string for downstream tooling / skill allowlist
	// references. A rename is a breaking change.
	assert.Equal(t, "signatory_ingest_analysis", tool.Name())

	// Description: non-empty and points the caller at the tool's
	// purpose. LLM routing depends on description quality; we don't
	// test for specific wording but do check it's substantive and
	// names the role.
	desc := tool.Description()
	assert.NotEmpty(t, desc)
	assert.Greater(t, len(desc), 100,
		"description should be substantive enough to guide LLM routing, got %d chars", len(desc))
	assert.Contains(t, desc, "AnalystOutput",
		"description should name the payload type so the LLM can match on it")

	// InputSchema: must parse as JSON and describe the expected shape.
	// DisallowUnknownFields in the handler relies on "additionalProperties":
	// false, so the validator-shim's strict-reject behavior works.
	schemaBytes := tool.InputSchema()
	require.NotEmpty(t, schemaBytes)

	var schema map[string]any
	require.NoError(t, json.Unmarshal(schemaBytes, &schema),
		"InputSchema must be valid JSON")

	assert.Equal(t, "object", schema["type"], "top-level schema type should be object")

	required, ok := schema["required"].([]any)
	require.True(t, ok, "schema should declare a required[] array")
	requiredSet := map[string]bool{}
	for _, f := range required {
		if s, ok := f.(string); ok {
			requiredSet[s] = true
		}
	}
	assert.True(t, requiredSet["analyst_output"],
		"analyst_output must be in required[] so missing-field validation fires")

	// additionalProperties: false makes the validator reject unknown
	// top-level keys. Without this, a typo in the caller's envelope
	// (e.g., "analystt_output") would silently succeed as an empty
	// ingest.
	assert.Equal(t, false, schema["additionalProperties"],
		"additionalProperties must be false to enable strict-reject of typos")

	// Properties must at least include analyst_output.
	props, ok := schema["properties"].(map[string]any)
	require.True(t, ok, "schema should declare a properties map")
	assert.Contains(t, props, "analyst_output",
		"analyst_output must appear under properties")
	assert.Contains(t, props, "source",
		"source must appear under properties (optional but schema-known)")
	assert.Contains(t, props, "analysis_session_id",
		"analysis_session_id must appear under properties so agents can discover the session-linkage contract from the schema")
}

// TestIngestAnalysisTool_WithAnalysisSession exercises the Phase 3
// session linkage: an ingest carrying analysis_session_id stamps
// the FK at INSERT time, making the output queryable via the
// session surface. This is the critical path — the /analyze skill
// relies on this field to tie analyst outputs back to the
// originating dispatch.
func TestIngestAnalysisTool_WithAnalysisSession(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)

	// Create the entity + session that the ingest will link against.
	entity := seedEntity(t, s, "pkg:test/widget", "widget")
	session := &profile.AnalysisSession{
		ID:        profile.NewEntityID(), // reuse the UUID helper; session IDs are plain UUIDs
		EntityID:  entity.ID,
		TargetURI: "pkg:test/widget",
		InvokedBy: "test-actor",
		StartedAt: time.Now().UTC(),
		Status:    profile.AnalysisSessionInProgress,
	}
	require.NoError(t, s.CreateAnalysisSession(context.Background(), session))

	tool := &IngestAnalysisTool{Store: s}
	payload := minimalValidAnalystOutputJSON(t)

	envelope := map[string]any{
		"analyst_output":      json.RawMessage(payload),
		"analysis_session_id": session.ID,
	}
	raw, err := json.Marshal(envelope)
	require.NoError(t, err)

	resp := tool.Handle(context.Background(), raw)
	require.Equal(t, "ok", resp.Status, "expected ok, got %+v", resp.Error)
	data := resp.Data.(ingestAnalysisData)

	// Verify the FK landed: ListOutputsForSession should surface
	// this output under the session we linked to.
	outputs, err := s.ListOutputsForSession(context.Background(), session.ID)
	require.NoError(t, err)
	require.Len(t, outputs, 1,
		"output must surface under the session it was ingested with")
	assert.Equal(t, data.OutputID, outputs[0].OutputID)
}

// TestIngestAnalysisTool_WithAnalysisSession_UnknownID confirms
// that passing a bogus session id fails cleanly rather than
// silently dropping the linkage. The FK constraint catches it at
// the store layer; the MCP surface relays the failure.
func TestIngestAnalysisTool_WithAnalysisSession_UnknownID(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	tool := &IngestAnalysisTool{Store: s}
	payload := minimalValidAnalystOutputJSON(t)

	envelope := map[string]any{
		"analyst_output":      json.RawMessage(payload),
		"analysis_session_id": profile.NewEntityID(), // never created
	}
	raw, err := json.Marshal(envelope)
	require.NoError(t, err)

	resp := tool.Handle(context.Background(), raw)
	assert.NotEqual(t, "ok", resp.Status,
		"unknown analysis_session_id must surface as an error — FK constraint enforces linkage integrity")
	require.NotNil(t, resp.Error)
	assert.Contains(t, resp.Error.Message, "ingest failed",
		"error message should identify the ingest phase so callers can retry with a corrected id")
}

// TestIngestAnalysisTool_SynthesisWithoutSession_SchemaViolation
// confirms the new error mapping: the store-layer
// ErrSynthesisRequiresSession sentinel is converted to a
// CodeSchemaViolation MCP response with the offending field named.
// That makes the error agent-correctable — the synthesist drops
// analysis_session_id, gets this loud rejection, and retries with
// the field included in the same turn.
func TestIngestAnalysisTool_SynthesisWithoutSession_SchemaViolation(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	tool := &IngestAnalysisTool{Store: s}

	// Build a minimal-valid synthesis-shape payload.
	synth := exchange.AnalystOutput{
		Attribution: exchange.AgentAttribution{
			AnalystID: "signatory-synthesis-v1",
			// Model and InvokedAt server-stamped at ingest.
		},
		Target: "pkg:test/widget",
		SynthesisSupplement: &exchange.SynthesisSupplement{
			ProposedPosture: exchange.ProposedPosture{
				Tier:             exchange.ProposedTierTrustedForNow,
				RationaleSummary: "test rationale",
			},
			Reasoning: "test reasoning",
			Summary:   "test summary",
		},
	}
	payload, err := json.Marshal(synth)
	require.NoError(t, err)

	// Ingest WITHOUT analysis_session_id — the bug pattern.
	envelope := map[string]any{
		"analyst_output": json.RawMessage(payload),
	}
	raw, err := json.Marshal(envelope)
	require.NoError(t, err)

	resp := tool.Handle(context.Background(), raw)
	require.Equal(t, "error", resp.Status)
	require.NotNil(t, resp.Error)
	assert.Equal(t, mcp.CodeSchemaViolation, resp.Error.Code,
		"synthesis-requires-session must surface as schema violation, not internal error, so the agent retries")
	assert.Contains(t, resp.Error.Message, "analysis_session_id",
		"error message must name the missing field so the agent knows what to add")
}

// TestIngestAnalysisTool_RejectsClientSuppliedModel asserts that an
// ingest payload carrying attribution.model is rejected with a clear
// CodeSchemaViolation pointing at the field. Background:
// agents have no reliable way to know what model they are running
// on, and recent dogfood evidence shows synthesists hallucinating
// model identity (e.g. "claude-3.5-sonnet" stamped on a 2026-05-06
// row). The fix is to make the field server-stamped (NULL/empty
// until OTEL backfill writes the real value) and reject any
// caller-supplied value at the schema validator.
func TestIngestAnalysisTool_RejectsClientSuppliedModel(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	tool := &IngestAnalysisTool{Store: s}

	out := exchange.AnalystOutput{
		Attribution: exchange.AgentAttribution{
			AnalystID: "test-analyst",
			Model:     "claude-sonnet-4-7", // forbidden — server-stamped
		},
		Target: "pkg:test/widget",
	}
	payload, err := json.Marshal(out)
	require.NoError(t, err)

	resp := tool.Handle(context.Background(), wrapIngestPayload(t, payload, ""))
	require.Equal(t, "error", resp.Status)
	require.NotNil(t, resp.Error)
	assert.Equal(t, mcp.CodeSchemaViolation, resp.Error.Code)
	assert.Contains(t, resp.Error.Message, "attribution.model",
		"error must name the offending field so the agent can self-correct")
	assert.Contains(t, resp.Error.Message, "server-stamped",
		"error must explain WHY the field is rejected (so the agent doesn't just guess at the right value)")
}

// TestIngestAnalysisTool_RejectsClientSuppliedInvokedAt — same shape
// as the model test, for the invoked_at field. Agents don't have
// access to a reliable wall clock either; the timestamp must be
// stamped server-side at INSERT.
func TestIngestAnalysisTool_RejectsClientSuppliedInvokedAt(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	tool := &IngestAnalysisTool{Store: s}

	out := exchange.AnalystOutput{
		Attribution: exchange.AgentAttribution{
			AnalystID: "test-analyst",
			InvokedAt: "2026-05-06T10:30:00Z", // forbidden — server-stamped
		},
		Target: "pkg:test/widget",
	}
	payload, err := json.Marshal(out)
	require.NoError(t, err)

	resp := tool.Handle(context.Background(), wrapIngestPayload(t, payload, ""))
	require.Equal(t, "error", resp.Status)
	require.NotNil(t, resp.Error)
	assert.Equal(t, mcp.CodeSchemaViolation, resp.Error.Code)
	assert.Contains(t, resp.Error.Message, "attribution.invoked_at",
		"error must name the offending field")
	assert.Contains(t, resp.Error.Message, "server-stamped",
		"error must explain why the field is rejected")
}

// TestIngestAnalysisTool_StampsInvokedAtFromServerClock asserts that
// when the agent submits a payload WITHOUT attribution.invoked_at
// (the new contract), the ingest tool stamps it from time.Now()
// before persisting. The stored value must be a parseable RFC3339
// timestamp within tolerance of the test's call site clock.
func TestIngestAnalysisTool_StampsInvokedAtFromServerClock(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	tool := &IngestAnalysisTool{Store: s}

	out := exchange.AnalystOutput{
		Attribution: exchange.AgentAttribution{
			AnalystID: "test-analyst",
			// Model and InvokedAt deliberately empty — server-stamped contract.
		},
		Target: "pkg:test/widget",
	}
	payload, err := json.Marshal(out)
	require.NoError(t, err)

	before := time.Now().UTC()
	resp := tool.Handle(context.Background(), wrapIngestPayload(t, payload, ""))
	after := time.Now().UTC()

	require.Equal(t, "ok", resp.Status, "expected ok, got %+v", resp.Error)
	data := resp.Data.(ingestAnalysisData)

	filter := store.AnalystOutputFilter{EntityID: data.EntityID}
	summaries, err := s.ListAnalystOutputs(context.Background(), filter)
	require.NoError(t, err)
	require.Len(t, summaries, 1)

	stored, err := time.Parse(time.RFC3339, summaries[0].InvokedAt)
	require.NoError(t, err,
		"stored invoked_at must be a parseable RFC3339 timestamp; got %q",
		summaries[0].InvokedAt)

	// Tolerance: stored time should be in [before, after]. We allow
	// a generous 2s slop on each end to accommodate clock granularity
	// and slow CI runners. Failures here mean the timestamp wasn't
	// stamped from time.Now() at all.
	assert.False(t, stored.Before(before.Add(-2*time.Second)),
		"stored invoked_at %s is BEFORE the test's pre-call clock %s — "+
			"server-stamping is broken or stamping a stale value",
		stored, before)
	assert.False(t, stored.After(after.Add(2*time.Second)),
		"stored invoked_at %s is AFTER the test's post-call clock %s — "+
			"server-stamping is generating a future timestamp",
		stored, after)
}

// TestIngestAnalysisTool_StoresEmptyModelForBackfill asserts that
// when the agent submits a payload without attribution.model, the
// ingest tool persists an empty string for model. The empty value
// is a sentinel meaning "model identity pending OTEL correlation"
// — half 2 of this fix will UPDATE these rows with the real model
// name from Claude Code's OTEL spans (joined by analysis_session_id).
//
// We test for "" rather than NULL because the migrate.go schema
// declares analyst_outputs.model as TEXT NOT NULL. Lifting that
// constraint to allow NULL is a separate migration; for now the
// renderer at cmd/signatory/show_synthesis.go already treats "" as
// "elide the (model) suffix", which is the correct UX.
func TestIngestAnalysisTool_StoresEmptyModelForBackfill(t *testing.T) {
	t.Parallel()
	s := openTestStore(t)
	tool := &IngestAnalysisTool{Store: s}

	out := exchange.AnalystOutput{
		Attribution: exchange.AgentAttribution{
			AnalystID: "test-analyst",
			// Model deliberately empty — server-stamped contract.
		},
		Target: "pkg:test/widget",
	}
	payload, err := json.Marshal(out)
	require.NoError(t, err)

	resp := tool.Handle(context.Background(), wrapIngestPayload(t, payload, ""))
	require.Equal(t, "ok", resp.Status, "expected ok, got %+v", resp.Error)
	data := resp.Data.(ingestAnalysisData)

	filter := store.AnalystOutputFilter{EntityID: data.EntityID}
	summaries, err := s.ListAnalystOutputs(context.Background(), filter)
	require.NoError(t, err)
	require.Len(t, summaries, 1)
	assert.Equal(t, "", summaries[0].Model,
		"stored model must be empty until OTEL backfill writes the real value; "+
			"a non-empty value here means the agent or the validator allowed model through")
}
