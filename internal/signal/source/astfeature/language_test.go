package astfeature

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestLanguageForPath pins the single source of truth for source-file
// inclusion shared by the BlobStreamer predicates and the PR-defense
// changelist router. Test/vendor/build-output paths and non-source
// extensions are excluded; build.rs is admitted (supply-chain entry).
func TestLanguageForPath(t *testing.T) {
	t.Parallel()

	cases := []struct {
		path     string
		wantLang string
		wantOK   bool
	}{
		// Go
		{"main.go", LangGo, true},
		{"pkg/util.go", LangGo, true},
		{"foo_test.go", "", false},
		{"vendor/x/y.go", "", false},
		{"pkg/vendor/dep.go", "", false},
		// Python
		{"app.py", LangPython, true},
		{"test_app.py", "", false},
		{"app_test.py", "", false},
		{"conftest.py", "", false},
		{"tests/unit.py", "", false},
		{".venv/lib/x.py", "", false},
		// JS/TS (node analyzer reports "javascript")
		{"index.js", LangJavaScript, true},
		{"a.mjs", LangJavaScript, true},
		{"src/x.tsx", LangJavaScript, true},
		{"types.d.ts", "", false},
		{"bundle.min.js", "", false},
		{"x.test.tsx", "", false},
		{"y.spec.js", "", false},
		{"node_modules/dep/index.js", "", false},
		{"dist/bundle.js", "", false},
		// Rust
		{"src/lib.rs", LangRust, true},
		{"build.rs", LangRust, true},
		{"tests/it.rs", "", false},
		{"target/debug/x.rs", "", false},
		// Non-source
		{"README.md", "", false},
		{"go.mod", "", false},
		{"CLAUDE.md", "", false},
		{".github/workflows/ci.yml", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()
			lang, ok := LanguageForPath(tc.path)
			assert.Equal(t, tc.wantOK, ok, "included?")
			assert.Equal(t, tc.wantLang, lang, "language")
		})
	}
}

// TestLanguageForChangedFile pins the PR-defense routing: same language
// detection as LanguageForPath, but test files ARE included (an attacker
// hides payloads in test code — the prt-scan conftest.py vector). Vendored
// / build-output / minified source is classified ExcludedTree — PR-defense
// scans it outside the block tier rather than silently dropping it (that
// silent drop was a content-block bypass). Only non-source files and
// type-only .d.ts declarations are NotSource.
func TestLanguageForChangedFile(t *testing.T) {
	t.Parallel()

	cases := []struct {
		path     string
		wantLang string
		wantIncl Inclusion
	}{
		// Test files — EXCLUDED by LanguageForPath, INCLUDED here.
		{"conftest.py", LangPython, Included},
		{"tests/unit.py", LangPython, Included},
		{"app_test.py", LangPython, Included},
		{"pkg/widget_test.go", LangGo, Included},
		{"src/x.test.tsx", LangJavaScript, Included},
		{"__tests__/a.js", LangJavaScript, Included},
		{"benches/b.rs", LangRust, Included},
		{"examples/e.rs", LangRust, Included},
		// Normal source still included.
		{"main.go", LangGo, Included},
		{"build.rs", LangRust, Included},
		// Vendored / build-output / minified — ExcludedTree (was a silent
		// drop): still attacker-authored in this PR, scanned at warn tier.
		{"vendor/x/y.go", LangGo, ExcludedTree},
		{"pkg/vendor/dep.go", LangGo, ExcludedTree},
		{"node_modules/dep/index.js", LangJavaScript, ExcludedTree},
		{"dist/bundle.js", LangJavaScript, ExcludedTree},
		{".venv/lib/x.py", LangPython, ExcludedTree},
		{"a/site-packages/p/x.py", LangPython, ExcludedTree},
		{"target/debug/x.rs", LangRust, ExcludedTree},
		{"bundle.min.js", LangJavaScript, ExcludedTree},
		{"web/app.min.js", LangJavaScript, ExcludedTree},
		// Type-only declarations and non-source — NotSource (no runtime AST).
		{"types.d.ts", "", NotSource},
		{"README.md", "", NotSource},
		{"go.mod", "", NotSource},
	}

	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()
			lang, incl := LanguageForChangedFile(tc.path)
			assert.Equal(t, tc.wantIncl, incl, "inclusion")
			assert.Equal(t, tc.wantLang, lang, "language")
		})
	}
}
