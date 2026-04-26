package store

import "errors"

var (
	// ErrNotFound is returned when a requested entity, signal, posture, or burn does not exist.
	ErrNotFound = errors.New("not found")

	// ErrNilInput is returned when a required input parameter is nil.
	ErrNilInput = errors.New("nil input")

	// ErrOrphanedEntity is returned when a write path is asked to
	// insert a row whose entity_id does not resolve to a row in the
	// entities table. Distinct from ErrNotFound (which describes a
	// caller-initiated read miss): ErrOrphanedEntity describes a
	// rejection of a write that would have landed a row whose
	// foreign key doesn't resolve. Callers that want to programmatically
	// distinguish "you asked me to write an invalid reference" from
	// generic write failures use errors.Is.
	//
	// Defined here ahead of its first production use (Phase 5 of the
	// orphan-prevention audit; see design/orphanage.md). Callers will
	// surface it from AppendResolution and other write paths whose
	// entity_id referent must be pre-validated before INSERT.
	ErrOrphanedEntity = errors.New("referenced entity does not exist")

	// ErrSynthesisRequiresSession is returned by IngestAnalystOutput
	// when a synthesis-role output (analyst_id matching
	// exchange.SynthesistAnalystIDPrefix) is ingested without
	// WithAnalysisSession. Synthesis outputs are session-scoped
	// artifacts: the rollup query in `signatory analysis show`
	// filters by analysis_session_id, so an unlinked synthesis row
	// becomes invisible to the audit-trail surface its existence
	// was meant to populate. Pre-enforcement, agent noncompliance
	// produced 9 of 11 historical synthesis outputs in the dogfood
	// store as orphaned rows. Server-side enforcement converts the
	// silent-data-integrity-loss pattern into a loud, retryable
	// rejection. See design/dogfood-errors.md.
	//
	// The sibling invariant (synthesis outputs must carry non-empty
	// target) is enforced by the v1 schema validator
	// (exchange.Validate), which runs before this check; an empty
	// target produces a generic "target required" validation error
	// that catches the case before the synthesis-specific path is
	// reached. No need for a separate synthesis-target sentinel.
	ErrSynthesisRequiresSession = errors.New("synthesis output requires analysis_session_id")
)
