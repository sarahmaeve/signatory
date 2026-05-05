package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/sarahmaeve/signatory/internal/exchange"
	"github.com/sarahmaeve/signatory/internal/store"
)

// ShowSynthesisCmd renders a synthesis output as markdown matching
// the layout of design/dogfood/*-synthesis.md (pre-M6) and
// ~/signatory-output/*-synthesis.md (M6-era). The rendered
// document IS the human-readable view; the canonical store record
// is the analyst_output row with its synthesis_supplement JSON.
// See design/m6-synthesis-contract.md D3 (store-first; markdown
// is a view).
//
// Refuses non-synthesis outputs: analyst outputs have a different
// render shape (full conclusion bodies with rationale + citations)
// and a different audience (the synthesist, not the human trust
// decision-maker). Callers wanting the full analyst content should
// use `signatory show-analyses` for listings and
// `signatory show-conclusions` for per-conclusion drill-in.
type ShowSynthesisCmd struct {
	OutputID string `arg:"" help:"Synthesis output UUID. Find it with 'signatory show-analyses' (rows whose analyst_id starts with signatory-synthesis)."`
}

func (cmd *ShowSynthesisCmd) Run(globals *Globals) error {
	ctx := globals.Context
	if ctx == nil {
		ctx = context.Background()
	}
	s, err := globals.OpenStore(ctx)
	if err != nil {
		return err
	}
	defer s.Close() //nolint:errcheck // store close on command exit; error not actionable

	out, err := s.GetAnalystOutput(ctx, cmd.OutputID)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("no analyst output with id %q in the store", cmd.OutputID)
	}
	if err != nil {
		return fmt.Errorf("load analyst output: %w", err)
	}

	if out.SynthesisSupplement == nil {
		return fmt.Errorf(
			"output %q is not a synthesis (analyst_id %q). Use `signatory show-analyses` "+
				"to list analyses and `signatory show-conclusions --target <uri>` for "+
				"per-conclusion drill-in on analyst outputs",
			cmd.OutputID, out.Attribution.AnalystID)
	}

	// Look up the entity so we can surface its short name in the
	// title. Fall back to the target URI if the entity lookup fails
	// — the render should not fail on a cosmetic lookup.
	shortName := out.Target
	if entity, err := s.FindEntityByURI(ctx, out.Target); err == nil {
		shortName = entity.ShortName
	}

	renderSynthesisMarkdown(os.Stdout, out, shortName)
	return nil
}

// renderSynthesisMarkdown writes a synthesis output's markdown
// representation to w. Pure function — no store access — so tests
// can compose it from an in-memory AnalystOutput. The caller
// supplies shortName so the title can use the human-friendly
// identifier rather than the raw canonical URI.
//
// Section order matches the mod-synthesis.md fixture layout:
// title → posture header → reasoning → summary → concordance →
// key conclusions → gaps → action items → notes. Optional sections
// are elided when their source array is empty.
func renderSynthesisMarkdown(w io.Writer, out *exchange.AnalystOutput, shortName string) {
	s := out.SynthesisSupplement

	fmt.Fprintf(w, "# Trust Assessment: %s\n\n", shortName)
	fmt.Fprintf(w, "**Posture: %s**\n", s.ProposedPosture.Tier)
	if s.ProposedPosture.VersionScope != "" {
		fmt.Fprintf(w, "**Version scope: %s**\n", s.ProposedPosture.VersionScope)
	}
	fmt.Fprintf(w, "**Synthesist: %s", out.Attribution.AnalystID)
	if out.Attribution.Model != "" {
		fmt.Fprintf(w, " (%s)", out.Attribution.Model)
	}
	fmt.Fprintln(w, "**")
	if out.Attribution.InvokedAt != "" {
		fmt.Fprintf(w, "**Invoked at: %s**\n", out.Attribution.InvokedAt)
	}
	fmt.Fprintf(w, "**Target: %s**\n\n", out.Target)

	fmt.Fprintln(w, "## Reasoning")
	fmt.Fprintln(w)
	fmt.Fprintln(w, s.Reasoning)
	fmt.Fprintln(w)

	fmt.Fprintln(w, "## Summary")
	fmt.Fprintln(w)
	fmt.Fprintln(w, s.Summary)
	fmt.Fprintln(w)

	if len(s.ConcordanceStrengths) > 0 || len(s.ContradictionsDetected) > 0 {
		fmt.Fprintln(w, "## Cross-analyst Concordance")
		fmt.Fprintln(w)
		for _, c := range s.ConcordanceStrengths {
			fmt.Fprintf(w, "**Agreement — %s", c.Topic)
			if c.Confidence != "" {
				fmt.Fprintf(w, " (%s confidence)", c.Confidence)
			}
			fmt.Fprintf(w, ".** %s", c.Description)
			if len(c.AnalystRefs) > 0 {
				fmt.Fprintf(w, " Analysts: %s.", strings.Join(c.AnalystRefs, ", "))
			}
			if len(c.ConclusionIDs) > 0 {
				fmt.Fprintf(w, " Refs: %s.", strings.Join(c.ConclusionIDs, ", "))
			}
			fmt.Fprintln(w)
			fmt.Fprintln(w)
		}
		for _, c := range s.ContradictionsDetected {
			fmt.Fprintf(w, "**Contradiction — %s.** %s\n", c.Topic, c.Description)
			if c.SupportingAnalystA != "" {
				fmt.Fprintf(w, "  - %s: %s\n", c.SupportingAnalystA, strings.Join(c.ConclusionIDsA, ", "))
			}
			if c.SupportingAnalystB != "" {
				fmt.Fprintf(w, "  - %s: %s\n", c.SupportingAnalystB, strings.Join(c.ConclusionIDsB, ", "))
			}
			if c.ResolutionPreference != "" {
				fmt.Fprintf(w, "  - Resolution: %s\n", c.ResolutionPreference)
			}
			fmt.Fprintln(w)
		}
	}

	if len(s.KeyConclusionRefs) > 0 {
		fmt.Fprintln(w, "## Key Conclusions (ranked by weight on the posture decision)")
		fmt.Fprintln(w)
		// Sort by ascending weight (1 = highest-ranked) so the
		// narrative reads "most important first."
		sorted := slices.Clone(s.KeyConclusionRefs)
		slices.SortStableFunc(sorted, func(a, b exchange.ConclusionRef) int {
			return a.Weight - b.Weight
		})
		for _, r := range sorted {
			fmt.Fprintf(w, "%d. **%s** (output %s).", r.Weight, r.ConclusionLocalID, shortOutputID(r.OutputID))
			if r.ForgeryResistance != "" {
				fmt.Fprintf(w, " Forgery resistance: %s.", r.ForgeryResistance)
			}
			if r.RelevanceNote != "" {
				fmt.Fprintf(w, " %s", r.RelevanceNote)
			}
			fmt.Fprintln(w)
		}
		fmt.Fprintln(w)
	}

	if len(s.Gaps) > 0 {
		fmt.Fprintln(w, "## Gaps and Limitations")
		fmt.Fprintln(w)
		for _, g := range s.Gaps {
			fmt.Fprintf(w, "- %s\n", g)
		}
		fmt.Fprintln(w)
	}

	if len(s.ActionItems) > 0 {
		fmt.Fprintln(w, "## Action Items")
		fmt.Fprintln(w)
		for i, a := range s.ActionItems {
			fmt.Fprintf(w, "%d. %s\n", i+1, a)
		}
		fmt.Fprintln(w)
	}

	if s.Notes != "" {
		fmt.Fprintln(w, "## Notes")
		fmt.Fprintln(w)
		fmt.Fprintln(w, s.Notes)
		fmt.Fprintln(w)
	}
}

// shortOutputID returns the first 8 characters of a UUID for
// compact display, preserving enough prefix to disambiguate in
// practice while keeping the rendered output scannable.
func shortOutputID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}
