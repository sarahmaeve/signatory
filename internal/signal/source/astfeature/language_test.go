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
