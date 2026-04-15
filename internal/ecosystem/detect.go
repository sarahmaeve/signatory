package ecosystem

import (
	"context"
	"fmt"
)

// Ecosystem is the package-registry family a repo publishes into. It
// is NOT synonymous with language — a Go project publishes to Go
// modules, but a repo can be polyglot (Go backend + npm frontend)
// and in that case the detector reports the "primary" ecosystem
// plus all candidates.
type Ecosystem string

const (
	// EcosystemGo is the Go module registry (proxy.golang.org).
	// Signal: `go.mod` at repo root.
	EcosystemGo Ecosystem = "go"

	// EcosystemCrates is crates.io (Rust). Signal: `Cargo.toml`.
	EcosystemCrates Ecosystem = "crates"

	// EcosystemPyPI is pypi.org (Python). Signal: `pyproject.toml`
	// or `setup.py`.
	EcosystemPyPI Ecosystem = "pypi"

	// EcosystemNPM is npmjs.com (Node.js). Signal: `package.json`.
	EcosystemNPM Ecosystem = "npm"

	// EcosystemUnknown is the null ecosystem — returned when no
	// recognized manifest is present. The caller should treat this
	// as "unable to determine" rather than "no package here."
	EcosystemUnknown Ecosystem = ""
)

// DetectionResult reports the outcome of a single detection call.
// Primary is the best single guess for the ecosystem; Candidates
// holds every recognized ecosystem in priority order so callers
// with richer UIs (e.g., "we found go.mod and package.json; which
// were you asking about?") can present a choice. Language is
// GitHub's best-effort primary-language string (free-form, e.g.,
// "Python", "Go", "Rust", "JavaScript") — useful for picking a
// language-specific analyst-prompt variant.
type DetectionResult struct {
	Primary    Ecosystem
	Candidates []Ecosystem
	Language   string
	// RootFiles is the filename list the detector inspected. Returned
	// so callers can print a "we looked at: …" diagnostic, and so
	// tests can assert on the detector's inputs without replaying
	// the API dance.
	RootFiles []string
}

// Source is the small interface the detector needs from its data
// provider. Decoupling here means (a) the detector doesn't know
// about HTTP or GitHub specifics, and (b) tests can mock with a
// fake that doesn't stand up an httptest.Server.
//
// In production, *github.Client satisfies this interface via its
// ListRootFilenames and GetRepoLanguage methods.
type Source interface {
	// ListRootFilenames returns file names (not directories) at the
	// repo's root. Returns nil without error for a repo that
	// doesn't exist.
	ListRootFilenames(ctx context.Context, owner, repoName string) ([]string, error)

	// GetRepoLanguage returns GitHub's primary-language string, or
	// the empty string if none is determinable. Not authoritative
	// on its own — Python repos sometimes report as "TypeScript"
	// because of docs tooling — but useful as a tiebreaker.
	GetRepoLanguage(ctx context.Context, owner, repoName string) (string, error)
}

// Detector classifies a remote repo into an ecosystem by inspecting
// its root directory. Construct with NewDetector.
type Detector struct {
	src Source
}

// NewDetector returns a Detector that reads from src. Callers in
// production wire this up with a *github.Client; tests can pass a
// fake Source.
func NewDetector(src Source) *Detector {
	return &Detector{src: src}
}

// Detect classifies the repo at owner/repoName. It makes two API
// calls: one to list root filenames (for manifest-based ecosystem
// detection) and one to fetch GitHub's primary-language string (for
// template-variant selection). Both are always issued — even when a
// manifest is unambiguous, the language string drives Python-vs-Go
// prompt-variant selection downstream.
//
// The second call's failure is tolerated when the first call already
// produced a candidate: callers can still pick an ecosystem without
// the language hint. When no manifest matched, a language failure is
// fatal because we have nothing else to report.
//
// Returns a DetectionResult. Primary is EcosystemUnknown when no
// manifest matches; callers should surface that to the user as
// "couldn't determine, pass --ecosystem explicitly."
func (d *Detector) Detect(ctx context.Context, owner, repoName string) (*DetectionResult, error) {
	if d.src == nil {
		return nil, fmt.Errorf("ecosystem.Detector: nil Source (did you forget NewDetector?)")
	}
	files, err := d.src.ListRootFilenames(ctx, owner, repoName)
	if err != nil {
		return nil, fmt.Errorf("list root of %s/%s: %w", owner, repoName, err)
	}

	candidates := classifyRootFiles(files)
	result := &DetectionResult{
		Candidates: candidates,
		RootFiles:  files,
	}
	if len(candidates) > 0 {
		result.Primary = candidates[0]
	}

	// Language lookup. We do this regardless of whether we found a
	// manifest — even when the ecosystem is known, the primary
	// language informs template-variant selection (python vs go
	// security review). This costs one extra API call but keeps the
	// API shape uniform for callers.
	lang, err := d.src.GetRepoLanguage(ctx, owner, repoName)
	if err != nil {
		// A language-lookup failure is not fatal when we already
		// have a manifest-based ecosystem guess; degrade gracefully.
		if result.Primary == EcosystemUnknown {
			return nil, fmt.Errorf("get language for %s/%s: %w", owner, repoName, err)
		}
		return result, nil
	}
	result.Language = lang
	return result, nil
}

// manifestSignals enumerates, for each ecosystem, the filenames we
// treat as a positive signal that the repo publishes to that
// ecosystem. Order inside a slot does not matter (presence is what
// matters); order across ecosystems is controlled by priorityOrder.
//
// Keep this table flat and documented: every addition is a
// calibration decision (is this file a sure signal, or could it
// appear incidentally in a polyglot repo?).
var manifestSignals = map[Ecosystem][]string{
	EcosystemGo: {
		"go.mod", // Go modules are unambiguous.
	},
	EcosystemCrates: {
		"Cargo.toml",
	},
	EcosystemPyPI: {
		"pyproject.toml", // PEP 518/621 — modern Python packaging.
		"setup.py",       // Legacy but widely extant.
	},
	EcosystemNPM: {
		"package.json", // Lock files (package-lock.json, yarn.lock,
		// pnpm-lock.yaml) strengthen but aren't required — bare
		// package.json is already npm-conformant.
	},
}

// priorityOrder defines which ecosystem wins when a repo has
// multiple recognized manifests. Rationale (per ecosystem-design
// reconnaissance):
//   - Go first: go.mod is the most unambiguous signal and Go
//     repos rarely publish via another registry.
//   - Crates second: Cargo.toml is unambiguous; Rust repos rarely
//     also publish npm or PyPI packages (unlike Python repos with
//     JS frontends).
//   - PyPI third: setup.py/pyproject.toml clearly declare packaging
//     intent even in polyglot repos.
//   - npm last: package.json is the most common "polyglot drag-in"
//     — Go CLIs ship npm release scripts; Python projects ship JS
//     frontends. Without corroboration, treat npm as the tiebreaker.
//
// Callers who want the full multi-ecosystem story read
// DetectionResult.Candidates rather than .Primary.
var priorityOrder = []Ecosystem{
	EcosystemGo,
	EcosystemCrates,
	EcosystemPyPI,
	EcosystemNPM,
}

// classifyRootFiles scans names for the manifests in manifestSignals
// and returns matching ecosystems in priorityOrder. Duplicates are
// impossible by construction (each manifest maps to exactly one
// ecosystem). An empty slice is a valid result — the caller's job to
// decide what to do with "no recognized manifest."
func classifyRootFiles(names []string) []Ecosystem {
	present := make(map[Ecosystem]bool)
	nameSet := make(map[string]struct{}, len(names))
	for _, n := range names {
		nameSet[n] = struct{}{}
	}
	for eco, signals := range manifestSignals {
		for _, sig := range signals {
			if _, ok := nameSet[sig]; ok {
				present[eco] = true
				break
			}
		}
	}
	out := make([]Ecosystem, 0, len(present))
	for _, eco := range priorityOrder {
		if present[eco] {
			out = append(out, eco)
		}
	}
	return out
}
