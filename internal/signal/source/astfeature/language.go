package astfeature

import "strings"

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
	return languageFor(path, false)
}

// LanguageForChangedFile is the PR-defense variant of LanguageForPath: it
// INCLUDES test files (conftest.py, *_test.go, tests/, benches/, …),
// because a changed test file is authored code an attacker abuses — the
// prt-scan campaign injected payloads into conftest.py, which runs at
// pytest collection. It still excludes vendored / build-output /
// declaration / minified files: third-party or generated code that is
// AST-noisy and would false-positive a legitimate dependency bump.
//
// The two functions share the extension and non-test exclusion logic
// below, so the source-evolution and PR-defense consumers cannot drift on
// what counts as which language.
func LanguageForChangedFile(path string) (language string, included bool) {
	return languageFor(path, true)
}

func languageFor(path string, includeTests bool) (language string, included bool) {
	switch {
	case includedGo(path, includeTests):
		return LangGo, true
	case includedPython(path, includeTests):
		return LangPython, true
	case includedNode(path, includeTests):
		return LangJavaScript, true
	case includedRust(path, includeTests):
		return LangRust, true
	default:
		return "", false
	}
}

// includedGo: a .go file. Test files (_test.go) are excluded unless
// includeTests; vendored code is always excluded.
func includedGo(path string, includeTests bool) bool {
	if !strings.HasSuffix(path, ".go") {
		return false
	}
	if !includeTests && strings.HasSuffix(path, "_test.go") {
		return false
	}
	if path == "vendor" || strings.HasPrefix(path, "vendor/") || strings.Contains(path, "/vendor/") {
		return false
	}
	return true
}

// includedPython: a .py file. Test files (conftest.py, test_*, *_test.py,
// and tests/ test/ dirs) are excluded unless includeTests; vendored and
// virtual-env trees are always excluded.
func includedPython(path string, includeTests bool) bool {
	if !strings.HasSuffix(path, ".py") {
		return false
	}
	base := path
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	if !includeTests && (base == "conftest.py" ||
		strings.HasPrefix(base, "test_") ||
		strings.HasSuffix(base, "_test.py")) {
		return false
	}
	for seg := range strings.SplitSeq(path, "/") {
		switch seg {
		case "tests", "test":
			if !includeTests {
				return false
			}
		case "vendor", "_vendor", "site-packages", ".venv", "venv":
			return false
		}
	}
	return true
}

// includedNode: a JS/TS authored source file. .d.ts declarations and
// minified bundles are always excluded (no useful AST); test/spec files
// and __tests__/test/tests dirs are excluded unless includeTests;
// node_modules and build-output dirs are always excluded.
func includedNode(path string, includeTests bool) bool {
	if strings.HasSuffix(path, ".d.ts") {
		return false
	}
	ext := false
	for _, suf := range []string{".js", ".mjs", ".cjs", ".ts", ".tsx", ".jsx"} {
		if strings.HasSuffix(path, suf) {
			ext = true
			break
		}
	}
	if !ext {
		return false
	}

	base := path
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	if strings.Contains(base, ".min.") {
		return false
	}
	if !includeTests && (strings.Contains(base, ".test.") || strings.Contains(base, ".spec.")) {
		return false
	}
	for seg := range strings.SplitSeq(path, "/") {
		switch seg {
		case "__tests__", "test", "tests":
			if !includeTests {
				return false
			}
		case "node_modules", "dist", "build", "out":
			return false
		}
	}
	return true
}

// includedRust: a .rs source file plus build.rs (the cargo build-time
// entry point). tests/ benches/ examples/ dirs are excluded unless
// includeTests; target/ build output and vendor/ are always excluded.
func includedRust(path string, includeTests bool) bool {
	if !strings.HasSuffix(path, ".rs") {
		return false
	}
	for seg := range strings.SplitSeq(path, "/") {
		switch seg {
		case "tests", "benches", "examples":
			if !includeTests {
				return false
			}
		case "target", "vendor":
			return false
		}
	}
	return true
}
