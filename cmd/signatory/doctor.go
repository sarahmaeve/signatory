package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/sarahmaeve/signatory/internal/doctor"
	"github.com/sarahmaeve/signatory/internal/store"
)

// DoctorCmd runs the breadth-pass health check across the local
// signatory setup: Go runtime, env vars, TLS trust, MCP wiring,
// store, and the optional pipeline service. The probe registry
// lives in internal/doctor; this file is the CLI glue: argv →
// Options, Results → rendered prose or JSON, exit code.
//
// Designed for two audiences:
//
//   - Humans who hit a problem and want a one-shot diagnostic.
//     Default rendering is multi-line prose, modeled on
//     `signatory certs doctor`, with a [ok|warn|fail] tag, the
//     observed message, and a fix line when relevant.
//
//   - Scripts and CI that want a yes/no signal. `--json` emits a
//     structured report. `--strict` promotes any warn to a
//     non-zero exit so a CI gate can require a clean setup.
//
// Returns errSilentFailure (recognized by main.go) on non-zero
// exit so the human-readable report is the only thing the user
// sees — no duplicate stderr line.
type DoctorCmd struct {
	JSON   bool   `help:"Emit structured JSON instead of prose. Useful for scripts and CI."`
	Strict bool   `help:"Treat any warning as an exit-1 condition. Off by default (warns are informational)."`
	Only   string `help:"Comma-separated list of probe names to run. Default is all probes."`

	// Test seams.
	Stdout io.Writer                            `kong:"-"`
	Runner func(doctor.Options) []doctor.Result `kong:"-"`
}

// DoctorReport is the JSON contract for `signatory doctor --json`.
// Status mirrors the worst probe outcome ("ok", "warn", "fail"), so
// scripts can branch on the top-level field without iterating
// Results. Results are emitted in registry order — the same order
// as the prose rendering — so a diff between two reports stays
// readable.
type DoctorReport struct {
	Status  string          `json:"status"`
	Results []doctor.Result `json:"results"`
}

func (cmd *DoctorCmd) Run(globals *Globals) error {
	stdout := cmd.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	runner := cmd.Runner
	if runner == nil {
		runner = doctor.Run
	}

	opts := doctor.Options{
		Version:   version,
		Commit:    commit,
		BuildDate: buildDate,
		DBPath:    globals.DBPath,
		Only:      splitOnly(cmd.Only),

		// OpenStore wraps store.OpenSQLite + Close. Doctor only
		// needs to know whether the open + ping succeeds; we close
		// immediately so we don't hold the SQLite write lock past
		// the probe.
		OpenStore: func(ctx context.Context, dbPath string) error {
			path, err := store.ResolvePath(dbPath)
			if err != nil {
				return fmt.Errorf("resolve database path: %w", err)
			}
			s, err := store.OpenSQLite(ctx, path)
			if err != nil {
				return err
			}
			return s.Close()
		},
	}

	results := runner(opts)

	if cmd.JSON {
		// JSON path: write the structured report unconditionally,
		// then drive the exit via errSilentFailure when needed so
		// the caller sees a parseable stdout even on failure.
		if err := writeJSON(stdout, &DoctorReport{
			Status:  worstStatus(results),
			Results: results,
		}); err != nil {
			return err
		}
		if shouldFail(results, cmd.Strict) {
			return errSilentFailure
		}
		return nil
	}

	renderProse(stdout, results)
	if shouldFail(results, cmd.Strict) {
		return errSilentFailure
	}
	return nil
}

// splitOnly parses the comma-separated --only flag into a slice.
// Whitespace around each name is trimmed so `--only "a, b, c"`
// works as expected. Empty input → nil (run all probes), matching
// the contract internal/doctor.Run expects.
func splitOnly(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// worstStatus returns the most severe Status across results, in
// the order fail > warn > ok. Used by the JSON envelope's top-level
// status field. Empty results → "ok" (no probes ran, nothing
// failed).
func worstStatus(results []doctor.Result) string {
	worst := doctor.StatusOK
	for _, r := range results {
		switch r.Status {
		case doctor.StatusFail:
			return string(doctor.StatusFail)
		case doctor.StatusWarn:
			if worst == doctor.StatusOK {
				worst = doctor.StatusWarn
			}
		}
	}
	return string(worst)
}

// shouldFail decides whether the command should exit non-zero.
// Any fail always trips it; a warn does only under --strict. This
// keeps the default behavior friendly (warns are informational)
// while giving scripts the strict mode they need to gate on.
func shouldFail(results []doctor.Result, strict bool) bool {
	if doctor.HasFail(results) {
		return true
	}
	if strict && doctor.HasWarn(results) {
		return true
	}
	return false
}

// renderProse writes the human-readable report. Format matches
// `certs doctor` for visual consistency:
//
//	signatory doctor
//	────────────────────────────────────────
//	[ ok ] go-runtime           go1.25.1
//	[warn] github-token         GITHUB_TOKEN is not set...
//	       fix: export GITHUB_TOKEN=...
//	────────────────────────────────────────
//	status: NOT OK (1 fail, 2 warn)
//
// The fix line is indented under its probe so the visual flow is
// "what's wrong → how to fix it" without ambiguity about which
// probe owns the fix.
func renderProse(w io.Writer, results []doctor.Result) {
	const sep = "────────────────────────────────────────"
	fmt.Fprintln(w, "signatory doctor")
	fmt.Fprintln(w, sep)

	// Compute the column width for the probe-name field so the
	// status messages line up no matter which probes were filtered
	// in or out. Capped at a reasonable max to keep one
	// pathologically long name from squashing the message column.
	const maxNameWidth = 24
	nameWidth := 0
	for _, r := range results {
		if w := len(r.Name); w > nameWidth {
			nameWidth = w
		}
	}
	if nameWidth > maxNameWidth {
		nameWidth = maxNameWidth
	}

	for _, r := range results {
		tag := statusTag(r.Status)
		fmt.Fprintf(w, "%s %-*s  %s\n", tag, nameWidth, r.Name, r.Message)
		if r.Fix != "" {
			indent := strings.Repeat(" ", len(tag)+1+nameWidth+2)
			fmt.Fprintf(w, "%sfix: %s\n", indent, r.Fix)
		}
	}

	fmt.Fprintln(w, sep)
	failCount, warnCount := countByStatus(results)
	switch {
	case failCount > 0:
		fmt.Fprintf(w, "status: NOT OK (%d fail, %d warn)\n", failCount, warnCount)
	case warnCount > 0:
		fmt.Fprintf(w, "status: OK with warnings (%d warn)\n", warnCount)
	default:
		fmt.Fprintln(w, "status: OK")
	}
}

// statusTag returns the bracketed three-letter status tag, padded
// to a uniform width so columns align regardless of which symbol
// fired. "[ ok ]" / "[warn]" / "[fail]" — same width and visually
// distinct enough to scan at a glance.
func statusTag(s doctor.Status) string {
	switch s {
	case doctor.StatusOK:
		return "[ ok ]"
	case doctor.StatusWarn:
		return "[warn]"
	case doctor.StatusFail:
		return "[fail]"
	default:
		return "[????]"
	}
}

func countByStatus(results []doctor.Result) (failCount, warnCount int) {
	for _, r := range results {
		switch r.Status {
		case doctor.StatusFail:
			failCount++
		case doctor.StatusWarn:
			warnCount++
		}
	}
	return failCount, warnCount
}
