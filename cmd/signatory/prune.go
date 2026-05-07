package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/store"
)

// confirmPrompt is the function the prune subcommands invoke to ask
// the operator "are you really sure?" before applying a destructive
// plan. Package-level variable so tests can swap in a stub that
// returns the desired answer without driving real stdin.
//
// Default reads from os.Stdin / os.Stdout via promptConfirmation. The
// real-stdin path is the only one a non-test caller hits; tests use
// setConfirmPrompt (see prune_test.go) to inject a deterministic
// stub.
var confirmPrompt = func(scopeLabel string) bool {
	return promptConfirmation(os.Stdout, os.Stdin, scopeLabel)
}

// Decision is the per-op answer for `prune duplicates`'s interactive
// walk. Mirrors the y/n/a/q vocabulary git-add-p uses:
//
//	DecisionApply              — apply this op, advance to the next
//	DecisionSkip               — skip this op, advance to the next
//	DecisionApplyAllRemaining  — apply this op AND every remaining op
//	                             without further prompting (operator
//	                             accepts the rest sight-unseen)
//	DecisionQuit               — do not apply this op; stop the walk
//	                             entirely. Earlier-applied ops stay
//	                             applied (they were committed per-op).
type Decision int

const (
	DecisionApply Decision = iota
	DecisionSkip
	DecisionApplyAllRemaining
	DecisionQuit
)

// selectOpPrompt is the per-op decision function for
// `prune duplicates`. Production reads from os.Stdin / os.Stdout via
// promptSelectOp; tests substitute via setSelectOpPrompt.
var selectOpPrompt = func(op store.ConsolidationOp) Decision {
	return promptSelectOp(os.Stdout, os.Stdin, op)
}

// promptSelectOp prints a per-op summary plus a "[y/n/a/q]" prompt,
// reads one line of input from in, and maps the answer to a
// Decision. Recognized variants per Decision:
//
//	y / yes           → DecisionApply
//	n / no            → DecisionSkip
//	a / all           → DecisionApplyAllRemaining
//	q / quit          → DecisionQuit
//
// Anything else — including empty input, EOF, or unrecognized text —
// collapses to DecisionSkip. Fail-safe: the only way to authorize an
// op is to type "y", "yes", or escalate via "a"/"all". Operators who
// hit Enter accidentally, or pipe an empty stdin, get skip-by-
// default (no destructive action).
//
// The prompt always echoes the op's source and canonical URI so the
// operator can verify they're answering about THIS op, not the next
// one. The plan rendering above the loop is the full picture; this
// per-op echo is the local reminder.
func promptSelectOp(out io.Writer, in io.Reader, op store.ConsolidationOp) Decision {
	fmt.Fprintf(out, "  [%s / %s] %s → %s\n", op.Action, op.Class, op.Source.CanonicalURI, op.CanonicalURI)
	fmt.Fprint(out, "  Apply? [y]es / [n]o / [a]ll-remaining / [q]uit: ")

	var input string
	if _, err := fmt.Fscanln(in, &input); err != nil {
		return DecisionSkip
	}
	switch strings.TrimSpace(strings.ToLower(input)) {
	case "y", "yes":
		return DecisionApply
	case "n", "no":
		return DecisionSkip
	case "a", "all":
		return DecisionApplyAllRemaining
	case "q", "quit":
		return DecisionQuit
	default:
		return DecisionSkip
	}
}

// promptConfirmation prints a "[y/N]" prompt naming scopeLabel,
// reads one line of input from in, and returns true only when that
// line is an unambiguous affirmative ("y" or "yes", case-insensitive,
// surrounding whitespace ignored). Anything else — including empty
// input, EOF, or text that starts with 'y' but isn't y/yes — returns
// false.
//
// Fail-safe by design: the only way to authorize a destructive prune
// is to type something explicitly affirmative. A stray Enter, a
// script feeding empty stdin, or a piped close all yield "do not
// proceed." Per design discussion 2026-04-28: scripted use must
// adapt (pipe `yes`); the CLI itself does not provide a non-prompt
// shortcut.
func promptConfirmation(out io.Writer, in io.Reader, scopeLabel string) bool {
	fmt.Fprintf(out, "Proceed with destructive prune for %s? [y/N]: ", scopeLabel)
	var input string
	if _, err := fmt.Fscanln(in, &input); err != nil {
		// EOF, blank line, or scan error all collapse to "do not
		// proceed." Fscanln treats a blank line as an "unexpected
		// newline" error; that's the correct branch here.
		return false
	}
	answer := strings.TrimSpace(strings.ToLower(input))
	return answer == "y" || answer == "yes"
}

// PruneCmd groups the destructive-cleanup verbs. Each subcommand
// defaults to dry-run: prints the plan, exits without writing.
// `--destructive` reveals the plan AND prompts the operator with an
// interactive [y/N] confirmation; the apply path runs only when the
// operator types "y" or "yes." There is no scripted-bypass flag —
// piping `yes` to stdin is the only way to automate; this is a
// deliberate fail-safe choice.
//
// Design shape mirrors PostureCmd / CertsCmd: a dispatcher struct
// whose fields are individual subcommand structs. Keeps the kong
// help text flat (`signatory prune entity …`, `signatory prune
// versioned …`) rather than hiding behind a wrapper verb.
//
// Safety:
//
//   - Two locks before any data mutation: --destructive must be
//     explicitly passed AND the interactive [y/N] must be answered
//     affirmatively. Either omitted, the command exits without
//     writes.
//   - The apply path is all-or-nothing per invocation — a failed
//     trigger reinstall rolls back the entire delete.
//   - Migrations already back up the DB before every schema change;
//     prune does NOT back up independently. If you want a safety
//     snapshot before a bulk prune, run `cp ~/.signatory/signatory.db
//     ~/.signatory/signatory.db.pre-prune` yourself, or run a
//     no-op migration (none exist in v0.1 but future schema bumps
//     will cover this).
type PruneCmd struct {
	Entity     PruneEntityCmd     `cmd:"" help:"Delete one entity (by canonical URI or UUID) plus every child row referencing it."`
	Versioned  PruneVersionedCmd  `cmd:"" help:"Bulk-delete all entities whose canonical_uri carries a @V version suffix (pre-v10 fragmented rows). Scoped npm packages are NOT matched."`
	Orphans    PruneOrphansCmd    `cmd:"" help:"Delete entities that have no child rows in any direct-child table (no analyses, postures, burns, signals, dependency_observations)."`
	Duplicates PruneDuplicatesCmd `cmd:"" help:"Consolidate non-canonical entity rows: case-fold collisions, pkg:go→pkg:golang ecosystem-prefix drift, and pre-v10 @V-suffixed entity rows. Sibling exists → retarget FKs and delete; no sibling → rename in place."`
}

// --- entity ---------------------------------------------------------------

// PruneEntityCmd deletes a single entity. Takes a target that
// resolveEntity already knows how to parse — canonical URI, GitHub
// shorthand, versioned or not — so the UX matches every other
// target-taking verb (posture, burn, analyze).
type PruneEntityCmd struct {
	Target      string `arg:"" help:"Entity to delete. Accepts canonical URIs (pkg:/repo:/identity:/org:/patch:), GitHub shorthand (owner/repo), and entity UUIDs. Versioned URIs look up the UNVERSIONED entity under Plan A — pass the UUID if you want the versioned row specifically."`
	Destructive bool   `help:"Reveal the plan AND prompt for confirmation before applying. Without this flag the command runs in inform-only (dry-run) mode and exits without writes. Even with the flag, the interactive [y/N] prompt must be answered affirmatively."`
}

func (cmd *PruneEntityCmd) Run(globals *Globals) error {
	ctx := globals.Context
	if ctx == nil {
		ctx = context.Background()
	}
	s, err := globals.OpenStore(ctx)
	if err != nil {
		return err
	}
	defer s.Close() //nolint:errcheck // store close on command exit; error is not actionable

	entityID, err := resolvePruneTarget(ctx, s, cmd.Target)
	if err != nil {
		return err
	}

	return runPrune(ctx, s, []string{entityID}, cmd.Destructive, fmt.Sprintf("entity %q", cmd.Target))
}

// resolvePruneTarget turns a user-supplied target into an entity
// UUID. Accepts a literal UUID (if it looks like one), otherwise
// goes through resolveEntity's canonical-URI parser. No
// auto-create fallback — prune requires an existing row.
func resolvePruneTarget(ctx context.Context, s store.Store, target string) (string, error) {
	// Literal UUID: caller already knows the entity ID. Handy for
	// post-mortem cleanup via `signatory prune entity <uuid>` when
	// the canonical URI is ambiguous or the row has no URI-indexable
	// form (e.g. a malformed historical row).
	if looksLikeUUID(target) {
		entity, err := s.GetEntity(ctx, target)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return "", fmt.Errorf("no entity with id %q; run `signatory show-analyses` or inspect the DB to find the right id", target)
			}
			return "", err
		}
		return entity.ID, nil
	}

	// Otherwise route through the standard target parser so
	// `signatory prune entity owner/repo` and `… pkg:npm/lodash`
	// work the same as they do for posture/burn/summary verbs.
	entity, err := resolveEntity(ctx, s, target)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", fmt.Errorf("no entity matches target %q — nothing to prune", target)
		}
		return "", err
	}
	return entity.ID, nil
}

// looksLikeUUID is a crude shape check — 36 chars with 4 hyphens
// in the expected positions. Kong passes the raw string so we can
// route UUIDs vs URIs without importing the uuid package for a
// parse round-trip.
func looksLikeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
}

// --- versioned ------------------------------------------------------------

// PruneVersionedCmd deletes every entity whose canonical_uri
// carries a version suffix (pkg:X@V, repo:X@V). This is the
// post-v10 dogfood-cleanup surface — v10 stopped creating these,
// but pre-v10 data still has them fragmented across versions.
//
// Scoped npm packages (pkg:npm/@types/node) are NOT matched —
// SplitURIVersion anchors on the last path segment, and the scope
// `@` lives in the first segment. The list-path is deliberately
// conservative; if it turns out to miss legitimately-versioned
// rows, that's a data-shape we can inspect via `signatory
// show-analyses` before expanding the scan.
type PruneVersionedCmd struct {
	Destructive bool `help:"Reveal the plan AND prompt for confirmation before applying. Without this flag the command runs in inform-only (dry-run) mode."`
}

func (cmd *PruneVersionedCmd) Run(globals *Globals) error {
	ctx := globals.Context
	if ctx == nil {
		ctx = context.Background()
	}
	s, err := globals.OpenStore(ctx)
	if err != nil {
		return err
	}
	defer s.Close() //nolint:errcheck // store close on command exit; error is not actionable

	ids, err := s.ListVersionedEntities(ctx)
	if err != nil {
		return fmt.Errorf("list versioned entities: %w", err)
	}
	if len(ids) == 0 {
		fmt.Println("No versioned entities to prune. The store is already on Plan-A canonicalization.")
		return nil
	}

	return runPrune(ctx, s, ids, cmd.Destructive, fmt.Sprintf("%d versioned entities", len(ids)))
}

// --- orphans --------------------------------------------------------------

// PruneOrphansCmd deletes entities with no child rows in any of
// the direct-child tables. An audit_log row alone is NOT enough
// to keep an entity alive — audit is observation, not a reason to
// exist.
type PruneOrphansCmd struct {
	Destructive bool `help:"Reveal the plan AND prompt for confirmation before applying. Without this flag the command runs in inform-only (dry-run) mode."`
}

func (cmd *PruneOrphansCmd) Run(globals *Globals) error {
	ctx := globals.Context
	if ctx == nil {
		ctx = context.Background()
	}
	s, err := globals.OpenStore(ctx)
	if err != nil {
		return err
	}
	defer s.Close() //nolint:errcheck // store close on command exit; error is not actionable

	ids, err := s.ListOrphanEntities(ctx)
	if err != nil {
		return fmt.Errorf("list orphan entities: %w", err)
	}
	if len(ids) == 0 {
		fmt.Println("No orphan entities found.")
		return nil
	}

	return runPrune(ctx, s, ids, cmd.Destructive, fmt.Sprintf("%d orphan entities", len(ids)))
}

// --- duplicates -----------------------------------------------------------

// PruneDuplicatesCmd consolidates non-canonical entity rows. Three
// fragmentation classes (per the audit captured 2026-04-28 dogfood):
//
//   - case-fold: repo:/identity:/org:/patch: schemes whose
//     constructors lowercase. A non-lowercase row is non-canonical.
//   - ecosystem-prefix: pkg:go/<path> → pkg:golang/<path> (purl spec).
//   - versioned-entity: <base>@V → <base> (Plan-A canonicalization).
//
// For each detected non-canonical row, the action is either:
//
//   - merge: a canonical sibling already exists. The non-canonical
//     row's child FK references retarget to the sibling, then the
//     non-canonical row is deleted. Self-referential collected_from
//     links the merge would create are NULLed (M2 invariant).
//
//   - rename: no canonical sibling. The non-canonical row's
//     canonical_uri is updated in place; child FKs unchanged.
//
// Same two-lock safety pattern as the other prune verbs:
// --destructive plus an interactive [y/N] confirmation. Without
// either, exits in inform-only (dry-run) mode after printing the plan.
type PruneDuplicatesCmd struct {
	Destructive bool `help:"Reveal the consolidation plan AND prompt for confirmation before applying. Without this flag the command runs in inform-only (dry-run) mode and exits without writes."`
}

func (cmd *PruneDuplicatesCmd) Run(globals *Globals) error {
	ctx := globals.Context
	if ctx == nil {
		ctx = context.Background()
	}
	s, err := globals.OpenStore(ctx)
	if err != nil {
		return err
	}
	defer s.Close() //nolint:errcheck // store close on command exit; error is not actionable

	plan, err := s.ListDuplicateFragmentations(ctx)
	if err != nil {
		return fmt.Errorf("list duplicate fragmentations: %w", err)
	}
	if len(plan.Ops) == 0 {
		fmt.Println("No duplicate URI fragmentations found. The store is canonical.")
		return nil
	}

	renderConsolidationPlan(plan)

	if !cmd.Destructive {
		fmt.Println()
		fmt.Println("Dry-run only. Re-run with --destructive to apply (you'll be prompted per op).")
		return nil
	}

	// Per-op interactive walk. Each op is committed in its own
	// transaction (one ApplyConsolidation call per op), so a Quit
	// after some applies leaves those applied — the operator can
	// stop at any point without rolling back successes.
	//
	// applyAllRemaining short-circuits the prompt once the operator
	// has typed "a"/"all": every subsequent op auto-applies.
	fmt.Println()
	fmt.Println("Per-op selection. y=apply, n=skip, a=apply this and all remaining, q=quit (keeps earlier).")
	fmt.Println()

	aggregate := &store.ConsolidationReport{RowsByTable: map[string]int{}}
	applyAllRemaining := false

walk:
	for _, op := range plan.Ops {
		decision := DecisionApply
		if !applyAllRemaining {
			decision = selectOpPrompt(op)
		}
		switch decision {
		case DecisionSkip:
			fmt.Println("  → skipped")
			fmt.Println()
			continue
		case DecisionQuit:
			fmt.Println("  → quit; earlier-applied ops are kept, nothing further written.")
			fmt.Println()
			break walk
		case DecisionApplyAllRemaining:
			applyAllRemaining = true
			fallthrough
		case DecisionApply:
			// Fall through to the per-op apply below.
		}

		// Apply this single op as its own transaction. ApplyConsolidation
		// handles the trigger ceremony per call; running it once per op
		// is slightly more SQL than batching, but it's the price of
		// per-op commit semantics — a real Quit must not roll back the
		// y-answered ops that came before it.
		opPlan := &store.ConsolidationPlan{Ops: []store.ConsolidationOp{op}}
		report, err := s.ApplyConsolidation(ctx, opPlan)
		if err != nil {
			return fmt.Errorf("apply op %s → %s: %w", op.Source.CanonicalURI, op.CanonicalURI, err)
		}
		aggregate.MergedCount += report.MergedCount
		aggregate.RenamedCount += report.RenamedCount
		for k, v := range report.RowsByTable {
			aggregate.RowsByTable[k] += v
		}
		fmt.Println("  → applied")
		fmt.Println()
	}

	if aggregate.MergedCount == 0 && aggregate.RenamedCount == 0 {
		fmt.Println("No ops applied.")
		return nil
	}
	renderConsolidationResult(aggregate)
	return nil
}

// renderConsolidationPlan prints a human-readable preview of the
// merge/rename ops. Mirrors renderPrunePlan's shape (one entity per
// line, child counts inline) but distinguishes merge from rename
// inline since they're semantically different operations.
func renderConsolidationPlan(plan *store.ConsolidationPlan) {
	fmt.Println("Consolidation plan: duplicate URI fragmentations")
	fmt.Println("─────────────────────────────────────")

	// Sort ops by source canonical_uri for stable output (the listing
	// is already in canonical_uri sort order, but defensive: render
	// always sorts).
	ops := append([]store.ConsolidationOp(nil), plan.Ops...)
	slices.SortFunc(ops, func(a, b store.ConsolidationOp) int {
		return cmp.Compare(a.Source.CanonicalURI, b.Source.CanonicalURI)
	})

	for _, op := range ops {
		switch op.Action {
		case store.ConsolidationActionMerge:
			fmt.Printf("  [merge / %s] %s\n", op.Class, op.Source.CanonicalURI)
			fmt.Printf("      → into %s\n", op.CanonicalURI)
			// Per-child-table FK retarget counts.
			for _, k := range slices.Sorted(maps.Keys(op.ChildCounts)) {
				fmt.Printf("        %s: %d FK(s) retarget\n", k, op.ChildCounts[k])
			}
		case store.ConsolidationActionRename:
			fmt.Printf("  [rename / %s] %s\n", op.Class, op.Source.CanonicalURI)
			fmt.Printf("      → %s\n", op.CanonicalURI)
		}
	}
	fmt.Println("─────────────────────────────────────")
	fmt.Printf("Total: %d op(s)\n", len(ops))
}

// renderConsolidationResult prints the post-apply summary.
func renderConsolidationResult(report *store.ConsolidationReport) {
	fmt.Println("Consolidation complete.")
	fmt.Printf("  merged:  %d\n", report.MergedCount)
	fmt.Printf("  renamed: %d\n", report.RenamedCount)
	if len(report.RowsByTable) > 0 {
		fmt.Println("Rows touched:")
		for _, k := range slices.Sorted(maps.Keys(report.RowsByTable)) {
			fmt.Printf("  %-40s %d\n", k, report.RowsByTable[k])
		}
	}
}

// --- shared plan/apply loop ----------------------------------------------

// runPrune computes the plan, prints it, and routes through the
// two-lock safety gate before any data mutation:
//
//  1. Without `destructive=true`, exits in inform-only (dry-run)
//     mode after printing the plan. Operator sees what would happen
//     and re-runs with --destructive to advance.
//
//  2. With `destructive=true`, prints the plan, prompts the operator
//     for an interactive [y/N] confirmation, and applies only when
//     confirmPrompt returns true. The prompt is the second lock —
//     even an operator who has typed --destructive must affirm
//     after seeing the plan rendered.
//
// Centralizes the UX so every prune subcommand renders the same
// "here's what would happen + are you sure?" flow. The confirmPrompt
// indirection is the testing seam (see setConfirmPrompt in
// prune_test.go).
func runPrune(ctx context.Context, s store.Store, entityIDs []string, destructive bool, scopeLabel string) error {
	plan, err := s.PlanPruneEntities(ctx, entityIDs)
	if err != nil {
		return fmt.Errorf("plan prune: %w", err)
	}

	renderPrunePlan(os.Stdout, plan, scopeLabel)

	if !destructive {
		fmt.Println()
		fmt.Println("Dry-run only. Re-run with --destructive to apply (you'll be prompted to confirm).")
		return nil
	}

	fmt.Println()
	if !confirmPrompt(scopeLabel) {
		fmt.Println("Cancelled — no changes written.")
		return nil
	}

	fmt.Printf("Applying prune for %s …\n", scopeLabel)
	report, err := s.PruneEntities(ctx, entityIDs)
	if err != nil {
		return fmt.Errorf("apply prune: %w", err)
	}
	renderPruneResult(os.Stdout, report)
	return nil
}

// renderPrunePlan prints a human-readable preview to out. Sorts
// entity rows by canonical URI so the output is stable across runs.
//
// The Collateral section appears only when plan.Collateral is non-
// empty — it names entities NOT in the prune scope whose data will
// be touched as a side-effect (typically because an analyst_output's
// collected_from_entity_id points at a pruned entity while the row's
// entity_id points at one that wasn't requested). Surfacing this is
// the operator-safety counterpart to the per-entity listing: dry-run
// must reveal collateral, not just the targeted set.
func renderPrunePlan(out io.Writer, plan *store.PruneReport, scopeLabel string) {
	fmt.Fprintf(out, "Prune plan: %s\n", scopeLabel)
	fmt.Fprintln(out, "─────────────────────────────────────")

	// Per-entity listing.
	slices.SortFunc(plan.Entities, func(a, b store.EntityPruneDetail) int {
		return cmp.Compare(a.CanonicalURI, b.CanonicalURI)
	})
	for _, e := range plan.Entities {
		fmt.Fprintf(out, "  %s  %s\n", shortID(e.ID), e.CanonicalURI)
		// Child counts — sorted for stable output.
		for _, k := range slices.Sorted(maps.Keys(e.ChildCounts)) {
			fmt.Fprintf(out, "      %s: %d\n", k, e.ChildCounts[k])
		}
		if len(e.ChildCounts) == 0 {
			fmt.Fprintln(out, "      (no child rows)")
		}
	}

	// Collateral section: untargeted entities whose data will be
	// touched as a side-effect. Skipped entirely when there's no
	// collateral so the common-case output stays tight.
	if len(plan.Collateral) > 0 {
		fmt.Fprintln(out, "─────────────────────────────────────")
		fmt.Fprintln(out, "Collateral entities affected (not in the prune scope):")
		for _, c := range plan.Collateral {
			fmt.Fprintf(out, "  %s  %s\n", shortID(c.ID), c.CanonicalURI)
			for _, k := range slices.Sorted(maps.Keys(c.AffectedRows)) {
				fmt.Fprintf(out, "      %s: %d\n", k, c.AffectedRows[k])
			}
		}
	}

	// Aggregate totals.
	fmt.Fprintln(out, "─────────────────────────────────────")
	fmt.Fprintln(out, "Total rows that would be deleted:")
	for _, k := range slices.Sorted(maps.Keys(plan.RowsByTable)) {
		fmt.Fprintf(out, "  %-30s %d\n", k, plan.RowsByTable[k])
	}
}

// renderPruneResult prints the actual deletion outcome to out after
// the apply. Mirrors the plan format so the user can eyeball "plan
// said N, DB reported N" alignment.
func renderPruneResult(out io.Writer, report *store.PruneReport) {
	fmt.Fprintln(out, "Prune complete.")
	fmt.Fprintln(out, "Rows deleted:")
	for _, k := range slices.Sorted(maps.Keys(report.RowsByTable)) {
		fmt.Fprintf(out, "  %-30s %d\n", k, report.RowsByTable[k])
	}
}

// shortID returns the first 8 chars of a UUID so `prune list`
// output stays aligned. Callers that need the full id can pass
// `-v` in the future (not implemented in v0.1).
func shortID(id string) string {
	if len(id) < 8 {
		return id
	}
	return id[:8]
}

// Keep the profile package referenced — future prune features
// (prune by tier, prune by ecosystem, etc.) will need target
// resolution helpers. Removing this creates a churn-ish unused-
// import blip on the first of those features.
var _ = profile.ValidateCanonicalURI
