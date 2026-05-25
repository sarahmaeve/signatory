package repofiles

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/agentconfig"
)

// TestScan_AlignsWithIsConfigPath is the cross-validation between
// the two surfaces that share agentconfig.Loci as their source of
// truth: the per-clone scanner that walks the filesystem and the
// single-path classifier agentconfig.IsConfigPath. A synthetic
// clone is populated with one file per Locus, Scan() is run
// against it, and IsConfigPath must agree on every Match.Path.
// Drift between the two surfaces would split the source-of-truth
// and is the failure mode this test exists to catch — for example,
// a future refactor that special-cases a path in one surface but
// not the other.
//
// The fixture set deliberately exercises one file per Locus rather
// than a hand-curated subset, so the test fails loudly if any new
// Locus is added without keeping the two surfaces in sync.
//
// The per-path positive test (every known path classifies as
// agent-config) lives in agentconfig/locus_test.go as
// TestIsConfigPath_KnownFiles; this test is strictly the cross-
// package alignment pin.
func TestScan_AlignsWithIsConfigPath(t *testing.T) {
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
			assert.True(t, agentconfig.IsConfigPath(m.Path),
				"Scan emitted path %q (family %q); agentconfig.IsConfigPath must agree — "+
					"drift means the two surfaces no longer share a source of truth",
				m.Path, m.Family)
		})
	}
}
