package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/sarahmaeve/signatory/internal/exchange"
	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/store"
)

// ShowAnalysesCmd lists analyst outputs in the store. With no
// target, lists everything (newest-ingested first); with a
// target, filters to that entity. The optional --analyst flag
// filters by analyst_id.
//
// Use case: "have we seen this target before?" — pass the target
// URL or canonical URI and see what's been ingested.
type ShowAnalysesCmd struct {
	Target  string `arg:"" optional:"" help:"Optional target URI (canonical or URL form). When set, lists only outputs for that entity."`
	Analyst string `help:"Filter by analyst_id (e.g. signatory-security-v1, signatory-provenance-v1, signatory-synthesis-v1)."`
	Limit   int    `help:"Maximum number of rows to return. 0 = no limit." default:"0"`
}

func (cmd *ShowAnalysesCmd) Run(globals *Globals) error {
	ctx := context.Background()
	s, err := globals.OpenStore(ctx)
	if err != nil {
		return err
	}
	defer s.Close() //nolint:errcheck // store close on command exit; error is not actionable

	canonicalURI := normalizeTargetForQuery(cmd.Target)

	// Surface effective-burn status BEFORE the analyses listing
	// when querying a specific target. This is the lede-first
	// contract: a human running `show-analyses repo:github/X/Y`
	// or an LLM consumer reading the captured output linearly
	// hits BURNED before it sees the analysis listing.
	//
	// EffectiveBurnByURI catches three cases in one call:
	//   - direct burn on the queried entity,
	//   - signal-derived cascade (entity has owner_profile /
	//     maintainer_count / publish_origin_consistency signals
	//     pointing at a burned identity),
	//   - URI-derived cascade for brand-new repos by burned
	//     operators (no entity row yet, but repo:github/X/Y
	//     names a burned X).
	//
	// Banner is additive: the existing absence/listing message
	// still appears below it. Exit code stays 0 in every absence
	// case — show-analyses is read-only and surfacing a burn is
	// not an error. A real store error (other than ErrNotFound)
	// surfaces to stderr as a warning so the listing still runs;
	// burn surfacing is enrichment, not load-bearing.
	//
	// List-all (canonicalURI == "") skips this — there's no
	// specific target to attribute a banner to, and per-row
	// burn-tagging would be N+1 store calls for marginal value.
	if canonicalURI != "" {
		if burn, ebCtx, burnErr := s.EffectiveBurnByURI(ctx, canonicalURI); burnErr == nil {
			renderShowAnalysesBurnBanner(burn, ebCtx)
		} else if !errors.Is(burnErr, store.ErrNotFound) {
			fmt.Fprintf(os.Stderr, "warning: burn check on %s failed: %v\n", canonicalURI, burnErr)
		}
	}

	filter := store.AnalystOutputFilter{
		EntityURI: canonicalURI,
		AnalystID: cmd.Analyst,
		Limit:     cmd.Limit,
	}
	rows, err := s.ListAnalystOutputs(ctx, filter)
	if errors.Is(err, store.ErrNotFound) {
		// Target didn't resolve to any known entity — distinct from
		// "entity exists but has no outputs." Callers benefit from
		// seeing the distinction; the store now signals it explicitly.
		fmt.Printf("No entity matches %q (target has never been ingested)\n", cmd.Target)
		return nil
	}
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		if cmd.Target != "" {
			fmt.Printf("No analyses for %s\n", cmd.Target)
		} else {
			fmt.Println("No analyses in store")
		}
		return nil
	}
	for _, r := range rows {
		fmt.Printf("%s  %s  round=%d  ingested=%s\n",
			r.OutputID[:8], r.AnalystID, r.Round, r.IngestedAt)
		fmt.Printf("    target: %s\n", r.EntityURI)
		fmt.Printf("    %d conclusion(s), %d positive absence(s), %d observation(s), %d methodology pattern(s)\n",
			r.ConclusionsCount, r.PositiveAbsenceCount, r.ObservationCount, r.PatternCount)
		if r.SourcePath != "" {
			fmt.Printf("    source: %s\n", r.SourcePath)
		}
	}
	return nil
}

// renderShowAnalysesBurnBanner writes the BURNED block to stdout
// for show-analyses. Adapts the store-side EffectiveBurnContext
// to the shared formatBurnLine input shape, then prints with a
// trailing blank line so the BURNED banner is visually separated
// from the listing/absence message that follows.
//
// Stdout, not stderr: the banner is part of the command's primary
// output; an LLM consumer reading the captured output linearly
// must see BURNED before the analyses listing/absence message.
func renderShowAnalysesBurnBanner(burn *profile.Burn, ctx *store.EffectiveBurnContext) {
	var viaURI, viaRole string
	if ctx != nil && !ctx.Direct && ctx.ViaOwner != nil {
		viaURI = ctx.ViaOwner.CanonicalURI
		viaRole = ctx.ViaRole
	}
	fmt.Printf("%s\n\n", formatBurnLine(burnDisplayInput{
		Reason:      burn.Reason,
		BurnedBy:    burn.BurnedBy,
		BurnedAt:    burn.BurnedAt,
		ViaOwnerURI: viaURI,
		ViaRole:     viaRole,
	}))
}

// ShowConclusionsCmd queries the conclusions table across analyst outputs.
// Filtering by severity, signal_type, target, analyst is supported;
// rationale is omitted from the listing output to keep it scannable.
type ShowConclusionsCmd struct {
	Target       string   `help:"Filter by target URI (canonical or URL form)."`
	Analyst      string   `help:"Filter by analyst_id."`
	SignalType   string   `help:"Filter by signal_type (registry name)."`
	Severity     []string `help:"Filter by one or more severity values: critical, high, medium, low, informational, positive."`
	DesignIntent bool     `help:"Limit to conclusions flagged design_intent: true."`
	Limit        int      `help:"Maximum number of rows to return. 0 = no limit." default:"0"`
}

func (cmd *ShowConclusionsCmd) Run(globals *Globals) error {
	ctx := context.Background()
	s, err := globals.OpenStore(ctx)
	if err != nil {
		return err
	}
	defer s.Close() //nolint:errcheck // store close on command exit; error is not actionable

	severities, err := parseSeverities(cmd.Severity)
	if err != nil {
		return err
	}
	filter := store.ConclusionFilter{
		EntityURI:        normalizeTargetForQuery(cmd.Target),
		AnalystID:        cmd.Analyst,
		SignalType:       cmd.SignalType,
		SeverityIn:       severities,
		DesignIntentOnly: cmd.DesignIntent,
		Limit:            cmd.Limit,
	}
	rows, err := s.ListConclusions(ctx, filter)
	if errors.Is(err, store.ErrNotFound) {
		fmt.Printf("No entity matches %q (target has never been ingested)\n", cmd.Target)
		return nil
	}
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Println("No conclusions match the filter")
		return nil
	}
	for _, f := range rows {
		flags := ""
		if f.DesignIntent {
			flags += " [design_intent]"
		}
		if f.HasSupersedes {
			flags += " [supersedes]"
		}
		signalTag := ""
		if f.SignalType != "" {
			signalTag = " (" + f.SignalType + ")"
		}
		fmt.Printf("%s  [%s]%s %s%s\n",
			f.ConclusionLocalID, f.SeverityDefault, flags, f.Category, signalTag)
		fmt.Printf("    target: %s   analyst: %s   citations: %d\n",
			f.EntityURI, f.AnalystID, f.CitationCount)
		fmt.Printf("    %s\n", truncateLine(f.Verdict, 110))
	}
	return nil
}

// ShowMethodologyCmd queries methodology patterns across analyst
// outputs. The aggregation use case: "which network_endpoints
// patterns actually fired across our analyses?" — pass --signal-group
// network_endpoints --hit-on-target to get exactly that view.
type ShowMethodologyCmd struct {
	Target      string `help:"Filter by target URI."`
	Analyst     string `help:"Filter by analyst_id."`
	SignalGroup string `help:"Filter by signal_group (e.g. network_endpoints, governance, vitality)."`
	HitOnTarget string `help:"Filter by hit_on_target: hit, miss, or any (default)." default:"any" enum:"hit,miss,any"`
	Limit       int    `help:"Maximum number of rows to return. 0 = no limit." default:"0"`
}

func (cmd *ShowMethodologyCmd) Run(globals *Globals) error {
	ctx := context.Background()
	s, err := globals.OpenStore(ctx)
	if err != nil {
		return err
	}
	defer s.Close() //nolint:errcheck // store close on command exit; error is not actionable

	filter := store.MethodologyPatternFilter{
		EntityURI:   normalizeTargetForQuery(cmd.Target),
		AnalystID:   cmd.Analyst,
		SignalGroup: cmd.SignalGroup,
		Limit:       cmd.Limit,
	}
	switch cmd.HitOnTarget {
	case "hit":
		t := true
		filter.HitOnTarget = &t
	case "miss":
		f := false
		filter.HitOnTarget = &f
	}

	rows, err := s.ListMethodologyPatterns(ctx, filter)
	if errors.Is(err, store.ErrNotFound) {
		fmt.Printf("No entity matches %q (target has never been ingested)\n", cmd.Target)
		return nil
	}
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Println("No methodology patterns match the filter")
		return nil
	}
	for _, p := range rows {
		hit := "?"
		if p.HitOnTarget != nil {
			if *p.HitOnTarget {
				hit = "hit"
			} else {
				hit = "miss"
			}
		}
		composes := ""
		if p.ComposesWithCount > 0 {
			composes = fmt.Sprintf(" [composes-with: %d]", p.ComposesWithCount)
		}
		grepStatus := "no-grep"
		if p.HasPatternText {
			grepStatus = "grep"
		}
		fmt.Printf("%s  [%s]  %s  (%s/%s, %s)%s\n",
			p.PatternLocalID, hit, p.SignalGroup,
			p.GrepPrecision, p.ReasoningDepth, grepStatus, composes)
		fmt.Printf("    target: %s   analyst: %s\n", p.EntityURI, p.AnalystID)
		fmt.Printf("    %s\n", truncateLine(p.Description, 110))
	}
	return nil
}

// normalizeTargetForQuery resolves a user-supplied target to the
// canonical URI used as the entities.canonical_uri lookup key.
// Empty input is preserved (no filter applied); unresolvable
// input is passed through so the store returns an ErrNotFound the
// command surfaces as "no entity matches" — clearer than silently
// returning zero rows.
//
// profile.ResolveTarget is the single source of truth for target
// acceptance across signatory's CLI surface; see its doc and
// target_test.go for the full accepted-forms matrix. This wrapper
// just handles the show-command ergonomics (empty input is fine;
// unresolvable input should pass through rather than error).
func normalizeTargetForQuery(target string) string {
	if target == "" {
		return ""
	}
	resolved, err := profile.ResolveTarget(target)
	if err != nil {
		// Pass through to the store so the "No entity matches"
		// branch fires with the user's original input quoted
		// back to them. Bailing out here with an error would
		// suppress that helpful message.
		return target
	}
	return resolved.CanonicalURI
}

// parseSeverities converts CLI --severity strings into the typed
// enum values, validating each.
func parseSeverities(raw []string) ([]exchange.SeverityValue, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := make([]exchange.SeverityValue, 0, len(raw))
	for _, s := range raw {
		v := exchange.SeverityValue(s)
		if !v.Valid() {
			return nil, fmt.Errorf("invalid severity %q (expected one of: critical, high, medium, low, informational, positive)", s)
		}
		out = append(out, v)
	}
	return out, nil
}
