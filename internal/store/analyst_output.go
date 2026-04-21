package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sarahmaeve/signatory/internal/exchange"
	"github.com/sarahmaeve/signatory/internal/profile"
)

// IngestResult reports the outcome of ingesting an AnalystOutput.
type IngestResult struct {
	// OutputID is the UUID of the analyst_outputs row. On idempotent
	// ingest (file's content_hash matched an existing row), this is
	// the existing row's ID; on first ingest, the freshly-generated
	// UUID.
	OutputID string

	// EntityID is the UUID of the primary entity the AnalystOutput
	// is recorded under — the caller's identity when WithPrimaryTarget
	// was used, otherwise the target named in the AnalystOutput itself.
	// Existing entities are reused; missing entities are created with
	// EntityProject type as a default (callers can refine later).
	EntityID string

	// CollectedFromEntityID, when non-empty, is the UUID of the entity
	// the analysis was actually performed against — populated only
	// when WithPrimaryTarget resolved to a different identity than
	// out.Target. Empty when the primary identity matches the analyst
	// output's own target (the common case).
	CollectedFromEntityID string

	// Idempotent is true when the file's content_hash was already
	// present in analyst_outputs and no rows were written. Callers
	// can use this to surface "already ingested" UX.
	Idempotent bool
}

// ingestOpts captures variadic IngestAnalystOutput options.
type ingestOpts struct {
	// primaryTarget overrides out.Target as the entity identity used
	// for write-path indexing. When set AND it resolves to a
	// different canonical URI than out.Target, out.Target's entity
	// becomes collected_from_entity_id. When empty (default), the
	// pre-M2 behavior is preserved: out.Target is the only identity.
	primaryTarget string
}

// IngestOption configures IngestAnalystOutput. Variadic to keep the
// existing signature backwards-compatible; callers that don't care
// about M2 identity indexing pass zero options and get pre-M2
// behavior.
type IngestOption func(*ingestOpts)

// WithPrimaryTarget tells the ingest path to record the analysis
// under target as the caller's identity. When the resolved canonical
// URI differs from out.Target's canonical URI, out.Target's entity
// is recorded as collected_from_entity_id. This is the agent-facing-
// contract §3.2 mechanism: a pkg:npm/X analysis collected from
// repo:github/Y is indexed under pkg:npm/X and queryable via either
// URI.
//
// Passing the same target as out.Target is a no-op — the resulting
// row has collected_from_entity_id = NULL.
func WithPrimaryTarget(target string) IngestOption {
	return func(o *ingestOpts) { o.primaryTarget = target }
}

// IngestAnalystOutput stores an exchange.AnalystOutput in the
// SQLite tables added by migration v4. It:
//
//  1. Validates the document via exchange.Validate.
//  2. Computes a sha256 over the canonical JSON; if a matching
//     analyst_outputs.content_hash already exists, returns
//     IngestResult{OutputID: <existing>, Idempotent: true}
//     without writing anything.
//  3. Finds the entity by target URI; if absent, creates one.
//  4. Inserts the analyst_outputs row plus all the per-conclusion /
//     observation / methodology rows in one transaction.
//
// Append-only invariants are enforced by triggers (migration v4),
// so this function only ever INSERTs. Re-running an analysis on
// the same target with new content produces a new row (different
// invoked_at, different content_hash); the supersedes metadata
// captures the relationship per the schema.
//
// sourcePath is recorded on the analyst_outputs row for audit
// trail; it can be empty when ingesting from in-memory input.
//
// Options extend the function for agent-facing-contract use cases
// — chiefly WithPrimaryTarget, which decouples the caller's stated
// identity from the analyst's internal target. Pre-M2 callers pass
// no options and get the original single-identity behavior.
func (s *SQLite) IngestAnalystOutput(
	ctx context.Context,
	out *exchange.AnalystOutput,
	sourcePath string,
	opts ...IngestOption,
) (*IngestResult, error) {
	if out == nil {
		return nil, ErrNilInput
	}
	if err := out.Validate(); err != nil {
		return nil, fmt.Errorf("validate analyst output: %w", err)
	}

	var options ingestOpts
	for _, opt := range opts {
		opt(&options)
	}

	hash, err := analystOutputContentHash(out)
	if err != nil {
		return nil, fmt.Errorf("compute content hash: %w", err)
	}

	// Idempotency check: same content already ingested?
	existingID, err := s.lookupOutputByHash(ctx, hash)
	if err != nil {
		return nil, err
	}
	if existingID != "" {
		entityID, err := s.lookupOutputEntity(ctx, existingID)
		if err != nil {
			return nil, err
		}
		return &IngestResult{
			OutputID:   existingID,
			EntityID:   entityID,
			Idempotent: true,
		}, nil
	}

	// Primary identity: resolve the analyst's own target first, then
	// optionally override with the caller's identity from options.
	// Both routes go through ensureEntityForTarget so the canonical
	// URI and entity type are consistent.
	analystEntityID, err := s.ensureEntityForTarget(ctx, out.Target)
	if err != nil {
		return nil, err
	}
	entityID := analystEntityID
	var collectedFromEntityID string
	if options.primaryTarget != "" {
		primaryEntityID, err := s.ensureEntityForTarget(ctx, options.primaryTarget)
		if err != nil {
			return nil, fmt.Errorf("resolve primary target %q: %w", options.primaryTarget, err)
		}
		// Only record the resolution hop when the two identities
		// actually differ. Same-identity passthrough (pre-M2 default
		// behavior) keeps the row's collected_from column NULL.
		if primaryEntityID != analystEntityID {
			entityID = primaryEntityID
			collectedFromEntityID = analystEntityID
		}
	}

	outputID := uuid.NewString()
	now := time.Now().UTC().Format(time.RFC3339)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	if err = insertAnalystOutputRow(ctx, tx, outputID, entityID, collectedFromEntityID, out, sourcePath, hash, now); err != nil {
		return nil, err
	}
	if err = insertConclusions(ctx, tx, outputID, out.Conclusions); err != nil {
		return nil, err
	}
	if err = insertPositiveAbsences(ctx, tx, outputID, out.PositiveAbsences); err != nil {
		return nil, err
	}
	if err = insertObservations(ctx, tx, outputID, out.Observations); err != nil {
		return nil, err
	}
	if err = insertMethodologyTrace(ctx, tx, outputID, out.MethodologyTrace); err != nil {
		return nil, err
	}
	if err = insertOutputSupersedes(ctx, tx, outputID, out.Supersedes); err != nil {
		return nil, err
	}
	if err = insertOutputReframesFrom(ctx, tx, outputID, out.ReframesFrom); err != nil {
		return nil, err
	}

	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit ingest tx: %w", err)
	}

	return &IngestResult{
		OutputID:              outputID,
		EntityID:              entityID,
		CollectedFromEntityID: collectedFromEntityID,
		Idempotent:            false,
	}, nil
}

// analystOutputContentHash returns sha256(canonical JSON of out).
// "Canonical" here means whatever encoding/json produces with default
// settings — since the same AnalystOutput struct always serializes
// identically, this is sufficient for re-ingestion idempotency. We
// do NOT need a normalized canonical form (jcs / JSC) for v1; that
// would matter if hashes were exchanged across instances.
func analystOutputContentHash(out *exchange.AnalystOutput) (string, error) {
	raw, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func (s *SQLite) lookupOutputByHash(ctx context.Context, hash string) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM analyst_outputs WHERE content_hash = ?`, hash).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("lookup output by hash: %w", err)
	}
	return id, nil
}

func (s *SQLite) lookupOutputEntity(ctx context.Context, outputID string) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx,
		`SELECT entity_id FROM analyst_outputs WHERE id = ?`, outputID).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("lookup output entity: %w", err)
	}
	return id, nil
}

// ensureEntityForTarget finds an existing entity whose canonical_uri
// matches `target`, or creates a new one with EntityProject type as
// default.
//
// The target string from an AnalystOutput might be a canonical URI
// (`pkg:cargo/atuin`) or a recognizable URL (`https://github.com/owner/repo`)
// that signatory's URI scheme normalizes. We normalize URL forms to
// canonical URIs before lookup/insert so that two analyst outputs
// using different surface forms (one URL, one purl) for the same
// project resolve to the same entity row.
//
// Normalization rules currently supported:
//   - Already-canonical URIs (pkg:..., repo:..., identity:..., org:...,
//     patch:...) pass through unchanged
//   - GitHub URLs in any of the standard forms get normalized via
//     profile.NormalizeGitHubRepoInput → `repo:github/owner/name`
//   - Anything else fails ValidateCanonicalURI and returns an error
//
// Default fields for newly-created entities:
//   - type: EntityProject (most analyst outputs target a project)
//   - short_name: derived from the canonical URI (last path segment)
//   - description: empty (filled by later signal collection)
//   - ecosystem: derived from URI prefix when possible (pkg:cargo →
//     "cargo", pkg:npm → "npm", pkg:pypi → "pypi"); empty otherwise
//   - url: original target if it was an http(s) URL; empty otherwise
//
// Existing entities are returned untouched — ingestion does not
// mutate entity metadata. The signal collectors are the right path
// for entity enrichment.
func (s *SQLite) ensureEntityForTarget(ctx context.Context, target string) (string, error) {
	if target == "" {
		return "", fmt.Errorf("%w: AnalystOutput.Target is empty", ErrNilInput)
	}

	canonicalURI, err := normalizeTargetToCanonicalURI(target)
	if err != nil {
		return "", err
	}

	existing, err := s.FindEntityByURI(ctx, canonicalURI)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return "", fmt.Errorf("lookup entity: %w", err)
	}
	if existing != nil {
		return existing.ID, nil
	}

	id := uuid.NewString()
	now := time.Now().UTC()
	entity := &profile.Entity{
		ID:           id,
		CanonicalURI: canonicalURI,
		Type:         profile.EntityProject,
		ShortName:    deriveShortName(canonicalURI),
		Ecosystem:    deriveEcosystem(canonicalURI),
		URL:          deriveURL(target), // preserve original URL if http(s)
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := s.PutEntity(ctx, entity); err != nil {
		return "", fmt.Errorf("create entity for target %q: %w", target, err)
	}
	return id, nil
}

// normalizeTargetToCanonicalURI converts an AnalystOutput.Target
// into a form acceptable to profile.ValidateCanonicalURI. Canonical
// URIs pass through; recognizable URLs (currently GitHub) get
// normalized; unrecognized inputs return a wrapped error.
func normalizeTargetToCanonicalURI(target string) (string, error) {
	// Already a canonical URI? Use as-is.
	if err := profile.ValidateCanonicalURI(target); err == nil {
		return target, nil
	}
	// Looks like a GitHub URL? Normalize via the existing helper.
	lower := strings.ToLower(target)
	if strings.Contains(lower, "github.com") {
		uri, _, _, err := profile.NormalizeGitHubRepoInput(target)
		if err != nil {
			return "", fmt.Errorf("normalize GitHub target %q: %w", target, err)
		}
		return uri, nil
	}
	// Unrecognized — surface the validation error so the caller knows
	// what scheme prefix is missing.
	return "", fmt.Errorf("target %q is not a canonical URI and not a recognized URL form: %w",
		target, profile.ValidateCanonicalURI(target))
}

// deriveShortName picks a human-friendly label from a target URI.
// For a GitHub URL like "https://github.com/nvbn/thefuck" we want
// "thefuck" not "nvbn/thefuck". For a purl like "pkg:cargo/atuin"
// we want "atuin". For an arbitrary string we fall back to the
// last path segment.
func deriveShortName(target string) string {
	t := target
	// Strip purl scheme if present.
	if strings.HasPrefix(t, "pkg:") {
		if slash := strings.Index(t, "/"); slash > 0 {
			t = t[slash+1:]
		}
	}
	// Strip URL scheme if present.
	if i := strings.Index(t, "://"); i >= 0 {
		t = t[i+3:]
	}
	// Take the last path segment.
	if slash := strings.LastIndex(t, "/"); slash >= 0 {
		t = t[slash+1:]
	}
	if t == "" {
		return target
	}
	return t
}

// deriveEcosystem extracts the ecosystem token from a purl-style URI.
// Returns "" for non-purl URIs (callers can refine via signal collection
// if needed).
func deriveEcosystem(target string) string {
	if !strings.HasPrefix(target, "pkg:") {
		return ""
	}
	rest := target[len("pkg:"):]
	if slash := strings.Index(rest, "/"); slash > 0 {
		return rest[:slash]
	}
	return ""
}

// deriveURL returns the target as a URL when it has an http(s):// scheme,
// otherwise returns empty (the caller's CanonicalURI is already a URL
// in the http(s) case).
func deriveURL(target string) string {
	if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
		return target
	}
	return ""
}

func insertAnalystOutputRow(
	ctx context.Context,
	tx *sql.Tx,
	outputID, entityID, collectedFromEntityID string,
	out *exchange.AnalystOutput,
	sourcePath, contentHash, ingestedAt string,
) error {
	// Normalize empty collected_from to SQL NULL so the FK
	// constraint doesn't try to resolve it against entities(id).
	var collectedFrom interface{}
	if collectedFromEntityID != "" {
		collectedFrom = collectedFromEntityID
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO analyst_outputs
		 (id, entity_id, analyst_id, model, prompt_version, invoked_at,
		  ingested_at, round, target_commit, round_notes, source_path,
		  content_hash, collected_from_entity_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		outputID, entityID,
		out.Attribution.AnalystID, out.Attribution.Model,
		out.Attribution.PromptVersion, out.Attribution.InvokedAt,
		ingestedAt, out.Attribution.Round,
		out.TargetCommit, out.RoundNotes, sourcePath, contentHash,
		collectedFrom)
	if err != nil {
		return fmt.Errorf("insert analyst_outputs: %w", err)
	}
	return nil
}

func insertConclusions(ctx context.Context, tx *sql.Tx, outputID string, conclusions []exchange.Conclusion) error {
	for i := range conclusions {
		f := &conclusions[i]
		conclusionID := uuid.NewString()
		_, err := tx.ExecContext(ctx,
			`INSERT INTO conclusions
			 (id, output_id, conclusion_local_id, verdict, rationale,
			  severity_default, design_intent, category, signal_type,
			  answers_question)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			conclusionID, outputID, f.ID, f.Verdict, f.Rationale,
			string(f.Severity.Default), boolToInt(f.DesignIntent),
			f.Category, derefString(f.SignalType), derefString(f.AnswersQuestion))
		if err != nil {
			return fmt.Errorf("insert conclusion %q: %w", f.ID, err)
		}

		// Conditional severity overrides
		for _, ctxSeverity := range f.Severity.ByContext {
			_, err = tx.ExecContext(ctx,
				`INSERT INTO conclusion_severity_contexts
				 (conclusion_id, host_isolation, platform, value)
				 VALUES (?, ?, ?, ?)`,
				conclusionID, ctxSeverity.Context.HostIsolation,
				ctxSeverity.Context.Platform, string(ctxSeverity.Value))
			if err != nil {
				return fmt.Errorf("insert conclusion_severity_contexts for %q: %w", f.ID, err)
			}
		}

		// Supersedes
		for _, sup := range f.Supersedes {
			_, err = tx.ExecContext(ctx,
				`INSERT INTO conclusion_supersedes
				 (conclusion_id, prior_id, prior_round, kind)
				 VALUES (?, ?, ?, ?)`,
				conclusionID, sup.PriorID, sup.PriorRound, string(sup.Kind))
			if err != nil {
				return fmt.Errorf("insert conclusion_supersedes for %q: %w", f.ID, err)
			}
		}

		// Prerequisites (ordered)
		for seq, text := range f.Prerequisites {
			_, err = tx.ExecContext(ctx,
				`INSERT INTO conclusion_prerequisites (conclusion_id, seq, text)
				 VALUES (?, ?, ?)`, conclusionID, seq, text)
			if err != nil {
				return fmt.Errorf("insert conclusion_prerequisites for %q: %w", f.ID, err)
			}
		}

		// Remediation hints (ordered)
		for seq, text := range f.RemediationHints {
			_, err = tx.ExecContext(ctx,
				`INSERT INTO conclusion_remediation_hints (conclusion_id, seq, text)
				 VALUES (?, ?, ?)`, conclusionID, seq, text)
			if err != nil {
				return fmt.Errorf("insert conclusion_remediation_hints for %q: %w", f.ID, err)
			}
		}

		// Related conclusions
		for _, rel := range f.RelatedConclusions {
			_, err = tx.ExecContext(ctx,
				`INSERT INTO conclusion_related (conclusion_id, related_id)
				 VALUES (?, ?)`, conclusionID, rel)
			if err != nil {
				return fmt.Errorf("insert conclusion_related for %q: %w", f.ID, err)
			}
		}

		// Citations attach via polymorphic FK
		if err = insertCitations(ctx, tx, "conclusion", conclusionID, f.Citations); err != nil {
			return err
		}
	}
	return nil
}

func insertPositiveAbsences(
	ctx context.Context, tx *sql.Tx, outputID string,
	absences []exchange.PositiveAbsence,
) error {
	for i := range absences {
		pa := &absences[i]
		paID := uuid.NewString()
		_, err := tx.ExecContext(ctx,
			`INSERT INTO positive_absences
			 (id, output_id, pattern_checked, description, confidence, pattern_ref)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			paID, outputID, pa.PatternChecked, pa.Description,
			string(pa.Confidence), derefString(pa.PatternRef))
		if err != nil {
			return fmt.Errorf("insert positive_absence: %w", err)
		}
		if err = insertCitations(ctx, tx, "positive_absence", paID, pa.Citations); err != nil {
			return err
		}
	}
	return nil
}

func insertObservations(
	ctx context.Context, tx *sql.Tx, outputID string,
	observations []exchange.Observation,
) error {
	for i := range observations {
		o := &observations[i]
		obsID := uuid.NewString()
		_, err := tx.ExecContext(ctx,
			`INSERT INTO observations
			 (id, output_id, observation_local_id, title, body, category, signal_type)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			obsID, outputID, o.ID, o.Title, o.Body, o.Category,
			derefString(o.SignalType))
		if err != nil {
			return fmt.Errorf("insert observation %q: %w", o.ID, err)
		}
		if err = insertCitations(ctx, tx, "observation", obsID, o.Citations); err != nil {
			return err
		}
	}
	return nil
}

func insertMethodologyTrace(
	ctx context.Context, tx *sql.Tx, outputID string,
	mc *exchange.MethodologyCatalog,
) error {
	if mc == nil {
		return nil
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO methodology_catalogs
		 (output_id, source_analyst_id, source_model, source_invoked_at, notes)
		 VALUES (?, ?, ?, ?, ?)`,
		outputID, mc.Source.AnalystID, mc.Source.Model,
		mc.Source.InvokedAt, mc.Notes)
	if err != nil {
		return fmt.Errorf("insert methodology_catalog: %w", err)
	}
	for i := range mc.Patterns {
		p := &mc.Patterns[i]
		patID := uuid.NewString()
		_, err = tx.ExecContext(ctx,
			`INSERT INTO methodology_patterns
			 (id, output_id, pattern_local_id, signal_group, description,
			  pattern_text, grep_precision, reasoning_depth, miss_mode,
			  false_positive_notes, hit_on_target)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			patID, outputID, p.ID, p.SignalGroup, p.Description,
			derefString(p.Pattern),
			string(p.CollectorHint.GrepPrecision),
			string(p.CollectorHint.ReasoningDepth),
			string(p.CollectorHint.MissMode),
			p.FalsePositiveNotes,
			tristateBool(p.HitOnTarget))
		if err != nil {
			return fmt.Errorf("insert methodology_pattern %q: %w", p.ID, err)
		}
		for _, comp := range p.ComposesWith {
			_, err = tx.ExecContext(ctx,
				`INSERT INTO methodology_pattern_composes (pattern_id, composes_with)
				 VALUES (?, ?)`, patID, comp)
			if err != nil {
				return fmt.Errorf("insert methodology_pattern_composes for %q: %w", p.ID, err)
			}
		}
	}
	return nil
}

func insertOutputSupersedes(
	ctx context.Context, tx *sql.Tx, outputID string,
	supersedes []exchange.Supersession,
) error {
	for _, sup := range supersedes {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO output_supersedes (output_id, prior_id, prior_round, kind)
			 VALUES (?, ?, ?, ?)`,
			outputID, sup.PriorID, sup.PriorRound, string(sup.Kind))
		if err != nil {
			return fmt.Errorf("insert output_supersedes: %w", err)
		}
	}
	return nil
}

func insertOutputReframesFrom(
	ctx context.Context, tx *sql.Tx, outputID string, reframes []string,
) error {
	for seq, text := range reframes {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO output_reframes_from (output_id, seq, text)
			 VALUES (?, ?, ?)`, outputID, seq, text)
		if err != nil {
			return fmt.Errorf("insert output_reframes_from: %w", err)
		}
	}
	return nil
}

// insertCitations inserts a slice of Citations attached to a parent
// row of a given kind. parentKind ∈ {"conclusion", "positive_absence",
// "observation", "methodology_pattern"}.
//
// Citation's nullable LineStart/LineEnd are stored as -1 sentinel
// values per the schema convention (SQLite has no real nullable INT
// in this codebase's pattern); Citation's Scope is stored as the
// scope_kind/scope_path columns, empty when absent.
func insertCitations(
	ctx context.Context, tx *sql.Tx,
	parentKind, parentID string,
	citations []exchange.Citation,
) error {
	for seq := range citations {
		c := &citations[seq]
		var lineStart, lineEnd = -1, -1
		if c.LineStart != nil {
			lineStart = *c.LineStart
		}
		if c.LineEnd != nil {
			lineEnd = *c.LineEnd
		}
		var scopeKind, scopePath string
		if c.Scope != nil {
			scopeKind = c.Scope.Kind
			scopePath = c.Scope.Path
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO citations
			 (id, parent_kind, parent_id, seq, path, line_start, line_end,
			  scope_kind, scope_path, commit_sha, quoted)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			uuid.NewString(), parentKind, parentID, seq,
			c.Path, lineStart, lineEnd, scopeKind, scopePath,
			derefString(c.CommitSHA), derefString(c.Quoted))
		if err != nil {
			return fmt.Errorf("insert citation %d for %s/%s: %w",
				seq, parentKind, parentID, err)
		}
	}
	return nil
}

// derefString returns *s if non-nil, else "". Used to flatten
// optional-string fields from the exchange types into NOT-NULL
// TEXT columns.
func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// boolToInt maps Go bool to SQLite's INTEGER (0 or 1).
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// tristateBool maps Go *bool to SQLite's INTEGER with -1 sentinel
// for nil. Per the schema convention for nullable bools that
// SQLite doesn't natively support: -1 = null, 0 = false, 1 = true.
func tristateBool(b *bool) int {
	if b == nil {
		return -1
	}
	if *b {
		return 1
	}
	return 0
}
