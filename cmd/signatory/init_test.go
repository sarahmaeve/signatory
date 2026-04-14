package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sarahmaeve/signatory/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The CLI-layer tests for `signatory init` focus on the thin wrapper
// around config.InitProject: flag plumbing, exit behavior, and the
// real embedded template set. Deep scenarios (idempotency nuances,
// partial upgrades, error shapes) live in internal/config.

func TestInit_ScaffoldsProjectFromRealEmbedded(t *testing.T) {
	// End-to-end: use the actual signatory.EmbeddedTemplates through
	// Run() to prove the embed directive at module root is wired up
	// correctly and that the wrapper command propagates options to
	// config.InitProject.
	dir := t.TempDir()
	cmd := &InitCmd{Dir: dir, Quiet: true}
	require.NoError(t, cmd.Run(&Globals{}))

	// Real shipped templates should be on disk.
	for _, rel := range []string{
		"templates/handoffs/README.md",
		"templates/handoffs/security-review-v1.md",
		"templates/handoffs/security-review-go-v1.md",
		"templates/handoffs/provenance-review-v1.md",
	} {
		_, err := os.Stat(filepath.Join(dir, rel))
		assert.NoError(t, err, "expected %s on disk after init", rel)
	}

	// Scaffolded config should be present and parseable.
	cfg, err := config.DiscoverAndLoad(dir)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, config.ConfigFileName), cfg.Source)
}

func TestInit_SecondRunSkipsUnlessForced(t *testing.T) {
	dir := t.TempDir()

	// First run seeds everything.
	require.NoError(t, (&InitCmd{Dir: dir, Quiet: true}).Run(&Globals{}))

	// User customizes a template.
	customized := filepath.Join(dir, "templates", "handoffs", "README.md")
	require.NoError(t, os.WriteFile(customized, []byte("my edits"), 0o644))

	// Second run without force should leave the edits in place.
	require.NoError(t, (&InitCmd{Dir: dir, Quiet: true}).Run(&Globals{}))
	got, err := os.ReadFile(customized)
	require.NoError(t, err)
	assert.Equal(t, "my edits", string(got),
		"--force not passed; user edits must be preserved")

	// With --force, the embedded version overwrites the customization.
	require.NoError(t, (&InitCmd{Dir: dir, Quiet: true, Force: true}).Run(&Globals{}))
	got, err = os.ReadFile(customized)
	require.NoError(t, err)
	assert.NotEqual(t, "my edits", string(got),
		"--force should overwrite the customized template")
}

func TestInit_DirFlagParsesAsPositional(t *testing.T) {
	// Regression guard: `signatory init .` was a UX trap in an
	// earlier iteration of this command because Dir was a flag and
	// kong rejected the positional. After the fix, `.` parses as the
	// Dir positional. kong's type:"path" expands relative paths to
	// absolute at parse time, so we assert it's absolute and non-empty
	// rather than literal "." — the absolute-path round-trip is what
	// InitProject expects downstream.
	ctx, cli := parseCLI(t, "init", ".")
	require.NoError(t, ctx.Error)
	assert.True(t, filepath.IsAbs(cli.Init.Dir), "Dir=%q should be absolute", cli.Init.Dir)
}

func TestInit_NoArgsDefaultsToCWD(t *testing.T) {
	// With no positional, kong applies the default `"."` and then
	// the type:"path" expander promotes it to an absolute path.
	ctx, cli := parseCLI(t, "init")
	require.NoError(t, ctx.Error)
	assert.True(t, filepath.IsAbs(cli.Init.Dir), "Dir=%q should be absolute", cli.Init.Dir)
}

func TestInit_PositionalPathIsAccepted(t *testing.T) {
	ctx, cli := parseCLI(t, "init", "/tmp/myproj")
	require.NoError(t, ctx.Error)
	assert.Equal(t, "/tmp/myproj", cli.Init.Dir)
}
