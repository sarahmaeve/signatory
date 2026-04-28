package store

import (
	"context"
	"time"

	"github.com/sarahmaeve/signatory/internal/exchange"
	"github.com/sarahmaeve/signatory/internal/profile"
)

// Store defines the persistence interface for signatory's data.
// The primary implementation uses SQLite.
type Store interface {
	// Entity operations
	GetEntity(ctx context.Context, id string) (*profile.Entity, error)
	FindEntityByURI(ctx context.Context, canonicalURI string) (*profile.Entity, error)
	// FindEntityByVersionedBaseURI returns any entity whose canonical_uri
	// matches `baseURI@<something>`. Lookup-side fallback for pre-Plan-A
	// versioned-entity rows; see SQLite implementation for ordering rules.
	FindEntityByVersionedBaseURI(ctx context.Context, baseURI string) (*profile.Entity, error)
	PutEntity(ctx context.Context, entity *profile.Entity) error

	// Signal operations (append-only)
	GetSignals(ctx context.Context, entityID string) ([]profile.Signal, error)
	GetLatestSignals(ctx context.Context, entityID string) ([]profile.Signal, error)
	AppendSignals(ctx context.Context, signals []profile.Signal) error
	GetSignalsByGroup(ctx context.Context, entityID string, group profile.SignalGroup) ([]profile.Signal, error)

	// Posture operations (versioned). WithdrawPosture is the soft-
	// delete counterpart to SetPosture; reads filter out withdrawn
	// rows by default. HasPostures is the boolean shortcut used by
	// LookupEntity's weight-aware alternate walk — cheaper than
	// GetPostures when the caller only needs "any active posture?"
	// rather than the full list.
	GetPosture(ctx context.Context, entityID string, version string) (*profile.Posture, error)
	GetPostures(ctx context.Context, entityID string) ([]profile.Posture, error)
	HasPostures(ctx context.Context, entityID string) (bool, error)
	SetPosture(ctx context.Context, posture *profile.Posture) error
	WithdrawPosture(ctx context.Context, entityID, version, withdrawnBy, reason string, at time.Time) error

	// Burn operations. WithdrawBurn is the soft-delete counterpart
	// to SetBurn; GetBurn and ListBurns filter out withdrawn rows
	// by default.
	GetBurn(ctx context.Context, entityID string) (*profile.Burn, error)
	SetBurn(ctx context.Context, burn *profile.Burn) error
	WithdrawBurn(ctx context.Context, entityID, withdrawnBy, reason string, at time.Time) error
	ListBurns(ctx context.Context) ([]profile.Burn, error)

	// Dependency observations (append-only)
	AppendDependencyObservations(ctx context.Context, observations []profile.DependencyObservation) error
	GetLatestDependencies(ctx context.Context, projectID string) ([]profile.DependencyObservation, error)

	// Audit log (append-only)
	AppendAuditEntry(ctx context.Context, entry *profile.AuditEntry) error

	// Signal resolutions
	AppendResolution(ctx context.Context, resolution *profile.SignalResolution) error

	// Team identities
	GetTeamIdentity(ctx context.Context, id string) (*profile.TeamIdentity, error)
	PutTeamIdentity(ctx context.Context, identity *profile.TeamIdentity) error

	// Analyst output ingestion (append-only, idempotent on content_hash).
	IngestAnalystOutput(ctx context.Context, out *exchange.AnalystOutput, sourcePath string, opts ...IngestOption) (*IngestResult, error)

	// Analyst output queries (read path).
	ListAnalystOutputs(ctx context.Context, filter AnalystOutputFilter) ([]AnalystOutputSummary, error)
	GetAnalystOutput(ctx context.Context, outputID string) (*exchange.AnalystOutput, error)
	GetOutputEntity(ctx context.Context, outputID string) (*profile.Entity, error)
	ListConclusions(ctx context.Context, filter ConclusionFilter) ([]ConclusionSummary, error)
	ListMethodologyPatterns(ctx context.Context, filter MethodologyPatternFilter) ([]MethodologyPatternSummary, error)

	// Summary helpers (M7). SeverityCounts groups one output's
	// conclusions by severity_default; ListRelatedURIs walks the
	// collected_from links in both directions to surface identity
	// relationships. Both power the signatory_summary verb.
	SeverityCounts(ctx context.Context, outputID string) (map[exchange.SeverityValue]int, error)
	ListRelatedURIs(ctx context.Context, entityID string) ([]string, error)

	// Synthesis proposal reader (M6d). Narrow helper for `posture
	// accept <output-id>`: returns the ProposedPosture recorded on
	// a synthesis output without reconstructing the full document.
	// Returns ErrNotFound when outputID is unknown OR when the row
	// isn't a synthesis (both cases mean "no proposal to accept").
	GetSynthesisProposal(ctx context.Context, outputID string) (*exchange.ProposedPosture, error)

	// Prune operations — destructive cleanup paths. Each plan call
	// is read-only and suitable for rendering a dry-run preview;
	// the apply call executes the plan inside a single transaction
	// with append-only triggers temporarily suspended. Intended to
	// run only behind an explicit operator action (`signatory
	// prune …`).
	PlanPruneEntities(ctx context.Context, entityIDs []string) (*PruneReport, error)
	PruneEntities(ctx context.Context, entityIDs []string) (*PruneReport, error)
	ListVersionedEntities(ctx context.Context) ([]string, error)
	ListOrphanEntities(ctx context.Context) ([]string, error)

	// Analysis session operations — the durable audit identity for
	// each /analyze run. Link analyst outputs to a session by
	// passing WithAnalysisSession to IngestAnalystOutput; the
	// session is closed via CloseAnalysisSession (one-way, enforced
	// by a store-layer terminal-state guard).
	//
	// Design rationale: design/phase3-plan.md.
	//
	// Errors: CreateAnalysisSession and CloseAnalysisSession wrap
	// ErrNilInput for missing required fields. GetAnalysisSession
	// and CloseAnalysisSession return ErrNotFound when the id is
	// unknown. CloseAnalysisSession returns a non-sentinel error
	// when the session is already terminal.
	CreateAnalysisSession(ctx context.Context, session *profile.AnalysisSession) error
	GetAnalysisSession(ctx context.Context, id string) (*profile.AnalysisSession, error)
	ListAnalysisSessions(ctx context.Context, filter AnalysisSessionFilter) ([]profile.AnalysisSession, error)
	CloseAnalysisSession(ctx context.Context, id string, params profile.AnalysisSessionCloseParams) error
	ListOutputsForSession(ctx context.Context, sessionID string) ([]AnalystOutputSummary, error)

	// Close releases database resources.
	Close() error
}

// AnalysisSessionFilter narrows ListAnalysisSessions. Each field
// is optional; zero-value means "no filter on this dimension."
// Combined conjunctively (AND) when more than one is set.
type AnalysisSessionFilter struct {
	// EntityID limits to sessions targeting one entity. Use the
	// unversioned entity ID; TargetVersion filters separately.
	EntityID string

	// TargetVersion limits to sessions whose caller-supplied @V
	// matched this string. Use "" to match only unversioned runs;
	// leave zero (the empty-string) to disable the filter — the
	// store treats a zero-value filter as "don't apply this clause."
	TargetVersion string

	// Status limits to sessions in the named lifecycle state.
	// Common query: Status=AnalysisSessionInProgress to find
	// stragglers.
	Status profile.AnalysisSessionStatus

	// Since returns only sessions that started on or after this
	// instant. Zero-value means no lower bound. Used by the skill
	// to answer "show me today's runs."
	Since time.Time

	// Limit caps the returned row count. 0 means unbounded;
	// callers rendering tables should set a reasonable ceiling
	// (50 is the CLI default) to keep output tractable.
	Limit int
}
