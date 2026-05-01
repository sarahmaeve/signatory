package golang

import (
	"errors"
	"iter"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// iterErrFree adapts a value-only iterator into the
// iter.Seq2[SourceFile, error] shape the analyzer accepts. Test
// convenience; production callers (BlobStreamer) yield real errors
// when blob fetches fail mid-stream.
func iterErrFree(seq iter.Seq[SourceFile]) iter.Seq2[SourceFile, error] {
	return func(yield func(SourceFile, error) bool) {
		for sf := range seq {
			if !yield(sf, nil) {
				return
			}
		}
	}
}

func TestAnalyze_EmptySource_ZerosAllFeatures(t *testing.T) {
	t.Parallel()

	a := NewAnalyzer()
	files := iterErrFree(slices.Values([]SourceFile{}))
	feats, err := a.Analyze(t.Context(), files)
	require.NoError(t, err)
	assert.Equal(t, 0, feats.InitCount)
}

func TestAnalyze_SingleInit_CountsOne(t *testing.T) {
	t.Parallel()

	a := NewAnalyzer()
	files := iterErrFree(slices.Values([]SourceFile{
		{Path: "main.go", Content: []byte("package main\n\nfunc init() { _ = 1 }\n")},
	}))
	feats, err := a.Analyze(t.Context(), files)
	require.NoError(t, err)
	assert.Equal(t, 1, feats.InitCount)
}

func TestAnalyze_MultipleInitFunctions_CountedAcrossFiles(t *testing.T) {
	t.Parallel()

	a := NewAnalyzer()
	files := iterErrFree(slices.Values([]SourceFile{
		{Path: "a.go", Content: []byte("package main\n\nfunc init() {}\n")},
		{Path: "b.go", Content: []byte("package main\n\nfunc init() {}\n")},
		{Path: "subpkg/c.go", Content: []byte("package subpkg\n\nfunc init() {}\n")},
	}))
	feats, err := a.Analyze(t.Context(), files)
	require.NoError(t, err)
	assert.Equal(t, 3, feats.InitCount)
}

func TestAnalyze_TwoInitsInOneFile_CountedSeparately(t *testing.T) {
	t.Parallel()

	// Go permits multiple init() per file. Each runs at import.
	src := `package main

func init() {}

func init() {}
`
	a := NewAnalyzer()
	files := iterErrFree(slices.Values([]SourceFile{
		{Path: "main.go", Content: []byte(src)},
	}))
	feats, err := a.Analyze(t.Context(), files)
	require.NoError(t, err)
	assert.Equal(t, 2, feats.InitCount)
}

func TestAnalyze_MethodNamedInit_NotCounted(t *testing.T) {
	t.Parallel()

	// A method on a type named init() is NOT a package init function
	// and does not run on import. Only func init() at file scope with
	// no receiver counts.
	src := `package main

type Foo struct{}

func (f *Foo) init() {}

func init() {}
`
	a := NewAnalyzer()
	files := iterErrFree(slices.Values([]SourceFile{
		{Path: "main.go", Content: []byte(src)},
	}))
	feats, err := a.Analyze(t.Context(), files)
	require.NoError(t, err)
	assert.Equal(t, 1, feats.InitCount)
}

func TestAnalyze_FunctionNamedInitInComment_NotCounted(t *testing.T) {
	t.Parallel()

	// Defends against a regex-matching implementation: comments and
	// strings containing the text "func init()" must not be counted.
	src := `package main

// init() handles startup. The real init is below.
const initSource = "func init() { do_evil() }"

func realStart() {}
`
	a := NewAnalyzer()
	files := iterErrFree(slices.Values([]SourceFile{
		{Path: "main.go", Content: []byte(src)},
	}))
	feats, err := a.Analyze(t.Context(), files)
	require.NoError(t, err)
	assert.Equal(t, 0, feats.InitCount)
}

func TestAnalyze_MalformedFile_SkipsAndContinuesOtherFiles(t *testing.T) {
	t.Parallel()

	// A malformed .go file in the tree should not abort analysis;
	// the matrix's purpose is to surface features that DO exist in
	// valid files. The analyst sees the unparseable file separately
	// (as a structural anomaly recorded by the matrix assembler in a
	// later commit).
	a := NewAnalyzer()
	files := iterErrFree(slices.Values([]SourceFile{
		{Path: "broken.go", Content: []byte("not actually go source ;;;")},
		{Path: "good.go", Content: []byte("package main\n\nfunc init() {}\n")},
	}))
	feats, err := a.Analyze(t.Context(), files)
	require.NoError(t, err)
	assert.Equal(t, 1, feats.InitCount)
}

func TestAnalyze_UpstreamIteratorError_PropagatesToCaller(t *testing.T) {
	t.Parallel()

	// When the source provider (BlobStreamer in production) yields an
	// error mid-stream, Analyze surfaces it to the caller. Previously
	// counted features are abandoned because partial results would
	// mislead the matrix.
	wantErr := errors.New("upstream blob fetch failed")
	a := NewAnalyzer()
	files := func(yield func(SourceFile, error) bool) {
		if !yield(SourceFile{Path: "ok.go", Content: []byte("package main\n")}, nil) {
			return
		}
		yield(SourceFile{}, wantErr)
	}
	_, err := a.Analyze(t.Context(), files)
	require.Error(t, err)
	assert.ErrorIs(t, err, wantErr)
}
