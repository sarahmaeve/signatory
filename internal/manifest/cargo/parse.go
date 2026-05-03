// Package cargo parses Rust/Cargo manifests (Cargo.toml + Cargo.lock)
// into the ecosystem-neutral types defined in internal/manifest.
//
// Uses github.com/BurntSushi/toml — the same library already vendored
// for PyPI's pyproject.toml parsing. Cargo.toml and Cargo.lock are
// both TOML; the library handles every TOML 1.0 edge case.
//
// Handles:
//   - Single-crate Cargo.toml with [dependencies], [dev-dependencies],
//     [build-dependencies]
//   - Platform-conditional deps: [target.'cfg(...)'.dependencies] and
//     variants
//   - Workspace roots: [workspace] with members list, walking each
//     member's Cargo.toml to produce a flat union of all deps
//   - Workspace dependency inheritance: { workspace = true }
//   - Non-registry deps: path and git deps → cargo-local
//   - Cargo.lock transitive resolution and graph extraction
package cargo

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/sarahmaeve/signatory/internal/manifest"
	"github.com/sarahmaeve/signatory/internal/profile"
)

// maxCargoTomlBytes caps untrusted Cargo.toml input. Real Cargo.toml
// files are well under 16 KiB even for large workspaces; 64 KiB rules
// out adversarial input without rejecting any legitimate file. Matches
// the PyPI pyproject.toml cap for consistency.
const maxCargoTomlBytes = 64 * 1024

// maxCargoLockBytes caps Cargo.lock input. Large workspace lockfiles
// (e.g., bevy with ~400 packages) can run to 100+ KiB. 1 MiB is
// generous and still prevents OOM from adversarial input.
const maxCargoLockBytes = 1024 * 1024

// Parse reads the Cargo.toml at path and returns ProjectInfo + Deps.
//
// If the Cargo.toml is a workspace root ([workspace] table present),
// all workspace members are walked and their deps unioned into a flat
// list. If it's a single-crate manifest ([package] present), only
// that crate's deps are returned.
//
// When a Cargo.lock exists alongside the manifest, transitive deps
// are extracted from it and included with Direct=false.
func Parse(path string) (manifest.ProjectInfo, []manifest.Dep, error) {
	if path == "" {
		return manifest.ProjectInfo{}, nil, fmt.Errorf("cargo: manifest path is empty")
	}

	data, err := readCapped(path, maxCargoTomlBytes)
	if err != nil {
		return manifest.ProjectInfo{}, nil, fmt.Errorf("cargo: %w", err)
	}

	var f cargoToml
	if _, err := toml.Decode(string(data), &f); err != nil {
		return manifest.ProjectInfo{}, nil, fmt.Errorf("cargo: parse %q: %w", path, err)
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return manifest.ProjectInfo{}, nil, fmt.Errorf("cargo: resolve %q: %w", path, err)
	}

	info := manifest.ProjectInfo{
		ManifestPath: absPath,
		Ecosystem:    "cargo",
	}

	var directDeps []manifest.Dep

	if f.Workspace != nil {
		// Workspace root: walk members and union deps.
		info.EcoVersion = extractWorkspaceRustVersion(&f)
		directDeps, err = parseWorkspaceMembers(filepath.Dir(absPath), f.Workspace)
		if err != nil {
			return manifest.ProjectInfo{}, nil, err
		}
	} else if f.Package != nil {
		// Single-crate manifest.
		info.Name = f.Package.Name
		info.EcoVersion = extractRustVersion(f.Package)
		directDeps = extractDeps(&f)
	} else {
		return manifest.ProjectInfo{}, nil, fmt.Errorf(
			"cargo: %q has neither [package] nor [workspace] table", path)
	}

	// Attempt to resolve transitive deps from Cargo.lock.
	lockPath := filepath.Join(filepath.Dir(absPath), "Cargo.lock")
	allDeps := mergeWithLockfile(directDeps, lockPath)

	return info, allDeps, nil
}

// ParseGraph extracts the transitive dependency graph from a
// Cargo.lock file. Unlike Go's ParseGraph (which shells out to
// `go mod graph`), this is pure TOML parsing — Cargo.lock contains
// explicit parent→child dependency edges.
//
// Returns manifest.ErrGraphUnavailable when the lockfile doesn't
// exist or can't be parsed.
func ParseGraph(path string) (manifest.Graph, error) {
	if path == "" {
		return manifest.Graph{}, fmt.Errorf("%w: path is empty",
			manifest.ErrGraphUnavailable)
	}

	lock, err := parseLockfile(path)
	if err != nil {
		return manifest.Graph{}, fmt.Errorf("%w: %w",
			manifest.ErrGraphUnavailable, err)
	}

	return buildGraph(lock)
}

// --- TOML structures ---

// cargoToml models the subset of Cargo.toml signatory reads.
type cargoToml struct {
	Package   *packageTable   `toml:"package"`
	Workspace *workspaceTable `toml:"workspace"`

	// Dep tables at the crate level.
	Dependencies      map[string]any `toml:"dependencies"`
	DevDependencies   map[string]any `toml:"dev-dependencies"`
	BuildDependencies map[string]any `toml:"build-dependencies"`

	// Platform-conditional deps. The TOML key is
	// `target.'cfg(...)'.dependencies` etc. BurntSushi/toml decodes
	// this as a map from the target spec string to a struct with
	// the same dep-table fields.
	Target map[string]targetDeps `toml:"target"`
}

type packageTable struct {
	Name        string `toml:"name"`
	Version     string `toml:"version"`
	Edition     string `toml:"edition"`
	RustVersion any    `toml:"rust-version"` // string or {workspace = true}
}

type workspaceTable struct {
	Members      []string       `toml:"members"`
	Dependencies map[string]any `toml:"dependencies"`
	Package      *workspacePkg  `toml:"package"`
}

type workspacePkg struct {
	RustVersion string `toml:"rust-version"`
}

type targetDeps struct {
	Dependencies      map[string]any `toml:"dependencies"`
	DevDependencies   map[string]any `toml:"dev-dependencies"`
	BuildDependencies map[string]any `toml:"build-dependencies"`
}

// --- dep extraction ---

// extractDeps pulls all direct deps from a single-crate cargoToml.
func extractDeps(f *cargoToml) []manifest.Dep {
	var deps []manifest.Dep
	deps = append(deps, parseDepsMap(f.Dependencies)...)
	deps = append(deps, parseDepsMap(f.DevDependencies)...)
	deps = append(deps, parseDepsMap(f.BuildDependencies)...)

	// Platform-conditional deps.
	for _, td := range f.Target {
		deps = append(deps, parseDepsMap(td.Dependencies)...)
		deps = append(deps, parseDepsMap(td.DevDependencies)...)
		deps = append(deps, parseDepsMap(td.BuildDependencies)...)
	}

	return deps
}

// parseDepsMap converts a TOML dep map (where values can be strings
// or inline tables) into manifest.Dep entries.
//
// Cargo.toml dep entries have three forms:
//
//	serde = "1.0"                           # string shorthand
//	serde = { version = "1.0", ... }        # inline table with version
//	local = { path = "../local" }           # path dep (non-registry)
//	forked = { git = "https://..." }        # git dep (non-registry)
//	inherited = { workspace = true }        # workspace inheritance
func parseDepsMap(m map[string]any) []manifest.Dep {
	if len(m) == 0 {
		return nil
	}

	deps := make([]manifest.Dep, 0, len(m))
	for name, val := range m {
		d := manifest.Dep{
			Name:      name,
			Direct:    true,
			Ecosystem: "cargo",
		}

		switch v := val.(type) {
		case string:
			// Simple version string: serde = "1.0"
			d.Version = v
			d.CanonicalURI = profile.CanonicalPackageURI("cargo", name)

		case map[string]any:
			// Inline table. Check for non-registry indicators first.
			if _, hasPath := v["path"]; hasPath {
				d.Ecosystem = "cargo-local"
				// CanonicalURI intentionally empty for local deps.
				deps = append(deps, d)
				continue
			}
			if _, hasGit := v["git"]; hasGit {
				d.Ecosystem = "cargo-local"
				deps = append(deps, d)
				continue
			}
			// workspace = true means the version comes from
			// [workspace.dependencies]. We can't resolve it here
			// (the workspace table isn't in scope at this level),
			// but we still emit the dep with whatever version we can
			// find. The workspace walk resolves versions separately.
			if ws, ok := v["workspace"]; ok {
				if b, isBool := ws.(bool); isBool && b {
					// Workspace dep — version resolved from workspace table.
					// For now, emit without version; the workspace walk
					// will fill it in if available.
					d.CanonicalURI = profile.CanonicalPackageURI("cargo", name)
					deps = append(deps, d)
					continue
				}
			}
			// Registry dep with version in table.
			if ver, ok := v["version"]; ok {
				if vs, isStr := ver.(string); isStr {
					d.Version = vs
				}
			}
			d.CanonicalURI = profile.CanonicalPackageURI("cargo", name)

		default:
			// Unknown shape — emit with what we have.
			d.CanonicalURI = profile.CanonicalPackageURI("cargo", name)
		}

		deps = append(deps, d)
	}
	return deps
}

// --- workspace handling ---

// parseWorkspaceMembers walks each workspace member's Cargo.toml
// and returns the flat union of all members' direct deps.
func parseWorkspaceMembers(rootDir string, ws *workspaceTable) ([]manifest.Dep, error) {
	memberDirs, err := expandMembers(rootDir, ws.Members)
	if err != nil {
		return nil, fmt.Errorf("cargo workspace: %w", err)
	}

	seen := make(map[string]bool)
	var allDeps []manifest.Dep

	for _, dir := range memberDirs {
		memberPath := filepath.Join(dir, "Cargo.toml")
		data, err := readCapped(memberPath, maxCargoTomlBytes)
		if err != nil {
			// Member listed but Cargo.toml doesn't exist — skip
			// gracefully rather than failing the whole parse. This can
			// happen with conditional workspace members.
			continue
		}

		var mf cargoToml
		if _, err := toml.Decode(string(data), &mf); err != nil {
			return nil, fmt.Errorf("cargo workspace member %q: %w", memberPath, err)
		}

		memberDeps := extractDeps(&mf)

		// Resolve workspace = true deps from workspace.dependencies.
		for i := range memberDeps {
			if memberDeps[i].Version == "" && memberDeps[i].Ecosystem == "cargo" {
				memberDeps[i].Version = resolveWorkspaceVersion(memberDeps[i].Name, ws)
			}
		}

		// Union: deduplicate by canonical URI (first seen wins).
		for _, d := range memberDeps {
			key := d.Name + "@" + d.Version
			if d.CanonicalURI != "" {
				key = d.CanonicalURI
			}
			if !seen[key] {
				seen[key] = true
				allDeps = append(allDeps, d)
			}
		}
	}

	return allDeps, nil
}

// expandMembers expands workspace member patterns (which can contain
// globs) to absolute directory paths.
func expandMembers(rootDir string, patterns []string) ([]string, error) {
	var dirs []string
	for _, pattern := range patterns {
		absPattern := filepath.Join(rootDir, pattern)
		matches, err := filepath.Glob(absPattern)
		if err != nil {
			return nil, fmt.Errorf("expand member pattern %q: %w", pattern, err)
		}
		if len(matches) == 0 {
			// Literal path that doesn't match a glob — try it as-is.
			dirs = append(dirs, absPattern)
		} else {
			dirs = append(dirs, matches...)
		}
	}
	return dirs, nil
}

// resolveWorkspaceVersion looks up a dep's version from
// [workspace.dependencies]. Returns empty if not found.
func resolveWorkspaceVersion(name string, ws *workspaceTable) string {
	if ws == nil || ws.Dependencies == nil {
		return ""
	}
	val, ok := ws.Dependencies[name]
	if !ok {
		return ""
	}
	switch v := val.(type) {
	case string:
		return v
	case map[string]any:
		if ver, ok := v["version"]; ok {
			if vs, isStr := ver.(string); isStr {
				return vs
			}
		}
	}
	return ""
}

// --- rust-version extraction ---

func extractRustVersion(pkg *packageTable) string {
	if pkg == nil {
		return ""
	}
	switch v := pkg.RustVersion.(type) {
	case string:
		return v
	case map[string]any:
		// { workspace = true } — resolved from workspace table by
		// the caller if applicable.
		return ""
	}
	return ""
}

func extractWorkspaceRustVersion(f *cargoToml) string {
	if f.Workspace != nil && f.Workspace.Package != nil {
		return f.Workspace.Package.RustVersion
	}
	return ""
}

// --- lockfile handling ---

// cargoLock models the subset of Cargo.lock signatory reads.
type cargoLock struct {
	Package []lockPackage `toml:"package"`
}

type lockPackage struct {
	Name         string   `toml:"name"`
	Version      string   `toml:"version"`
	Source       string   `toml:"source"`
	Checksum     string   `toml:"checksum"`
	Dependencies []string `toml:"dependencies"`
}

// parseLockfile reads and decodes a Cargo.lock.
func parseLockfile(path string) (*cargoLock, error) {
	data, err := readCapped(path, maxCargoLockBytes)
	if err != nil {
		return nil, err
	}

	var lock cargoLock
	if _, err := toml.Decode(string(data), &lock); err != nil {
		return nil, fmt.Errorf("parse %q: %w", path, err)
	}
	return &lock, nil
}

// mergeWithLockfile enriches direct deps with transitive deps from
// Cargo.lock if it exists. Direct deps retain Direct=true; lockfile-
// only packages become Direct=false.
func mergeWithLockfile(directDeps []manifest.Dep, lockPath string) []manifest.Dep {
	lock, err := parseLockfile(lockPath)
	if err != nil {
		// No lockfile or parse error — return direct deps only.
		return directDeps
	}

	// Build set of direct dep names for fast lookup.
	directNames := make(map[string]bool, len(directDeps))
	for _, d := range directDeps {
		directNames[d.Name] = true
	}

	// The root package (the crate itself) is the one without a
	// "source" field — local workspace crates also lack source.
	// We identify the root(s) as any package whose name matches a
	// direct dep's membership OR has no source field AND is not a
	// direct dep.
	rootNames := make(map[string]bool)
	for _, pkg := range lock.Package {
		if pkg.Source == "" && !directNames[pkg.Name] {
			rootNames[pkg.Name] = true
		}
	}

	// Add transitive deps from lockfile.
	var allDeps []manifest.Dep
	allDeps = append(allDeps, directDeps...)

	seen := make(map[string]bool, len(directDeps))
	for _, d := range directDeps {
		seen[d.Name] = true
	}

	for _, pkg := range lock.Package {
		if seen[pkg.Name] || rootNames[pkg.Name] {
			continue
		}
		// Update version on direct deps if lockfile has a resolved version.
		// (Direct deps already added above.)

		// This is a transitive dep.
		seen[pkg.Name] = true
		allDeps = append(allDeps, manifest.Dep{
			Name:         pkg.Name,
			CanonicalURI: profile.CanonicalPackageURI("cargo", pkg.Name),
			Version:      pkg.Version,
			Direct:       false,
			Ecosystem:    "cargo",
		})
	}

	return allDeps
}

// buildGraph converts a parsed Cargo.lock into a manifest.Graph.
func buildGraph(lock *cargoLock) (manifest.Graph, error) {
	if lock == nil || len(lock.Package) == 0 {
		return manifest.Graph{}, fmt.Errorf("empty lockfile")
	}

	g := manifest.Graph{}

	// The root package is the first one without a source field.
	for _, pkg := range lock.Package {
		if pkg.Source == "" {
			g.RootURI = profile.CanonicalPackageURI("cargo", pkg.Name)
			break
		}
	}

	for _, pkg := range lock.Package {
		parentURI := profile.CanonicalPackageURI("cargo", pkg.Name)
		for _, depStr := range pkg.Dependencies {
			childName := parseLockDepRef(depStr)
			if childName == "" {
				continue
			}
			childURI := profile.CanonicalPackageURI("cargo", childName)
			g.Edges = append(g.Edges, manifest.Edge{
				Parent: parentURI,
				Child:  childURI,
			})
		}
	}

	if len(g.Edges) == 0 {
		return manifest.Graph{}, fmt.Errorf("no edges in Cargo.lock")
	}

	return g, nil
}

// parseLockDepRef extracts the crate name from a Cargo.lock
// dependency reference. The format is either:
//   - "name version" (e.g., "serde_derive 1.0.219")
//   - "name" (less common, older formats)
func parseLockDepRef(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	name, _, _ := strings.Cut(ref, " ")
	return name
}

// --- utilities ---

// readCapped reads a file up to maxBytes. Returns an error if the
// file exceeds the cap or can't be read.
func readCapped(path string, maxBytes int64) ([]byte, error) {
	fh, err := os.Open(path) //nolint:gosec // G304: caller-supplied manifest path
	if err != nil {
		return nil, fmt.Errorf("open %q: %w", path, err)
	}
	defer fh.Close() //nolint:errcheck // read-only

	data, err := io.ReadAll(io.LimitReader(fh, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", path, err)
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("file %q exceeds %d-byte cap", path, maxBytes)
	}
	return data, nil
}

// writeFile is a test helper — writes content to path.
func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600) //nolint:gosec // G306: test helper
}
