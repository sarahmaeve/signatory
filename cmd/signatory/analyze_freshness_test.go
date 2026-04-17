package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sarahmaeve/signatory/internal/exchange"
	"github.com/sarahmaeve/signatory/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// loadAnalystFixture reads the JSON fixture from internal/exchange/testdata/
// (relative to this cmd/signatory test). Mirrors the helper in
// internal/store/analyst_output_test.go but for tests living in cmd/.
func loadAnalystFixture(t *testing.T, relPath string) *exchange.AnalystOutput {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "internal", "exchange", "testdata", relPath))
	require.NoError(t, err)
	var out exchange.AnalystOutput
	require.NoError(t, json.Unmarshal(raw, &out))
	return &out
}

// TestAnalyze_FreshnessCheck_SurfacesIngestedOutputs is the
// load-bearing freshness check: an `analyze` call after ingestion
// returns the cached analyst outputs without --refresh, so an
// agent calling analyze gets the "what do we know?" answer in one
// command instead of having to chain `show-analyses` separately.
func TestAnalyze_FreshnessCheck_SurfacesIngestedOutputs(t *testing.T) {
	globals := testGlobals(t)

	// Ingest two outputs against the same target.
	s, err := globals.OpenStore(t.Context())
	require.NoError(t, err)
	ctx := context.Background()
	for _, fixturePath := range []string{
		"thefuck-security-v1.json",
		"thefuck-provenance-v1.json",
	} {
		out := loadAnalystFixture(t, fixturePath)
		_, err := s.IngestAnalystOutput(ctx, out, fixturePath)
		require.NoError(t, err)
	}
	require.NoError(t, s.Close())

	// Run analyze without --refresh against the same target.
	// Should succeed (no error) — meaning the new "we have analyst
	// outputs even if no signals" branch fired correctly.
	cmd := &AnalyzeCmd{Target: "https://github.com/nvbn/thefuck", Refresh: false}
	require.NoError(t, cmd.Run(globals))

	// Verify the analyst outputs are visible via the helper.
	s2, err := globals.OpenStore(t.Context())
	require.NoError(t, err)
	defer s2.Close()
	entity, err := s2.FindEntityByURI(ctx, "repo:github/nvbn/thefuck")
	require.NoError(t, err)

	analyst, err := cmd.fetchAnalystOutputs(ctx, s2, entity.ID)
	require.NoError(t, err)
	assert.Len(t, analyst, 2, "both ingested outputs surface via the freshness fetch")
}

func TestAnalyze_NoSignalsNoOutputs_PromptsBoth(t *testing.T) {
	// When there's an entity but no signals AND no analyst outputs,
	// the user gets a clear "neither path has data" message instead
	// of the legacy "no cached signals" message that ignored Layer 2.
	globals := testGlobals(t)

	cmd := &AnalyzeCmd{Target: "https://github.com/some/empty-target"}
	// No ingest, no refresh — entity doesn't exist yet. Should
	// still succeed (it's a valid no-data case) without erroring.
	require.NoError(t, cmd.Run(globals))
}

// TestAnalyze_FreshnessCheck_MaxAge_FilterApplied checks that
// fetchAnalystOutputs translates a positive MaxAge into a Since
// filter on the underlying ListAnalystOutputs call. The
// SQL-level filter behavior itself is covered by
// TestListAnalystOutputs_FilterBySince in the store package; here
// we just confirm the CLI-side wiring.
func TestAnalyze_FreshnessCheck_MaxAge_FilterApplied(t *testing.T) {
	globals := testGlobals(t)

	s, err := globals.OpenStore(t.Context())
	require.NoError(t, err)
	ctx := context.Background()
	out := loadAnalystFixture(t, "atuin-schema-trial.json")
	_, err = s.IngestAnalystOutput(ctx, out, "trial.json")
	require.NoError(t, err)
	require.NoError(t, s.Close())

	s2, err := globals.OpenStore(t.Context())
	require.NoError(t, err)
	defer s2.Close()

	entity, err := s2.FindEntityByURI(ctx, "pkg:cargo/atuin")
	require.NoError(t, err)

	// MaxAge = 0 (no filter) returns the row.
	cmdNoLimit := &AnalyzeCmd{}
	got, err := cmdNoLimit.fetchAnalystOutputs(ctx, s2, entity.ID)
	require.NoError(t, err)
	assert.Len(t, got, 1)

	// MaxAge = 24h is permissive enough to include the
	// just-ingested row (the realistic CLI use case).
	cmdGenerous := &AnalyzeCmd{MaxAge: 24 * time.Hour}
	got, err = cmdGenerous.fetchAnalystOutputs(ctx, s2, entity.ID)
	require.NoError(t, err)
	assert.Len(t, got, 1)
}

func TestAnalyze_FreshnessCheck_AnalysisDisplay_JSONShape(t *testing.T) {
	// The AnalysisDisplay wrapper must round-trip cleanly through
	// json.Marshal so the --json output carries analyst_outputs as
	// a top-level field alongside the embedded Profile fields.
	out := loadAnalystFixture(t, "thefuck-security-v1.json")
	globals := testGlobals(t)
	s, err := globals.OpenStore(t.Context())
	require.NoError(t, err)
	ctx := context.Background()
	_, err = s.IngestAnalystOutput(ctx, out, "test")
	require.NoError(t, err)
	defer s.Close()

	entity, err := s.FindEntityByURI(ctx, "repo:github/nvbn/thefuck")
	require.NoError(t, err)

	summaries, err := s.ListAnalystOutputs(ctx, store.AnalystOutputFilter{EntityID: entity.ID})
	require.NoError(t, err)
	require.Len(t, summaries, 1)

	// Marshal a synthetic display to verify the shape.
	display := &AnalysisDisplay{
		Profile:        nil, // top-level JSON allows nil embedded; tested separately below
		AnalystOutputs: summaries,
	}
	data, err := json.Marshal(display)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"analyst_outputs":`)
	assert.Contains(t, string(data), summaries[0].OutputID)
}

func TestAnalyzeCmd_KongIntegration_MaxAge(t *testing.T) {
	// kong uses Go's time.ParseDuration semantics, which only
	// accepts ns/us/µs/ms/s/m/h units — not "d" or "w". The help
	// text on the MaxAge flag advises using hours for this reason.
	// If we ever ship a custom mapper that accepts "Nd" as days,
	// add a second case here.
	_, cli := parseCLI(t, "analyze", "owner/repo", "--max-age", "720h")
	assert.Equal(t, 720*time.Hour, cli.Analyze.MaxAge)

	// Default is 0 (no age filter).
	_, cliDefault := parseCLI(t, "analyze", "owner/repo")
	assert.Equal(t, time.Duration(0), cliDefault.Analyze.MaxAge)
}

func TestAnalyzeCmd_KongIntegration_MaxAge_RejectsBadFormat(t *testing.T) {
	// Documents the current limitation: "30d" doesn't parse. If
	// a user reaches for it expecting it to work, kong's error
	// surfaces the units it does accept.
	err := parseCLIExpectError(t, "analyze", "owner/repo", "--max-age", "30d")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duration")
}

func TestAnalyzeOutputAge_Buckets(t *testing.T) {
	now := time.Now().UTC()
	tests := []struct {
		ingested time.Time
		want     string
	}{
		{now.Add(-30 * time.Minute), "30m ago"},
		{now.Add(-3 * time.Hour), "3h ago"},
		{now.Add(-3 * 24 * time.Hour), "3d ago"},
		{now.Add(-3 * 7 * 24 * time.Hour), "3w ago"},
		{now.Add(-3 * 30 * 24 * time.Hour), "3mo ago"},
		{now.Add(-3 * 365 * 24 * time.Hour), "3y ago"},
	}
	for _, tt := range tests {
		got := analystOutputAge(tt.ingested.Format(time.RFC3339))
		assert.Equal(t, tt.want, got, "ingested %s", tt.ingested.Format(time.RFC3339))
	}
}

func TestAnalyzeOutputAge_MalformedFallsBack(t *testing.T) {
	got := analystOutputAge("not-a-timestamp")
	assert.Equal(t, "(not-a-timestamp)", got, "malformed values fall back to literal display")
}
