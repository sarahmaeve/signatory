package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sarahmaeve/signatory/internal/exchange"
)

// AnalystOutputSummary is the lightweight read-row for listings —
// enough to identify an output and surface counts without the cost
// of reconstructing the full AnalystOutput document tree. Callers
// who need the full document use GetAnalystOutput.
type AnalystOutputSummary struct {
	OutputID string
	EntityID string
	// EntityURI is the canonical_uri of the entity the analysis is
	// indexed under — the caller's identity in the M2 model
	// (pkg:npm/X), or the analyst's own target when no resolution
	// hop happened.
	EntityURI string
	// CollectedFromEntityID is the UUID of the entity the analysis
	// was actually performed against, when it differs from EntityID.
	// Empty when there was no resolution hop (pre-M2 rows, or rows
	// where primary_target == out.Target at ingest).
	CollectedFromEntityID string
	// CollectedFromURI is the canonical_uri of the collected-from
	// entity, joined for display. Empty when CollectedFromEntityID
	// is empty. Agent-facing-contract §3.2 transparent-with-citation:
	// every response where a resolution hop happened cites both
	// URIs so duplicates are hard to create and the hop is visible.
	CollectedFromURI     string
	AnalystID            string
	Model                string
	PromptVersion        string
	InvokedAt            string // RFC3339 strings preserved verbatim
	IngestedAt           string
	Round                int
	TargetCommit         string
	SourcePath           string
	ContentHash          string
	ConclusionsCount     int
	PositiveAbsenceCount int
	ObservationCount     int
	PatternCount         int
}

// AnalystOutputFilter narrows ListAnalystOutputs results.
// Empty string fields and zero Limit are ignored (return all).
type AnalystOutputFilter struct {
	// EntityID matches an exact entities.id (UUID).
	EntityID string

	// EntityURI is looked up against entities.canonical_uri and
	// then translated to EntityID. Convenience for callers who
	// have a target string but not the entity's UUID.
	EntityURI string

	// AnalystID matches analyst_outputs.analyst_id (e.g.
	// "external-sec-v1", "signatory-provenance").
	AnalystID string

	// Since limits results to outputs ingested at or after this
	// timestamp. Zero value disables the filter (returns all
	// regardless of age). The freshness check on `signatory
	// analyze` uses this to surface "what's been seen recently"
	// without paging through years of history.
	Since time.Time

	// Limit caps the result set. 0 = no limit.
	Limit int
}

// ListAnalystOutputs returns matching outputs, newest-ingested first.
// The result includes child counts joined from the per-entity
// child tables; this costs one COUNT-aggregating subquery per row
// shape, which is fine at our scale (small) and remains accurate
// against the append-only data model.
//
// When the EntityURI filter is specified but does not resolve to a
// known entity, returns (nil, ErrNotFound). Callers that treat
// "unknown target" as "no analyses yet" should check for this
// sentinel explicitly — see `errors.Is(err, store.ErrNotFound)`.
// The `signatory show-*` commands use this distinction to print a
// better message (entity doesn't exist vs. entity has no rows yet).
func (s *SQLite) ListAnalystOutputs(ctx context.Context, filter AnalystOutputFilter) ([]AnalystOutputSummary, error) {
	// Resolve EntityURI → EntityID if specified. Done before SQL
	// construction so the WHERE clause stays simple.
	entityID := filter.EntityID
	if entityID == "" && filter.EntityURI != "" {
		ent, err := s.FindEntityByURI(ctx, filter.EntityURI)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil, ErrNotFound
			}
			return nil, fmt.Errorf("resolve entity URI %q: %w", filter.EntityURI, err)
		}
		entityID = ent.ID
	}

	var clauses []string
	var args []any
	if entityID != "" {
		// Walk both the primary entity_id and the collected_from_entity_id
		// (M2 identity indexing): a query for pkg:npm/X finds analyses
		// that were collected against repo:github/Y and indexed under
		// pkg:npm/X, AND finds analyses indexed directly under the
		// caller's repo:github/Y URI.
		clauses = append(clauses, "(ao.entity_id = ? OR ao.collected_from_entity_id = ?)")
		args = append(args, entityID, entityID)
	}
	if filter.AnalystID != "" {
		clauses = append(clauses, "ao.analyst_id = ?")
		args = append(args, filter.AnalystID)
	}
	if !filter.Since.IsZero() {
		clauses = append(clauses, "ao.ingested_at >= ?")
		args = append(args, filter.Since.UTC().Format(time.RFC3339))
	}
	whereSQL := ""
	if len(clauses) > 0 {
		whereSQL = "WHERE " + strings.Join(clauses, " AND ")
	}
	limitSQL := ""
	if filter.Limit > 0 {
		limitSQL = fmt.Sprintf("LIMIT %d", filter.Limit)
	}

	query := fmt.Sprintf(`
		SELECT
			ao.id, ao.entity_id, e.canonical_uri,
			ao.collected_from_entity_id, COALESCE(cf.canonical_uri, ''),
			ao.analyst_id, ao.model, ao.prompt_version,
			ao.invoked_at, ao.ingested_at, ao.round,
			ao.target_commit, ao.source_path, ao.content_hash,
			(SELECT COUNT(*) FROM conclusions         WHERE output_id = ao.id) AS conclusions_count,
			(SELECT COUNT(*) FROM positive_absences  WHERE output_id = ao.id) AS absence_count,
			(SELECT COUNT(*) FROM observations       WHERE output_id = ao.id) AS obs_count,
			(SELECT COUNT(*) FROM methodology_patterns WHERE output_id = ao.id) AS pat_count
		FROM analyst_outputs ao
		INNER JOIN entities e ON e.id = ao.entity_id
		LEFT JOIN entities cf ON cf.id = ao.collected_from_entity_id
		%s
		ORDER BY ao.ingested_at DESC
		%s`, whereSQL, limitSQL)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list analyst_outputs: %w", err)
	}
	defer rows.Close() //nolint:errcheck // close on read-only rows; any real error surfaced during Scan

	var out []AnalystOutputSummary
	for rows.Next() {
		var s AnalystOutputSummary
		var collectedFromID sql.NullString
		if err := rows.Scan(
			&s.OutputID, &s.EntityID, &s.EntityURI,
			&collectedFromID, &s.CollectedFromURI,
			&s.AnalystID, &s.Model, &s.PromptVersion,
			&s.InvokedAt, &s.IngestedAt, &s.Round,
			&s.TargetCommit, &s.SourcePath, &s.ContentHash,
			&s.ConclusionsCount, &s.PositiveAbsenceCount,
			&s.ObservationCount, &s.PatternCount,
		); err != nil {
			return nil, fmt.Errorf("scan analyst_output row: %w", err)
		}
		if collectedFromID.Valid {
			s.CollectedFromEntityID = collectedFromID.String
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ConclusionSummary is the lightweight read-row for conclusions listings.
// Verdict is included because it's the load-bearing identifier for
// a conclusion when a human is scanning a list; rationale stays out
// (per the same logic as format-check --summary).
type ConclusionSummary struct {
	OutputID          string
	EntityID          string
	EntityURI         string
	AnalystID         string
	IngestedAt        string
	ConclusionID      string // UUID
	ConclusionLocalID string // "F001"
	Verdict           string
	SeverityDefault   string
	DesignIntent      bool
	Category          string
	SignalType        string // "" if absent
	CitationCount     int
	HasSupersedes     bool
	BySupersedesIDs   []string // populated only when filter.IncludeSupersedes is true
}

// ConclusionFilter narrows ListConclusions results.
type ConclusionFilter struct {
	EntityID   string
	EntityURI  string
	AnalystID  string
	SignalType string

	// SeverityIn limits to conclusions whose severity_default is in
	// the provided set. Empty = all severities.
	SeverityIn []exchange.SeverityValue

	// DesignIntentOnly limits to conclusions with design_intent = true.
	// Useful for "what does this project deliberately do that we
	// should know about?" queries.
	DesignIntentOnly bool

	Limit int
}

// ListConclusions returns conclusions across one or more analyst outputs,
// newest first by the parent output's ingested_at. Each row joins
// to entities for the canonical_uri convenience field and to
// analyst_outputs for analyst_id + ingested_at attribution.
//
// When the EntityURI filter is specified but does not resolve,
// returns (nil, ErrNotFound); see ListAnalystOutputs for the
// rationale.
func (s *SQLite) ListConclusions(ctx context.Context, filter ConclusionFilter) ([]ConclusionSummary, error) {
	entityID := filter.EntityID
	if entityID == "" && filter.EntityURI != "" {
		ent, err := s.FindEntityByURI(ctx, filter.EntityURI)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil, ErrNotFound
			}
			return nil, fmt.Errorf("resolve entity URI %q: %w", filter.EntityURI, err)
		}
		entityID = ent.ID
	}

	var clauses []string
	var args []any
	if entityID != "" {
		// Walk both the primary entity_id and the collected_from_entity_id
		// (M2 identity indexing): a query for pkg:npm/X finds analyses
		// that were collected against repo:github/Y and indexed under
		// pkg:npm/X, AND finds analyses indexed directly under the
		// caller's repo:github/Y URI.
		clauses = append(clauses, "(ao.entity_id = ? OR ao.collected_from_entity_id = ?)")
		args = append(args, entityID, entityID)
	}
	if filter.AnalystID != "" {
		clauses = append(clauses, "ao.analyst_id = ?")
		args = append(args, filter.AnalystID)
	}
	if filter.SignalType != "" {
		clauses = append(clauses, "f.signal_type = ?")
		args = append(args, filter.SignalType)
	}
	if filter.DesignIntentOnly {
		clauses = append(clauses, "f.design_intent = 1")
	}
	if len(filter.SeverityIn) > 0 {
		placeholders := strings.Repeat("?,", len(filter.SeverityIn))
		placeholders = strings.TrimRight(placeholders, ",")
		clauses = append(clauses, "f.severity_default IN ("+placeholders+")")
		for _, sev := range filter.SeverityIn {
			args = append(args, string(sev))
		}
	}
	whereSQL := ""
	if len(clauses) > 0 {
		whereSQL = "WHERE " + strings.Join(clauses, " AND ")
	}
	limitSQL := ""
	if filter.Limit > 0 {
		limitSQL = fmt.Sprintf("LIMIT %d", filter.Limit)
	}

	query := fmt.Sprintf(`
		SELECT
			ao.id, ao.entity_id, e.canonical_uri, ao.analyst_id, ao.ingested_at,
			f.id, f.conclusion_local_id, f.verdict, f.severity_default,
			f.design_intent, f.category, f.signal_type,
			(SELECT COUNT(*) FROM citations WHERE parent_kind = 'conclusion' AND parent_id = f.id) AS cite_count,
			(SELECT COUNT(*) FROM conclusion_supersedes WHERE conclusion_id = f.id) > 0 AS has_supersedes
		FROM conclusions f
		INNER JOIN analyst_outputs ao ON ao.id = f.output_id
		INNER JOIN entities e ON e.id = ao.entity_id
		%s
		ORDER BY ao.ingested_at DESC, f.conclusion_local_id ASC
		%s`, whereSQL, limitSQL)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list conclusions: %w", err)
	}
	defer rows.Close() //nolint:errcheck // close on read-only rows; any real error surfaced during Scan

	var out []ConclusionSummary
	for rows.Next() {
		var f ConclusionSummary
		var designIntent int
		if err := rows.Scan(
			&f.OutputID, &f.EntityID, &f.EntityURI,
			&f.AnalystID, &f.IngestedAt,
			&f.ConclusionID, &f.ConclusionLocalID, &f.Verdict,
			&f.SeverityDefault, &designIntent, &f.Category, &f.SignalType,
			&f.CitationCount, &f.HasSupersedes,
		); err != nil {
			return nil, fmt.Errorf("scan conclusion row: %w", err)
		}
		f.DesignIntent = designIntent != 0
		out = append(out, f)
	}
	return out, rows.Err()
}

// MethodologyPatternSummary is the lightweight read-row for
// pattern listings. Description is included because that's the
// load-bearing identifier for a pattern.
type MethodologyPatternSummary struct {
	OutputID          string
	EntityURI         string
	AnalystID         string
	IngestedAt        string
	PatternID         string // UUID
	PatternLocalID    string // "MP-PY-NET-01"
	SignalGroup       string
	Description       string
	HasPatternText    bool
	GrepPrecision     string
	ReasoningDepth    string
	MissMode          string // "" if absent
	HitOnTarget       *bool  // nil if -1 sentinel
	ComposesWithCount int
}

// MethodologyPatternFilter narrows ListMethodologyPatterns.
type MethodologyPatternFilter struct {
	EntityID    string
	EntityURI   string
	AnalystID   string
	SignalGroup string

	// HitOnTarget filters by the tristate hit_on_target column:
	// nil = no filter; true = hit only; false = miss only.
	HitOnTarget *bool

	Limit int
}

// ListMethodologyPatterns returns patterns across analyst outputs.
// The aggregation use case ("which network_endpoints patterns
// actually fired across our analyses?") is the main consumer; the
// hit_on_target filter and signal_group filter map directly.
//
// When the EntityURI filter is specified but does not resolve,
// returns (nil, ErrNotFound); see ListAnalystOutputs for the
// rationale.
func (s *SQLite) ListMethodologyPatterns(ctx context.Context, filter MethodologyPatternFilter) ([]MethodologyPatternSummary, error) {
	entityID := filter.EntityID
	if entityID == "" && filter.EntityURI != "" {
		ent, err := s.FindEntityByURI(ctx, filter.EntityURI)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil, ErrNotFound
			}
			return nil, fmt.Errorf("resolve entity URI %q: %w", filter.EntityURI, err)
		}
		entityID = ent.ID
	}

	var clauses []string
	var args []any
	if entityID != "" {
		// Walk both the primary entity_id and the collected_from_entity_id
		// (M2 identity indexing): a query for pkg:npm/X finds analyses
		// that were collected against repo:github/Y and indexed under
		// pkg:npm/X, AND finds analyses indexed directly under the
		// caller's repo:github/Y URI.
		clauses = append(clauses, "(ao.entity_id = ? OR ao.collected_from_entity_id = ?)")
		args = append(args, entityID, entityID)
	}
	if filter.AnalystID != "" {
		clauses = append(clauses, "ao.analyst_id = ?")
		args = append(args, filter.AnalystID)
	}
	if filter.SignalGroup != "" {
		clauses = append(clauses, "mp.signal_group = ?")
		args = append(args, filter.SignalGroup)
	}
	if filter.HitOnTarget != nil {
		if *filter.HitOnTarget {
			clauses = append(clauses, "mp.hit_on_target = 1")
		} else {
			clauses = append(clauses, "mp.hit_on_target = 0")
		}
	}
	whereSQL := ""
	if len(clauses) > 0 {
		whereSQL = "WHERE " + strings.Join(clauses, " AND ")
	}
	limitSQL := ""
	if filter.Limit > 0 {
		limitSQL = fmt.Sprintf("LIMIT %d", filter.Limit)
	}

	query := fmt.Sprintf(`
		SELECT
			ao.id, e.canonical_uri, ao.analyst_id, ao.ingested_at,
			mp.id, mp.pattern_local_id, mp.signal_group, mp.description,
			LENGTH(mp.pattern_text) > 0 AS has_pattern_text,
			mp.grep_precision, mp.reasoning_depth, mp.miss_mode,
			mp.hit_on_target,
			(SELECT COUNT(*) FROM methodology_pattern_composes WHERE pattern_id = mp.id) AS composes_count
		FROM methodology_patterns mp
		INNER JOIN analyst_outputs ao ON ao.id = mp.output_id
		INNER JOIN entities e ON e.id = ao.entity_id
		%s
		ORDER BY ao.ingested_at DESC, mp.signal_group ASC, mp.pattern_local_id ASC
		%s`, whereSQL, limitSQL)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list methodology_patterns: %w", err)
	}
	defer rows.Close() //nolint:errcheck // close on read-only rows; any real error surfaced during Scan

	var out []MethodologyPatternSummary
	for rows.Next() {
		var p MethodologyPatternSummary
		var hit int
		if err := rows.Scan(
			&p.OutputID, &p.EntityURI, &p.AnalystID, &p.IngestedAt,
			&p.PatternID, &p.PatternLocalID, &p.SignalGroup, &p.Description,
			&p.HasPatternText, &p.GrepPrecision, &p.ReasoningDepth, &p.MissMode,
			&hit, &p.ComposesWithCount,
		); err != nil {
			return nil, fmt.Errorf("scan methodology_pattern row: %w", err)
		}
		switch hit {
		case 1:
			t := true
			p.HitOnTarget = &t
		case 0:
			f := false
			p.HitOnTarget = &f
		default:
			// -1 sentinel = nil
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SeverityCounts returns the per-severity count of conclusions on
// one AnalystOutput. Keys are exchange.SeverityValue; values are
// counts. Zero counts are omitted — callers iterating the map see
// only severities that actually occur on this output. Used by the
// summary assembler (M7).
func (s *SQLite) SeverityCounts(ctx context.Context, outputID string) (map[exchange.SeverityValue]int, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT severity_default, COUNT(*)
		 FROM conclusions
		 WHERE output_id = ?
		 GROUP BY severity_default`, outputID)
	if err != nil {
		return nil, fmt.Errorf("query severity counts: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only rows; scan errors propagate below

	out := make(map[exchange.SeverityValue]int)
	for rows.Next() {
		var sev string
		var n int
		if err := rows.Scan(&sev, &n); err != nil {
			return nil, fmt.Errorf("scan severity-count row: %w", err)
		}
		out[exchange.SeverityValue(sev)] = n
	}
	return out, rows.Err()
}

// ListRelatedURIs returns canonical URIs of entities that share an
// analyst_outputs row with entityID via the M2 collected_from link
// — both directions: entities this one's analyses were collected
// from, and entities whose analyses were collected from this one.
// Used by the summary assembler to surface "what other identities
// does signatory know are related to this one?"
//
// Deduplication happens at the SQL level via DISTINCT; the caller
// additionally strips its own URI and sorts for stable display.
func (s *SQLite) ListRelatedURIs(ctx context.Context, entityID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT uri FROM (
		    -- Forward: this entity's analyses → the source entities
		    -- those analyses were collected from.
		    SELECT e.canonical_uri AS uri
		    FROM analyst_outputs ao
		    JOIN entities e ON e.id = ao.collected_from_entity_id
		    WHERE ao.entity_id = ?
		    UNION
		    -- Reverse: analyses collected from this entity → the
		    -- primary-identity entities those analyses landed under.
		    SELECT e.canonical_uri AS uri
		    FROM analyst_outputs ao
		    JOIN entities e ON e.id = ao.entity_id
		    WHERE ao.collected_from_entity_id = ?
		)`, entityID, entityID)
	if err != nil {
		return nil, fmt.Errorf("query related URIs: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only rows

	var out []string
	for rows.Next() {
		var uri string
		if err := rows.Scan(&uri); err != nil {
			return nil, fmt.Errorf("scan related URI: %w", err)
		}
		out = append(out, uri)
	}
	return out, rows.Err()
}

// GetSynthesisProposal returns the ProposedPosture recorded against a
// synthesis output, reading only the denormalized proposed_tier /
// proposed_version_scope columns plus the rationale_summary field
// inside the JSON blob. Avoids the full GetAnalystOutput
// reconstruction when the caller (M6d `signatory posture accept`)
// only needs the proposal fields.
//
// Returns ErrNotFound when:
//   - outputID doesn't exist, OR
//   - the row has proposed_tier = NULL (i.e. it's not a synthesis)
//
// The caller can't tell these apart from this method alone; that's
// by design — both are "there's no proposal to accept here." Callers
// that need to distinguish "row missing" from "row present but not a
// synthesis" should use GetAnalystOutput first.
func (s *SQLite) GetSynthesisProposal(ctx context.Context, outputID string) (*exchange.ProposedPosture, error) {
	if outputID == "" {
		return nil, fmt.Errorf("%w: outputID required", ErrNilInput)
	}
	var (
		proposedTier         sql.NullString
		proposedVersionScope sql.NullString
		supplementJSON       sql.NullString
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT proposed_tier, proposed_version_scope, synthesis_supplement_json
		 FROM analyst_outputs WHERE id = ?`, outputID).Scan(
		&proposedTier, &proposedVersionScope, &supplementJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load synthesis proposal for %s: %w", outputID, err)
	}
	// proposed_tier NULL means the row isn't a synthesis. Treat as
	// not-found to keep the accept verb's error handling uniform.
	if !proposedTier.Valid {
		return nil, ErrNotFound
	}
	// The rationale lives inside the JSON blob — we already decided
	// not to denormalize it (too long-form, never filtered on). Pull
	// just that one field out of the blob.
	var partial struct {
		ProposedPosture struct {
			RationaleSummary string `json:"rationale_summary"`
		} `json:"proposed_posture"`
	}
	if supplementJSON.Valid && supplementJSON.String != "" {
		if err := json.Unmarshal([]byte(supplementJSON.String), &partial); err != nil {
			return nil, fmt.Errorf("unmarshal synthesis_supplement for proposal %s: %w", outputID, err)
		}
	}
	return &exchange.ProposedPosture{
		Tier:             proposedTier.String,
		VersionScope:     proposedVersionScope.String, // NullString zero-value is "", which is the correct "unversioned" marker
		RationaleSummary: partial.ProposedPosture.RationaleSummary,
	}, nil
}

// GetAnalystOutput reconstructs the full AnalystOutput document
// from the v4 row decomposition. Inverse of IngestAnalystOutput.
//
// Cost: ~10–15 queries depending on document complexity. Acceptable
// at our scale (a few KB of structured data per output, one user
// query at a time). Optimization deferred until profiling indicates
// it's needed.
func (s *SQLite) GetAnalystOutput(ctx context.Context, outputID string) (*exchange.AnalystOutput, error) {
	if outputID == "" {
		return nil, fmt.Errorf("%w: outputID required", ErrNilInput)
	}

	out := &exchange.AnalystOutput{}
	var entityID, ignoredHash, ignoredIngestedAt, ignoredSourcePath string
	var supplementJSON sql.NullString
	var rowTarget string
	err := s.db.QueryRowContext(ctx,
		`SELECT entity_id, analyst_id, model, prompt_version, invoked_at,
		        ingested_at, round, target_commit, round_notes, source_path,
		        content_hash, synthesis_supplement_json, target
		 FROM analyst_outputs WHERE id = ?`, outputID).Scan(
		&entityID,
		&out.Attribution.AnalystID, &out.Attribution.Model,
		&out.Attribution.PromptVersion, &out.Attribution.InvokedAt,
		&ignoredIngestedAt, &out.Attribution.Round,
		&out.TargetCommit, &out.RoundNotes, &ignoredSourcePath, &ignoredHash,
		&supplementJSON, &rowTarget,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load analyst_output: %w", err)
	}
	// Reconstruct out.Target from the row's target column (v10+) —
	// it carries the caller-supplied identity (including @V if any),
	// which is the faithful answer to "what was this analyst
	// analyzing?" even after Plan-A canonicalization moved the entity
	// row itself to an unversioned URI.
	//
	// Fallback for pre-v10 rows: target is empty-string defaulted by
	// the v10 migration but a mis-applied migration could leave old
	// rows with target=''; in that case the entity's canonical_uri
	// is the lossless answer (pre-v10 entity canonical_uri == the
	// caller's target verbatim).
	if rowTarget != "" {
		out.Target = rowTarget
	} else {
		var entityURI string
		if err := s.db.QueryRowContext(ctx,
			`SELECT canonical_uri FROM entities WHERE id = ?`,
			entityID).Scan(&entityURI); err != nil {
			return nil, fmt.Errorf("load target URI: %w", err)
		}
		out.Target = entityURI
	}

	// M6a: reconstruct the synthesis supplement from the JSON column
	// when present. Non-synthesist outputs have supplement_json = NULL
	// and SynthesisSupplement stays nil.
	if supplementJSON.Valid && supplementJSON.String != "" {
		var supplement exchange.SynthesisSupplement
		if err := json.Unmarshal([]byte(supplementJSON.String), &supplement); err != nil {
			return nil, fmt.Errorf("unmarshal synthesis supplement for %s: %w", outputID, err)
		}
		out.SynthesisSupplement = &supplement
	}

	out.Conclusions, err = s.loadConclusions(ctx, outputID)
	if err != nil {
		return nil, err
	}
	out.PositiveAbsences, err = s.loadPositiveAbsences(ctx, outputID)
	if err != nil {
		return nil, err
	}
	out.Observations, err = s.loadObservations(ctx, outputID)
	if err != nil {
		return nil, err
	}
	out.MethodologyTrace, err = s.loadMethodologyTrace(ctx, outputID)
	if err != nil {
		return nil, err
	}
	out.Supersedes, err = s.loadOutputSupersedes(ctx, outputID)
	if err != nil {
		return nil, err
	}
	out.ReframesFrom, err = s.loadOutputReframesFrom(ctx, outputID)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetConclusion returns the full Conclusion for one conclusion UUID, including
// rationale, citations, severity contexts, supersession records,
// prerequisites, remediation hints, and related conclusions. This is the
// complement to ListConclusions (which returns summary rows) — use it
// when the caller has a ConclusionID and wants every field.
//
// Returns ErrNotFound when the UUID does not exist. The query cost is
// ~6 statements (one to load the row, one per child table); acceptable
// for a detail lookup. The loader helpers are shared with the
// output-level GetAnalystOutput path.
func (s *SQLite) GetConclusion(ctx context.Context, conclusionID string) (*exchange.Conclusion, error) {
	if conclusionID == "" {
		return nil, fmt.Errorf("%w: conclusionID required", ErrNilInput)
	}

	var (
		f            exchange.Conclusion
		designIntent int
		signalT      string
		answers      string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT conclusion_local_id, verdict, rationale,
		        severity_default, design_intent, category,
		        signal_type, answers_question
		 FROM conclusions WHERE id = ?`, conclusionID).Scan(
		&f.ID, &f.Verdict, &f.Rationale,
		&f.Severity.Default, &designIntent, &f.Category,
		&signalT, &answers,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load conclusion: %w", err)
	}
	f.DesignIntent = designIntent != 0
	if signalT != "" {
		f.SignalType = &signalT
	}
	if answers != "" {
		f.AnswersQuestion = &answers
	}

	// Child tables — identical set to loadConclusions' per-row block.
	if f.Severity.ByContext, err = s.loadConclusionSeverityContexts(ctx, conclusionID); err != nil {
		return nil, err
	}
	if f.Supersedes, err = s.loadConclusionSupersedes(ctx, conclusionID); err != nil {
		return nil, err
	}
	if f.Prerequisites, err = s.loadOrderedTexts(ctx, "conclusion_prerequisites", "conclusion_id", conclusionID); err != nil {
		return nil, err
	}
	if f.RemediationHints, err = s.loadOrderedTexts(ctx, "conclusion_remediation_hints", "conclusion_id", conclusionID); err != nil {
		return nil, err
	}
	if f.RelatedConclusions, err = s.loadConclusionRelated(ctx, conclusionID); err != nil {
		return nil, err
	}
	if f.Citations, err = s.loadCitations(ctx, "conclusion", conclusionID); err != nil {
		return nil, err
	}
	return &f, nil
}

func (s *SQLite) loadConclusions(ctx context.Context, outputID string) ([]exchange.Conclusion, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, conclusion_local_id, verdict, rationale,
		        severity_default, design_intent, category,
		        signal_type, answers_question
		 FROM conclusions WHERE output_id = ? ORDER BY conclusion_local_id`,
		outputID)
	if err != nil {
		return nil, fmt.Errorf("query conclusions: %w", err)
	}
	defer rows.Close() //nolint:errcheck // close on read-only rows; any real error surfaced during Scan

	type conclusionRow struct {
		uuid    string
		f       exchange.Conclusion
		signalT string
		answers string
	}
	var raw []conclusionRow
	for rows.Next() {
		var cr conclusionRow
		var designIntent int
		if err := rows.Scan(
			&cr.uuid, &cr.f.ID, &cr.f.Verdict, &cr.f.Rationale,
			&cr.f.Severity.Default, &designIntent, &cr.f.Category,
			&cr.signalT, &cr.answers,
		); err != nil {
			return nil, err
		}
		cr.f.DesignIntent = designIntent != 0
		if cr.signalT != "" {
			cr.f.SignalType = &cr.signalT
		}
		if cr.answers != "" {
			cr.f.AnswersQuestion = &cr.answers
		}
		raw = append(raw, cr)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Walk children for each conclusion.
	out := make([]exchange.Conclusion, 0, len(raw))
	for i := range raw {
		f := raw[i].f
		f.Severity.ByContext, err = s.loadConclusionSeverityContexts(ctx, raw[i].uuid)
		if err != nil {
			return nil, err
		}
		f.Supersedes, err = s.loadConclusionSupersedes(ctx, raw[i].uuid)
		if err != nil {
			return nil, err
		}
		f.Prerequisites, err = s.loadOrderedTexts(ctx, "conclusion_prerequisites", "conclusion_id", raw[i].uuid)
		if err != nil {
			return nil, err
		}
		f.RemediationHints, err = s.loadOrderedTexts(ctx, "conclusion_remediation_hints", "conclusion_id", raw[i].uuid)
		if err != nil {
			return nil, err
		}
		f.RelatedConclusions, err = s.loadConclusionRelated(ctx, raw[i].uuid)
		if err != nil {
			return nil, err
		}
		f.Citations, err = s.loadCitations(ctx, "conclusion", raw[i].uuid)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}

func (s *SQLite) loadConclusionSeverityContexts(ctx context.Context, conclusionID string) ([]exchange.ContextualSeverity, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT host_isolation, platform, value
		 FROM conclusion_severity_contexts WHERE conclusion_id = ?
		 ORDER BY host_isolation, platform`, conclusionID)
	if err != nil {
		return nil, fmt.Errorf("query conclusion_severity_contexts: %w", err)
	}
	defer rows.Close() //nolint:errcheck // close on read-only rows; any real error surfaced during Scan
	var out []exchange.ContextualSeverity
	for rows.Next() {
		var cs exchange.ContextualSeverity
		var value string
		if err := rows.Scan(&cs.Context.HostIsolation, &cs.Context.Platform, &value); err != nil {
			return nil, err
		}
		cs.Value = exchange.SeverityValue(value)
		out = append(out, cs)
	}
	return out, rows.Err()
}

func (s *SQLite) loadConclusionSupersedes(ctx context.Context, conclusionID string) ([]exchange.Supersession, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT prior_id, prior_round, kind FROM conclusion_supersedes
		 WHERE conclusion_id = ? ORDER BY prior_id`, conclusionID)
	if err != nil {
		return nil, fmt.Errorf("query conclusion_supersedes: %w", err)
	}
	defer rows.Close() //nolint:errcheck // close on read-only rows; any real error surfaced during Scan
	var out []exchange.Supersession
	for rows.Next() {
		var sup exchange.Supersession
		var kind string
		if err := rows.Scan(&sup.PriorID, &sup.PriorRound, &kind); err != nil {
			return nil, err
		}
		sup.Kind = exchange.SupersessionKind(kind)
		out = append(out, sup)
	}
	return out, rows.Err()
}

// orderedTextTables is the allowlist of (table, parent_column) pairs
// loadOrderedTexts is permitted to query. New callers extend this map;
// anything not on the list is a loud panic rather than a silent SQL
// injection. The value (parentID) is always bound via ?; only the
// table and column identifiers are interpolated, and those must come
// from this allowlist.
var orderedTextTables = map[string]string{
	"conclusion_prerequisites":     "conclusion_id",
	"conclusion_remediation_hints": "conclusion_id",
}

// loadOrderedTexts handles conclusion_prerequisites and conclusion_remediation_hints,
// which share schema (conclusion_id, seq, text). Generalizes to keep the
// per-loader code small.
//
// Safety: identifiers are interpolated (SQL does not support binding
// schema names) but the allowlist above guarantees only known-safe
// values reach the query. The only user-influenced value — parentID —
// is parameterized via ?.
func (s *SQLite) loadOrderedTexts(ctx context.Context, table, parentCol, parentID string) ([]string, error) {
	if want, ok := orderedTextTables[table]; !ok || want != parentCol {
		return nil, fmt.Errorf("loadOrderedTexts: (table=%q, parentCol=%q) is not on the allowlist", table, parentCol)
	}
	q := fmt.Sprintf(`SELECT text FROM %s WHERE %s = ? ORDER BY seq`, table, parentCol) //nolint:gosec // G201: table and parentCol are allowlist-checked above; parentID is bound via ?
	rows, err := s.db.QueryContext(ctx, q, parentID)
	if err != nil {
		return nil, fmt.Errorf("query %s: %w", table, err)
	}
	defer rows.Close() //nolint:errcheck // close on read-only rows; any real error surfaced during Scan
	var out []string
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			return nil, err
		}
		out = append(out, text)
	}
	return out, rows.Err()
}

func (s *SQLite) loadConclusionRelated(ctx context.Context, conclusionID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT related_id FROM conclusion_related WHERE conclusion_id = ? ORDER BY related_id`,
		conclusionID)
	if err != nil {
		return nil, fmt.Errorf("query conclusion_related: %w", err)
	}
	defer rows.Close() //nolint:errcheck // close on read-only rows; any real error surfaced during Scan
	var out []string
	for rows.Next() {
		var rel string
		if err := rows.Scan(&rel); err != nil {
			return nil, err
		}
		out = append(out, rel)
	}
	return out, rows.Err()
}

func (s *SQLite) loadCitations(ctx context.Context, parentKind, parentID string) ([]exchange.Citation, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT path, line_start, line_end, scope_kind, scope_path, commit_sha, quoted
		 FROM citations WHERE parent_kind = ? AND parent_id = ? ORDER BY seq`,
		parentKind, parentID)
	if err != nil {
		return nil, fmt.Errorf("query citations: %w", err)
	}
	defer rows.Close() //nolint:errcheck // close on read-only rows; any real error surfaced during Scan
	var out []exchange.Citation
	for rows.Next() {
		var c exchange.Citation
		var path, scopeKind, scopePath, commitSHA, quoted string
		var lineStart, lineEnd int
		if err := rows.Scan(&path, &lineStart, &lineEnd, &scopeKind, &scopePath, &commitSHA, &quoted); err != nil {
			return nil, err
		}
		c.Path = path
		if lineStart >= 0 {
			ls := lineStart
			c.LineStart = &ls
		}
		if lineEnd >= 0 {
			le := lineEnd
			c.LineEnd = &le
		}
		if scopeKind != "" {
			c.Scope = &exchange.ScopeRef{Kind: scopeKind, Path: scopePath}
		}
		if commitSHA != "" {
			c.CommitSHA = &commitSHA
		}
		if quoted != "" {
			c.Quoted = &quoted
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *SQLite) loadPositiveAbsences(ctx context.Context, outputID string) ([]exchange.PositiveAbsence, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, pattern_checked, description, confidence, pattern_ref
		 FROM positive_absences WHERE output_id = ? ORDER BY pattern_checked`, outputID)
	if err != nil {
		return nil, fmt.Errorf("query positive_absences: %w", err)
	}
	defer rows.Close() //nolint:errcheck // close on read-only rows; any real error surfaced during Scan

	type paRow struct {
		uuid       string
		pa         exchange.PositiveAbsence
		patternRef string
	}
	var raw []paRow
	for rows.Next() {
		var pr paRow
		var conf string
		if err := rows.Scan(&pr.uuid, &pr.pa.PatternChecked, &pr.pa.Description, &conf, &pr.patternRef); err != nil {
			return nil, err
		}
		pr.pa.Confidence = exchange.Confidence(conf)
		if pr.patternRef != "" {
			pr.pa.PatternRef = &pr.patternRef
		}
		raw = append(raw, pr)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]exchange.PositiveAbsence, 0, len(raw))
	for i := range raw {
		pa := raw[i].pa
		pa.Citations, err = s.loadCitations(ctx, "positive_absence", raw[i].uuid)
		if err != nil {
			return nil, err
		}
		out = append(out, pa)
	}
	return out, nil
}

func (s *SQLite) loadObservations(ctx context.Context, outputID string) ([]exchange.Observation, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, observation_local_id, title, body, category, signal_type
		 FROM observations WHERE output_id = ? ORDER BY observation_local_id`, outputID)
	if err != nil {
		return nil, fmt.Errorf("query observations: %w", err)
	}
	defer rows.Close() //nolint:errcheck // close on read-only rows; any real error surfaced during Scan

	type obsRow struct {
		uuid    string
		o       exchange.Observation
		signalT string
	}
	var raw []obsRow
	for rows.Next() {
		var or obsRow
		if err := rows.Scan(&or.uuid, &or.o.ID, &or.o.Title, &or.o.Body, &or.o.Category, &or.signalT); err != nil {
			return nil, err
		}
		if or.signalT != "" {
			or.o.SignalType = &or.signalT
		}
		raw = append(raw, or)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]exchange.Observation, 0, len(raw))
	for i := range raw {
		o := raw[i].o
		o.Citations, err = s.loadCitations(ctx, "observation", raw[i].uuid)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, nil
}

func (s *SQLite) loadMethodologyTrace(ctx context.Context, outputID string) (*exchange.MethodologyCatalog, error) {
	mc := &exchange.MethodologyCatalog{}
	err := s.db.QueryRowContext(ctx,
		`SELECT source_analyst_id, source_model, source_invoked_at, notes
		 FROM methodology_catalogs WHERE output_id = ?`, outputID).Scan(
		&mc.Source.AnalystID, &mc.Source.Model, &mc.Source.InvokedAt, &mc.Notes)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil // no catalog for this output
	}
	if err != nil {
		return nil, fmt.Errorf("query methodology_catalog: %w", err)
	}

	patternRows, err := s.db.QueryContext(ctx,
		`SELECT id, pattern_local_id, signal_group, description, pattern_text,
		        grep_precision, reasoning_depth, miss_mode,
		        false_positive_notes, hit_on_target
		 FROM methodology_patterns WHERE output_id = ? ORDER BY pattern_local_id`,
		outputID)
	if err != nil {
		return nil, fmt.Errorf("query methodology_patterns: %w", err)
	}
	defer patternRows.Close() //nolint:errcheck // close on read-only rows; any real error surfaced during Scan

	type patRow struct {
		uuid    string
		p       exchange.MethodologyPattern
		patText string
	}
	var raw []patRow
	for patternRows.Next() {
		var pr patRow
		var hit int
		var missMode string
		if err := patternRows.Scan(&pr.uuid, &pr.p.ID, &pr.p.SignalGroup, &pr.p.Description,
			&pr.patText, &pr.p.CollectorHint.GrepPrecision, &pr.p.CollectorHint.ReasoningDepth,
			&missMode, &pr.p.FalsePositiveNotes, &hit); err != nil {
			return nil, err
		}
		if pr.patText != "" {
			pr.p.Pattern = &pr.patText
		}
		pr.p.CollectorHint.MissMode = exchange.MissMode(missMode)
		switch hit {
		case 1:
			t := true
			pr.p.HitOnTarget = &t
		case 0:
			f := false
			pr.p.HitOnTarget = &f
		}
		raw = append(raw, pr)
	}
	if err := patternRows.Err(); err != nil {
		return nil, err
	}

	mc.Patterns = make([]exchange.MethodologyPattern, 0, len(raw))
	for i := range raw {
		p := raw[i].p
		p.ComposesWith, err = s.loadComposesWith(ctx, raw[i].uuid)
		if err != nil {
			return nil, err
		}
		mc.Patterns = append(mc.Patterns, p)
	}
	return mc, nil
}

func (s *SQLite) loadComposesWith(ctx context.Context, patternID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT composes_with FROM methodology_pattern_composes
		 WHERE pattern_id = ? ORDER BY composes_with`, patternID)
	if err != nil {
		return nil, fmt.Errorf("query methodology_pattern_composes: %w", err)
	}
	defer rows.Close() //nolint:errcheck // close on read-only rows; any real error surfaced during Scan
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *SQLite) loadOutputSupersedes(ctx context.Context, outputID string) ([]exchange.Supersession, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT prior_id, prior_round, kind FROM output_supersedes
		 WHERE output_id = ? ORDER BY prior_id`, outputID)
	if err != nil {
		return nil, fmt.Errorf("query output_supersedes: %w", err)
	}
	defer rows.Close() //nolint:errcheck // close on read-only rows; any real error surfaced during Scan
	var out []exchange.Supersession
	for rows.Next() {
		var sup exchange.Supersession
		var kind string
		if err := rows.Scan(&sup.PriorID, &sup.PriorRound, &kind); err != nil {
			return nil, err
		}
		sup.Kind = exchange.SupersessionKind(kind)
		out = append(out, sup)
	}
	return out, rows.Err()
}

func (s *SQLite) loadOutputReframesFrom(ctx context.Context, outputID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT text FROM output_reframes_from WHERE output_id = ? ORDER BY seq`, outputID)
	if err != nil {
		return nil, fmt.Errorf("query output_reframes_from: %w", err)
	}
	defer rows.Close() //nolint:errcheck // close on read-only rows; any real error surfaced during Scan
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
