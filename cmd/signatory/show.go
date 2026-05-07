package main

import (
	"context"
	"errors"
	"fmt"
	"io"
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
// ShowAnalysesResult is the JSON contract for `signatory show-analyses --json`.
type ShowAnalysesResult struct {
	Status   string                       `json:"status"`
	Analyses []store.AnalystOutputSummary `json:"analyses"`
}

type ShowAnalysesCmd struct {
	Target  string `arg:"" optional:"" help:"Optional target URI (canonical or URL form). When set, lists only outputs for that entity."`
	Analyst string `help:"Filter by analyst_id (e.g. signatory-security-v1, signatory-provenance-v1, signatory-synthesis-v1)."`
	Limit   int    `help:"Maximum number of rows to return. 0 = no limit." default:"0"`
	JSON    bool   `help:"Emit structured JSON instead of prose."`

	Stdout io.Writer `kong:"-"`
}

func (cmd *ShowAnalysesCmd) Run(globals *Globals) error {
	// Root context. globals.Context, when set, carries the SIGINT-
	// cancellation wiring from main(); Ctrl-C at the CLI propagates
	// through store calls. Tests leave this nil and fall back to
	// context.Background(). See analyze.go for the originating pattern.
	ctx := globals.Context
	if ctx == nil {
		ctx = context.Background()
	}
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
	//
	// Note: this still uses the URI-form (not the resolved entity
	// ID) so the URI-derived-cascade case still fires for inputs
	// that don't have an entity row yet — that's the whole point
	// of EffectiveBurnByURI's third case.
	if canonicalURI != "" {
		if burn, ebCtx, burnErr := s.EffectiveBurnByURI(ctx, canonicalURI); burnErr == nil {
			renderShowAnalysesBurnBanner(burn, ebCtx)
		} else if !errors.Is(burnErr, store.ErrNotFound) {
			fmt.Fprintf(os.Stderr, "warning: burn check on %s failed: %v\n", canonicalURI, burnErr)
		}
	}

	// Listing-filter resolution: the alternate-URI walk lives here,
	// not in normalizeTargetForQuery, because the "find me the
	// entity behind this target" lookup needs to reach the same
	// equivalence class summary uses. Pre-2026-05-07 this was a
	// direct FindEntityByURI on the canonical URI, which missed
	// vanity-Go-path / cross-scheme equivalents that summary's
	// LookupEntity walk handled correctly.
	entityID, err := resolveTargetEntityID(ctx, s, cmd.Target)
	stdout := cmd.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	if errors.Is(err, store.ErrNotFound) {
		if cmd.JSON {
			return writeJSON(stdout, &ShowAnalysesResult{Status: "no_entity"})
		}
		fmt.Printf("No entity matches %q (target has never been ingested)\n", cmd.Target)
		return nil
	}
	if err != nil {
		return err
	}

	filter := store.AnalystOutputFilter{
		EntityID:  entityID,
		AnalystID: cmd.Analyst,
		Limit:     cmd.Limit,
	}

	rows, err := s.ListAnalystOutputs(ctx, filter)
	if errors.Is(err, store.ErrNotFound) {
		// Defensive: resolveTargetEntityID already passed, so the
		// entity exists. ListAnalystOutputs should not return
		// ErrNotFound for an EntityID-keyed filter. If it does,
		// fall through to the same UX as the resolution miss
		// rather than leaking the surprising error to the user.
		if cmd.JSON {
			return writeJSON(stdout, &ShowAnalysesResult{Status: "no_entity"})
		}
		fmt.Printf("No entity matches %q (target has never been ingested)\n", cmd.Target)
		return nil
	}
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		if cmd.JSON {
			return writeJSON(stdout, &ShowAnalysesResult{Status: "empty"})
		}
		if cmd.Target != "" {
			fmt.Printf("No analyses for %s\n", cmd.Target)
		} else {
			fmt.Println("No analyses in store")
		}
		return nil
	}

	if cmd.JSON {
		return writeJSON(stdout, &ShowAnalysesResult{
			Status:   "ok",
			Analyses: rows,
		})
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

// ShowConclusionsResult is the JSON contract for `signatory show-conclusions --json`.
type ShowConclusionsResult struct {
	Status      string                    `json:"status"`
	Conclusions []store.ConclusionSummary `json:"conclusions"`
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
	JSON         bool     `help:"Emit structured JSON instead of prose."`

	Stdout io.Writer `kong:"-"`
}

func (cmd *ShowConclusionsCmd) Run(globals *Globals) error {
	ctx := globals.Context
	if ctx == nil {
		ctx = context.Background()
	}
	s, err := globals.OpenStore(ctx)
	if err != nil {
		return err
	}
	defer s.Close() //nolint:errcheck // store close on command exit; error is not actionable

	severities, err := parseSeverities(cmd.Severity)
	if err != nil {
		return err
	}
	stdout := cmd.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}

	// See ShowAnalysesCmd.Run for why the resolution moved off
	// FindEntityByURI onto the alternate-walking LookupEntityID.
	entityID, err := resolveTargetEntityID(ctx, s, cmd.Target)
	if errors.Is(err, store.ErrNotFound) {
		if cmd.JSON {
			return writeJSON(stdout, &ShowConclusionsResult{Status: "no_entity"})
		}
		fmt.Printf("No entity matches %q (target has never been ingested)\n", cmd.Target)
		return nil
	}
	if err != nil {
		return err
	}

	filter := store.ConclusionFilter{
		EntityID:         entityID,
		AnalystID:        cmd.Analyst,
		SignalType:       cmd.SignalType,
		SeverityIn:       severities,
		DesignIntentOnly: cmd.DesignIntent,
		Limit:            cmd.Limit,
	}

	rows, err := s.ListConclusions(ctx, filter)
	if errors.Is(err, store.ErrNotFound) {
		// Defensive: same as ShowAnalysesCmd — resolveTargetEntityID
		// already gated the absent-entity case, so this branch is
		// belt-and-braces against a future store-side change.
		if cmd.JSON {
			return writeJSON(stdout, &ShowConclusionsResult{Status: "no_entity"})
		}
		fmt.Printf("No entity matches %q (target has never been ingested)\n", cmd.Target)
		return nil
	}
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		if cmd.JSON {
			return writeJSON(stdout, &ShowConclusionsResult{Status: "empty"})
		}
		fmt.Println("No conclusions match the filter")
		return nil
	}

	if cmd.JSON {
		return writeJSON(stdout, &ShowConclusionsResult{
			Status:      "ok",
			Conclusions: rows,
		})
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
	ctx := globals.Context
	if ctx == nil {
		ctx = context.Background()
	}
	s, err := globals.OpenStore(ctx)
	if err != nil {
		return err
	}
	defer s.Close() //nolint:errcheck // store close on command exit; error is not actionable

	// See ShowAnalysesCmd.Run for why the resolution moved off
	// FindEntityByURI onto the alternate-walking LookupEntityID.
	entityID, err := resolveTargetEntityID(ctx, s, cmd.Target)
	if errors.Is(err, store.ErrNotFound) {
		fmt.Printf("No entity matches %q (target has never been ingested)\n", cmd.Target)
		return nil
	}
	if err != nil {
		return err
	}

	filter := store.MethodologyPatternFilter{
		EntityID:    entityID,
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
		// Defensive — same rationale as the sibling show-* commands.
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
//
// Used today only for the show-analyses burn-banner URI — the
// listing-filter resolution moved to resolveTargetEntityID so the
// show-* family inherits LookupEntity's alternate-URI walk.
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

// resolveTargetEntityID translates a user-supplied target string to
// an entity ID for use as a store filter (filter.EntityID). Walks
// the canonical-URI alternates via store.LookupEntityID — the same
// equivalence rules summary uses — so vanity Go paths,
// pkg:go ↔ pkg:golang swaps, and case-fold variants resolve to the
// underlying entity row regardless of which form the user typed.
//
// Empty target returns ("", nil) — "no filter" semantics propagate
// straight into the filter struct.
//
// The error contract is intentionally narrow:
//
//   - ErrNotFound covers BOTH "no entity in the alternate walk" AND
//     "input didn't parse as any URI form." Show-* commands print
//     the user's original input in their "no entity matches"
//     message; collapsing the malformed-input case here keeps that
//     prose uniform whether the input was unparseable garbage or
//     well-formed-but-unmatched.
//   - Other errors (DB-side) propagate verbatim.
//
// The collapse is a deliberate CLI UX choice — the parallel MCP
// surfaces handle the malformed-vs-unmatched distinction explicitly
// because their consumers (LLM agents) benefit from the typed
// distinction. See internal/mcp/tools/show_analyses.go for the
// CodeSchemaViolation vs CodeNotFound branching.
func resolveTargetEntityID(ctx context.Context, s store.Store, target string) (string, error) {
	if target == "" {
		return "", nil
	}
	id, err := store.LookupEntityID(ctx, s, target)
	if err == nil {
		return id, nil
	}
	if errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	// Heuristic for the malformed-input collapse: if a fresh
	// ResolveTarget call also fails, the original error is from
	// profile parsing and we treat it as "no entity matches" per
	// the doc. Otherwise it's a DB-side error and we propagate.
	// Cheap (in-memory parse) and keeps the behavior boundary
	// truthful: only known-malformed input is swallowed.
	if _, perr := profile.ResolveTarget(target); perr != nil {
		return "", store.ErrNotFound
	}
	return "", err
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
