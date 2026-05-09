package repofiles

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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

	// Rust-specific: detect proc-macro crates from Cargo.toml.
	// Only emits when Cargo.toml exists — non-Rust repos produce no
	// proc_macro_crate signal (absent-because-not-applicable, not
	// absent-because-we-failed).
	c.detectProcMacro(&result, entity.ID, now)

	return &result, nil
}

// detectProcMacro reads the root Cargo.toml (if present) and emits a
// proc_macro_crate signal when [lib] proc-macro = true. Proc macros
// execute inside rustc at compile time — a distinct and elevated
// attack surface compared to regular library/binary crates.
//
// Does not emit anything if Cargo.toml is absent (non-Rust repo).
// Emits present=false when Cargo.toml exists but is not a proc macro.
func (c *Collector) detectProcMacro(result *signal.CollectionResult, entityID string, collectedAt time.Time) {
	cargoPath := filepath.Join(c.path, "Cargo.toml")
	data, err := os.ReadFile(cargoPath) //nolint:gosec // G304: c.path is the collector's by-design input (the clone root we were asked to inspect)
	if err != nil {
		// File doesn't exist or unreadable — not applicable. Don't
		// emit anything; this is "signal not applicable" not "signal
		// absent due to failure."
		return
	}

	// Quick scan for proc-macro = true in the [lib] section. A full
	// TOML parse is overkill — the pattern `proc-macro = true` under
	// a [lib] header is unambiguous and the file is already capped by
	// filesystem reads (64 KiB typical Cargo.toml).
	isProcMacro := detectProcMacroInToml(string(data))

	result.RecordSignal(entityID, "proc_macro_crate", sourceName, collectedAt, c.ttl,
		map[string]any{
			"present": isProcMacro,
		})
}

// detectProcMacroInToml scans TOML content for `proc-macro = true`
// under a [lib] section. Uses line-by-line scanning rather than a full
// TOML parse — the pattern is unambiguous and avoids importing a TOML
// library into the repofiles package (which operates on raw file
// content for all its other detections).
func detectProcMacroInToml(content string) bool {
	inLib := false
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		// Section headers.
		if strings.HasPrefix(trimmed, "[") {
			inLib = trimmed == "[lib]"
			continue
		}
		if !inLib {
			continue
		}
		// Strip comments.
		if idx := strings.IndexByte(trimmed, '#'); idx >= 0 {
			trimmed = strings.TrimSpace(trimmed[:idx])
		}
		// Match proc-macro = true (with flexible whitespace).
		key, val, ok := strings.Cut(trimmed, "=")
		if !ok {
			continue
		}
		if strings.TrimSpace(key) == "proc-macro" && strings.TrimSpace(val) == "true" {
			return true
		}
	}
	return false
}
