package repofiles

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAgentConfigFamilies_Declaration locks in the declared family
// list. Adding or removing a family is a deliberate act — the test
// failing is the reminder to update the agent_config_files signal's
// documented coverage in types.go and the design entry in the same
// commit.
//
// The list intentionally diverges from Families() (hygiene) because
// the threat model is different: these files are AI-agent
// instruction surfaces, and the Trapdoor (2026-05) campaign weaponized
// .cursorrules and CLAUDE.md as carriers for zero-width-Unicode
// prompt-injection payloads. The corpus targets known carrier shapes
// across the AI-coding-agent landscape.
func TestAgentConfigFamilies_Declaration(t *testing.T) {
	t.Parallel()

	fams := AgentConfigFamilies()
	names := make([]string, 0, len(fams))
	for _, f := range fams {
		names = append(names, f.Name)
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
	assert.Equal(t, want, names,
		"agent-config family list and order must stay stable — compound signal value iteration depends on it")
}

// TestAgentConfigFamilies_AllHaveDetectorAndDirs enforces that each
// family is fully declared. A nil detector or empty Dirs would
// silently disable the family at runtime. Preferred can be empty for
// families where multiple matched files are all of-interest (e.g.
// .cursor/rules/*.mdc has no single canonical filename); we enforce
// only Detector + Dirs here.
func TestAgentConfigFamilies_AllHaveDetectorAndDirs(t *testing.T) {
	t.Parallel()

	for _, f := range AgentConfigFamilies() {
		t.Run(f.Name, func(t *testing.T) {
			t.Parallel()
			require.NotNil(t, f.Detector, "family %q missing Detector", f.Name)
			require.NotEmpty(t, f.Dirs, "family %q missing Dirs", f.Name)
		})
	}
}

// TestAgentConfigFamilies_Immutable verifies AgentConfigFamilies()
// returns a fresh copy on each call — mutating the returned slice must
// not leak into the package-level declaration.
func TestAgentConfigFamilies_Immutable(t *testing.T) {
	t.Parallel()

	first := AgentConfigFamilies()
	first[0].Name = "MUTATED"

	second := AgentConfigFamilies()
	assert.Equal(t, "cursorrules", second[0].Name,
		"AgentConfigFamilies() must return a fresh slice on each call")
}

// TestAgentConfigFamilies_Disjoint guards against accidental overlap
// with the hygiene Families() list. The two surfaces emit distinct
// signal types and should never share a family name; a collision
// would create ambiguity in signal-value maps.
func TestAgentConfigFamilies_Disjoint(t *testing.T) {
	t.Parallel()

	hygiene := make(map[string]struct{})
	for _, f := range Families() {
		hygiene[f.Name] = struct{}{}
	}
	for _, f := range AgentConfigFamilies() {
		_, collides := hygiene[f.Name]
		assert.False(t, collides,
			"agent-config family %q collides with hygiene family of the same name", f.Name)
	}
}

// TestAgentConfigFamilies_Scan_Trapdoor models the Trapdoor 2026-05
// campaign IOC shape: a repo containing .cursorrules and CLAUDE.md
// at root. Both must be detected by the agent-config scanner; the
// existing hygiene scanner must NOT pick them up (they belong to a
// separate signal surface).
func TestAgentConfigFamilies_Scan_Trapdoor(t *testing.T) {
	t.Parallel()

	clone := t.TempDir()
	// Minimal git-marker so validateClone passes (the scanner requires
	// the directory looks like a working tree).
	require.NoError(t, os.Mkdir(filepath.Join(clone, ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(clone, ".cursorrules"), []byte("# rules"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(clone, "CLAUDE.md"), []byte("# claude"), 0o644))

	matches, err := Scan(clone, AgentConfigFamilies())
	require.NoError(t, err)

	families := make(map[string]string, len(matches))
	for _, m := range matches {
		families[m.Family] = m.Path
	}
	assert.Equal(t, ".cursorrules", families["cursorrules"],
		"Trapdoor's .cursorrules carrier should be detected by the cursorrules family")
	assert.Equal(t, "CLAUDE.md", families["claude_md"],
		"Trapdoor's CLAUDE.md carrier should be detected by the claude_md family")

	// Negative: the hygiene Families() scan must not pick up these files.
	hygieneMatches, err := Scan(clone, Families())
	require.NoError(t, err)
	for _, m := range hygieneMatches {
		assert.NotEqual(t, ".cursorrules", m.Path,
			"hygiene scan must not detect .cursorrules — it's an agent-config surface, not a hygiene surface")
		assert.NotEqual(t, "CLAUDE.md", m.Path,
			"hygiene scan must not detect CLAUDE.md — it's an agent-config surface, not a hygiene surface")
	}
}

// TestAgentConfigFamilies_Scan_Subdirs covers files inside .claude/
// and .cursor/rules/ — the subdirectory-scoped families that need the
// scanner to walk dotted paths.
func TestAgentConfigFamilies_Scan_Subdirs(t *testing.T) {
	t.Parallel()

	clone := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(clone, ".git"), 0o755))
	require.NoError(t, os.Mkdir(filepath.Join(clone, ".claude"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(clone, ".cursor", "rules"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(clone, ".claude", "settings.json"), []byte(`{}`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(clone, ".claude", "CLAUDE.md"), []byte("# instructions"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(clone, ".cursor", "rules", "001-base.mdc"), []byte("# rules"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(clone, ".cursor", "rules", "002-style.mdc"), []byte("# style"), 0o644))

	matches, err := Scan(clone, AgentConfigFamilies())
	require.NoError(t, err)

	byFamily := make(map[string][]string)
	for _, m := range matches {
		byFamily[m.Family] = append(byFamily[m.Family], m.Path)
	}
	assert.ElementsMatch(t, []string{".claude/settings.json"}, byFamily["claude_dir_settings"])
	assert.ElementsMatch(t, []string{".claude/CLAUDE.md"}, byFamily["claude_dir_claude_md"])
	assert.ElementsMatch(t,
		[]string{".cursor/rules/001-base.mdc", ".cursor/rules/002-style.mdc"},
		byFamily["cursor_rules_dir"],
		"all .mdc files under .cursor/rules/ should be detected (no single canonical name)")
}
