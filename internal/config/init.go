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
	// 0o750: owner rwx, group rx, others none. Tighter than the
	// conventional 0o755 because signatory project dirs are
	// single-user by design; other-readable adds no user value.
	if err := os.MkdirAll(absDir, 0o750); err != nil {
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
		if err := os.MkdirAll(path, 0o750); err != nil {
			return nil, fmt.Errorf("mkdir %s: %w", path, err)
		}
		result.DirectoriesCreated = append(result.DirectoriesCreated, path)
		if opts.Out != nil {
			fmt.Fprintf(opts.Out, "ok   %s\n", path)
		}
	}

	// 3. Config scaffold. Use O_EXCL (without --force) or O_TRUNC
	// (with --force) so the existence check and the write happen as
	// one atomic operation. The previous stat-then-write shape had a
	// TOCTOU window where a concurrent init or an adversary could
	// slip a file in between (config reviewer F11). The handoff
	// command's writeHandoff already uses this pattern; matching it
	// here keeps the discipline consistent.
	cfgPath := filepath.Join(absDir, ConfigFileName)
	result.ConfigPath = cfgPath

	flag := os.O_WRONLY | os.O_CREATE
	if opts.Force {
		flag |= os.O_TRUNC
	} else {
		flag |= os.O_EXCL
	}
	// 0o644: owner rw, group/other r. Intentional — the config is
	// meant to be read by the user's editor, potentially by scripted
	// tools running as a different uid in the same project, and has
	// no sensitive content (it's a path-list scaffold). 0o600 would
	// be inappropriately restrictive for user-facing scaffold output.
	f, err := os.OpenFile(cfgPath, flag, 0o644) //nolint:gosec // G302: user-facing scaffold, no secrets; 0o600 would block scripted tools running as non-owner in the same project
	switch {
	case err == nil:
		_, writeErr := f.Write([]byte(ConfigScaffold))
		closeErr := f.Close()
		if writeErr != nil {
			return nil, fmt.Errorf("write %s: %w", cfgPath, writeErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close %s: %w", cfgPath, closeErr)
		}
		result.ConfigWritten = true
		if opts.Out != nil {
			fmt.Fprintf(opts.Out, "wrote %s\n", cfgPath)
		}
	case os.IsExist(err):
		// O_EXCL fired — file already there and --force was not passed.
		// This is the documented "skip-on-exists" path.
		if opts.Out != nil {
			fmt.Fprintf(opts.Out, "skip %s (exists; --force to overwrite)\n", cfgPath)
		}
	default:
		return nil, fmt.Errorf("open %s: %w", cfgPath, err)
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

		// 0o750 mirrors the dir perms in InitProject above.
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return err
		}
		// Use O_EXCL when not in force mode so the existence check and
		// the write happen as one atomic operation — the same discipline
		// applied to the config scaffold (init.go TOCTOU fix). A
		// stat-then-write window can be raced by a concurrent init or
		// adversarial file creation; O_EXCL eliminates the window.
		// With force mode we use O_TRUNC to overwrite any existing file.
		// We open the destination before reading the embedded FS data so
		// we can skip early (without the read) when O_EXCL fires.
		flag := os.O_WRONLY | os.O_CREATE
		if force {
			flag |= os.O_TRUNC
		} else {
			flag |= os.O_EXCL
		}
		// 0o644 rationale matches the config-scaffold write above: these
		// are user-editable template files, not secrets.
		f, openErr := os.OpenFile(target, flag, 0o644) //nolint:gosec // G302: user-facing template file, no secrets (same rationale as the config scaffold write above)
		if openErr != nil {
			if os.IsExist(openErr) {
				// O_EXCL fired — file already present without --force.
				skipped++
				if out != nil {
					fmt.Fprintf(out, "skip %s (exists)\n", target)
				}
				return nil
			}
			return openErr
		}
		data, readErr := fs.ReadFile(embedFS, path)
		if readErr != nil {
			f.Close()
			return readErr
		}
		_, writeErr := f.Write(data)
		closeErr := f.Close()
		if writeErr != nil {
			return writeErr
		}
		if closeErr != nil {
			return closeErr
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
