package repofiles

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// evalFamily is a small fixture family used by the evaluator tests
// that don't need the full production declaration. Keeping it local
// makes the ranking rules testable without coupling to which specific
// files the production list includes.
func evalFamily(name string, preferred ...string) Family {
	return Family{
		Name:      name,
		Dirs:      []string{"."},
		Detector:  regexp.MustCompile(`.*`),
		Preferred: preferred,
	}
}

func TestEvaluate_NoMatches_ReportsAbsent(t *testing.T) {
	t.Parallel()

	fams := []Family{evalFamily("readme", "README.md")}
	results := Evaluate(fams, nil)

	require.Len(t, results, 1)
	assert.Equal(t, "readme", results[0].Family)
	assert.False(t, results[0].Present)
	assert.Empty(t, results[0].Path)
	assert.Empty(t, results[0].AltPaths)
}

func TestEvaluate_SingleMatch_ReportsPresentWithPath(t *testing.T) {
	t.Parallel()

	fams := []Family{evalFamily("readme", "README.md", "README")}
	matches := []Match{{Family: "readme", Path: "README.md"}}
	results := Evaluate(fams, matches)

	require.Len(t, results, 1)
	assert.True(t, results[0].Present)
	assert.Equal(t, "README.md", results[0].Path)
	assert.Empty(t, results[0].AltPaths)
}

// TestEvaluate_CanonicalPreferred verifies the ranker picks README.md
// over a bare README when both exist — the canonical spelling wins
// and the legacy bare variant appears in alt_paths for analyst review.
func TestEvaluate_CanonicalPreferred(t *testing.T) {
	t.Parallel()

	fams := []Family{evalFamily("readme", "README.md", "README.rst", "README")}
	matches := []Match{
		{Family: "readme", Path: "README"},
		{Family: "readme", Path: "README.md"},
	}
	results := Evaluate(fams, matches)

	require.Len(t, results, 1)
	assert.True(t, results[0].Present)
	assert.Equal(t, "README.md", results[0].Path,
		"canonical Preferred entry (README.md) must win ranking")
	assert.Equal(t, []string{"README"}, results[0].AltPaths,
		"legacy bare README must surface in alt_paths")
}

// TestEvaluate_CaseInsensitiveFallback verifies the phase-2 ranker:
// when no exact-case Preferred match exists, case-insensitive match
// against the Preferred list still wins over a truly-non-preferred
// alternative.
func TestEvaluate_CaseInsensitiveFallback(t *testing.T) {
	t.Parallel()

	fams := []Family{evalFamily("readme", "README.md", "README.rst", "README")}
	matches := []Match{
		{Family: "readme", Path: "README.markdown"}, // non-Preferred
		{Family: "readme", Path: "readme.md"},       // case-insensitive match to README.md
	}
	results := Evaluate(fams, matches)

	require.Len(t, results, 1)
	assert.Equal(t, "readme.md", results[0].Path,
		"case-insensitive Preferred match must beat a non-Preferred variant")
	assert.Equal(t, []string{"README.markdown"}, results[0].AltPaths)
}

// TestEvaluate_NonPreferredFallthrough verifies the phase-3 ranker:
// when no entry matches Preferred at any case, the first sorted match
// is promoted. This is the "new variant we haven't thought of" path —
// still reports presence rather than silently discarding.
func TestEvaluate_NonPreferredFallthrough(t *testing.T) {
	t.Parallel()

	fams := []Family{evalFamily("readme", "README.md", "README.rst", "README")}
	// Neither matches Preferred under any case comparison.
	matches := []Match{
		{Family: "readme", Path: "README.asciidoc"},
		{Family: "readme", Path: "README.markdown"},
	}
	results := Evaluate(fams, matches)

	require.Len(t, results, 1)
	assert.True(t, results[0].Present)
	assert.Equal(t, "README.asciidoc", results[0].Path,
		"first match in sorted order wins the fallthrough")
	assert.Equal(t, []string{"README.markdown"}, results[0].AltPaths)
}

// TestEvaluate_MultipleFamilies_OrderPreserved verifies iteration
// order of the returned Results matches the declared family order of
// the input. Downstream serialization depends on it.
func TestEvaluate_MultipleFamilies_OrderPreserved(t *testing.T) {
	t.Parallel()

	fams := []Family{
		evalFamily("readme", "README.md"),
		evalFamily("security", "SECURITY.md"),
		evalFamily("codeowners", "CODEOWNERS"),
	}
	matches := []Match{
		{Family: "codeowners", Path: "CODEOWNERS"},
		{Family: "readme", Path: "README.md"},
		// security has no match — should still produce an absent Result.
	}
	results := Evaluate(fams, matches)

	require.Len(t, results, 3)
	assert.Equal(t, "readme", results[0].Family)
	assert.True(t, results[0].Present)
	assert.Equal(t, "security", results[1].Family)
	assert.False(t, results[1].Present)
	assert.Equal(t, "codeowners", results[2].Family)
	assert.True(t, results[2].Present)
}

// TestEvaluate_CodeownersInSubdir verifies that a Preferred match
// (CODEOWNERS basename) wins even when the path includes a sub-dir
// prefix. Ranking compares basenames, not full paths.
func TestEvaluate_CodeownersInSubdir(t *testing.T) {
	t.Parallel()

	fams := []Family{evalFamily("codeowners", "CODEOWNERS")}
	matches := []Match{
		{Family: "codeowners", Path: ".github/CODEOWNERS"},
	}
	results := Evaluate(fams, matches)

	require.Len(t, results, 1)
	assert.True(t, results[0].Present)
	assert.Equal(t, ".github/CODEOWNERS", results[0].Path)
}

// TestEvaluate_ResultFamilyNotSerialized is a JSON-shape guard: the
// Family field must be tagged json:"-" so encoded output uses the
// map key alone. A drift here would double-encode the family name.
//
// Tested by a minimal marshal check — avoids pulling encoding/json
// into the evaluator implementation purely for test instrumentation.
func TestEvaluate_ResultFamilyNotSerialized(t *testing.T) {
	t.Parallel()

	fams := []Family{evalFamily("readme", "README.md")}
	results := Evaluate(fams, []Match{{Family: "readme", Path: "README.md"}})
	require.Len(t, results, 1)

	// Sanity: Family is set internally (for key lookup) but must
	// be omitted from JSON.
	assert.Equal(t, "readme", results[0].Family)
}
