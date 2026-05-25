package agentconfig

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLoci_Declaration locks in the canonical locus list and order.
// Adding or removing a Locus is a deliberate act — the failing test
// is the reminder to update downstream test expectations (repofiles
// AgentConfigFamilies test, astfeature catalog test, threat-
// landscape entry).
func TestLoci_Declaration(t *testing.T) {
	t.Parallel()

	got := make([]string, 0, len(Loci()))
	for _, l := range Loci() {
		got = append(got, l.Name)
	}

	want := []string{
		"cursorrules",
		"claude_md",
		"agents_md",
		"gemini_md",
		"copilot_repo_instructions",
		"copilot_path_instructions",
		"claude_dir_settings",
		"claude_dir_claude_md",
		"cursor_rules_dir",
		"aider_conf",
		"zed_settings",
		"codex_instructions",
		"continue_config",
		"windsurfrules",
	}
	assert.Equal(t, want, got)
}

// TestLoci_AllFieldsDeclared catches a regression where a Locus
// landed with a nil Detector, empty Dirs, or empty
// RuntimePathPrefixes — any of which would silently disable that
// Locus at one or both consumer sites.
func TestLoci_AllFieldsDeclared(t *testing.T) {
	t.Parallel()

	for _, l := range Loci() {
		t.Run(l.Name, func(t *testing.T) {
			t.Parallel()
			require.NotNil(t, l.Detector, "Locus %q missing Detector", l.Name)
			require.NotEmpty(t, l.Dirs, "Locus %q missing Dirs", l.Name)
			require.NotEmpty(t, l.RuntimePathPrefixes,
				"Locus %q missing RuntimePathPrefixes — would be invisible to "+
					"the source-AST persistence-write catalog", l.Name)
		})
	}
}

// TestLoci_RuntimePrefixesAreAnchored locks in the "/" prefix
// convention. A prefix like ".codex" without the leading slash
// would substring-match inside an unrelated path like
// "test/foo.codex.txt"; the leading "/" anchors the match at a
// path boundary.
func TestLoci_RuntimePrefixesAreAnchored(t *testing.T) {
	t.Parallel()

	for _, l := range Loci() {
		for _, prefix := range l.RuntimePathPrefixes {
			t.Run(l.Name+"_"+prefix, func(t *testing.T) {
				t.Parallel()
				assert.True(t, strings.HasPrefix(prefix, "/"),
					"Locus %q RuntimePathPrefix %q must start with '/' to "+
						"anchor at a path boundary", l.Name, prefix)
			})
		}
	}
}

// TestLoci_Immutable verifies Loci() returns a fresh copy on each
// call — mutating the returned slice must not leak into the
// package-level declaration.
func TestLoci_Immutable(t *testing.T) {
	t.Parallel()

	first := Loci()
	first[0].Name = "MUTATED"

	second := Loci()
	assert.Equal(t, "cursorrules", second[0].Name)
}

// TestLoci_Immutable_SliceFields covers what TestLoci_Immutable does
// not: Name is a string (value-typed), so a shallow copy is safe for
// it; Dirs / Preferred / RuntimePathPrefixes are slice headers whose
// backing arrays the original implementation aliased to the package-
// level singletons. A caller mutating loci[0].Dirs[0] silently
// polluted every future caller. The doc on Loci() claims the
// declaration cannot be mutated by callers — this test pins that
// claim against the slice fields specifically.
func TestLoci_Immutable_SliceFields(t *testing.T) {
	t.Parallel()

	first := Loci()
	require.NotEmpty(t, first[0].Dirs)
	require.NotEmpty(t, first[0].RuntimePathPrefixes)
	require.NotEmpty(t, first[0].Preferred,
		"test premise: cursorrules locus declares Preferred")

	originalDir := first[0].Dirs[0]
	originalPrefix := first[0].RuntimePathPrefixes[0]
	originalPreferred := first[0].Preferred[0]

	first[0].Dirs[0] = "MUTATED_DIR"
	first[0].RuntimePathPrefixes[0] = "MUTATED_PREFIX"
	first[0].Preferred[0] = "MUTATED_PREFERRED"

	second := Loci()
	assert.Equal(t, originalDir, second[0].Dirs[0],
		"Dirs slice header aliased the package singleton; mutation leaked across Loci() calls")
	assert.Equal(t, originalPrefix, second[0].RuntimePathPrefixes[0],
		"RuntimePathPrefixes slice header aliased the package singleton; mutation leaked")
	assert.Equal(t, originalPreferred, second[0].Preferred[0],
		"Preferred slice header aliased the package singleton; mutation leaked")
}

// TestIsConfigPath_KnownFiles covers every Locus's detector. A
// regression on any single entry would silently drop that file
// class from cross-package classifiers.
func TestIsConfigPath_KnownFiles(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		path string
		want bool
	}{
		{"cursorrules_root", ".cursorrules", true},
		{"claude_md_root", "CLAUDE.md", true},
		{"agents_md_root", "AGENTS.md", true},
		{"gemini_md_root", "GEMINI.md", true},
		{"copilot_repo_instructions", ".github/copilot-instructions.md", true},
		{"copilot_path_instructions_one", ".github/instructions/typescript.instructions.md", true},
		{"copilot_path_instructions_two", ".github/instructions/api-style.instructions.md", true},
		{"windsurfrules_root", ".windsurfrules", true},
		{"aider_conf_yml", ".aider.conf.yml", true},
		{"aider_conf_yaml", ".aider.conf.yaml", true},
		{"claude_dir_settings", ".claude/settings.json", true},
		{"claude_dir_claude_md", ".claude/CLAUDE.md", true},
		{"zed_settings", ".zed/settings.json", true},
		{"codex_instructions", ".codex/instructions.md", true},
		{"codex_config_json", ".codex/config.json", true},
		{"codex_config_toml", ".codex/config.toml", true},
		{"continue_config", ".continue/config.json", true},
		{"cursor_rules_mdc", ".cursor/rules/001-base.mdc", true},
		{"cursor_rules_named", ".cursor/rules/language-style.mdc", true},

		// Case variants per case-insensitive detectors.
		{"claude_md_lowercase", "claude.md", true},
		{"agents_md_mixed", "Agents.md", true},

		// Non-matches.
		{"readme", "README.md", false},
		{"security", "SECURITY.md", false},
		{"package_json", "package.json", false},
		{"settings_at_root", "settings.json", false},
		{"settings_random_dir", "src/settings.json", false},
		{"mdc_at_root", "rules.mdc", false},
		{"random_dotfile", ".gitignore", false},
		{"copilot_instructions_at_root", "copilot-instructions.md", false},
		{"copilot_instructions_random_dir", "docs/copilot-instructions.md", false},
		{"non_instructions_md_in_instructions_dir", ".github/instructions/README.md", false},
		{"empty_path", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, IsConfigPath(tc.path),
				"IsConfigPath(%q)", tc.path)
		})
	}
}

// TestRuntimePersistencePrefixes_Membership locks in the union
// the AST persistence-write catalog appends. Drift between this
// list and astfeature.PersistencePathPatterns is caught by a
// companion test in astfeature; this test is the within-package
// pin.
func TestRuntimePersistencePrefixes_Membership(t *testing.T) {
	t.Parallel()

	got := RuntimePersistencePrefixes()
	want := []string{
		"/.cursorrules",
		"/CLAUDE.md",
		"/AGENTS.md",
		"/GEMINI.md",
		"/.github/copilot-instructions.md",
		"/.github/instructions/",
		"/.claude/",
		"/.cursor/",
		"/.aider/",
		"/.zed/",
		"/.codex/",
		"/.continue/",
		"/.windsurfrules",
		"/.windsurf/",
	}
	for _, w := range want {
		assert.True(t, slices.Contains(got, w),
			"runtime prefix %q missing from RuntimePersistencePrefixes()", w)
	}
}

// TestRuntimePersistencePrefixes_NoDuplicates confirms the dedupe
// guarantee. The .claude/ prefix appears on two Loci
// (claude_dir_settings and claude_dir_claude_md); the union must
// list it once.
func TestRuntimePersistencePrefixes_NoDuplicates(t *testing.T) {
	t.Parallel()

	got := RuntimePersistencePrefixes()
	seen := make(map[string]int)
	for _, p := range got {
		seen[p]++
	}
	for prefix, count := range seen {
		assert.Equal(t, 1, count,
			"prefix %q appears %d times — dedupe broke", prefix, count)
	}
}

// TestRuntimePersistencePrefixes_CoversAllLoci is the cross-
// validation that motivated this package: every Locus MUST
// contribute at least one prefix to the union. The original bug
// — /.codex/ in astfeature's catalog with no corresponding Family
// in repofiles — becomes structurally impossible because every
// Locus is required to declare RuntimePathPrefixes (enforced by
// TestLoci_AllFieldsDeclared).
func TestRuntimePersistencePrefixes_CoversAllLoci(t *testing.T) {
	t.Parallel()

	prefixes := RuntimePersistencePrefixes()
	for _, l := range Loci() {
		t.Run(l.Name, func(t *testing.T) {
			t.Parallel()
			covered := false
			for _, lp := range l.RuntimePathPrefixes {
				if slices.Contains(prefixes, lp) {
					covered = true
					break
				}
			}
			assert.True(t, covered,
				"Locus %q has no prefix in the union — drift introduced", l.Name)
		})
	}
}
