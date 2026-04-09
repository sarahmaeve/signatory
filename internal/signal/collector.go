package signal

import (
	"context"

	"github.com/sarahmaeve/signatory/internal/profile"
)

// Collector defines the interface for signal source providers.
// Each implementation (GitHub, npm registry, OpenSSF Scorecard, etc.)
// collects signals from a specific source and returns a CollectionResult
// that includes both successfully collected signals and any failures
// with absence records.
//
// A Collector should return a non-nil CollectionResult even on partial
// failure. The only case where error should be non-nil is when collection
// cannot proceed at all (e.g., the target cannot be parsed). Individual
// signal collection failures should be recorded as absences within the
// result, not returned as the error.
type Collector interface {
	// Name returns the collector's identifier (e.g., "github", "npm-registry").
	Name() string

	// Collect gathers signals for the given entity. Returns a
	// CollectionResult containing both collected signals (including
	// absence records for failed collections) and a summary of failures.
	//
	// Returns error only if collection cannot proceed at all.
	// Partial failures are recorded in the result, not as errors.
	Collect(ctx context.Context, entity *profile.Entity) (*CollectionResult, error)
}
