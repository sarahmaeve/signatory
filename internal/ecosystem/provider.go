package ecosystem

import "context"

// Dependency represents a single dependency parsed from a manifest.
type Dependency struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Pinned  bool   `json:"pinned"`
	Direct  bool   `json:"direct"`
	RepoURL string `json:"repo_url,omitempty"`
}

// Provider defines the interface for ecosystem-specific logic.
// Each implementation (npm, PyPI, Go modules, etc.) handles
// manifest parsing, dependency resolution, and package-to-repo mapping.
type Provider interface {
	// Name returns the ecosystem identifier (e.g., "npm", "pypi").
	Name() string

	// DetectManifest returns the manifest path if one exists in the given directory.
	DetectManifest(dir string) (string, bool)

	// ParseManifest reads a manifest file and returns all dependencies.
	ParseManifest(path string) ([]Dependency, error)

	// ResolveRepo attempts to map a package name to its source repository URL.
	ResolveRepo(ctx context.Context, packageName string) (string, error)
}
