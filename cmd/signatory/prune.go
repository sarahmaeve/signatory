package main

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/store"
)

// PruneCmd groups the destructive-cleanup verbs. Each subcommand
// defaults to dry-run: prints the plan, exits without writing.
// `--yes` applies the plan inside a single transaction with the
// append-only triggers temporarily suspended.
//
// Design shape mirrors PostureCmd / CertsCmd: a dispatcher struct
// whose fields are individual subcommand structs. Keeps the kong
// help text flat (`signatory prune entity …`, `signatory prune
// versioned …`) rather than hiding behind a wrapper verb.
//
// Safety:
//
//   - The apply path is all-or-nothing per invocation — a failed
//     trigger reinstall rolls back the entire delete.
//   - Migrations already back up the DB before every schema change;
//     prune does NOT back up independently. If you want a safety
//     snapshot before a bulk prune, run `cp ~/.signatory/signatory.db
//     ~/.signatory/signatory.db.pre-prune` yourself, or run a
//     no-op migration (none exist in v0.1 but future schema bumps
//     will cover this).
type PruneCmd struct {
	Entity    PruneEntityCmd    `cmd:"" help:"Delete one entity (by canonical URI or UUID) plus every child row referencing it."`
	Versioned PruneVersionedCmd `cmd:"" help:"Bulk-delete all entities whose canonical_uri carries a @V version suffix (pre-v10 fragmented rows). Scoped npm packages are NOT matched."`
	Orphans   PruneOrphansCmd   `cmd:"" help:"Delete entities that have no child rows in any direct-child table (no analyses, postures, burns, signals, dependency_observations)."`
}

// --- entity ---------------------------------------------------------------

// PruneEntityCmd deletes a single entity. Takes a target that
// resolveEntity already knows how to parse — canonical URI, GitHub
// shorthand, versioned or not — so the UX matches every other
// target-taking verb (posture, burn, analyze).
type PruneEntityCmd struct {
	Target string `arg:"" help:"Entity to delete. Accepts canonical URIs (pkg:/repo:/identity:/org:/patch:), GitHub shorthand (owner/repo), and entity UUIDs. Versioned URIs look up the UNVERSIONED entity under Plan A — pass the UUID if you want the versioned row specifically."`
	Yes    bool   `help:"Skip the dry-run preview and delete immediately. Required for scripts; interactive invocations should run without --yes first to see the plan."`
}

func (cmd *PruneEntityCmd) Run(globals *Globals) error {
	ctx := context.Background()
	s, err := globals.OpenStore(ctx)
	if err != nil {
		return err
	}
	defer s.Close() //nolint:errcheck // store close on command exit; error is not actionable

	entityID, err := resolvePruneTarget(ctx, s, cmd.Target)
	if err != nil {
		return err
	}

	return runPrune(ctx, s, []string{entityID}, cmd.Yes, fmt.Sprintf("entity %q", cmd.Target))
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
	Yes bool `help:"Skip the dry-run preview and delete immediately."`
}

func (cmd *PruneVersionedCmd) Run(globals *Globals) error {
	ctx := context.Background()
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

	return runPrune(ctx, s, ids, cmd.Yes, fmt.Sprintf("%d versioned entities", len(ids)))
}

// --- orphans --------------------------------------------------------------

// PruneOrphansCmd deletes entities with no child rows in any of
// the direct-child tables. An audit_log row alone is NOT enough
// to keep an entity alive — audit is observation, not a reason to
// exist.
type PruneOrphansCmd struct {
	Yes bool `help:"Skip the dry-run preview and delete immediately."`
}

func (cmd *PruneOrphansCmd) Run(globals *Globals) error {
	ctx := context.Background()
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

	return runPrune(ctx, s, ids, cmd.Yes, fmt.Sprintf("%d orphan entities", len(ids)))
}

// --- shared plan/apply loop ----------------------------------------------

// runPrune computes the plan, prints it, and either exits (dry run)
// or applies (when yes=true). Centralizes the UX so every prune
// subcommand renders the same "here's what would happen" preview.
func runPrune(ctx context.Context, s store.Store, entityIDs []string, yes bool, scopeLabel string) error {
	plan, err := s.PlanPruneEntities(ctx, entityIDs)
	if err != nil {
		return fmt.Errorf("plan prune: %w", err)
	}

	renderPrunePlan(plan, scopeLabel)

	if !yes {
		fmt.Println()
		fmt.Println("Dry-run only. Re-run with --yes to apply.")
		return nil
	}

	fmt.Println()
	fmt.Printf("Applying prune for %s …\n", scopeLabel)
	report, err := s.PruneEntities(ctx, entityIDs)
	if err != nil {
		return fmt.Errorf("apply prune: %w", err)
	}
	renderPruneResult(report)
	return nil
}

// renderPrunePlan prints a human-readable preview. Sorts entity
// rows by canonical URI so the output is stable across runs.
func renderPrunePlan(plan *store.PruneReport, scopeLabel string) {
	fmt.Printf("Prune plan: %s\n", scopeLabel)
	fmt.Println("─────────────────────────────────────")

	// Per-entity listing.
	sort.Slice(plan.Entities, func(i, j int) bool {
		return plan.Entities[i].CanonicalURI < plan.Entities[j].CanonicalURI
	})
	for _, e := range plan.Entities {
		fmt.Printf("  %s  %s\n", shortID(e.ID), e.CanonicalURI)
		// Child counts — sorted for stable output.
		keys := make([]string, 0, len(e.ChildCounts))
		for k := range e.ChildCounts {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Printf("      %s: %d\n", k, e.ChildCounts[k])
		}
		if len(keys) == 0 {
			fmt.Println("      (no child rows)")
		}
	}

	// Aggregate totals.
	fmt.Println("─────────────────────────────────────")
	fmt.Println("Total rows that would be deleted:")
	keys := make([]string, 0, len(plan.RowsByTable))
	for k := range plan.RowsByTable {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("  %-30s %d\n", k, plan.RowsByTable[k])
	}
}

// renderPruneResult prints the actual deletion outcome after the
// apply. Mirrors the plan format so the user can eyeball "plan
// said N, DB reported N" alignment.
func renderPruneResult(report *store.PruneReport) {
	fmt.Println("Prune complete.")
	fmt.Println("Rows deleted:")
	keys := make([]string, 0, len(report.RowsByTable))
	for k := range report.RowsByTable {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("  %-30s %d\n", k, report.RowsByTable[k])
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
