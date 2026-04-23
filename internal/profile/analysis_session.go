package profile

import "time"

// AnalysisSessionStatus is the lifecycle marker on an analysis run.
// Stored verbatim in analysis_sessions.status; a migration-level
// CHECK constraint pins the column to these four values.
type AnalysisSessionStatus string

const (
	AnalysisSessionInProgress AnalysisSessionStatus = "in_progress"
	AnalysisSessionCompleted  AnalysisSessionStatus = "completed"
	AnalysisSessionFailed     AnalysisSessionStatus = "failed"
	AnalysisSessionPartial    AnalysisSessionStatus = "partial"
)

// IsTerminal reports whether a status represents a closed session.
// Terminal sessions cannot be reopened; CloseAnalysisSession enforces
// this at the store layer.
func (s AnalysisSessionStatus) IsTerminal() bool {
	switch s {
	case AnalysisSessionCompleted, AnalysisSessionFailed, AnalysisSessionPartial:
		return true
	default:
		return false
	}
}

// AnalysisSession is the durable audit identity for one invocation
// of the /analyze pipeline (or a manual equivalent): target, who
// started it, timing, and how it ended.
//
// Session ID is the audit identity used across analyst_outputs
// (via analyst_outputs.analysis_session_id). It is distinct from
// PipelineSessionID, which is the transport-layer pointer to the
// pipeline relay's own session — nullable, cleanable independently.
// Design rationale: design/phase3-plan.md.
type AnalysisSession struct {
	// ID is the audit identity — a fresh UUID per session, stable
	// for the life of the row. Analysts pass it into
	// signatory_ingest_analysis so their output rows join back here.
	ID string `json:"id"`

	// EntityID is the entity this session targets — the Plan-A
	// unversioned canonical-URI entity, regardless of whether the
	// caller supplied @V. Joins with entities.id.
	EntityID string `json:"entity_id"`

	// TargetURI preserves the caller's original target string
	// (possibly with @V). Purely for audit.
	TargetURI string `json:"target_uri"`

	// TargetVersion is the @V split off the original target, or
	// "" for unversioned runs. Matches analyst_outputs.target_version.
	TargetVersion string `json:"target_version,omitempty"`

	// InvokedBy is the team identity actor (resolved via
	// identity.Current) at begin-time.
	InvokedBy string `json:"invoked_by"`

	// PipelineSessionID is the ephemeral pipeline-service session
	// this run used, if any. Empty for runs that skipped the relay.
	// Not a FK in the DB — pipeline sessions live independently.
	PipelineSessionID string `json:"pipeline_session_id,omitempty"`

	// ExpectedAnalysts is the list of analyst role IDs the skill
	// dispatched. Used by `analyze show` to render expected-vs-landed.
	// Stored as comma-joined in the DB; the store layer handles
	// the split/join so this struct stays honest.
	ExpectedAnalysts []string `json:"expected_analysts,omitempty"`

	// StartedAt is when the session began, captured server-side so
	// skew between orchestrator and CLI clocks doesn't matter.
	StartedAt time.Time `json:"started_at"`

	// EndedAt is the terminal timestamp, or nil while the session
	// is still in_progress. Pointer form so omitempty works — a
	// zero time.Time is NOT omitted by encoding/json.
	EndedAt *time.Time `json:"ended_at,omitempty"`

	// Status is the lifecycle marker. See AnalysisSessionStatus.
	Status AnalysisSessionStatus `json:"status"`

	// SynthesisOutputID is the UUID of the synthesis output that
	// closed this session, or "" if none landed. Nullable FK to
	// analyst_outputs.id (ON DELETE SET NULL) so pruning a
	// synthesis doesn't orphan the session row.
	SynthesisOutputID string `json:"synthesis_output_id,omitempty"`

	// Notes is free-form operator commentary recorded at begin-time
	// (e.g. "dogfood re-run after the canonicalization fix").
	Notes string `json:"notes,omitempty"`
}

// AnalysisSessionCloseParams describes a one-way transition out of
// in_progress. Status must be terminal (Completed/Failed/Partial);
// EndedAt is required; SynthesisOutputID is optional and only set
// when the close was auto-triggered by a synthesis ingest.
//
// Suffixed "Params" rather than "Close" alone because the bare word
// `close` is a Go builtin (channel close) — this naming avoids
// predeclared-identifier shadowing when the type is used as a
// parameter name.
type AnalysisSessionCloseParams struct {
	Status            AnalysisSessionStatus
	EndedAt           time.Time
	SynthesisOutputID string
}
