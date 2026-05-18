// Command smoke-source-evolution drives an end-to-end test against
// a freshly-built `signatory analyze` for a real Go module,
// verifying that the THREE source-evolution signals land with their
// expected schemas:
//
//   - version_pin_table       (gopublish-emitted)
//   - source_evolution_matrix (source-collector-emitted)
//   - source_evolution_anomaly (source-collector-emitted)
//
// What it proves (in order):
//
//  1. The signatory binary builds cleanly from cmd/signatory/.
//  2. `signatory analyze pkg:golang/<module> --refresh --clone --json`
//     returns valid JSON containing a signals[] array.
//  3. signals[] contains all three source-evolution signals — proving
//     the gopublish dispatch + source-evolution dispatch + ordering
//     constraint (gopublish before source-evolution) all hold under
//     real network + real git.
//  4. version_pin_table schema (cap=12, pin shape, RFC3339 timestamps,
//     bucket invariants).
//  5. source_evolution_matrix schema:
//     - module_path matches target
//     - rows count > 0 and bounded by budget (LastN+MajorLeaves
//     under HardCap; for kong's single-major v1.x history,
//     exactly LastN=12)
//     - present rows have AST + Structural populated
//     - present rows' tag_sha is 40-char lowercase hex
//     - tag_sha values appear in version_pin_table.pins (cross-
//     signal consistency: matrix anchors to gopublish's pin)
//  6. source_evolution_anomaly schema:
//     - For a healthy library (kong is the canonical baseline):
//     AnomalyPresent == false (no false-positive on legitimate
//     evolution). This is the load-bearing check that the
//     multi-feature joint threshold is conservative enough.
//
// Why this exists alongside the unit + dispatch test layers:
//
//   - Unit tests (internal/signal/source/{anomaly,matrix,...}_test.go):
//     prove each component in isolation against fakes/mocks.
//   - Dispatch tests (cmd/signatory/collectors_test.go): prove the
//     collector is wired into dispatch with correct order.
//   - This driver proves the full chain end-to-end against real
//     proxy.golang.org, real github clone, real git cat-file --batch,
//     real AST analysis, real anomaly detection, real signal
//     emission. The gap between "all unit + integration tests pass"
//     and "the deployed pipeline produces what we expect on a real
//     module" is exactly what this catches.
//
// Network + git dependency: hits proxy.golang.org for the target
// module, clones the target's GitHub repo, runs cat-file across 12
// versions. Run on demand, not in default CI test runs. Total
// runtime ~30-60 seconds depending on network.
//
// Usage (from repo root):
//
//	go run ./cmd/smoke-source-evolution
//	go run ./cmd/smoke-source-evolution -target pkg:golang/github.com/google/uuid
//	go run ./cmd/smoke-source-evolution -target pkg:golang/github.com/hashicorp/go-retryablehttp
//
// The hashicorp/go-retryablehttp run is particularly meaningful: it's
// the canonical legitimate ancestor that the BufferZoneCorp campaign
// typosquatted. Asserting AnomalyPresent==false against go-retryablehttp
// is the proof that the matrix's threshold doesn't false-positive on
// the very library the typosquats were imitating.
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
// CLI library with multi-year tag history; we depend on it so its
// tag history won't surprise us, and it has enough tags (50+) that
// the maxPinFetches=12 cap is exercised.
const defaultTarget = "pkg:golang/github.com/alecthomas/kong"

// expectedProcessedCount is the gopublish collector's hardcoded
// maxPinFetches cap. Lifted as a constant so a future cap change
// requires updating exactly one place.
const expectedProcessedCount = 12

// analyzeTimeout bounds how long the analyze command can run.
// Includes clone + 12 sequential GetVersionInfo calls with
// 200-800ms jitter (~6s of jitter alone) + every other collector
// (github API, openssf-scorecard, source-evolution's git
// cat-file/ls-tree across 12 versions). Well over an order of
// magnitude headroom for typical runs.
const analyzeTimeout = 10 * time.Minute

// shaHexLen is the length of a git object SHA in hex form.
const shaHexLen = 40

func main() {
	var target string
	flag.StringVar(&target, "target", defaultTarget,
		"Go module canonical URI to analyze (pkg:golang/<module-path>)")
	flag.Parse()

	if err := run(target); err != nil {
		fmt.Fprintf(os.Stderr, "smoke-source-evolution: %v\n", err)
		os.Exit(1)
	}
}

func run(target string) error {
	ctx, cancel := context.WithTimeout(context.Background(), analyzeTimeout)
	defer cancel()

	tmp, err := os.MkdirTemp("", "smoke-source-evolution-*")
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
		preview := string(out)
		if len(preview) > 2048 {
			preview = preview[:2048] + "...[truncated]"
		}
		return fmt.Errorf("unmarshal analyze output: %w\n--- output: ---\n%s", err, preview)
	}
	rep.pass("parsed; %d signals reported", len(result.Signals))

	wantModulePath := strings.TrimPrefix(target, "pkg:golang/")
	wantModulePath = strings.TrimPrefix(wantModulePath, "pkg:go/")

	pinTable, err := validatePinTable(result, wantModulePath, rep)
	if err != nil {
		return err
	}

	matrix, presentCount, err := validateSourceMatrix(result, wantModulePath, rep)
	if err != nil {
		return err
	}

	if err := validateCrossSignalAnchors(matrix, pinTable, rep); err != nil {
		return err
	}

	anomaly, err := validateSourceAnomaly(result, rep)
	if err != nil {
		return err
	}

	printSmokeSummary(target, pinTable, matrix, anomaly, presentCount)
	return nil
}

// validateMetadata checks the per-signal metadata fields against
// expected values. Centralized so the per-signal blocks above
// don't repeat the same error-message boilerplate.
func validateMetadata(s analyzeSignal, group, forgery, source string) error {
	if s.Group != group {
		return fmt.Errorf("expected Group=%q, got %q", group, s.Group)
	}
	if s.ForgeryResistance != forgery {
		return fmt.Errorf("expected ForgeryResistance=%q, got %q", forgery, s.ForgeryResistance)
	}
	if s.Source != source {
		return fmt.Errorf("expected Source=%q, got %q", source, s.Source)
	}
	return nil
}

// validatePinTableCounts checks the version_pin_table count
// invariants: total > 0, processed bounded by total, cap engaged
// when total >= cap, sum of buckets == processed.
func validatePinTableCounts(pt pinTableValue) error {
	if pt.VersionCountTotal <= 0 {
		return fmt.Errorf("expected version_count_total > 0, got %d", pt.VersionCountTotal)
	}
	if pt.VersionCountProcessed > pt.VersionCountTotal {
		return fmt.Errorf("processed=%d > total=%d (impossible)",
			pt.VersionCountProcessed, pt.VersionCountTotal)
	}
	if pt.VersionCountTotal >= expectedProcessedCount {
		if pt.VersionCountProcessed != expectedProcessedCount {
			return fmt.Errorf("expected processed=%d (cap) when total=%d, got %d",
				expectedProcessedCount, pt.VersionCountTotal, pt.VersionCountProcessed)
		}
	} else {
		if pt.VersionCountProcessed != pt.VersionCountTotal {
			return fmt.Errorf("expected processed=total=%d when below cap, got processed=%d",
				pt.VersionCountTotal, pt.VersionCountProcessed)
		}
	}
	bucketTotal := len(pt.Pins) + len(pt.MissingOriginVersions) + len(pt.FetchFailedVersions)
	if bucketTotal != pt.VersionCountProcessed {
		return fmt.Errorf("buckets sum to %d (pins=%d, missing-origin=%d, fetch-failed=%d), expected %d",
			bucketTotal, len(pt.Pins), len(pt.MissingOriginVersions),
			len(pt.FetchFailedVersions), pt.VersionCountProcessed)
	}
	return nil
}

// validatePinShape walks every pin and asserts version, source,
// SHA hex form, and RFC3339 published_at.
func validatePinShape(pins []pinTablePin) error {
	if len(pins) == 0 {
		return fmt.Errorf("expected at least one successful pin for a healthy module; got 0")
	}
	for i, pin := range pins {
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
			return fmt.Errorf("pin[%d] (%s): published_at %q not parseable as RFC3339: %w",
				i, pin.Version, pin.PublishedAt, err)
		}
	}
	return nil
}

// enumerateMissingSignal builds an error that names what was
// expected and lists every signal type actually present, so a
// future smoke regression has the diagnostic data inline rather
// than requiring re-runs with extra logging.
func enumerateMissingSignal(result analyzeJSON, expected string) error {
	types := make([]string, 0, len(result.Signals))
	for _, s := range result.Signals {
		types = append(types, s.Type)
	}
	return fmt.Errorf("%s signal not found; got types: %s", expected, strings.Join(types, ", "))
}

// validatePinTable finds the version_pin_table signal in the analyze
// output, validates its metadata, parses its value, and runs full
// shape checks (module_path match, count bounds, per-pin shape).
// Returns the parsed pinTableValue for use by the cross-signal
// anchor check that runs once both pin table and matrix are
// in hand.
func validatePinTable(result analyzeJSON, wantModulePath string, rep *reporter) (pinTableValue, error) {
	rep.step("find version_pin_table signal")
	sig, ok := result.findSignal("version_pin_table")
	if !ok {
		return pinTableValue{}, enumerateMissingSignal(result, "version_pin_table")
	}
	rep.pass("version_pin_table present")

	rep.step("validate version_pin_table metadata")
	if err := validateMetadata(sig, "publication", "very-high", "go-publish"); err != nil {
		return pinTableValue{}, err
	}
	rep.pass("metadata: group=publication forgery=very-high source=go-publish")

	rep.step("validate version_pin_table value shape")
	var pt pinTableValue
	if err := json.Unmarshal(sig.Value, &pt); err != nil {
		return pinTableValue{}, fmt.Errorf("unmarshal version_pin_table value: %w", err)
	}
	if pt.ModulePath != wantModulePath {
		return pinTableValue{}, fmt.Errorf("expected module_path=%q, got %q", wantModulePath, pt.ModulePath)
	}
	if err := validatePinTableCounts(pt); err != nil {
		return pinTableValue{}, err
	}
	if err := validatePinShape(pt.Pins); err != nil {
		return pinTableValue{}, err
	}
	rep.pass("counts: total=%d processed=%d (cap=%d), %d pins all valid",
		pt.VersionCountTotal, pt.VersionCountProcessed,
		expectedProcessedCount, len(pt.Pins))
	return pt, nil
}

// validateSourceMatrix finds the source_evolution_matrix signal,
// validates its metadata + envelope (module_path, ecosystem, row
// bounds), then delegates per-row shape and diff-plumbing checks
// to focused helpers. Returns the parsed matrix and the count of
// rows whose TagSHALocalStatus is "present" — the latter is needed
// by the summary printer and by the cross-signal anchor check.
func validateSourceMatrix(result analyzeJSON, wantModulePath string, rep *reporter) (matrixValue, int, error) {
	rep.step("find source_evolution_matrix signal")
	sig, ok := result.findSignal("source_evolution_matrix")
	if !ok {
		return matrixValue{}, 0, enumerateMissingSignal(result, "source_evolution_matrix")
	}
	rep.pass("source_evolution_matrix present")

	rep.step("validate source_evolution_matrix metadata")
	if err := validateMetadata(sig, "publication", "very-high", "source-evolution"); err != nil {
		return matrixValue{}, 0, err
	}
	rep.pass("metadata: group=publication forgery=very-high source=source-evolution")

	rep.step("validate source_evolution_matrix value shape")
	var m matrixValue
	if err := json.Unmarshal(sig.Value, &m); err != nil {
		return matrixValue{}, 0, fmt.Errorf("unmarshal source_evolution_matrix value: %w", err)
	}
	if m.ModulePath != wantModulePath {
		return matrixValue{}, 0, fmt.Errorf("matrix.module_path: expected %q, got %q", wantModulePath, m.ModulePath)
	}
	if m.Ecosystem != "go" {
		return matrixValue{}, 0, fmt.Errorf("matrix.ecosystem: expected \"go\", got %q", m.Ecosystem)
	}
	if len(m.Rows) == 0 {
		return matrixValue{}, 0, fmt.Errorf("matrix has zero rows; expected > 0 for a healthy module")
	}
	// LastN=12 default. For a single-major history (kong is all
	// v1.x in its recent 12), MajorLeaves contributes 0 and
	// HardCap doesn't engage. Multi-major modules see up to 16.
	if len(m.Rows) > 20 {
		return matrixValue{}, 0, fmt.Errorf("matrix has %d rows; HardCap=20 should bound this", len(m.Rows))
	}
	rep.pass("matrix: module_path=%s ecosystem=go rows=%d",
		m.ModulePath, len(m.Rows))

	rep.step("validate matrix row shape")
	presentCount, err := validateMatrixRows(m.Rows)
	if err != nil {
		return matrixValue{}, 0, err
	}
	if presentCount == 0 {
		return matrixValue{}, 0, fmt.Errorf("matrix has %d rows but none are present; expected most rows present for a healthy module", len(m.Rows))
	}
	rep.pass("rows: %d total, %d present, all status enum values valid",
		len(m.Rows), presentCount)

	rep.step("validate cross-version diff plumbing")
	if err := validateMatrixDiffPlumbing(m.Rows); err != nil {
		return matrixValue{}, 0, err
	}
	rep.pass("diff_from_previous: populated on newer rows, nil on oldest")

	return m, presentCount, nil
}

// validateMatrixRows enforces the per-row shape contract: every row
// must have a Version and TagSHALocalStatus; rows whose status is
// "present" must have AST/Structural populated and a 40-char lower-
// hex tag_sha sourced from proxy.golang.org. Other status values
// (missing_from_clone, missing_origin, fetch_failed) are partial
// rows where analysis blocks may be nil — no further shape
// constraints at this level. Returns the count of "present" rows
// so the caller can assert "at least one fresh row" downstream.
func validateMatrixRows(rows []matrixRow) (int, error) {
	presentCount := 0
	for i, r := range rows {
		if r.Version == "" {
			return 0, fmt.Errorf("row[%d]: empty version", i)
		}
		if r.TagSHALocalStatus == "" {
			return 0, fmt.Errorf("row[%d] (%s): empty tag_sha_local_status", i, r.Version)
		}
		switch r.TagSHALocalStatus {
		case "present":
			presentCount++
			if r.AST == nil {
				return 0, fmt.Errorf("row[%d] (%s, present): AST is nil", i, r.Version)
			}
			if r.Structural == nil {
				return 0, fmt.Errorf("row[%d] (%s, present): Structural is nil", i, r.Version)
			}
			if len(r.TagSHA) != shaHexLen {
				return 0, fmt.Errorf("row[%d] (%s): expected %d-char tag_sha, got %d (%q)",
					i, r.Version, shaHexLen, len(r.TagSHA), r.TagSHA)
			}
			if !isHexLower(r.TagSHA) {
				return 0, fmt.Errorf("row[%d] (%s): tag_sha contains non-hex: %q", i, r.Version, r.TagSHA)
			}
			if r.TagSHASource != "proxy.golang.org" {
				return 0, fmt.Errorf("row[%d] (%s): expected tag_sha_source=proxy.golang.org, got %q",
					i, r.Version, r.TagSHASource)
			}
		case "missing_from_clone", "missing_origin", "fetch_failed":
			// Partial row — analysis blocks may be nil. No
			// further shape constraints at this level.
		default:
			return 0, fmt.Errorf("row[%d] (%s): unknown tag_sha_local_status %q",
				i, r.Version, r.TagSHALocalStatus)
		}
	}
	return presentCount, nil
}

// validateMatrixDiffPlumbing verifies the diff-from-previous plumbing
// at the row endpoints: the newest row should have DiffFromPrevious
// populated when there's at least one previous "present" row to
// diff against, and the oldest row should always have
// DiffFromPrevious nil (nothing earlier to diff from).
func validateMatrixDiffPlumbing(rows []matrixRow) error {
	if len(rows) >= 2 && rows[0].TagSHALocalStatus == "present" &&
		rows[1].TagSHALocalStatus == "present" {
		if rows[0].DiffFromPrevious == nil {
			return fmt.Errorf("newest row's DiffFromPrevious is nil but both rows are present")
		}
	}
	last := rows[len(rows)-1]
	if last.DiffFromPrevious != nil {
		return fmt.Errorf("oldest row (%s) has non-nil DiffFromPrevious; expected nil", last.Version)
	}
	return nil
}

// validateCrossSignalAnchors confirms every "present" row's tag_sha
// is also a pin in the version_pin_table. Proves both signals were
// derived from the same anchor set, not independent (and possibly
// divergent) views of the module's history. A miss here means the
// matrix and pin table disagree on which versions are real, which
// would invalidate the pin table's role as the matrix's anchor.
func validateCrossSignalAnchors(matrix matrixValue, pt pinTableValue, rep *reporter) error {
	rep.step("cross-signal: every present row's tag_sha is in pin table")
	pinSHAs := make(map[string]struct{}, len(pt.Pins))
	for _, p := range pt.Pins {
		pinSHAs[p.SHA] = struct{}{}
	}
	for _, r := range matrix.Rows {
		if r.TagSHALocalStatus != "present" {
			continue
		}
		if _, ok := pinSHAs[r.TagSHA]; !ok {
			return fmt.Errorf("row (%s) tag_sha=%s not found in version_pin_table.pins — cross-signal anchor broken",
				r.Version, r.TagSHA)
		}
	}
	rep.pass("all present rows anchor to a pin in version_pin_table")
	return nil
}

// validateSourceAnomaly finds the source_evolution_anomaly signal,
// validates its metadata, parses its value, and asserts the
// load-bearing healthy-library invariant: a well-maintained library
// (kong, go-retryablehttp, google/uuid) must NOT fire the anomaly.
// Firing on a clean target indicates the joint threshold
// (MinSpikedFeatures=2) is too aggressive and would false-positive
// in production. Failing this assertion is the smoke binary's whole
// reason to exist.
func validateSourceAnomaly(result analyzeJSON, rep *reporter) (anomalyValue, error) {
	rep.step("find source_evolution_anomaly signal")
	sig, ok := result.findSignal("source_evolution_anomaly")
	if !ok {
		return anomalyValue{}, enumerateMissingSignal(result, "source_evolution_anomaly")
	}
	rep.pass("source_evolution_anomaly present")

	rep.step("validate source_evolution_anomaly metadata")
	if err := validateMetadata(sig, "publication", "very-high", "source-evolution"); err != nil {
		return anomalyValue{}, err
	}
	rep.pass("metadata: group=publication forgery=very-high source=source-evolution")

	rep.step("validate anomaly value shape — healthy library should not fire anomaly")
	var a anomalyValue
	if err := json.Unmarshal(sig.Value, &a); err != nil {
		return anomalyValue{}, fmt.Errorf("unmarshal source_evolution_anomaly value: %w", err)
	}
	if a.AnomalyPresent {
		return anomalyValue{}, fmt.Errorf(
			"anomaly fired on healthy library — false positive!\n"+
				"  first_anomalous_version: %s\n"+
				"  previous_version:        %s\n"+
				"  spiked_features:         %v\n"+
				"  This indicates the multi-feature joint threshold (MinSpikedFeatures=2)\n"+
				"  is too sensitive for legitimate package evolution. Either the target's\n"+
				"  history has unusual features that read as a spike, or the threshold\n"+
				"  needs tightening (more features, or near-zero baseline tolerance)",
			a.FirstAnomalousVersion, a.PreviousVersion, a.SpikedFeatures)
	}
	rep.pass("anomaly_present=false (no false positive on healthy library)")
	return a, nil
}

// printSmokeSummary writes the final all-checks-passed banner plus
// a one-line view of each signal's headline numbers — pins+missing
// counts for the pin table, total+present row counts for the
// matrix, anomaly_present for the anomaly. When the newest row is
// "present", also prints its short SHA and AST feature counts so
// dogfood operators can spot-check what the matrix actually saw.
func printSmokeSummary(
	target string,
	pinTable pinTableValue,
	matrix matrixValue,
	anomaly anomalyValue,
	presentCount int,
) {
	fmt.Println()
	fmt.Println("=== smoke-source-evolution: ALL CHECKS PASSED ===")
	fmt.Printf("Target:                %s\n", target)
	fmt.Printf("version_pin_table:     %d pins, %d missing-origin, %d fetch-failed\n",
		len(pinTable.Pins), len(pinTable.MissingOriginVersions), len(pinTable.FetchFailedVersions))
	fmt.Printf("matrix:                %d rows (%d present)\n", len(matrix.Rows), presentCount)
	fmt.Printf("anomaly_present:       %v (expected: false on healthy library)\n", anomaly.AnomalyPresent)
	if len(matrix.Rows) > 0 && matrix.Rows[0].TagSHALocalStatus == "present" {
		newest := matrix.Rows[0]
		fmt.Printf("Newest row:            %s @ %s\n",
			newest.Version, newest.TagSHA[:12])
		if newest.AST != nil {
			fmt.Printf("Newest row AST:        init=%d network=%d sensitive=%d exec=%d xor=%d base64=%d\n",
				newest.AST.InitCount, newest.AST.NetworkCallSites,
				newest.AST.SensitivePathReads, newest.AST.ExecCalls,
				newest.AST.XORAssignments, newest.AST.Base64DecodeCalls)
		}
	}
}

// buildTarget compiles the signatory binary into binPath.
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

// isHexLower reports whether s is all hex digits 0-9 + a-f.
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

// =====================================================
// JSON shape mirrors
// =====================================================

// analyzeJSON is the subset of `signatory analyze --json` output
// the smoke needs.
type analyzeJSON struct {
	Signals []analyzeSignal `json:"signals"`
}

// analyzeSignal mirrors profile.Signal's snake_case JSON shape.
type analyzeSignal struct {
	Type              string          `json:"type"`
	Group             string          `json:"group"`
	Source            string          `json:"source"`
	ForgeryResistance string          `json:"forgery_resistance"`
	Value             json.RawMessage `json:"value"`
}

func (a analyzeJSON) findSignal(signalType string) (analyzeSignal, bool) {
	for _, s := range a.Signals {
		if s.Type == signalType {
			return s, true
		}
	}
	return analyzeSignal{}, false
}

// pinTableValue mirrors gopublish.VersionPinTableValue. Defined
// locally so the smoke's contract stays independent of the
// producer's struct shape — a JSON-tag rename in gopublish breaks
// the smoke loudly rather than silently following along.
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

// matrixValue mirrors source.MatrixValue.
type matrixValue struct {
	ModulePath string      `json:"module_path"`
	Ecosystem  string      `json:"ecosystem"`
	Language   string      `json:"language"`
	Rows       []matrixRow `json:"rows"`
}

// matrixRow mirrors source.MatrixRow. AST/Structural/DiffFromPrevious
// are nullable in JSON for non-present rows.
type matrixRow struct {
	Version           string            `json:"version"`
	TagSHA            string            `json:"tag_sha"`
	TagSHASource      string            `json:"tag_sha_source"`
	TagSHALocalStatus string            `json:"tag_sha_local_status"`
	AST               *matrixAST        `json:"ast"`
	Structural        *matrixStructural `json:"structural"`
	DiffFromPrevious  *matrixDiffStat   `json:"diff_from_previous"`
}

// matrixAST mirrors astfeature.Counts (snake_case JSON tags).
type matrixAST struct {
	InitCount          int `json:"init_count"`
	NetworkCallSites   int `json:"network_call_sites"`
	SensitivePathReads int `json:"sensitive_path_reads"`
	ExecCalls          int `json:"exec_calls"`
	XORAssignments     int `json:"xor_assignments"`
	Base64DecodeCalls  int `json:"base64_decode_calls"`
}

// matrixStructural mirrors source.Structural.
type matrixStructural struct {
	FileCount           int      `json:"file_count"`
	LOC                 int      `json:"loc"`
	NewTopLevelPackages []string `json:"new_top_level_packages"`
	NewSymbolExports    []string `json:"new_symbol_exports"`
}

// matrixDiffStat mirrors source.DiffStat.
type matrixDiffStat struct {
	FilesAdded   int `json:"files_added"`
	FilesChanged int `json:"files_changed"`
	FilesRemoved int `json:"files_removed"`
	LinesAdded   int `json:"lines_added"`
	LinesRemoved int `json:"lines_removed"`
}

// anomalyValue mirrors source.AnomalyValue.
type anomalyValue struct {
	AnomalyPresent        bool     `json:"anomaly_present"`
	FirstAnomalousVersion string   `json:"first_anomalous_version,omitempty"`
	PreviousVersion       string   `json:"previous_version,omitempty"`
	SpikedFeatures        []string `json:"spiked_features,omitempty"`
}

// =====================================================
// reporter — tiny CLI progress logger
// =====================================================

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
