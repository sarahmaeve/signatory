package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/sarahmaeve/signatory/internal/exchange"
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
	Analyst string `help:"Filter by analyst_id (e.g. signatory-provenance, external-sec-v1)."`
	Limit   int    `help:"Maximum number of rows to return. 0 = no limit." default:"0"`
}

func (cmd *ShowAnalysesCmd) Run(globals *Globals) error {
	s, err := globals.OpenStore()
	if err != nil {
		return err
	}
	defer s.Close()

	filter := store.AnalystOutputFilter{
		EntityURI: normalizeTargetForQuery(cmd.Target),
		AnalystID: cmd.Analyst,
		Limit:     cmd.Limit,
	}
	rows, err := s.ListAnalystOutputs(context.Background(), filter)
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
		fmt.Printf("    %d finding(s), %d positive absence(s), %d observation(s), %d methodology pattern(s)\n",
			r.FindingsCount, r.PositiveAbsenceCount, r.ObservationCount, r.PatternCount)
		if r.SourcePath != "" {
			fmt.Printf("    source: %s\n", r.SourcePath)
		}
	}
	return nil
}

// ShowFindingsCmd queries the findings table across analyst outputs.
// Filtering by severity, signal_type, target, analyst is supported;
// rationale is omitted from the output to keep the listing
// scannable (use show-output --full to see one in detail).
type ShowFindingsCmd struct {
	Target       string   `help:"Filter by target URI (canonical or URL form)."`
	Analyst      string   `help:"Filter by analyst_id."`
	SignalType   string   `help:"Filter by signal_type (registry name)."`
	Severity     []string `help:"Filter by one or more severity values: critical, high, medium, low, informational, positive."`
	DesignIntent bool     `help:"Limit to findings flagged design_intent: true."`
	Limit        int      `help:"Maximum number of rows to return. 0 = no limit." default:"0"`
}

func (cmd *ShowFindingsCmd) Run(globals *Globals) error {
	s, err := globals.OpenStore()
	if err != nil {
		return err
	}
	defer s.Close()

	severities, err := parseSeverities(cmd.Severity)
	if err != nil {
		return err
	}
	filter := store.FindingFilter{
		EntityURI:        normalizeTargetForQuery(cmd.Target),
		AnalystID:        cmd.Analyst,
		SignalType:       cmd.SignalType,
		SeverityIn:       severities,
		DesignIntentOnly: cmd.DesignIntent,
		Limit:            cmd.Limit,
	}
	rows, err := s.ListFindings(context.Background(), filter)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Println("No findings match the filter")
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
			f.FindingLocalID, f.SeverityDefault, flags, f.Category, signalTag)
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
	s, err := globals.OpenStore()
	if err != nil {
		return err
	}
	defer s.Close()

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

	rows, err := s.ListMethodologyPatterns(context.Background(), filter)
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

// normalizeTargetForQuery accepts either a canonical URI or a
// recognized URL and returns the canonical form to use in the
// query filter. On unrecognized input, returns the input unchanged
// (the store will then no-op the lookup).
//
// This is the read-side mirror of normalizeTargetToCanonicalURI in
// the store package — keeping them aligned matters because
// otherwise a `signatory ingest` followed by `signatory show` for
// the same target would fail to find anything.
func normalizeTargetForQuery(target string) string {
	if target == "" {
		return ""
	}
	// Canonical purl/repo/identity/etc. URIs all start with a known
	// scheme prefix; pass through unmodified.
	for _, prefix := range []string{"pkg:", "repo:", "identity:", "org:", "patch:"} {
		if strings.HasPrefix(target, prefix) {
			return target
		}
	}
	// Anything else (e.g., a GitHub URL) gets normalized by the
	// store-side FindEntityByURI lookup chain. The store wraps the
	// raw target through profile.NormalizeGitHubRepoInput when the
	// canonical-form lookup misses, so the read path matches the
	// ingest path's normalization. Pass through unmodified here.
	return target
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
