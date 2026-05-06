package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/sarahmaeve/signatory/internal/exchange"
	"github.com/sarahmaeve/signatory/internal/htmlreport"
	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/store"
)

// ShowSynthesisCmd renders a synthesis output as markdown matching
// the layout of ~/signatory-output/*-synthesis.md. The rendered
// document IS the human-readable view; the canonical store record
// is the analyst_output row with its synthesis_supplement JSON
// (store-first; markdown is a view).
//
// Refuses non-synthesis outputs: analyst outputs have a different
// render shape (full conclusion bodies with rationale + citations)
// and a different audience (the synthesist, not the human trust
// decision-maker). Callers wanting the full analyst content should
// use `signatory show-analyses` for listings and
// `signatory show-conclusions` for per-conclusion drill-in.
type ShowSynthesisCmd struct {
	OutputID string `arg:"" help:"Synthesis output UUID. Find it with 'signatory show-analyses' (rows whose analyst_id starts with signatory-synthesis)."`

	// HTMLDir, when set, switches the command from "render markdown
	// to stdout" to "write a static HTML report directory under
	// HTMLDir and print the absolute path to the generated
	// index.html". The HTML site cross-links the synthesis to its
	// referenced conclusions and per-analyst pages so the operator
	// doesn't need to chain show-conclusions / show-analyses calls
	// to read the supporting findings.
	//
	// HTMLDir must already exist; the command auto-creates a
	// subdirectory <short>-<output-id-short> inside it. v0.1 has no
	// --force / overwrite — choose a different parent or remove the
	// existing subdir to recover from a collision.
	HTMLDir string `name:"html" type:"existingdir" help:"Write a static HTML site under DIR/<auto-named subdir>/ instead of markdown to stdout. Prints the absolute path to the generated index.html on success." placeholder:"DIR"`
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

	// HTML mode is purely additive: when --html is unset the existing
	// markdown-to-stdout path runs unchanged.
	if cmd.HTMLDir != "" {
		return cmd.runHTML(ctx, s, out, shortName)
	}

	renderSynthesisMarkdown(os.Stdout, out, shortName)
	return nil
}

// runHTML loads every analyst output the synthesis cites in
// KeyConclusionRefs and hands the bundle to htmlreport.WriteReportTree.
// On success, the absolute path to the generated index.html is the
// only thing written to stdout — the operator can pipe it to `open`.
//
// Also resolves the target's external registry URL and the entity's
// currently-recorded posture from the store, so the index can show
// "Recommended posture (synthesist)" alongside "Current recorded
// posture (signatory db)" and link the target URI to its registry
// page.
func (cmd *ShowSynthesisCmd) runHTML(ctx context.Context, s store.Store, out *exchange.AnalystOutput, shortName string) error {
	loaded, err := loadReferencedAnalystOutputs(ctx, s, out)
	if err != nil {
		return err
	}

	recordedPosture, err := lookupRecordedPosture(ctx, s, out.Target)
	if err != nil {
		// Posture lookup is decorative; failure shouldn't block the
		// report. Treat any error as "no recorded posture" and move
		// on. (FindEntityByURI returning ErrNotFound is the common
		// case — the entity isn't tracked in this store.)
		recordedPosture = nil
	}

	indexPath, err := htmlreport.WriteReportTree(htmlreport.WriteReportTreeInput{
		ParentDir:         cmd.HTMLDir,
		Synth:             out,
		SynthesisOutputID: cmd.OutputID,
		ShortName:         shortName,
		Loaded:            loaded,
		TargetURL:         htmlreport.PURLToRegistryURL(out.Target),
		RecordedPosture:   recordedPosture,
		GeneratedAt:       time.Now().UTC().Format(time.RFC3339),
		Version:           buildVersionStamp(),
	})
	if err != nil {
		return fmt.Errorf("write HTML report: %w", err)
	}
	fmt.Fprintln(os.Stdout, indexPath)
	return nil
}

// lookupRecordedPosture returns the most recent active posture for
// the entity at target, translated into the htmlreport package's
// local RecordedPosture shape. Returns nil (without error) when no
// posture exists for the entity. Returns an error only on a real
// store failure that the caller should report; the calling code
// degrades gracefully on any error by treating it as "no recorded
// posture."
func lookupRecordedPosture(ctx context.Context, s store.Store, targetURI string) (*htmlreport.RecordedPosture, error) {
	entityURI, _ := profile.SplitURIVersion(targetURI)
	entity, err := s.FindEntityByURI(ctx, entityURI)
	if err != nil {
		return nil, err
	}
	postures, err := s.GetPostures(ctx, entity.ID)
	if err != nil {
		return nil, err
	}
	if len(postures) == 0 {
		return nil, nil
	}
	// GetPostures returns active (non-withdrawn) postures. v0.1 takes
	// the first one — multiple active postures across different
	// version scopes is rare in current dogfood usage; if it becomes
	// common, this is the seam to add per-version-scope rendering.
	p := postures[0]
	return &htmlreport.RecordedPosture{
		Tier:      string(p.Tier),
		Version:   p.Version,
		SetBy:     p.SetBy,
		SetAt:     p.SetAt.UTC().Format(time.RFC3339),
		Rationale: p.Rationale,
	}, nil
}

// loadReferencedAnalystOutputs walks the synthesis supplement's
// KeyConclusionRefs, collects the distinct OutputIDs, and loads each
// from the store. A miss on any individual id is NOT fatal — the
// resulting map is sparse, and BuildLinkPlan converts the absent
// entry into a DanglingRef that drives a stub page. That keeps the
// "snapshot of the store" contract: data corruption surfaces as a
// banner, not a hard fail.
func loadReferencedAnalystOutputs(ctx context.Context, s store.Store, synth *exchange.AnalystOutput) (map[string]*exchange.AnalystOutput, error) {
	loaded := map[string]*exchange.AnalystOutput{}
	if synth.SynthesisSupplement == nil {
		return loaded, nil
	}
	seen := map[string]struct{}{}
	for _, ref := range synth.SynthesisSupplement.KeyConclusionRefs {
		if ref.OutputID == "" {
			continue
		}
		if _, ok := seen[ref.OutputID]; ok {
			continue
		}
		seen[ref.OutputID] = struct{}{}
		ao, err := s.GetAnalystOutput(ctx, ref.OutputID)
		if errors.Is(err, store.ErrNotFound) {
			// Leave it absent in the loaded map; the link planner
			// produces a DanglingRef that drives a stub page.
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("load referenced analyst output %s: %w", ref.OutputID, err)
		}
		loaded[ref.OutputID] = ao
	}
	return loaded, nil
}

// buildVersionStamp returns the version string written to every
// page footer. Reads the build-time package vars threaded into the
// binary by main.go's ldflags. Pairs version with commit when both
// are stamped (real release builds); collapses to just version
// when commit is the placeholder.
func buildVersionStamp() string {
	v := strings.TrimSpace(version)
	if v == "" {
		v = "dev"
	}
	c := strings.TrimSpace(commit)
	if c != "" && c != "none" {
		return v + "+" + c
	}
	return v
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
