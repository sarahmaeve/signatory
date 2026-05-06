package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/exchange"
)

// TestShowSynthesis_HappyPath_RendersAllSections exercises the full
// synthesis-supplement shape: every optional array populated, every
// string non-empty. The rendered markdown should include the
// posture tier, reasoning, summary, a concordance entry, a
// contradiction entry, ranked key conclusions, gaps, action items,
// and notes. This is the "mod-synthesis.md layout" target from
// design/m6-synthesis-contract.md §6 M6e.
func TestShowSynthesis_HappyPath_RendersAllSections(t *testing.T) {
	g := newTestGlobals(t)
	outputID := ingestSynthesisForAccept(t, g, &exchange.AnalystOutput{
		Attribution: exchange.AgentAttribution{
			AnalystID: "signatory-synthesis-v1",
			// Model and InvokedAt server-stamped at ingest.
		},
		Target: "pkg:npm/show-synthesis-example",
		SynthesisSupplement: &exchange.SynthesisSupplement{
			ProposedPosture: exchange.ProposedPosture{
				Tier:             exchange.ProposedTierVettedFrozen,
				VersionScope:     "2.2.4",
				RationaleSummary: "short rationale",
			},
			Reasoning: "multi-paragraph reasoning body",
			Summary:   "two-sentence summary",
			ConcordanceStrengths: []exchange.ConcordanceEntry{
				{
					Topic:         "minimal dependency surface",
					Description:   "both analysts arrived at zero-dep independently",
					AnalystRefs:   []string{"signatory-provenance", "external-sec-v1"},
					ConclusionIDs: []string{"F005", "O001"},
					Confidence:    "HIGH",
				},
			},
			ContradictionsDetected: []exchange.ContradictionEntry{
				{
					Topic:                "release cadence",
					Description:          "provenance healthy; security slow",
					SupportingAnalystA:   "signatory-provenance",
					SupportingAnalystB:   "external-sec-v1",
					ConclusionIDsA:       []string{"F003"},
					ConclusionIDsB:       []string{"F011"},
					ResolutionPreference: "prefer provenance's read",
				},
			},
			KeyConclusionRefs: []exchange.ConclusionRef{
				{
					OutputID:          "abcdef0123456789",
					ConclusionLocalID: "F002",
					Weight:            1,
					ForgeryResistance: "VERY HIGH",
					RelevanceNote:     "publication anchor is the load-bearing signal",
				},
				{
					OutputID:          "fedcba9876543210",
					ConclusionLocalID: "F001",
					Weight:            2,
					ForgeryResistance: "HIGH",
				},
			},
			Gaps:        []string{"no OSV cross-check", "transitives not audited"},
			ActionItems: []string{"pin in go.sum", "validate CreateFromVCS inputs"},
			Notes:       "confidence slightly shaded by mid-analysis upstream update",
		},
	})

	cmd := &ShowSynthesisCmd{OutputID: outputID}
	stdout := captureStdout(t, func() {
		require.NoError(t, cmd.Run(g))
	})

	assert.Contains(t, stdout, "# Trust Assessment:",
		"render must carry the trust-assessment heading")
	assert.Contains(t, stdout, "show-synthesis-example",
		"render must identify the target")
	assert.Contains(t, stdout, "**Posture: vetted-frozen**",
		"render must surface the proposed tier")
	assert.Contains(t, stdout, "**Version scope: 2.2.4**",
		"render must surface the version scope when present")
	assert.Contains(t, stdout, "multi-paragraph reasoning body",
		"render must include the reasoning body verbatim")
	assert.Contains(t, stdout, "two-sentence summary",
		"render must include the summary")
	assert.Contains(t, stdout, "minimal dependency surface",
		"render must include concordance entries")
	assert.Contains(t, stdout, "release cadence",
		"render must include contradiction entries")
	assert.Contains(t, stdout, "F002",
		"render must name key conclusion local ids")
	assert.Contains(t, stdout, "VERY HIGH",
		"render must include forgery resistance labels")
	assert.Contains(t, stdout, "no OSV cross-check",
		"render must include gaps")
	assert.Contains(t, stdout, "pin in go.sum",
		"render must include action items")
	assert.Contains(t, stdout, "confidence slightly shaded",
		"render must include notes when present")
}

// TestShowSynthesis_MinimalSupplement_OmitsEmptySections asserts the
// render degrades gracefully when optional arrays are empty —
// produces the required sections (posture, reasoning, summary)
// without empty "Concordance", "Gaps", or "Action Items" headers.
func TestShowSynthesis_MinimalSupplement_OmitsEmptySections(t *testing.T) {
	g := newTestGlobals(t)
	outputID := ingestSynthesisForAccept(t, g, synthesisForAccept())

	cmd := &ShowSynthesisCmd{OutputID: outputID}
	stdout := captureStdout(t, func() {
		require.NoError(t, cmd.Run(g))
	})

	assert.Contains(t, stdout, "synthesis reasoning body")
	assert.Contains(t, stdout, "synthesis summary")
	assert.NotContains(t, stdout, "## Cross-analyst Concordance",
		"empty concordance + contradictions → omit the section")
	assert.NotContains(t, stdout, "## Key Conclusions",
		"empty key_conclusion_refs → omit the section")
	assert.NotContains(t, stdout, "## Gaps and Limitations",
		"empty gaps → omit the section")
	assert.NotContains(t, stdout, "## Action Items",
		"empty action items → omit the section")
	assert.NotContains(t, stdout, "## Notes",
		"empty notes → omit the section")
}

// TestShowSynthesis_UnknownOutputID_Errors asserts a clean error
// for a UUID that isn't in the store.
func TestShowSynthesis_UnknownOutputID_Errors(t *testing.T) {
	g := newTestGlobals(t)
	cmd := &ShowSynthesisCmd{OutputID: "00000000-0000-0000-0000-000000000000"}
	err := cmd.Run(g)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no analyst output")
}

// TestShowSynthesis_NonSynthesisOutput_Errors asserts the command
// refuses to render a non-synthesis output (security/provenance
// outputs don't carry a synthesis_supplement and have a different
// render shape). The error points to the right alternative verb.
func TestShowSynthesis_NonSynthesisOutput_Errors(t *testing.T) {
	g := newTestGlobals(t)

	lineStart := 10
	analyst := &exchange.AnalystOutput{
		Attribution: exchange.AgentAttribution{
			AnalystID: "external-sec-v1",
			// Model and InvokedAt server-stamped at ingest.
		},
		Target: "pkg:npm/non-synthesis",
		Conclusions: []exchange.Conclusion{
			{
				ID: "F001", Verdict: "v", Rationale: "r",
				Severity: exchange.Severity{Default: exchange.SeverityLow},
				Category: "c",
				Citations: []exchange.Citation{
					{Path: "src/x.go", LineStart: &lineStart},
				},
			},
		},
	}
	outputID := ingestSynthesisForAccept(t, g, analyst)

	cmd := &ShowSynthesisCmd{OutputID: outputID}
	err := cmd.Run(g)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a synthesis",
		"error must explain that the output isn't a synthesis")
	assert.Contains(t, err.Error(), "show-analyses",
		"error should point at the right alternative verb for non-synthesis outputs")
}
