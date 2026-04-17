package git

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal"
)

// sourceName is the Collector's identifier, recorded on every signal
// this package emits so downstream queries can distinguish git-local
// observations from github-API ones. Declared as a package constant
// so there is exactly one place to change it.
const sourceName = "git"

// Default behavioral parameters. Each is overridable via a With*
// option — see Collector.WithWindow and Collector.WithCommitCap
// below. The defaults match the v0.1 plan in
// design/v0.1-invariants.md: 12-month window, 1000-commit cap, and
// a 24-hour signal TTL matching the github collector's cadence.
const (
	defaultWindow    = 365 * 24 * time.Hour
	defaultCommitCap = 1000
	defaultTTL       = 24 * time.Hour
)

// Collector reads signals from a local git clone. Construct one per
// target using NewCollector; the path is baked into the instance
// because each invocation operates on a specific on-disk clone.
//
// The collector is safe to hold across multiple calls to Collect
// (it has no mutable state after construction), but it's more
// idiomatic in this codebase to construct a fresh collector per
// target from cmd/signatory/analyze.go's collector-assembly path.
type Collector struct {
	// path is the absolute filesystem path to the local clone.
	// Required; NewCollector accepts any string and validation
	// happens in Collect before the first git invocation.
	path string

	// window bounds the observation period for commit-based
	// signals (signing ratios, maintainer concentration, etc.).
	// Signals derived from the full history (first_commit_date,
	// identity_graph_depth) ignore this field.
	window time.Duration

	// commitCap is the maximum number of commits a single
	// windowed query will parse. Defends against pathologically
	// large commit counts consuming unbounded memory — git's own
	// memory stays bounded by `-n`, and the parser's bounded by
	// this cap too.
	commitCap int
}

// NewCollector constructs a git collector rooted at path. The path
// is not validated at construction time; validation happens on the
// first Collect call, where the error has a cleaner surface to
// report through.
//
// Defaults:
//   - Window: 12 months (design/v0.1-invariants.md decision).
//   - CommitCap: 1000 commits per windowed query.
//   - TTL: 24 hours (matches the github collector's cadence).
func NewCollector(path string) *Collector {
	return &Collector{
		path:      path,
		window:    defaultWindow,
		commitCap: defaultCommitCap,
	}
}

// WithWindow overrides the commit-observation window. Useful for
// tests and for targeted analyses that want a narrower or wider
// lens than the default 12 months.
func (c *Collector) WithWindow(d time.Duration) *Collector {
	c.window = d
	return c
}

// WithCommitCap overrides the per-query commit cap. Tests use this
// to exercise the parser with small inputs; production callers
// should rarely need to.
func (c *Collector) WithCommitCap(n int) *Collector {
	c.commitCap = n
	return c
}

// Name implements signal.Collector.
func (c *Collector) Name() string { return sourceName }

// ErrNoClone is returned when Collect is called with an empty path
// or a path that doesn't resolve to a git working-tree clone. The
// orchestrator (cmd/signatory/analyze.go) translates this into the
// top-level "fail loudly" error defined by v0.1 Invariant 2 — the
// git collector doesn't proceed with partial signals when the
// prerequisite isn't met.
var ErrNoClone = errors.New("git collector: no local clone at path")

// Collect runs every git-local signal pass supported by v0.1 and
// returns a CollectionResult. Each individual signal is
// independent — a failure in one (e.g., commit signing parse
// error) does not prevent others from being collected.
//
// Collect returns a non-nil error only when the clone is missing
// or invalid; individual signal failures are recorded as failures
// in the result, consistent with the github collector's contract.
func (c *Collector) Collect(ctx context.Context, entity *profile.Entity) (*signal.CollectionResult, error) {
	if err := c.validateClone(); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	var result signal.CollectionResult

	c.collectCommitSigning(ctx, &result, entity.ID, now, defaultTTL)
	c.collectTags(ctx, &result, entity.ID, now, defaultTTL)

	return &result, nil
}

// validateClone confirms c.path names a git working-tree clone.
// Cheap check — stat for `.git` as either a directory (normal
// clone) or a file (submodule / worktree indirection).
//
// We intentionally don't run `git rev-parse --is-inside-work-tree`
// here: that would cost a subprocess for a check the stat already
// answers, and the first real git call (in collectCommitSigning)
// surfaces any subtler repository-corruption errors naturally.
func (c *Collector) validateClone() error {
	if c.path == "" {
		return ErrNoClone
	}
	info, err := os.Stat(filepath.Join(c.path, ".git"))
	if err != nil || info == nil {
		return ErrNoClone
	}
	return nil
}

// sanitize strips the clone path from error-message text before
// the message is persisted to the store. Signal.reason values
// live in SQLite indefinitely; absolute filesystem paths in them
// would leak per-machine detail in any shared database export.
//
// Replaces c.path with the literal string "<clone>". Other
// path-shaped substrings (temp dirs, home dir) are left alone —
// they're rarely present in git's stderr and a more aggressive
// sanitizer risks over-redacting genuinely useful diagnostic text.
func (c *Collector) sanitize(s string) string {
	if c.path == "" {
		return s
	}
	return strings.ReplaceAll(s, c.path, "<clone>")
}
