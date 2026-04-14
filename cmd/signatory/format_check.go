package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sarahmaeve/signatory/internal/exchange"
)

// FormatCheckCmd checks an analyst output file for structural
// conformance to the signatory v1 schema. It is NOT a semantic
// validator: it does not assess whether the analyst's findings are
// correct, well-calibrated, or trustworthy. It only confirms that
// the document parses, has every required field, uses valid enum
// values, and satisfies the structural invariants encoded in
// internal/exchange/validate.go (Citation either-lines-or-scope,
// unique IDs, etc.).
//
// Use this as a pre-flight before submitting an analyst output for
// downstream consumption — if format-check passes, the document is
// machine-consumable; whether it's a *good* analysis is a different
// question that humans (and the synthesist) decide.
//
// This command exists because both the dual-analyst trial agents
// independently reached for ad-hoc validation tooling. The right
// answer is one canonical command, used by everyone.
type FormatCheckCmd struct {
	File    string `arg:"" help:"Path to a JSON or markdown analyst output file." type:"existingfile"`
	Format  string `help:"Input format: json, markdown, or auto (detect from extension/content)." default:"auto" enum:"json,markdown,auto"`
	Quiet   bool   `help:"Suppress the one-line OK summary; errors still print. Mutually exclusive with --summary." short:"q"`
	Summary bool   `help:"Print structural summary of the document (findings, absences, methodology) without prose. Replaces the one-line OK." short:"s"`
}

func (cmd *FormatCheckCmd) Run(globals *Globals) error {
	raw, err := os.ReadFile(cmd.File)
	if err != nil {
		return fmt.Errorf("read %s: %w", cmd.File, err)
	}

	format := cmd.Format
	if format == "auto" {
		format = detectAnalystOutputFormat(cmd.File, raw)
	}

	out, err := parseAnalystOutput(raw, format)
	if err != nil {
		return fmt.Errorf("parse %s as %s: %w", cmd.File, format, err)
	}

	if err := out.Validate(); err != nil {
		return fmt.Errorf("format check failed for %s:\n%w", cmd.File, err)
	}

	switch {
	case cmd.Summary:
		printSummary(os.Stdout, cmd.File, format, out)
	case !cmd.Quiet:
		patternCount := 0
		if out.MethodologyTrace != nil {
			patternCount = len(out.MethodologyTrace.Patterns)
		}
		fmt.Printf("OK %s (%s): %d finding(s), %d positive absence(s), %d observation(s), %d methodology pattern(s)\n",
			cmd.File, format,
			len(out.Findings),
			len(out.PositiveAbsences),
			len(out.Observations),
			patternCount,
		)
	}
	return nil
}

// printSummary writes a structural breakdown of an AnalystOutput
// without including any of its prose fields (verdict, rationale,
// observation body, round_notes content). The intent is to let a
// reader see the *shape* of an analysis — what severities, what
// signal types, what categories, how many citations per finding,
// what methodology groups — without spoilers on the actual claims
// or having to read several thousand words of nested rationale.
//
// Rendering guidelines:
//   - Identifying fields are shown verbatim (Finding.ID, IDs of
//     methodology patterns, etc.).
//   - Naming fields that ARE prose but identify entries are
//     truncated to a fixed width (PositiveAbsence.PatternChecked,
//     Observation.Title) — without them the entry can't be
//     distinguished from siblings.
//   - True prose fields are excluded entirely; if the consumer
//     wants the rationale they should read the file.
func printSummary(w io.Writer, path, format string, out *exchange.AnalystOutput) {
	fmt.Fprintf(w, "%s (%s)\n", path, format)
	fmt.Fprintf(w, "  attribution: %s / %s", out.Attribution.AnalystID, out.Attribution.Model)
	if out.Attribution.Round > 0 {
		fmt.Fprintf(w, " (round %d)", out.Attribution.Round)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  target: %s\n", out.Target)
	if out.TargetCommit != "" {
		fmt.Fprintf(w, "  target_commit: %s\n", out.TargetCommit)
	}
	if len(out.Supersedes) > 0 {
		fmt.Fprintf(w, "  supersedes %d prior\n", len(out.Supersedes))
	}

	fmt.Fprintf(w, "\nfindings (%d):\n", len(out.Findings))
	for i := range out.Findings {
		printFindingSummary(w, &out.Findings[i])
	}

	if len(out.PositiveAbsences) > 0 {
		fmt.Fprintf(w, "\npositive_absences (%d):\n", len(out.PositiveAbsences))
		for _, pa := range out.PositiveAbsences {
			fmt.Fprintf(w, "  [%s] %s\n", pa.Confidence, truncateLine(pa.PatternChecked, 90))
		}
	}

	if len(out.Observations) > 0 {
		fmt.Fprintf(w, "\nobservations (%d):\n", len(out.Observations))
		for _, o := range out.Observations {
			sigInfo := ""
			if o.SignalType != nil {
				sigInfo = " (" + *o.SignalType + ")"
			}
			fmt.Fprintf(w, "  %s [%s]%s: %s\n", o.ID, o.Category, sigInfo, truncateLine(o.Title, 90))
		}
	}

	if out.MethodologyTrace != nil && len(out.MethodologyTrace.Patterns) > 0 {
		fmt.Fprintf(w, "\nmethodology_trace (%d patterns):\n", len(out.MethodologyTrace.Patterns))
		groups := groupPatternsBySignalGroup(out.MethodologyTrace.Patterns)
		for _, g := range sortedKeys(groups) {
			ids := groups[g]
			fmt.Fprintf(w, "  %s (%d): %s\n", g, len(ids), strings.Join(ids, ", "))
		}
	}

	if out.RoundNotes != "" {
		fmt.Fprintf(w, "\nround_notes: %d chars (excluded from summary)\n", len(out.RoundNotes))
	}

	if len(out.ReframesFrom) > 0 {
		fmt.Fprintf(w, "\nreframes_from: %d entries\n", len(out.ReframesFrom))
	}
}

// printFindingSummary writes the structural summary of one Finding:
// IDs, flags, counts, signal type. Verdict and rationale (the prose)
// are deliberately omitted.
func printFindingSummary(w io.Writer, f *exchange.Finding) {
	sevTag := string(f.Severity.Default)
	if n := len(f.Severity.ByContext); n > 0 {
		sevTag += fmt.Sprintf(" +%d ctx", n)
	}
	flags := ""
	if f.DesignIntent {
		flags += " [design_intent]"
	}
	if n := len(f.Supersedes); n > 0 {
		flags += fmt.Sprintf(" [supersedes %d]", n)
	}
	if f.AnswersQuestion != nil && *f.AnswersQuestion != "" {
		flags += fmt.Sprintf(" [answers %s]", *f.AnswersQuestion)
	}
	fmt.Fprintf(w, "  %s [%s]%s %s\n", f.ID, sevTag, flags, f.Category)
	if f.SignalType != nil {
		fmt.Fprintf(w, "      signal_type: %s\n", *f.SignalType)
	}
	fmt.Fprintf(w, "      cite/prereq/fix: %d/%d/%d\n",
		len(f.Citations), len(f.Prerequisites), len(f.RemediationHints))
	if n := len(f.RelatedFindings); n > 0 {
		fmt.Fprintf(w, "      related: %s\n", strings.Join(f.RelatedFindings, ", "))
	}
}

// truncateLine collapses internal whitespace and trims to maxLen,
// adding an ellipsis if truncated. Used for fields that are prose
// but identify entries (pattern_checked, observation title).
func truncateLine(s string, maxLen int) string {
	// Collapse internal whitespace (newlines, tabs, runs of spaces)
	// to single spaces so a multi-line field renders cleanly on
	// one summary line.
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// groupPatternsBySignalGroup buckets methodology patterns by
// signal_group, preserving pattern IDs for each bucket.
func groupPatternsBySignalGroup(patterns []exchange.MethodologyPattern) map[string][]string {
	g := make(map[string][]string)
	for _, p := range patterns {
		g[p.SignalGroup] = append(g[p.SignalGroup], p.ID)
	}
	return g
}

// sortedKeys returns the map's keys in lexicographic order so the
// summary output is deterministic across runs.
func sortedKeys(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// detectAnalystOutputFormat picks json or markdown for an input file
// based on its extension first and a content sniff as fallback. The
// caller can always force format with --format if auto-detection is
// wrong.
func detectAnalystOutputFormat(path string, raw []byte) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".json":
		return "json"
	case ".md", ".markdown":
		return "markdown"
	}
	// No recognized extension. Sniff the content.
	// Markdown-with-frontmatter starts with `---` on its own line
	// (we accept LF and CRLF; the BOM is handled by UnmarshalMarkdown
	// itself, so we don't need to peek past it here).
	if bytes.HasPrefix(raw, []byte("---\n")) || bytes.HasPrefix(raw, []byte("---\r\n")) {
		return "markdown"
	}
	// Default to JSON if the body looks like a JSON object — leading
	// whitespace then `{` is a strong-enough heuristic.
	trimmed := bytes.TrimLeft(raw, " \t\n\r")
	if len(trimmed) > 0 && trimmed[0] == '{' {
		return "json"
	}
	// Fall through to JSON; the parse will error with a useful
	// message if it's actually neither.
	return "json"
}

// parseAnalystOutput dispatches on format and returns a parsed
// AnalystOutput. JSON parsing uses DisallowUnknownFields so a typo'd
// field name produces a clear error rather than a silent drop.
func parseAnalystOutput(raw []byte, format string) (*exchange.AnalystOutput, error) {
	switch format {
	case "json":
		var out exchange.AnalystOutput
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&out); err != nil {
			return nil, err
		}
		return &out, nil
	case "markdown":
		return exchange.UnmarshalMarkdown(raw)
	default:
		return nil, fmt.Errorf("unknown format %q (expected json, markdown, or auto)", format)
	}
}
