package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/sarahmaeve/signatory/internal/exchange"
	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/summary"
)

// SummaryCmd is the M7 "one place to start" verb. It answers
// "everything signatory knows about this target, at a decision
// level" in a single call — canonical URI, posture, burn status,
// analyses rollup, related identities. Designed to replace the
// cross-call flail (show-analyses → show-conclusions → posture get
// → burn list) that yesterday's synthesist-agent dogfood exposed.
//
// Output is human-readable by default; pass --json for programmatic
// consumers (the /analyze skill replacement, the synthesist's
// structured input in M6).
//
// See agent-facing-contract.md §5 M7.
type SummaryCmd struct {
	Target string `arg:"" help:"Target to summarize (canonical URI, URL, or shorthand)."`
	JSON   bool   `name:"json" help:"Emit JSON instead of human-readable output."`
}

func (cmd *SummaryCmd) Run(globals *Globals) error {
	ctx := context.Background()

	resolved, err := profile.ResolveTarget(cmd.Target)
	if err != nil {
		return NewUsageError(fmt.Errorf("resolve target %q: %w", cmd.Target, err))
	}

	db, err := globals.OpenStore(ctx)
	if err != nil {
		return err
	}
	defer db.Close() //nolint:errcheck // store close on command exit; error is not actionable

	s, err := summary.New(db).Assemble(ctx, resolved.CanonicalURI)
	if err != nil {
		if errors.Is(err, summary.ErrEntityNotFound) {
			// Soft absence, matching `posture get`'s contract: a
			// target with no ingested record prints a one-line "no
			// record" message and exits 0. Pre-2026-04-22 this was
			// a usage error (exit 64), asymmetric with the sibling
			// read verbs and confusing for scripters who pipelined
			// them. The dogfood-walkthrough read-surface pass
			// highlighted the discrepancy.
			fmt.Printf("No signatory record for: %s\n", cmd.Target)
			return nil
		}
		return err
	}

	if cmd.JSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(s)
	}
	renderSummaryHuman(os.Stdout, s)
	return nil
}

// renderSummaryHuman prints a compact, scan-friendly view of a
// summary. Designed to be the answer to "what do I know about X?"
// a human can read at a glance — hierarchy is: identity first,
// then current-state (posture + burn), then history (analyses).
func renderSummaryHuman(w io.Writer, s *summary.Summary) {
	fmt.Fprintf(w, "URI:       %s\n", s.CanonicalURI)
	fmt.Fprintf(w, "Name:      %s\n", s.ShortName)
	fmt.Fprintf(w, "Type:      %s\n", s.EntityType)
	if s.URL != "" {
		fmt.Fprintf(w, "URL:       %s\n", s.URL)
	}

	if len(s.RelatedURIs) > 0 {
		fmt.Fprintln(w, "Related:")
		for _, r := range s.RelatedURIs {
			fmt.Fprintf(w, "  - %s\n", r)
		}
	}

	fmt.Fprintln(w)

	if s.Posture != nil {
		fmt.Fprintf(w, "Posture:   %s", s.Posture.Tier)
		if s.Posture.Version != "" {
			fmt.Fprintf(w, " (version %s)", s.Posture.Version)
		}
		fmt.Fprintln(w)
		fmt.Fprintf(w, "Rationale: %s\n", firstLine(s.Posture.Rationale))
		fmt.Fprintf(w, "Set by:    %s at %s\n", s.Posture.SetBy, s.Posture.SetAt.Format(time.RFC3339))
	} else {
		fmt.Fprintln(w, "Posture:   (none recorded)")
	}

	if s.Burn != nil {
		// Shared formatter (cmd/signatory/burn_display.go): direct
		// vs cascade phrasing is centralised so signatory analyze /
		// summary / show-analyses present cascaded burns identically.
		fmt.Fprintf(w, "\n%s\n", formatBurnLine(burnDisplayInput{
			Reason:      s.Burn.Reason,
			BurnedBy:    s.Burn.BurnedBy,
			BurnedAt:    s.Burn.BurnedAt,
			ViaOwnerURI: s.Burn.ViaOwnerURI,
			ViaRole:     s.Burn.ViaRole,
		}))
	}

	if len(s.Analyses) > 0 {
		fmt.Fprintf(w, "\nAnalyses (%d):\n", len(s.Analyses))
		for _, a := range s.Analyses {
			fmt.Fprintf(w, "  %s  %s  round=%d  ingested=%s\n",
				a.OutputID[:8], a.AnalystID, a.Round, a.IngestedAt.Format(time.RFC3339))
			if a.CollectedFromURI != "" {
				fmt.Fprintf(w, "    collected_from: %s\n", a.CollectedFromURI)
			}
			// Severity bracket appears only when there's something to
			// break down. Zero-conclusion rows (synthesis outputs per
			// Plan-A design) would otherwise render "[-]" — visual
			// noise that doesn't say anything useful. 2026-04-22
			// read-surface pass.
			if a.ConclusionCount == 0 {
				fmt.Fprintf(w, "    %d conclusion(s), %d absence(s), %d observation(s), %d pattern(s)\n",
					a.ConclusionCount,
					a.PositiveAbsenceCount, a.ObservationCount, a.MethodologyPatternCnt)
			} else {
				fmt.Fprintf(w, "    %d conclusion(s) [%s], %d absence(s), %d observation(s), %d pattern(s)\n",
					a.ConclusionCount, formatSeverityBreakdown(a.SeverityCounts),
					a.PositiveAbsenceCount, a.ObservationCount, a.MethodologyPatternCnt)
			}
		}
		fmt.Fprintf(w, "\nDrill in: `signatory show-conclusions --target %s`\n", s.CanonicalURI)
	} else {
		fmt.Fprintln(w, "\nAnalyses: (none ingested)")
	}
}

// formatSeverityBreakdown renders a compact "high=1 medium=2
// positive=3" string from a SeverityCounts map. Empty counts
// render as "-" to signal absence explicitly. Ordering is severity
// significance (critical → high → medium → low → informational →
// positive) so the worst thing is always first.
func formatSeverityBreakdown(c summary.SeverityCounts) string {
	if len(c) == 0 {
		return "-"
	}
	order := []exchange.SeverityValue{
		exchange.SeverityCritical,
		exchange.SeverityHigh,
		exchange.SeverityMedium,
		exchange.SeverityLow,
		exchange.SeverityInformational,
		exchange.SeverityPositive,
	}
	var out string
	for _, sev := range order {
		if n, ok := c[sev]; ok && n > 0 {
			if out != "" {
				out += " "
			}
			out += fmt.Sprintf("%s=%d", sev, n)
		}
	}
	if out == "" {
		return "-"
	}
	return out
}
