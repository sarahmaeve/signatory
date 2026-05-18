package python

import (
	"context"
	"errors"
	"iter"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/signal/source/astfeature"
)

// seq builds an iter.Seq2 from explicit (file, err) pairs so tests
// can drive the analyzer's stream-error and drain behavior exactly.
func seq(pairs ...struct {
	f   astfeature.SourceFile
	err error
}) iter.Seq2[astfeature.SourceFile, error] {
	return func(yield func(astfeature.SourceFile, error) bool) {
		for _, p := range pairs {
			if !yield(p.f, p.err) {
				return
			}
		}
	}
}

type fe = struct {
	f   astfeature.SourceFile
	err error
}

func TestAnalyzer_Analyze_ReturnsEmptyCounts(t *testing.T) {
	t.Parallel()
	// Placeholder until the real parser (roadmap item #4): every
	// field stays zero regardless of file content. This is the
	// contract the matrix/anomaly layer relies on — AST-blind for
	// Python, structural+diff still flow.
	a := NewAnalyzer()
	counts, err := a.Analyze(t.Context(), seq(
		fe{f: astfeature.SourceFile{Path: "pkg/__init__.py", Content: []byte("import os\nos.system('x')\n")}},
		fe{f: astfeature.SourceFile{Path: "pkg/core.py", Content: []byte("eval('1')\n")}},
	))
	require.NoError(t, err)
	assert.Equal(t, astfeature.Counts{}, counts,
		"placeholder analyzer must not invent feature counts before the real parser exists")
}

func TestAnalyzer_Analyze_PropagatesUpstreamStreamError(t *testing.T) {
	t.Parallel()
	// Same contract as golang.Analyzer: a mid-stream provider error
	// (e.g. BlobStreamer blob-fetch failure) aborts with that error
	// rather than silently yielding empty counts, so the assembler
	// does not record a misleading all-zero row.
	wantErr := errors.New("blob fetch boom")
	a := NewAnalyzer()
	_, err := a.Analyze(t.Context(), seq(
		fe{f: astfeature.SourceFile{Path: "ok.py", Content: []byte("x = 1\n")}},
		fe{err: wantErr},
	))
	assert.ErrorIs(t, err, wantErr)
}

func TestAnalyzer_Analyze_HonorsContextCancellation(t *testing.T) {
	t.Parallel()
	a := NewAnalyzer()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := a.Analyze(ctx, seq(
		fe{f: astfeature.SourceFile{Path: "a.py", Content: []byte("x = 1\n")}},
	))
	assert.ErrorIs(t, err, ctx.Err())
}
