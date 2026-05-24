package repofiles

import (
	"testing"

	"github.com/stretchr/testify/assert"
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

// TestIsAgentConfigPath_AlignsWithScan is a meta-test: every file
// the per-clone Scan() finds via AgentConfigFamilies must also be
// classified true by IsAgentConfigPath. Drift between the two
// surfaces would split the source-of-truth.
func TestIsAgentConfigPath_AlignsWithScan(t *testing.T) {
	t.Parallel()

	// Sample paths the Scan-based tests use; check every one against
	// the single-path classifier.
	samplePaths := []string{
		".cursorrules",
		"CLAUDE.md",
		"AGENTS.md",
		".claude/settings.json",
		".claude/CLAUDE.md",
		".cursor/rules/001-base.mdc",
		".aider.conf.yml",
		".zed/settings.json",
		".continue/config.json",
		".windsurfrules",
	}
	for _, p := range samplePaths {
		t.Run(p, func(t *testing.T) {
			t.Parallel()
			assert.True(t, IsAgentConfigPath(p),
				"path %q is detected by Scan; IsAgentConfigPath must agree", p)
		})
	}
}
