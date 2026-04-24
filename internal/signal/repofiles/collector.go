package repofiles

import (
	"context"
	"time"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal"
)

// sourceName is the collector's identifier on every emitted signal —
// matches the package name and the git/github collector convention
// (one constant, one place to change it if renaming).
const sourceName = "repofiles"

// defaultTTL matches the git collector's cadence. 24h is short enough
// to pick up a newly-added CONTRIBUTING between runs and long enough
// that the scan doesn't churn the store on routine analyses.
const defaultTTL = 24 * time.Hour

// Collector emits a compound "repo_files" signal summarizing the
// presence of conventional project-hygiene files under a local git
// clone. It implements signal.Collector and is wired into the
// collector-assembly path alongside the git and github collectors
// when the entity is git-hosted.
type Collector struct {
	path string
	ttl  time.Duration
}

// NewCollector constructs a collector rooted at clonePath. Path
// validation happens on the first Collect call — the constructor
// doesn't fail even for an empty or missing path, so the caller can
// build a collector slice once per analysis and let each Collect
// surface its own clone problems uniformly.
func NewCollector(clonePath string) *Collector {
	return &Collector{path: clonePath, ttl: defaultTTL}
}

// Name implements signal.Collector.
func (c *Collector) Name() string { return sourceName }

// Collect scans the clone, ranks matches, and emits exactly one
// compound signal of type "repo_files". The signal's value is a
// map keyed by family name — stable across runs because the
// declared family order drives iteration and encoding/json emits
// map keys in sorted order.
//
// A missing or invalid clone returns ErrNoClone with no signal
// emitted; this matches the git collector's fail-loudly contract
// (v0.1 Invariant 2). Partial sub-dir failures are absorbed by the
// scanner as "lose coverage for that dir, keep going" and do not
// surface as errors or absences — a missing .github/ sub-dir is
// the common case for most repos, not an anomaly worth flagging.
func (c *Collector) Collect(_ context.Context, entity *profile.Entity) (*signal.CollectionResult, error) {
	fams := Families()

	matches, err := Scan(c.path, fams)
	if err != nil {
		return nil, err
	}
	results := Evaluate(fams, matches)

	// Compound value: map[family_name]Result. Using Result directly
	// gives JSON fields {present, path, alt_paths} per family. The
	// json:"-" tag on Result.Family prevents the name being encoded
	// twice (once as map key, once as struct field).
	value := make(map[string]Result, len(results))
	for _, r := range results {
		value[r.Family] = r
	}

	now := time.Now().UTC()
	var result signal.CollectionResult
	result.RecordSignal(entity.ID, "repo_files", sourceName, now, c.ttl, value)
	return &result, nil
}
