package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/sarahmaeve/signatory/internal/profile"
)

// ConsolidationAction names what `signatory prune duplicates` does
// with one fragmented entity row. Two outcomes:
//
//   - merge: a canonical sibling already exists at the canonical URI.
//     The non-canonical row's child FK references retarget to the
//     sibling, then the non-canonical row is deleted.
//
//   - rename: no canonical sibling exists. The non-canonical row's
//     canonical_uri is updated in place; child FKs unchanged (they
//     already point at the same entity ID).
type ConsolidationAction string

const (
	ConsolidationActionMerge  ConsolidationAction = "merge"
	ConsolidationActionRename ConsolidationAction = "rename"
)

// ConsolidationClass names which fragmentation pattern produced the
// op. Surfaced in plan rendering so the operator can see WHY a row
// is non-canonical.
type ConsolidationClass string

const (
	// ConsolidationClassCaseFold: repo:/identity:/org:/patch: with
	// non-lowercase platform/owner/name segments. Canonical form
	// lowercases per the CanonicalRepoURI / CanonicalIdentityURI etc.
	// constructors.
	ConsolidationClassCaseFold ConsolidationClass = "case-fold"

	// ConsolidationClassEcosystemPrefix: pkg:go/<path> rewrites to
	// pkg:golang/<path>. The "go" identifier was a 2026-04-20 internal
	// coining; "golang" is the [purl spec](https://github.com/package-url/purl-spec)
	// type.
	ConsolidationClassEcosystemPrefix ConsolidationClass = "ecosystem-prefix"

	// ConsolidationClassVersionedEntity: <base>@V entity rows from
	// pre-v10 ingest paths. Plan-A canonicalization stores entities
	// at the unversioned base URI; the version belongs on the
	// posture / analyst_output row, not the entity URI.
	ConsolidationClassVersionedEntity ConsolidationClass = "versioned-entity"
)

// ConsolidationEntity is the minimal entity identification shipped
// in a ConsolidationOp — id and current canonical_uri. Avoids
// pulling the full *profile.Entity through the plan structure when
// only these two fields drive the apply path and the rendering.
type ConsolidationEntity struct {
	ID           string
	CanonicalURI string
}

// ConsolidationOp describes one merge-or-rename action against a
// non-canonical entity row. See ConsolidationAction for the
// dispatch.
type ConsolidationOp struct {
	Action ConsolidationAction
	Class  ConsolidationClass

	// Source is the non-canonical entity that's being consolidated.
	Source ConsolidationEntity

	// CanonicalURI is the URI Source's row should live under post-
	// consolidation. Computed by profile.CanonicalizeURI on
	// Source.CanonicalURI.
	CanonicalURI string

	// CanonicalSibling is the existing entity at CanonicalURI when
	// Action == merge. nil for rename ops.
	CanonicalSibling *ConsolidationEntity

	// ChildCounts is the per-child-table count of FK references that
	// would be retargeted by a merge. Empty for rename ops (the
	// rename leaves child FKs alone). Populated only for the merge
	// path so plan rendering can show "this entity has N analyses
	// and M postures linked through it; they will move to the
	// canonical sibling."
	ChildCounts map[string]int
}

// ConsolidationPlan is the output of ListDuplicateFragmentations and
// the input of ApplyConsolidation. A plan with zero ops means "store
// is already canonical, nothing to do."
type ConsolidationPlan struct {
	Ops []ConsolidationOp
}

// ConsolidationReport is the post-apply summary. RowsByTable counts
// total FK retargets across all merge ops, broken down by the same
// table-column labels PruneReport uses (e.g. "analyst_outputs",
// "analyst_outputs (collected_from)").
type ConsolidationReport struct {
	MergedCount  int
	RenamedCount int
	RowsByTable  map[string]int
}

// ListDuplicateFragmentations scans every entity row, computes its
// canonical form via profile.CanonicalizeURI, and builds a
// ConsolidationPlan listing the rows that aren't yet canonical. For
// each, looks up whether a canonical sibling already exists in the
// store to choose between merge and rename.
//
// Read-only. Safe to call from dry-run UI paths.
//
// Order: ops are returned in canonical_uri sort order so plan
// renderings are stable and tests can assert on specific entries
// without flake from random map iteration.
//
// Implementation note: collects all entity rows into memory in a
// first phase, then runs the per-entity FindEntityByURI / countChildFKs
// queries in a second phase. The two-phase shape avoids a
// SQLite-driver deadlock where nested QueryContext calls on the same
// *DB block on the connection the outer rows reader holds. For the
// dogfood-scale entity counts (tens of rows; thousands at worst), the
// extra slice allocation is trivial.
func (s *SQLite) ListDuplicateFragmentations(ctx context.Context) (*ConsolidationPlan, error) {
	type entityRow struct{ id, uri string }
	var entities []entityRow

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, canonical_uri FROM entities ORDER BY canonical_uri`)
	if err != nil {
		return nil, fmt.Errorf("scan entities: %w", err)
	}
	for rows.Next() {
		var r entityRow
		if scanErr := rows.Scan(&r.id, &r.uri); scanErr != nil {
			rows.Close() //nolint:errcheck
			return nil, fmt.Errorf("scan entity row: %w", scanErr)
		}
		entities = append(entities, r)
	}
	if closeErr := rows.Close(); closeErr != nil {
		return nil, closeErr
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	plan := &ConsolidationPlan{}
	for _, e := range entities {
		canonical := profile.CanonicalizeURI(e.uri)
		if canonical == e.uri {
			continue // already canonical
		}

		op := ConsolidationOp{
			Source:       ConsolidationEntity{ID: e.id, CanonicalURI: e.uri},
			CanonicalURI: canonical,
			Class:        classifyFragmentation(e.uri, canonical),
		}

		// Look up the canonical sibling. If present, the op is a merge;
		// if not, a rename.
		sibling, err := s.FindEntityByURI(ctx, canonical)
		switch {
		case err == nil:
			op.Action = ConsolidationActionMerge
			op.CanonicalSibling = &ConsolidationEntity{ID: sibling.ID, CanonicalURI: sibling.CanonicalURI}
			counts, err := s.countChildFKs(ctx, e.id)
			if err != nil {
				return nil, fmt.Errorf("count child FKs for entity %s: %w", e.id, err)
			}
			op.ChildCounts = counts
		case errors.Is(err, ErrNotFound):
			op.Action = ConsolidationActionRename
		default:
			return nil, fmt.Errorf("lookup canonical sibling for %s: %w", canonical, err)
		}

		plan.Ops = append(plan.Ops, op)
	}
	return plan, nil
}

// classifyFragmentation names which fragmentation rule fired for a
// source URI. Cheap string-shape inspection — no I/O, no parsing
// complexity. Matches the rules in profile.CanonicalizeURI.
//
// Composition note: a single source URI may exhibit MULTIPLE
// fragmentation patterns (e.g. pkg:go/github.com/Owner/Repo@v1 has
// ecosystem-prefix AND case-fold AND versioned-entity all at once).
// We pick the dominant class for display by checking the cheapest
// distinguishing transform first, in source-URI shape order.
//
// canonical is unused — only the SOURCE URI's shape determines which
// fragmentation rule fired. Kept on the signature so callers don't
// have to re-derive context if a future class needs it.
func classifyFragmentation(source, _ string) ConsolidationClass {
	if base, _ := profile.SplitURIVersion(source); base != source {
		return ConsolidationClassVersionedEntity
	}
	if hasGoEcoPrefix(source) {
		return ConsolidationClassEcosystemPrefix
	}
	return ConsolidationClassCaseFold
}

// hasGoEcoPrefix reports whether s starts with "pkg:go/" (the
// internal-coining ecosystem prefix that's been replaced with
// pkg:golang/). Standalone helper so the classifier reads cleanly.
func hasGoEcoPrefix(s string) bool {
	const prefix = "pkg:go/"
	return len(s) > len(prefix) && s[:len(prefix)] == prefix
}

// countChildFKs returns the per-child-table count of FK references
// to entityID. Used by ListDuplicateFragmentations to populate the
// merge op's ChildCounts so the plan renderer can show the operator
// what's about to move. Same per-table label scheme as PruneReport
// uses for consistency in CLI output.
//
// Must cover every table that holds a row referencing entities(id),
// otherwise applyMerge's DELETE FROM entities will fail with
// "FOREIGN KEY constraint failed (787)" — surfaced 2026-04-28
// dogfood when an analysis_session row blocked a BurntSushi/toml
// merge. The set is derived from `pragma_foreign_key_list` over
// every table; audit_log is included even though it lacks a hard FK
// (see applyMerge for the rationale).
func (s *SQLite) countChildFKs(ctx context.Context, entityID string) (map[string]int, error) {
	tables := []struct {
		query string
		label string
	}{
		{`SELECT COUNT(*) FROM analyst_outputs WHERE entity_id = ?`, "analyst_outputs"},
		{`SELECT COUNT(*) FROM analyst_outputs WHERE collected_from_entity_id = ?`, "analyst_outputs (collected_from)"},
		{`SELECT COUNT(*) FROM postures WHERE entity_id = ?`, "postures"},
		{`SELECT COUNT(*) FROM burns WHERE entity_id = ?`, "burns"},
		{`SELECT COUNT(*) FROM signals WHERE entity_id = ?`, "signals"},
		{`SELECT COUNT(*) FROM dependency_observations WHERE project_id = ?`, "dependency_observations"},
		{`SELECT COUNT(*) FROM dependency_observations WHERE entity_id = ?`, "dependency_observations (entity_id)"},
		{`SELECT COUNT(*) FROM analysis_sessions WHERE entity_id = ?`, "analysis_sessions"},
		{`SELECT COUNT(*) FROM signal_resolutions WHERE entity_id = ?`, "signal_resolutions"},
		{`SELECT COUNT(*) FROM audit_log WHERE entity_id = ?`, "audit_log"},
	}
	counts := map[string]int{}
	for _, t := range tables {
		var n int
		if err := s.db.QueryRowContext(ctx, t.query, entityID).Scan(&n); err != nil {
			return nil, err
		}
		if n > 0 {
			counts[t.label] = n
		}
	}
	return counts, nil
}

// ApplyConsolidation executes a plan inside one transaction with
// append-only triggers temporarily suspended. All-or-nothing per
// invocation: a single failed op rolls back the entire batch.
//
// Empty plan is a no-op; safe to call from CLI paths that always
// fetch a plan before applying.
//
// Trigger lifecycle mirrors PruneEntities: capture from sqlite_master
// at the start of the tx, drop, do the data work, reinstall, commit.
// The transaction rollback restores the schema state if anything
// goes wrong, so a partial reinstall can't strand the DB with mutable
// append-only tables.
func (s *SQLite) ApplyConsolidation(ctx context.Context, plan *ConsolidationPlan) (*ConsolidationReport, error) {
	report := &ConsolidationReport{RowsByTable: map[string]int{}}
	if plan == nil || len(plan.Ops) == 0 {
		return report, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin consolidation tx: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	triggers, err := captureAppendOnlyTriggers(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("capture triggers: %w", err)
	}
	if err = dropTriggers(ctx, tx, triggers); err != nil {
		return nil, fmt.Errorf("drop triggers: %w", err)
	}

	for _, op := range plan.Ops {
		switch op.Action {
		case ConsolidationActionMerge:
			if err = applyMerge(ctx, tx, op, report); err != nil {
				return nil, fmt.Errorf("merge %s → %s: %w", op.Source.CanonicalURI, op.CanonicalURI, err)
			}
			report.MergedCount++
		case ConsolidationActionRename:
			if err = applyRename(ctx, tx, op); err != nil {
				return nil, fmt.Errorf("rename %s → %s: %w", op.Source.CanonicalURI, op.CanonicalURI, err)
			}
			report.RenamedCount++
		default:
			err = fmt.Errorf("unknown consolidation action %q on op for %s", op.Action, op.Source.CanonicalURI)
			return nil, err
		}
	}

	if err = reinstallTriggers(ctx, tx, triggers); err != nil {
		return nil, fmt.Errorf("reinstall triggers: %w", err)
	}

	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit consolidation tx: %w", err)
	}
	return report, nil
}

// applyMerge retargets every FK reference from op.Source.ID to
// op.CanonicalSibling.ID across the six child relationships, then
// deletes the source entity row. Self-referential collected_from
// (where the merge would point an analyst_output's
// collected_from_entity_id at its own entity_id) is NULLed instead
// of pointing at the canonical entity — same logical state, but
// avoids self-loop cruft.
//
// Counts go into report.RowsByTable for plan-vs-actual diffing.
func applyMerge(ctx context.Context, tx *sql.Tx, op ConsolidationOp, report *ConsolidationReport) error {
	if op.CanonicalSibling == nil {
		return fmt.Errorf("merge op missing CanonicalSibling for %s", op.Source.CanonicalURI)
	}
	srcID := op.Source.ID
	dstID := op.CanonicalSibling.ID

	// Each retarget: UPDATE child SET col = dstID WHERE col = srcID.
	//
	// The set must cover every FK-bearing column referencing entities(id)
	// (per pragma_foreign_key_list) plus audit_log.entity_id (no hard FK
	// but we retarget for cleanliness — orphan audit entries pointing at
	// a deleted UUID dirty the canonical entity's history). Surfaced
	// 2026-04-28 dogfood: a missing analysis_sessions retarget caused
	// the BurntSushi/toml merge's DELETE to fail with FOREIGN KEY
	// constraint failed (787). The fail-fast + per-op tx rollback meant
	// the operator's earlier-applied ops stayed safe; only the failed
	// op rolled back.
	retargets := []struct {
		query string
		label string
	}{
		{`UPDATE analyst_outputs SET entity_id = ? WHERE entity_id = ?`, "analyst_outputs"},
		{`UPDATE analyst_outputs SET collected_from_entity_id = ? WHERE collected_from_entity_id = ?`, "analyst_outputs (collected_from)"},
		{`UPDATE postures SET entity_id = ? WHERE entity_id = ?`, "postures"},
		{`UPDATE burns SET entity_id = ? WHERE entity_id = ?`, "burns"},
		{`UPDATE signals SET entity_id = ? WHERE entity_id = ?`, "signals"},
		{`UPDATE dependency_observations SET project_id = ? WHERE project_id = ?`, "dependency_observations"},
		{`UPDATE dependency_observations SET entity_id = ? WHERE entity_id = ?`, "dependency_observations (entity_id)"},
		{`UPDATE analysis_sessions SET entity_id = ? WHERE entity_id = ?`, "analysis_sessions"},
		{`UPDATE signal_resolutions SET entity_id = ? WHERE entity_id = ?`, "signal_resolutions"},
		{`UPDATE audit_log SET entity_id = ? WHERE entity_id = ?`, "audit_log"},
	}
	for _, r := range retargets {
		res, err := tx.ExecContext(ctx, r.query, dstID, srcID)
		if err != nil {
			return fmt.Errorf("%s: %w", r.label, err)
		}
		n, _ := res.RowsAffected()
		if n > 0 {
			report.RowsByTable[r.label] += int(n)
		}
	}

	// NULL out self-referential collected_from links the merge created.
	// (analyst_output X had entity_id=src AND collected_from=src; after
	// the two retargets above it'd have entity_id=dst AND collected_from=dst —
	// a self-loop. Restoring NULL keeps the M2 invariant "collected_from
	// is set only when the analysis was performed against a DIFFERENT
	// identity than entity_id.")
	res, err := tx.ExecContext(ctx,
		`UPDATE analyst_outputs SET collected_from_entity_id = NULL
		 WHERE entity_id = collected_from_entity_id`)
	if err != nil {
		return fmt.Errorf("null self-referential collected_from: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		report.RowsByTable["analyst_outputs (collected_from null'd)"] += int(n)
	}

	// Delete the source entity row.
	if _, err := tx.ExecContext(ctx, `DELETE FROM entities WHERE id = ?`, srcID); err != nil {
		return fmt.Errorf("delete source entity: %w", err)
	}
	report.RowsByTable["entities"]++
	return nil
}

// applyRename updates the source entity's canonical_uri in place.
// Child FKs (which key off entity ID, not URI) need no changes —
// they continue to point at the same row.
//
// Doesn't drop append-only triggers; entities table is not append-
// only (PutEntity already mutates it via INSERT OR REPLACE).
func applyRename(ctx context.Context, tx *sql.Tx, op ConsolidationOp) error {
	res, err := tx.ExecContext(ctx,
		`UPDATE entities SET canonical_uri = ? WHERE id = ?`,
		op.CanonicalURI, op.Source.ID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return fmt.Errorf("expected exactly 1 row updated for rename, got %d (entity may have been deleted concurrently)", n)
	}
	return nil
}
