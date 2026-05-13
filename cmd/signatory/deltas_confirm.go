package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/sarahmaeve/signatory/internal/profile"
)

// allRunsPromptThreshold is the inclusive upper bound on "runs"
// (distinct collected_at timestamps for the rendered set) below which
// `signatory deltas --all` proceeds silently. Above this, the user is
// asked to confirm the expansion.
//
// 10 was chosen because 1–5 runs is a normal small history, 6–10 is
// borderline, and 10+ is when scrolling becomes a real cost. The
// 2026-05-12 tanstack dogfood produced ~10 runs in one day; that's
// roughly where the warning should start firing.
//
// Tunable in one place; no env var or config knob yet — change the
// constant if production usage suggests a different floor.
const allRunsPromptThreshold = 10

// countRuns returns the number of distinct CollectedAt timestamps
// across the signal slice. A single `signatory analyze` invocation
// produces many signal rows that share a collected_at, so this is
// the closest the store has to a "run" count.
//
// Signals whose timestamps differ by sub-second jitter (different
// collectors finishing at slightly different millisecond boundaries)
// would count as separate runs — acceptable for v1 since the
// threshold is well above any realistic jitter cluster, and a
// stricter bucket-by-minute heuristic would obscure deliberate
// back-to-back runs the user actually performed.
func countRuns(signals []profile.Signal) int {
	if len(signals) == 0 {
		return 0
	}
	seen := make(map[time.Time]struct{}, len(signals))
	for _, sig := range signals {
		seen[sig.CollectedAt] = struct{}{}
	}
	return len(seen)
}

// confirmAllExpansion prompts the user to confirm a large `--all`
// expansion. Returns (proceed, err). The caller passes the runs
// count (see countRuns), the target URI for context, and a yes flag
// that short-circuits the prompt.
//
// Defaults:
//   - yes=true: skip prompt, proceed.
//   - runs <= allRunsPromptThreshold: skip prompt, proceed.
//   - prompt fires: y/Y/yes/YES (case-insensitive, whitespace-tolerant)
//     proceeds; anything else (including bare Enter and EOF) bails.
//
// EOF is treated as "no" so a non-interactive caller (closed pipe,
// no --yes) exits clean rather than blocking on stdin forever.
//
// The prompt text is written to `out`; in production this is the
// command's Stderr so it doesn't contaminate machine-readable stdout.
func confirmAllExpansion(out io.Writer, in io.Reader, runs int, target string, yes bool) (bool, error) {
	if yes {
		return true, nil
	}
	if runs <= allRunsPromptThreshold {
		return true, nil
	}

	if _, err := fmt.Fprintf(out,
		"warning: %d runs of observations for %s. Continue (y/N)? ",
		runs, target); err != nil {
		return false, fmt.Errorf("write prompt: %w", err)
	}

	if in == nil {
		// No reader at all — caller didn't wire stdin. Match the
		// non-interactive bail path; safer than blocking.
		return false, nil
	}

	reader := bufio.NewReader(in)
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("read confirmation: %w", err)
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	switch answer {
	case "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}
