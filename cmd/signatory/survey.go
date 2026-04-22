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
// table + indirect count); --json for machine consumption by
// downstream tooling (CI gates, dashboards, future web UI).
//
// Deliberately does NOT render an "action items" / suggested-
// commands section. The CLI verbs survey would naturally point at
// (`signatory analyze ...`) only collect signals — they cannot
// produce the trust verdict that flips a [?] row into [✓] / [✗].
// That verdict requires an analyst-agent dispatch (the /analyze
// Claude skill, or any client invoking signatory_ingest_analysis
// over MCP). Suggesting a CLI command that doesn't deliver the
// thing the user expects is worse than suggesting nothing —
// removed 2026-04-22 after dogfood walk-through made the
// vocabulary mismatch explicit.
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
		// Stderr diagnostic; deliberate discard. See stickyWriter's
		// doc in analyze.go for the asymmetry between contract
		// output (propagates) and progress/warning text (discards).
		_, _ = fmt.Fprintln(stderr, "# --refresh is not implemented in survey v0.1; use `signatory analyze <target> --refresh` for individual deps")
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
// table, indirect summary. See the package-level doc for why
// no "action items" / suggested-commands section is emitted.
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
	sw := &stickyWriter{w: w}

	// ---- Project header ----
	sw.Writef("Surveying %s\n", filepath.Base(r.Project.ManifestPath))
	if r.Project.Name != "" {
		ecoVer := r.Project.EcoVersion
		if ecoVer == "" {
			ecoVer = "(unspecified)"
		}
		sw.Writef("  project:  %s (%s %s)\n", r.Project.Name, r.Project.Ecosystem, ecoVer)
	}
	sw.Writef("  manifest: %s\n", r.Project.ManifestPath)
	sw.Writeln()

	// ---- Summary ----
	sw.Writeln("Summary")
	sw.Writef("  %d dependencies   (%d direct · %d indirect)\n",
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
			if annotation := tierSummaryAnnotation(tier); annotation != "" {
				sw.Writef("  %-5d %-18s %s\n", count, tier, annotation)
			} else {
				sw.Writef("  %-5d %s\n", count, tier)
			}
		}
	}
	sw.Writeln()

	// ---- Direct deps table ----
	direct := filterDirectDeps(r.Deps)
	if len(direct) > 0 {
		sw.Writef("Direct dependencies (%d)\n", len(direct))
		for _, d := range direct {
			renderDep(sw, d)
		}
		sw.Writeln()
	}

	// ---- Indirect deps (count by default, full list with --all) ----
	indirect := filterIndirectDeps(r.Deps)
	if len(indirect) > 0 {
		if includeIndirect {
			sw.Writef("Indirect dependencies (%d)\n", len(indirect))
			for _, d := range indirect {
				renderDep(sw, d)
			}
			sw.Writeln()
		} else {
			byTier := map[survey.Tier]int{}
			for _, d := range indirect {
				byTier[d.Tier]++
			}
			summary := formatIndirectSummary(byTier)
			sw.Writef("Indirect dependencies: %d — %s\n", len(indirect), summary)
			renderIndirectBreakdown(sw, r.Summary.IndirectByReachability)
			sw.Writeln("  (use --all to list)")
			sw.Writeln()
		}
	}

	// ---- Vet-path footer (shown only when direct deps need review) ----
	//
	// Guarded on NeedsReview so a fully-vetted project gets no
	// footer — printing navigation hints for a resolved state is
	// noise. Names *categories* of next move rather than specific
	// verbatim commands: /analyze (the Claude skill, not the CLI
	// verb) for an LLM-backed review, posture set for a verdict
	// the user already holds, burn for a known-bad. Replaces the
	// prior "Action items" section that rendered commands which
	// didn't produce postures — see package-level doc.
	if len(r.Summary.NeedsReview) > 0 {
		sw.Writeln("To vet direct dependencies:")
		sw.Writeln("  /analyze <target>             — LLM-backed analyst review (Claude session + signatory MCP)")
		sw.Writeln("  signatory posture set ...     — record a verdict you already hold")
		sw.Writeln("  signatory burn ...            — flag as compromised or known-bad")
		sw.Writeln()
	}

	return sw.Err()
}

// renderDep prints one line for a DepResult via sw. Format:
//
//	[icon] <name><pad> <version><pad> <tier>  <suffix>
//
// The icon is a two-character visual cue. suffix carries context
// like "burn: <reason>" or "other-versions" or the posture
// rationale (truncated).
//
// Takes a *stickyWriter so it participates in printSurveyHuman's
// error chain: if the caller has already hit a broken pipe, the
// format calls here become no-ops instead of racing to write to
// a closed stream.
func renderDep(sw *stickyWriter, d survey.DepResult) {
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
	case d.OtherVersions != nil && d.OtherVersions.MostRecent != nil:
		// Surface the most-recent prior-version posture plus a
		// posture-count. Visibility only — the user decides whether
		// to navigate (e.g., `signatory posture get --all`) or
		// commission a fresh review. We deliberately do NOT render
		// a suggested extension verb here; see the survey package
		// doc for why.
		noun := "postures"
		if d.OtherVersions.TotalPostures == 1 {
			noun = "posture"
		}
		suffix = fmt.Sprintf("(%s %s; %d %s on record)",
			d.OtherVersions.MostRecent.Version,
			d.OtherVersions.MostRecent.Tier,
			d.OtherVersions.TotalPostures, noun)
	case d.PostureRationale != "":
		suffix = truncate(d.PostureRationale, 60)
	}

	if suffix != "" {
		sw.Writef("  %s %s %s %-20s %s\n", icon, paddedName, paddedVersion, d.Tier, suffix)
	} else {
		sw.Writef("  %s %s %s %s\n", icon, paddedName, paddedVersion, d.Tier)
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

// renderIndirectBreakdown emits the three-bucket breakdown of
// indirect deps (resolved on their own / inherit coverage from
// resolved directs / await an unresolved direct) when the
// reachability pass produced data. When data is unavailable
// (graph extraction not implemented for this ecosystem, or the
// toolchain failed), emits a single "(drill-down unavailable on
// this system)" line instead.
//
// Wording chosen deliberately — Option B from the planning
// chat. "Inherit coverage" / "await" frames the trust
// relationship; "resolved on their own" names the indirect's
// own verdict. Avoid prescriptive language ("blocked by",
// "defer-safe"); survey reports state, doesn't recommend action.
//
// Buckets render only when non-zero, so a project where every
// indirect is OwnResolved gets a single line, not three.
func renderIndirectBreakdown(sw *stickyWriter, b survey.IndirectReachabilityBreakdown) {
	if !b.HasData() {
		sw.Writeln("  (drill-down unavailable on this system)")
		return
	}
	if b.OwnResolved > 0 {
		sw.Writef("  %d resolved on their own\n", b.OwnResolved)
	}
	if b.ViaResolved > 0 {
		sw.Writef("  %d inherit coverage from resolved directs\n", b.ViaResolved)
	}
	if b.ViaUnresolved > 0 {
		sw.Writef("  %d await an unresolved direct\n", b.ViaUnresolved)
	}
}

// tierSummaryAnnotation returns a parenthetical clarifier for the
// two "needs attention" tiers, so the user can distinguish the
// two very different states they represent:
//
//   - unexamined: the store HAS an entity and Layer 1 signal data
//     for this dep, but no posture verdict has been recorded. The
//     next move is either /analyze (for an agent-produced verdict)
//     or posture set (for a user-held verdict).
//
//   - not-in-store: the store has NO record at all for this dep.
//     The next move is the same, but runs cold — no cached signals
//     to build on.
//
// All other tiers (vetted-frozen, trusted-for-now, rejected, burned,
// local-replace, unknown-provenance) are resolved states and return
// empty. They don't need clarification in the summary block; the
// per-row direct-deps table carries their rationale / burn reason
// inline.
func tierSummaryAnnotation(t survey.Tier) string {
	switch t {
	case survey.TierUnexamined:
		return "(signal data in store; no posture verdict yet)"
	case survey.TierNotInStore:
		return "(no data collected yet)"
	}
	return ""
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
