package store

import (
	"context"

	"github.com/sarahmaeve/signatory/internal/profile"
)

// Store defines the persistence interface for signatory's data.
// The primary implementation uses SQLite.
type Store interface {
	// Entity operations
	GetEntity(ctx context.Context, id string) (*profile.Entity, error)
	PutEntity(ctx context.Context, entity *profile.Entity) error
	FindEntity(ctx context.Context, name string, entityType profile.EntityType) (*profile.Entity, error)

	// Signal operations
	GetSignals(ctx context.Context, entityID string) ([]profile.Signal, error)
	PutSignals(ctx context.Context, signals []profile.Signal) error
	GetSignalsByGroup(ctx context.Context, entityID string, group profile.SignalGroup) ([]profile.Signal, error)

	// Posture operations
	GetPosture(ctx context.Context, entityID string) (*profile.Posture, error)
	SetPosture(ctx context.Context, posture *profile.Posture) error

	// Burn operations
	GetBurn(ctx context.Context, entityID string) (*profile.Burn, error)
	SetBurn(ctx context.Context, burn *profile.Burn) error
	ListBurns(ctx context.Context) ([]profile.Burn, error)

	// Close releases database resources.
	Close() error
}
