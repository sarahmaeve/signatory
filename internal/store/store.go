package store

import (
	"context"

	"github.com/sarahmaeve/signatory/internal/exchange"
	"github.com/sarahmaeve/signatory/internal/profile"
)

// Store defines the persistence interface for signatory's data.
// The primary implementation uses SQLite.
type Store interface {
	// Entity operations
	GetEntity(ctx context.Context, id string) (*profile.Entity, error)
	FindEntityByURI(ctx context.Context, canonicalURI string) (*profile.Entity, error)
	PutEntity(ctx context.Context, entity *profile.Entity) error

	// Signal operations (append-only)
	GetSignals(ctx context.Context, entityID string) ([]profile.Signal, error)
	GetLatestSignals(ctx context.Context, entityID string) ([]profile.Signal, error)
	AppendSignals(ctx context.Context, signals []profile.Signal) error
	GetSignalsByGroup(ctx context.Context, entityID string, group profile.SignalGroup) ([]profile.Signal, error)

	// Posture operations (versioned)
	GetPosture(ctx context.Context, entityID string, version string) (*profile.Posture, error)
	GetPostures(ctx context.Context, entityID string) ([]profile.Posture, error)
	SetPosture(ctx context.Context, posture *profile.Posture) error

	// Burn operations
	GetBurn(ctx context.Context, entityID string) (*profile.Burn, error)
	SetBurn(ctx context.Context, burn *profile.Burn) error
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
	IngestAnalystOutput(ctx context.Context, out *exchange.AnalystOutput, sourcePath string) (*IngestResult, error)

	// Close releases database resources.
	Close() error
}
