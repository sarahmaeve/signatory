package main

import (
	"context"
	"fmt"
	"os"
)

// IngestCmd loads a JSON or markdown analyst output file into the
// signatory store. It validates the document via format-check
// internals, then writes the structured rows per migration v5
// (analyst_outputs + conclusions + observations + methodology + ...).
//
// Idempotent on content: re-ingesting the same file is a no-op
// because analyst_outputs.content_hash has a UNIQUE constraint.
// The reported OutputID will be the existing row's UUID in that
// case, with an "(idempotent: existing row)" note in the summary.
//
// This command pairs with `signatory format-check` — format-check
// confirms a file parses; ingest commits it. Most callers should
// run format-check first as a pre-flight, but ingest re-validates
// at the store layer regardless (defense in depth).
type IngestCmd struct {
	File   string `arg:"" help:"Path to a JSON or markdown analyst output file." type:"existingfile"`
	Format string `help:"Input format: json, markdown, or auto (detect from extension/content)." default:"auto" enum:"json,markdown,auto"`
	Quiet  bool   `help:"Suppress success summary; errors still print." short:"q"`
}

func (cmd *IngestCmd) Run(globals *Globals) error {
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

	// Re-validate at the ingest layer in case the caller skipped
	// format-check. The store-layer call also validates, but
	// surfacing the error here gives a clearer command-line error.
	if err := out.Validate(); err != nil {
		return fmt.Errorf("validate %s:\n%w", cmd.File, err)
	}

	ctx := context.Background()
	store, err := globals.OpenStore(ctx)
	if err != nil {
		return err
	}
	defer store.Close()

	result, err := store.IngestAnalystOutput(ctx, out, cmd.File)
	if err != nil {
		return fmt.Errorf("ingest %s: %w", cmd.File, err)
	}

	if !cmd.Quiet {
		patternCount := 0
		if out.MethodologyTrace != nil {
			patternCount = len(out.MethodologyTrace.Patterns)
		}
		idempotency := ""
		if result.Idempotent {
			idempotency = " (idempotent: matched existing row by content_hash)"
		}
		fmt.Printf("Ingested %s (%s) → output_id=%s entity_id=%s%s\n",
			cmd.File, format, result.OutputID, result.EntityID, idempotency)
		fmt.Printf("  %d conclusion(s), %d positive absence(s), %d observation(s), %d methodology pattern(s)\n",
			len(out.Conclusions), len(out.PositiveAbsences),
			len(out.Observations), patternCount)
	}
	return nil
}
