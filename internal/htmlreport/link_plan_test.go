package htmlreport

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/exchange"
)

// linkPlanFixtureSecurity returns a security-analyst output carrying
// F001, F003, F005 — used as one of the two analyst outputs that
// linkPlan tests load. Phase A's link plan rule: a conclusion gets
// a page iff it is referenced anywhere in the synthesis.
func linkPlanFixtureSecurity() *exchange.AnalystOutput {
	return &exchange.AnalystOutput{
		Attribution: exchange.AgentAttribution{
			AnalystID: "signatory-security-v1",
			Round:     1,
		},
		Conclusions: []exchange.Conclusion{
			{ID: "F001", Verdict: "v1", Severity: exchange.Severity{Default: exchange.SeverityHigh}},
			{ID: "F003", Verdict: "v3", Severity: exchange.Severity{Default: exchange.SeverityMedium}},
			{ID: "F005", Verdict: "v5", Severity: exchange.Severity{Default: exchange.SeverityLow}},
		},
	}
}

// linkPlanFixtureProvenance returns a provenance-analyst output
// carrying F010, F012. F012 is referenced by both KeyConclusionRefs
// AND a contradiction entry — the test asserts on dedup.
func linkPlanFixtureProvenance() *exchange.AnalystOutput {
	return &exchange.AnalystOutput{
		Attribution: exchange.AgentAttribution{
			AnalystID: "signatory-provenance-v1",
			Round:     2,
		},
		Conclusions: []exchange.Conclusion{
			{ID: "F010", Verdict: "v10", Severity: exchange.Severity{Default: exchange.SeverityPositive}},
			{ID: "F012", Verdict: "v12", Severity: exchange.Severity{Default: exchange.SeverityHigh}},
		},
	}
}

func linkPlanFixtureSynthesis() *exchange.AnalystOutput {
	return &exchange.AnalystOutput{
		Attribution: exchange.AgentAttribution{AnalystID: "signatory-synthesis-v1"},
		SynthesisSupplement: &exchange.SynthesisSupplement{
			KeyConclusionRefs: []exchange.ConclusionRef{
				{OutputID: "out-sec", ConclusionLocalID: "F003", Weight: 1},
				{OutputID: "out-prov", ConclusionLocalID: "F012", Weight: 2},
				// Dangling: output known, local-id absent.
				{OutputID: "out-sec", ConclusionLocalID: "F999", Weight: 3},
				// Dangling: output unknown.
				{OutputID: "out-ghost", ConclusionLocalID: "F001", Weight: 4},
			},
			ConcordanceStrengths: []exchange.ConcordanceEntry{
				{
					Topic:         "minimal dep surface",
					AnalystRefs:   []string{"signatory-security-v1", "signatory-provenance-v1"},
					ConclusionIDs: []string{"F001", "F010"},
				},
				{
					// Concordance miss: ConclusionID does not exist in
					// any of the listed analysts' outputs. Silently
					// dropped — does NOT produce a Dangling.
					Topic:         "phantom topic",
					AnalystRefs:   []string{"signatory-security-v1"},
					ConclusionIDs: []string{"F404"},
				},
			},
			ContradictionsDetected: []exchange.ContradictionEntry{
				{
					Topic:              "release cadence",
					SupportingAnalystA: "signatory-provenance-v1",
					SupportingAnalystB: "signatory-security-v1",
					// F012 already in KeyConclusionRefs — must dedup.
					ConclusionIDsA: []string{"F012"},
					// F005 only referenced here.
					ConclusionIDsB: []string{"F005"},
				},
			},
		},
	}
}

func TestBuildLinkPlan_HappyPathAndDangling(t *testing.T) {
	synth := linkPlanFixtureSynthesis()
	loaded := map[string]*exchange.AnalystOutput{
		"out-sec":  linkPlanFixtureSecurity(),
		"out-prov": linkPlanFixtureProvenance(),
	}

	plan := BuildLinkPlan(synth, loaded)

	t.Run("AnalystPages populated for every loaded output", func(t *testing.T) {
		assert.Equal(t, "analysts/signatory-security-v1-r1.html",
			plan.AnalystPages["signatory-security-v1"])
		assert.Equal(t, "analysts/signatory-provenance-v1-r2.html",
			plan.AnalystPages["signatory-provenance-v1"])
		assert.Len(t, plan.AnalystPages, 2)
	})

	t.Run("AnalystToOutput maps each analyst-id to its output id", func(t *testing.T) {
		assert.Equal(t, "out-sec", plan.AnalystToOutput["signatory-security-v1"])
		assert.Equal(t, "out-prov", plan.AnalystToOutput["signatory-provenance-v1"])
	})

	t.Run("ConclusionPages: keys cover every resolvable reference", func(t *testing.T) {
		// From KeyConclusionRefs (resolvable):
		assert.Contains(t, plan.ConclusionPages,
			ConclusionKey{OutputID: "out-sec", LocalID: "F003"})
		assert.Contains(t, plan.ConclusionPages,
			ConclusionKey{OutputID: "out-prov", LocalID: "F012"})
		// From concordance (resolved through AnalystToOutput):
		assert.Contains(t, plan.ConclusionPages,
			ConclusionKey{OutputID: "out-sec", LocalID: "F001"})
		assert.Contains(t, plan.ConclusionPages,
			ConclusionKey{OutputID: "out-prov", LocalID: "F010"})
		// From contradiction (resolved through AnalystToOutput):
		assert.Contains(t, plan.ConclusionPages,
			ConclusionKey{OutputID: "out-sec", LocalID: "F005"})

		// Dangling KeyConclusionRefs NOT in ConclusionPages.
		assert.NotContains(t, plan.ConclusionPages,
			ConclusionKey{OutputID: "out-sec", LocalID: "F999"})
		assert.NotContains(t, plan.ConclusionPages,
			ConclusionKey{OutputID: "out-ghost", LocalID: "F001"})
		// Phantom concordance ref NOT in ConclusionPages either.
		assert.NotContains(t, plan.ConclusionPages,
			ConclusionKey{OutputID: "out-sec", LocalID: "F404"})

		// Total: 5 unique entries (F012 referenced twice but
		// deduped).
		assert.Len(t, plan.ConclusionPages, 5)
	})

	t.Run("ConclusionPages paths use <output-short>-<local>.html slug", func(t *testing.T) {
		// Output ids are short here ("out-sec", "out-prov"); the
		// renderer's shortOutputID returns them unchanged.
		assert.Equal(t, "conclusions/out-sec-F003.html",
			plan.ConclusionPages[ConclusionKey{OutputID: "out-sec", LocalID: "F003"}])
		assert.Equal(t, "conclusions/out-prov-F012.html",
			plan.ConclusionPages[ConclusionKey{OutputID: "out-prov", LocalID: "F012"}])
	})

	t.Run("Dangling lists each unresolvable KeyConclusionRef once", func(t *testing.T) {
		require.Len(t, plan.Dangling, 2)

		var byKey = map[ConclusionKey]DanglingRef{}
		for _, d := range plan.Dangling {
			byKey[ConclusionKey{OutputID: d.OutputID, LocalID: d.LocalID}] = d
		}

		// Output-not-loaded case.
		ghost, ok := byKey[ConclusionKey{OutputID: "out-ghost", LocalID: "F001"}]
		require.True(t, ok)
		assert.Contains(t, ghost.Reason, "output not loaded")

		// Local-id-absent-in-loaded-output case.
		missing, ok := byKey[ConclusionKey{OutputID: "out-sec", LocalID: "F999"}]
		require.True(t, ok)
		assert.Contains(t, missing.Reason, "local id absent")
	})

	t.Run("ResolveConcordanceID round-trip through plan helper", func(t *testing.T) {
		// Sanity: the renderer's resolver uses AnalystToOutput +
		// ConclusionPages, and BuildLinkPlan must populate both
		// such that a happy-path concordance ref resolves.
		path := plan.ResolveConcordanceID(
			[]string{"signatory-security-v1"}, "F001")
		assert.Equal(t, "conclusions/out-sec-F001.html", path)

		// Phantom resolves to "" (no entry was added).
		assert.Empty(t, plan.ResolveConcordanceID(
			[]string{"signatory-security-v1"}, "F404"))
	})
}

// TestBuildLinkPlan_NilSupplement guards: a non-synthesis output
// passed in by mistake should not panic. Returns an empty plan with
// no dangling.
func TestBuildLinkPlan_NilSupplement(t *testing.T) {
	synth := &exchange.AnalystOutput{
		Attribution: exchange.AgentAttribution{AnalystID: "signatory-security-v1"},
		// SynthesisSupplement nil.
	}
	plan := BuildLinkPlan(synth, nil)
	require.NotNil(t, plan)
	assert.Empty(t, plan.ConclusionPages)
	assert.Empty(t, plan.AnalystPages)
	assert.Empty(t, plan.Dangling)
}

// TestBuildLinkPlan_EmptyLoadedMap exercises the path where the
// synthesis carries refs but no analyst outputs were loaded — every
// KeyConclusionRef should land in Dangling.
func TestBuildLinkPlan_EmptyLoadedMap(t *testing.T) {
	synth := linkPlanFixtureSynthesis()
	plan := BuildLinkPlan(synth, map[string]*exchange.AnalystOutput{})

	assert.Empty(t, plan.ConclusionPages)
	assert.Empty(t, plan.AnalystPages)
	// All 4 KeyConclusionRefs are dangling (3 distinct + 1 normally
	// resolvable now also unresolvable).
	assert.Len(t, plan.Dangling, 4)
	for _, d := range plan.Dangling {
		assert.Contains(t, d.Reason, "output not loaded")
	}
}
