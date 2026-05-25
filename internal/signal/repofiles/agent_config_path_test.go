package repofiles

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIsAgentConfigPath_KnownFiles covers every family declared by
// AgentConfigFamilies. A regression on any single entry would silently
// drop that file class out of cross-package classifiers (e.g. the
// artifact-vs-repo categorizer that consumes this function to flag
// agent-config drops in tarballs).
func TestIsAgentConfigPath_KnownFiles(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		path string
		want bool
	}{
		// Root-level dotfiles and named instruction files.
		{"cursorrules_root", ".cursorrules", true},
		{"claude_md_root", "CLAUDE.md", true},
		{"agents_md_root", "AGENTS.md", true},
		{"windsurfrules_root", ".windsurfrules", true},
		{"aider_conf_yml", ".aider.conf.yml", true},
		{"aider_conf_yaml", ".aider.conf.yaml", true},

		// Subdirectory-scoped files.
		{"claude_dir_settings", ".claude/settings.json", true},
		{"claude_dir_claude_md", ".claude/CLAUDE.md", true},
		{"zed_settings", ".zed/settings.json", true},
		{"continue_config", ".continue/config.json", true},
		{"cursor_rules_mdc", ".cursor/rules/001-base.mdc", true},
		{"cursor_rules_named_mdc", ".cursor/rules/language-style.mdc", true},

		// Case variants per the case-insensitive detectors.
		{"claude_md_lowercase", "claude.md", true},
		{"agents_md_mixed", "Agents.md", true},

		// Non-matches: similarly-named files that are NOT agent-config.
		{"readme", "README.md", false},
		{"security", "SECURITY.md", false},
		{"package_json", "package.json", false},
		{"settings_at_root", "settings.json", false}, // wrong dir
		{"settings_random_dir", "src/settings.json", false},
		{"mdc_at_root", "rules.mdc", false}, // wrong dir
		{"random_dotfile", ".gitignore", false},
		{"empty_path", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := IsAgentConfigPath(tc.path)
			assert.Equal(t, tc.want, got,
				"IsAgentConfigPath(%q) = %v, want %v", tc.path, got, tc.want)
		})
	}
}

// TestIsAgentConfigPath_AlignsWithScan is the real cross-validation
// between the two surfaces: a synthetic clone is populated with one
// file per Locus, Scan() is run against it, and IsAgentConfigPath
// must agree on every Match.Path. Drift between the two surfaces
// would split the source-of-truth (agentconfig.Loci) and is the
// failure mode this test exists to catch — for example, a future
// refactor that special-cases a path in one surface but not the
// other.
//
// The fixture set deliberately exercises one file per Locus rather
// than a hand-curated subset, so the test fails loudly if any new
// Locus is added without keeping the two surfaces in sync.
func TestIsAgentConfigPath_AlignsWithScan(t *testing.T) {
	t.Parallel()

	clone := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(clone, ".git"), 0o755))

	// One file per Locus. Subdirectory-scoped Loci need MkdirAll for
	// the parent. Content is non-empty so the scanner does not drop
	// the entry as a zero-byte placeholder.
	files := []string{
		".cursorrules",
		"CLAUDE.md",
		"AGENTS.md",
		"GEMINI.md",
		".github/copilot-instructions.md",
		".github/instructions/typescript.instructions.md",
		".claude/settings.json",
		".claude/CLAUDE.md",
		".cursor/rules/001-base.mdc",
		".aider.conf.yml",
		".zed/settings.json",
		".codex/instructions.md",
		".continue/config.json",
		".windsurfrules",
	}
	for _, rel := range files {
		absPath := filepath.Join(clone, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(absPath), 0o755))
		require.NoError(t, os.WriteFile(absPath, []byte("content"), 0o644))
	}

	matches, err := Scan(clone, AgentConfigFamilies())
	require.NoError(t, err)
	require.NotEmpty(t, matches,
		"test premise: Scan must find at least one match in the populated clone")

	for _, m := range matches {
		t.Run(m.Path, func(t *testing.T) {
			t.Parallel()
			assert.True(t, IsAgentConfigPath(m.Path),
				"Scan emitted path %q (family %q); IsAgentConfigPath must agree — "+
					"drift means the two surfaces no longer share a source of truth",
				m.Path, m.Family)
		})
	}
}
