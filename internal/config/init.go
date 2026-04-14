package config

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ConfigScaffold is the contents written to signatory.config.toml by
// InitProject when no config exists yet. All keys are commented out:
// the scaffold serves as self-documentation rather than a
// functional configuration. Users uncomment and edit as needed.
const ConfigScaffold = `# signatory.config.toml — path preferences for signatory.
#
# All keys are optional. Entries added here are consulted *before*
# the default ./templates/ and ./filestore/ paths. Within each list,
# entries are tried in order and the first match wins.
#
# Reads  (templates):  CLI flags → this file → ./templates/ → embedded
# Writes (filestores): CLI flags → this file → ./filestore/

# Additional directories to search for prompt templates.
# templates = [
#     "/Users/you/shared-signatory/templates",
# ]

# Preferred output directories for filestore artifacts.
# filestores = [
#     "/Users/you/signatory-artifacts",
# ]
`

// InitResult summarizes the outcome of InitProject so callers can
// print a user-facing report. All counts are >= 0; slices are
// non-nil but may be empty.
type InitResult struct {
	// TemplatesCopied is the count of template files written from
	// the embedded fallback into the target templates/ directory.
	TemplatesCopied int

	// TemplatesSkipped is the count of template files that already
	// existed at the destination and were preserved (force=false).
	TemplatesSkipped int

	// DirectoriesCreated is the list of directories created (or
	// confirmed to exist) during init — always includes filestore/
	// and filestore/analysis/.
	DirectoriesCreated []string

	// ConfigWritten is true when InitProject wrote a fresh
	// signatory.config.toml. False when a config already existed
	// and force=false.
	ConfigWritten bool

	// ConfigPath is the absolute path of the config file, whether
	// newly written or pre-existing.
	ConfigPath string
}

// InitOptions controls InitProject's behavior.
type InitOptions struct {
	// Dir is the absolute or relative directory to initialize. An
	// empty value means the process CWD.
	Dir string

	// Force, when true, overwrites any existing templates and config
	// scaffold. Without force, existing files are preserved.
	Force bool

	// Out receives per-file progress messages. Pass nil to suppress.
	Out io.Writer

	// EmbeddedFS and EmbeddedPrefix point at the source of truth for
	// the template files to copy. Production callers pass
	// signatory.EmbeddedTemplates and "templates"; tests can pass a
	// fstest.MapFS.
	EmbeddedFS     fs.FS
	EmbeddedPrefix string
}

// InitProject scaffolds the directory layout signatory expects in a
// user's project:
//
//   - ./templates/ populated from EmbeddedFS (each file is either
//     written fresh or preserved depending on Force).
//   - ./filestore/ and ./filestore/analysis/ created if absent.
//   - ./signatory.config.toml written if absent (or overwritten with
//     Force).
//
// InitProject is idempotent by design: repeated runs without --force
// are safe and only fill in gaps. The common upgrade path is:
//
//	signatory init           # run after upgrading the binary
//
// which copies any NEW templates shipped by the new version without
// trampling user edits to existing ones.
func InitProject(opts InitOptions) (*InitResult, error) {
	if opts.EmbeddedFS == nil {
		return nil, fmt.Errorf("InitOptions.EmbeddedFS is required")
	}
	dir := opts.Dir
	if dir == "" {
		dir = "."
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve dir %s: %w", dir, err)
	}
	if err := os.MkdirAll(absDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", absDir, err)
	}

	result := &InitResult{}

	// 1. Templates from embedded fallback.
	templatesRoot := filepath.Join(absDir, DefaultTemplateDir)
	copied, skipped, err := copyEmbeddedTree(opts.EmbeddedFS, opts.EmbeddedPrefix, templatesRoot, opts.Force, opts.Out)
	if err != nil {
		return nil, fmt.Errorf("copy templates to %s: %w", templatesRoot, err)
	}
	result.TemplatesCopied = copied
	result.TemplatesSkipped = skipped

	// 2. Filestore output tree.
	for _, sub := range []string{DefaultFilestoreDir, filepath.Join(DefaultFilestoreDir, "analysis")} {
		path := filepath.Join(absDir, sub)
		if err := os.MkdirAll(path, 0o755); err != nil {
			return nil, fmt.Errorf("mkdir %s: %w", path, err)
		}
		result.DirectoriesCreated = append(result.DirectoriesCreated, path)
		if opts.Out != nil {
			fmt.Fprintf(opts.Out, "ok   %s\n", path)
		}
	}

	// 3. Config scaffold.
	cfgPath := filepath.Join(absDir, ConfigFileName)
	result.ConfigPath = cfgPath
	_, statErr := os.Stat(cfgPath)
	switch {
	case statErr == nil && !opts.Force:
		if opts.Out != nil {
			fmt.Fprintf(opts.Out, "skip %s (exists; --force to overwrite)\n", cfgPath)
		}
	case statErr == nil, os.IsNotExist(statErr):
		if err := os.WriteFile(cfgPath, []byte(ConfigScaffold), 0o644); err != nil {
			return nil, fmt.Errorf("write %s: %w", cfgPath, err)
		}
		result.ConfigWritten = true
		if opts.Out != nil {
			fmt.Fprintf(opts.Out, "wrote %s\n", cfgPath)
		}
	default:
		return nil, fmt.Errorf("stat %s: %w", cfgPath, statErr)
	}

	return result, nil
}

// copyEmbeddedTree walks every regular file under embedFS/prefix and
// writes it to dst/<relative-path>. Directories along the way are
// created as needed. Files already present at the destination are
// preserved unless force is true. The embedded set is the source of
// truth; this helper never reads dst back into the embed FS.
func copyEmbeddedTree(embedFS fs.FS, prefix, dst string, force bool, out io.Writer) (copied, skipped int, err error) {
	if prefix == "" {
		prefix = "."
	}
	walkErr := fs.WalkDir(embedFS, prefix, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		rel := strings.TrimPrefix(path, prefix)
		rel = strings.TrimPrefix(rel, "/")
		if rel == "" {
			return nil
		}
		target := filepath.Join(dst, rel)

		if !force {
			if _, statErr := os.Stat(target); statErr == nil {
				skipped++
				if out != nil {
					fmt.Fprintf(out, "skip %s (exists)\n", target)
				}
				return nil
			}
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		data, readErr := fs.ReadFile(embedFS, path)
		if readErr != nil {
			return readErr
		}
		if err := os.WriteFile(target, data, 0o644); err != nil {
			return err
		}
		copied++
		if out != nil {
			fmt.Fprintf(out, "wrote %s\n", target)
		}
		return nil
	})
	if walkErr != nil {
		return copied, skipped, walkErr
	}
	return copied, skipped, nil
}
