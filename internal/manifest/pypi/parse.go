package pypi

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/sarahmaeve/signatory/internal/manifest"
)

// ErrPyProjectTOMLNotYetSupported signals that a pyproject.toml
// file was passed to Parse but the parser hasn't landed yet. The
// detect layer (manifest.Detect) recognizes pyproject.toml as a
// Python project marker, so survey will route here on a modern
// Python repo. Until the parser ships, callers should fall back
// to a sibling requirements.txt if the project provides one.
//
// Wrapped with the file path for context so survey can render a
// useful "we saw this file but can't read it yet" message.
var ErrPyProjectTOMLNotYetSupported = errors.New("pyproject.toml parsing not yet implemented")

// ErrSetupPyNotParseable signals that a setup.py was passed but
// signatory refuses to parse it. setup.py is Python source code:
// a deterministic dep extraction would require executing arbitrary
// code at import time (setup.py can read os.environ, hit the
// network, conditionally compute deps from the install host, etc.).
// The error message redirects the user to pyproject.toml or
// requirements.txt, both of which are statically parseable.
var ErrSetupPyNotParseable = errors.New("setup.py is Python source code and cannot be safely parsed without execution; provide pyproject.toml or requirements.txt for dep enumeration")

// Parse is the top-level manifest dispatcher for the PyPI ecosystem.
// It routes by the file's basename to the appropriate parser:
//
//   - requirements.txt → ParseRequirements (deps-only). The returned
//     ProjectInfo has ManifestPath and Ecosystem populated; Name and
//     EcoVersion are empty because requirements.txt carries no
//     project identity.
//   - pyproject.toml → ErrPyProjectTOMLNotYetSupported.
//   - setup.py → ErrSetupPyNotParseable.
//   - any other filename → an unrecognized-format error naming the file.
//
// ManifestPath in the returned ProjectInfo is always absolute,
// even when the caller passes a relative path — callers that key
// off ManifestPath (the store, audit logs) need stability across
// cwd changes.
func Parse(path string) (manifest.ProjectInfo, []manifest.Dep, error) {
	base := filepath.Base(path)
	switch base {
	case "requirements.txt":
		return parseRequirementsAsManifest(path)
	case "pyproject.toml":
		return manifest.ProjectInfo{}, nil,
			fmt.Errorf("%w: %s", ErrPyProjectTOMLNotYetSupported, path)
	case "setup.py":
		return manifest.ProjectInfo{}, nil,
			fmt.Errorf("%w: %s", ErrSetupPyNotParseable, path)
	default:
		return manifest.ProjectInfo{}, nil, fmt.Errorf(
			"pypi: unrecognized manifest filename %q "+
				"(supported: requirements.txt; recognized but not yet parsed: pyproject.toml, setup.py)",
			base)
	}
}

// parseRequirementsAsManifest wraps ParseRequirements with a sparse
// ProjectInfo so the dispatcher's return shape matches the gomod
// and npm Parse functions. The deps slice is whatever the
// underlying parser produced — errors propagate verbatim.
func parseRequirementsAsManifest(path string) (manifest.ProjectInfo, []manifest.Dep, error) {
	deps, err := ParseRequirements(path)
	if err != nil {
		return manifest.ProjectInfo{}, nil, err
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return manifest.ProjectInfo{}, nil, fmt.Errorf("resolve %q: %w", path, err)
	}
	return manifest.ProjectInfo{
		ManifestPath: absPath,
		Ecosystem:    "pypi",
		// Name and EcoVersion intentionally empty.
	}, deps, nil
}
