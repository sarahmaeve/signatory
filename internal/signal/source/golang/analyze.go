// Package golang provides AST-based feature extraction for Go source
// code, used by the source-evolution collector to compute per-tagged-
// version feature counts.
//
// The analyzer consumes (path, content) pairs through an iterator and
// emits a Features struct. It never touches the filesystem; tests
// construct in-memory file iterators and feed them through.
//
// Pattern catalogs (sensitive paths, network-egress call sites, exec
// call sites) live in patterns.go and are package-level vars to keep
// changes auditable through PR review.
//
// Why AST instead of regex: the questions the matrix asks are
// structural ("is this a top-level init? is this a net/http.Get
// call?"), not lexical. A regex implementation would false-positive
// on comments, strings, and method receivers, and false-negative on
// aliased imports. The matrix's "forgery resistance VERY HIGH" claim
// rests on faithful structural reads; AST is the cost-zero way to
// get there because go/parser is in the standard library.
//
// Known v0.1 gaps (documented in matrix caveats so analysts know):
//   - Dot imports (`import . "net/http"`): unqualified calls to
//     dot-imported packages are NOT resolved. Rare in package init.
//   - Method calls on values of an imported type (e.g.,
//     `http.DefaultClient.Do(req)`): not matched. Package-level
//     function counterparts (`http.Get`, `http.NewRequest`) cover
//     the common path; type-resolved method matching is future work.
package golang

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"iter"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/sarahmaeve/signatory/internal/signal/source/astfeature"
)

// Features is the shared, language-neutral AST construct-count record.
// It lives in the astfeature package as Counts; this alias keeps the
// golang analyzer's call sites reading naturally — the analyzer
// genuinely produces the shared type, it does not own it.
type Features = astfeature.Counts

// SourceFile is the shared, language-neutral analyzer input. It lives
// in the astfeature package; this alias keeps the golang analyzer and
// its tests reading naturally.
type SourceFile = astfeature.SourceFile

// Analyzer walks Go source AST and accumulates astfeature.Counts.
//
// Stateless across calls; safe to reuse. Constructor exists so
// future commits can add WithPatterns / WithThreshold options
// without breaking callers.
type Analyzer struct{}

// NewAnalyzer returns an Analyzer with the default pattern catalogs.
func NewAnalyzer() *Analyzer {
	return &Analyzer{}
}

// Analyze parses the iterator of source files and returns aggregated
// Features.
//
// Parser errors on individual files are non-fatal: the file is
// skipped and counting proceeds. Threat model is "what features
// does this version's source contain?" — a malformed file is
// interesting but doesn't kill the matrix; the analyst still sees
// the features from valid files in that version.
//
// Errors yielded by the upstream iterator (e.g., the BlobStreamer
// reporting a blob fetch failure mid-stream) are returned to the
// caller. Partial counts are abandoned because partial-result rows
// would mislead the matrix.
//
// Context cancellation is honored between files.
func (a *Analyzer) Analyze(ctx context.Context, files iter.Seq2[SourceFile, error]) (Features, error) {
	var feats Features
	fset := token.NewFileSet()

	for file, err := range files {
		if err != nil {
			return Features{}, err
		}
		if err := ctx.Err(); err != nil {
			return Features{}, err
		}

		f, perr := parser.ParseFile(fset, file.Path, file.Content, parser.ParseComments)
		if perr != nil {
			// Malformed file. Skip; do not propagate. The caller's
			// matrix-row assembler may record file-level parse counts
			// in structural fields later.
			continue
		}

		imports := buildImportMap(f)
		a.walkFile(f, imports, &feats)
	}
	return feats, nil
}

// walkFile inspects one parsed file's AST and increments the
// relevant Features fields in place.
func (a *Analyzer) walkFile(f *ast.File, imports map[string]string, feats *Features) {
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncDecl:
			if isPackageInit(x) {
				feats.InitCount++
			}
		case *ast.AssignStmt:
			if x.Tok == token.XOR_ASSIGN {
				feats.XORAssignments++
			}
		case *ast.CallExpr:
			site, ok := callSiteOf(x, imports)
			if !ok {
				return true
			}
			if slices.Contains(NetworkEgressCallSites, site) {
				feats.NetworkCallSites++
			}
			if slices.Contains(SensitivePathReadCallSites, site) {
				if pathArgIsSensitive(x.Args, imports) {
					feats.SensitivePathReads++
				}
			}
			if slices.Contains(ExecCallSites, site) {
				feats.ExecCalls++
			}
			if slices.Contains(Base64DecodeCallSites, site) {
				feats.Base64DecodeCalls++
			}
		}
		return true
	})
}

// isPackageInit reports whether fn is a top-level init() declaration:
// name is "init", no receiver. A method named init on some type does
// not run at package import and is not counted.
func isPackageInit(fn *ast.FuncDecl) bool {
	return fn.Name.Name == "init" && fn.Recv == nil
}

// pathArgIsSensitive reports whether the first call argument
// statically resolves to a path containing any SensitivePathPatterns
// entry. Returns false for empty args, dynamic args, or paths that
// don't match the catalog.
func pathArgIsSensitive(args []ast.Expr, imports map[string]string) bool {
	if len(args) == 0 {
		return false
	}
	p, ok := extractPathLiteral(args[0], imports)
	if !ok {
		return false
	}
	return pathMatchesAnyPattern(p)
}

// pathMatchesAnyPattern reports whether p contains any
// SensitivePathPatterns entry as a substring.
func pathMatchesAnyPattern(p string) bool {
	for _, pat := range SensitivePathPatterns {
		if strings.Contains(p, pat) {
			return true
		}
	}
	return false
}

// extractPathLiteral statically resolves an expression to a path
// string. Handles:
//   - String literals: "foo/bar" -> "foo/bar"
//   - filepath.Join calls: filepath.Join("a", "b", "c") -> "a/b/c";
//     dynamic args (identifiers, function calls other than nested
//     filepath.Join) are dropped from the fold so a partially-static
//     path still surfaces its sensitive substring.
//
// Returns ok=false for fully-dynamic expressions, malformed string
// literals, and any other expression shape. The matrix's caveats list
// names the propagation gaps (local consts, string concatenation,
// fmt.Sprintf) that future work may close.
func extractPathLiteral(expr ast.Expr, imports map[string]string) (string, bool) {
	switch x := expr.(type) {
	case *ast.BasicLit:
		if x.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(x.Value)
		if err != nil {
			return "", false
		}
		return s, true
	case *ast.CallExpr:
		cs, ok := callSiteOf(x, imports)
		if !ok || cs != (CallSite{Pkg: "path/filepath", Fn: "Join"}) {
			return "", false
		}
		return foldFilepathJoin(x.Args, imports)
	}
	return "", false
}

// foldFilepathJoin collects statically-resolvable parts from a
// filepath.Join argument list and joins them with "/". Drops any
// dynamic args; if every arg is dynamic, returns ok=false.
//
// Uses path.Clean (POSIX) rather than filepath.Clean so the result
// is platform-independent — patterns are POSIX-flavored, and the
// host OS that runs the analyzer is irrelevant to what the source
// code expresses.
func foldFilepathJoin(args []ast.Expr, imports map[string]string) (string, bool) {
	var parts []string
	for _, arg := range args {
		s, ok := extractPathLiteral(arg, imports)
		if !ok {
			continue
		}
		parts = append(parts, s)
	}
	if len(parts) == 0 {
		return "", false
	}
	return path.Clean(strings.Join(parts, "/")), true
}

// buildImportMap returns local-name -> import-path for all imports
// in the file. Default name is the last segment of the path; named
// imports use the explicit name. Blank imports (`_`) and dot imports
// (`.`) are skipped — v0.1 accepts the dot-import gap; resolving
// dot-imported names requires either type-checked symbol resolution
// or a much heavier import-aware walker.
func buildImportMap(f *ast.File) map[string]string {
	imports := make(map[string]string, len(f.Imports))
	for _, imp := range f.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		var name string
		switch {
		case imp.Name == nil:
			name = lastSegment(path)
		case imp.Name.Name == "_":
			// Blank import: package init runs but no symbols enter
			// the file's namespace. Nothing to resolve.
			continue
		case imp.Name.Name == ".":
			// Dot import: symbols enter the file's namespace
			// unqualified. v0.1 doesn't resolve these; documented
			// in package caveats and Q4.
			continue
		default:
			name = imp.Name.Name
		}
		imports[name] = path
	}
	return imports
}

// lastSegment returns the path element after the final "/", or the
// whole string if there's no slash. Used for default Go import-name
// resolution: `import "net/http"` exposes the symbol `http`.
func lastSegment(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

// callSiteOf extracts a (package-path, function-or-method-chain) pair
// from a call expression. Two shapes are recognized:
//
//  1. Direct package call: `pkg.Fn(...)`
//     Returns {Pkg: <import-path>, Fn: "Fn"}.
//
//  2. Two-level method chain on a package-level value:
//     `pkg.Var.Method(...)`
//     Returns {Pkg: <import-path>, Fn: "Var.Method"}.
//     Catalog entries match by composing the leaf names with a dot,
//     e.g., {Pkg: "net/http", Fn: "DefaultClient.Do"}.
//
// Returns ok=false for any other call shape:
//   - bare function call `Fn(...)` (no selector)
//   - method call on a local value `x.Method(...)` (X not in imports)
//   - three-or-more-level chains `pkg.Sub.Var.Method(...)` (gap)
//   - struct-literal method calls `pkg.Type{}.Method(...)` (gap)
//   - pointer-of-literal `(&pkg.Type{}).Method(...)` (gap)
//
// All gaps are documented in package caveats; the analyzer is
// intentionally conservative on resolution to avoid false-positive
// matches.
func callSiteOf(call *ast.CallExpr, imports map[string]string) (CallSite, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return CallSite{}, false
	}

	// Case 1: pkg.Fn(...) — sel.X is *ast.Ident.
	if ident, ok := sel.X.(*ast.Ident); ok {
		pkgPath, known := imports[ident.Name]
		if !known {
			return CallSite{}, false
		}
		return CallSite{Pkg: pkgPath, Fn: sel.Sel.Name}, true
	}

	// Case 2: pkg.Var.Method(...) — sel.X is *ast.SelectorExpr whose
	// inner X is *ast.Ident in the import map.
	innerSel, ok := sel.X.(*ast.SelectorExpr)
	if !ok {
		return CallSite{}, false
	}
	innerIdent, ok := innerSel.X.(*ast.Ident)
	if !ok {
		return CallSite{}, false
	}
	pkgPath, known := imports[innerIdent.Name]
	if !known {
		return CallSite{}, false
	}
	return CallSite{
		Pkg: pkgPath,
		Fn:  innerSel.Sel.Name + "." + sel.Sel.Name,
	}, true
}
