package pypi

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/sarahmaeve/signatory/internal/manifest"
)

// maxIncludeDepth caps recursion through -r references. A
// requirements.txt that includes more than this many nested levels
// is either malformed or attempting to defeat the parser; either way,
// stopping with a clear error beats blowing the stack.
const maxIncludeDepth = 5

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

	data, err := os.ReadFile(path) //nolint:gosec // G304: caller-supplied or include-resolved path validated against scopeRoot before reaching this read
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", path, err)
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
// non-comment, non-include line. Returns ok=false for lines that
// are pip-only directives (--hash on its own, --index-url, etc.)
// rather than dep declarations.
func parseRequirementLine(line string) (manifest.Dep, bool) {
	// Strip env marker: everything from the first ';' onward.
	// Markers are stripped before tokenization because they can
	// contain spaces and quoted values that confuse the simple
	// whitespace tokenization below.
	if idx := strings.Index(line, ";"); idx >= 0 {
		line = line[:idx]
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return manifest.Dep{}, false
	}

	// Editable install: -e <spec>. Always classified as pypi-local.
	if rest, ok := strings.CutPrefix(line, "-e "); ok {
		return manifest.Dep{
			Name:      "-e " + strings.TrimSpace(rest),
			Ecosystem: "pypi-local",
			Direct:    true,
		}, true
	}

	// Other dash-prefixed lines (--hash on its own from a
	// continuation, --index-url, etc.) aren't deps.
	if strings.HasPrefix(line, "-") {
		return manifest.Dep{}, false
	}

	fields := strings.Fields(line)
	if len(fields) == 0 {
		return manifest.Dep{}, false
	}

	// PEP 508 URL form: "name @ url"
	if len(fields) >= 3 && fields[1] == "@" {
		return manifest.Dep{
			Name:      strings.Join(fields[:3], " "),
			Ecosystem: "pypi-local",
			Direct:    true,
		}, true
	}

	spec := fields[0]

	// VCS, URL, or local file?
	if isNonRegistrySpec(spec) {
		return manifest.Dep{
			Name:      spec,
			Ecosystem: "pypi-local",
			Direct:    true,
		}, true
	}

	// Standard form: <name>[<extras>][<version_specifiers>]
	name, version := splitNameAndVersion(spec)
	canonicalName := stripExtras(name)

	dep := manifest.Dep{
		Name:      name,
		Version:   version,
		Direct:    true,
		Ecosystem: "pypi",
	}
	if isValidPEP508Name(canonicalName) {
		dep.CanonicalURI = "pkg:pypi/" + pep503Normalize(canonicalName)
	}
	// Invalid names get the pypi ecosystem slug but no CanonicalURI
	// — same defensive pattern as npm's malformed-name handling.
	return dep, true
}

// splitNameAndVersion separates a spec into name (possibly with
// extras) and version specifier (with operator preserved).
// Skips over the [extras] block when scanning for the operator
// boundary so "requests[security]==2.31.0" splits at the '=', not
// inside the bracket.
func splitNameAndVersion(spec string) (name, version string) {
	inExtras := false
	for i, r := range spec {
		switch r {
		case '[':
			inExtras = true
			continue
		case ']':
			inExtras = false
			continue
		}
		if inExtras {
			continue
		}
		switch r {
		case '<', '>', '=', '!', '~':
			return spec[:i], spec[i:]
		}
	}
	return spec, ""
}

// stripExtras drops the [extras] suffix from a name.
// "requests[security]" → "requests"; "requests" → "requests".
func stripExtras(name string) string {
	if idx := strings.Index(name, "["); idx >= 0 {
		return name[:idx]
	}
	return name
}

// pep503Normalize applies PEP 503 name normalization for use in
// the canonical URI: lowercase the name and collapse runs of
// [_.-] to a single hyphen.
//
//	"Python_Dotenv" → "python-dotenv"
//	"python__dot..env" → "python-dot-env"
//
// The canonical form is what the registry resolves equivalent
// names to; signatory uses it to ensure that pkg:pypi/Requests
// and pkg:pypi/requests don't fragment the store.
func pep503Normalize(name string) string {
	name = strings.ToLower(name)
	var sb strings.Builder
	sb.Grow(len(name))
	inSep := false
	for _, r := range name {
		if r == '_' || r == '.' || r == '-' {
			if !inSep {
				sb.WriteRune('-')
				inSep = true
			}
			continue
		}
		sb.WriteRune(r)
		inSep = false
	}
	return sb.String()
}

// pep508Name is the PEP 508 distribution name grammar
// (case-insensitive): an alphanumeric character, optionally
// followed by a sequence of alphanumerics, dots, hyphens, and
// underscores, ending with an alphanumeric.
//
// Names that don't match get no CanonicalURI — defensive against
// stamping pkg:pypi/.. or pkg:pypi/<empty> into the store.
var pep508Name = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]*[A-Za-z0-9])?$`)

// isValidPEP508Name reports whether name conforms to the PEP 508
// distribution name grammar.
func isValidPEP508Name(name string) bool {
	return pep508Name.MatchString(name)
}

// isNonRegistrySpec returns true when spec points somewhere other
// than the public PyPI registry. Mirrors npm's isNonRegistrySpec
// shape. Comparing case-insensitively is defensive — VCS prefixes
// are conventionally lowercase but loose tooling accepts mixed.
func isNonRegistrySpec(spec string) bool {
	s := strings.ToLower(spec)
	for _, prefix := range []string{
		"git+",
		"hg+",
		"svn+",
		"bzr+",
		"http://",
		"https://",
		"ftp://",
		"file:",
	} {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}
