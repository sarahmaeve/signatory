package source

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"maps"
	"path"
	"slices"
	"time"

	"github.com/sarahmaeve/signatory/internal/signal/source/astfeature"
	"github.com/sarahmaeve/signatory/internal/signal/source/golang"
)

// MatrixValue is the JSON-marshaled value of the
// source_evolution_matrix signal. One per Go-ecosystem entity per
// analysis; rows are sorted semver-descending (most-recent first).
//
// Compound shape (one row per selected version) is by design: a
// single record beats per-version-pair records because it gives
// the analyst all rows in one query, with no cross-row stitching.
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
	Version           string             `json:"version"`
	TagSHA            string             `json:"tag_sha"`
	TagSHASource      string             `json:"tag_sha_source"`
	TagSHALocalStatus string             `json:"tag_sha_local_status"`
	AST               *astfeature.Counts `json:"ast"`
	Structural        *Structural        `json:"structural"`
	DiffFromPrevious  *DiffStat          `json:"diff_from_previous"`
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
	// FileCount and LOC count the source files the per-language
	// filter streamed (Go .go or Python .py), not Go specifically —
	// the names were Go-only until pypi support; the json keys are
	// language-neutral so a pypi matrix doesn't read as "go_loc".
	FileCount           int      `json:"file_count"`
	LOC                 int      `json:"loc"`
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
	// forgery-resistance HIGH signal.
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

	// TagSHALocalAnalyzeFailed: SHA was present (or expected to be —
	// the failure prevents us from being certain) but the analyzer
	// or BlobStreamer returned an error other than
	// ErrSHAMissingFromClone. Causes include context cancellation
	// (SIGINT mid-analysis), ErrBlobSizeExceedsCap (a cat-file
	// header reporting a size over the per-blob cap — itself a
	// supply-chain-relevant signal), ErrBlobStreamerClosed
	// (subprocess shutdown mid-iteration), and generic git-pipe
	// IO errors. Distinct from TagSHALocalMissingFromClone so an
	// analyst reading the matrix can tell "we never had the SHA"
	// from "we had it, the analyzer tripped on it" — those have
	// different remediation paths and different forgery-resistance
	// implications. (Surfaced by the 2026-05-02 adversarial-review
	// punch list, Tier 1 #1.4.)
	TagSHALocalAnalyzeFailed = "analyze_failed"
)

// SourceProvider is the narrow interface the Assembler uses to
// read source content and compute diffs. BlobStreamer satisfies
// it implicitly; tests inject hand-built fakes.
type SourceProvider interface {
	EnumerateSourceFiles(ctx context.Context, sha string) iter.Seq2[astfeature.SourceFile, error]
	DiffStat(ctx context.Context, sha1, sha2 string) (DiffStat, error)
}

// LanguageAnalyzer extracts AST construct counts from a stream of
// source files and names the language it analyzes. Factored out so a
// second language analyzer can be substituted without touching the
// Assembler. Language() feeds MatrixValue.Language/Ecosystem so the
// emitted matrix self-describes instead of hardwiring "go".
// *golang.Analyzer and *python.Analyzer satisfy it.
type LanguageAnalyzer interface {
	Analyze(ctx context.Context, files iter.Seq2[astfeature.SourceFile, error]) (astfeature.Counts, error)
	Language() string
}

// ecosystemForLanguage maps an analyzer's language to the registry
// ecosystem whose version_pin_table anchors the matrix. Narrow on
// purpose: source-evolution supports exactly these today, and a
// missing case should surface (empty string in the signal) rather
// than be silently mislabeled.
func ecosystemForLanguage(language string) string {
	switch language {
	case "go":
		return "go"
	case "python":
		return "pypi"
	default:
		return ""
	}
}

// Compile-time assertions that BlobStreamer satisfies SourceProvider
// and *golang.Analyzer satisfies LanguageAnalyzer. Live in matrix.go
// (the consumer's home) so a future implementation change that breaks
// either contract fails build at the consumer rather than silently
// slipping by.
var (
	_ SourceProvider   = (*BlobStreamer)(nil)
	_ LanguageAnalyzer = (*golang.Analyzer)(nil)
)

// Assembler builds a MatrixValue from a PinTable + budget options.
// Composes a SourceProvider (for reading versions) and a
// LanguageAnalyzer (for AST feature extraction).
//
// Stateless across Build calls; safe to reuse.
type Assembler struct {
	provider SourceProvider
	analyzer LanguageAnalyzer
}

// NewAssembler returns an Assembler wired to the given provider
// and analyzer. Both are required — nil provider or nil analyzer
// causes Build to return an error rather than panicking later.
func NewAssembler(provider SourceProvider, analyzer LanguageAnalyzer) *Assembler {
	return &Assembler{provider: provider, analyzer: analyzer}
}

// Build assembles a MatrixValue from a pin table, applying budget
// selection to the union of pinned + missing-origin + fetch-failed
// versions. Each row is constructed per version with
// tag_sha_local_status set according to the version's classification
// in the pin table and whether the local clone has the SHA.
//
// Two passes:
//
//  1. Per-version: build each row's AST + Structural by analyzing
//     the version's source (pinned + present-in-clone case) or
//     recording the gap status (missing/fetch-failed cases).
//
//  2. Cross-version: for each adjacent pair of rows in the
//     semver-descending sequence, diff the older against the newer
//     and populate the newer row's DiffFromPrevious +
//     Structural.NewTopLevelPackages. Pairs where either side
//     lacks a present analysis (missing-from-clone, missing-origin,
//     fetch-failed) skip the diff — those rows have nil
//     DiffFromPrevious.
//
// Returns a non-nil MatrixValue with empty Rows for empty input.
//
// Errors only when a structural problem prevents any progress
// (nil provider/analyzer). Per-version analysis failures land in
// the row's tag_sha_local_status field; per-pair diff failures
// leave DiffFromPrevious nil but don't abort.
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

	// Chronological axis for budget recency + row order comes from
	// the pin table's publish times (ecosystem-neutral). Only pinned
	// versions carry a time; missing-origin / fetch-failed strings
	// have none and fall back to semver via chronoCmpFunc.
	pub := make(map[string]time.Time, len(pinTable.Pins))
	for _, p := range pinTable.Pins {
		pub[p.Version] = p.PublishedAt
	}
	opts.publishedAt = pub

	sel := Select(allVersions, opts)

	pinByVersion := make(map[string]VersionPin, len(pinTable.Pins))
	for _, p := range pinTable.Pins {
		pinByVersion[p.Version] = p
	}
	missingOriginSet := setOf(pinTable.MissingOriginVersions)
	fetchFailedSet := setOf(pinTable.FetchFailedVersions)

	// Pass 1: per-version row construction. aux carries per-row
	// state that doesn't ship in the JSON output (the package set
	// for cross-version diff in pass 2).
	rows := make([]MatrixRow, 0, len(sel.SelectedVersions))
	auxByIdx := make([]rowAux, 0, len(sel.SelectedVersions))
	for _, version := range sel.SelectedVersions {
		row, aux := a.buildRow(ctx, version, pinByVersion, missingOriginSet, fetchFailedSet)
		rows = append(rows, row)
		auxByIdx = append(auxByIdx, aux)
	}

	// Pass 2: cross-version diff + new-packages population.
	a.applyCrossVersion(ctx, rows, auxByIdx)

	language := a.analyzer.Language()
	return MatrixValue{
		ModulePath: pinTable.ModulePath,
		Ecosystem:  ecosystemForLanguage(language),
		Language:   language,
		Budget:     sel,
		Rows:       rows,
	}, nil
}

// rowAux holds the per-row state needed by the cross-version pass
// but not surfaced in the JSON output. Currently just SHA (for
// DiffStat) and the package directory set (for NewTopLevelPackages
// computation). Stays parallel to the rows slice.
type rowAux struct {
	sha      string
	packages map[string]struct{}
}

// applyCrossVersion walks the rows in semver-descending order and,
// for each adjacent pair (newer at i, older at i+1), populates the
// newer row's DiffFromPrevious and NewTopLevelPackages. The oldest
// row (last in the slice) has nil DiffFromPrevious by definition.
//
// Skip cases (DiffFromPrevious stays nil):
//   - Either row's TagSHALocalStatus is not "present" — at least
//     one side of the diff has no SHA we can analyze.
//   - DiffStat returns an error — record a debug-level mark and
//     move on; per-pair failures shouldn't abort the matrix.
//
// NewTopLevelPackages is the set of package directories present
// in the newer row but absent in the older. Sorted ascending for
// deterministic JSON output.
func (a *Assembler) applyCrossVersion(ctx context.Context, rows []MatrixRow, aux []rowAux) {
	for i := 0; i < len(rows)-1; i++ {
		newer := &rows[i]
		older := &rows[i+1]
		newerAux := aux[i]
		olderAux := aux[i+1]

		// Both sides must be analyzable to diff.
		if newer.TagSHALocalStatus != TagSHALocalPresent ||
			older.TagSHALocalStatus != TagSHALocalPresent {
			continue
		}
		if newerAux.sha == "" || olderAux.sha == "" {
			continue
		}

		// DiffStat: from older SHA to newer SHA.
		stat, err := a.provider.DiffStat(ctx, olderAux.sha, newerAux.sha)
		if err == nil {
			newer.DiffFromPrevious = &stat
		}
		// NewTopLevelPackages: present in newer, absent in older.
		if newer.Structural != nil {
			var added []string
			for pkg := range newerAux.packages {
				if _, existed := olderAux.packages[pkg]; !existed {
					added = append(added, pkg)
				}
			}
			slices.Sort(added)
			if added == nil {
				added = []string{}
			}
			newer.Structural.NewTopLevelPackages = added
		}
	}
}

// buildRow dispatches a version to the right row-construction
// path based on its pin-table classification: pinned (try analysis),
// missing-origin (record gap, no analysis), or fetch-failed (record
// gap). Returns the row plus its rowAux for the cross-version pass.
//
// A version that's in none of the classifications is an internal
// logic error — it shouldn't reach buildRow because Selection's
// input is exactly the union of those sets.
func (a *Assembler) buildRow(
	ctx context.Context,
	version string,
	pinByVersion map[string]VersionPin,
	missingOriginSet, fetchFailedSet map[string]struct{},
) (MatrixRow, rowAux) {
	if pin, ok := pinByVersion[version]; ok {
		return a.buildRowFromPin(ctx, version, pin)
	}
	if _, ok := missingOriginSet[version]; ok {
		return MatrixRow{
			Version:           version,
			TagSHALocalStatus: TagSHALocalMissingOrigin,
		}, rowAux{}
	}
	if _, ok := fetchFailedSet[version]; ok {
		return MatrixRow{
			Version:           version,
			TagSHALocalStatus: TagSHALocalFetchFailed,
		}, rowAux{}
	}
	// Defensive: should not reach here.
	return MatrixRow{
		Version:           version,
		TagSHALocalStatus: "unknown",
	}, rowAux{}
}

// buildRowFromPin runs the AST analyzer + structural counter at
// the pin's SHA. Two distinct error classes produce partial rows:
//
//   - ErrSHAMissingFromClone (proxy SHA not in local object DB) →
//     TagSHALocalMissingFromClone. The expected case when --refresh
//     didn't fetch a proxy-pinned SHA; itself diagnostic.
//   - Any other analyzer/provider error (context cancellation
//     during analysis, ErrBlobSizeExceedsCap on a tampered blob
//     header, ErrBlobStreamerClosed mid-iteration, generic git
//     pipe IO errors) → TagSHALocalAnalyzeFailed. The SHA was
//     reachable but analysis tripped on it; conflating this with
//     missing-from-clone would blind the analyst to the real
//     event class.
//
// Both cases preserve the row with nil AST/Structural so the
// matrix doesn't abort on one version's failure. The 2026-05-02
// adversarial review (Tier 1 #1.4) split the bucket into two
// distinct statuses; the doc comment for TagSHALocalAnalyzeFailed
// in the const block above lists the concrete error classes.
//
// Returns rowAux populated with sha + packages set when analysis
// succeeded; empty rowAux for partial rows (the cross-version
// pass skips pairs without aux).
func (a *Assembler) buildRowFromPin(ctx context.Context, version string, pin VersionPin) (MatrixRow, rowAux) {
	row := MatrixRow{
		Version:      version,
		TagSHA:       pin.SHA,
		TagSHASource: pin.Source,
	}

	feats, structural, packages, err := a.analyzeAtSHA(ctx, pin.SHA)
	if err != nil {
		if errors.Is(err, ErrSHAMissingFromClone) {
			row.TagSHALocalStatus = TagSHALocalMissingFromClone
		} else {
			row.TagSHALocalStatus = TagSHALocalAnalyzeFailed
		}
		return row, rowAux{}
	}

	row.TagSHALocalStatus = TagSHALocalPresent
	row.AST = &feats
	row.Structural = &structural
	return row, rowAux{sha: pin.SHA, packages: packages}
}

// analyzeAtSHA collects all Go source files at sha, computes
// per-file LOC + package-set + total file count, and runs the AST
// analyzer to produce Features. Returns Features, Structural, the
// per-version package directory set (consumed by the cross-version
// pass for NewTopLevelPackages), and any error.
//
// Files are collected to a slice rather than streamed twice
// (analyzer + counters): the iter.Seq2 from BlobStreamer is
// single-pass. Memory cost: typical Go module's .go files total
// <500KB; not a concern at the budget caps in play.
func (a *Assembler) analyzeAtSHA(ctx context.Context, sha string) (astfeature.Counts, Structural, map[string]struct{}, error) {
	var files []astfeature.SourceFile
	packageDirs := make(map[string]struct{})

	for sf, err := range a.provider.EnumerateSourceFiles(ctx, sha) {
		if err != nil {
			return astfeature.Counts{}, Structural{}, nil, err
		}
		files = append(files, sf)
		packageDirs[goPackageDir(sf.Path)] = struct{}{}
	}

	feats, err := a.analyzer.Analyze(ctx, sliceToErrSeq(files))
	if err != nil {
		return astfeature.Counts{}, Structural{}, nil, fmt.Errorf("analyze sha %s: %w", sha, err)
	}

	structural := Structural{
		FileCount: len(files),
		LOC:       totalLOC(files),
		// NewTopLevelPackages is filled by applyCrossVersion (pass
		// 2) when the previous-version's package set is known.
		// NewSymbolExports remains future work.
		NewTopLevelPackages: []string{},
		NewSymbolExports:    []string{},
	}

	return feats, structural, packageDirs, nil
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
func totalLOC(files []astfeature.SourceFile) int {
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
func sliceToErrSeq(files []astfeature.SourceFile) iter.Seq2[astfeature.SourceFile, error] {
	return func(yield func(astfeature.SourceFile, error) bool) {
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
	return slices.Sorted(maps.Keys(s))
}
