package signal

import (
	"context"

	"github.com/sarahmaeve/signatory/internal/profile"
)

// Collector defines the interface for signal source providers.
// Each implementation (GitHub, npm registry, OpenSSF Scorecard, etc.)
// collects signals from a specific source and returns them in
// the common Signal format.
type Collector interface {
	// Name returns the collector's identifier (e.g., "github", "npm-registry").
	Name() string

	// Collect gathers signals for the given entity.
	Collect(ctx context.Context, entity *profile.Entity) ([]profile.Signal, error)
}
