package config

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// DefaultTemplateDir and DefaultFilestoreDir are the repo-root
// convention locations that every Resolver falls through to when no
// CLI flag or config entry resolves a request. They are relative
// paths interpreted against Resolver.BaseDir (or the process CWD
// when BaseDir is empty).
const (
	DefaultTemplateDir  = "templates"
	DefaultFilestoreDir = "filestore"
)

// Resolver picks a filesystem location for template reads and
// filestore writes, unifying three sources of preference — CLI
// flags, the loaded config file, and the built-in defaults —
// behind two methods that all call sites use.
//
// The lookup order is always:
//
//	READ  templates:  CLI → Config → BaseDir/templates  → Embedded
//	WRITE filestores: CLI → Config → BaseDir/filestore
//
// Within each tier, entries are tried in the order provided by the
// caller / user. For reads, the first existing file wins. For writes,
// the first writable directory wins (permission or ENOSPC failures
// cause the next candidate to be tried).
//
// Resolvers are cheap to construct; a new one per command invocation
// is fine. They are NOT safe for concurrent use across goroutines
// without external synchronization (not currently a problem because
// CLI commands are single-goroutine).
type Resolver struct {
	// CLITemplateDirs are directories supplied via `--template-dir`
	// (repeatable), in the order the user specified them.
	CLITemplateDirs []string

	// CLIFilestoreDirs are directories supplied via `--filestore-dir`
	// (repeatable), in the order the user specified them.
	CLIFilestoreDirs []string

	// Config is the parsed signatory.config.toml; may be nil.
	Config *Config

	// EmbeddedFS, when non-nil, provides the compiled-in fallback
	// template set. Production callers pass signatory.EmbeddedTemplates
	// from the module root; tests can substitute a fstest.MapFS.
	EmbeddedFS fs.FS

	// EmbeddedPrefix is the subpath inside EmbeddedFS under which
	// templates live. For signatory.EmbeddedTemplates the prefix is
	// "templates". Tests may use "" to address MapFS roots directly.
	EmbeddedPrefix string

	// BaseDir is the root against which DefaultTemplateDir and
	// DefaultFilestoreDir are resolved. Empty means the process CWD
	// (the production default).
	BaseDir string
}

// OpenTemplate opens a template by its relative path (e.g.,
// "handoffs/security-review-v1.md") for reading. The caller must
// close the returned reader.
//
// Returns:
//   - rc:       the opened reader
//   - source:   the path the template was read from (absolute for
//     filesystem entries; "<embedded>/PATH" for the fallback)
//   - embedded: true when the reader is backed by EmbeddedFS rather
//     than the filesystem
//   - err:      non-nil if no configured location contains the template
//
// Path-traversal components ("..") in name are rejected to prevent a
// malicious template name from escaping the configured directories.
func (r *Resolver) OpenTemplate(name string) (rc io.ReadCloser, source string, embedded bool, err error) {
	if err := validateRelName(name); err != nil {
		return nil, "", false, err
	}

	for _, dir := range r.CLITemplateDirs {
		if f, path, ok := tryOpenFile(filepath.Join(dir, name)); ok {
			return f, path, false, nil
		}
	}
	if r.Config != nil {
		for _, dir := range r.Config.Templates {
			if f, path, ok := tryOpenFile(filepath.Join(dir, name)); ok {
				return f, path, false, nil
			}
		}
	}
	if f, path, ok := tryOpenFile(filepath.Join(r.base(), DefaultTemplateDir, name)); ok {
		return f, path, false, nil
	}

	if r.EmbeddedFS != nil {
		embeddedPath := name
		if r.EmbeddedPrefix != "" {
			embeddedPath = r.EmbeddedPrefix + "/" + name
		}
		f, openErr := r.EmbeddedFS.Open(embeddedPath)
		if openErr == nil {
			// fs.File already satisfies io.ReadCloser (it has both Read
			// and Close), so no type assertion is needed. An unchecked
			// assertion would panic against any custom fs.FS whose Open
			// returns a value that is an fs.File but not registered as
			// an io.ReadCloser — idiomatic Go avoids this risk entirely
			// by relying on structural satisfaction.
			return f, "<embedded>/" + embeddedPath, true, nil
		}
	}

	return nil, "", false, fmt.Errorf("template %q not found in any configured location", name)
}

// ResolveFilestoreOutput chooses an output location for an artifact
// named by its path relative to a filestore root (e.g.,
// "analysis/thefuck-security-v1.json"). It returns the absolute file
// path the caller should write and the directory that won resolution
// — useful for logging ("wrote to /path/filestore/...").
//
// A directory is considered "usable" when the resolver can MkdirAll
// it and successfully create a probe file inside it. On failure the
// next candidate is tried; if none succeed, the error from the last
// attempt is returned.
//
// Path-traversal components ("..") in name are rejected.
func (r *Resolver) ResolveFilestoreOutput(name string) (outputPath, chosenDir string, err error) {
	if err := validateRelName(name); err != nil {
		return "", "", err
	}

	candidates := make([]string, 0, 1+len(r.CLIFilestoreDirs)+len(r.Config.filestoreList()))
	candidates = append(candidates, r.CLIFilestoreDirs...)
	candidates = append(candidates, r.Config.filestoreList()...)
	candidates = append(candidates, filepath.Join(r.base(), DefaultFilestoreDir))

	var lastErr error
	for _, dir := range candidates {
		abs, probeErr := ensureWritableDir(dir)
		if probeErr != nil {
			lastErr = probeErr
			continue
		}
		outPath := filepath.Join(abs, name)
		if mkErr := os.MkdirAll(filepath.Dir(outPath), 0o755); mkErr != nil {
			lastErr = mkErr
			continue
		}
		return outPath, abs, nil
	}

	if lastErr != nil {
		return "", "", fmt.Errorf("no writable filestore directory available: %w", lastErr)
	}
	return "", "", fmt.Errorf("no filestore directory configured")
}

// ListTemplateSearchPath returns the ordered list of directories
// OpenTemplate would consult for filesystem reads, plus a final
// "<embedded>" marker when an embedded fallback is configured.
// Useful for `signatory init` / `signatory handoff --show-path`-type
// diagnostics: users can see exactly where signatory is looking.
func (r *Resolver) ListTemplateSearchPath() []string {
	out := make([]string, 0)
	out = append(out, r.CLITemplateDirs...)
	if r.Config != nil {
		out = append(out, r.Config.Templates...)
	}
	out = append(out, filepath.Join(r.base(), DefaultTemplateDir))
	if r.EmbeddedFS != nil {
		out = append(out, "<embedded>")
	}
	return out
}

// base returns the effective root for default path resolution. An
// explicit BaseDir on the Resolver wins; otherwise "." (the process
// CWD).
func (r *Resolver) base() string {
	if r.BaseDir != "" {
		return r.BaseDir
	}
	return "."
}

// validateRelName rejects names that would escape the configured
// directory, contain absolute paths, or include NUL bytes. We accept
// slash-separated subpaths ("handoffs/foo.md") so callers can address
// the templates/ subtree naturally.
//
// Defence layers, in order:
//  1. Empty / absolute paths → error.
//  2. NUL bytes → error (some OS APIs truncate at NUL, enabling bypass).
//  3. Percent signs → error (URL-encoded traversal: %2e%2e/foo).
//  4. Backslashes → error (Windows-separator traversal on non-Windows hosts;
//     also catches the ..\\foo form that filepath.ToSlash alone wouldn't
//     convert on Linux where '\\' is not the separator).
//  5. Non-ASCII characters → error (fullwidth dots U+FF0E and other
//     normalisation-equivalent forms that look like '.' to a human but
//     reach the OS as different bytes).
//  6. Segment-by-segment ".." check on the forward-slash-split name.
func validateRelName(name string) error {
	if name == "" {
		return fmt.Errorf("name must not be empty")
	}
	if filepath.IsAbs(name) {
		return fmt.Errorf("name %q must be relative, not absolute", name)
	}
	if strings.ContainsRune(name, 0x00) {
		return fmt.Errorf("name must not contain NUL")
	}
	// Reject percent signs: they are never valid in template/filestore
	// names and are the canonical encoding used by URL-traversal payloads
	// (%2e%2e/, %252e%252e/, etc.).
	if strings.ContainsRune(name, '%') {
		return fmt.Errorf("name %q must not contain percent-encoded sequences", name)
	}
	// Reject backslashes: on non-Windows systems, '\\' is a valid filename
	// character and filepath.ToSlash does NOT convert it, so ..\\foo would
	// pass the segment check below. On Windows, filepath.ToSlash converts
	// backslashes before the segment check, so this guard is redundant but
	// harmless.
	if strings.ContainsRune(name, '\\') {
		return fmt.Errorf("name %q must not contain backslashes", name)
	}
	// Reject non-ASCII characters (fullwidth full stop U+FF0E, overlong
	// UTF-8 sequences, and similar normalisation tricks). Template and
	// filestore names are always plain ASCII identifiers.
	for i := 0; i < len(name); i++ {
		if name[i] > 0x7E {
			return fmt.Errorf("name %q must contain only printable ASCII characters", name)
		}
	}
	// Normalize to forward slashes and check each segment for "..".
	normalized := filepath.ToSlash(name)
	for _, part := range strings.Split(normalized, "/") {
		if part == ".." {
			return fmt.Errorf("name %q must not contain %q", name, "..")
		}
	}
	return nil
}

// tryOpenFile attempts to open path. On success, returns the open
// file plus its absolute path (for diagnostic reporting). On any
// error — ENOENT, EACCES, EISDIR, etc. — returns ok=false so the
// caller tries the next candidate.
func tryOpenFile(path string) (*os.File, string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, "", false
	}
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		f.Close()
		return nil, "", false
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	return f, abs, true
}

// ensureWritableDir creates dir if absent and verifies the current
// process can write inside it by round-tripping a temp file. The
// probe is required because os.MkdirAll succeeds on read-only
// filesystems for already-existing directories, and we specifically
// want "can the caller write here?" semantics.
func ensureWritableDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("abs %s: %w", dir, err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", abs, err)
	}
	probe, err := os.CreateTemp(abs, ".signatory-write-probe-*")
	if err != nil {
		return "", fmt.Errorf("probe %s: %w", abs, err)
	}
	name := probe.Name()
	probe.Close()
	if removeErr := os.Remove(name); removeErr != nil {
		// Clean-up failure is logged implicitly via return error —
		// probe creation succeeded, so the directory IS writable.
		// Remove failure is very unusual (races); proceed with abs.
		_ = removeErr
	}
	return abs, nil
}

// filestoreList is a nil-safe accessor that returns the config's
// Filestores list or a nil slice when the Config pointer is nil.
// Used by ResolveFilestoreOutput so the call site stays tidy.
func (cfg *Config) filestoreList() []string {
	if cfg == nil {
		return nil
	}
	return cfg.Filestores
}
