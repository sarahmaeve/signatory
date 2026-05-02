package exfilwatch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal"
)

// sourceName is the collector's identifier on every emitted signal —
// matches the package name and the repofiles/git collector convention.
const sourceName = "exfilwatch"

// signalType is the registry key for the emitted signal. Registered
// in internal/signal/types.go; signal.Make panics on unregistered
// types so a mismatch here is a build-time failure mode in tests.
const signalType = "exfil_capture_host"

// defaultTTL matches repofiles and git: 24h is short enough to pick
// up a freshly-introduced literal between runs, long enough to avoid
// store churn on routine analyses.
const defaultTTL = 24 * time.Hour

// ErrNoClone is returned when the collector is asked to scan a path
// that doesn't resolve to a directory. By the time Collect runs,
// resolveClonePath upstream has either produced a real clone or
// errored, so an empty/missing path here is a programming bug rather
// than a normal absence — fail loudly per v0.1 Invariant 2 (same
// contract as the repofiles collector).
var ErrNoClone = errors.New("exfilwatch: clone path is empty or not a directory")

// Collector scans a local clone for literal references to
// HTTP-capture-as-a-service hosts (see Hosts) and emits one
// exfil_capture_host signal carrying the hit list. A clean tree
// emits an empty hit list — the signal is always present when a
// clone is available, so a downstream consumer can distinguish "we
// checked, nothing found" from "we didn't check."
type Collector struct {
	path string
	ttl  time.Duration
}

// NewCollector constructs a collector rooted at clonePath. Path
// validation happens on the first Collect call (matching the
// repofiles convention) so the caller can build a collector slice
// once per analysis without per-collector path probing.
func NewCollector(clonePath string) *Collector {
	return &Collector{path: clonePath, ttl: defaultTTL}
}

// Name implements signal.Collector.
func (c *Collector) Name() string { return sourceName }

// Collect runs Scan over the clone and emits exactly one
// exfil_capture_host signal carrying the (possibly empty) hit list.
//
// Returns ErrNoClone when path is empty or not a directory; matches
// repofiles's fail-loudly contract for a missing clone. nil entity
// returns an empty CollectionResult — symmetric with the source
// collector's nil-entity guard.
func (c *Collector) Collect(_ context.Context, entity *profile.Entity) (*signal.CollectionResult, error) {
	result := &signal.CollectionResult{}
	if entity == nil {
		return result, nil
	}
	if c.path == "" {
		return nil, ErrNoClone
	}
	info, err := os.Stat(c.path)
	if err != nil || !info.IsDir() {
		return nil, ErrNoClone
	}

	hits, err := Scan(c.path)
	if err != nil {
		return nil, fmt.Errorf("%s: scan: %w", sourceName, err)
	}
	if hits == nil {
		// JSON-stable: empty result encodes as [] rather than null.
		hits = []Hit{}
	}

	result.RecordSignal(entity.ID, signalType, sourceName,
		time.Now().UTC(), c.ttl, hits)
	return result, nil
}
