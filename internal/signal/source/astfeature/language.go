package astfeature

import (
	"slices"
	"strings"
)

// Language identifiers returned by LanguageForPath / LanguageForChangedFile.
// They match the per-language analyzers' Language() values so the routing
// key is consistent across the source-evolution pipeline and the
// PR-defense scanner.
const (
	LangGo         = "go"
	LangPython     = "python"
	LangJavaScript = "javascript"
	LangRust       = "rust"
)

// Inclusion is the PR-defense classification of a changed path, returned by
// LanguageForChangedFile. It distinguishes two cases that the baseline
// boolean filter collapses into "false":
//
//   - NotSource    — not a source file we analyze (or a type-only .d.ts,
//     which has no runtime AST).
//   - Included     — a source file to AST-analyze on the normal block path.
//   - ExcludedTree — a source file deliberately placed in a vendored /
//     build-output / minified tree. For the source-evolution baseline that
//     is third-party-or-generated noise; but in a PR it is THIS changelist's
//     attacker-authored bytes, so PR-defense still scans it — outside the
//     block tier (see internal/prdefense.Scan).
type Inclusion int

const (
	NotSource Inclusion = iota
	Included
	ExcludedTree
)

// LanguageForPath classifies a posix-style, repo-relative path as one of
// the source languages the AST analyzers consume, EXCLUDING test files,
// vendored / build-output trees, TypeScript declaration files, minified
// bundles, and non-source extensions. This is the cross-version
// source-evolution baseline filter: the BlobStreamer predicates
// (isGoSourceFile, …) delegate here, where test code is noise that would
// pollute a package's runtime-AST baseline.
//
// PR-defense uses LanguageForChangedFile instead — see its doc.
func LanguageForPath(path string) (language string, included bool) {
	lang, ok := extLanguage(path)
	if !ok || inExcludedTree(lang, path) || isTestPath(lang, path) {
		return "", false
	}
	return lang, true
}

// LanguageForChangedFile is the PR-defense variant of LanguageForPath. It
// INCLUDES test files (conftest.py, *_test.go, tests/, benches/, …),
// because a changed test file is authored code an attacker abuses — the
// prt-scan campaign injected payloads into conftest.py, which runs at
// pytest collection.
//
// Unlike the baseline, it does NOT silently drop vendored / build-output /
// minified source: it returns ExcludedTree for those so PR-defense can scan
// them outside the block tier rather than ignore them (a changed file under
// vendor/ or named *.min.js is attacker-authored in this PR — invisible
// AST exclusion there was a content-block bypass). Only genuinely
// AST-less inputs (non-source extensions, .d.ts declarations) are NotSource.
//
// The extension and exclusion predicates are shared with LanguageForPath,
// so the source-evolution and PR-defense consumers cannot drift on what
// counts as which language or which tree.
func LanguageForChangedFile(path string) (language string, inclusion Inclusion) {
	lang, ok := extLanguage(path)
	if !ok {
		return "", NotSource
	}
	if inExcludedTree(lang, path) {
		return lang, ExcludedTree
	}
	return lang, Included
}

// extLanguage returns the source language implied by the path's extension —
// the single source of truth for extension→language. A .d.ts TypeScript
// declaration is NOT a source file: it is type-only and carries no runtime
// AST, so it returns ("", false) and is never scanned by either consumer.
func extLanguage(path string) (string, bool) {
	if strings.HasSuffix(path, ".d.ts") {
		return "", false
	}
	switch {
	case strings.HasSuffix(path, ".go"):
		return LangGo, true
	case strings.HasSuffix(path, ".py"):
		return LangPython, true
	case strings.HasSuffix(path, ".rs"):
		return LangRust, true
	}
	for _, suf := range []string{".js", ".mjs", ".cjs", ".ts", ".tsx", ".jsx"} {
		if strings.HasSuffix(path, suf) {
			return LangJavaScript, true
		}
	}
	return "", false
}

// inExcludedTree reports whether path sits in a vendored / build-output /
// minified location for its language — third-party or generated code that
// is AST-noisy for the source-evolution baseline and would false-positive a
// legitimate dependency bump.
func inExcludedTree(lang, path string) bool {
	switch lang {
	case LangGo:
		return hasPathSegment(path, "vendor")
	case LangPython:
		return hasPathSegment(path, "vendor", "_vendor", "site-packages", ".venv", "venv")
	case LangJavaScript:
		if strings.Contains(lastSegment(path), ".min.") {
			return true
		}
		return hasPathSegment(path, "node_modules", "dist", "build", "out")
	case LangRust:
		return hasPathSegment(path, "target", "vendor")
	}
	return false
}

// isTestPath reports whether path is a test/spec file or lives in a test
// directory for its language. The baseline excludes these (test code
// pollutes a runtime-AST baseline); PR-defense includes them.
func isTestPath(lang, path string) bool {
	base := lastSegment(path)
	switch lang {
	case LangGo:
		return strings.HasSuffix(path, "_test.go")
	case LangPython:
		if base == "conftest.py" || strings.HasPrefix(base, "test_") || strings.HasSuffix(base, "_test.py") {
			return true
		}
		return hasPathSegment(path, "tests", "test")
	case LangJavaScript:
		if strings.Contains(base, ".test.") || strings.Contains(base, ".spec.") {
			return true
		}
		return hasPathSegment(path, "__tests__", "test", "tests")
	case LangRust:
		return hasPathSegment(path, "tests", "benches", "examples")
	}
	return false
}

// hasPathSegment reports whether any "/"-delimited segment of path equals
// one of segs (matching "vendor", "a/vendor/b", "vendor/x" but not
// "myvendor" or "vendorx").
func hasPathSegment(path string, segs ...string) bool {
	for s := range strings.SplitSeq(path, "/") {
		if slices.Contains(segs, s) {
			return true
		}
	}
	return false
}

func lastSegment(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}
