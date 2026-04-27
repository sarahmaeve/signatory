package pypi

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/sarahmaeve/signatory/internal/manifest"
)

// maxIncludeDepth caps recursion through -r references. A
// requirements.txt that includes more than this many nested levels
// is either malformed or attempting to defeat the parser; either way,
// stopping with a clear error beats blowing the stack.
const maxIncludeDepth = 5

// maxRequirementsBytes caps untrusted requirements.txt input. Each
// file (the top-level entry point AND every -r-included child) is
// bounded independently — the depth cap stops cycles, but without a
// per-file size cap a single huge child file can still drive
// unbounded allocation. Real-world pip-compile output for large
// projects runs well under 16 KiB; 64 KiB leaves comfortable
// headroom while ruling out adversarial input. Adjust if a real
// project legitimately exceeds it.
const maxRequirementsBytes = 64 * 1024

// ErrIncludeOutOfScope is returned when a -r include directive
// resolves to a path outside the original requirements file's
// directory tree (absolute path, .. traversal, or any combination
// that escapes the scope root). Callers should treat the entire
// parse as failed — partial deps from a file that tried to read
// /etc/passwd shouldn't be silently surfaced.
var ErrIncludeOutOfScope = errors.New("requirements include points outside file directory")

// ErrIncludeDepthExceeded is returned when -r references chain
// more than maxIncludeDepth levels deep. Real projects don't nest
// this many levels; cycles and pathological cases do.
var ErrIncludeDepthExceeded = errors.New("requirements include depth exceeded")

// ErrFileTooLarge is returned when a requirements.txt file (top-
// level or -r-included) exceeds maxRequirementsBytes. Wrapping with
// %w lets callers errors.Is for the specific cap-trip case without
// matching string-prose.
var ErrFileTooLarge = errors.New("requirements file exceeds size cap")

// ParseRequirements reads a pip requirements.txt file at path and
// returns the dependencies it declares. The file's directory
// becomes the scope root for any -r include resolution — includes
// must resolve to paths within (or below) this directory.
//
// requirements.txt has no project metadata, so callers receive
// only the dep list. For project identity, use the pyproject.toml
// parser when it lands.
//
// All deps are marked Direct=true: requirements.txt has no notion
// of transitive deps. A pip-compile-output requirements.txt does
// list transitive deps, but signatory has no way to distinguish
// them from direct deps at parse time without a separate manifest
// (e.g., the input requirements.in). v0.1 treats every line as
// direct; the analyst can refine when surveying real projects.
//
// Returns ErrIncludeOutOfScope when an -r reference targets a
// path outside the original file's directory tree.
// Returns ErrIncludeDepthExceeded for chains longer than maxIncludeDepth.
func ParseRequirements(path string) ([]manifest.Dep, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve %q: %w", path, err)
	}
	scopeRoot := filepath.Dir(absPath)
	return parseAtDepth(absPath, scopeRoot, 0)
}

// parseAtDepth is the recursive implementation. depth starts at 0
// for the top-level call and increments at each -r hop. scopeRoot
// is the original file's directory — every -r resolution checks
// against this single anchor, not the current file's directory,
// so a chain of subdirectory -r references can't escape via
// successive .. segments.
func parseAtDepth(path, scopeRoot string, depth int) ([]manifest.Dep, error) {
	if depth > maxIncludeDepth {
		return nil, fmt.Errorf("%w: at depth %d (file: %s)", ErrIncludeDepthExceeded, depth, path)
	}

	// Open once and read with a size-bounded LimitReader. Same
	// pattern as parsePyProject: the LimitReader caps bytes from
	// the SAME open file handle, so there's no os.Stat → os.ReadFile
	// TOCTOU window where a small file could be swapped for a
	// larger one between the size check and the read.
	fh, err := os.Open(path) //nolint:gosec // G304: caller-supplied or include-resolved path validated against scopeRoot before reaching this read; bytes capped by LimitReader below
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", path, err)
	}
	defer fh.Close() //nolint:errcheck // read-only file; close cannot fail meaningfully

	// Read up to maxRequirementsBytes+1 bytes. If we got the +1,
	// the file exceeds the cap. The cap fires per-file, so a -r
	// chain of N moderately-sized files is fine; one huge file
	// (top-level or included) is not.
	data, err := io.ReadAll(io.LimitReader(fh, maxRequirementsBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", path, err)
	}
	if len(data) > maxRequirementsBytes {
		return nil, fmt.Errorf("%w: %s (cap: %d bytes)",
			ErrFileTooLarge, path, maxRequirementsBytes)
	}

	lines := splitContinuations(string(data))
	var deps []manifest.Dep

	for _, raw := range lines {
		line := stripComment(raw)
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		if includePath, ok := stripIncludePrefix(line); ok {
			included, err := resolveAndParseInclude(includePath, path, scopeRoot, depth+1)
			if err != nil {
				return nil, err
			}
			deps = append(deps, included...)
			continue
		}

		dep, ok := parseRequirementLine(line)
		if !ok {
			continue
		}
		deps = append(deps, dep)
	}

	return deps, nil
}

// resolveAndParseInclude validates a -r target against the scope
// root and recurses. Validation rules:
//
//   - Absolute paths are rejected outright.
//   - Relative paths are joined to the CURRENT file's directory
//     (not scopeRoot) so subdirectory includes work as expected.
//   - The resolved path is checked against scopeRoot — anything
//     that resolves above the original file's directory is rejected.
//
// The two-step "resolve relative to current, check against scope"
// is what defends against nested traversal: a chain of -r references
// can never escape the original file's directory tree, even if each
// hop is locally valid.
func resolveAndParseInclude(includePath, currentFile, scopeRoot string, depth int) ([]manifest.Dep, error) {
	if filepath.IsAbs(includePath) {
		return nil, fmt.Errorf("%w: %s", ErrIncludeOutOfScope, includePath)
	}

	currentDir := filepath.Dir(currentFile)
	target := filepath.Clean(filepath.Join(currentDir, includePath))

	if !isWithinScope(target, scopeRoot) {
		return nil, fmt.Errorf("%w: %s (resolves to %s, outside %s)",
			ErrIncludeOutOfScope, includePath, target, scopeRoot)
	}

	return parseAtDepth(target, scopeRoot, depth)
}

// isWithinScope returns true when target is the same as root or
// a descendant. Uses filepath.Rel and rejects results that start
// with "..", which would mean target is above root.
func isWithinScope(target, root string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	if rel == ".." {
		return false
	}
	if strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

// stripIncludePrefix recognizes the short -r form. Returns the
// target path (whitespace-trimmed) and true when the line is an
// include directive, otherwise false.
//
// The long --requirement form is deferred — survey-able projects
// in the wild use the short form near-exclusively.
func stripIncludePrefix(line string) (string, bool) {
	rest, ok := strings.CutPrefix(line, "-r")
	if !ok {
		return "", false
	}
	// Must have whitespace after -r, otherwise this is some other
	// dash-prefixed token (e.g., a malformed token starting with -r).
	if rest == "" || (rest[0] != ' ' && rest[0] != '\t') {
		return "", false
	}
	return strings.TrimSpace(rest), true
}

// stripComment removes everything from the first '#' to end of line.
// requirements.txt comments are line-anchored: there's no quoting
// rule that would put a literal '#' in a non-comment context (env
// markers don't contain '#').
func stripComment(line string) string {
	if idx := strings.Index(line, "#"); idx >= 0 {
		return line[:idx]
	}
	return line
}

// splitContinuations joins backslash-newline continuations into
// single logical lines. Per pip's grammar, a line ending with `\`
// continues onto the next; the typical use is spreading multiple
// --hash directives across lines for a single requirement.
//
// Continuation is a raw concat (no inserted whitespace). Real-world
// continuation usage already includes whitespace before the `\` or
// at the start of the next line, so token boundaries are preserved.
// A pathological "split mid-token" continuation isn't supported
// — but isn't observed in practice.
func splitContinuations(content string) []string {
	var out []string
	var current strings.Builder

	// Strip a single trailing newline so we don't emit a spurious
	// empty line at the end. Multiple trailing newlines still produce
	// empty lines, which the caller filters.
	content = strings.TrimSuffix(content, "\n")

	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasSuffix(line, "\\") {
			current.WriteString(line[:len(line)-1])
			continue
		}
		current.WriteString(line)
		out = append(out, current.String())
		current.Reset()
	}
	if current.Len() > 0 {
		// File ended with a trailing backslash — treat what we've got as a final line.
		out = append(out, current.String())
	}
	return out
}

// parseRequirementLine extracts a Dep from a single non-empty,
// non-comment, non-include line of a requirements.txt file.
// Handles the requirements.txt-specific wrapping (pip directives
// like -e and --hash that aren't part of PEP 508) then delegates
// the actual dependency-specification parsing to
// parsePEP508Requirement so the same logic is reused by the
// pyproject.toml parser.
//
// Returns ok=false for lines that are pip-only directives
// (--hash on its own from a continuation, --index-url, etc.)
// rather than dep declarations.
func parseRequirementLine(line string) (manifest.Dep, bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return manifest.Dep{}, false
	}

	// Editable install: -e <spec>. pip-specific syntax not present
	// in PEP 508; classify as pypi-local and surface the verbatim
	// spec so the operator can see what was declared.
	if rest, ok := strings.CutPrefix(line, "-e "); ok {
		return manifest.Dep{
			Name:      "-e " + strings.TrimSpace(rest),
			Ecosystem: "pypi-local",
			Direct:    true,
		}, true
	}

	// Other dash-prefixed lines (--hash on its own from a
	// continuation, --index-url, etc.) aren't deps. pip-specific
	// preprocessing — caller of parsePEP508Requirement is supposed
	// to have stripped these by the time the spec reaches the
	// shared helper.
	if strings.HasPrefix(line, "-") {
		return manifest.Dep{}, false
	}

	return parsePEP508Requirement(line)
}
