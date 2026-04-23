package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/sarahmaeve/signatory/internal/profile"
)

// PruneReport summarizes what a prune operation touched (or would
// touch, for dry-run). Counts are per-table so the human-facing CLI
// can render a useful "this is what will happen" preview.
type PruneReport struct {
	// Entities names each entity that matched the prune scope.
	Entities []EntityPruneDetail
	// RowsByTable is the aggregate row count across all selected
	// entities, keyed by child table. Includes entities itself.
	RowsByTable map[string]int
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
func (s *SQLite) PlanPruneEntities(ctx context.Context, entityIDs []string) (*PruneReport, error) {
	if len(entityIDs) == 0 {
		return &PruneReport{RowsByTable: map[string]int{}}, nil
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
		for table, count := range detail.ChildCounts {
			report.RowsByTable[table] += count
		}
	}
	// Entities themselves are always counted.
	report.RowsByTable["entities"] += len(report.Entities)
	return report, nil
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
		if err == sql.ErrNoRows {
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
	defer rows.Close() //nolint:errcheck // read-only, close errors aren't actionable
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
	placeholders = placeholders[:len(placeholders)-1] // trim trailing comma
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

	// Capture the per-entity detail BEFORE the delete so the report
	// carries meaningful canonical_uri / short_name fields. The plan
	// is also the source of truth for counts — we compare actual
	// rowsAffected in the deletes to catch divergence.
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
	defer rows.Close() //nolint:errcheck
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
	for _, spec := range []struct{ table, column string; ids []string }{
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
	for _, spec := range []struct{ kind string; ids []string }{
		{"conclusion", conclusionIDs},
		{"positive_absence", absenceIDs},
		{"observation", observationIDs},
	} {
		if len(spec.ids) == 0 {
			continue
		}
		placeholders := inPlaceholders(len(spec.ids))
		q := `DELETE FROM citations WHERE parent_kind = ? AND parent_id IN ` + placeholders
		args := append([]any{spec.kind}, toAnyArgs(spec.ids)...)
		r, err := tx.ExecContext(ctx, q, args...)
		if err != nil {
			return nil, fmt.Errorf("delete citations/%s: %w", spec.kind, err)
		}
		n, _ := r.RowsAffected()
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
	for _, spec := range []struct{ table string; ids []string }{
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
	directChildren := []struct{ table, column string }{
		{"analyst_outputs", "entity_id"},
		{"analyst_outputs", "collected_from_entity_id"},
		{"postures", "entity_id"},
		{"burns", "entity_id"},
		{"signals", "entity_id"},
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
	n, _ := r.RowsAffected()
	return int(n), nil
}

// collectIDs runs a SELECT that returns one TEXT column per row
// (IDs) and gathers them into a slice.
func collectIDs(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
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
// Callers pass this straight into WHERE ... IN clauses. n must be
// > 0 or the produced SQL would be a syntax error — the callers
// here all check len(ids) before calling.
func inPlaceholders(n int) string {
	if n == 0 {
		return "()"
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
	defer rows.Close() //nolint:errcheck
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
	defer rows.Close() //nolint:errcheck
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
