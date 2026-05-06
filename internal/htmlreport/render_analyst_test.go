package htmlreport

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/exchange"
)

// fixtureAnalystOutput returns an AnalystOutput populated with two
// conclusions (one referenced by the synthesis, one not), a positive
// absence, an observation, and a methodology trace. Exercises every
// section the analyst page renders.
func fixtureAnalystOutput() *exchange.AnalystOutput {
	signalReleaseCadence := "release-cadence"
	hitYes := true
	patternRef := "P001"
	return &exchange.AnalystOutput{
		Attribution: exchange.AgentAttribution{
			AnalystID:     "signatory-security-v1",
			Model:         "claude-opus-4-7",
			PromptVersion: "v1.2.0",
			InvokedAt:     "2026-05-06T12:00:00Z",
			Round:         2,
		},
		Target:     "pkg:npm/example",
		RoundNotes: "second pass; first round missed the release-cadence regression.",
		Conclusions: []exchange.Conclusion{
			{
				ID:         "F003",
				Verdict:    "Release cadence has slowed materially since 2025-Q3",
				Severity:   exchange.Severity{Default: exchange.SeverityHigh},
				Category:   "vitality",
				SignalType: &signalReleaseCadence,
			},
			{
				ID:       "F004",
				Verdict:  "All authors verified via gpg signatures",
				Severity: exchange.Severity{Default: exchange.SeverityPositive},
				Category: "publication",
			},
		},
		PositiveAbsences: []exchange.PositiveAbsence{
			{
				PatternChecked: "obfuscated install script",
				Description:    "no postinstall hook; no curl|sh patterns",
				Confidence:     exchange.ConfidenceThoroughlyReviewed,
				PatternRef:     &patternRef,
			},
		},
		Observations: []exchange.Observation{
			{
				ID:       "O001",
				Title:    "Maintainer trajectory clean",
				Body:     "Single author for 4 years; consistent commit cadence.\n\nNo dramatic style shifts.",
				Category: "personality",
			},
		},
		MethodologyTrace: &exchange.MethodologyCatalog{
			Source: exchange.AgentAttribution{
				AnalystID: "signatory-security-v1",
				Model:     "claude-opus-4-7",
			},
			Notes: "patterns checked this round.",
			Patterns: []exchange.MethodologyPattern{
				{
					ID:          "P001",
					SignalGroup: "publication",
					Description: "obfuscated install script",
					CollectorHint: exchange.CollectorHint{
						GrepPrecision:  exchange.GrepPrecisionHigh,
						ReasoningDepth: exchange.ReasoningDepthNone,
						MissMode:       exchange.MissModeBalanced,
					},
					HitOnTarget: &hitYes,
				},
			},
		},
	}
}

func TestRenderAnalystPage_HappyPath(t *testing.T) {
	in := AnalystPageInput{
		Output: fixtureAnalystOutput(),
		Plan: &LinkPlan{
			ConclusionPages: map[ConclusionKey]string{
				// F003 referenced by synthesis → has a page.
				{OutputID: "out-sec-0001", LocalID: "F003"}: "conclusions/out-sec-0-F003.html",
				// F004 NOT in plan → listed without a link.
			},
		},
		OutputID: "out-sec-0001",
		Page: PageContext{
			RootPrefix:  "../",
			GeneratedAt: "2026-05-06T13:00:00Z",
			Version:     "v0.1.0-test",
		},
	}

	var buf bytes.Buffer
	require.NoError(t, RenderAnalystPage(&buf, in))
	out := buf.String()

	t.Run("HTML shell with stylesheet via root prefix", func(t *testing.T) {
		assert.Contains(t, out, "<!DOCTYPE html>")
		assert.Contains(t, out, `<link rel="stylesheet" href="../assets/style.css">`)
	})

	t.Run("attribution block carries analyst-id, model, prompt-version, invoked-at, round", func(t *testing.T) {
		assert.Contains(t, out, "signatory-security-v1")
		assert.Contains(t, out, "claude-opus-4-7")
		assert.Contains(t, out, "v1.2.0")
		assert.Contains(t, out, "2026-05-06T12:00:00Z")
		assert.Contains(t, out, "round 2")
	})

	t.Run("round notes rendered as paragraphs", func(t *testing.T) {
		assert.Contains(t, out, "<p>second pass; first round missed the release-cadence regression.</p>")
	})

	t.Run("conclusion list: referenced linked, unreferenced plain", func(t *testing.T) {
		// Both conclusions appear by id + verdict.
		assert.Contains(t, out, "F003")
		assert.Contains(t, out, "Release cadence has slowed materially since 2025-Q3")
		assert.Contains(t, out, "F004")
		assert.Contains(t, out, "All authors verified via gpg signatures")

		// F003 is linked (in the plan).
		assert.Contains(t, out, `href="../conclusions/out-sec-0-F003.html"`)
		// F004 is not linked (absent from the plan).
		idxF004 := strings.Index(out, "F004")
		require.GreaterOrEqual(t, idxF004, 0)
		assert.NotContains(t, out, "F004.html",
			"F004 must not produce an anchor since it has no plan entry")

		// Severity surfaces on each entry.
		assert.Contains(t, out, "severity-high")
		assert.Contains(t, out, "severity-positive")
	})

	t.Run("positive absences verbatim", func(t *testing.T) {
		assert.Contains(t, out, "obfuscated install script")
		assert.Contains(t, out, "no postinstall hook; no curl|sh patterns")
		assert.Contains(t, out, "thoroughly_reviewed")
		assert.Contains(t, out, "P001") // pattern ref surfaces
	})

	t.Run("observations verbatim", func(t *testing.T) {
		assert.Contains(t, out, "Maintainer trajectory clean")
		// Multi-paragraph body splits.
		assert.Contains(t, out, "<p>Single author for 4 years; consistent commit cadence.</p>")
		assert.Contains(t, out, "<p>No dramatic style shifts.</p>")
	})

	t.Run("methodology trace inline", func(t *testing.T) {
		assert.Contains(t, out, "P001")
		assert.Contains(t, out, "obfuscated install script")
		assert.Contains(t, out, "high")     // grep precision
		assert.Contains(t, out, "none")     // reasoning depth
		assert.Contains(t, out, "balanced") // miss mode
		// HitOnTarget true surfaces.
		assert.Contains(t, out, "hit")
	})

	t.Run("footer with back-link to index", func(t *testing.T) {
		assert.Contains(t, out, `href="../index.html"`)
		assert.Contains(t, out, "2026-05-06T13:00:00Z")
		assert.Contains(t, out, "v0.1.0-test")
	})
}

func TestRenderAnalystPage_RejectsNilOutput(t *testing.T) {
	in := AnalystPageInput{
		Output: nil,
		Page:   PageContext{RootPrefix: "../"},
	}
	var buf bytes.Buffer
	err := RenderAnalystPage(&buf, in)
	require.Error(t, err)
}
