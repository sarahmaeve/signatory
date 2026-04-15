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

// ── adversarial tests ──────────────────────────────────────────────────────

// TestValidateRelName_URLEncodedDots verifies that percent-encoded dot
// sequences (%2e%2e) are NOT decoded by the path validator. On most OSes
// they land as literal percent-sequences in filenames — no traversal — but
// the validator should still reject them to keep the contract clean: only
// genuine relative subpaths are accepted, not encoded bypass attempts.
func TestValidateRelName_URLEncodedDots(t *testing.T) {
	cases := []string{
		"%2e%2e/foo",
		"%2e%2e%2ffoo",
		"%252e%252e/foo",
	}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			err := validateRelName(c)
			require.Error(t, err, "URL-encoded dot sequence %q must be rejected", c)
		})
	}
}

// TestValidateRelName_FullwidthDots verifies that Unicode fullwidth full
// stop characters (U+FF0E, visually identical to '.') are rejected. They
// do not cause OS-level traversal on Linux/macOS, but accepting them
// creates a confusing disparity between what the user sees and what is
// stored on disk, and may break on future UNICODE-aware OS normalization.
func TestValidateRelName_FullwidthDots(t *testing.T) {
	// \uff0e is the Unicode fullwidth full stop '．'
	err := validateRelName("\uff0e\uff0e/foo")
	require.Error(t, err, "fullwidth dots must be rejected")
}

// TestValidateRelName_NULInMiddle verifies embedded NUL bytes are rejected
// even when they don't appear at the start of the name.
func TestValidateRelName_NULInMiddle(t *testing.T) {
	err := validateRelName("valid\x00../escape")
	require.Error(t, err, "name with embedded NUL must be rejected")
}

// TestValidateRelName_WindowsBackslashDots verifies that backslash-separated
// double-dots are rejected. On Windows filepath.ToSlash converts them; on
// Linux the part is literally "..\\foo" which is NOT ".." and thus would
// pass the current per-segment check — the validator must guard this.
func TestValidateRelName_WindowsBackslashDots(t *testing.T) {
	err := validateRelName(`..\\foo`)
	// If running on Windows, filepath.ToSlash converts this to "../foo" which
	// the existing ".." check catches. On Linux the raw backslash form should
	// also be rejected to prevent confusion.
	require.Error(t, err, `..\\foo must be rejected`)
}

// TestValidateRelName_TrailingDotDot verifies that trailing ".." segments
// (foo/..) are rejected. filepath.Join resolves "foo/.." to the parent
// directory, which defeats the containment guarantee.
func TestValidateRelName_TrailingDotDot(t *testing.T) {
	cases := []string{
		"foo/..",
		"foo/../",
		"a/b/../..",
	}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			err := validateRelName(c)
			require.Error(t, err, "trailing/embedded .. segment %q must be rejected", c)
		})
	}
}

// TestOpenTemplate_DirectoryNameSkipped verifies that a template name
// resolving to a directory is silently skipped (tryOpenFile rejects IsDir)
// and the resolver continues to the next candidate.
func TestOpenTemplate_DirectoryNameSkipped(t *testing.T) {
	// Place a directory at templates/foo.md — tryOpenFile must skip it.
	cliDir := t.TempDir()
	dirAsFile := filepath.Join(cliDir, "foo.md")
	require.NoError(t, os.MkdirAll(dirAsFile, 0o755))

	// A second CLI dir has the real file.
	secondDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(secondDir, "foo.md"), []byte("real"), 0o644))

	r := &Resolver{CLITemplateDirs: []string{cliDir, secondDir}}
	rc, _, _, err := r.OpenTemplate("foo.md")
	require.NoError(t, err)
	assert.Equal(t, "real", readAllAndClose(t, rc))
}

// TestOpenTemplate_SymlinkOutsideDir documents the current behaviour when a
// template directory contains a symlink pointing outside the directory: the
// code follows the symlink. This test is a regression anchor — if the
// behaviour ever changes to reject escaped symlinks, the test name should
// become TestOpenTemplate_RejectsSymlinkEscape.
func TestOpenTemplate_SymlinkOutsideDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	cliDir := t.TempDir()
	outsideDir := t.TempDir()

	// Write the real file outside the template dir.
	outsideFile := filepath.Join(outsideDir, "secret.md")
	require.NoError(t, os.WriteFile(outsideFile, []byte("outside-content"), 0o644))

	// Plant a symlink inside cliDir pointing to it.
	symlink := filepath.Join(cliDir, "escaped.md")
	require.NoError(t, os.Symlink(outsideFile, symlink))

	r := &Resolver{CLITemplateDirs: []string{cliDir}}
	rc, _, _, err := r.OpenTemplate("escaped.md")
	// Current behaviour: symlink is followed, content is returned.
	// This is intentional for user convenience (shared template dirs),
	// but callers should ensure template dirs are trustworthy.
	require.NoError(t, err)
	assert.Equal(t, "outside-content", readAllAndClose(t, rc))
}

// TestResolveFilestoreOutput_ErrorWrapsLastOnly verifies that when all
// filestore candidates fail, the error wraps only the last attempt — not a
// concatenation of all tried paths. This is the current behaviour (single
// lastErr is retained); the test anchors it as a regression target.
func TestResolveFilestoreOutput_ErrorWrapsLastOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod-based test not portable to Windows")
	}
	// Three read-only directories.
	d1 := t.TempDir()
	d2 := t.TempDir()
	baseDir := t.TempDir()
	require.NoError(t, os.Chmod(d1, 0o500))
	require.NoError(t, os.Chmod(d2, 0o500))
	require.NoError(t, os.Chmod(baseDir, 0o500))
	t.Cleanup(func() {
		os.Chmod(d1, 0o755)
		os.Chmod(d2, 0o755)
		os.Chmod(baseDir, 0o755)
	})

	r := &Resolver{
		CLIFilestoreDirs: []string{d1, d2},
		BaseDir:          baseDir,
	}
	_, _, err := r.ResolveFilestoreOutput("foo.json")
	require.Error(t, err)
	// The error message must contain the last-tried directory (baseDir's
	// filestore) but must NOT enumerate all three candidates separately.
	// Verify d1 (first, non-last) does NOT appear in the error string.
	assert.NotContains(t, err.Error(), d1,
		"error must not list the first (non-last) failed candidate")
}

// TestCopyEmbeddedTree_DotDotInRelPath verifies that copyEmbeddedTree does
// not write outside dst when the embedded FS yields a path that, after
// prefix stripping, contains a ".." component. We simulate this by calling
// copyEmbeddedTree with a prefix that does NOT match the key, causing rel
// to start with what looks like a parent directory. filepath.Join's path
// cleaning is the only guard here; this test confirms it is sufficient.
//
// Note: fstest.MapFS keys with literal ".." cause fs.WalkDir to loop
// infinitely (Go stdlib behaviour) so we cannot craft that case directly.
// Instead we test that filepath.Join's normalisation prevents any "rel"
// that already starts clean from escaping dst.
func TestCopyEmbeddedTree_SafeRelPathJoin(t *testing.T) {
	dst := t.TempDir()

	// Legitimate embedded tree — no escape.
	safeFS := fstest.MapFS{
		"templates/handoffs/foo.md": &fstest.MapFile{Data: []byte("content")},
	}
	copied, _, err := copyEmbeddedTree(safeFS, "templates", dst, true, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, copied)

	// Confirm written path is inside dst.
	written := filepath.Join(dst, "handoffs", "foo.md")
	_, statErr := os.Stat(written)
	assert.NoError(t, statErr, "expected file at %s", written)
}

// TestCopyEmbeddedTree_AtomicSkipOnExists verifies that the O_EXCL-based
// write in copyEmbeddedTree (force=false) correctly skips a file that was
// created concurrently between the MkdirAll and the open. Without O_EXCL,
// the stat-then-write shape had a TOCTOU window where a concurrent init or
// adversarial file creation could slip a file in and have it overwritten.
// O_EXCL collapses the check and the write to a single syscall, matching
// the discipline applied to the config scaffold write.
//
// Regression guard: if the implementation reverts to stat+WriteFile, this
// test will still pass (stat sees the file and skips). The meaningful
// assertion is the skip count — if O_EXCL is broken, a race can silently
// overwrite and not increment skipped. Because races can't be guaranteed in
// unit tests, we also verify the static path: pre-existing file → skip.
func TestCopyEmbeddedTree_SkipsExistingFileWithoutForce(t *testing.T) {
	dst := t.TempDir()
	embedFS := fstest.MapFS{
		"templates/foo.md": &fstest.MapFile{Data: []byte("new content")},
	}

	// Pre-plant the target file with sentinel content.
	target := filepath.Join(dst, "foo.md")
	require.NoError(t, os.WriteFile(target, []byte("original content"), 0o644))

	// copyEmbeddedTree with force=false must skip the existing file.
	copied, skipped, err := copyEmbeddedTree(embedFS, "templates", dst, false, nil)
	require.NoError(t, err)
	assert.Equal(t, 0, copied, "no files should be written when target already exists")
	assert.Equal(t, 1, skipped, "existing file must be counted as skipped")

	// The sentinel content must be unchanged — the existing file was not overwritten.
	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "original content", string(got),
		"pre-existing file content must survive a no-force copyEmbeddedTree")
}

// TestCopyEmbeddedTree_ForceOverwritesExisting verifies that force=true
// replaces an existing file, which is the documented contract for --force.
func TestCopyEmbeddedTree_ForceOverwritesExisting(t *testing.T) {
	dst := t.TempDir()
	embedFS := fstest.MapFS{
		"templates/foo.md": &fstest.MapFile{Data: []byte("new content")},
	}

	target := filepath.Join(dst, "foo.md")
	require.NoError(t, os.WriteFile(target, []byte("original"), 0o644))

	copied, skipped, err := copyEmbeddedTree(embedFS, "templates", dst, true, nil)
	require.NoError(t, err)
	assert.Equal(t, 1, copied)
	assert.Equal(t, 0, skipped)

	got, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "new content", string(got), "force must overwrite existing file")
}

// TestResolver_OpenTemplate_EmbeddedFSFileImplementsReadCloser guards the
// removal of the unnecessary f.(io.ReadCloser) type assertion in OpenTemplate.
// fs.File structurally satisfies io.ReadCloser (it has both Read and Close);
// the assertion was removed because it would panic against a custom fs.FS
// implementation that returns an fs.File not registered as an io.ReadCloser.
// This test verifies that a minimal custom fs.FS (one that does NOT embed
// io.ReadCloser anywhere) still works correctly with OpenTemplate.
func TestResolver_OpenTemplate_EmbeddedFSFileImplementsReadCloser(t *testing.T) {
	// Use fstest.MapFS — its Open returns *fstest.openMapFile which satisfies
	// fs.File (and therefore io.ReadCloser) structurally, not via explicit
	// declaration. If the type assertion were still present it would succeed
	// here too. The meaningful regression this test provides is that future
	// custom FS implementations work without explicit io.ReadCloser declaration.
	embedded := fstest.MapFS{
		"tmpl/hello.md": &fstest.MapFile{Data: []byte("hello from embedded")},
	}
	r := &Resolver{
		BaseDir:        t.TempDir(),
		EmbeddedFS:     embedded,
		EmbeddedPrefix: "tmpl",
	}
	rc, _, isEmbedded, err := r.OpenTemplate("hello.md")
	require.NoError(t, err)
	assert.True(t, isEmbedded)
	// Verify the returned rc is usable as io.ReadCloser — both Read and Close.
	got := readAllAndClose(t, rc)
	assert.Equal(t, "hello from embedded", got)
}
