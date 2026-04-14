package config

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeEmbed returns a fstest.MapFS mimicking the module-root embed.FS
// shape: every entry is under a top-level "templates/" prefix.
func fakeEmbed() fstest.MapFS {
	return fstest.MapFS{
		"templates/handoffs/alpha.md":        &fstest.MapFile{Data: []byte("alpha content")},
		"templates/handoffs/beta.md":         &fstest.MapFile{Data: []byte("beta content")},
		"templates/handoffs/subdir/gamma.md": &fstest.MapFile{Data: []byte("gamma content")},
	}
}

func TestInitProject_FreshDirectory(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	result, err := InitProject(InitOptions{
		Dir:            dir,
		EmbeddedFS:     fakeEmbed(),
		EmbeddedPrefix: "templates",
		Out:            &buf,
	})
	require.NoError(t, err)
	assert.Equal(t, 3, result.TemplatesCopied)
	assert.Equal(t, 0, result.TemplatesSkipped)
	assert.True(t, result.ConfigWritten)

	// All three files should be on disk with matching contents.
	for name, want := range map[string]string{
		"templates/handoffs/alpha.md":        "alpha content",
		"templates/handoffs/beta.md":         "beta content",
		"templates/handoffs/subdir/gamma.md": "gamma content",
	} {
		got, err := os.ReadFile(filepath.Join(dir, name))
		require.NoError(t, err, "missing %s", name)
		assert.Equal(t, want, string(got))
	}

	// filestore/analysis should have been created.
	info, err := os.Stat(filepath.Join(dir, "filestore", "analysis"))
	require.NoError(t, err)
	assert.True(t, info.IsDir())

	// Config scaffold should have been written with the documented keys.
	cfg, err := os.ReadFile(filepath.Join(dir, ConfigFileName))
	require.NoError(t, err)
	assert.Contains(t, string(cfg), "templates")
	assert.Contains(t, string(cfg), "filestores")
}

func TestInitProject_IdempotentWithoutForce(t *testing.T) {
	// Running init twice without --force should preserve the first
	// run's output: templates and config untouched, counts reflect
	// skip behavior.
	dir := t.TempDir()
	_, err := InitProject(InitOptions{
		Dir: dir, EmbeddedFS: fakeEmbed(), EmbeddedPrefix: "templates",
	})
	require.NoError(t, err)

	// Second run modifies nothing.
	result, err := InitProject(InitOptions{
		Dir: dir, EmbeddedFS: fakeEmbed(), EmbeddedPrefix: "templates",
	})
	require.NoError(t, err)
	assert.Equal(t, 0, result.TemplatesCopied)
	assert.Equal(t, 3, result.TemplatesSkipped)
	assert.False(t, result.ConfigWritten, "config already present; should skip")
}

func TestInitProject_ForceOverwrites(t *testing.T) {
	dir := t.TempDir()
	// Pre-plant a template with customized content that force should
	// replace.
	alpha := filepath.Join(dir, "templates", "handoffs", "alpha.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(alpha), 0o755))
	require.NoError(t, os.WriteFile(alpha, []byte("user customization"), 0o644))

	result, err := InitProject(InitOptions{
		Dir: dir, EmbeddedFS: fakeEmbed(), EmbeddedPrefix: "templates", Force: true,
	})
	require.NoError(t, err)
	assert.Equal(t, 3, result.TemplatesCopied)
	assert.Equal(t, 0, result.TemplatesSkipped)

	got, err := os.ReadFile(alpha)
	require.NoError(t, err)
	assert.Equal(t, "alpha content", string(got))
}

func TestInitProject_NewTemplatesFillGaps(t *testing.T) {
	// Realistic upgrade scenario: user has run `init` before, then
	// upgrades signatory which ships one new template. Re-running
	// init (no --force) should pick up ONLY the new file.
	dir := t.TempDir()

	priorEmbed := fstest.MapFS{
		"templates/handoffs/alpha.md": &fstest.MapFile{Data: []byte("alpha")},
	}
	_, err := InitProject(InitOptions{Dir: dir, EmbeddedFS: priorEmbed, EmbeddedPrefix: "templates"})
	require.NoError(t, err)

	newerEmbed := fstest.MapFS{
		"templates/handoffs/alpha.md": &fstest.MapFile{Data: []byte("alpha")},
		"templates/handoffs/new.md":   &fstest.MapFile{Data: []byte("new")},
	}
	result, err := InitProject(InitOptions{Dir: dir, EmbeddedFS: newerEmbed, EmbeddedPrefix: "templates"})
	require.NoError(t, err)
	assert.Equal(t, 1, result.TemplatesCopied)
	assert.Equal(t, 1, result.TemplatesSkipped)
}

func TestInitProject_RejectsNilEmbed(t *testing.T) {
	_, err := InitProject(InitOptions{Dir: t.TempDir()})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "EmbeddedFS")
}

func TestInitProject_QuietWriter(t *testing.T) {
	// Verify that Out=nil is the documented no-op path and doesn't panic.
	dir := t.TempDir()
	_, err := InitProject(InitOptions{
		Dir: dir, EmbeddedFS: fakeEmbed(), EmbeddedPrefix: "templates", Out: nil,
	})
	require.NoError(t, err)
}

// TestInitProject_DiscardWriter documents that io.Discard is a valid
// sink when the caller wants to suppress output without passing nil.
// (Some callers prefer non-nil io.Writer for uniformity.)
func TestInitProject_DiscardWriter(t *testing.T) {
	dir := t.TempDir()
	_, err := InitProject(InitOptions{
		Dir: dir, EmbeddedFS: fakeEmbed(), EmbeddedPrefix: "templates", Out: io.Discard,
	})
	require.NoError(t, err)
}
