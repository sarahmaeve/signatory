package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/sarahmaeve/signatory/internal/profile"
)

// PruneReport summarizes what a prune operation touched (or would
// touch, for dry-run). Counts are per-table so the human-facing CLI
// can render a useful "this is what will happen" preview.
//
// Counts are typed `int` (not `int64`) deliberately. RowsAffected on
// modernc.org/sqlite returns int64; we cast to int at the boundary.
// On every platform signatory ships to (darwin/linux amd64/arm64)
// `int` is 64-bit so the cast is identity. A 32-bit build deleting
// >2B rows would overflow, but signatory doesn't target 32-bit and
// a single-user trust store with billions of rows is implausible.
// If 32-bit support ever ships, switch these maps to map[string]int64
// and propagate.
type PruneReport struct {
	// Entities names each entity that matched the prune scope.
	Entities []EntityPruneDetail
	// RowsByTable is the aggregate row count across all selected
	// entities, keyed by child table. Includes entities itself.
	RowsByTable map[string]int
	// Collateral names entities NOT in the prune scope whose data
	// will be touched as a side-effect — typically because an
	// analyst_output references them via collected_from_entity_id
	// while its entity_id points at a pruned entity, or vice versa.
	// Pruning entity B deletes such rows even though their entity_id
	// points at A; A is then "collateral" because A's analysis count
	// shrinks without the operator targeting A.
	//
	// Empty in the common case (single-entity prune with no
	// collected_from cross-references).
	Collateral []CollateralEntity
}

// CollateralEntity describes one untargeted entity whose data will
// be touched as a side-effect of pruning the requested set. ID and
// CanonicalURI are populated for CLI display; AffectedRows is keyed
// by table name (matching RowsByTable's vocabulary) and reports how
// many rows will be removed from THIS entity's perspective — not
// the global prune total.
type CollateralEntity struct {
	ID           string
	CanonicalURI string
	ShortName    string
	AffectedRows map[string]int
}

// EntityPruneDetail is the per-entity breakdown. Short_name and
// canonical_uri come along so the CLI can render a readable list
// without a second store round-trip.
type EntityPruneDetail struct {
	ID           string
	CanonicalURI string
	ShortName    string
	ChildCounts  map[string]int
}

// PlanPruneEntities computes what would be deleted if the given
// entity IDs were pruned, without touching the store. Read-only;
// safe to call from dry-run paths. Output is suitable for rendering
// directly or for gating a confirmation prompt.
//
// The returned report has two parts that serve different purposes:
//
//   - Entities []EntityPruneDetail — best-effort per-entity breakdown
//     for the CLI listing. Computed via planOneEntity per id; uses
//     "analyst_outputs (collected_from)" as a separate label from
//     "analyst_outputs" so the operator can see WHICH column drove
//     the count. Per-entity counts may overcount cross-referenced
//     rows when multiple entities are pruned together (one row that
//     references entity A via entity_id and entity B via
//     collected_from gets one count under A and another under B).
//
//   - RowsByTable map[string]int — the parity-checked aggregate.
//     Computed via aggregatePruneCounts in a read-only tx. Uses
//     simple table-name labels matching executePruneDeletes' actual
//     output, so the operator can verify dry-run vs apply by reading
//     this map alone. TestPruneEntities_PlanMatchesReport pins the
//     plan ↔ apply equality for this map.
func (s *SQLite) PlanPruneEntities(ctx context.Context, entityIDs []string) (*PruneReport, error) {
	if len(entityIDs) == 0 {
		return &PruneReport{RowsByTable: map[string]int{}}, nil
	}
	if len(entityIDs) > maxPruneEntityIDs {
		return nil, fmt.Errorf("plan of %d entities exceeds the v0.1 single-batch cap (%d); chunked execution is not yet implemented — split into smaller batches and re-invoke", len(entityIDs), maxPruneEntityIDs)
	}

	report := &PruneReport{
		RowsByTable: map[string]int{},
	}
	for _, id := range entityIDs {
		detail, err := s.planOneEntity(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("plan prune for entity %s: %w", id, err)
		}
		if detail == nil {
			// Skip entities that don't exist — caller may have
			// passed a stale id; we don't want a missing row to
			// abort a bulk plan.
			continue
		}
		report.Entities = append(report.Entities, *detail)
	}

	// Aggregate counts mirror executePruneDeletes' actual cascade so
	// dry-run output matches apply output. This is the source of
	// truth for RowsByTable; the per-entity ChildCounts above are a
	// diagnostic breakdown only.
	aggregate, err := s.aggregatePruneCounts(ctx, entityIDs)
	if err != nil {
		return nil, fmt.Errorf("aggregate plan counts: %w", err)
	}
	report.RowsByTable = aggregate

	// Cross-entity collateral: rows in the cascade reference entities
	// not in entityIDs. The CLI render needs these so the operator
	// knows their prune will silently touch entities they didn't
	// target.
	collateral, err := s.findCollateralEntities(ctx, entityIDs)
	if err != nil {
		return nil, fmt.Errorf("find collateral entities: %w", err)
	}
	report.Collateral = collateral
	return report, nil
}

// findCollateralEntities returns the entities NOT in entityIDs whose
// analyst_outputs get deleted as a side-effect of this prune.
//
// Cascade shape: every analyst_output deleted satisfies entity_id IN
// entityIDs OR collected_from_entity_id IN entityIDs. The OTHER column
// (when not in entityIDs and not NULL) names a collateral entity —
// one whose data shrinks even though the operator didn't target it.
//
// Query strategy: a single round-trip with a UNION-ALL over the two
// directions (X.entity_id collateral-via-collected_from sweep, and
// X.collected_from collateral-via-entity_id sweep), joined through
// entities for human-readable display fields. Sorted by canonical_uri
// for stable test/CLI output.
func (s *SQLite) findCollateralEntities(ctx context.Context, entityIDs []string) ([]CollateralEntity, error) {
	if len(entityIDs) == 0 {
		return nil, nil
	}
	placeholders := inPlaceholders(len(entityIDs))

	// Each branch of the UNION uses entityIDs twice (one IN, one
	// NOT IN); two branches → four copies total.
	q := fmt.Sprintf(`
		SELECT e.id, e.canonical_uri, e.short_name, COUNT(*) AS n
		  FROM (
		      SELECT entity_id AS collat_id
		        FROM analyst_outputs
		       WHERE collected_from_entity_id IN %[1]s
		         AND entity_id NOT IN %[1]s
		      UNION ALL
		      SELECT collected_from_entity_id AS collat_id
		        FROM analyst_outputs
		       WHERE entity_id IN %[1]s
		         AND collected_from_entity_id IS NOT NULL
		         AND collected_from_entity_id NOT IN %[1]s
		  ) c
		  JOIN entities e ON e.id = c.collat_id
		 GROUP BY e.id, e.canonical_uri, e.short_name
		 ORDER BY e.canonical_uri`, placeholders) //nolint:gosec // G201: placeholders is generated from an integer count, not user input; values bind via QueryContext args below

	args := make([]any, 0, 4*len(entityIDs))
	for range 4 {
		args = append(args, toAnyArgs(entityIDs)...)
	}

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query collateral entities: %w", err)
	}
	defer rows.Close() //nolint:errcheck // rows iteration complete; rows.Err() captures read-side errors below

	var out []CollateralEntity
	for rows.Next() {
		var c CollateralEntity
		var n int
		if err := rows.Scan(&c.ID, &c.CanonicalURI, &c.ShortName, &n); err != nil {
			return nil, fmt.Errorf("scan collateral row: %w", err)
		}
		c.AffectedRows = map[string]int{"analyst_outputs": n}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// aggregatePruneCounts mirrors executePruneDeletes' walk in count
// mode, returning a per-table row-count map keyed by the same labels
// executePruneDeletes uses. plan.RowsByTable equality with
// report.RowsByTable depends on this function and executePruneDeletes
// touching the same set of tables with the same labels.
//
// Adding a new table to executePruneDeletes REQUIRES adding the same
// table here. TestPruneEntities_PlanMatchesReport catches the
// divergence at test time.
//
// Implementation: collects intermediate ID lists (output, conclusion,
// absence, observation, signal, pattern) inside one read-only tx, then
// runs COUNT(*) per table using those ID sieves. The ID-collection
// phase mirrors executePruneDeletes' shape exactly so additions stay
// in lockstep. The read-only tx provides snapshot semantics — without
// it, a concurrent INSERT mid-walk could land between two of our
// counts and the aggregate would no longer be self-consistent.
func (s *SQLite) aggregatePruneCounts(ctx context.Context, entityIDs []string) (map[string]int, error) {
	counts := map[string]int{}
	if len(entityIDs) == 0 {
		return counts, nil
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin read-only tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // read-only tx; rollback releases the snapshot, no error to act on

	// --- Step 1: collect ID sieves (mirrors executePruneDeletes lines ~345-373).
	outputIDs, err := collectIDs(ctx, tx,
		`SELECT id FROM analyst_outputs WHERE entity_id IN `+inPlaceholders(len(entityIDs))+
			` OR collected_from_entity_id IN `+inPlaceholders(len(entityIDs)),
		doubleArgs(entityIDs)...)
	if err != nil {
		return nil, fmt.Errorf("collect output ids: %w", err)
	}

	var conclusionIDs, absenceIDs, observationIDs, patternIDs []string
	if len(outputIDs) > 0 {
		conclusionIDs, err = collectIDs(ctx, tx,
			`SELECT id FROM conclusions WHERE output_id IN `+inPlaceholders(len(outputIDs)),
			toAnyArgs(outputIDs)...)
		if err != nil {
			return nil, fmt.Errorf("collect conclusion ids: %w", err)
		}
		absenceIDs, err = collectIDs(ctx, tx,
			`SELECT id FROM positive_absences WHERE output_id IN `+inPlaceholders(len(outputIDs)),
			toAnyArgs(outputIDs)...)
		if err != nil {
			return nil, fmt.Errorf("collect absence ids: %w", err)
		}
		observationIDs, err = collectIDs(ctx, tx,
			`SELECT id FROM observations WHERE output_id IN `+inPlaceholders(len(outputIDs)),
			toAnyArgs(outputIDs)...)
		if err != nil {
			return nil, fmt.Errorf("collect observation ids: %w", err)
		}
		patternIDs, err = collectIDs(ctx, tx,
			`SELECT id FROM methodology_patterns WHERE output_id IN `+inPlaceholders(len(outputIDs)),
			toAnyArgs(outputIDs)...)
		if err != nil {
			return nil, fmt.Errorf("collect pattern ids: %w", err)
		}
	}

	signalIDs, err := collectIDs(ctx, tx,
		`SELECT id FROM signals WHERE entity_id IN `+inPlaceholders(len(entityIDs)),
		toAnyArgs(entityIDs)...)
	if err != nil {
		return nil, fmt.Errorf("collect signal ids: %w", err)
	}

	// --- Step 2: COUNT each table executePruneDeletes deletes from.

	// Level 5: conclusion children (mirrors lines ~377-392).
	for _, sp := range []struct{ table, column string }{
		{"conclusion_severity_contexts", "conclusion_id"},
		{"conclusion_supersedes", "conclusion_id"},
		{"conclusion_prerequisites", "conclusion_id"},
		{"conclusion_remediation_hints", "conclusion_id"},
		{"conclusion_related", "conclusion_id"},
	} {
		n, err := countByIDs(ctx, tx, sp.table, sp.column, conclusionIDs)
		if err != nil {
			return nil, err
		}
		if n > 0 {
			counts[sp.table] = n
		}
	}

	// Citations — three passes per kind, summed under "citations"
	// (mirrors lines ~394-416).
	citationCount := 0
	for _, kc := range []struct {
		kind string
		ids  []string
	}{
		{"conclusion", conclusionIDs},
		{"positive_absence", absenceIDs},
		{"observation", observationIDs},
	} {
		if len(kc.ids) == 0 {
			continue
		}
		var n int
		q := `SELECT COUNT(*) FROM citations WHERE parent_kind = ? AND parent_id IN ` + inPlaceholders(len(kc.ids))
		args := append([]any{kc.kind}, toAnyArgs(kc.ids)...)
		if err := tx.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
			return nil, fmt.Errorf("count citations/%s: %w", kc.kind, err)
		}
		citationCount += n
	}
	if citationCount > 0 {
		counts["citations"] = citationCount
	}

	// Methodology pattern chain (mirrors lines ~422-438).
	if n, err := countByIDs(ctx, tx, "methodology_pattern_composes", "pattern_id", patternIDs); err != nil {
		return nil, err
	} else if n > 0 {
		counts["methodology_pattern_composes"] = n
	}
	if n, err := countByIDs(ctx, tx, "methodology_patterns", "output_id", outputIDs); err != nil {
		return nil, err
	} else if n > 0 {
		counts["methodology_patterns"] = n
	}

	// Level 4: output children (mirrors lines ~441-458).
	for _, table := range []string{
		"conclusions", "positive_absences", "observations",
		"methodology_catalogs", "output_supersedes", "output_reframes_from",
	} {
		n, err := countByIDs(ctx, tx, table, "output_id", outputIDs)
		if err != nil {
			return nil, err
		}
		if n > 0 {
			counts[table] = n
		}
	}

	// Level 3: signals' children (mirrors lines ~460-480).
	if n, err := countByIDs(ctx, tx, "signal_evidence", "signal_id", signalIDs); err != nil {
		return nil, err
	} else if n > 0 {
		counts["signal_evidence"] = n
	}

	// signal_resolutions: union of the signal_id sweep AND the entity_id
	// sweep. Apply does these as two sequential DELETEs that sum their
	// counts; a single SELECT with OR gets the same count (rows that
	// match both clauses contribute once, matching the second DELETE
	// finding zero rows after the first removed them).
	sigResCount, err := countSignalResolutions(ctx, tx, signalIDs, entityIDs)
	if err != nil {
		return nil, err
	}
	if sigResCount > 0 {
		counts["signal_resolutions"] = sigResCount
	}

	// Level 2: direct entity children (mirrors lines ~493-511).
	//
	// analyst_outputs is counted via OR across both columns: apply does
	// two DELETEs (entity_id then collected_from_entity_id) that sum,
	// but the second finds zero rows after the first removed them, so
	// SELECT COUNT WHERE entity_id IN ... OR collected_from_entity_id
	// IN ... gives the same total without double-counting cross-refs.
	var analystOutputsCount int
	{
		q := `SELECT COUNT(*) FROM analyst_outputs WHERE entity_id IN ` + inPlaceholders(len(entityIDs)) +
			` OR collected_from_entity_id IN ` + inPlaceholders(len(entityIDs))
		if err := tx.QueryRowContext(ctx, q, doubleArgs(entityIDs)...).Scan(&analystOutputsCount); err != nil {
			return nil, fmt.Errorf("count analyst_outputs: %w", err)
		}
	}
	if analystOutputsCount > 0 {
		counts["analyst_outputs"] = analystOutputsCount
	}

	for _, sp := range []struct{ table, column string }{
		{"postures", "entity_id"},
		{"burns", "entity_id"},
		{"signals", "entity_id"},
		{"dependency_observations", "project_id"},
		{"audit_log", "entity_id"},
	} {
		n, err := countByIDs(ctx, tx, sp.table, sp.column, entityIDs)
		if err != nil {
			return nil, err
		}
		if n > 0 {
			counts[sp.table] = n
		}
	}

	// Level 1: entities themselves.
	if n, err := countByIDs(ctx, tx, "entities", "id", entityIDs); err != nil {
		return nil, err
	} else if n > 0 {
		counts["entities"] = n
	}

	return counts, nil
}

// countByIDs runs SELECT COUNT(*) FROM <table> WHERE <column> IN (?...).
// Returns 0 with no error on empty id list, matching deleteByIDs'
// no-op-on-empty contract.
func countByIDs(ctx context.Context, tx *sql.Tx, table, column string, ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	q := fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE %s IN %s`, table, column, inPlaceholders(len(ids))) //nolint:gosec // G201: table/column names are package constants, not user input
	var n int
	if err := tx.QueryRowContext(ctx, q, toAnyArgs(ids)...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count %s: %w", table, err)
	}
	return n, nil
}

// countSignalResolutions counts the union of two sweeps apply
// performs (signal_id IN signalIDs OR entity_id IN entityIDs).
// Either list may be empty; the function adapts the WHERE clause
// so a no-signal entity prune still counts entity_id-keyed
// resolutions.
func countSignalResolutions(ctx context.Context, tx *sql.Tx, signalIDs, entityIDs []string) (int, error) {
	switch {
	case len(signalIDs) == 0 && len(entityIDs) == 0:
		return 0, nil
	case len(signalIDs) == 0:
		return countByIDs(ctx, tx, "signal_resolutions", "entity_id", entityIDs)
	case len(entityIDs) == 0:
		return countByIDs(ctx, tx, "signal_resolutions", "signal_id", signalIDs)
	}
	q := `SELECT COUNT(*) FROM signal_resolutions WHERE signal_id IN ` + inPlaceholders(len(signalIDs)) +
		` OR entity_id IN ` + inPlaceholders(len(entityIDs))
	args := append(toAnyArgs(signalIDs), toAnyArgs(entityIDs)...)
	var n int
	if err := tx.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count signal_resolutions: %w", err)
	}
	return n, nil
}

// planOneEntity collects row counts for one entity's children.
// Returns nil if the entity doesn't exist so bulk plans tolerate
// stale input.
func (s *SQLite) planOneEntity(ctx context.Context, entityID string) (*EntityPruneDetail, error) {
	var canonicalURI, shortName string
	err := s.db.QueryRowContext(ctx,
		`SELECT canonical_uri, short_name FROM entities WHERE id = ?`,
		entityID).Scan(&canonicalURI, &shortName)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}

	detail := &EntityPruneDetail{
		ID:           entityID,
		CanonicalURI: canonicalURI,
		ShortName:    shortName,
		ChildCounts:  map[string]int{},
	}

	// Direct-child tables keyed by entity_id.
	directChildren := []struct {
		table   string
		column  string
		counter string
	}{
		{"analyst_outputs", "entity_id", "analyst_outputs"},
		{"analyst_outputs", "collected_from_entity_id", "analyst_outputs (collected_from)"},
		{"postures", "entity_id", "postures"},
		{"burns", "entity_id", "burns"},
		{"signals", "entity_id", "signals"},
		{"dependency_observations", "project_id", "dependency_observations"},
		{"audit_log", "entity_id", "audit_log"},
	}
	for _, c := range directChildren {
		var n int
		q := fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE %s = ?`, c.table, c.column) //nolint:gosec // G201: table/column names are package constants, not user input
		if err := s.db.QueryRowContext(ctx, q, entityID).Scan(&n); err != nil {
			return nil, fmt.Errorf("count %s.%s: %w", c.table, c.column, err)
		}
		if n > 0 {
			detail.ChildCounts[c.counter] += n
		}
	}

	// Transitive children (via analyst_outputs): conclusions,
	// positive_absences, observations, citations, etc. We count
	// them here so the dry-run preview is accurate, but the actual
	// DELETE cascade is driven by output IDs, not entity IDs.
	var outputIDs []string
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM analyst_outputs WHERE entity_id = ? OR collected_from_entity_id = ?`,
		entityID, entityID)
	if err != nil {
		return nil, fmt.Errorf("list output ids: %w", err)
	}
	defer rows.Close() //nolint:errcheck // rows iteration complete; rows.Err() captures read-side errors below
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		outputIDs = append(outputIDs, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if len(outputIDs) > 0 {
		// Counts for child tables keyed on output_id.
		transitiveChildren := []string{
			"conclusions", "positive_absences", "observations",
			"methodology_catalogs", "output_supersedes",
			"output_reframes_from",
		}
		for _, table := range transitiveChildren {
			n, err := countForOutputs(ctx, s.db, table, "output_id", outputIDs)
			if err != nil {
				return nil, err
			}
			if n > 0 {
				detail.ChildCounts[table] += n
			}
		}
	}

	// signal_evidence is a child of signals (not analyst_outputs),
	// so count it via the signal ids for this entity. Empty in the
	// common case — signals come from collectors, not ingest.
	var sigEvCount int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM signal_evidence WHERE signal_id IN
		   (SELECT id FROM signals WHERE entity_id = ?)`, entityID).
		Scan(&sigEvCount); err != nil {
		return nil, fmt.Errorf("count signal_evidence: %w", err)
	}
	if sigEvCount > 0 {
		detail.ChildCounts["signal_evidence"] += sigEvCount
	}

	return detail, nil
}

// countForOutputs does a parameterized COUNT across the given
// output-id set. Splits into manageable batches if the list grows
// large; for v0.1 scale the single-batch path is enough.
func countForOutputs(ctx context.Context, db *sql.DB, table, column string, outputIDs []string) (int, error) {
	if len(outputIDs) == 0 {
		return 0, nil
	}
	placeholders := strings.Repeat("?,", len(outputIDs))
	placeholders = placeholders[:len(placeholders)-1]                                         // trim trailing comma
	q := fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE %s IN (%s)`, table, column, placeholders) //nolint:gosec // G201: table/column names are package constants
	args := make([]any, len(outputIDs))
	for i, id := range outputIDs {
		args[i] = id
	}
	var n int
	if err := db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count %s: %w", table, err)
	}
	return n, nil
}

// PruneEntities deletes the named entity rows and every child
// record that references them. Returns a report of what was
// actually deleted. Wraps the deletion in a single transaction
// with append-only triggers temporarily suspended — triggers are
// saved verbatim from sqlite_master, dropped, and reinstalled
// after the deletes so any post-prune INSERT still runs through
// their checks.
//
// Safety model:
//
//   - Caller is responsible for confirmation / backup. Every trust-
//     decision path that lands here came through an explicit
//     operator action (e.g. `signatory prune … --yes`).
//
//   - Transaction scope: trigger-drop, deletes, and trigger-reinstall
//     all live in one tx. A crash or context cancel rolls back the
//     entire operation; triggers come back because the transaction
//     rollback restores the schema state from before DROP TRIGGER.
//
//   - Empty input list: early return with zero counts. Keeps caller
//     code simple ("prune the empty set of stale entities" is a no-
//     op, not an error).
func (s *SQLite) PruneEntities(ctx context.Context, entityIDs []string) (*PruneReport, error) {
	if len(entityIDs) == 0 {
		return &PruneReport{RowsByTable: map[string]int{}}, nil
	}
	if len(entityIDs) > maxPruneEntityIDs {
		return nil, fmt.Errorf("prune of %d entities exceeds the v0.1 single-batch cap (%d); chunked execution is not yet implemented — split into smaller batches and re-invoke", len(entityIDs), maxPruneEntityIDs)
	}

	// Guard the trigger-drop window: see requireSingleConnection's
	// docstring for why this is load-bearing and not just defensive.
	if err := s.requireSingleConnection(); err != nil {
		return nil, err
	}

	// Capture the per-entity detail BEFORE the delete so the report
	// carries meaningful canonical_uri / short_name fields. The plan
	// also feeds the entity listing the CLI renders. Plan ↔ apply
	// count parity is enforced by TestPruneEntities_PlanMatchesReport
	// against aggregatePruneCounts (used to populate plan.RowsByTable);
	// a divergence at runtime would mean one of those two walks fell
	// out of sync with executePruneDeletes' table set.
	plan, err := s.PlanPruneEntities(ctx, entityIDs)
	if err != nil {
		return nil, fmt.Errorf("plan prune: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin prune tx: %w", err)
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

	actualCounts, err := executePruneDeletes(ctx, tx, entityIDs)
	if err != nil {
		return nil, fmt.Errorf("execute deletes: %w", err)
	}

	if err = reinstallTriggers(ctx, tx, triggers); err != nil {
		return nil, fmt.Errorf("reinstall triggers: %w", err)
	}

	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit prune tx: %w", err)
	}

	// Prefer actual-counts for the returned report — they reflect
	// what the DB actually did, not what the plan predicted.
	// Fall back to plan's per-entity detail for canonical_uri /
	// short_name since those don't change between plan and execute.
	report := &PruneReport{
		Entities:    plan.Entities,
		RowsByTable: actualCounts,
	}
	return report, nil
}

// --- internal helpers ------------------------------------------------------

// maxPruneEntityIDs caps the entityIDs slice each PruneEntities (and
// PlanPruneEntities, ApplyConsolidation) call accepts. The cap exists
// because several queries in the cascade — particularly the OR-on-
// both-columns sieves in aggregatePruneCounts/executePruneDeletes
// (×2) and the UNION in findCollateralEntities (×4) — expand
// entityIDs into multiple bind-variable copies. SQLite's
// SQLITE_MAX_VARIABLE_NUMBER caps total bind variables per statement;
// modernc.org/sqlite's effective limit is currently large (~32k) but
// the SQLite default is 999, so we cap conservatively to keep
// signatory portable across SQLite builds.
//
// 250 entities × 4 (worst case) = 1000 bind variables. Comfortably
// under both the SQLite default and modernc's compiled limit.
//
// v0.2 should replace this cap with proper chunked execution
// (batched IN-lists across multiple statements, all inside one tx
// to preserve all-or-nothing semantics). The current cap forces an
// honest error rather than a cryptic "too many SQL variables" from
// deep inside executePruneDeletes — but it forces operators to chunk
// manually, which is friction.
const maxPruneEntityIDs = 250

// requireSingleConnection guards trigger-drop windows: PruneEntities
// and ApplyConsolidation both temporarily drop append-only triggers
// inside their tx. The invariant the triggers enforce (analyst_outputs
// is append-only, conclusions is append-only, etc.) is suspended
// during that window. With a multi-connection pool, a writer on a
// parallel connection could land an INSERT/UPDATE that bypasses the
// suspended trigger and silently corrupt the invariant.
//
// OpenSQLite sets the pool to size 1 (sqlite.go around line 126).
// This guard re-checks at function entry so a future change that
// loosens the pool surfaces here as a clear error rather than as
// downstream data corruption.
func (s *SQLite) requireSingleConnection() error {
	if max := s.db.Stats().MaxOpenConnections; max != 1 {
		return fmt.Errorf("destructive prune requires single-connection pool (MaxOpenConnections=%d); see SetMaxOpenConns in OpenSQLite — the trigger-drop window is data-integrity-safe only when no concurrent writers can bypass the suspended append-only triggers", max)
	}
	return nil
}

// appendOnlyTrigger captures the name + SQL of a trigger so we can
// drop it and reinstall it later. The SQL comes straight from
// sqlite_master, which stores each trigger's original CREATE
// statement verbatim.
type appendOnlyTrigger struct {
	Name string
	SQL  string
}

// captureAppendOnlyTriggers returns every trigger whose name ends
// in _no_update or _no_delete — the convention signatory uses for
// append-only enforcement. Scoped narrowly so we don't suspend
// unrelated triggers that may be added later for other purposes.
func captureAppendOnlyTriggers(ctx context.Context, tx *sql.Tx) ([]appendOnlyTrigger, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT name, sql FROM sqlite_master
		  WHERE type = 'trigger'
		    AND (name LIKE '%_no_update' OR name LIKE '%_no_delete')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // rows iteration complete; rows.Err() captures read-side errors below
	var triggers []appendOnlyTrigger
	for rows.Next() {
		var t appendOnlyTrigger
		if err := rows.Scan(&t.Name, &t.SQL); err != nil {
			return nil, err
		}
		triggers = append(triggers, t)
	}
	return triggers, rows.Err()
}

func dropTriggers(ctx context.Context, tx *sql.Tx, triggers []appendOnlyTrigger) error {
	for _, t := range triggers {
		if _, err := tx.ExecContext(ctx, `DROP TRIGGER IF EXISTS `+t.Name); err != nil { //nolint:gosec // G202: trigger names come from sqlite_master, not user input
			return fmt.Errorf("drop trigger %s: %w", t.Name, err)
		}
	}
	return nil
}

func reinstallTriggers(ctx context.Context, tx *sql.Tx, triggers []appendOnlyTrigger) error {
	for _, t := range triggers {
		if t.SQL == "" {
			// Defensive: an internal trigger with empty .sql would
			// be lost here. Skip rather than fail — the original
			// protection is gone either way, and we'd rather
			// complete the prune than leave the DB half-mutated.
			continue
		}
		if _, err := tx.ExecContext(ctx, t.SQL); err != nil { //nolint:gosec // G202: CREATE TRIGGER text captured from sqlite_master
			return fmt.Errorf("reinstall trigger %s: %w", t.Name, err)
		}
	}
	return nil
}

// executePruneDeletes runs the full cascade for a set of entity
// IDs. Order matters: deepest children first, then working up
// toward the entity row. FK constraints enforce this ordering at
// commit time, but following it in the statements themselves keeps
// error messages pointed at the true cause.
//
// Returns the actual per-table row counts observed during the
// deletes.
func executePruneDeletes(ctx context.Context, tx *sql.Tx, entityIDs []string) (map[string]int, error) {
	counts := map[string]int{}

	// Collect output_ids + their conclusion / absence / observation /
	// methodology-catalog child IDs so the deeply-nested citations
	// and conclusion_* tables can be pruned cleanly.
	outputIDs, err := collectIDs(ctx, tx,
		`SELECT id FROM analyst_outputs WHERE entity_id IN `+inPlaceholders(len(entityIDs))+
			` OR collected_from_entity_id IN `+inPlaceholders(len(entityIDs)),
		doubleArgs(entityIDs)...)
	if err != nil {
		return nil, fmt.Errorf("collect output ids: %w", err)
	}

	var conclusionIDs, absenceIDs, observationIDs []string
	if len(outputIDs) > 0 {
		conclusionIDs, err = collectIDs(ctx, tx,
			`SELECT id FROM conclusions WHERE output_id IN `+inPlaceholders(len(outputIDs)),
			toAnyArgs(outputIDs)...)
		if err != nil {
			return nil, err
		}
		absenceIDs, err = collectIDs(ctx, tx,
			`SELECT id FROM positive_absences WHERE output_id IN `+inPlaceholders(len(outputIDs)),
			toAnyArgs(outputIDs)...)
		if err != nil {
			return nil, err
		}
		observationIDs, err = collectIDs(ctx, tx,
			`SELECT id FROM observations WHERE output_id IN `+inPlaceholders(len(outputIDs)),
			toAnyArgs(outputIDs)...)
		if err != nil {
			return nil, err
		}
	}

	// Level 5: children of conclusions / absences / observations /
	// methodology patterns.
	for _, spec := range []struct {
		table, column string
		ids           []string
	}{
		{"conclusion_severity_contexts", "conclusion_id", conclusionIDs},
		{"conclusion_supersedes", "conclusion_id", conclusionIDs},
		{"conclusion_prerequisites", "conclusion_id", conclusionIDs},
		{"conclusion_remediation_hints", "conclusion_id", conclusionIDs},
		{"conclusion_related", "conclusion_id", conclusionIDs},
	} {
		if n, err := deleteByIDs(ctx, tx, spec.table, spec.column, spec.ids); err != nil {
			return nil, err
		} else if n > 0 {
			counts[spec.table] = n
		}
	}

	// Citations (parented by kind + id — three passes, one per kind).
	for _, spec := range []struct {
		kind string
		ids  []string
	}{
		{"conclusion", conclusionIDs},
		{"positive_absence", absenceIDs},
		{"observation", observationIDs},
	} {
		if len(spec.ids) == 0 {
			continue
		}
		placeholders := inPlaceholders(len(spec.ids))
		//nolint:gosec // G202: placeholders is a generated (?,?,?) string from an integer count, not user input; actual values bind via ExecContext args below
		q := `DELETE FROM citations WHERE parent_kind = ? AND parent_id IN ` + placeholders
		args := append([]any{spec.kind}, toAnyArgs(spec.ids)...)
		r, err := tx.ExecContext(ctx, q, args...)
		if err != nil {
			return nil, fmt.Errorf("delete citations/%s: %w", spec.kind, err)
		}
		n, err := r.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("rows affected after deleting citations/%s: %w", spec.kind, err)
		}
		counts["citations"] += int(n)
	}

	// Methodology chain: patterns reference analyst_outputs directly
	// via output_id (NOT via an intermediate catalog id); catalogs
	// use output_id itself as their PK. So both chain to output_id
	// without a middle-layer id scan.
	if len(outputIDs) > 0 {
		patternIDs, err := collectIDs(ctx, tx,
			`SELECT id FROM methodology_patterns WHERE output_id IN `+inPlaceholders(len(outputIDs)),
			toAnyArgs(outputIDs)...)
		if err != nil {
			return nil, err
		}
		if n, err := deleteByIDs(ctx, tx, "methodology_pattern_composes", "pattern_id", patternIDs); err != nil {
			return nil, err
		} else if n > 0 {
			counts["methodology_pattern_composes"] = n
		}
		if n, err := deleteByIDs(ctx, tx, "methodology_patterns", "output_id", outputIDs); err != nil {
			return nil, err
		} else if n > 0 {
			counts["methodology_patterns"] = n
		}
	}

	// Level 4: children of analyst_outputs.
	for _, spec := range []struct {
		table string
		ids   []string
	}{
		{"conclusions", outputIDs},
		{"positive_absences", outputIDs},
		{"observations", outputIDs},
		{"methodology_catalogs", outputIDs},
		{"output_supersedes", outputIDs},
		{"output_reframes_from", outputIDs},
	} {
		if n, err := deleteByIDs(ctx, tx, spec.table, "output_id", spec.ids); err != nil {
			return nil, err
		} else if n > 0 {
			counts[spec.table] = n
		}
	}

	// Level 3: signals' children (signal_evidence keyed on signal_id,
	// signal_resolutions keyed on signal_id) before the signals
	// themselves.
	if len(entityIDs) > 0 {
		signalIDs, err := collectIDs(ctx, tx,
			`SELECT id FROM signals WHERE entity_id IN `+inPlaceholders(len(entityIDs)),
			toAnyArgs(entityIDs)...)
		if err != nil {
			return nil, err
		}
		if n, err := deleteByIDs(ctx, tx, "signal_evidence", "signal_id", signalIDs); err != nil {
			return nil, err
		} else if n > 0 {
			counts["signal_evidence"] = n
		}
		if n, err := deleteByIDs(ctx, tx, "signal_resolutions", "signal_id", signalIDs); err != nil {
			return nil, err
		} else if n > 0 {
			counts["signal_resolutions"] = n
		}
	}

	// Level 2: direct children of entities.
	//
	// signal_resolutions gains an entity_id sweep from v12 (orphan-
	// prevention audit; design/orphanage.md). Without it, pruning
	// entity X would leave signal_resolutions rows whose entity_id=X
	// but whose signal_id doesn't belong to X's signals (possible
	// under the cross-entity-consistency gap documented by
	// sqlite_security_test.go), and the subsequent DELETE FROM
	// entities would fail the new FK constraint. The Level-3 sweep
	// above already removes rows by signal_id — this sweep is the
	// belt-and-suspenders pass catching the cross-entity case.
	directChildren := []struct{ table, column string }{
		{"analyst_outputs", "entity_id"},
		{"analyst_outputs", "collected_from_entity_id"},
		{"postures", "entity_id"},
		{"burns", "entity_id"},
		{"signals", "entity_id"},
		{"signal_resolutions", "entity_id"},
		{"dependency_observations", "project_id"},
		{"audit_log", "entity_id"},
	}
	for _, c := range directChildren {
		if n, err := deleteByIDs(ctx, tx, c.table, c.column, entityIDs); err != nil {
			return nil, err
		} else if n > 0 {
			// Key by table; the "analyst_outputs" pass happens twice
			// (entity_id + collected_from), we sum them.
			counts[c.table] += n
		}
	}

	// Level 1: entities themselves.
	if n, err := deleteByIDs(ctx, tx, "entities", "id", entityIDs); err != nil {
		return nil, err
	} else if n > 0 {
		counts["entities"] = n
	}

	return counts, nil
}

// deleteByIDs runs `DELETE FROM <table> WHERE <column> IN (?...)`
// and returns the row count. No-op on empty id list.
func deleteByIDs(ctx context.Context, tx *sql.Tx, table, column string, ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	q := fmt.Sprintf(`DELETE FROM %s WHERE %s IN %s`, table, column, inPlaceholders(len(ids))) //nolint:gosec // G201: table/column names are package constants, not user input
	r, err := tx.ExecContext(ctx, q, toAnyArgs(ids)...)
	if err != nil {
		return 0, fmt.Errorf("delete %s: %w", table, err)
	}
	n, err := r.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected after deleting %s: %w", table, err)
	}
	return int(n), nil
}

// collectIDs runs a SELECT that returns one TEXT column per row
// (IDs) and gathers them into a slice.
func collectIDs(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // rows iteration complete; rows.Err() captures read-side errors below
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// inPlaceholders returns "(?, ?, ..., ?)" for the given count.
// Callers pass this straight into WHERE ... IN clauses.
//
// Panics on n <= 0. SQL `IN ()` is invalid and silently producing it
// would mask a missing len-guard at the call site, surfacing later as
// a cryptic SQLite syntax error instead of a clear caller-bug. Every
// caller in this package guards len(ids) > 0 before calling; the panic
// is the trip-wire if a future caller forgets.
func inPlaceholders(n int) string {
	if n <= 0 {
		panic(fmt.Sprintf("inPlaceholders requires n > 0, got %d; the caller must guard len(ids) > 0 before invoking (SQL `IN ()` is invalid)", n))
	}
	return "(" + strings.Repeat("?,", n-1) + "?)"
}

// toAnyArgs converts a []string to []any so it can be spread into
// an ExecContext / QueryContext arg list.
func toAnyArgs(ids []string) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}

// doubleArgs returns the input slice twice concatenated — used for
// queries with two IN clauses against the same id set (entity_id
// + collected_from_entity_id).
func doubleArgs(ids []string) []any {
	out := make([]any, 0, len(ids)*2)
	for _, id := range ids {
		out = append(out, id)
	}
	for _, id := range ids {
		out = append(out, id)
	}
	return out
}

// --- selectors for bulk-prune convenience ---------------------------------

// ListVersionedEntities returns IDs of entities whose canonical_uri
// carries an @V version suffix (pkg:X@V or repo:X@V). Uses
// profile.SplitURIVersion rather than a raw LIKE so scoped npm
// packages (pkg:npm/@types/node) aren't mistaken for versioned
// entities — the scope `@` lives in a non-last segment and must
// survive the scan.
//
// Read-only. Callers wrap with a confirmation + PruneEntities for
// the `signatory prune versioned` surface.
func (s *SQLite) ListVersionedEntities(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, canonical_uri FROM entities WHERE canonical_uri LIKE 'pkg:%@%' OR canonical_uri LIKE 'repo:%@%'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // rows iteration complete; rows.Err() captures read-side errors below
	var ids []string
	for rows.Next() {
		var id, uri string
		if err := rows.Scan(&id, &uri); err != nil {
			return nil, err
		}
		// Double-check with SplitURIVersion so scoped npm packages
		// (@types/node, @stripe/*) — which match the LIKE but are
		// NOT versioned — don't get pruned.
		_, version := profile.SplitURIVersion(uri)
		if version != "" {
			ids = append(ids, id)
		}
	}
	return ids, rows.Err()
}

// ListOrphanEntities returns IDs of entities with no child rows in
// any of the known child tables. "Orphan" here means: no analyst_
// outputs (primary or collected_from), no postures, no burns, no
// signals, no dependency_observations. An audit_log row alone is
// NOT enough to save an entity — audit is observation, not reason-
// to-exist.
//
// Implementation: one query that LEFT JOINs against each child
// table's aggregated counts and filters rows where all counts
// are zero. Single round-trip beats per-entity child-count probes
// when the entity table grows.
func (s *SQLite) ListOrphanEntities(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT e.id
		  FROM entities e
		  LEFT JOIN (SELECT entity_id, COUNT(*) AS n FROM analyst_outputs GROUP BY entity_id) ao
		         ON ao.entity_id = e.id
		  LEFT JOIN (SELECT collected_from_entity_id AS entity_id, COUNT(*) AS n FROM analyst_outputs WHERE collected_from_entity_id IS NOT NULL GROUP BY collected_from_entity_id) aocf
		         ON aocf.entity_id = e.id
		  LEFT JOIN (SELECT entity_id, COUNT(*) AS n FROM postures GROUP BY entity_id) p
		         ON p.entity_id = e.id
		  LEFT JOIN (SELECT entity_id, COUNT(*) AS n FROM burns GROUP BY entity_id) b
		         ON b.entity_id = e.id
		  LEFT JOIN (SELECT entity_id, COUNT(*) AS n FROM signals GROUP BY entity_id) s
		         ON s.entity_id = e.id
		  LEFT JOIN (SELECT project_id AS entity_id, COUNT(*) AS n FROM dependency_observations GROUP BY project_id) d
		         ON d.entity_id = e.id
		 WHERE COALESCE(ao.n,0) = 0
		   AND COALESCE(aocf.n,0) = 0
		   AND COALESCE(p.n,0) = 0
		   AND COALESCE(b.n,0) = 0
		   AND COALESCE(s.n,0) = 0
		   AND COALESCE(d.n,0) = 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // rows iteration complete; rows.Err() captures read-side errors below
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
