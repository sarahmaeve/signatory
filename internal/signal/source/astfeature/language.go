package astfeature

import "strings"

// Language identifiers returned by LanguageForPath. They match the
// per-language analyzers' Language() values so the routing key is
// consistent across the source-evolution pipeline and the PR-defense
// scanner.
const (
	LangGo         = "go"
	LangPython     = "python"
	LangJavaScript = "javascript"
	LangRust       = "rust"
)

// LanguageForPath classifies a posix-style, repo-relative path as one of
// the source languages the AST analyzers consume, applying that
// language's test / vendor / build-output exclusions. It returns
// ("", false) for anything that is not authored runtime source in a
// supported language: tests, vendored or build-output trees, TypeScript
// declaration files, minified bundles, and non-source extensions.
//
// This is the single source of truth for source-file inclusion. The
// source-evolution BlobStreamer predicates (isGoSourceFile, …) delegate
// here, and the PR-defense changelist scanner uses it to route each
// changed file to the right analyzer — so the include/exclude logic
// cannot drift between the two consumers.
func LanguageForPath(path string) (language string, included bool) {
	switch {
	case includedGo(path):
		return LangGo, true
	case includedPython(path):
		return LangPython, true
	case includedNode(path):
		return LangJavaScript, true
	case includedRust(path):
		return LangRust, true
	default:
		return "", false
	}
}

// includedGo: a .go file that is the package's own importable code —
// not a _test.go file, not vendored.
func includedGo(path string) bool {
	if !strings.HasSuffix(path, ".go") {
		return false
	}
	if strings.HasSuffix(path, "_test.go") {
		return false
	}
	if path == "vendor" {
		return false
	}
	if strings.HasPrefix(path, "vendor/") {
		return false
	}
	if strings.Contains(path, "/vendor/") {
		return false
	}
	return true
}

// includedPython: a .py file that is the package's own importable
// runtime source — not tests, not vendored / virtual-env trees.
func includedPython(path string) bool {
	if !strings.HasSuffix(path, ".py") {
		return false
	}
	base := path
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	if base == "conftest.py" ||
		strings.HasPrefix(base, "test_") ||
		strings.HasSuffix(base, "_test.py") {
		return false
	}
	for seg := range strings.SplitSeq(path, "/") {
		switch seg {
		case "tests", "test", "vendor", "_vendor", "site-packages", ".venv", "venv":
			return false
		}
	}
	return true
}

// includedNode: a JS/TS authored runtime source file — not a .d.ts
// declaration, not minified, not a test/spec file, not vendored or
// build output.
func includedNode(path string) bool {
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
	if strings.Contains(base, ".test.") || strings.Contains(base, ".spec.") {
		return false
	}
	for seg := range strings.SplitSeq(path, "/") {
		switch seg {
		case "__tests__", "test", "tests", "node_modules", "dist", "build", "out":
			return false
		}
	}
	return true
}

// includedRust: a .rs authored source file plus build.rs (the cargo
// build-time entry point) — not tests, benches, examples, build output,
// or vendored deps.
func includedRust(path string) bool {
	if !strings.HasSuffix(path, ".rs") {
		return false
	}
	for seg := range strings.SplitSeq(path, "/") {
		switch seg {
		case "tests", "benches", "examples", "target", "vendor":
			return false
		}
	}
	return true
}
