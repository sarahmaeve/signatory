// Package python is the Python source-evolution analyzer. It mirrors
// the golang package's role: turn a stream of source files into an
// astfeature.Counts per version.
//
// Status: placeholder. Real AST construct counting (the Python
// equivalent of the golang analyzer's init/network/exec/xor/base64
// extraction) is roadmap item #4 — the only genuine cost center in
// Python source-evolution support. Until it lands, Analyze returns
// empty Counts so the matrix/anomaly layer stays AST-blind for
// Python while structural (LOC, file count) and diff signal still
// flow. It deliberately preserves the golang.Analyzer error/ctx
// contract so the Assembler treats both analyzers identically: a
// mid-stream provider error aborts the row rather than producing a
// misleading all-zero matrix entry.
package python

import (
	"context"
	"iter"

	"github.com/sarahmaeve/signatory/internal/signal/source/astfeature"
)

// Analyzer is the Python source analyzer. Stateless across calls;
// safe to reuse. The constructor exists for symmetry with
// golang.NewAnalyzer and so item #4 can add options (parser choice,
// pattern catalogs) without breaking the collector's call site.
type Analyzer struct{}

// NewAnalyzer returns a ready Python Analyzer.
func NewAnalyzer() *Analyzer { return &Analyzer{} }

// Language names the language this analyzer handles. Feeds
// MatrixValue.Language/Ecosystem ("python" → pypi ecosystem).
func (a *Analyzer) Language() string { return "python" }

// Analyze drains the file stream and returns empty Counts.
//
// Errors yielded by the upstream iterator (e.g. the BlobStreamer
// reporting a blob fetch failure mid-stream) are returned to the
// caller — identical to golang.Analyzer, so a partial stream never
// becomes a deceptively clean all-zero row. Context cancellation is
// honored between files. The stream is drained fully so the
// provider's single-pass enumeration completes.
func (a *Analyzer) Analyze(ctx context.Context, files iter.Seq2[astfeature.SourceFile, error]) (astfeature.Counts, error) {
	for _, err := range files {
		if err != nil {
			return astfeature.Counts{}, err
		}
		if err := ctx.Err(); err != nil {
			return astfeature.Counts{}, err
		}
	}
	return astfeature.Counts{}, nil
}
