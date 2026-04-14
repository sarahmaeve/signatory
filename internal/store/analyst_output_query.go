package store

import (
	"context"
	"database/sql"
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
	OutputID             string
	EntityID             string
	EntityURI            string // canonical_uri from the joined entity row
	AnalystID            string
	Model                string
	PromptVersion        string
	InvokedAt            string // RFC3339 strings preserved verbatim
	IngestedAt           string
	Round                int
	TargetCommit         string
	SourcePath           string
	ContentHash          string
	FindingsCount        int
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
func (s *SQLite) ListAnalystOutputs(ctx context.Context, filter AnalystOutputFilter) ([]AnalystOutputSummary, error) {
	// Resolve EntityURI → EntityID if specified. Done before SQL
	// construction so the WHERE clause stays simple.
	entityID := filter.EntityID
	if entityID == "" && filter.EntityURI != "" {
		ent, err := s.FindEntityByURI(ctx, filter.EntityURI)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				// No entity = no outputs. Return empty rather than an error
				// so callers can treat "unknown target" as "no analyses yet."
				return nil, nil
			}
			return nil, fmt.Errorf("resolve entity URI %q: %w", filter.EntityURI, err)
		}
		entityID = ent.ID
	}

	var clauses []string
	var args []any
	if entityID != "" {
		clauses = append(clauses, "ao.entity_id = ?")
		args = append(args, entityID)
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
			ao.analyst_id, ao.model, ao.prompt_version,
			ao.invoked_at, ao.ingested_at, ao.round,
			ao.target_commit, ao.source_path, ao.content_hash,
			(SELECT COUNT(*) FROM findings           WHERE output_id = ao.id) AS findings_count,
			(SELECT COUNT(*) FROM positive_absences  WHERE output_id = ao.id) AS absence_count,
			(SELECT COUNT(*) FROM observations       WHERE output_id = ao.id) AS obs_count,
			(SELECT COUNT(*) FROM methodology_patterns WHERE output_id = ao.id) AS pat_count
		FROM analyst_outputs ao
		INNER JOIN entities e ON e.id = ao.entity_id
		%s
		ORDER BY ao.ingested_at DESC
		%s`, whereSQL, limitSQL)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list analyst_outputs: %w", err)
	}
	defer rows.Close()

	var out []AnalystOutputSummary
	for rows.Next() {
		var s AnalystOutputSummary
		if err := rows.Scan(
			&s.OutputID, &s.EntityID, &s.EntityURI,
			&s.AnalystID, &s.Model, &s.PromptVersion,
			&s.InvokedAt, &s.IngestedAt, &s.Round,
			&s.TargetCommit, &s.SourcePath, &s.ContentHash,
			&s.FindingsCount, &s.PositiveAbsenceCount,
			&s.ObservationCount, &s.PatternCount,
		); err != nil {
			return nil, fmt.Errorf("scan analyst_output row: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// FindingSummary is the lightweight read-row for findings listings.
// Verdict is included because it's the load-bearing identifier for
// a finding when a human is scanning a list; rationale stays out
// (per the same logic as format-check --summary).
type FindingSummary struct {
	OutputID        string
	EntityID        string
	EntityURI       string
	AnalystID       string
	IngestedAt      string
	FindingID       string // UUID
	FindingLocalID  string // "F001"
	Verdict         string
	SeverityDefault string
	DesignIntent    bool
	Category        string
	SignalType      string // "" if absent
	CitationCount   int
	HasSupersedes   bool
	BySupersedesIDs []string // populated only when filter.IncludeSupersedes is true
}

// FindingFilter narrows ListFindings results.
type FindingFilter struct {
	EntityID   string
	EntityURI  string
	AnalystID  string
	SignalType string

	// SeverityIn limits to findings whose severity_default is in
	// the provided set. Empty = all severities.
	SeverityIn []exchange.SeverityValue

	// DesignIntentOnly limits to findings with design_intent = true.
	// Useful for "what does this project deliberately do that we
	// should know about?" queries.
	DesignIntentOnly bool

	Limit int
}

// ListFindings returns findings across one or more analyst outputs,
// newest first by the parent output's ingested_at. Each row joins
// to entities for the canonical_uri convenience field and to
// analyst_outputs for analyst_id + ingested_at attribution.
func (s *SQLite) ListFindings(ctx context.Context, filter FindingFilter) ([]FindingSummary, error) {
	entityID := filter.EntityID
	if entityID == "" && filter.EntityURI != "" {
		ent, err := s.FindEntityByURI(ctx, filter.EntityURI)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil, nil
			}
			return nil, fmt.Errorf("resolve entity URI %q: %w", filter.EntityURI, err)
		}
		entityID = ent.ID
	}

	var clauses []string
	var args []any
	if entityID != "" {
		clauses = append(clauses, "ao.entity_id = ?")
		args = append(args, entityID)
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
			f.id, f.finding_local_id, f.verdict, f.severity_default,
			f.design_intent, f.category, f.signal_type,
			(SELECT COUNT(*) FROM citations WHERE parent_kind = 'finding' AND parent_id = f.id) AS cite_count,
			(SELECT COUNT(*) FROM finding_supersedes WHERE finding_id = f.id) > 0 AS has_supersedes
		FROM findings f
		INNER JOIN analyst_outputs ao ON ao.id = f.output_id
		INNER JOIN entities e ON e.id = ao.entity_id
		%s
		ORDER BY ao.ingested_at DESC, f.finding_local_id ASC
		%s`, whereSQL, limitSQL)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list findings: %w", err)
	}
	defer rows.Close()

	var out []FindingSummary
	for rows.Next() {
		var f FindingSummary
		var designIntent int
		if err := rows.Scan(
			&f.OutputID, &f.EntityID, &f.EntityURI,
			&f.AnalystID, &f.IngestedAt,
			&f.FindingID, &f.FindingLocalID, &f.Verdict,
			&f.SeverityDefault, &designIntent, &f.Category, &f.SignalType,
			&f.CitationCount, &f.HasSupersedes,
		); err != nil {
			return nil, fmt.Errorf("scan finding row: %w", err)
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
func (s *SQLite) ListMethodologyPatterns(ctx context.Context, filter MethodologyPatternFilter) ([]MethodologyPatternSummary, error) {
	entityID := filter.EntityID
	if entityID == "" && filter.EntityURI != "" {
		ent, err := s.FindEntityByURI(ctx, filter.EntityURI)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil, nil
			}
			return nil, fmt.Errorf("resolve entity URI %q: %w", filter.EntityURI, err)
		}
		entityID = ent.ID
	}

	var clauses []string
	var args []any
	if entityID != "" {
		clauses = append(clauses, "ao.entity_id = ?")
		args = append(args, entityID)
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
	defer rows.Close()

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
	var ignoredEntityID, ignoredHash, ignoredIngestedAt, ignoredSourcePath string
	err := s.db.QueryRowContext(ctx,
		`SELECT entity_id, analyst_id, model, prompt_version, invoked_at,
		        ingested_at, round, target_commit, round_notes, source_path,
		        content_hash
		 FROM analyst_outputs WHERE id = ?`, outputID).Scan(
		&ignoredEntityID,
		&out.Attribution.AnalystID, &out.Attribution.Model,
		&out.Attribution.PromptVersion, &out.Attribution.InvokedAt,
		&ignoredIngestedAt, &out.Attribution.Round,
		&out.TargetCommit, &out.RoundNotes, &ignoredSourcePath, &ignoredHash,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load analyst_output: %w", err)
	}
	// Resolve target via the joined entity's canonical_uri.
	var targetURI string
	if err := s.db.QueryRowContext(ctx,
		`SELECT canonical_uri FROM entities WHERE id = ?`,
		ignoredEntityID).Scan(&targetURI); err != nil {
		return nil, fmt.Errorf("load target URI: %w", err)
	}
	out.Target = targetURI

	out.Findings, err = s.loadFindings(ctx, outputID)
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

func (s *SQLite) loadFindings(ctx context.Context, outputID string) ([]exchange.Finding, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, finding_local_id, verdict, rationale,
		        severity_default, design_intent, category,
		        signal_type, answers_question
		 FROM findings WHERE output_id = ? ORDER BY finding_local_id`,
		outputID)
	if err != nil {
		return nil, fmt.Errorf("query findings: %w", err)
	}
	defer rows.Close()

	type findingRow struct {
		uuid    string
		f       exchange.Finding
		signalT string
		answers string
	}
	var raw []findingRow
	for rows.Next() {
		var fr findingRow
		var designIntent int
		if err := rows.Scan(
			&fr.uuid, &fr.f.ID, &fr.f.Verdict, &fr.f.Rationale,
			&fr.f.Severity.Default, &designIntent, &fr.f.Category,
			&fr.signalT, &fr.answers,
		); err != nil {
			return nil, err
		}
		fr.f.DesignIntent = designIntent != 0
		if fr.signalT != "" {
			fr.f.SignalType = &fr.signalT
		}
		if fr.answers != "" {
			fr.f.AnswersQuestion = &fr.answers
		}
		raw = append(raw, fr)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Walk children for each finding.
	out := make([]exchange.Finding, 0, len(raw))
	for i := range raw {
		f := raw[i].f
		f.Severity.ByContext, err = s.loadFindingSeverityContexts(ctx, raw[i].uuid)
		if err != nil {
			return nil, err
		}
		f.Supersedes, err = s.loadFindingSupersedes(ctx, raw[i].uuid)
		if err != nil {
			return nil, err
		}
		f.Prerequisites, err = s.loadOrderedTexts(ctx, "finding_prerequisites", "finding_id", raw[i].uuid)
		if err != nil {
			return nil, err
		}
		f.RemediationHints, err = s.loadOrderedTexts(ctx, "finding_remediation_hints", "finding_id", raw[i].uuid)
		if err != nil {
			return nil, err
		}
		f.RelatedFindings, err = s.loadFindingRelated(ctx, raw[i].uuid)
		if err != nil {
			return nil, err
		}
		f.Citations, err = s.loadCitations(ctx, "finding", raw[i].uuid)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}

func (s *SQLite) loadFindingSeverityContexts(ctx context.Context, findingID string) ([]exchange.ContextualSeverity, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT host_isolation, platform, value
		 FROM finding_severity_contexts WHERE finding_id = ?
		 ORDER BY host_isolation, platform`, findingID)
	if err != nil {
		return nil, fmt.Errorf("query finding_severity_contexts: %w", err)
	}
	defer rows.Close()
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

func (s *SQLite) loadFindingSupersedes(ctx context.Context, findingID string) ([]exchange.Supersession, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT prior_id, prior_round, kind FROM finding_supersedes
		 WHERE finding_id = ? ORDER BY prior_id`, findingID)
	if err != nil {
		return nil, fmt.Errorf("query finding_supersedes: %w", err)
	}
	defer rows.Close()
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

// loadOrderedTexts handles finding_prerequisites and finding_remediation_hints,
// which share schema (finding_id, seq, text). Generalizes to keep the
// per-loader code small.
func (s *SQLite) loadOrderedTexts(ctx context.Context, table, parentCol, parentID string) ([]string, error) {
	q := fmt.Sprintf(`SELECT text FROM %s WHERE %s = ? ORDER BY seq`, table, parentCol)
	rows, err := s.db.QueryContext(ctx, q, parentID)
	if err != nil {
		return nil, fmt.Errorf("query %s: %w", table, err)
	}
	defer rows.Close()
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

func (s *SQLite) loadFindingRelated(ctx context.Context, findingID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT related_id FROM finding_related WHERE finding_id = ? ORDER BY related_id`,
		findingID)
	if err != nil {
		return nil, fmt.Errorf("query finding_related: %w", err)
	}
	defer rows.Close()
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
	defer rows.Close()
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
	defer rows.Close()

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
	defer rows.Close()

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
	defer patternRows.Close()

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
	defer rows.Close()
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
	defer rows.Close()
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
	defer rows.Close()
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
