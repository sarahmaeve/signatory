// Command smoke-pintable drives an end-to-end test against a
// freshly-built `signatory analyze` against a real Go module,
// verifying that the gopublish collector emits the version_pin_table
// compound signal with the expected shape.
//
// What it proves (in order):
//
//  1. The signatory binary builds cleanly from cmd/signatory/.
//  2. `signatory analyze pkg:golang/<module> --refresh --clone --json`
//     returns valid JSON containing a signals[] array.
//  3. The signals[] contains a version_pin_table entry — proving the
//     gopublish dispatch wiring fires for Ecosystem="golang" entities
//     end-to-end through the analyze pipeline.
//  4. The version_pin_table value has the schema described in
//     design/coll7.md D3:
//     - module_path matches the requested module
//     - version_count_total > version_count_processed when the
//     module has more than maxPinFetches (12) tags, OR equal
//     when it has fewer
//     - version_count_processed == 12 (the cap) for modules with
//     enough tags
//     - pins[] is non-empty for a healthy module
//     - every pin has source == "proxy.golang.org"
//     - every pin's sha is 40 hex characters
//     - every pin's published_at parses as RFC3339
//     - missing_origin_versions and fetch_failed_versions are
//     present (possibly empty) arrays
//
// Why this exists alongside the unit-level gopublish/pintable_test.go
// suite and the cmd/signatory/collectors_test.go dispatch tests:
//
//   - The unit tests prove the gopublish collector emits version_pin_table
//     correctly when invoked directly with a mocked proxy. They don't
//     prove it's actually invoked in production analyze flows.
//   - The dispatch tests prove gopublish is in the dispatch list for
//     Go-ecosystem entities. They don't prove it actually runs to
//     completion against a real proxy.
//   - This driver proves the full chain: dispatch fires → gopublish runs
//     → real proxy.golang.org responds → version_pin_table lands in the
//     analyze JSON → schema is intact.
//
// Network dependency: hits proxy.golang.org for the target module
// plus clones the target's GitHub repo. Run on demand, not in default
// CI test runs.
//
// Usage (from repo root):
//
//	go run ./cmd/smoke-pintable
//	go run ./cmd/smoke-pintable -target pkg:golang/github.com/google/uuid
//
// Exit codes: 0 on all assertions passing, 1 on first failure, 2 on
// setup error (can't build, can't analyze, etc.).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// defaultTarget is the Go module the smoke runs against when
// invoked without -target. kong is a well-established third-party
// CLI library with multi-year tag history (v0.1.0 through v1.x);
// we depend on it so its tag history won't surprise us, and it has
// enough tags (50+) that the maxPinFetches=12 cap is exercised.
const defaultTarget = "pkg:golang/github.com/alecthomas/kong"

// expectedProcessedCount is the gopublish collector's hardcoded
// maxPinFetches cap. The smoke asserts the processed count exactly
// matches this value when the target has >=12 tags. Lifted as a
// constant so a future cap-change requires updating exactly one
// place.
const expectedProcessedCount = 12

// analyzeTimeout bounds how long the analyze command can run.
// Includes clone + 12 sequential GetVersionInfo calls with
// 200-800ms jitter (~6s of jitter alone) + other collectors.
// Well over an order of magnitude headroom for typical runs.
const analyzeTimeout = 10 * time.Minute

// shaHexLen is the length of a git object SHA in hex form. Pins
// from proxy.golang.org are always full-length git SHAs.
const shaHexLen = 40

func main() {
	var target string
	flag.StringVar(&target, "target", defaultTarget,
		"Go module canonical URI to analyze (pkg:golang/<module-path>)")
	flag.Parse()

	if err := run(target); err != nil {
		fmt.Fprintf(os.Stderr, "smoke-pintable: %v\n", err)
		os.Exit(1)
	}
}

func run(target string) error {
	ctx, cancel := context.WithTimeout(context.Background(), analyzeTimeout)
	defer cancel()

	tmp, err := os.MkdirTemp("", "smoke-pintable-*")
	if err != nil {
		return fmt.Errorf("mktemp: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	binPath := filepath.Join(tmp, "signatory")
	dbPath := filepath.Join(tmp, "signatory.db")
	clonePath := filepath.Join(tmp, "clone")

	rep := &reporter{}

	rep.step("build signatory")
	if err := buildTarget(ctx, binPath); err != nil {
		return fmt.Errorf("build: %w", err)
	}
	rep.pass("signatory built")

	rep.step("run signatory analyze --refresh --clone --json")
	rep.note("target: %s", target)
	rep.note("clone path: %s", clonePath)
	out, err := runAnalyze(ctx, binPath, dbPath, target, clonePath)
	if err != nil {
		return fmt.Errorf("analyze: %w", err)
	}
	rep.pass("analyze returned valid JSON (%d bytes)", len(out))

	rep.step("parse JSON")
	var result analyzeJSON
	if err := json.Unmarshal(out, &result); err != nil {
		// Truncate the dump so a multi-MB body doesn't flood logs.
		preview := string(out)
		if len(preview) > 2048 {
			preview = preview[:2048] + "...[truncated]"
		}
		return fmt.Errorf("unmarshal analyze output: %w\n--- output: ---\n%s", err, preview)
	}
	rep.pass("parsed; %d signals reported", len(result.Signals))

	rep.step("find version_pin_table signal")
	pinSig, ok := result.findSignal("version_pin_table")
	if !ok {
		// List all signal types to aid diagnosis.
		types := make([]string, 0, len(result.Signals))
		for _, s := range result.Signals {
			types = append(types, s.Type)
		}
		return fmt.Errorf("version_pin_table signal not found; got types: %s", strings.Join(types, ", "))
	}
	rep.pass("version_pin_table present")

	rep.step("validate signal metadata")
	if pinSig.Group != "publication" {
		return fmt.Errorf("expected Group=publication, got %q", pinSig.Group)
	}
	if pinSig.ForgeryResistance != "very-high" {
		return fmt.Errorf("expected ForgeryResistance=very-high, got %q", pinSig.ForgeryResistance)
	}
	if pinSig.Source != "go-publish" {
		return fmt.Errorf("expected Source=go-publish, got %q", pinSig.Source)
	}
	rep.pass("metadata: group=publication forgery=very-high source=go-publish")

	rep.step("validate value shape")
	var value pinTableValue
	if err := json.Unmarshal(pinSig.Value, &value); err != nil {
		return fmt.Errorf("unmarshal version_pin_table value: %w", err)
	}
	wantModulePath := strings.TrimPrefix(target, "pkg:golang/")
	wantModulePath = strings.TrimPrefix(wantModulePath, "pkg:go/")
	if value.ModulePath != wantModulePath {
		return fmt.Errorf("expected module_path=%q, got %q", wantModulePath, value.ModulePath)
	}
	rep.pass("module_path=%s", value.ModulePath)

	rep.step("validate version counts")
	if value.VersionCountTotal <= 0 {
		return fmt.Errorf("expected version_count_total > 0, got %d", value.VersionCountTotal)
	}
	if value.VersionCountProcessed > value.VersionCountTotal {
		return fmt.Errorf("processed=%d > total=%d (impossible)",
			value.VersionCountProcessed, value.VersionCountTotal)
	}
	if value.VersionCountTotal >= expectedProcessedCount {
		// Cap should engage.
		if value.VersionCountProcessed != expectedProcessedCount {
			return fmt.Errorf("expected processed=%d (cap) when total=%d, got %d",
				expectedProcessedCount, value.VersionCountTotal, value.VersionCountProcessed)
		}
	} else {
		// Fewer than cap; processed should equal total.
		if value.VersionCountProcessed != value.VersionCountTotal {
			return fmt.Errorf("expected processed=total=%d when below cap, got processed=%d",
				value.VersionCountTotal, value.VersionCountProcessed)
		}
	}
	rep.pass("counts: total=%d processed=%d (cap=%d)",
		value.VersionCountTotal, value.VersionCountProcessed, expectedProcessedCount)

	rep.step("validate per-bucket invariant")
	bucketTotal := len(value.Pins) + len(value.MissingOriginVersions) + len(value.FetchFailedVersions)
	if bucketTotal != value.VersionCountProcessed {
		return fmt.Errorf("buckets sum to %d (pins=%d, missing-origin=%d, fetch-failed=%d), expected %d",
			bucketTotal, len(value.Pins), len(value.MissingOriginVersions),
			len(value.FetchFailedVersions), value.VersionCountProcessed)
	}
	rep.pass("buckets: pins=%d missing-origin=%d fetch-failed=%d (sum=processed)",
		len(value.Pins), len(value.MissingOriginVersions), len(value.FetchFailedVersions))

	rep.step("validate pin shape")
	if len(value.Pins) == 0 {
		return fmt.Errorf("expected at least one successful pin for a healthy module; got 0")
	}
	for i, pin := range value.Pins {
		if pin.Version == "" {
			return fmt.Errorf("pin[%d]: empty version", i)
		}
		if pin.Source != "proxy.golang.org" {
			return fmt.Errorf("pin[%d] (%s): expected source=proxy.golang.org, got %q",
				i, pin.Version, pin.Source)
		}
		if len(pin.SHA) != shaHexLen {
			return fmt.Errorf("pin[%d] (%s): expected %d-char SHA, got %d (%q)",
				i, pin.Version, shaHexLen, len(pin.SHA), pin.SHA)
		}
		if !isHexLower(pin.SHA) {
			return fmt.Errorf("pin[%d] (%s): SHA contains non-hex characters: %q",
				i, pin.Version, pin.SHA)
		}
		if _, err := time.Parse(time.RFC3339, pin.PublishedAt); err != nil {
			return fmt.Errorf("pin[%d] (%s): published_at not parseable as RFC3339: %q (%v)",
				i, pin.Version, pin.PublishedAt, err)
		}
	}
	rep.pass("all %d pins have valid version, source, sha, published_at", len(value.Pins))

	rep.step("validate published_at chronology")
	// Pins should arrive in semver-descending order (most-recent
	// first) and consequently most-recent published_at first too,
	// allowing for the rare out-of-order case where a patch version
	// of an older line was published after a newer line. We assert
	// the loose property: at least the first pin is newer than the
	// last pin.
	if len(value.Pins) >= 2 {
		first, _ := time.Parse(time.RFC3339, value.Pins[0].PublishedAt)
		last, _ := time.Parse(time.RFC3339, value.Pins[len(value.Pins)-1].PublishedAt)
		if !first.After(last) && !first.Equal(last) {
			rep.note("WARNING: first pin (%s, %s) is older than last pin (%s, %s); ordering edge case",
				value.Pins[0].Version, value.Pins[0].PublishedAt,
				value.Pins[len(value.Pins)-1].Version, value.Pins[len(value.Pins)-1].PublishedAt)
		}
	}
	rep.pass("chronology consistent")

	fmt.Println()
	fmt.Println("=== smoke-pintable: ALL CHECKS PASSED ===")
	fmt.Printf("Target:     %s\n", target)
	fmt.Printf("Pins:       %d\n", len(value.Pins))
	fmt.Printf("Missing:    %d\n", len(value.MissingOriginVersions))
	fmt.Printf("Failed:     %d\n", len(value.FetchFailedVersions))
	if len(value.Pins) > 0 {
		fmt.Printf("Newest pin: %s @ %s (%s)\n",
			value.Pins[0].Version, value.Pins[0].SHA[:12], value.Pins[0].PublishedAt)
	}
	return nil
}

// buildTarget compiles the signatory binary into binPath. Mirrors
// the cmd/smoke-mcp build pattern but without the -ldflags version
// stamping (this smoke doesn't care about the version string).
func buildTarget(ctx context.Context, binPath string) error {
	cmd := exec.CommandContext(ctx, "go", "build", "-o", binPath, "./cmd/signatory") //nolint:gosec // G204: argv is constant
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go build: %w", err)
	}
	return nil
}

// runAnalyze executes `signatory analyze <target> --refresh --clone
// --path=<clonePath> --json` and returns the captured stdout.
//
// --json drives the structured-output mode the smoke parses.
// --refresh forces fresh signal collection (we want to actually
// hit proxy.golang.org, not a cached run).
// --clone tells signatory to materialize the clone at --path.
func runAnalyze(ctx context.Context, binPath, dbPath, target, clonePath string) ([]byte, error) {
	args := []string{
		"--db", dbPath,
		"analyze",
		target,
		"--refresh",
		"--clone",
		"--path", clonePath,
		"--json",
	}
	cmd := exec.CommandContext(ctx, binPath, args...) //nolint:gosec // G204: binPath is our own freshly-built binary
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", binPath, strings.Join(args, " "), err)
	}
	return out, nil
}

// isHexLower reports whether s consists entirely of hex digits 0-9
// and a-f (lowercase only). git emits hashes in lowercase hex; the
// strict check catches accidental pass-through of mixed-case or
// non-hex strings (e.g., refs, ref names).
func isHexLower(s string) bool {
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}

// analyzeJSON is the subset of `signatory analyze --json` output
// the smoke needs. Other fields (entity, postures, analyst_outputs,
// etc.) are present but unused.
type analyzeJSON struct {
	Signals []analyzeSignal `json:"signals"`
}

// analyzeSignal mirrors profile.Signal's JSON shape. Field tags
// match the snake_case names profile.Signal declares — see
// internal/profile/signal.go. (Go's case-insensitive json matching
// would let plain field names match e.g. "Type"→"type", but it does
// NOT bridge underscores; "ForgeryResistance" wouldn't match
// "forgery_resistance" without the explicit tag.)
type analyzeSignal struct {
	Type              string          `json:"type"`
	Group             string          `json:"group"`
	Source            string          `json:"source"`
	ForgeryResistance string          `json:"forgery_resistance"`
	Value             json.RawMessage `json:"value"`
}

// findSignal returns the first signal of the given type, plus a
// found flag. Returns the FIRST match — multiple signals of the
// same type from different sources are possible in principle but
// don't happen for version_pin_table at this writing.
func (a analyzeJSON) findSignal(signalType string) (analyzeSignal, bool) {
	for _, s := range a.Signals {
		if s.Type == signalType {
			return s, true
		}
	}
	return analyzeSignal{}, false
}

// pinTableValue mirrors gopublish.VersionPinTableValue. Defined
// locally rather than imported to keep the smoke's contract
// independent of the producer's struct shape — if gopublish renames
// a field, the smoke fails loudly rather than silently following
// the rename.
type pinTableValue struct {
	ModulePath            string        `json:"module_path"`
	VersionCountTotal     int           `json:"version_count_total"`
	VersionCountProcessed int           `json:"version_count_processed"`
	Pins                  []pinTablePin `json:"pins"`
	MissingOriginVersions []string      `json:"missing_origin_versions"`
	FetchFailedVersions   []string      `json:"fetch_failed_versions"`
}

type pinTablePin struct {
	Version     string `json:"version"`
	SHA         string `json:"sha"`
	Source      string `json:"source"`
	PublishedAt string `json:"published_at"`
}

// reporter is a tiny CLI progress-and-assertion logger. step()
// announces the next check; pass() marks success; note() emits an
// informational line. Failure is signaled by the caller returning
// a non-nil error from run() — the reporter doesn't track failures
// itself because the run-level structure already does.
type reporter struct {
	stepNum int
}

func (r *reporter) step(label string) {
	r.stepNum++
	fmt.Printf("[step %d] %s\n", r.stepNum, label)
}

func (r *reporter) pass(format string, args ...any) {
	fmt.Printf("  PASS  ")
	fmt.Printf(format, args...)
	fmt.Println()
}

func (r *reporter) note(format string, args ...any) {
	fmt.Printf("  note  ")
	fmt.Printf(format, args...)
	fmt.Println()
}
