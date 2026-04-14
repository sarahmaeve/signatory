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

func loadAnalysisFixture(t *testing.T, relPath string) *exchange.AnalystOutput {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "design", "analysis", relPath))
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
	assertCount(t, s, "findings", "output_id = ?", result.OutputID, len(out.Findings))
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
			out := loadAnalysisFixture(t, f)
			result, err := s.IngestAnalystOutput(ctx, out, f)
			require.NoError(t, err, "ingest of %s", f)
			assert.NotEmpty(t, result.OutputID)
			assert.False(t, result.Idempotent)
		})
	}

	// Both fixtures target the same URL → should resolve to the same
	// entity (after normalization to the canonical repo:github/... form).
	var entityCount int
	require.NoError(t, s.db.QueryRow(
		`SELECT COUNT(*) FROM entities WHERE canonical_uri = ?`,
		"repo:github/nvbn/thefuck").Scan(&entityCount))
	assert.Equal(t, 1, entityCount, "both thefuck outputs should share one entity")

	var outputCount int
	require.NoError(t, s.db.QueryRow(
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
	out := loadAnalysisFixture(t, "thefuck-security-v1.json")

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

func TestIngest_FindingFields_PreservedFully(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	out := loadFixture(t, "atuin-schema-trial.json")

	result, err := s.IngestAnalystOutput(ctx, out, "")
	require.NoError(t, err)

	// Spot-check F001 — the "positive correction" finding with
	// supersession, design_intent, severity_default=positive.
	var verdict, severityDefault string
	var designIntent int
	err = s.db.QueryRowContext(ctx,
		`SELECT verdict, severity_default, design_intent
		 FROM findings WHERE output_id = ? AND finding_local_id = 'F001'`,
		result.OutputID).Scan(&verdict, &severityDefault, &designIntent)
	require.NoError(t, err)
	assert.Contains(t, verdict, "atuin-ai")
	assert.Equal(t, "positive", severityDefault)
	assert.Equal(t, 1, designIntent, "F001 has design_intent: true")

	// Supersedes row exists.
	var priorID, kind string
	err = s.db.QueryRowContext(ctx,
		`SELECT fs.prior_id, fs.kind
		 FROM finding_supersedes fs
		 INNER JOIN findings f ON fs.finding_id = f.id
		 WHERE f.output_id = ? AND f.finding_local_id = 'F001'`,
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
	// multi_user+windows). All should be in finding_severity_contexts.
	rows, err := s.db.QueryContext(ctx,
		`SELECT fsc.host_isolation, fsc.platform, fsc.value
		 FROM finding_severity_contexts fsc
		 INNER JOIN findings f ON fsc.finding_id = f.id
		 WHERE f.output_id = ? AND f.finding_local_id = 'F003'
		 ORDER BY fsc.host_isolation, fsc.platform`,
		result.OutputID)
	require.NoError(t, err)
	defer rows.Close()

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
		   SELECT id FROM findings WHERE output_id = ?
		   UNION SELECT id FROM positive_absences WHERE output_id = ?
		 )
		 GROUP BY parent_kind`,
		result.OutputID, result.OutputID)
	require.NoError(t, err)
	defer rows.Close()

	kinds := map[string]int{}
	for rows.Next() {
		var kind string
		var count int
		require.NoError(t, rows.Scan(&kind, &count))
		kinds[kind] = count
	}
	assert.Greater(t, kinds["finding"], 0, "findings have citations")
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
	defer rows.Close()

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

// --- helpers ---

func contentHash(t *testing.T, out *exchange.AnalystOutput) string {
	t.Helper()
	hash, err := analystOutputContentHash(out)
	require.NoError(t, err)
	return hash
}

func assertCount(t *testing.T, s *SQLite, table, where string, arg interface{}, expected int) {
	t.Helper()
	var got int
	err := s.db.QueryRow(
		"SELECT COUNT(*) FROM "+table+" WHERE "+where, arg,
	).Scan(&got)
	require.NoError(t, err)
	assert.Equal(t, expected, got, "row count in %s where %s", table, where)
}
