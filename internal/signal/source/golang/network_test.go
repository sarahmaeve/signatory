package golang

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAnalyze_HTTPGet_CountsOne(t *testing.T) {
	t.Parallel()

	src := `package main

import "net/http"

func init() {
	_, _ = http.Get("https://example.com")
}
`
	feats := analyzeOne(t, "main.go", src)
	assert.Equal(t, 1, feats.NetworkCallSites)
}

func TestAnalyze_AliasedHTTPImport_CountsOne(t *testing.T) {
	t.Parallel()

	// `import h "net/http"` aliases the package locally to `h`.
	// A regex looking for `http.Get` would miss this; AST resolves
	// `h` against the import map and matches net/http.Get.
	src := `package main

import h "net/http"

func init() {
	_, _ = h.Get("https://example.com")
}
`
	feats := analyzeOne(t, "main.go", src)
	assert.Equal(t, 1, feats.NetworkCallSites)
}

func TestAnalyze_NetDial_Counts(t *testing.T) {
	t.Parallel()

	src := `package main

import "net"

func init() {
	_, _ = net.Dial("tcp", "1.2.3.4:80")
}
`
	feats := analyzeOne(t, "main.go", src)
	assert.Equal(t, 1, feats.NetworkCallSites)
}

func TestAnalyze_HTTPNewRequestVariants_BothCount(t *testing.T) {
	t.Parallel()

	src := `package main

import (
	"context"
	"net/http"
)

func init() {
	_, _ = http.NewRequest("GET", "https://example.com", nil)
	_, _ = http.NewRequestWithContext(context.Background(), "GET", "https://x", nil)
}
`
	feats := analyzeOne(t, "main.go", src)
	assert.Equal(t, 2, feats.NetworkCallSites)
}

func TestAnalyze_LocalPackageWithSimilarLeafName_NotCounted(t *testing.T) {
	t.Parallel()

	// A locally-defined package aliased to "http" but pointing at
	// a different import path must NOT count as net/http. This
	// validates that the catalog matches on full import path, not
	// on the local identifier.
	src := `package main

import http "example.com/myhttp"

func init() {
	_ = http.Get("anything")
}
`
	feats := analyzeOne(t, "main.go", src)
	assert.Equal(t, 0, feats.NetworkCallSites)
}

func TestAnalyze_StringLiteralContainsCallExpression_NotCounted(t *testing.T) {
	t.Parallel()

	// Defends against a regex/lexer implementation. A string literal
	// that textually contains "http.Get(...)" must not count. AST
	// passes this naturally because string literals are *ast.BasicLit
	// nodes, not *ast.CallExpr.
	src := `package main

import "net/http"

const helpText = "Use http.Get(url) for simple cases."

func init() {
	_ = helpText
	_ = http.DefaultClient
}
`
	feats := analyzeOne(t, "main.go", src)
	assert.Equal(t, 0, feats.NetworkCallSites)
}

func TestAnalyze_DotImportNetHTTP_NotCountedV01(t *testing.T) {
	t.Parallel()

	// Dot imports bring symbols into the file namespace unqualified.
	// v0.1 explicitly does NOT resolve these (see Q4 in coll7.md).
	// Document the gap with a test so future work that closes it
	// will fail this assertion and force a deliberate update.
	src := `package main

import . "net/http"

func init() {
	_, _ = Get("https://example.com")
}
`
	feats := analyzeOne(t, "main.go", src)
	assert.Equal(t, 0, feats.NetworkCallSites)
}

func TestAnalyze_BlankImportNetHTTP_NoFalsePositive(t *testing.T) {
	t.Parallel()

	// Blank imports run the package's init() but expose no symbols.
	// No call sites to detect; count must be zero.
	src := `package main

import _ "net/http"

func init() {
}
`
	feats := analyzeOne(t, "main.go", src)
	assert.Equal(t, 0, feats.NetworkCallSites)
}

func TestAnalyze_HTTPGetOutsideInit_StillCounts(t *testing.T) {
	t.Parallel()

	// The analyzer counts every call site, regardless of enclosing
	// function. Whether init() vs other functions matters is the
	// analyst's interpretation; the matrix surfaces the count.
	src := `package main

import "net/http"

func loadConfig() {
	_, _ = http.Get("https://example.com")
}
`
	feats := analyzeOne(t, "main.go", src)
	assert.Equal(t, 1, feats.NetworkCallSites)
}

func TestAnalyze_HTTPDefaultClientDo_CountsViaMethodChain(t *testing.T) {
	t.Parallel()

	// http.DefaultClient.Do(req) is a two-level method chain:
	// pkg.Var.Method. callSiteOf resolves it to
	// {Pkg: "net/http", Fn: "DefaultClient.Do"}, which matches the
	// catalog. Local variables bound to *http.Client (next test)
	// remain a v0.1 gap.
	src := `package main

import "net/http"

func init() {
	req, _ := http.NewRequest("GET", "https://example.com", nil)
	_, _ = http.DefaultClient.Do(req)
}
`
	feats := analyzeOne(t, "main.go", src)
	// http.NewRequest counts; http.DefaultClient.Do counts.
	assert.Equal(t, 2, feats.NetworkCallSites)
}

func TestAnalyze_HTTPLocalClientVarDo_NotCountedV01(t *testing.T) {
	t.Parallel()

	// `client := &http.Client{}; client.Do(req)` binds a *http.Client
	// to a local variable, then calls .Do on it. The selector's X is
	// the local Ident "client" which isn't in the import map; v0.1
	// has no type info to resolve client back to net/http. Documented
	// gap; future work could add intra-procedural binding tracking.
	src := `package main

import "net/http"

func init() {
	client := &http.Client{}
	req, _ := http.NewRequest("GET", "https://example.com", nil)
	_, _ = client.Do(req)
}
`
	feats := analyzeOne(t, "main.go", src)
	// http.NewRequest counts (case 1); client.Do does not (local var).
	assert.Equal(t, 1, feats.NetworkCallSites)
}

func TestAnalyze_ThreeLevelMethodChain_NotCountedV01(t *testing.T) {
	t.Parallel()

	// http.DefaultClient.Transport.RoundTrip(req) is a three-level
	// chain. callSiteOf handles two levels; deeper chains are a
	// documented v0.1 gap. The legitimate-use surface for chains
	// this deep in init() is small enough that the gap is acceptable.
	src := `package main

import "net/http"

func init() {
	req, _ := http.NewRequest("GET", "https://example.com", nil)
	_, _ = http.DefaultClient.Transport.RoundTrip(req)
}
`
	feats := analyzeOne(t, "main.go", src)
	// Only http.NewRequest matches; the three-level chain doesn't.
	assert.Equal(t, 1, feats.NetworkCallSites)
}

// TestPatternsCatalog_NoDuplicates is a hygiene check on the
// catalog itself: a duplicate entry would silently inflate counts
// when slices.Contains matches twice (it doesn't, but reviewers
// would have to think about it). Better to assert no duplicates.
func TestPatternsCatalog_NoDuplicates(t *testing.T) {
	t.Parallel()

	seen := make(map[CallSite]struct{}, len(NetworkEgressCallSites))
	for _, cs := range NetworkEgressCallSites {
		_, dup := seen[cs]
		require.Falsef(t, dup, "duplicate CallSite %+v", cs)
		seen[cs] = struct{}{}
	}
	// Sanity: not empty.
	assert.NotEmpty(t, NetworkEgressCallSites)
	// Sanity: every entry has both fields populated.
	for _, cs := range NetworkEgressCallSites {
		assert.NotEmpty(t, cs.Pkg)
		assert.NotEmpty(t, cs.Fn)
	}
	// Quiet the unused-import in some build modes.
	_ = slices.Contains[[]CallSite, CallSite]
}

// analyzeOne is a test helper for single-file analysis. Most network
// tests in this file have one source file; this collapses the
// iterator boilerplate.
func analyzeOne(t *testing.T, path, content string) Features {
	t.Helper()
	a := NewAnalyzer()
	files := func(yield func(SourceFile, error) bool) {
		yield(SourceFile{Path: path, Content: []byte(content)}, nil)
	}
	feats, err := a.Analyze(t.Context(), files)
	require.NoError(t, err)
	return feats
}
