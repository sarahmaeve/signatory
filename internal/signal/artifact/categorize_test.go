package artifact

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestClassify_AgentConfig covers the new agent_config bucket — a
// tarball entry whose path identifies an AI-agent configuration
// file (.cursorrules, CLAUDE.md, AGENTS.md, .claude/settings.json,
// etc.). The motivating shape is the xz-precedent applied to
// AI-instruction injection: a file shipping in the tarball but
// absent from git at that commit is the dropped-in-tarball
// agent-config payload. Family detectors are the single source of
// truth — sourced from repofiles.IsAgentConfigPath.
func TestClassify_AgentConfig(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		path string
	}{
		{"cursorrules_root", ".cursorrules"},
		{"claude_md_root", "CLAUDE.md"},
		{"agents_md_root", "AGENTS.md"},
		{"claude_dir_settings", ".claude/settings.json"},
		{"claude_dir_claude_md", ".claude/CLAUDE.md"},
		{"cursor_rules_mdc", ".cursor/rules/001-base.mdc"},
		{"aider_conf_yml", ".aider.conf.yml"},
		{"zed_settings", ".zed/settings.json"},
		{"continue_config", ".continue/config.json"},
		{"windsurfrules", ".windsurfrules"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := classify(tc.path)
			assert.Equal(t, CategoryAgentConfig, got,
				"classify(%q) must return CategoryAgentConfig", tc.path)
		})
	}
}

// TestClassify_AgentConfigOrderingOverBuildGlue confirms agent_config
// fires before build_glue when both could match. Today no agent-
// config path is also a build_glue basename, so the ordering is
// belt-and-braces — but if a future agent-config family ever
// declares e.g. `.aider/setup.py`, ordering must not silently
// reclassify it as build_glue.
func TestClassify_AgentConfigOrderingOverBuildGlue(t *testing.T) {
	t.Parallel()

	// Construct a path that BOTH a hypothetical agent-config family
	// and the build_glue basename catalog would match. We use the
	// real agent-config path (.aider.conf.yml) and verify the
	// classifier reports agent_config rather than letting it fall
	// through to other / build_glue. (.aider.conf.yml does not
	// presently match any build_glue rule; this is the future-proof
	// assertion.)
	got := classify(".aider.conf.yml")
	assert.Equal(t, CategoryAgentConfig, got,
		"agent_config must win the classification race over future "+
			"build_glue collisions")
}

// TestClassify_AgentConfigNotConfusedWithHygieneFiles ensures the
// hygiene-file shapes (README, SECURITY, CONTRIBUTING, …) that
// repofiles' Families() declares do NOT classify as agent_config.
// The categorizer must distinguish "AI-agent instruction file" from
// "project-hygiene file".
func TestClassify_AgentConfigNotConfusedWithHygieneFiles(t *testing.T) {
	t.Parallel()

	cases := []string{
		"README.md",
		"SECURITY.md",
		"CONTRIBUTING.md",
		"CODEOWNERS",
		".mailmap",
		"CHANGELOG.md",
		"package.json",
	}
	for _, p := range cases {
		t.Run(p, func(t *testing.T) {
			t.Parallel()
			got := classify(p)
			assert.NotEqual(t, CategoryAgentConfig, got,
				"hygiene file %q must not classify as agent_config", p)
		})
	}
}

// TestClassify_AgentConfigOverridesOther confirms that a path that
// would otherwise fall into the "other" bucket gets the more-
// specific agent_config bucket instead. Before this work,
// `.cursorrules` landed in "other"; the new category makes it
// explicit.
func TestClassify_AgentConfigOverridesOther(t *testing.T) {
	t.Parallel()

	// Sanity: a random non-agent-config dotfile DOES fall to "other".
	assert.Equal(t, CategoryOther, classify(".gitignore"),
		"non-agent-config dotfile baseline")

	// The new bucket: agent-config dotfile gets the specific bucket.
	assert.Equal(t, CategoryAgentConfig, classify(".cursorrules"),
		"agent-config dotfile must classify as agent_config, not other")
}
