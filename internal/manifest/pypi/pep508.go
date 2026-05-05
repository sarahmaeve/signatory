package pypi

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/sarahmaeve/signatory/internal/manifest"
)

// This file holds the PyPI-name-handling primitives that operate
// at the level of a single PEP 508 dependency specification —
// shared between the requirements.txt parser (where each spec is
// one line of input) and the upcoming pyproject.toml parser
// (where each spec is one string in a TOML array, both for PEP
// 621 [project] tables and PEP 735 [dependency-groups]).
//
// Strictness: parsePEP508Requirement implements the PRAGMATIC
// superset of PEP 508 that pip and the broader Python ecosystem
// actually accept — strict PEP 508 has only `name_req` (with
// version specifiers and markers) and `url_req` (the `name @ url`
// form), but real-world inputs commonly use VCS prefixes
// (`git+https://`, `hg+`, etc.) and bare URLs to wheels/sdists.
// Rejecting those would fail on real pyproject.toml files.
// Pip-flavored URL forms are classified as pypi-local with empty
// CanonicalURI, the same convention the requirements.txt parser
// uses.
//
// Names: per PEP 503, the canonical form for registry lookups is
// lowercase with runs of [-_.] collapsed to a single hyphen. This
// is the same regex PEP 735 mandates for dependency-group name
// normalization, so pep503Normalize is reused for both purposes.

// parsePEP508Requirement parses a single PEP 508 dependency
// specification (with pip-flavored extensions) into a Dep. The
// caller is responsible for any pip-specific preprocessing —
// stripping `-e`, `--hash`, comments, line continuations, `-r`
// recursion — before reaching this function.
//
// Returns ok=false for empty input. Malformed names parse but
// produce a Dep with empty CanonicalURI (defensive pattern; the
// invalid name still surfaces to the operator but no bad URI
// gets stamped into the store).
//
// Recognized forms:
//   - bare name: "requests"
//   - with version specifier(s): "requests==2.31.0",
//     "requests>=2.30,<3", "requests~=2.30"
//   - with extras: "requests[security]==2.31.0"
//   - with environment marker (stripped): "requests==2.31.0
//     ; python_version >= '3.10'"
//   - with trailing pip options (stripped): "requests==2.31.0
//     --hash=sha256:..."
//   - PEP 508 URL form: "requests @ https://..."
//   - pip-flavored URL/VCS prefix: "git+https://...",
//     "https://example.com/foo-1.0.whl", "file:///tmp/local-pkg"
func parsePEP508Requirement(spec string) (manifest.Dep, bool) {
	// Reject invalid UTF-8 early. Requirements come from files on
	// disk that could be binary garbage or maliciously encoded.
	// Passing invalid bytes through would stamp bad URIs into the
	// store.
	if !utf8.ValidString(spec) {
		return manifest.Dep{}, false
	}

	// Strip env marker: everything from the first ';' onward.
	// Markers can contain spaces and quoted values that confuse
	// the whitespace tokenization below; remove first.
	if idx := strings.Index(spec, ";"); idx >= 0 {
		spec = spec[:idx]
	}
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return manifest.Dep{}, false
	}

	fields := strings.Fields(spec)
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

	first := fields[0]

	// Pip-flavored URL/VCS forms.
	if isNonRegistrySpec(first) {
		return manifest.Dep{
			Name:      first,
			Ecosystem: "pypi-local",
			Direct:    true,
		}, true
	}

	// Standard form: <name>[<extras>][<version_specifiers>]
	name, version := splitNameAndVersion(first)
	canonicalName := stripExtras(name)

	// An empty canonical name means the spec was something like "!"
	// or "==" — operator chars with no actual package name. Reject.
	if canonicalName == "" {
		return manifest.Dep{}, false
	}

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

// pep503Normalize applies PEP 503 name normalization: lowercase
// the name and collapse runs of [_.-] to a single hyphen.
//
//	"Python_Dotenv" → "python-dotenv"
//	"python__dot..env" → "python-dot-env"
//
// Used in two places: for canonical URI construction
// ("pkg:pypi/<normalized>") and for PEP 735 dependency-group
// name comparisons. Both call sites need the same transform —
// PEP 735 explicitly cites the same rule.
func pep503Normalize(name string) string {
	if !utf8.ValidString(name) {
		return ""
	}
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
// than the public PyPI registry — VCS (git+, hg+, svn+, bzr+),
// URL (http(s):, ftp:), or local file (file:). Mirrors npm's
// isNonRegistrySpec shape. Comparing case-insensitively is
// defensive — VCS prefixes are conventionally lowercase but
// loose tooling accepts mixed.
//
// Not strict PEP 508 — the spec only defines `name @ url` for
// URL-shaped requirements. The prefixes here are pip-flavored
// extensions accepted by every Python packaging tool that reads
// requirements.txt or pyproject.toml dependency arrays.
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
