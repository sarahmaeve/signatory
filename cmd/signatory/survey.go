package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sarahmaeve/signatory/internal/manifest"
	"github.com/sarahmaeve/signatory/internal/survey"
)

// SurveyCmd assesses the trust posture of a project's dependency
// tree. Parses a manifest (v0.1: go.mod only), looks up each
// dependency in the store, and reports per-dep tier plus
// aggregate summary.
//
// The dashboard entry point from ROADMAP.md's v0.1 must-do #1 —
// the first surface that turns signatory's accumulated analyses
// into a per-project "what's the state of my dep tree?" view.
//
// Output: human-readable by default (summary + direct-deps
// table + indirect count + action items); --json for machine
// consumption by downstream tooling (CI gates, dashboards,
// future web UI).
//
// Scope note: --refresh is accepted but ignored in v0.1 with a
// stderr note. A useful cross-dep refresh requires decisions
// about network-cost budgets, partial-failure handling, and
// cloning machinery that aren't yet settled. Users can refresh
// individual deps via `signatory analyze <target> --refresh`.
type SurveyCmd struct {
	Manifest string `help:"Path to dependency manifest (default: auto-detect go.mod in CWD)." short:"m" type:"existingfile" optional:""`
	Refresh  bool   `help:"Collect fresh signals for each dep (not implemented in v0.1; use 'signatory analyze <target> --refresh' per-dep)." default:"false"`
	JSON     bool   `help:"Output as JSON." default:"false"`
	All      bool   `help:"Include indirect dependencies in the output (default: summarize by count)." short:"a" default:"false"`

	// Stdout and Stderr let tests inject buffers. Production paths
	// leave them nil; Run defaults them to os.Stdout / os.Stderr.
	// kong:"-" excludes them from the CLI flag surface — they're a
	// testing seam, not a user-tunable knob.
	Stdout io.Writer `kong:"-"`
	Stderr io.Writer `kong:"-"`
}

func (cmd *SurveyCmd) Run(globals *Globals) error {
	ctx := context.Background()

	// Default the stdio sinks when production callers leave them
	// nil. Tests inject buffers via cmd.Stdout / cmd.Stderr to
	// capture output without racing on the process-wide
	// os.Stdout / os.Stderr globals.
	stdout := cmd.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	stderr := cmd.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	manifestPath := cmd.Manifest
	if manifestPath == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("resolve current directory: %w", err)
		}
		path, _, err := manifest.Detect(cwd)
		if err != nil {
			return fmt.Errorf("%w\npass --manifest explicitly to survey a manifest outside the current directory", err)
		}
		manifestPath = path
	}

	if cmd.Refresh {
		fmt.Fprintln(stderr, "# --refresh is not implemented in survey v0.1; use `signatory analyze <target> --refresh` for individual deps")
	}

	s, err := globals.OpenStore(ctx)
	if err != nil {
		return err
	}
	defer s.Close() //nolint:errcheck // store close on command exit; error not actionable

	result, err := survey.Run(ctx, s, manifestPath)
	if err != nil {
		return err
	}

	if cmd.JSON {
		return printSurveyJSON(stdout, result)
	}
	return printSurveyHuman(stdout, result, cmd.All)
}

// printSurveyJSON emits the result as indented JSON to w. Stable
// map iteration is handled by Go's encoding/json (maps are
// sorted by key). Deps are in manifest order, which is the
// intuitive order for a consumer reading a dependency tree.
func printSurveyJSON(w io.Writer, r survey.Result) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal survey result: %w", err)
	}
	_, err = fmt.Fprintln(w, string(data))
	return err
}

// printSurveyHuman renders the survey result as a terminal
// dashboard to w: project header, summary counts, direct-deps
// table, indirect summary, action items.
//
// Width discipline: the direct-deps column layout targets an
// 80-character terminal by default. Longer dep names or version
// strings still render; they just wrap. The goal is scannable
// first-glance state, not pixel-perfect alignment.
//
// All output goes through the writer — no global os.Stdout
// references — so tests can inject a bytes.Buffer and run
// with t.Parallel() safely.
func printSurveyHuman(w io.Writer, r survey.Result, includeIndirect bool) error {
	// ---- Project header ----
	fmt.Fprintf(w, "Surveying %s\n", filepath.Base(r.Project.ManifestPath))
	if r.Project.Name != "" {
		ecoVer := r.Project.EcoVersion
		if ecoVer == "" {
			ecoVer = "(unspecified)"
		}
		fmt.Fprintf(w, "  project:  %s (%s %s)\n", r.Project.Name, r.Project.Ecosystem, ecoVer)
	}
	fmt.Fprintf(w, "  manifest: %s\n", r.Project.ManifestPath)
	fmt.Fprintln(w)

	// ---- Summary ----
	fmt.Fprintln(w, "Summary")
	fmt.Fprintf(w, "  %d dependencies   (%d direct · %d indirect)\n",
		r.Summary.Total, r.Summary.Direct, r.Summary.Indirect)
	// Ordered tier output — stable across runs and matches the
	// design/trust-policy-v1.md ordering (strongest-endorsement
	// to strongest-rejection, with synthetic tiers at the end).
	tierOrder := []survey.Tier{
		survey.TierVettedFrozen,
		survey.TierTrustedForNow,
		survey.TierUnexamined,
		survey.TierUnknownProvenance,
		survey.TierRejected,
		survey.TierBurned,
		survey.TierLocalReplace,
		survey.TierNotInStore,
	}
	for _, tier := range tierOrder {
		count := r.Summary.ByTier[tier]
		if count > 0 {
			fmt.Fprintf(w, "  %-5d %s\n", count, tier)
		}
	}
	fmt.Fprintln(w)

	// ---- Direct deps table ----
	direct := filterDirectDeps(r.Deps)
	if len(direct) > 0 {
		fmt.Fprintf(w, "Direct dependencies (%d)\n", len(direct))
		for _, d := range direct {
			renderDep(w, d)
		}
		fmt.Fprintln(w)
	}

	// ---- Indirect deps (count by default, full list with --all) ----
	indirect := filterIndirectDeps(r.Deps)
	if len(indirect) > 0 {
		if includeIndirect {
			fmt.Fprintf(w, "Indirect dependencies (%d)\n", len(indirect))
			for _, d := range indirect {
				renderDep(w, d)
			}
			fmt.Fprintln(w)
		} else {
			byTier := map[survey.Tier]int{}
			for _, d := range indirect {
				byTier[d.Tier]++
			}
			summary := formatIndirectSummary(byTier)
			fmt.Fprintf(w, "Indirect dependencies: %d — %s\n", len(indirect), summary)
			fmt.Fprintln(w, "  (use --all to list)")
			fmt.Fprintln(w)
		}
	}

	// ---- Action items ----
	if len(r.Summary.NeedsReview) > 0 {
		fmt.Fprintln(w, "Action items")
		fmt.Fprintf(w, "  %d direct dependencies to analyze:\n", len(r.Summary.NeedsReview))
		for _, uri := range r.Summary.NeedsReview {
			cloneName := cloneDirNameForURI(uri)
			fmt.Fprintf(w, "    signatory analyze %s --refresh --clone --path filestore/clones/%s\n",
				analyzableForm(uri), cloneName)
		}
		fmt.Fprintln(w)
	} else if len(direct) > 0 {
		// All direct deps have resolved tiers. Celebrate briefly.
		fmt.Fprintln(w, "No outstanding action items — every direct dep has a resolved tier.")
		fmt.Fprintln(w)
	}

	return nil
}

// renderDep prints one line for a DepResult to w. Format:
//
//	[icon] <name><pad> <version><pad> <tier>  <suffix>
//
// The icon is a two-character visual cue. suffix carries context
// like "burn: <reason>" or "other-versions" or the posture
// rationale (truncated).
func renderDep(w io.Writer, d survey.DepResult) {
	icon := tierIcon(d.Tier)
	name := d.Dep.Name
	version := d.Dep.Version
	if version == "" {
		version = "(no version)"
	}

	// Pad name and version for visual alignment at typical widths.
	// Long names just overflow; alignment is a scannability
	// affordance, not a hard constraint.
	const nameWidth = 40
	const versionWidth = 16
	paddedName := padRight(name, nameWidth)
	paddedVersion := padRight(version, versionWidth)

	suffix := ""
	switch {
	case d.Tier == survey.TierBurned && d.BurnReason != "":
		suffix = "burn: " + truncate(d.BurnReason, 60)
	case d.HasOtherVersions:
		suffix = "(other versions in store)"
	case d.PostureRationale != "":
		suffix = truncate(d.PostureRationale, 60)
	}

	if suffix != "" {
		fmt.Fprintf(w, "  %s %s %s %-20s %s\n", icon, paddedName, paddedVersion, d.Tier, suffix)
	} else {
		fmt.Fprintf(w, "  %s %s %s %s\n", icon, paddedName, paddedVersion, d.Tier)
	}
}

// tierIcon returns a short visual marker for each tier. Uses
// bracketed ASCII rather than emoji so the output stays
// scannable in log files, CI output, and terminals without
// emoji support.
func tierIcon(t survey.Tier) string {
	switch t {
	case survey.TierVettedFrozen:
		return "[✓]"
	case survey.TierTrustedForNow:
		return "[~]"
	case survey.TierUnexamined:
		return "[?]"
	case survey.TierUnknownProvenance:
		return "[?]"
	case survey.TierRejected:
		return "[✗]"
	case survey.TierBurned:
		return "[!]"
	case survey.TierLocalReplace:
		return "[→]"
	case survey.TierNotInStore:
		return "[·]"
	default:
		return "[·]"
	}
}

// formatIndirectSummary produces a compact one-line summary of
// indirect-dep tier distribution, for the default (non-verbose)
// indirect output.
func formatIndirectSummary(byTier map[survey.Tier]int) string {
	if len(byTier) == 0 {
		return "none"
	}
	// Emit in tier-order precedence, same ordering as the main
	// summary block.
	tiers := []survey.Tier{
		survey.TierVettedFrozen,
		survey.TierTrustedForNow,
		survey.TierUnexamined,
		survey.TierUnknownProvenance,
		survey.TierRejected,
		survey.TierBurned,
		survey.TierLocalReplace,
		survey.TierNotInStore,
	}
	parts := make([]string, 0, len(tiers))
	for _, t := range tiers {
		if byTier[t] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", byTier[t], t))
		}
	}
	return strings.Join(parts, ", ")
}

// analyzableForm converts a canonical URI back to a form
// `signatory analyze` accepts gracefully. `repo:github/X/Y`
// becomes `github.com/X/Y` (the shorthand ResolveTarget
// handles); `pkg:go/...` stays as-is.
func analyzableForm(uri string) string {
	const repoGithubPrefix = "repo:github/"
	if strings.HasPrefix(uri, repoGithubPrefix) {
		return "github.com/" + strings.TrimPrefix(uri, repoGithubPrefix)
	}
	return uri
}

// cloneDirNameForURI derives a safe directory-component name for
// `--path filestore/clones/<name>` from a canonical URI. Uses
// the last path segment, sanitized. For `pkg:go/modernc.org/sqlite`
// this yields "sqlite"; for `repo:github/alecthomas/kong` this
// yields "kong".
func cloneDirNameForURI(uri string) string {
	for i := len(uri) - 1; i >= 0; i-- {
		if uri[i] == '/' {
			return uri[i+1:]
		}
	}
	return uri
}

func filterDirectDeps(deps []survey.DepResult) []survey.DepResult {
	var out []survey.DepResult
	for _, d := range deps {
		if d.Dep.Direct {
			out = append(out, d)
		}
	}
	// Stable sort by name so multiple runs against the same
	// manifest produce identical output (helps diffing / CI).
	sort.SliceStable(out, func(i, j int) bool { return out[i].Dep.Name < out[j].Dep.Name })
	return out
}

func filterIndirectDeps(deps []survey.DepResult) []survey.DepResult {
	var out []survey.DepResult
	for _, d := range deps {
		if !d.Dep.Direct {
			out = append(out, d)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Dep.Name < out[j].Dep.Name })
	return out
}

func padRight(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(s))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}
