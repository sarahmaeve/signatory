package source

import (
	"context"
	"errors"
	"iter"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/signal/source/golang"
)

// fakeSourceProvider is a hand-built SourceProvider for matrix
// tests. Maps SHA → file list (or error) for EnumerateGoFiles, and
// SHA-pair → DiffStat for DiffStat. Missing entries return
// ErrSHAMissingFromClone via the iterator.
type fakeSourceProvider struct {
	filesBySHA  map[string][]golang.SourceFile
	missingSHAs map[string]bool
	diffsByPair map[[2]string]DiffStat
}

func (f *fakeSourceProvider) EnumerateGoFiles(_ context.Context, sha string) iter.Seq2[golang.SourceFile, error] {
	return func(yield func(golang.SourceFile, error) bool) {
		if f.missingSHAs[sha] {
			yield(golang.SourceFile{}, ErrSHAMissingFromClone)
			return
		}
		for _, sf := range f.filesBySHA[sha] {
			if !yield(sf, nil) {
				return
			}
		}
	}
}

func (f *fakeSourceProvider) DiffStat(_ context.Context, sha1, sha2 string) (DiffStat, error) {
	stat, ok := f.diffsByPair[[2]string{sha1, sha2}]
	if !ok {
		return DiffStat{}, nil
	}
	return stat, nil
}

// ============================================================
// Build — happy paths
// ============================================================

func TestAssemble_EmptyPinTable_EmptyMatrix(t *testing.T) {
	t.Parallel()
	a := NewAssembler(&fakeSourceProvider{}, golang.NewAnalyzer())
	mv, err := a.Build(context.Background(), PinTable{ModulePath: "example.com/empty"}, BudgetOpts{})
	require.NoError(t, err)
	assert.Equal(t, "example.com/empty", mv.ModulePath)
	assert.Equal(t, "go", mv.Ecosystem)
	assert.Equal(t, "go", mv.Language)
	assert.Empty(t, mv.Rows)
}

func TestAssemble_SinglePinnedVersion_RowHasASTAndStructural(t *testing.T) {
	t.Parallel()

	const sha = "abc1234567890123456789012345678901234567"
	provider := &fakeSourceProvider{
		filesBySHA: map[string][]golang.SourceFile{
			sha: {
				{Path: "main.go", Content: []byte("package main\n\nfunc init() {}\n")},
				{Path: "lib.go", Content: []byte("package main\n\nfunc Hello() string { return \"hi\" }\n")},
			},
		},
	}
	a := NewAssembler(provider, golang.NewAnalyzer())

	mv, err := a.Build(context.Background(), PinTable{
		ModulePath: "example.com/foo",
		Pins: []VersionPin{
			{Version: "v0.1.0", SHA: sha, Source: "proxy.golang.org"},
		},
	}, BudgetOpts{})
	require.NoError(t, err)
	require.Len(t, mv.Rows, 1)

	row := mv.Rows[0]
	assert.Equal(t, "v0.1.0", row.Version)
	assert.Equal(t, sha, row.TagSHA)
	assert.Equal(t, "proxy.golang.org", row.TagSHASource)
	assert.Equal(t, TagSHALocalPresent, row.TagSHALocalStatus)

	require.NotNil(t, row.AST)
	assert.Equal(t, 1, row.AST.InitCount, "main.go's init() should be counted")

	require.NotNil(t, row.Structural)
	assert.Equal(t, 2, row.Structural.GoFileCount)
	assert.Greater(t, row.Structural.GoLOC, 0)
	// Commit 12 leaves these empty; commit 13 fills NewTopLevelPackages.
	assert.Empty(t, row.Structural.NewTopLevelPackages)
	assert.Empty(t, row.Structural.NewSymbolExports)
}

func TestAssemble_MultiplePinnedVersions_RowsSortedDescBySemver(t *testing.T) {
	t.Parallel()

	provider := &fakeSourceProvider{
		filesBySHA: map[string][]golang.SourceFile{
			"sha010": {{Path: "main.go", Content: []byte("package main\n")}},
			"sha020": {{Path: "main.go", Content: []byte("package main\n\nfunc init() {}\n")}},
			"sha030": {{Path: "main.go", Content: []byte("package main\n\nfunc init() {}\nfunc init() {}\n")}},
		},
	}
	a := NewAssembler(provider, golang.NewAnalyzer())

	mv, err := a.Build(context.Background(), PinTable{
		ModulePath: "example.com/multi",
		Pins: []VersionPin{
			{Version: "v0.1.0", SHA: "sha010", Source: "proxy.golang.org"},
			{Version: "v0.3.0", SHA: "sha030", Source: "proxy.golang.org"},
			{Version: "v0.2.0", SHA: "sha020", Source: "proxy.golang.org"},
		},
	}, BudgetOpts{})
	require.NoError(t, err)
	require.Len(t, mv.Rows, 3)

	// Most-recent first.
	assert.Equal(t, "v0.3.0", mv.Rows[0].Version)
	assert.Equal(t, "v0.2.0", mv.Rows[1].Version)
	assert.Equal(t, "v0.1.0", mv.Rows[2].Version)

	// AST counts reflect each version's source.
	assert.Equal(t, 0, mv.Rows[2].AST.InitCount, "v0.1.0 has no init")
	assert.Equal(t, 1, mv.Rows[1].AST.InitCount, "v0.2.0 has one init")
	assert.Equal(t, 2, mv.Rows[0].AST.InitCount, "v0.3.0 has two init")
}

func TestAssemble_BudgetSelection_RecordedInValue(t *testing.T) {
	t.Parallel()

	provider := &fakeSourceProvider{
		filesBySHA: map[string][]golang.SourceFile{
			"sha010": {{Path: "main.go", Content: []byte("package main\n")}},
			"sha020": {{Path: "main.go", Content: []byte("package main\n")}},
			"sha030": {{Path: "main.go", Content: []byte("package main\n")}},
		},
	}
	a := NewAssembler(provider, golang.NewAnalyzer())

	mv, err := a.Build(context.Background(), PinTable{
		ModulePath: "example.com/budgeted",
		Pins: []VersionPin{
			{Version: "v0.1.0", SHA: "sha010", Source: "proxy.golang.org"},
			{Version: "v0.2.0", SHA: "sha020", Source: "proxy.golang.org"},
			{Version: "v0.3.0", SHA: "sha030", Source: "proxy.golang.org"},
		},
	}, BudgetOpts{LastN: 2, MajorLeaves: 0, HardCap: 20})
	require.NoError(t, err)

	// Budget selected 2 most-recent.
	require.Len(t, mv.Rows, 2)
	assert.Equal(t, "v0.3.0", mv.Rows[0].Version)
	assert.Equal(t, "v0.2.0", mv.Rows[1].Version)

	// Budget metadata recorded for the analyst's audit.
	assert.Equal(t, SelectionStrategyV1, mv.Budget.SelectionStrategy)
	assert.Equal(t, 2, mv.Budget.LastN)
	assert.Equal(t, []string{"v0.3.0", "v0.2.0"}, mv.Budget.SelectedVersions)
	assert.Equal(t, []string{"v0.1.0"}, mv.Budget.SkippedVersions)
}

// ============================================================
// Build — non-pinned classifications
// ============================================================

func TestAssemble_MissingFromClone_RowHasNullAnalysisAndStatus(t *testing.T) {
	t.Parallel()

	const sha = "abc1234567890123456789012345678901234567"
	provider := &fakeSourceProvider{
		missingSHAs: map[string]bool{sha: true},
	}
	a := NewAssembler(provider, golang.NewAnalyzer())

	mv, err := a.Build(context.Background(), PinTable{
		ModulePath: "example.com/missing",
		Pins: []VersionPin{
			{Version: "v0.1.0", SHA: sha, Source: "proxy.golang.org"},
		},
	}, BudgetOpts{})
	require.NoError(t, err)
	require.Len(t, mv.Rows, 1)

	row := mv.Rows[0]
	assert.Equal(t, "v0.1.0", row.Version)
	assert.Equal(t, sha, row.TagSHA)
	assert.Equal(t, TagSHALocalMissingFromClone, row.TagSHALocalStatus)
	assert.Nil(t, row.AST)
	assert.Nil(t, row.Structural)
}

func TestAssemble_MissingOriginVersion_RowHasStatusOnly(t *testing.T) {
	t.Parallel()

	a := NewAssembler(&fakeSourceProvider{}, golang.NewAnalyzer())

	mv, err := a.Build(context.Background(), PinTable{
		ModulePath:            "example.com/preGo20",
		MissingOriginVersions: []string{"v0.1.0"},
	}, BudgetOpts{})
	require.NoError(t, err)
	require.Len(t, mv.Rows, 1)

	row := mv.Rows[0]
	assert.Equal(t, "v0.1.0", row.Version)
	assert.Empty(t, row.TagSHA, "no SHA known for missing-origin")
	assert.Equal(t, TagSHALocalMissingOrigin, row.TagSHALocalStatus)
	assert.Nil(t, row.AST)
	assert.Nil(t, row.Structural)
}

func TestAssemble_FetchFailedVersion_RowHasStatusOnly(t *testing.T) {
	t.Parallel()

	a := NewAssembler(&fakeSourceProvider{}, golang.NewAnalyzer())

	mv, err := a.Build(context.Background(), PinTable{
		ModulePath:          "example.com/flaky",
		FetchFailedVersions: []string{"v0.1.0"},
	}, BudgetOpts{})
	require.NoError(t, err)
	require.Len(t, mv.Rows, 1)

	row := mv.Rows[0]
	assert.Equal(t, "v0.1.0", row.Version)
	assert.Equal(t, TagSHALocalFetchFailed, row.TagSHALocalStatus)
	assert.Nil(t, row.AST)
	assert.Nil(t, row.Structural)
}

func TestAssemble_MixedClassifications_AllRowsRepresented(t *testing.T) {
	t.Parallel()

	provider := &fakeSourceProvider{
		filesBySHA: map[string][]golang.SourceFile{
			"sha030": {{Path: "main.go", Content: []byte("package main\n")}},
		},
	}
	a := NewAssembler(provider, golang.NewAnalyzer())

	mv, err := a.Build(context.Background(), PinTable{
		ModulePath: "example.com/mixed",
		Pins: []VersionPin{
			{Version: "v0.3.0", SHA: "sha030", Source: "proxy.golang.org"},
		},
		MissingOriginVersions: []string{"v0.1.0"},
		FetchFailedVersions:   []string{"v0.2.0"},
	}, BudgetOpts{})
	require.NoError(t, err)
	require.Len(t, mv.Rows, 3)

	// Sorted descending: v0.3.0, v0.2.0, v0.1.0.
	assert.Equal(t, "v0.3.0", mv.Rows[0].Version)
	assert.Equal(t, TagSHALocalPresent, mv.Rows[0].TagSHALocalStatus)
	require.NotNil(t, mv.Rows[0].AST)

	assert.Equal(t, "v0.2.0", mv.Rows[1].Version)
	assert.Equal(t, TagSHALocalFetchFailed, mv.Rows[1].TagSHALocalStatus)
	assert.Nil(t, mv.Rows[1].AST)

	assert.Equal(t, "v0.1.0", mv.Rows[2].Version)
	assert.Equal(t, TagSHALocalMissingOrigin, mv.Rows[2].TagSHALocalStatus)
	assert.Nil(t, mv.Rows[2].AST)
}

// ============================================================
// Build — error cases
// ============================================================

func TestAssemble_NilProvider_Errors(t *testing.T) {
	t.Parallel()
	a := NewAssembler(nil, golang.NewAnalyzer())
	_, err := a.Build(context.Background(), PinTable{}, BudgetOpts{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil SourceProvider")
}

func TestAssemble_NilAnalyzer_Errors(t *testing.T) {
	t.Parallel()
	a := NewAssembler(&fakeSourceProvider{}, nil)
	_, err := a.Build(context.Background(), PinTable{}, BudgetOpts{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil Analyzer")
}

// ============================================================
// Helper coverage — countLines, goPackageDir
// ============================================================

func TestCountLines(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"\n", 1},
		{"a\n", 1},
		{"a\nb\n", 2},
		{"a\nb", 2}, // trailing partial line counted
		{"a", 1},    // no newline at all
		{"a\n\n", 2},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, countLines([]byte(tc.in)))
		})
	}
}

func TestGoPackageDir(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{"main.go", ""},
		{"lib/foo.go", "lib"},
		{"internal/util/util.go", "internal/util"},
		{"deeply/nested/path/file.go", "deeply/nested/path"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, goPackageDir(tc.in))
		})
	}
}

func TestSortedKeys(t *testing.T) {
	t.Parallel()
	got := sortedKeys(map[string]struct{}{
		"banana": {},
		"apple":  {},
		"cherry": {},
	})
	assert.Equal(t, []string{"apple", "banana", "cherry"}, got)
}

// ============================================================
// Cross-version pass (commit 13)
// ============================================================

func TestAssemble_TwoVersions_DiffFromPreviousPopulatedOnNewer(t *testing.T) {
	t.Parallel()

	const (
		shaOld = "1111111111111111111111111111111111111111"
		shaNew = "2222222222222222222222222222222222222222"
	)
	provider := &fakeSourceProvider{
		filesBySHA: map[string][]golang.SourceFile{
			shaOld: {{Path: "main.go", Content: []byte("package main\n")}},
			shaNew: {
				{Path: "main.go", Content: []byte("package main\n")},
				{Path: "lib.go", Content: []byte("package main\n\nfunc Hello() string { return \"hi\" }\n")},
			},
		},
		diffsByPair: map[[2]string]DiffStat{
			{shaOld, shaNew}: {
				FilesAdded: 1,
				LinesAdded: 3,
			},
		},
	}
	a := NewAssembler(provider, golang.NewAnalyzer())

	mv, err := a.Build(context.Background(), PinTable{
		ModulePath: "example.com/twover",
		Pins: []VersionPin{
			{Version: "v0.1.0", SHA: shaOld, Source: "proxy.golang.org"},
			{Version: "v0.2.0", SHA: shaNew, Source: "proxy.golang.org"},
		},
	}, BudgetOpts{})
	require.NoError(t, err)
	require.Len(t, mv.Rows, 2)

	// rows[0] = newest = v0.2.0; rows[1] = oldest = v0.1.0.
	newer := mv.Rows[0]
	older := mv.Rows[1]

	require.NotNil(t, newer.DiffFromPrevious, "newest row should have diff vs older")
	assert.Equal(t, 1, newer.DiffFromPrevious.FilesAdded)
	assert.Equal(t, 3, newer.DiffFromPrevious.LinesAdded)

	assert.Nil(t, older.DiffFromPrevious, "oldest row has no previous; nil")
}

func TestAssemble_NewTopLevelPackages_PopulatedOnPackageGrowth(t *testing.T) {
	t.Parallel()

	const (
		shaOld = "aaaa111111111111111111111111111111111111"
		shaNew = "bbbb222222222222222222222222222222222222"
	)
	provider := &fakeSourceProvider{
		filesBySHA: map[string][]golang.SourceFile{
			shaOld: {
				{Path: "main.go", Content: []byte("package main\n")},
				{Path: "lib/foo.go", Content: []byte("package lib\n")},
			},
			shaNew: {
				{Path: "main.go", Content: []byte("package main\n")},
				{Path: "lib/foo.go", Content: []byte("package lib\n")},
				{Path: "internal/util/util.go", Content: []byte("package util\n")},
				{Path: "newpkg/x.go", Content: []byte("package newpkg\n")},
			},
		},
	}
	a := NewAssembler(provider, golang.NewAnalyzer())

	mv, err := a.Build(context.Background(), PinTable{
		ModulePath: "example.com/grow",
		Pins: []VersionPin{
			{Version: "v0.1.0", SHA: shaOld, Source: "proxy.golang.org"},
			{Version: "v0.2.0", SHA: shaNew, Source: "proxy.golang.org"},
		},
	}, BudgetOpts{})
	require.NoError(t, err)
	require.Len(t, mv.Rows, 2)

	newer := mv.Rows[0]
	require.NotNil(t, newer.Structural)
	// New packages: internal/util and newpkg.
	assert.Equal(t,
		[]string{"internal/util", "newpkg"},
		newer.Structural.NewTopLevelPackages)

	// Older row's NewTopLevelPackages stays empty (no previous to diff against).
	older := mv.Rows[1]
	require.NotNil(t, older.Structural)
	assert.Empty(t, older.Structural.NewTopLevelPackages)
}

func TestAssemble_NewTopLevelPackages_EmptyWhenNoPackageChange(t *testing.T) {
	t.Parallel()

	const (
		shaOld = "cccc111111111111111111111111111111111111"
		shaNew = "dddd222222222222222222222222222222222222"
	)
	provider := &fakeSourceProvider{
		filesBySHA: map[string][]golang.SourceFile{
			shaOld: {
				{Path: "main.go", Content: []byte("package main\nvar x = 1\n")},
				{Path: "lib/foo.go", Content: []byte("package lib\n")},
			},
			shaNew: {
				// Same package layout, just modified content in main.go.
				{Path: "main.go", Content: []byte("package main\nvar x = 2\nvar y = 3\n")},
				{Path: "lib/foo.go", Content: []byte("package lib\n")},
			},
		},
	}
	a := NewAssembler(provider, golang.NewAnalyzer())

	mv, err := a.Build(context.Background(), PinTable{
		ModulePath: "example.com/stable",
		Pins: []VersionPin{
			{Version: "v0.1.0", SHA: shaOld, Source: "proxy.golang.org"},
			{Version: "v0.2.0", SHA: shaNew, Source: "proxy.golang.org"},
		},
	}, BudgetOpts{})
	require.NoError(t, err)
	require.Len(t, mv.Rows, 2)

	newer := mv.Rows[0]
	require.NotNil(t, newer.Structural)
	assert.Empty(t, newer.Structural.NewTopLevelPackages,
		"identical package layout should produce empty NewTopLevelPackages")
}

func TestAssemble_ThreeVersionsInChain_DiffsAreAdjacent(t *testing.T) {
	t.Parallel()

	const (
		sha1 = "1111000000000000000000000000000000000000"
		sha2 = "2222000000000000000000000000000000000000"
		sha3 = "3333000000000000000000000000000000000000"
	)
	provider := &fakeSourceProvider{
		filesBySHA: map[string][]golang.SourceFile{
			sha1: {{Path: "main.go", Content: []byte("package main\n")}},
			sha2: {{Path: "main.go", Content: []byte("package main\nvar x = 1\n")}},
			sha3: {{Path: "main.go", Content: []byte("package main\nvar x = 1\nvar y = 2\n")}},
		},
		diffsByPair: map[[2]string]DiffStat{
			{sha1, sha2}: {LinesAdded: 1},
			{sha2, sha3}: {LinesAdded: 1},
			// {sha1, sha3} is intentionally absent — assembler must NOT
			// diff non-adjacent pairs.
		},
	}
	a := NewAssembler(provider, golang.NewAnalyzer())

	mv, err := a.Build(context.Background(), PinTable{
		ModulePath: "example.com/chain",
		Pins: []VersionPin{
			{Version: "v0.1.0", SHA: sha1, Source: "proxy.golang.org"},
			{Version: "v0.2.0", SHA: sha2, Source: "proxy.golang.org"},
			{Version: "v0.3.0", SHA: sha3, Source: "proxy.golang.org"},
		},
	}, BudgetOpts{})
	require.NoError(t, err)
	require.Len(t, mv.Rows, 3)

	// rows[0] = v0.3.0 (newest), rows[1] = v0.2.0, rows[2] = v0.1.0.
	require.NotNil(t, mv.Rows[0].DiffFromPrevious)
	assert.Equal(t, 1, mv.Rows[0].DiffFromPrevious.LinesAdded, "v0.3.0 vs v0.2.0")

	require.NotNil(t, mv.Rows[1].DiffFromPrevious)
	assert.Equal(t, 1, mv.Rows[1].DiffFromPrevious.LinesAdded, "v0.2.0 vs v0.1.0")

	assert.Nil(t, mv.Rows[2].DiffFromPrevious, "oldest has no previous")
}

func TestAssemble_DiffSkippedWhenAdjacentRowMissingFromClone(t *testing.T) {
	t.Parallel()

	const (
		sha1 = "1111000000000000000000000000000000000000"
		sha2 = "2222000000000000000000000000000000000000"
	)
	provider := &fakeSourceProvider{
		filesBySHA: map[string][]golang.SourceFile{
			// sha1 missing intentionally; sha2 present.
			sha2: {{Path: "main.go", Content: []byte("package main\n")}},
		},
		missingSHAs: map[string]bool{sha1: true},
	}
	a := NewAssembler(provider, golang.NewAnalyzer())

	mv, err := a.Build(context.Background(), PinTable{
		ModulePath: "example.com/gap",
		Pins: []VersionPin{
			{Version: "v0.1.0", SHA: sha1, Source: "proxy.golang.org"},
			{Version: "v0.2.0", SHA: sha2, Source: "proxy.golang.org"},
		},
	}, BudgetOpts{})
	require.NoError(t, err)
	require.Len(t, mv.Rows, 2)

	// v0.2.0 (newer, sha2) is present; v0.1.0 (older, sha1) is
	// missing-from-clone. Diff should be skipped because one side
	// has no analyzable SHA — newer's DiffFromPrevious stays nil.
	newer := mv.Rows[0]
	older := mv.Rows[1]
	assert.Equal(t, TagSHALocalPresent, newer.TagSHALocalStatus)
	assert.Equal(t, TagSHALocalMissingFromClone, older.TagSHALocalStatus)
	assert.Nil(t, newer.DiffFromPrevious, "diff skipped when older is missing-from-clone")
}

func TestAssemble_DiffSkippedWhenAdjacentRowIsMissingOrigin(t *testing.T) {
	t.Parallel()

	const sha2 = "2222000000000000000000000000000000000000"
	provider := &fakeSourceProvider{
		filesBySHA: map[string][]golang.SourceFile{
			sha2: {{Path: "main.go", Content: []byte("package main\n")}},
		},
	}
	a := NewAssembler(provider, golang.NewAnalyzer())

	mv, err := a.Build(context.Background(), PinTable{
		ModulePath: "example.com/missingorigin",
		Pins: []VersionPin{
			{Version: "v0.2.0", SHA: sha2, Source: "proxy.golang.org"},
		},
		MissingOriginVersions: []string{"v0.1.0"},
	}, BudgetOpts{})
	require.NoError(t, err)
	require.Len(t, mv.Rows, 2)

	newer := mv.Rows[0]
	older := mv.Rows[1]
	assert.Equal(t, TagSHALocalPresent, newer.TagSHALocalStatus)
	assert.Equal(t, TagSHALocalMissingOrigin, older.TagSHALocalStatus)
	assert.Nil(t, newer.DiffFromPrevious)
}

// ============================================================
// Existing passes-through-cross-version edge cases
// ============================================================

// Sanity: the upstream-error path is exercised via the analyzer's
// own tests (TestAnalyze_UpstreamIteratorError_PropagatesToCaller).
// Here we just confirm the assembler surfaces the error.
func TestAssemble_ProviderYieldsNonMissingError_PartialRow(t *testing.T) {
	t.Parallel()

	const sha = "deadbeef00000000000000000000000000000000"
	wantErr := errors.New("some other provider error")
	provider := &errorYieldingProvider{sha: sha, err: wantErr}

	a := NewAssembler(provider, golang.NewAnalyzer())
	mv, err := a.Build(context.Background(), PinTable{
		ModulePath: "example.com/x",
		Pins: []VersionPin{
			{Version: "v0.1.0", SHA: sha, Source: "proxy.golang.org"},
		},
	}, BudgetOpts{})
	require.NoError(t, err)
	require.Len(t, mv.Rows, 1)

	// Non-missing-from-clone errors also produce a partial row;
	// commit 12 conservatively maps them to missing_from_clone
	// rather than crashing. (A future commit could distinguish.)
	assert.Equal(t, TagSHALocalMissingFromClone, mv.Rows[0].TagSHALocalStatus)
	assert.Nil(t, mv.Rows[0].AST)
}

type errorYieldingProvider struct {
	sha string
	err error
}

func (p *errorYieldingProvider) EnumerateGoFiles(_ context.Context, sha string) iter.Seq2[golang.SourceFile, error] {
	return func(yield func(golang.SourceFile, error) bool) {
		if sha == p.sha {
			yield(golang.SourceFile{}, p.err)
		}
	}
}

func (p *errorYieldingProvider) DiffStat(_ context.Context, _, _ string) (DiffStat, error) {
	return DiffStat{}, nil
}
