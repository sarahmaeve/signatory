package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/sarahmaeve/signatory/internal/exchange"
)

// BuildOutputCmd converts structured agent text to v1-schema JSON.
//
// This implements the "tokens for judgment, code for structure"
// principle (design/agent-output-contract.md): agents produce
// structured natural language (their strongest modality); this
// command handles serialization to schema-valid JSON. The agent
// never writes raw JSON — the binary constructs it.
//
// The structured text format is documented in
// design/agent-output-contract.md and uses H2-delimited sections
// with key: value fields for metadata and freeform body text for
// rationale. Citations use a compact path:line-line "quoted" syntax
// that the parser expands into proper Citation objects.
type BuildOutputCmd struct {
	From   string `arg:"" help:"Path to the structured agent text file." type:"existingfile"`
	Target string `help:"Canonical URI for the target (e.g. pkg:pypi/python-dotenv, repo:github/owner/name). Overrides any Target line in the text." required:""`
	Output string `short:"o" help:"Write v1-schema JSON to this path. Stdout if omitted."`
	Force  bool   `help:"Overwrite --output if it exists."`
}

func (cmd *BuildOutputCmd) Run(globals *Globals) error {
	f, err := os.Open(cmd.From)
	if err != nil {
		return fmt.Errorf("open input: %w", err)
	}
	defer func() { _ = f.Close() }()

	out, err := exchange.ParseStructuredOutput(f, cmd.Target)
	if err != nil {
		return fmt.Errorf("parse structured output: %w", err)
	}

	// Validate the parsed output using the same logic as format-check.
	if err := out.Validate(); err != nil {
		fmt.Fprintln(os.Stderr, "validation errors in parsed output:")
		fmt.Fprintf(os.Stderr, "  %s\n", err)
		return fmt.Errorf("parsed output failed validation; fix the structured text and retry")
	}

	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal JSON: %w", err)
	}
	data = append(data, '\n')

	if cmd.Output == "" {
		_, err = os.Stdout.Write(data)
		return err
	}

	flag := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if !cmd.Force {
		flag = os.O_WRONLY | os.O_CREATE | os.O_EXCL
	}
	w, err := os.OpenFile(cmd.Output, flag, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("%s already exists; pass --force to overwrite", cmd.Output)
		}
		return fmt.Errorf("create output: %w", err)
	}
	if _, err := w.Write(data); err != nil {
		_ = w.Close()
		return fmt.Errorf("write output: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("close output: %w", err)
	}

	fmt.Fprintf(os.Stderr, "built %s → %s (%d conclusion(s), %d absence(s), %d observation(s))\n",
		cmd.From, cmd.Output,
		len(out.Conclusions), len(out.PositiveAbsences), len(out.Observations))
	return nil
}
