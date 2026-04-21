package synthesis_test

// Integration test for the M6b evidence assembler. Lives in the
// synthesis_test package (not synthesis) so it exercises the public
// API only and can import *store.SQLite without import-cycle gymnastics.

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/exchange"
	"github.com/sarahmaeve/signatory/internal/store"
	"github.com/sarahmaeve/signatory/internal/synthesis"
)

// TestAssemble_WithRealStore wires the assembler to a fresh SQLite
// store and walks a full ingest → assemble round-trip. Catches
// bugs the fake-store unit tests can't see: SQL scan mismatches,
// migration-column drift, JSON unmarshaling of the full Conclusion
// tree. Two inputs go in — a real analyst output and a synthesis
// output — and only the analyst output should surface in the
// Evidence (D9 filter tested end-to-end).
func TestAssemble_WithRealStore(t *testing.T) {
	ctx := context.Background()

	dir := t.TempDir()
	s, err := store.OpenSQLite(ctx, filepath.Join(dir, "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	lineStart := 42
	analyst := &exchange.AnalystOutput{
		Attribution: exchange.AgentAttribution{
			AnalystID: "external-sec-v1",
			Model:     "claude-test",
			InvokedAt: "2026-04-21T00:00:00Z",
		},
		Target: "pkg:npm/integration-example",
		Conclusions: []exchange.Conclusion{
			{
				ID:        "F001",
				Verdict:   "integration-test finding",
				Rationale: "round-trip rationale",
				Severity:  exchange.Severity{Default: exchange.SeverityMedium},
				Category:  "injection",
				Citations: []exchange.Citation{
					{Path: "src/handler.go", LineStart: &lineStart},
				},
			},
		},
	}
	_, err = s.IngestAnalystOutput(ctx, analyst, "integration-test-analyst")
	require.NoError(t, err)

	synth := &exchange.AnalystOutput{
		Attribution: exchange.AgentAttribution{
			AnalystID: "signatory-synthesis-v1",
			Model:     "claude-test",
			InvokedAt: "2026-04-21T00:05:00Z",
		},
		Target: "pkg:npm/integration-example",
		SynthesisSupplement: &exchange.SynthesisSupplement{
			ProposedPosture: exchange.ProposedPosture{
				Tier:             exchange.ProposedTierTrustedForNow,
				RationaleSummary: "integration-test synthesis rationale",
			},
			Reasoning: "integration-test reasoning body",
			Summary:   "integration-test summary",
		},
	}
	_, err = s.IngestAnalystOutput(ctx, synth, "integration-test-synth")
	require.NoError(t, err)

	// Assemble via the real store. Evidence should carry exactly
	// one Analysis (the analyst output), with its content intact.
	// The synthesis output must NOT appear — D9 filter.
	ev, err := synthesis.New(s).Assemble(ctx, "pkg:npm/integration-example")
	require.NoError(t, err)
	require.NotNil(t, ev)

	assert.Equal(t, "pkg:npm/integration-example", ev.CanonicalURI)
	require.Len(t, ev.Analyses, 1,
		"only the analyst output should land in Evidence; the synthesis is filtered per D9")
	assert.Equal(t, "external-sec-v1", ev.Analyses[0].AnalystID)
	require.Len(t, ev.Analyses[0].Conclusions, 1)
	assert.Equal(t, "integration-test finding", ev.Analyses[0].Conclusions[0].Verdict)
	assert.Equal(t, "round-trip rationale", ev.Analyses[0].Conclusions[0].Rationale)
	require.Len(t, ev.Analyses[0].Conclusions[0].Citations, 1)
	require.NotNil(t, ev.Analyses[0].Conclusions[0].Citations[0].LineStart)
	assert.Equal(t, 42, *ev.Analyses[0].Conclusions[0].Citations[0].LineStart)
}
