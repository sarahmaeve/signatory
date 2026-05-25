package repofiles

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sarahmaeve/signatory/internal/contentinjection"
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

// maxCargoTomlBytes caps detectProcMacro's read so an attacker-
// controlled multi-GiB Cargo.toml cannot OOM the collector. A typical
// Cargo.toml is well under 64 KiB; 1 MiB accommodates the largest
// real-world workspace roots while remaining a hard DoS ceiling. The
// false-negative tradeoff (proc-macro = true beyond the cap is
// invisible) is acceptable: a malicious crate engineering its Cargo
// manifest to push the declaration past 1 MiB is itself a strong
// hostility signal and is bounded by what cargo will actually parse.
const maxCargoTomlBytes = 1 * 1024 * 1024

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

	// Ecosystem-agnostic: detect AI-agent config files and scan
	// their contents for the content-injection-surface primitives
	// per design/anti-subversion.md. Always emits (the "we looked,
	// here's what we found" shape — empty findings is itself a
	// positive observation).
	c.detectAgentConfig(&result, entity.ID, now)

	return &result, nil
}

// agentConfigFile is one detected AI-agent config file. The compound
// agent_config_files signal value carries a list of these entries.
type agentConfigFile struct {
	Family string `json:"family"`
	Path   string `json:"path"`
}

// agentConfigInjectionEntry is one file with positive
// content-injection findings. The compound
// agent_config_content_injection signal value carries a list of
// these.
type agentConfigInjectionEntry struct {
	Family    string                     `json:"family"`
	Path      string                     `json:"path"`
	Findings  []contentinjection.Finding `json:"findings"`
	Truncated bool                       `json:"truncated,omitempty"`
}

// detectAgentConfig scans the clone for AI-agent config files
// (CLAUDE.md, .cursorrules, .claude/settings.json, etc. — see
// AgentConfigFamilies) and content-scans each match with the
// content-injection primitive package.
//
// Two signals are emitted:
//
//   - agent_config_files: the (possibly empty) inventory of detected
//     AI-agent config files, with family + path per entry. Always
//     emits.
//   - agent_config_content_injection: the (possibly empty) list of
//     files that produced at least one content-injection finding.
//     Always emits — empty payload is the positive
//     "we-scanned-and-found-nothing" observation.
//
// Per-file IO errors are absorbed silently: a file that cannot be
// read is skipped from the injection scan but still appears in the
// inventory (it was detected by name). A failed scan of one file
// does not block the other findings.
func (c *Collector) detectAgentConfig(result *signal.CollectionResult, entityID string, collectedAt time.Time) {
	matches, err := Scan(c.path, AgentConfigFamilies())
	if err != nil {
		// Same shape as detectProcMacro on read failure: emit
		// nothing. The repo_files signal's success above already
		// confirmed the clone is valid; a second-scan error here
		// would be a transient filesystem issue.
		return
	}

	files := make([]agentConfigFile, 0, len(matches))
	for _, m := range matches {
		files = append(files, agentConfigFile(m))
	}
	result.RecordSignal(entityID, "agent_config_files", sourceName, collectedAt, c.ttl,
		map[string]any{
			"files":        files,
			"family_count": len(files),
		})

	// Per design/anti-subversion.md §"Where AI-instruction files
	// fit" §2: imperative-mood prose IS the expected content on
	// these files, so the markdown_comment primitive is useless
	// here. Suppress it; the other six primitives carry the load
	// (zero-width Unicode, bidi controls, tag block, exfil-shaped
	// markdown images, lexical injection phrases, encoded blobs).
	scanOpts := contentinjection.ScanOptions{
		SuppressPrimitives: []contentinjection.Primitive{
			contentinjection.PrimitiveMarkdownComment,
		},
	}

	injections := make([]agentConfigInjectionEntry, 0)
	totalFindings := 0
	for _, m := range matches {
		absPath := filepath.Join(c.path, m.Path)
		scan, err := contentinjection.ScanFileWithOptions(absPath, scanOpts)
		if err != nil {
			// Per-file IO failure: skip the injection scan for this
			// file but keep the inventory entry. A symlink to outside
			// the clone, a race-removed file, or a permission glitch
			// is not the collector's problem to surface.
			continue
		}
		if !scan.HasFindings() {
			continue
		}
		injections = append(injections, agentConfigInjectionEntry{
			Family:    m.Family,
			Path:      m.Path,
			Findings:  scan.Findings,
			Truncated: scan.Truncated,
		})
		totalFindings += len(scan.Findings)
	}
	result.RecordSignal(entityID, "agent_config_content_injection", sourceName, collectedAt, c.ttl,
		map[string]any{
			"files_with_findings": injections,
			"total_finding_count": totalFindings,
		})
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
	f, err := os.Open(cargoPath) //nolint:gosec // G304: c.path is the collector's by-design input (the clone root we were asked to inspect)
	if err != nil {
		// File doesn't exist or unreadable — not applicable. Don't
		// emit anything; this is "signal not applicable" not "signal
		// absent due to failure."
		return
	}
	defer func() { _ = f.Close() }()

	// Bounded read: maxCargoTomlBytes caps memory consumption for an
	// adversarial-size Cargo.toml. See the constant's doc for the
	// false-negative tradeoff.
	data, err := io.ReadAll(io.LimitReader(f, maxCargoTomlBytes))
	if err != nil {
		return
	}

	// Quick scan for proc-macro = true in the [lib] section. A full
	// TOML parse is overkill — the pattern `proc-macro = true` under
	// a [lib] header is unambiguous.
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
