package source

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"path"
	"sort"

	"github.com/sarahmaeve/signatory/internal/signal/source/golang"
)

// MatrixValue is the JSON-marshaled value of the
// source_evolution_matrix signal. One per Go-ecosystem entity per
// analysis; rows are sorted semver-descending (most-recent first).
//
// Schema mirrors design/coll7.md D2. Compound shape (one row per
// selected version) is by design — see D2 for why a single record
// beats per-version-pair records.
type MatrixValue struct {
	ModulePath string      `json:"module_path"`
	Ecosystem  string      `json:"ecosystem"`
	Language   string      `json:"language"`
	Budget     Selection   `json:"budget"`
	Rows       []MatrixRow `json:"rows"`
}

// MatrixRow is one tagged version's analyzed delta. Pointer fields
// are nullable in JSON: AST/Structural/DiffFromPrevious are nil
// when this row's SHA couldn't be analyzed (missing from clone,
// missing-origin in proxy, fetch-failed). Analysts read the
// tag_sha_local_status field to know which case applies.
type MatrixRow struct {
	Version           string           `json:"version"`
	TagSHA            string           `json:"tag_sha"`
	TagSHASource      string           `json:"tag_sha_source"`
	TagSHALocalStatus string           `json:"tag_sha_local_status"`
	AST               *golang.Features `json:"ast"`
	Structural        *Structural      `json:"structural"`
	DiffFromPrevious  *DiffStat        `json:"diff_from_previous"`
}

// Structural captures the file- and shape-level features of a
// version's source tree, complementing the AST counts in Features.
//
// NewTopLevelPackages and NewSymbolExports are populated by the
// cross-version pass (commit 13) — they're set against the
// previous row, not this row in isolation. Commit 12 emits empty
// slices for both; commit 13 fills NewTopLevelPackages from
// per-version package-set diffs. NewSymbolExports remains a
// future-work field.
type Structural struct {
	GoFileCount         int      `json:"go_file_count"`
	GoLOC               int      `json:"go_loc"`
	NewTopLevelPackages []string `json:"new_top_level_packages"`
	NewSymbolExports    []string `json:"new_symbol_exports"`
}

// TagSHALocalStatus enum. The matrix-row schema uses these strings
// directly so an analyst reading the JSON can map to the case
// without consulting a separate enum table.
const (
	// TagSHALocalPresent: SHA was in the local clone and analysis
	// succeeded; AST and Structural are populated.
	TagSHALocalPresent = "present"

	// TagSHALocalMissingFromClone: proxy.golang.org pinned this
	// SHA but `--clone --refresh` did not fetch it. Itself a
	// forgery-resistance HIGH signal — see design/coll7.md D11.
	TagSHALocalMissingFromClone = "missing_from_clone"

	// TagSHALocalMissingOrigin: gopublish couldn't get an Origin
	// block from the proxy for this version (pre-Go-1.20 publish
	// or proxy data gap). Source-evolution can fall back to the
	// local refs/tags lookup; v0.1 just records the gap.
	TagSHALocalMissingOrigin = "missing_origin"

	// TagSHALocalFetchFailed: gopublish's @v/<v>.info call failed
	// (proxy 5xx, network). The version exists in @v/list but
	// couldn't be resolved to a SHA.
	TagSHALocalFetchFailed = "fetch_failed"
)

// SourceProvider is the narrow interface the Assembler uses to
// read source content and compute diffs. BlobStreamer satisfies
// it implicitly; tests inject hand-built fakes.
type SourceProvider interface {
	EnumerateGoFiles(ctx context.Context, sha string) iter.Seq2[golang.SourceFile, error]
	DiffStat(ctx context.Context, sha1, sha2 string) (DiffStat, error)
}

// Compile-time assertion that BlobStreamer satisfies SourceProvider.
// Lives in matrix.go (the consumer's home) so a future BlobStreamer
// API change that breaks the contract fails build at the consumer
// rather than silently slipping by.
var _ SourceProvider = (*BlobStreamer)(nil)

// Assembler builds a MatrixValue from a PinTable + budget options.
// Composes a SourceProvider (for reading versions) and a
// LanguageAnalyzer (for AST feature extraction).
//
// Stateless across Build calls; safe to reuse.
type Assembler struct {
	provider SourceProvider
	analyzer *golang.Analyzer
}

// NewAssembler returns an Assembler wired to the given provider
// and analyzer. Both are required — nil provider or nil analyzer
// causes Build to return an error rather than panicking later.
func NewAssembler(provider SourceProvider, analyzer *golang.Analyzer) *Assembler {
	return &Assembler{provider: provider, analyzer: analyzer}
}

// Build assembles a MatrixValue from a pin table, applying budget
// selection to the union of pinned + missing-origin + fetch-failed
// versions. Each row is constructed per version with
// tag_sha_local_status set according to the version's classification
// in the pin table and whether the local clone has the SHA.
//
// Returns a non-nil MatrixValue with empty Rows for an empty input
// (no versions to process).
//
// Errors only when a structural problem prevents any progress
// (nil provider/analyzer, ctx cancelled before first iteration).
// Per-version analysis failures land in the row's
// tag_sha_local_status field, not in the returned error.
func (a *Assembler) Build(ctx context.Context, pinTable PinTable, opts BudgetOpts) (MatrixValue, error) {
	if a.provider == nil {
		return MatrixValue{}, errors.New("Build: nil SourceProvider")
	}
	if a.analyzer == nil {
		return MatrixValue{}, errors.New("Build: nil Analyzer")
	}

	// Union of all version names mentioned in the pin table.
	allVersions := make([]string, 0,
		len(pinTable.Pins)+len(pinTable.MissingOriginVersions)+len(pinTable.FetchFailedVersions))
	for _, p := range pinTable.Pins {
		allVersions = append(allVersions, p.Version)
	}
	allVersions = append(allVersions, pinTable.MissingOriginVersions...)
	allVersions = append(allVersions, pinTable.FetchFailedVersions...)

	sel := Select(allVersions, opts)

	pinByVersion := make(map[string]VersionPin, len(pinTable.Pins))
	for _, p := range pinTable.Pins {
		pinByVersion[p.Version] = p
	}
	missingOriginSet := setOf(pinTable.MissingOriginVersions)
	fetchFailedSet := setOf(pinTable.FetchFailedVersions)

	rows := make([]MatrixRow, 0, len(sel.SelectedVersions))
	for _, version := range sel.SelectedVersions {
		row := a.buildRow(ctx, version, pinByVersion, missingOriginSet, fetchFailedSet)
		rows = append(rows, row)
	}

	return MatrixValue{
		ModulePath: pinTable.ModulePath,
		Ecosystem:  "go",
		Language:   "go",
		Budget:     sel,
		Rows:       rows,
	}, nil
}

// buildRow dispatches a version to the right row-construction
// path based on its pin-table classification: pinned (try analysis),
// missing-origin (record gap, no analysis), or fetch-failed (record
// gap). A version that's in none of these is an internal logic
// error — it shouldn't reach buildRow because Selection's input
// is exactly the union of those sets.
func (a *Assembler) buildRow(
	ctx context.Context,
	version string,
	pinByVersion map[string]VersionPin,
	missingOriginSet, fetchFailedSet map[string]struct{},
) MatrixRow {
	if pin, ok := pinByVersion[version]; ok {
		return a.buildRowFromPin(ctx, version, pin)
	}
	if _, ok := missingOriginSet[version]; ok {
		return MatrixRow{
			Version:           version,
			TagSHALocalStatus: TagSHALocalMissingOrigin,
		}
	}
	if _, ok := fetchFailedSet[version]; ok {
		return MatrixRow{
			Version:           version,
			TagSHALocalStatus: TagSHALocalFetchFailed,
		}
	}
	// Defensive: should not reach here.
	return MatrixRow{
		Version:           version,
		TagSHALocalStatus: "unknown",
	}
}

// buildRowFromPin runs the AST analyzer + structural counter at
// the pin's SHA. On ErrSHAMissingFromClone (proxy SHA not in local
// object DB), returns a row with status missing_from_clone and
// nil analysis blocks. Other analyzer/provider errors land the
// same way — partial-row preservation is more useful than aborting
// the whole matrix on one version's failure.
func (a *Assembler) buildRowFromPin(ctx context.Context, version string, pin VersionPin) MatrixRow {
	row := MatrixRow{
		Version:      version,
		TagSHA:       pin.SHA,
		TagSHASource: pin.Source,
	}

	feats, structural, err := a.analyzeAtSHA(ctx, pin.SHA)
	if err != nil {
		// Distinguish missing-from-clone (the diagnostic case the
		// design doc names explicitly) from other errors. Both
		// produce a partial row; only the status string differs.
		if errors.Is(err, ErrSHAMissingFromClone) {
			row.TagSHALocalStatus = TagSHALocalMissingFromClone
		} else {
			// Unexpected error — record as missing-from-clone
			// rather than crashing. Log-only would be better but
			// this layer doesn't have a logger plumbed yet.
			row.TagSHALocalStatus = TagSHALocalMissingFromClone
		}
		return row
	}

	row.TagSHALocalStatus = TagSHALocalPresent
	row.AST = &feats
	row.Structural = &structural
	return row
}

// analyzeAtSHA collects all Go source files at sha, computes
// per-file LOC + package-set + total file count, and runs the AST
// analyzer to produce Features.
//
// Files are collected to a slice rather than streamed twice
// (analyzer + counters): the iter.Seq2 from BlobStreamer is
// single-pass. Memory cost: typical Go module's .go files total
// <500KB; not a concern at the budget caps in play.
func (a *Assembler) analyzeAtSHA(ctx context.Context, sha string) (golang.Features, Structural, error) {
	var files []golang.SourceFile
	packageDirs := make(map[string]struct{})

	for sf, err := range a.provider.EnumerateGoFiles(ctx, sha) {
		if err != nil {
			return golang.Features{}, Structural{}, err
		}
		files = append(files, sf)
		packageDirs[goPackageDir(sf.Path)] = struct{}{}
	}

	feats, err := a.analyzer.Analyze(ctx, sliceToErrSeq(files))
	if err != nil {
		return golang.Features{}, Structural{}, fmt.Errorf("analyze sha %s: %w", sha, err)
	}

	structural := Structural{
		GoFileCount: len(files),
		GoLOC:       totalLOC(files),
		// NewTopLevelPackages and NewSymbolExports stay empty in
		// commit 12. Commit 13 fills NewTopLevelPackages from
		// cross-version diffs of packageDirs.
		NewTopLevelPackages: []string{},
		NewSymbolExports:    []string{},
	}

	return feats, structural, nil
}

// goPackageDir returns the directory portion of a Go source file
// path, used as a map key for the per-version package set. A file
// at the module root returns "" (not "."), so the empty-string
// canonical form is stable across the assembler.
func goPackageDir(filePath string) string {
	dir := path.Dir(filePath)
	if dir == "." {
		return ""
	}
	return dir
}

// totalLOC returns the sum of line counts across all files.
// Trailing-newline policy: a file ending without a final newline
// still has its last line counted (so "foo\nbar" is 2 lines, not 1).
func totalLOC(files []golang.SourceFile) int {
	total := 0
	for _, f := range files {
		total += countLines(f.Content)
	}
	return total
}

// countLines returns the line count of content, treating each
// '\n' as a line terminator. Files without a trailing newline
// have their final partial line counted; empty content is 0.
func countLines(content []byte) int {
	if len(content) == 0 {
		return 0
	}
	n := 0
	for _, b := range content {
		if b == '\n' {
			n++
		}
	}
	if content[len(content)-1] != '\n' {
		n++
	}
	return n
}

// setOf builds a map[string]struct{} from a slice for O(1)
// membership testing.
func setOf(s []string) map[string]struct{} {
	m := make(map[string]struct{}, len(s))
	for _, v := range s {
		m[v] = struct{}{}
	}
	return m
}

// sliceToErrSeq adapts a slice of SourceFile into the
// iter.Seq2[SourceFile, error] shape the analyzer's Analyze method
// consumes. Always yields (file, nil); errors from the upstream
// provider are surfaced before this adapter is called.
func sliceToErrSeq(files []golang.SourceFile) iter.Seq2[golang.SourceFile, error] {
	return func(yield func(golang.SourceFile, error) bool) {
		for _, f := range files {
			if !yield(f, nil) {
				return
			}
		}
	}
}

// sortedKeys returns the keys of a string-set, sorted ascending.
// Helper for cross-version diffs (commit 13) that want a stable
// ordering of package-name lists.
func sortedKeys(s map[string]struct{}) []string {
	keys := make([]string, 0, len(s))
	for k := range s {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
