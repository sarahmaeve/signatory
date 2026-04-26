// Package pypi provides parsers for Python (PyPI) dependency manifests.
//
// Status as of v0.1: the only implemented format is requirements.txt,
// exposed via ParseRequirements. Parsers for pyproject.toml and
// setup.py are not yet wired up; manifest.Detect will pick those
// files up but survey will fail at parse-dispatch until they land.
//
// requirements.txt has no project identity (no name, no version),
// so ParseRequirements returns just a dep list rather than the
// (ProjectInfo, []Dep) shape used by gomod and npm. A top-level
// pypi.Parse dispatcher will arrive with the pyproject.toml parser,
// when there's a project descriptor to dispatch from.
//
// Supported requirements.txt syntax (subset of pip's full grammar):
//
//   - Bare names, version specifiers (==, !=, <, <=, >, >=, ~=),
//     and multi-constraint forms ("requests>=2.30,<3").
//   - Extras: "requests[security]==2.31.0". Extras are preserved in
//     Dep.Name for traceability and stripped from Dep.CanonicalURI
//     (the package identity on PyPI).
//   - Environment markers ("; python_version >= '3.10'") are
//     stripped — signatory is a static analyzer, not an installer,
//     so a marker only gates *when* a dep is installed, not whether
//     the project depends on it.
//   - Hash directives (--hash=sha256:...) are stripped; capturing
//     them is a future signal opportunity.
//   - Comments (# to end of line) and blank lines are skipped.
//   - Backslash-newline continuation joins logical lines.
//   - Recursive includes via "-r other.txt" with safety rails:
//     absolute paths and parent-directory traversal are rejected
//     with ErrIncludeOutOfScope; chains exceeding maxIncludeDepth
//     fail with ErrIncludeDepthExceeded.
//   - Non-registry specs (-e, git+https, file:, https URL,
//     PEP 508 "name @ url" form) are tagged Ecosystem="pypi-local"
//     with empty CanonicalURI, mirroring the npm-local convention.
//
// Out of scope for v0.1 (deferred):
//
//   - --index-url / --extra-index-url / -i directives
//   - Constraints files (-c)
//   - Per-requirement options (--no-binary, --only-binary)
//   - --hash capture (parser strips them but doesn't surface them)
//   - Long-form --requirement (only the short -r form is recognized)
package pypi
