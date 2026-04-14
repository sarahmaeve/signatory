package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadConfig_BasicFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "signatory.config.toml")
	body := `templates = ["/tmp/a", "/tmp/b"]
filestores = ["/tmp/out"]
`
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))

	cfg, err := LoadConfig(path)
	require.NoError(t, err)
	assert.Equal(t, []string{"/tmp/a", "/tmp/b"}, cfg.Templates)
	assert.Equal(t, []string{"/tmp/out"}, cfg.Filestores)
	// Source is normalized to an absolute path for diagnostics.
	assert.True(t, filepath.IsAbs(cfg.Source))
}

func TestLoadConfig_UnknownKeyRejected(t *testing.T) {
	// Unknown keys must error, not be silently ignored: a typo in a
	// user's config (e.g., "template" instead of "templates") should
	// surface loudly rather than drop through to defaults.
	dir := t.TempDir()
	path := filepath.Join(dir, "signatory.config.toml")
	require.NoError(t, os.WriteFile(path, []byte("template = [\"/a\"]\n"), 0o644))

	_, err := LoadConfig(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown key \"template\"")
	assert.Contains(t, err.Error(), "line 1")
}

func TestLoadConfig_WrongValueShape(t *testing.T) {
	// `templates = "foo"` as a scalar should be rejected — the
	// schema requires an array even for single-element lists.
	dir := t.TempDir()
	path := filepath.Join(dir, "signatory.config.toml")
	require.NoError(t, os.WriteFile(path, []byte("templates = \"one\"\n"), 0o644))

	_, err := LoadConfig(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be an array of strings")
}

func TestLoadConfig_MissingFile(t *testing.T) {
	_, err := LoadConfig(filepath.Join(t.TempDir(), "nonexistent.toml"))
	require.Error(t, err)
	// Either a plain fs.ErrNotExist wrap or an os.PathError — both
	// surface as a "no such file" style message.
	assert.True(t, os.IsNotExist(err), "got %v", err)
}

func TestDiscoverAndLoad_Absent(t *testing.T) {
	cfg, err := DiscoverAndLoad(t.TempDir())
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Empty(t, cfg.Source)
	assert.Empty(t, cfg.Templates)
	assert.Empty(t, cfg.Filestores)
}

func TestConfigScaffold_UncommentedParses(t *testing.T) {
	// Self-consistency: the scaffold signatory writes must itself be
	// a valid signatory config when the user uncomments the
	// placeholder keys. Without this test, changes to the scaffold
	// copy could drift from what the parser accepts.
	uncommented := `templates = [
    "/Users/you/shared-signatory/templates",
]
filestores = [
    "/Users/you/signatory-artifacts",
]
`
	dir := t.TempDir()
	path := filepath.Join(dir, ConfigFileName)
	require.NoError(t, os.WriteFile(path, []byte(uncommented), 0o644))

	cfg, err := LoadConfig(path)
	require.NoError(t, err)
	assert.Equal(t, []string{"/Users/you/shared-signatory/templates"}, cfg.Templates)
	assert.Equal(t, []string{"/Users/you/signatory-artifacts"}, cfg.Filestores)
}

// TestConfigScaffold_CommentedFormParses confirms the *default*
// scaffold — with every key commented out — loads as an empty config
// rather than errors. Users who run `signatory init` without ever
// editing the result must get a clean zero-override load.
func TestConfigScaffold_CommentedFormParses(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ConfigFileName)
	require.NoError(t, os.WriteFile(path, []byte(ConfigScaffold), 0o644))

	cfg, err := LoadConfig(path)
	require.NoError(t, err)
	assert.Empty(t, cfg.Templates)
	assert.Empty(t, cfg.Filestores)
}

func TestDiscoverAndLoad_Present(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ConfigFileName)
	require.NoError(t, os.WriteFile(path, []byte("templates = [\"/x\"]\n"), 0o644))

	cfg, err := DiscoverAndLoad(dir)
	require.NoError(t, err)
	assert.Equal(t, []string{"/x"}, cfg.Templates)
}
