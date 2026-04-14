package config

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// readAllAndClose reads r to completion then closes it. It is the
// canonical assertion helper for OpenTemplate — the function returns
// an io.ReadCloser and callers always need both operations.
func readAllAndClose(t *testing.T, r io.ReadCloser) string {
	t.Helper()
	defer r.Close()
	data, err := io.ReadAll(r)
	require.NoError(t, err)
	return string(data)
}

func TestResolver_OpenTemplate_CLIFirst(t *testing.T) {
	// Three layers all provide "handoffs/foo.md"; the CLI-provided
	// directory must win.
	baseDir := t.TempDir()
	cliDir := t.TempDir()
	configDir := t.TempDir()

	// BaseDir is a "project root" — its templates live under templates/.
	// CLI and Config entries are template directories in their own right
	// (no "templates/" prefix), so they contain the handoffs/ tree directly.
	require.NoError(t, os.MkdirAll(filepath.Join(baseDir, "templates", "handoffs"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(cliDir, "handoffs"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(configDir, "handoffs"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(baseDir, "templates", "handoffs", "foo.md"), []byte("from-base"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(cliDir, "handoffs", "foo.md"), []byte("from-cli"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "handoffs", "foo.md"), []byte("from-config"), 0o644))

	r := &Resolver{
		CLITemplateDirs: []string{cliDir},
		Config:          &Config{Templates: []string{configDir}},
		BaseDir:         baseDir,
	}
	rc, source, embedded, err := r.OpenTemplate("handoffs/foo.md")
	require.NoError(t, err)
	assert.False(t, embedded)
	assert.Equal(t, "from-cli", readAllAndClose(t, rc))
	assert.Contains(t, source, cliDir)
}

func TestResolver_OpenTemplate_ConfigBeatsBase(t *testing.T) {
	baseDir := t.TempDir()
	configDir := t.TempDir()

	require.NoError(t, os.MkdirAll(filepath.Join(baseDir, "templates", "handoffs"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(configDir, "handoffs"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(baseDir, "templates", "handoffs", "foo.md"), []byte("from-base"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "handoffs", "foo.md"), []byte("from-config"), 0o644))

	r := &Resolver{
		Config:  &Config{Templates: []string{configDir}},
		BaseDir: baseDir,
	}
	rc, _, _, err := r.OpenTemplate("handoffs/foo.md")
	require.NoError(t, err)
	assert.Equal(t, "from-config", readAllAndClose(t, rc))
}

func TestResolver_OpenTemplate_BaseDirFallback(t *testing.T) {
	baseDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(baseDir, "templates", "handoffs"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(baseDir, "templates", "handoffs", "foo.md"), []byte("from-base"), 0o644))

	r := &Resolver{BaseDir: baseDir}
	rc, source, _, err := r.OpenTemplate("handoffs/foo.md")
	require.NoError(t, err)
	assert.Equal(t, "from-base", readAllAndClose(t, rc))
	assert.Contains(t, source, filepath.Join(baseDir, "templates", "handoffs", "foo.md"))
}

func TestResolver_OpenTemplate_EmbeddedFallback(t *testing.T) {
	// Nothing on the filesystem; embedded wins by being the last
	// configured source with content.
	embedded := fstest.MapFS{
		"templates/handoffs/foo.md": &fstest.MapFile{Data: []byte("from-embedded")},
	}
	r := &Resolver{
		BaseDir:        t.TempDir(),
		EmbeddedFS:     embedded,
		EmbeddedPrefix: "templates",
	}
	rc, source, isEmbedded, err := r.OpenTemplate("handoffs/foo.md")
	require.NoError(t, err)
	assert.True(t, isEmbedded)
	assert.Equal(t, "from-embedded", readAllAndClose(t, rc))
	assert.Equal(t, "<embedded>/templates/handoffs/foo.md", source)
}

func TestResolver_OpenTemplate_MissingEverywhere(t *testing.T) {
	r := &Resolver{BaseDir: t.TempDir()}
	_, _, _, err := r.OpenTemplate("handoffs/foo.md")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found in any configured location")
}

func TestResolver_OpenTemplate_RejectsTraversal(t *testing.T) {
	// A request for "../escaped.md" must never resolve, even if
	// such a file exists alongside the configured directories.
	cliDir := t.TempDir()
	parent := filepath.Dir(cliDir)
	require.NoError(t, os.WriteFile(filepath.Join(parent, "escaped.md"), []byte("secret"), 0o644))
	t.Cleanup(func() { os.Remove(filepath.Join(parent, "escaped.md")) })

	r := &Resolver{CLITemplateDirs: []string{cliDir}}
	_, _, _, err := r.OpenTemplate("../escaped.md")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "..")
}

func TestResolver_OpenTemplate_RejectsAbsolutePath(t *testing.T) {
	r := &Resolver{BaseDir: t.TempDir()}
	var absName string
	if runtime.GOOS == "windows" {
		absName = `C:\a\b.md`
	} else {
		absName = "/etc/passwd"
	}
	_, _, _, err := r.OpenTemplate(absName)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be relative")
}

func TestResolver_OpenTemplate_SkipsMissingLayer(t *testing.T) {
	// If a CLI dir doesn't contain the file but config does, the
	// resolver must continue past the CLI miss.
	baseDir := t.TempDir()
	configDir := t.TempDir()
	cliDir := t.TempDir() // intentionally empty

	require.NoError(t, os.MkdirAll(filepath.Join(configDir, "handoffs"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "handoffs", "foo.md"), []byte("from-config"), 0o644))

	r := &Resolver{
		CLITemplateDirs: []string{cliDir},
		Config:          &Config{Templates: []string{configDir}},
		BaseDir:         baseDir,
	}
	rc, _, _, err := r.OpenTemplate("handoffs/foo.md")
	require.NoError(t, err)
	assert.Equal(t, "from-config", readAllAndClose(t, rc))
}

func TestResolver_ResolveFilestoreOutput_CLIFirst(t *testing.T) {
	base := t.TempDir()
	cli := t.TempDir()
	cfg := t.TempDir()

	r := &Resolver{
		CLIFilestoreDirs: []string{cli},
		Config:           &Config{Filestores: []string{cfg}},
		BaseDir:          base,
	}
	outPath, chosen, err := r.ResolveFilestoreOutput("analysis/foo.json")
	require.NoError(t, err)
	assert.Equal(t, cli, chosen)
	assert.Equal(t, filepath.Join(cli, "analysis", "foo.json"), outPath)

	// Parent dir should have been created eagerly.
	info, err := os.Stat(filepath.Dir(outPath))
	require.NoError(t, err)
	assert.True(t, info.IsDir())
}

func TestResolver_ResolveFilestoreOutput_SkipsUnwritable(t *testing.T) {
	// Unix: make the first candidate read-only so the probe fails.
	// Windows uses different ACL semantics; the behavior is the same
	// in spirit but the mechanism differs.
	if runtime.GOOS == "windows" {
		t.Skip("chmod-based unwritability test is not portable to Windows")
	}
	base := t.TempDir()
	readOnly := t.TempDir()
	require.NoError(t, os.Chmod(readOnly, 0o500))
	t.Cleanup(func() { os.Chmod(readOnly, 0o755) })

	r := &Resolver{
		CLIFilestoreDirs: []string{readOnly},
		BaseDir:          base,
	}
	outPath, chosen, err := r.ResolveFilestoreOutput("foo.json")
	require.NoError(t, err)
	// Fell through to BaseDir/filestore
	assert.Equal(t, filepath.Join(base, DefaultFilestoreDir), chosen)
	assert.Equal(t, filepath.Join(base, DefaultFilestoreDir, "foo.json"), outPath)
}

func TestResolver_ResolveFilestoreOutput_RejectsTraversal(t *testing.T) {
	r := &Resolver{BaseDir: t.TempDir()}
	_, _, err := r.ResolveFilestoreOutput("../escape.json")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "..")
}

func TestResolver_ResolveFilestoreOutput_DefaultBaseDir(t *testing.T) {
	// No overrides — resolver creates BaseDir/filestore lazily.
	base := t.TempDir()
	r := &Resolver{BaseDir: base}
	outPath, chosen, err := r.ResolveFilestoreOutput("report.json")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(base, DefaultFilestoreDir), chosen)
	assert.Equal(t, filepath.Join(base, DefaultFilestoreDir, "report.json"), outPath)
}

func TestResolver_ListTemplateSearchPath(t *testing.T) {
	r := &Resolver{
		CLITemplateDirs: []string{"/cli/a", "/cli/b"},
		Config:          &Config{Templates: []string{"/cfg/a"}},
		BaseDir:         "/base",
		EmbeddedFS:      fstest.MapFS{},
	}
	path := r.ListTemplateSearchPath()
	assert.Equal(t, []string{
		"/cli/a",
		"/cli/b",
		"/cfg/a",
		filepath.Join("/base", DefaultTemplateDir),
		"<embedded>",
	}, path)
}
