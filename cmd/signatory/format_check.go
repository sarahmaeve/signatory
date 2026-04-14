package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	File   string `arg:"" help:"Path to a JSON or markdown analyst output file." type:"existingfile"`
	Format string `help:"Input format: json, markdown, or auto (detect from extension/content)." default:"auto" enum:"json,markdown,auto"`
	Quiet  bool   `help:"Suppress the success summary; errors still print." short:"q"`
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

	if !cmd.Quiet {
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
