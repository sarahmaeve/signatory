package htmlreport

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/exchange"
)

// fixtureSynthesis returns a synthesis output with every supplement
// section populated. Held in a helper so multiple tests can mutate a
// fresh copy rather than sharing state.
func fixtureSynthesis() *exchange.AnalystOutput {
	return &exchange.AnalystOutput{
		Attribution: exchange.AgentAttribution{
			AnalystID: "signatory-synthesis-v1",
			Model:     "claude-opus-4-7",
			InvokedAt: "2026-05-06T12:00:00Z",
		},
		Target:       "pkg:npm/example",
		TargetCommit: "deadbeefcafef00d",
		SynthesisSupplement: &exchange.SynthesisSupplement{
			ProposedPosture: exchange.ProposedPosture{
				Tier:             exchange.ProposedTierVettedFrozen,
				VersionScope:     "1.2.3",
				RationaleSummary: "concise rationale",
			},
			Reasoning: "first paragraph of reasoning.\n\nsecond paragraph; mentions <script>alert('x')</script> as a literal.",
			Summary:   "two-sentence summary.",
			ConcordanceStrengths: []exchange.ConcordanceEntry{
				{
					Topic:         "minimal dep surface",
					Description:   "both analysts read zero deps",
					AnalystRefs:   []string{"signatory-security-v1", "signatory-provenance-v1"},
					ConclusionIDs: []string{"F001", "F010"},
					Confidence:    "HIGH",
				},
			},
			ContradictionsDetected: []exchange.ContradictionEntry{
				{
					Topic:                "release cadence",
					Description:          "provenance healthy; security slow",
					SupportingAnalystA:   "signatory-provenance-v1",
					SupportingAnalystB:   "signatory-security-v1",
					ConclusionIDsA:       []string{"F012"},
					ConclusionIDsB:       []string{"F003"},
					ResolutionPreference: "prefer provenance's read",
				},
			},
			KeyConclusionRefs: []exchange.ConclusionRef{
				// Intentionally out of weight order — the renderer
				// must sort ascending by weight (1 first).
				{
					OutputID:          "out-sec-0001",
					ConclusionLocalID: "F003",
					Weight:            2,
					ForgeryResistance: "HIGH",
				},
				{
					OutputID:          "out-prov-0001",
					ConclusionLocalID: "F012",
					Weight:            1,
					ForgeryResistance: "VERY HIGH",
					RelevanceNote:     "publication anchor",
				},
				{
					// Dangling: not in LinkPlan.ConclusionPages.
					OutputID:          "out-missing-9999",
					ConclusionLocalID: "F999",
					Weight:            3,
					ForgeryResistance: "LOW",
				},
			},
			Gaps:        []string{"no OSV cross-check", "transitives not audited"},
			ActionItems: []string{"pin in package-lock", "open upstream issue"},
			Notes:       "calibration shaded by mid-analysis update.",
		},
	}
}

// fixtureLinkPlan returns a LinkPlan that resolves every reference in
// fixtureSynthesis except the deliberately-dangling "out-missing-9999"
// / "F999" pair.
func fixtureLinkPlan() *LinkPlan {
	return &LinkPlan{
		ConclusionPages: map[ConclusionKey]string{
			{OutputID: "out-sec-0001", LocalID: "F003"}:  "conclusions/out-sec-0-F003.html",
			{OutputID: "out-prov-0001", LocalID: "F012"}: "conclusions/out-prov--F012.html",
			{OutputID: "out-sec-0001", LocalID: "F001"}:  "conclusions/out-sec-0-F001.html",
			{OutputID: "out-prov-0001", LocalID: "F010"}: "conclusions/out-prov--F010.html",
		},
		AnalystPages: map[string]string{
			"signatory-security-v1":   "analysts/signatory-security-v1-r1.html",
			"signatory-provenance-v1": "analysts/signatory-provenance-v1-r1.html",
		},
		AnalystToOutput: map[string]string{
			"signatory-security-v1":   "out-sec-0001",
			"signatory-provenance-v1": "out-prov-0001",
		},
		Dangling: []DanglingRef{
			{OutputID: "out-missing-9999", LocalID: "F999", Reason: "output not loaded"},
		},
	}
}

func TestRenderSynthesisIndex_HappyPath(t *testing.T) {
	in := SynthesisIndexInput{
		Synth:       fixtureSynthesis(),
		ShortName:   "example",
		Plan:        fixtureLinkPlan(),
		GeneratedAt: "2026-05-06T13:00:00Z",
		Version:     "v0.1.0-test",
	}

	var buf bytes.Buffer
	require.NoError(t, RenderSynthesisIndex(&buf, in))
	out := buf.String()

	t.Run("HTML shell", func(t *testing.T) {
		assert.Contains(t, out, "<!DOCTYPE html>")
		assert.Contains(t, out, `<html lang="en">`)
		assert.Contains(t, out, "</html>")
		assert.Contains(t, out, `<link rel="stylesheet" href="assets/style.css">`)
	})

	t.Run("agent banner above the title", func(t *testing.T) {
		assert.Contains(t, out, `<div class="agent-banner" role="banner">SIGNATORY AGENT REPORT</div>`)
		// Banner precedes the h1 in document order so it stamps the
		// page above the trust-assessment title.
		bannerIdx := strings.Index(out, "SIGNATORY AGENT REPORT")
		titleIdx := strings.Index(out, "<h1>Trust Assessment")
		require.Greater(t, bannerIdx, 0)
		require.Greater(t, titleIdx, 0)
		assert.Less(t, bannerIdx, titleIdx,
			"banner must appear before the h1 title in the HTML")
	})

	t.Run("title and posture", func(t *testing.T) {
		assert.Contains(t, out, "<title>Trust Assessment: example</title>")
		// Posture call-out carries the tier-specific CSS class. The
		// class attribute may include other classes alongside; assert
		// on the token itself, not the whole attribute value.
		assert.Contains(t, out, "posture-vetted-frozen")
		assert.Contains(t, out, "vetted-frozen")
		assert.Contains(t, out, "1.2.3") // version scope
	})

	t.Run("recommended posture label and source attribution", func(t *testing.T) {
		assert.Contains(t, out, "Recommended posture:")
		assert.Contains(t, out, "(from synthesist)")
		// No recorded posture in this fixture → the second call-out
		// must NOT appear.
		assert.NotContains(t, out, "Current recorded posture:")
		assert.NotContains(t, out, "(from signatory db)")
	})

	t.Run("attribution + target metadata", func(t *testing.T) {
		assert.Contains(t, out, "signatory-synthesis-v1")
		assert.Contains(t, out, "claude-opus-4-7")
		assert.Contains(t, out, "2026-05-06T12:00:00Z")
		assert.Contains(t, out, "pkg:npm/example")
		assert.Contains(t, out, "deadbeefcafef00d")
	})

	t.Run("reasoning rendered as paragraphs with HTML escaping", func(t *testing.T) {
		// Both source paragraphs land inside <p> blocks.
		assert.Contains(t, out, "<p>first paragraph of reasoning.</p>")
		assert.Contains(t, out, "<p>second paragraph; mentions &lt;script&gt;alert(&#39;x&#39;)&lt;/script&gt; as a literal.</p>")
		// Crucially, the literal <script> tag must NOT be present
		// unescaped — that would be an XSS injection.
		assert.NotContains(t, out, "<script>alert")
	})

	t.Run("summary and notes present", func(t *testing.T) {
		assert.Contains(t, out, "two-sentence summary.")
		assert.Contains(t, out, "calibration shaded by mid-analysis update.")
	})

	t.Run("concordance entry with resolved local-id link", func(t *testing.T) {
		assert.Contains(t, out, "minimal dep surface")
		assert.Contains(t, out, "both analysts read zero deps")
		assert.Contains(t, out, "HIGH")
		// F001 owned by signatory-security-v1 → resolves via AnalystToOutput.
		assert.Contains(t, out, `href="conclusions/out-sec-0-F001.html"`)
		// F010 owned by signatory-provenance-v1.
		assert.Contains(t, out, `href="conclusions/out-prov--F010.html"`)
	})

	t.Run("contradiction entry with both sides linked", func(t *testing.T) {
		assert.Contains(t, out, "release cadence")
		// Apostrophe in source prose is HTML-escaped to &#39; — that
		// escaping discipline is part of the contract, not an artifact
		// of the test.
		assert.Contains(t, out, "prefer provenance&#39;s read")
		assert.Contains(t, out, `href="conclusions/out-prov--F012.html"`)
		assert.Contains(t, out, `href="conclusions/out-sec-0-F003.html"`)
	})

	t.Run("key-conclusion list items carry kc-* anchor ids", func(t *testing.T) {
		// Each <li> for a KeyConclusionRefs entry must have
		// id="kc-<output-short>-<local>" so prose mentions can
		// intra-page-link to it. Output ids in the fixture are
		// out-prov-0001 (first 8 = "out-prov") and out-sec-0001
		// (first 8 = "out-sec-", with the trailing dash).
		assert.Contains(t, out, `id="kc-out-prov-F012"`)
		assert.Contains(t, out, `id="kc-out-sec--F003"`)
	})

	t.Run("key conclusions sorted by weight, dangling rendered without link", func(t *testing.T) {
		// Weight 1 (F012) precedes weight 2 (F003) precedes weight 3 (F999).
		idxF012 := strings.Index(out, "F012")
		idxF003 := strings.Index(out, "F003")
		idxF999 := strings.Index(out, "F999")
		require.True(t, idxF012 > 0 && idxF003 > 0 && idxF999 > 0)
		assert.Less(t, idxF012, idxF003, "F012 (weight 1) must precede F003 (weight 2)")
		assert.Less(t, idxF003, idxF999, "F003 (weight 2) must precede F999 (weight 3)")

		// F012 and F003 link; F999 (dangling) does not.
		assert.Contains(t, out, `href="conclusions/out-prov--F012.html"`)
		assert.Contains(t, out, `href="conclusions/out-sec-0-F003.html"`)
		assert.NotContains(t, out, "F999.html",
			"dangling key-conclusion ref must not produce an anchor")
	})

	t.Run("gaps as ul, action items as ol", func(t *testing.T) {
		// Both lists present.
		assert.Contains(t, out, "<ul")
		assert.Contains(t, out, "<ol")
		assert.Contains(t, out, "no OSV cross-check")
		assert.Contains(t, out, "pin in package-lock")
	})

	t.Run("footer carries generated-at and version", func(t *testing.T) {
		assert.Contains(t, out, "2026-05-06T13:00:00Z")
		assert.Contains(t, out, "v0.1.0-test")
	})
}

// TestRenderSynthesisIndex_TargetURL renders the target URI as a
// clickable anchor when TargetURL is supplied; otherwise as plain
// text. The anchor value is exactly what the caller passes — no
// rewriting in the renderer.
func TestRenderSynthesisIndex_TargetURL(t *testing.T) {
	t.Run("with URL → anchor", func(t *testing.T) {
		in := SynthesisIndexInput{
			Synth:     fixtureSynthesis(),
			ShortName: "example",
			TargetURL: "https://pypi.org/project/example/",
			Plan:      fixtureLinkPlan(),
		}
		var buf bytes.Buffer
		require.NoError(t, RenderSynthesisIndex(&buf, in))
		out := buf.String()
		assert.Contains(t, out, `<a href="https://pypi.org/project/example/">pkg:npm/example</a>`)
	})

	t.Run("without URL → plain text", func(t *testing.T) {
		in := SynthesisIndexInput{
			Synth:     fixtureSynthesis(),
			ShortName: "example",
			Plan:      fixtureLinkPlan(),
		}
		var buf bytes.Buffer
		require.NoError(t, RenderSynthesisIndex(&buf, in))
		out := buf.String()
		assert.Contains(t, out, "<dd>pkg:npm/example</dd>")
		assert.NotContains(t, out, `<a href="https://`)
	})
}

// TestRenderSynthesisIndex_RecordedPosture renders a second call-out
// when RecordedPosture is non-nil, with parallel structure to the
// recommended one and a different source attribution. When the
// recorded tier disagrees with the synthesist's recommendation, a
// `differs-from-recommended` class drives a clashing CSS treatment.
func TestRenderSynthesisIndex_RecordedPosture(t *testing.T) {
	t.Run("differs from recommendation → clash class applied", func(t *testing.T) {
		// Fixture's ProposedPosture is vetted-frozen; recorded is
		// trusted-for-now → tiers differ.
		in := SynthesisIndexInput{
			Synth:     fixtureSynthesis(),
			ShortName: "example",
			Plan:      fixtureLinkPlan(),
			RecordedPosture: &RecordedPosture{
				Tier:      "trusted-for-now",
				Version:   "1.0.0",
				SetBy:     "sarah@example.com",
				SetAt:     "2026-04-30T12:00:00Z",
				Rationale: "audited last quarter; baseline ok",
			},
			GeneratedAt: "2026-05-06T13:00:00Z",
			Version:     "v0.1.0-test",
		}
		var buf bytes.Buffer
		require.NoError(t, RenderSynthesisIndex(&buf, in))
		out := buf.String()

		assert.Contains(t, out, "Recommended posture:")
		assert.Contains(t, out, "(from synthesist)")

		assert.Contains(t, out, "Current recorded posture:")
		assert.Contains(t, out, "trusted-for-now")
		assert.Contains(t, out, "(from signatory db)")
		assert.Contains(t, out, "audited last quarter; baseline ok")
		assert.Contains(t, out, "sarah@example.com")
		assert.Contains(t, out, "2026-04-30T12:00:00Z")
		// Tier-specific CSS class on the recorded call-out.
		assert.Contains(t, out, "posture-trusted-for-now")
		// Clash class present because tiers differ.
		assert.Contains(t, out, "differs-from-recommended")
	})

	t.Run("agrees with recommendation → no clash class", func(t *testing.T) {
		in := SynthesisIndexInput{
			Synth:     fixtureSynthesis(),
			ShortName: "example",
			Plan:      fixtureLinkPlan(),
			RecordedPosture: &RecordedPosture{
				// Same tier as the synthesist's proposal but
				// different rationale — this test covers the
				// not-fully-equal-but-tiers-match path.
				Tier:      string(exchange.ProposedTierVettedFrozen),
				Rationale: "different rationale than the synthesist's",
			},
			GeneratedAt: "2026-05-06T13:00:00Z",
			Version:     "v0.1.0-test",
		}
		var buf bytes.Buffer
		require.NoError(t, RenderSynthesisIndex(&buf, in))
		out := buf.String()

		assert.Contains(t, out, "Current recorded posture:")
		assert.NotContains(t, out, "differs-from-recommended",
			"clash class must not appear when tiers agree")
		// Rationale differs → still rendered (apostrophe HTML-escaped
		// per the renderer's escaping discipline; assert on a
		// punctuation-free substring to avoid the &#39; mismatch).
		assert.Contains(t, out, "different rationale than the synthesist")
	})

	// Operator accepted the recommendation verbatim with --yes:
	// posture row is a faithful copy of tier + version + rationale.
	// The recorded call-out collapses to heading + set-by metadata
	// so the duplicate prose isn't shown twice on the page.
	t.Run("recorded matches recommendation → suppress duplicate prose", func(t *testing.T) {
		in := SynthesisIndexInput{
			Synth:     fixtureSynthesis(), // ProposedPosture is vetted-frozen / 1.2.3 / "concise rationale"
			ShortName: "example",
			Plan:      fixtureLinkPlan(),
			RecordedPosture: &RecordedPosture{
				Tier:      string(exchange.ProposedTierVettedFrozen),
				Version:   "1.2.3",
				Rationale: "concise rationale",
				SetBy:     "sarah@example.com",
				SetAt:     "2026-04-30T12:00:00Z",
			},
			GeneratedAt: "2026-05-06T13:00:00Z",
			Version:     "v0.1.0-test",
		}
		var buf bytes.Buffer
		require.NoError(t, RenderSynthesisIndex(&buf, in))
		out := buf.String()

		// The recorded heading + (from signatory db) source still
		// renders.
		assert.Contains(t, out, "Current recorded posture:")
		assert.Contains(t, out, "(from signatory db)")
		// Set-by metadata still renders.
		assert.Contains(t, out, "sarah@example.com")
		assert.Contains(t, out, "2026-04-30T12:00:00Z")

		// The recorded callout's rationale paragraph and version-scope
		// span are suppressed because they'd just duplicate the
		// recommended call-out above. The strings still appear in
		// the recommended block; what matters is they don't appear
		// TWICE.
		recordedIdx := strings.Index(out, "Current recorded posture:")
		require.Greater(t, recordedIdx, 0)
		recordedBlock := out[recordedIdx:]
		assert.NotContains(t, recordedBlock, "concise rationale",
			"recorded block should not duplicate the rationale paragraph")
		assert.NotContains(t, recordedBlock, `<span class="version-scope">`,
			"recorded block should not duplicate the version-scope span")
	})
}

// TestRenderSynthesisIndex_ReasoningLinkifiesLocalIDs asserts that
// occurrences of known local ids inside reasoning prose become
// clickable anchors to the in-page key-conclusion entries. Words
// that merely contain a local id as a substring (e.g. "F0010") must
// NOT match — word-boundary semantics.
func TestRenderSynthesisIndex_ReasoningLinkifiesLocalIDs(t *testing.T) {
	synth := fixtureSynthesis()
	synth.SynthesisSupplement.Reasoning = "first paragraph cites F012 by id.\n" +
		"\n" +
		"second paragraph: F003 (high) is the load-bearing finding; F0010 must not match."

	in := SynthesisIndexInput{
		Synth:     synth,
		ShortName: "example",
		Plan:      fixtureLinkPlan(),
	}
	var buf bytes.Buffer
	require.NoError(t, RenderSynthesisIndex(&buf, in))
	out := buf.String()

	// F012 → linked to its key-conclusion anchor (anchor uses the
	// first 8 chars of "out-prov-0001" = "out-prov", no trailing
	// dash).
	assert.Contains(t, out, `<a href="#kc-out-prov-F012">F012</a>`)
	// F003 → linked (anchor uses "out-sec-" with a trailing dash, so
	// the slug has a double dash before F003).
	assert.Contains(t, out, `<a href="#kc-out-sec--F003">F003</a>`)
	// F0010 must NOT have been linked through F001 — needs a F0010
	// id in the plan to link, and there isn't one.
	assert.NotContains(t, out, `>F001</a>0`)
}

// TestRenderSynthesisIndex_RejectsNonSynthesis guards the precondition
// that the input must carry a SynthesisSupplement. The renderer is
// pure, but this is a programming-error path — a caller that passes
// a non-synthesis output has a bug, and we return an error rather
// than panic on the nil dereference.
func TestRenderSynthesisIndex_RejectsNonSynthesis(t *testing.T) {
	in := SynthesisIndexInput{
		Synth: &exchange.AnalystOutput{
			Attribution: exchange.AgentAttribution{AnalystID: "signatory-security-v1"},
			Target:      "pkg:npm/example",
			// SynthesisSupplement intentionally nil.
		},
		ShortName:   "example",
		Plan:        &LinkPlan{},
		GeneratedAt: "2026-05-06T13:00:00Z",
		Version:     "v0.1.0-test",
	}

	var buf bytes.Buffer
	err := RenderSynthesisIndex(&buf, in)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "synthesis_supplement")
}

// TestRenderSynthesisIndex_EscapesShortNameInTitle asserts that an
// LLM-/operator-controlled short name cannot inject markup into the
// title. ShortName flows from the entity row, which is operator
// data, so the threat model is "future entity-naming flexibility",
// not "untrusted package registry input" — but escaping is uniform
// regardless.
func TestRenderSynthesisIndex_EscapesShortNameInTitle(t *testing.T) {
	in := SynthesisIndexInput{
		Synth:       fixtureSynthesis(),
		ShortName:   `evil"<script>name`,
		Plan:        fixtureLinkPlan(),
		GeneratedAt: "2026-05-06T13:00:00Z",
		Version:     "v0.1.0-test",
	}

	var buf bytes.Buffer
	require.NoError(t, RenderSynthesisIndex(&buf, in))
	out := buf.String()

	assert.NotContains(t, out, "<script>name")
	assert.Contains(t, out, "evil&#34;&lt;script&gt;name")
}
