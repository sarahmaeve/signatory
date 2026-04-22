package store

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sarahmaeve/signatory/internal/exchange"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// loadFixture reads a JSON fixture from internal/exchange/testdata/
// (relative to this package) and returns it parsed as an
// AnalystOutput. The exchange package's fixtures are reused here so
// the store tests stay aligned with the schema's reference instances.
func loadFixture(t *testing.T, relPath string) *exchange.AnalystOutput {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "exchange", "testdata", relPath))
	require.NoError(t, err)
	var out exchange.AnalystOutput
	require.NoError(t, json.Unmarshal(raw, &out))
	return &out
}

func TestIngest_AtuinTrialFixture(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	out := loadFixture(t, "atuin-schema-trial.json")

	result, err := s.IngestAnalystOutput(ctx, out, "../exchange/testdata/atuin-schema-trial.json")
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.NotEmpty(t, result.OutputID, "ingest should produce a UUID")
	assert.NotEmpty(t, result.EntityID, "ingest should resolve or create an entity")
	assert.False(t, result.Idempotent, "first ingest is not idempotent")

	// Validate counts in the DB match the source document.
	assertCount(t, s, "analyst_outputs", "id = ?", result.OutputID, 1)
	assertCount(t, s, "conclusions", "output_id = ?", result.OutputID, len(out.Conclusions))
	assertCount(t, s, "positive_absences", "output_id = ?", result.OutputID, len(out.PositiveAbsences))
	assertCount(t, s, "observations", "output_id = ?", result.OutputID, len(out.Observations))
	if out.MethodologyTrace != nil {
		assertCount(t, s, "methodology_catalogs", "output_id = ?", result.OutputID, 1)
		assertCount(t, s, "methodology_patterns", "output_id = ?", result.OutputID, len(out.MethodologyTrace.Patterns))
	}
}

func TestIngest_AtuinTrial_Idempotent(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	out := loadFixture(t, "atuin-schema-trial.json")

	first, err := s.IngestAnalystOutput(ctx, out, "src1.json")
	require.NoError(t, err)
	assert.False(t, first.Idempotent)

	// Ingest the same content (different source path) → same content_hash → idempotent.
	second, err := s.IngestAnalystOutput(ctx, out, "src2-different-path.json")
	require.NoError(t, err)
	assert.True(t, second.Idempotent, "same content hash should produce idempotent ingest")
	assert.Equal(t, first.OutputID, second.OutputID, "idempotent return should reference the existing row")
	assert.Equal(t, first.EntityID, second.EntityID)

	// And only one row is in the DB.
	assertCount(t, s, "analyst_outputs", "content_hash = ?", contentHash(t, out), 1)
}

func TestIngest_AllShipped_v1Fixtures(t *testing.T) {
	// End-to-end smoke: ingest every JSON fixture in design/analysis/
	// that's a v1 AnalystOutput. The atuin trial JSON is pre-v1 and
	// will fail format-check (per testdata/README.md) so we skip it.
	s := newTestDB(t)
	ctx := context.Background()

	files := []string{
		"thefuck-security-v1.json",
		"thefuck-provenance-v1.json",
	}
	for _, f := range files {
		t.Run(f, func(t *testing.T) {
			out := loadFixture(t, f)
			result, err := s.IngestAnalystOutput(ctx, out, f)
			require.NoError(t, err, "ingest of %s", f)
			assert.NotEmpty(t, result.OutputID)
			assert.False(t, result.Idempotent)
		})
	}

	// Both fixtures target the same URL → should resolve to the same
	// entity (after normalization to the canonical repo:github/... form).
	var entityCount int
	require.NoError(t, s.db.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM entities WHERE canonical_uri = ?`,
		"repo:github/nvbn/thefuck").Scan(&entityCount))
	assert.Equal(t, 1, entityCount, "both thefuck outputs should share one entity")

	var outputCount int
	require.NoError(t, s.db.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM analyst_outputs`).Scan(&outputCount))
	assert.Equal(t, 2, outputCount, "two distinct outputs ingested")
}

func TestIngest_AutoCreatesEntity(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	out := loadFixture(t, "atuin-schema-trial.json")

	result, err := s.IngestAnalystOutput(ctx, out, "")
	require.NoError(t, err)

	// The atuin trial fixture targets pkg:cargo/atuin — there should
	// now be an entity for it with sensible defaults.
	entity, err := s.FindEntityByURI(ctx, "pkg:cargo/atuin")
	require.NoError(t, err)
	require.NotNil(t, entity)
	assert.Equal(t, result.EntityID, entity.ID)
	assert.Equal(t, "atuin", entity.ShortName, "short_name derived from purl path tail")
	assert.Equal(t, "cargo", entity.Ecosystem, "ecosystem derived from purl scheme")
	assert.Empty(t, entity.URL, "purl targets shouldn't have an http URL set")
}

func TestIngest_HTTPSTarget_NormalizedToCanonicalURI(t *testing.T) {
	// AnalystOutput.Target carrying a GitHub URL gets normalized to
	// the canonical `repo:github/owner/name` form before entity
	// lookup/insert. The original URL is preserved on the entity's
	// URL field so the surface form isn't lost.
	s := newTestDB(t)
	ctx := context.Background()
	out := loadFixture(t, "thefuck-security-v1.json")

	_, err := s.IngestAnalystOutput(ctx, out, "")
	require.NoError(t, err)

	// Lookup uses the normalized canonical URI.
	entity, err := s.FindEntityByURI(ctx, "repo:github/nvbn/thefuck")
	require.NoError(t, err)
	require.NotNil(t, entity)
	assert.Equal(t, "thefuck", entity.ShortName, "short_name from path tail")
	assert.Equal(t, "https://github.com/nvbn/thefuck", entity.URL,
		"original URL preserved on entity for http(s) targets")
	assert.Empty(t, entity.Ecosystem, "repo: scheme has no ecosystem token")

	// Lookup by the original URL form should fail (no entity stored
	// under it) — the canonical_uri is the normalized form.
	_, err = s.FindEntityByURI(ctx, "https://github.com/nvbn/thefuck")
	require.Error(t, err, "raw URL is not the canonical_uri")
}

func TestIngest_ReusesExistingEntity(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	// Pre-create the entity so we can verify it's not recreated.
	preExisting := testEntity("preset-id", "pkg:cargo/atuin", "atuin-existing", time.Now().UTC())
	require.NoError(t, s.PutEntity(ctx, preExisting))

	out := loadFixture(t, "atuin-schema-trial.json")
	result, err := s.IngestAnalystOutput(ctx, out, "")
	require.NoError(t, err)

	assert.Equal(t, "preset-id", result.EntityID, "should reuse existing entity by canonical_uri")

	entity, err := s.FindEntityByURI(ctx, "pkg:cargo/atuin")
	require.NoError(t, err)
	assert.Equal(t, "atuin-existing", entity.ShortName,
		"existing entity's short_name should not be overwritten by ingest")
}

func TestIngest_ConclusionFields_PreservedFully(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	out := loadFixture(t, "atuin-schema-trial.json")

	result, err := s.IngestAnalystOutput(ctx, out, "")
	require.NoError(t, err)

	// Spot-check F001 — the "positive correction" conclusion with
	// supersession, design_intent, severity_default=positive.
	var verdict, severityDefault string
	var designIntent int
	err = s.db.QueryRowContext(ctx,
		`SELECT verdict, severity_default, design_intent
		 FROM conclusions WHERE output_id = ? AND conclusion_local_id = 'F001'`,
		result.OutputID).Scan(&verdict, &severityDefault, &designIntent)
	require.NoError(t, err)
	assert.Contains(t, verdict, "atuin-ai")
	assert.Equal(t, "positive", severityDefault)
	assert.Equal(t, 1, designIntent, "F001 has design_intent: true")

	// Supersedes row exists.
	var priorID, kind string
	err = s.db.QueryRowContext(ctx,
		`SELECT cs.prior_id, cs.kind
		 FROM conclusion_supersedes cs
		 INNER JOIN conclusions c ON cs.conclusion_id = c.id
		 WHERE c.output_id = ? AND c.conclusion_local_id = 'F001'`,
		result.OutputID).Scan(&priorID, &kind)
	require.NoError(t, err)
	assert.Equal(t, "r1-ai-subsystem-threat", priorID)
	assert.Equal(t, "corrects", kind)
}

func TestIngest_ConditionalSeverity_Stored(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	out := loadFixture(t, "atuin-schema-trial.json")
	result, err := s.IngestAnalystOutput(ctx, out, "")
	require.NoError(t, err)

	// F003 has three by_context entries (single_user, shared_host,
	// multi_user+windows). All should be in conclusion_severity_contexts.
	rows, err := s.db.QueryContext(ctx,
		`SELECT csc.host_isolation, csc.platform, csc.value
		 FROM conclusion_severity_contexts csc
		 INNER JOIN conclusions c ON csc.conclusion_id = c.id
		 WHERE c.output_id = ? AND c.conclusion_local_id = 'F003'
		 ORDER BY csc.host_isolation, csc.platform`,
		result.OutputID)
	require.NoError(t, err)
	defer rows.Close() //nolint:errcheck // close on read-only rows; any real error surfaced during Scan

	type ctxRow struct{ host, platform, value string }
	var got []ctxRow
	for rows.Next() {
		var r ctxRow
		require.NoError(t, rows.Scan(&r.host, &r.platform, &r.value))
		got = append(got, r)
	}
	require.Len(t, got, 3)
	assert.Equal(t, "multi_user", got[0].host)
	assert.Equal(t, "windows", got[0].platform)
	assert.Equal(t, "shared_host", got[1].host)
	assert.Equal(t, "single_user", got[2].host)
}

func TestIngest_Citations_PolymorphicFK(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	out := loadFixture(t, "atuin-schema-trial.json")
	result, err := s.IngestAnalystOutput(ctx, out, "")
	require.NoError(t, err)

	// Confirm we have citations of multiple parent_kinds.
	rows, err := s.db.QueryContext(ctx,
		`SELECT parent_kind, COUNT(*) FROM citations
		 WHERE parent_id IN (
		   SELECT id FROM conclusions WHERE output_id = ?
		   UNION SELECT id FROM positive_absences WHERE output_id = ?
		 )
		 GROUP BY parent_kind`,
		result.OutputID, result.OutputID)
	require.NoError(t, err)
	defer rows.Close() //nolint:errcheck // close on read-only rows; any real error surfaced during Scan

	kinds := map[string]int{}
	for rows.Next() {
		var kind string
		var count int
		require.NoError(t, rows.Scan(&kind, &count))
		kinds[kind] = count
	}
	assert.Greater(t, kinds["conclusion"], 0, "conclusions have citations")
	assert.Greater(t, kinds["positive_absence"], 0, "positive_absences have scope-based citations")

	// Scope-based citation (positive_absence) should have line_start = -1.
	var lineStart int
	err = s.db.QueryRowContext(ctx,
		`SELECT c.line_start FROM citations c
		 INNER JOIN positive_absences pa ON c.parent_id = pa.id
		 WHERE pa.output_id = ? AND c.parent_kind = 'positive_absence'
		 LIMIT 1`, result.OutputID).Scan(&lineStart)
	require.NoError(t, err)
	assert.Equal(t, -1, lineStart, "scope-based citations use -1 sentinel for line_start")
}

func TestIngest_MethodologyComposesWith_Stored(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	out := loadFixture(t, "atuin-schema-trial.json")
	result, err := s.IngestAnalystOutput(ctx, out, "")
	require.NoError(t, err)

	// MP-NET-03 composes with MP-NET-01 + MP-NET-02 per the fixture.
	rows, err := s.db.QueryContext(ctx,
		`SELECT mpc.composes_with FROM methodology_pattern_composes mpc
		 INNER JOIN methodology_patterns mp ON mpc.pattern_id = mp.id
		 WHERE mp.output_id = ? AND mp.pattern_local_id = 'MP-NET-03'
		 ORDER BY mpc.composes_with`,
		result.OutputID)
	require.NoError(t, err)
	defer rows.Close() //nolint:errcheck // close on read-only rows; any real error surfaced during Scan

	var got []string
	for rows.Next() {
		var c string
		require.NoError(t, rows.Scan(&c))
		got = append(got, c)
	}
	assert.Equal(t, []string{"MP-NET-01", "MP-NET-02"}, got)
}

func TestIngest_NilInput_Errors(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	_, err := s.IngestAnalystOutput(ctx, nil, "")
	require.Error(t, err)
}

func TestIngest_InvalidOutput_Errors(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	bad := &exchange.AnalystOutput{Target: "pkg:test/x"} // missing attribution
	_, err := s.IngestAnalystOutput(ctx, bad, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "validate")
}

// TestIngest_WithPrimaryTarget_CrossURILookup covers the core M2
// behavior: an analysis whose internal target is repo:github/Y but
// was collected on behalf of a caller asking about pkg:npm/X gets
// indexed under pkg:npm/X, and queries by EITHER URI find it.
func TestIngest_WithPrimaryTarget_CrossURILookup(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	// Load a fixture and override its target to a github URI.
	out := loadFixture(t, "atuin-schema-trial.json")
	out.Target = "repo:github/expressjs/express"

	// Caller originally asked about pkg:npm/express; ingest with
	// WithPrimaryTarget records it under pkg:npm/express with the
	// github URI captured as collected_from.
	result, err := s.IngestAnalystOutput(ctx, out, "",
		WithPrimaryTarget("pkg:npm/express"))
	require.NoError(t, err)
	require.NotEmpty(t, result.EntityID)
	require.NotEmpty(t, result.CollectedFromEntityID,
		"M2: resolution hop must populate CollectedFromEntityID")
	assert.NotEqual(t, result.EntityID, result.CollectedFromEntityID,
		"primary entity and collected-from entity must differ")

	// Lookup by pkg URI: the original caller's identity finds the analysis.
	byPkg, err := s.ListAnalystOutputs(ctx, AnalystOutputFilter{
		EntityURI: "pkg:npm/express",
	})
	require.NoError(t, err)
	require.Len(t, byPkg, 1, "pkg:npm/express query must find the analysis")
	assert.Equal(t, "pkg:npm/express", byPkg[0].EntityURI)
	assert.Equal(t, "repo:github/expressjs/express", byPkg[0].CollectedFromURI,
		"transparent-with-citation: response cites both URIs (§3.2)")

	// Lookup by repo URI: the resolved source also finds the analysis
	// via the reverse index on collected_from_entity_id.
	byRepo, err := s.ListAnalystOutputs(ctx, AnalystOutputFilter{
		EntityURI: "repo:github/expressjs/express",
	})
	require.NoError(t, err)
	require.Len(t, byRepo, 1, "repo:github/... query must also find the analysis (reverse walk)")
	assert.Equal(t, byPkg[0].OutputID, byRepo[0].OutputID,
		"both URIs must resolve to the SAME analysis row")
}

// TestIngest_WithPrimaryTarget_SameAsTarget_NoHop verifies that
// passing the same URI as WithPrimaryTarget AND as out.Target
// produces a row with no resolution hop recorded — the column
// stays NULL.
func TestIngest_WithPrimaryTarget_SameAsTarget_NoHop(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	out := loadFixture(t, "atuin-schema-trial.json")
	// Fixture's target is "pkg:cargo/atuin"; pass the same as primary.
	result, err := s.IngestAnalystOutput(ctx, out, "",
		WithPrimaryTarget(out.Target))
	require.NoError(t, err)
	assert.Empty(t, result.CollectedFromEntityID,
		"same-identity passthrough must not record a resolution hop")
}

// TestIngest_WithoutPrimaryTarget_DefaultBehavior verifies pre-M2
// behavior is preserved when no options are passed — the row's
// entity_id matches out.Target's entity, and collected_from is empty.
func TestIngest_WithoutPrimaryTarget_DefaultBehavior(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	out := loadFixture(t, "atuin-schema-trial.json")
	result, err := s.IngestAnalystOutput(ctx, out, "")
	require.NoError(t, err)
	assert.Empty(t, result.CollectedFromEntityID,
		"no options passed → no resolution hop recorded")
}

func TestIngest_AppendOnlyTriggers_FireOnUpdate(t *testing.T) {
	// Defense-in-depth: even a SQL-level UPDATE on analyst_outputs
	// should be blocked by the migration v4 trigger.
	s := newTestDB(t)
	ctx := context.Background()
	out := loadFixture(t, "atuin-schema-trial.json")
	result, err := s.IngestAnalystOutput(ctx, out, "")
	require.NoError(t, err)

	_, err = s.db.ExecContext(ctx,
		`UPDATE analyst_outputs SET round = 99 WHERE id = ?`, result.OutputID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "append-only")
}

// synthesisTestInput returns a minimally-valid synthesist AnalystOutput
// for M6a round-trip tests. Keeping this inline (rather than in a
// fixture file) means the test and its expected shape live together.
func synthesisTestInput() *exchange.AnalystOutput {
	return &exchange.AnalystOutput{
		Attribution: exchange.AgentAttribution{
			AnalystID: "signatory-synthesis-v1",
			Model:     "claude-test",
			InvokedAt: "2026-04-21T00:00:00Z",
		},
		Target: "pkg:npm/example",
		SynthesisSupplement: &exchange.SynthesisSupplement{
			ProposedPosture: exchange.ProposedPosture{
				Tier:             exchange.ProposedTierTrustedForNow,
				RationaleSummary: "minimal rationale for round-trip test",
			},
			Reasoning: "minimal reasoning paragraph",
			Summary:   "minimal summary",
		},
	}
}

// TestIngest_SynthesisSupplement_RoundTrip is the M6a integration test:
// a synthesist AnalystOutput with a supplement round-trips through
// IngestAnalystOutput → GetAnalystOutput unchanged. Drives migration v8
// (new columns), the supplement-aware ingest write, and the
// supplement-aware Get read.
func TestIngest_SynthesisSupplement_RoundTrip(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	input := synthesisTestInput()
	result, err := s.IngestAnalystOutput(ctx, input, "synthesis-test")
	require.NoError(t, err)
	require.NotEmpty(t, result.OutputID)

	loaded, err := s.GetAnalystOutput(ctx, result.OutputID)
	require.NoError(t, err)

	require.NotNil(t, loaded.SynthesisSupplement,
		"supplement must round-trip through storage")
	assert.Equal(t, input.SynthesisSupplement.ProposedPosture.Tier,
		loaded.SynthesisSupplement.ProposedPosture.Tier)
	assert.Equal(t, input.SynthesisSupplement.ProposedPosture.RationaleSummary,
		loaded.SynthesisSupplement.ProposedPosture.RationaleSummary)
	assert.Equal(t, input.SynthesisSupplement.Reasoning,
		loaded.SynthesisSupplement.Reasoning)
	assert.Equal(t, input.SynthesisSupplement.Summary,
		loaded.SynthesisSupplement.Summary)
}

// TestIngest_SynthesisSupplement_FullShapeRoundTrip exercises every
// field on the SynthesisSupplement struct (concordance, contradictions,
// conclusion refs, gaps, action_items, notes, version_scope) through
// the JSON-column round-trip. Deep equality verifies that the JSON
// serialization + deserialization doesn't lose or reshape any field.
func TestIngest_SynthesisSupplement_FullShapeRoundTrip(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	input := &exchange.AnalystOutput{
		Attribution: exchange.AgentAttribution{
			AnalystID: "signatory-synthesis-v1",
			Model:     "claude-test",
			InvokedAt: "2026-04-21T00:00:00Z",
		},
		Target: "pkg:npm/full-shape-example",
		SynthesisSupplement: &exchange.SynthesisSupplement{
			ProposedPosture: exchange.ProposedPosture{
				Tier:             exchange.ProposedTierVettedFrozen,
				VersionScope:     "2.2.4",
				RationaleSummary: "full-shape rationale for round-trip coverage",
			},
			Reasoning: "multi-paragraph reasoning body in markdown",
			Summary:   "two-sentence summary of the full-shape fixture",
			ConcordanceStrengths: []exchange.ConcordanceEntry{
				{
					Topic:         "minimal supply-chain surface",
					Description:   "both analysts arrived at zero-dep conclusion independently",
					AnalystRefs:   []string{"signatory-provenance", "external-sec-v1"},
					ConclusionIDs: []string{"F005", "O001"},
					Confidence:    "HIGH",
				},
			},
			ContradictionsDetected: []exchange.ContradictionEntry{
				{
					Topic:                "release cadence interpretation",
					Description:          "provenance calls it healthy; security calls it slow",
					SupportingAnalystA:   "signatory-provenance",
					SupportingAnalystB:   "external-sec-v1",
					ConclusionIDsA:       []string{"F003"},
					ConclusionIDsB:       []string{"F011"},
					ResolutionPreference: "provenance's read; cadence is context-dependent",
				},
			},
			KeyConclusionRefs: []exchange.ConclusionRef{
				{
					OutputID:          "out-uuid-prov",
					ConclusionLocalID: "F002",
					Weight:            1,
					ForgeryResistance: "VERY HIGH",
					RelevanceNote:     "publication anchor is the load-bearing signal",
				},
			},
			Gaps:        []string{"no OSV cross-check", "transitives not audited"},
			ActionItems: []string{"pin in go.sum", "validate CreateFromVCS inputs"},
			Notes:       "confidence slightly shaded by mid-analysis upstream update",
		},
	}

	result, err := s.IngestAnalystOutput(ctx, input, "full-shape-test")
	require.NoError(t, err)

	loaded, err := s.GetAnalystOutput(ctx, result.OutputID)
	require.NoError(t, err)

	require.NotNil(t, loaded.SynthesisSupplement)
	// Deep equality across the whole supplement. If this fails, the
	// diff pinpoints which field didn't round-trip.
	assert.Equal(t, input.SynthesisSupplement, loaded.SynthesisSupplement)
}

// TestGetAnalystOutput_NonSynthesist_NoSupplement confirms that
// loading a normal analyst output (security/provenance/etc.) returns
// a nil SynthesisSupplement. Belt-and-suspenders against a future
// bug where the Get path incorrectly materializes an empty supplement
// for non-synthesist rows.
func TestGetAnalystOutput_NonSynthesist_NoSupplement(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	out := loadFixture(t, "atuin-schema-trial.json") // security-analyst fixture
	result, err := s.IngestAnalystOutput(ctx, out, "non-synthesist-test")
	require.NoError(t, err)

	loaded, err := s.GetAnalystOutput(ctx, result.OutputID)
	require.NoError(t, err)

	assert.Nil(t, loaded.SynthesisSupplement,
		"non-synthesist output must come back from Get with nil supplement")
}

// TestGetSynthesisProposal_HappyPath loads a synthesist output's
// proposed posture via the narrow GetSynthesisProposal helper — the
// read path `signatory posture accept` (M6d) will use. The helper
// reads the denormalized columns directly, avoiding the full Get
// reconstruction cost when the caller only needs the proposal.
func TestGetSynthesisProposal_HappyPath(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	input := &exchange.AnalystOutput{
		Attribution: exchange.AgentAttribution{
			AnalystID: "signatory-synthesis-v1",
			Model:     "claude-test",
			InvokedAt: "2026-04-21T00:00:00Z",
		},
		Target: "pkg:npm/accept-target",
		SynthesisSupplement: &exchange.SynthesisSupplement{
			ProposedPosture: exchange.ProposedPosture{
				Tier:             exchange.ProposedTierVettedFrozen,
				VersionScope:     "1.2.3",
				RationaleSummary: "rationale the accept verb will copy verbatim",
			},
			Reasoning: "r",
			Summary:   "s",
		},
	}
	result, err := s.IngestAnalystOutput(ctx, input, "proposal-test")
	require.NoError(t, err)

	proposal, err := s.GetSynthesisProposal(ctx, result.OutputID)
	require.NoError(t, err)
	require.NotNil(t, proposal)
	assert.Equal(t, exchange.ProposedTierVettedFrozen, proposal.Tier)
	assert.Equal(t, "1.2.3", proposal.VersionScope)
	assert.Equal(t, "rationale the accept verb will copy verbatim", proposal.RationaleSummary)
}

// TestGetSynthesisProposal_NonSynthesist_NotFound asserts the
// narrow helper refuses to return a proposal for a non-synthesist
// row. A non-synthesist row has proposed_tier NULL, so the helper
// returns ErrNotFound — the accept verb uses this to error out
// early ("that output isn't a synthesis").
func TestGetSynthesisProposal_NonSynthesist_NotFound(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	out := loadFixture(t, "atuin-schema-trial.json")
	result, err := s.IngestAnalystOutput(ctx, out, "non-synthesis-proposal-test")
	require.NoError(t, err)

	_, err = s.GetSynthesisProposal(ctx, result.OutputID)
	assert.ErrorIs(t, err, ErrNotFound,
		"non-synthesist output must not yield a proposal; caller should get ErrNotFound")
}

// TestGetSynthesisProposal_UnknownOutputID_NotFound: asking about an
// output that doesn't exist at all returns ErrNotFound, same as a
// non-synthesist row. The accept verb treats both as "no proposal
// here" and surfaces a user-facing "no such synthesis" error.
func TestGetSynthesisProposal_UnknownOutputID_NotFound(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	_, err := s.GetSynthesisProposal(ctx, "00000000-0000-0000-0000-000000000000")
	assert.ErrorIs(t, err, ErrNotFound)
}

// --- helpers ---

func contentHash(t *testing.T, out *exchange.AnalystOutput) string {
	t.Helper()
	hash, err := analystOutputContentHash(out)
	require.NoError(t, err)
	return hash
}

func assertCount(t *testing.T, s *SQLite, table, where string, arg any, expected int) {
	t.Helper()
	var got int
	err := s.db.QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM "+table+" WHERE "+where, arg,
	).Scan(&got)
	require.NoError(t, err)
	assert.Equal(t, expected, got, "row count in %s where %s", table, where)
}

// --- normalizeTargetToCanonicalURI (ingest-canonicalization) ---
//
// The 2026-04-21 dogfood surfaced two bugs in the ingest path's URI
// handling:
//
//  1. An analyst that URL-encoded the scope `@` of a scoped npm package
//     (pkg:npm/%40stripe/stripe-react-native) landed its output at a
//     parallel entity row, orphaned from the other analyst who used
//     the literal-@ canonical form. Re-dispatch cost ~60k tokens.
//  2. `normalizeTargetToCanonicalURI` only accepted canonical URIs
//     and GitHub URLs — an npmjs.com URL passed as `collected_from`
//     was rejected even though `profile.ResolveTarget` would handle
//     it fine elsewhere.
//
// Fix shape: delegate to profile.ResolveTarget for URL-form inputs,
// and percent-decode the pkg URI body to canonicalize `%40` → `@`.

func TestNormalizeTargetToCanonicalURI_ScopedNpm_PercentDecoded(t *testing.T) {
	// The bug from the dogfood: analyst emits the %40 form because
	// they transcribed from an npmjs.com URL. Should canonicalize to
	// the literal-@ form so both analysts land at the same entity.
	got, err := normalizeTargetToCanonicalURI("pkg:npm/%40stripe/stripe-react-native")
	require.NoError(t, err)
	assert.Equal(t, "pkg:npm/@stripe/stripe-react-native", got,
		"percent-encoded scope marker must be decoded to its literal form")
}

func TestNormalizeTargetToCanonicalURI_PercentEncodedVersion_Decoded(t *testing.T) {
	// %40 also appears as the version separator in versioned pkg URIs.
	// Same canonicalization path; cover it explicitly.
	got, err := normalizeTargetToCanonicalURI("pkg:npm/foo%401.2.3")
	require.NoError(t, err)
	assert.Equal(t, "pkg:npm/foo@1.2.3", got)
}

func TestNormalizeTargetToCanonicalURI_NpmjsURL_Accepted(t *testing.T) {
	// An npmjs.com URL passed as target (or collected_from via MCP)
	// must normalize to the pkg:npm/ canonical form. Pre-fix this
	// returned a "not a recognized URL form" error because the helper
	// only recognized github.com.
	got, err := normalizeTargetToCanonicalURI("https://www.npmjs.com/package/@stripe/stripe-react-native")
	require.NoError(t, err)
	assert.Equal(t, "pkg:npm/@stripe/stripe-react-native", got,
		"npmjs.com URL must resolve to the canonical pkg:npm/ form")
}

func TestNormalizeTargetToCanonicalURI_NpmjsURL_WithVersion(t *testing.T) {
	got, err := normalizeTargetToCanonicalURI("https://www.npmjs.com/package/express/v/4.18.2")
	require.NoError(t, err)
	assert.Equal(t, "pkg:npm/express@4.18.2", got)
}

func TestNormalizeTargetToCanonicalURI_InvalidPercentEncoding_Rejected(t *testing.T) {
	// Bogus percent sequences must fail loudly rather than silently
	// produce corrupted canonical URIs.
	_, err := normalizeTargetToCanonicalURI("pkg:npm/%ZZ-bogus")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "percent")
}

func TestNormalizeTargetToCanonicalURI_AlreadyCanonical_Passthrough(t *testing.T) {
	tests := []string{
		"pkg:npm/lodash",
		"pkg:npm/@types/node",
		"pkg:npm/lodash@4.17.21",
		"pkg:go/golang.org/x/mod",
		"repo:github/nvbn/thefuck",
		"identity:github/alecthomas",
	}
	for _, uri := range tests {
		t.Run(uri, func(t *testing.T) {
			got, err := normalizeTargetToCanonicalURI(uri)
			require.NoError(t, err)
			assert.Equal(t, uri, got, "already-canonical URIs pass through unchanged")
		})
	}
}

func TestNormalizeTargetToCanonicalURI_GitHubURL_Preserved(t *testing.T) {
	// Don't regress the pre-existing GitHub URL normalization path.
	got, err := normalizeTargetToCanonicalURI("https://github.com/nvbn/thefuck")
	require.NoError(t, err)
	assert.Equal(t, "repo:github/nvbn/thefuck", got)
}

// TestIngest_ScopedNpmWithPercentEncodedScope_IndexesCanonically is
// the end-to-end regression check for the dogfood bug: two ingest
// calls — one with literal @, one with %40 — must land at the SAME
// entity row. Without the fix, they land at two different entities
// and downstream synthesis-evidence gathering misses one.
func TestIngest_ScopedNpmWithPercentEncodedScope_IndexesCanonically(t *testing.T) {
	s := newTestDB(t)
	ctx := t.Context()

	lineStart := 10
	canonical := &exchange.AnalystOutput{
		Attribution: exchange.AgentAttribution{
			AnalystID: "external-sec-v1",
			Model:     "claude-test",
			InvokedAt: "2026-04-21T00:00:00Z",
		},
		Target: "pkg:npm/@stripe/stripe-react-native",
		Conclusions: []exchange.Conclusion{
			{
				ID: "F001", Verdict: "v", Rationale: "r",
				Severity: exchange.Severity{Default: exchange.SeverityLow},
				Category: "c",
				Citations: []exchange.Citation{
					{Path: "src/main.ts", LineStart: &lineStart},
				},
			},
		},
	}
	urlEncoded := &exchange.AnalystOutput{
		Attribution: exchange.AgentAttribution{
			AnalystID: "signatory-provenance",
			Model:     "claude-test",
			InvokedAt: "2026-04-21T00:00:00Z",
		},
		Target: "pkg:npm/%40stripe/stripe-react-native",
		Conclusions: []exchange.Conclusion{
			{
				ID: "F001", Verdict: "v", Rationale: "r",
				Severity: exchange.Severity{Default: exchange.SeverityLow},
				Category: "c",
				Citations: []exchange.Citation{
					{Scope: &exchange.ScopeRef{Kind: exchange.ScopeKindWorkspace, Path: "."}},
				},
			},
		},
	}

	r1, err := s.IngestAnalystOutput(ctx, canonical, "canonical")
	require.NoError(t, err)
	r2, err := s.IngestAnalystOutput(ctx, urlEncoded, "%40-form")
	require.NoError(t, err)

	assert.Equal(t, r1.EntityID, r2.EntityID,
		"%40-scope ingest must land under the same entity as literal-@ ingest — no orphan entities from percent-encoding drift")
}
