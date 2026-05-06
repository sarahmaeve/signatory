package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
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

	// analysisSessionID links this output to a prior-created
	// analysis_sessions row. Empty (default) leaves the column NULL
	// — outputs ingested outside a session still land; the
	// session linkage is purely additive.
	analysisSessionID string
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

// WithAnalysisSession stamps analyst_outputs.analysis_session_id
// on the ingested row. The caller is responsible for ensuring
// sessionID names an existing analysis_sessions row — the FK
// constraint (ON DELETE SET NULL) surfaces a clear error otherwise.
//
// Set at INSERT time rather than UPDATE'd after the fact so we
// don't need to suspend the v3 append-only trigger. Soft-cut
// contract: ingests without this option still succeed; the
// column simply stays NULL.
func WithAnalysisSession(sessionID string) IngestOption {
	return func(o *ingestOpts) { o.analysisSessionID = sessionID }
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

	// Synthesis-role outputs are session-scoped artifacts: the rollup
	// query in `signatory analysis show` filters by
	// analysis_session_id, so an unlinked synthesis row is invisible
	// to the audit-trail surface its existence was meant to populate.
	// Agent compliance with the SESSION_INSTRUCTION block in the
	// handoff is observably inconsistent — 9 of 11 historical
	// synthesis outputs in the dogfood store were orphaned by
	// dropped fields. Loud rejection here lets the agent retry with
	// the corrected payload (the MCP boundary maps the sentinel to
	// CodeSchemaViolation with the missing field named).
	//
	// The sibling target-required invariant is enforced by
	// out.Validate() above; an empty target never reaches this point.
	if exchange.IsSynthesistRole(out.Attribution.AnalystID) && options.analysisSessionID == "" {
		return nil, ErrSynthesisRequiresSession
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
	analystResolution, err := s.ensureEntityForTarget(ctx, out.Target)
	if err != nil {
		return nil, err
	}
	// The row's entity_id is whichever identity we ultimately index
	// under (see M2 collected_from semantics below); the row's target
	// columns always reflect the ANALYST'S caller-supplied target so
	// "what was this output analyzing?" stays answerable from the row
	// alone.
	rowResolution := analystResolution
	var collectedFromEntityID string
	if options.primaryTarget != "" {
		primaryResolution, err := s.ensureEntityForTarget(ctx, options.primaryTarget)
		if err != nil {
			return nil, fmt.Errorf("resolve primary target %q: %w", options.primaryTarget, err)
		}
		// Only record the resolution hop when the two identities
		// actually differ. Same-identity passthrough (pre-M2 default
		// behavior) keeps the row's collected_from column NULL.
		if primaryResolution.EntityID != analystResolution.EntityID {
			rowResolution = primaryResolution
			collectedFromEntityID = analystResolution.EntityID
		}
	}

	outputID := uuid.NewString()
	now := time.Now().UTC().Format(time.RFC3339)

	// Server-stamp invoked_at from the wall clock. The validator
	// rejects caller-supplied attribution.invoked_at (see
	// AgentAttribution.validate for rationale: agents have no
	// reliable wall-clock API and routinely hallucinate timestamps),
	// so by this point out.Attribution.InvokedAt is empty and we're
	// filling in fresh ground rather than overwriting a guess.
	//
	// We pass `now` explicitly to insertAnalystOutputRow rather than
	// mutating out.Attribution.InvokedAt because the caller's *out
	// pointer is shared across calls — mutating it would mean a
	// second ingest of the same out (e.g. an idempotency test, or a
	// retry path) would arrive at the validator with a non-empty
	// InvokedAt and be rejected. Pass-through preserves the
	// caller's view of their own struct.
	//
	// Order matters: this `now` is captured AFTER
	// analystOutputContentHash (computed above) so the hash is
	// taken from the caller's exact payload. Two ingests of the
	// same payload at different times still hash identically and
	// trigger the idempotency short-circuit; if we hashed AFTER
	// stamping, identical payloads at different times would
	// re-ingest as fresh rows.
	//
	// model is left empty for now — half 2 will backfill it by
	// joining to OTEL gen_ai.request.model spans via
	// analysis_session_id. Until then, the show-synthesis renderer
	// elides the "(model)" suffix when empty.

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // rollback-after-commit is a no-op

	if err = insertAnalystOutputRow(ctx, tx, outputID, rowResolution, collectedFromEntityID, options.analysisSessionID, out, sourcePath, hash, now, now); err != nil {
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
		EntityID:              rowResolution.EntityID,
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

// resolvedTarget is the ingest-side split of a caller-supplied
// target into (entity URI, original canonical URI, version scope).
// The three forms answer different questions and live in different
// columns:
//
//   - EntityURI is the UNVERSIONED canonical URI — what the entity
//     row is keyed under (Plan-A canonicalization). `pkg:npm/X@V`
//     resolves to entity at `pkg:npm/X`; `repo:github/O/R@v1` to
//     entity at `repo:github/O/R`.
//
//   - FullURI is the caller's target as first normalized — includes
//     the @V if present. Persisted on analyst_outputs.target for
//     audit (answer to "what did the analyst say it was analyzing?").
//
//   - Version is the @V suffix alone, or "" when absent. Persisted
//     on analyst_outputs.target_version for cheap version-scoped
//     queries without re-parsing.
//
// Under the pre-v10 model these three collapsed to a single URI
// (FullURI == EntityURI); post-v10 they diverge for versioned
// targets so the entity row can be shared across versions.
type resolvedTarget struct {
	EntityURI string
	FullURI   string
	Version   string
}

// resolveIngestTarget normalizes a caller-supplied target string
// into the three forms above. Pure function — no I/O, no store
// access; the caller decides whether to lookup-or-create the entity
// at EntityURI. Exported via the package-internal ensureEntityForTarget.
//
// Error shape matches ensureEntityForTarget's previous contract:
// empty input and malformed targets both surface early so the ingest
// caller can fail fast.
func resolveIngestTarget(target string) (*resolvedTarget, error) {
	if target == "" {
		return nil, fmt.Errorf("%w: AnalystOutput.Target is empty", ErrNilInput)
	}
	full, err := normalizeTargetToCanonicalURI(target)
	if err != nil {
		return nil, err
	}
	base, version := profile.SplitURIVersion(full)
	return &resolvedTarget{
		EntityURI: base,
		FullURI:   full,
		Version:   version,
	}, nil
}

// ensureEntityForTarget finds or creates the entity for an ingest
// target, returning the resolved target triple so the caller can
// persist the caller-supplied URI + version on the analyst_outputs
// row while the entity row lives at the unversioned URI.
//
// Plan-A canonicalization: versioned purl/repo URIs (`pkg:X@V`,
// `repo:O/R@v1`) are split; the entity is keyed by the UNVERSIONED
// base, and the @V goes on the analyst_outputs row's version column.
// This keeps analyses across different versions of the same package
// on a single entity row (so posture/burn/summary queries don't
// fragment by version) and aligns the ingest path with the pre-
// existing posture-set behavior (see normalizeTargetForPosture in
// cmd/signatory/posture.go).
//
// The target string from an AnalystOutput might be a canonical URI
// (`pkg:cargo/atuin`), a recognizable URL (`https://github.com/owner/repo`),
// or a versioned form (`pkg:golang/github.com/stretchr/testify@v1.11.1`).
// Surface-form normalization (URL → purl, percent-decode scope
// markers, etc.) is handled by normalizeTargetToCanonicalURI;
// version stripping is handled by profile.SplitURIVersion.
//
// Default fields for newly-created entities:
//   - type: derived from the URI scheme via profile.EntityTypeForURI
//     (pkg: → package, repo: → project, identity: → identity, org: →
//     org, patch: → patch). Previously hardcoded to EntityProject,
//     which mistyped every pkg: ingest as a project and broke the
//     downstream Type-gates in cmd/signatory/analyze.go.
//   - short_name: derived from the UNVERSIONED canonical URI — so
//     a versioned ingest produces "testify", not "testify@v1.11.1"
//   - ecosystem: derived from URI prefix (pkg:cargo → "cargo", etc.)
//   - url: original http(s) URL target, else empty
//
// Existing entities are returned untouched — ingestion does not
// mutate entity metadata. Signal collectors are the enrichment path.
func (s *SQLite) ensureEntityForTarget(ctx context.Context, target string) (*entityResolution, error) {
	resolved, err := resolveIngestTarget(target)
	if err != nil {
		return nil, err
	}

	existing, err := s.FindEntityByURI(ctx, resolved.EntityURI)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, fmt.Errorf("lookup entity: %w", err)
	}
	if existing != nil {
		return &entityResolution{
			EntityID:       existing.ID,
			resolvedTarget: *resolved,
		}, nil
	}

	id := uuid.NewString()
	now := time.Now().UTC()
	entity := &profile.Entity{
		ID:           id,
		CanonicalURI: resolved.EntityURI,
		// Type derives from the URI scheme — pkg: → package, repo: →
		// project, identity: → identity, org: → org, patch: → patch.
		// Previously hardcoded to EntityProject, which silently
		// mistyped every pkg: ingest and tripped the Type-gated
		// resolver triggers in analyze.go (npm/pypi). See
		// TestIngest_EntityType_MatchesURIScheme for the regression.
		Type: profile.EntityTypeForURI(resolved.EntityURI),
		// Derive the short name from the unversioned base so
		// "pkg:npm/X@1.2.3" produces "X", not "X@1.2.3". Matters
		// for CLI rendering — `signatory summary` etc. pull
		// short_name verbatim, and a version-glued short name was
		// the ugly-render symptom in the 2026-04-22 dogfood.
		ShortName: deriveShortName(resolved.EntityURI),
		Ecosystem: deriveEcosystem(resolved.EntityURI),
		URL:       deriveURL(target), // preserve original URL if http(s)
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.PutEntity(ctx, entity); err != nil {
		return nil, fmt.Errorf("create entity for target %q: %w", target, err)
	}
	return &entityResolution{
		EntityID:       id,
		resolvedTarget: *resolved,
	}, nil
}

// entityResolution bundles the entity-id result with the resolved
// target triple that the caller needs to persist on the
// analyst_outputs row. Kept private to the store package — callers
// outside get the URI-normalization behavior via
// IngestAnalystOutput's contract, not by poking at this struct.
type entityResolution struct {
	EntityID string
	resolvedTarget
}

// normalizeTargetToCanonicalURI converts an ingest-side target
// string into the canonical URI form signatory stores. Accepts:
//
//   - Canonical URIs (pkg:/repo:/identity:/org:/patch:)
//   - pkg URIs with percent-encoded scope/version markers
//     (e.g. pkg:npm/%40stripe/foo → pkg:npm/@stripe/foo)
//   - npmjs.com package URLs (https://www.npmjs.com/package/…)
//   - GitHub URLs, SCP-form URLs, and owner/repo shorthand
//   - any other form profile.ResolveTarget accepts
//
// Delegates to ResolveTarget so ingest-side URI handling stays in
// sync with CLI-side handling (agent-facing-contract P1: one target
// grammar, everywhere). The percent-decode pass handles the
// 2026-04-21 dogfood case where an analyst transcribed a scoped
// npm package from an npmjs.com URL and carried the %40 encoding
// into the pkg URI, creating an orphan entity.
func normalizeTargetToCanonicalURI(target string) (string, error) {
	resolved, err := profile.ResolveTarget(target)
	if err != nil {
		return "", fmt.Errorf(
			"target %q is not a canonical URI, npmjs.com URL, or GitHub repo form: %w",
			target, err)
	}

	// pkg URIs may arrive with percent-encoded `@` markers when an
	// analyst mirrored an npmjs.com URL (where scope and version
	// separators are percent-encoded for URL safety) into the purl
	// grammar (where they're literal). Decode the body so
	// pkg:npm/%40scope/name canonicalizes to pkg:npm/@scope/name,
	// keeping both analysts' outputs on the same entity row.
	//
	// Scoped to pkg URIs because other schemes don't use `@` as a
	// grammar marker — decoding them blind would risk unintended
	// transforms.
	if strings.HasPrefix(resolved.CanonicalURI, "pkg:") {
		body := resolved.CanonicalURI[len("pkg:"):]
		decoded, decErr := url.PathUnescape(body)
		if decErr != nil {
			return "", fmt.Errorf(
				"pkg URI %q contains invalid percent-encoding: %w",
				resolved.CanonicalURI, decErr)
		}
		if decoded != body {
			canonical := "pkg:" + decoded
			// Re-validate the decoded form — a percent-decode can
			// in principle reveal control chars or other invalid
			// bytes that the encoded form hid.
			if verr := profile.ValidateCanonicalURI(canonical); verr != nil {
				return "", fmt.Errorf(
					"pkg URI normalization produced invalid form %q: %w",
					canonical, verr)
			}
			return canonical, nil
		}
	}
	return resolved.CanonicalURI, nil
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
	outputID string,
	resolution *entityResolution,
	collectedFromEntityID string,
	analysisSessionID string,
	out *exchange.AnalystOutput,
	sourcePath, contentHash, invokedAt, ingestedAt string,
) error {
	// Normalize empty FK-optional strings to SQL NULL so the FK
	// constraints don't try to resolve "".
	collectedFrom := nullableString(collectedFromEntityID)
	sessionArg := nullableString(analysisSessionID)

	// M6a: if the output carries a SynthesisSupplement, serialize
	// the full supplement to JSON and denormalize the proposed
	// tier + version_scope into their own columns for cheap query
	// access. Validator has already enforced that supplement is
	// present iff analyst_id is a synthesis role.
	supplementJSON, proposedTier, proposedVersionScope, err := synthesisSupplementColumns(out.SynthesisSupplement)
	if err != nil {
		return err
	}

	// v10: target + target_version capture the caller's identity
	// independent of the entity row (which may now be keyed at an
	// unversioned URI under Plan-A canonicalization). The row is the
	// one place "what did this analyst claim to be analyzing" can be
	// answered after the fact — entity rows are shared across versions.
	// v11: analysis_session_id links the output to the /analyze run
	// that produced it (nullable — ingests without a session still land).
	_, err = tx.ExecContext(ctx,
		`INSERT INTO analyst_outputs
		 (id, entity_id, analyst_id, model, prompt_version, invoked_at,
		  ingested_at, round, target_commit, round_notes, source_path,
		  content_hash, collected_from_entity_id,
		  synthesis_supplement_json, proposed_tier, proposed_version_scope,
		  target, target_version, analysis_session_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		outputID, resolution.EntityID,
		out.Attribution.AnalystID, out.Attribution.Model,
		out.Attribution.PromptVersion, invokedAt,
		ingestedAt, out.Attribution.Round,
		out.TargetCommit, out.RoundNotes, sourcePath, contentHash,
		collectedFrom,
		supplementJSON, proposedTier, proposedVersionScope,
		resolution.FullURI, resolution.Version, sessionArg)
	if err != nil {
		return fmt.Errorf("insert analyst_outputs: %w", err)
	}
	return nil
}

// synthesisSupplementColumns serializes a SynthesisSupplement into the
// three storage columns: the opaque JSON blob plus the two
// denormalized fields. nil supplement → three NULL values (the common
// case for non-synthesist outputs).
func synthesisSupplementColumns(supplement *exchange.SynthesisSupplement) (
	supplementJSON, proposedTier, proposedVersionScope any,
	err error,
) {
	if supplement == nil {
		return nil, nil, nil, nil
	}
	raw, err := json.Marshal(supplement)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("marshal synthesis supplement: %w", err)
	}
	supplementJSON = string(raw)
	proposedTier = supplement.ProposedPosture.Tier
	if supplement.ProposedPosture.VersionScope != "" {
		proposedVersionScope = supplement.ProposedPosture.VersionScope
	}
	return supplementJSON, proposedTier, proposedVersionScope, nil
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
// "observation"} — matches the CHECK constraint installed by
// migration v9. Any other value is rejected at the SQLite layer.
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
