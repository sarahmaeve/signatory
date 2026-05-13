package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/sarahmaeve/signatory/internal/deltas"
)

// DeltasCmd surfaces signal-value changes for a target over time.
// See design/deltas.md for the full spec — diff semantics, output
// shapes, and the Phase 1 / Phase 2 split.
//
// The verb itself is a thin shell: flag parsing, mutex check,
// --all confirmation prompt, render-target wiring. The actual
// store query → filter → window → diff pipeline lives in
// internal/deltas (Computer.Compute), shared with the
// signatory_deltas MCP tool.
type DeltasCmd struct {
	Target string `arg:"" help:"Target URI (pkg:npm/X, pkg:pypi/Y, repo:github/owner/repo, etc.)."`

	Since string `help:"Time-bounded view. Word shortcuts (yesterday, last-week, last-month), Go duration (2d, 12h, 30m), or RFC3339 timestamp. Default '24h' when no time flag is set."`
	Last  int    `help:"Show the most recent N observations per (type, source) group. Mutually exclusive with --since, --range, and --all."`
	Range string `help:"Bounded range as 'T1..T2' (inclusive). Each endpoint accepts the same syntax as --since. Mutually exclusive with --since, --last, and --all."`
	All   bool   `help:"Show the full history. Mutually exclusive with --since, --last, and --range. Prompts for confirmation when more than 10 collection runs are present (use --yes to skip)."`

	Type   string `help:"Filter by signal type (e.g., trusted_publishing, version_unpublish_observed)."`
	Source string `help:"Filter by collector source (e.g., npm-registry, github, git)."`
	Group  string `help:"Filter by signal group (vitality, governance, publication, hygiene, criticality, identity)."`

	IncludeUnchanged bool `help:"Include signals with no changes in the window. Default behavior suppresses them."`
	Expand           bool `help:"Restore per-transition detail for forge-drift signals (stars, forks, followers, open_issues). Default collapses these into a footer."`
	JSON             bool `help:"Emit structured JSON instead of human-readable text."`
	Yes              bool `short:"y" help:"Skip the confirmation prompt for large --all expansions."`

	Stdout io.Writer `kong:"-"`
	Stderr io.Writer `kong:"-"`
	Stdin  io.Reader `kong:"-"`
}

// Run executes the deltas query against the configured store and
// writes the rendered output (text or JSON) to cmd.Stdout. Warnings
// (e.g., large --all result sets) go to cmd.Stderr.
func (cmd *DeltasCmd) Run(globals *Globals) error {
	ctx := globals.Context
	if ctx == nil {
		ctx = context.Background()
	}
	if cmd.Stdout == nil {
		cmd.Stdout = os.Stdout
	}
	if cmd.Stderr == nil {
		cmd.Stderr = os.Stderr
	}
	if cmd.Stdin == nil {
		cmd.Stdin = os.Stdin
	}

	if err := cmd.validateFlags(); err != nil {
		return err
	}

	s, err := globals.OpenStore(ctx)
	if err != nil {
		return err
	}
	defer s.Close() //nolint:errcheck // store close on command exit; error not actionable

	window, err := cmd.window()
	if err != nil {
		return err
	}

	in, err := deltas.New(s).Compute(ctx, deltas.Params{
		Target: cmd.Target,
		Window: window,
		Type:   cmd.Type,
		Source: cmd.Source,
		Group:  cmd.Group,
	})
	if err != nil {
		if errors.Is(err, deltas.ErrEntityNotFound) {
			return fmt.Errorf("target %q not found in store — run `signatory analyze --refresh %s` first", cmd.Target, cmd.Target)
		}
		return err
	}

	// --all bound-check: when the rendered set has more than
	// `allRunsPromptThreshold` distinct collection runs, ask the
	// user before scrolling through it. --yes (or -y) bypasses.
	// EOF / non-interactive callers default to "no" so scripts
	// don't hang.
	if cmd.All {
		runs := countRunsInRender(in)
		proceed, err := confirmAllExpansion(cmd.Stderr, cmd.Stdin, runs, in.Target, cmd.Yes)
		if err != nil {
			return err
		}
		if !proceed {
			return nil
		}
	}

	if cmd.JSON {
		return deltas.RenderJSON(cmd.Stdout, in)
	}
	return deltas.RenderText(cmd.Stdout, in, deltas.TextOpts{
		IncludeUnchanged: cmd.IncludeUnchanged,
		Expand:           cmd.Expand,
	})
}

// validateFlags enforces mutual exclusion of --since, --last,
// --range, and --all. Kong doesn't have native group-mutex; we do
// it at Run time.
func (cmd *DeltasCmd) validateFlags() error {
	set := 0
	if cmd.Since != "" {
		set++
	}
	if cmd.Last > 0 {
		set++
	}
	if cmd.Range != "" {
		set++
	}
	if cmd.All {
		set++
	}
	if set > 1 {
		return NewUsageError(errors.New("--since, --last, --range, and --all are mutually exclusive"))
	}
	return nil
}

// window resolves the (--since | --last | --range | --all) tuple
// into a deltas.TimeWindow. Default when none is set: --since 24h.
func (cmd *DeltasCmd) window() (deltas.TimeWindow, error) {
	if cmd.All {
		return deltas.TimeWindow{All: true}, nil
	}
	if cmd.Last > 0 {
		return deltas.TimeWindow{Last: cmd.Last}, nil
	}
	if cmd.Range != "" {
		start, end, err := parseRangeShorthand(cmd.Range)
		if err != nil {
			return deltas.TimeWindow{}, NewUsageError(fmt.Errorf("--range %q: %w", cmd.Range, err))
		}
		return deltas.TimeWindow{RangeStart: start, RangeEnd: end}, nil
	}
	raw := cmd.Since
	if raw == "" {
		raw = "24h"
	}
	cutoff, err := parseTimeShorthand(raw)
	if err != nil {
		return deltas.TimeWindow{}, NewUsageError(fmt.Errorf("--since %q: %w", raw, err))
	}
	return deltas.TimeWindow{Cutoff: cutoff}, nil
}
