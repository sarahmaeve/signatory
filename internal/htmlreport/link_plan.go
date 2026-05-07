package htmlreport

import (
	"fmt"

	"github.com/sarahmaeve/signatory/internal/exchange"
)

// BuildLinkPlan walks a synthesis output's references against a set of
// loaded analyst outputs and produces the LinkPlan that the renderers
// consume.
//
// Inputs:
//
//   - synth: the synthesis AnalystOutput. Must carry a non-nil
//     SynthesisSupplement to produce any plan entries; a nil
//     supplement returns an empty plan (and no dangling refs) so the
//     function is safe to call defensively.
//   - loaded: a map keyed by analyst-output id. The CLI builds it by
//     calling GetAnalystOutput for each distinct OutputID in the
//     supplement's KeyConclusionRefs. Concordance and contradiction
//     entries don't carry output ids; they're resolved through
//     AnalystToOutput, which is populated from the loaded set.
//
// Output:
//
//   - *LinkPlan: ConclusionPages keyed by (output-id, local-id) for
//     every reference that resolves to a loaded conclusion;
//     AnalystPages keyed by analyst-id; AnalystToOutput keyed by
//     analyst-id mapping back to the row id; Dangling listing every
//     unresolvable KeyConclusionRefs entry (concordance and
//     contradiction misses are silently dropped per design §0).
//
// Resolution rules:
//
//   - KeyConclusionRefs carry an explicit OutputID. A ref resolves
//     iff that output is loaded AND contains a Conclusion with the
//     matching local id. Misses produce a DanglingRef with a Reason
//     distinguishing "output not loaded" from "local id absent in
//     output".
//   - ConcordanceStrengths carry AnalystRefs but no OutputID. For
//     each (AnalystRefs, ConclusionIDs) entry, walk AnalystRefs in
//     order; the first analyst-id whose loaded output contains the
//     local id wins. Misses are silently dropped — the renderer
//     falls back to plain text.
//   - ContradictionsDetected carry SupportingAnalystA and
//     SupportingAnalystB. Each side resolves independently against
//     its own analyst.
//   - Multiply-referenced conclusions deduplicate naturally because
//     the map key is (output-id, local-id) — adding a second time is
//     a no-op.
func BuildLinkPlan(synth *exchange.AnalystOutput, loaded map[string]*exchange.AnalystOutput) *LinkPlan {
	plan := &LinkPlan{
		ConclusionPages: map[ConclusionKey]string{},
		AnalystPages:    map[string]string{},
		AnalystToOutput: map[string]string{},
	}

	// Populate AnalystPages and AnalystToOutput from the loaded set.
	// Both maps key on analyst-id; the directory writer derives the
	// per-page slug from the round number.
	for outputID, ao := range loaded {
		if ao == nil {
			continue
		}
		analystID := ao.Attribution.AnalystID
		if analystID == "" {
			continue
		}
		plan.AnalystPages[analystID] = analystPageSlug(analystID, ao.Attribution.Round)
		plan.AnalystToOutput[analystID] = outputID
	}

	// No supplement → empty plan. Synthesis renderers reject the
	// case earlier; this is purely defensive.
	if synth == nil || synth.SynthesisSupplement == nil {
		return plan
	}
	s := synth.SynthesisSupplement

	// KeyConclusionRefs: explicit output id; misses become Dangling.
	for _, ref := range s.KeyConclusionRefs {
		ao, ok := loaded[ref.OutputID]
		if !ok || ao == nil {
			plan.Dangling = append(plan.Dangling, DanglingRef{
				OutputID: ref.OutputID,
				LocalID:  ref.ConclusionLocalID,
				Reason:   "output not loaded",
			})
			continue
		}
		if !hasConclusion(ao, ref.ConclusionLocalID) {
			plan.Dangling = append(plan.Dangling, DanglingRef{
				OutputID: ref.OutputID,
				LocalID:  ref.ConclusionLocalID,
				Reason:   "local id absent in output",
			})
			continue
		}
		key := ConclusionKey{OutputID: ref.OutputID, LocalID: ref.ConclusionLocalID}
		plan.ConclusionPages[key] = conclusionPageSlug(ref.OutputID, ref.ConclusionLocalID)
	}

	// Concordance: walk AnalystRefs to find the owning output.
	for _, c := range s.ConcordanceStrengths {
		for _, localID := range c.ConclusionIDs {
			tryAddConcordanceRef(plan, loaded, c.AnalystRefs, localID)
		}
	}

	// Contradiction: each side resolves independently.
	for _, c := range s.ContradictionsDetected {
		for _, localID := range c.ConclusionIDsA {
			tryAddConcordanceRef(plan, loaded, []string{c.SupportingAnalystA}, localID)
		}
		for _, localID := range c.ConclusionIDsB {
			tryAddConcordanceRef(plan, loaded, []string{c.SupportingAnalystB}, localID)
		}
	}

	return plan
}

// tryAddConcordanceRef walks analystRefs in order looking for one
// whose loaded output carries localID. Adds to ConclusionPages on
// first match; silently returns on no match (concordance and
// contradiction misses are not Dangling per design §0).
func tryAddConcordanceRef(plan *LinkPlan, loaded map[string]*exchange.AnalystOutput, analystRefs []string, localID string) {
	for _, analystID := range analystRefs {
		outputID, ok := plan.AnalystToOutput[analystID]
		if !ok {
			continue
		}
		ao, ok := loaded[outputID]
		if !ok || ao == nil {
			continue
		}
		if !hasConclusion(ao, localID) {
			continue
		}
		key := ConclusionKey{OutputID: outputID, LocalID: localID}
		// Map write is idempotent — second hit on the same key is a
		// no-op, which gives us cross-source dedup for free.
		plan.ConclusionPages[key] = conclusionPageSlug(outputID, localID)
		return
	}
}

// hasConclusion reports whether ao carries a Conclusion with id ==
// localID. Linear scan; conclusion lists are small (single-digit to
// low-double-digit) in practice.
func hasConclusion(ao *exchange.AnalystOutput, localID string) bool {
	for _, c := range ao.Conclusions {
		if c.ID == localID {
			return true
		}
	}
	return false
}

// conclusionPageSlug builds the relative path for a conclusion page.
// Format: conclusions/<output-id-short>-<local-id>.html. The
// output-id-short prefix prevents collisions when two analysts emit
// the same local id (security and provenance both happily produce
// F001).
func conclusionPageSlug(outputID, localID string) string {
	return fmt.Sprintf("conclusions/%s-%s.html", shortOutputID(outputID), localID)
}

// analystPageSlug builds the relative path for an analyst page.
// Format: analysts/<analyst-id>-r<round>.html. Round 0 (omitted in
// the supplement) renders as r0; multi-round outputs surface their
// round in the path so a future iteration that wants both rounds in
// the same report can land them at distinct slugs without renaming.
func analystPageSlug(analystID string, round int) string {
	return fmt.Sprintf("analysts/%s-r%d.html", analystID, round)
}
