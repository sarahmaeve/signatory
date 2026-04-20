package npm

import (
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/sarahmaeve/signatory/internal/manifest"
)

// Parse reads the package.json at path and returns ProjectInfo plus
// a []Dep mirroring the ecosystem-neutral shape gomod emits. If a
// package-lock.json (v2 or v3) lives alongside package.json, its
// resolved versions replace package.json's range specs for direct
// deps and the transitive tree is emitted with Direct=false.
//
// Non-registry dep specs (file:, git:, github:, http(s):, npm:alias,
// workspace:) are emitted with Ecosystem="npm-local" and an empty
// CanonicalURI — the same pattern gomod's go-local-replace uses.
// There's no registry entry to analyze, so a survey renderer should
// present these as "local, not analyzable remotely" rather than try
// to look them up.
func Parse(path string) (manifest.ProjectInfo, []manifest.Dep, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: operator-supplied manifest path
	if err != nil {
		return manifest.ProjectInfo{}, nil, fmt.Errorf("read %q: %w", path, err)
	}

	var pkg packageJSON
	if err := json.Unmarshal(data, &pkg); err != nil {
		return manifest.ProjectInfo{}, nil, fmt.Errorf("parse %q: %w", path, err)
	}

	info := manifest.ProjectInfo{
		Name:         pkg.Name,
		ManifestPath: path,
		Ecosystem:    "npm",
	}
	if pkg.Engines != nil {
		info.EcoVersion = pkg.Engines.Node
	}

	// Flatten all four dep classes into a single "direct" map. v0.1
	// doesn't distinguish dev/peer/optional/prod on the output; the
	// Direct bool is the only dimension the shared manifest.Dep type
	// exposes today. A future enhancement could carry the class on
	// the Ecosystem field ("npm-dev" etc.) if surveys want to filter.
	direct := make(map[string]string, len(pkg.Dependencies)+
		len(pkg.DevDependencies)+len(pkg.PeerDependencies)+
		len(pkg.OptionalDependencies))
	maps.Copy(direct, pkg.Dependencies)
	maps.Copy(direct, pkg.DevDependencies)
	maps.Copy(direct, pkg.PeerDependencies)
	maps.Copy(direct, pkg.OptionalDependencies)

	// Look for package-lock.json adjacent to package.json. Absence
	// is normal (projects that check in only package.json, or first-
	// time-install states) — we emit direct deps only in that case.
	lockfilePath := filepath.Join(filepath.Dir(path), "package-lock.json")
	resolvedVersions, lockfileErr := parseLockfile(lockfilePath)
	if lockfileErr != nil && !os.IsNotExist(lockfileErr) {
		return manifest.ProjectInfo{}, nil, fmt.Errorf("parse lockfile %q: %w", lockfilePath, lockfileErr)
	}

	deps := make([]manifest.Dep, 0, len(direct)+len(resolvedVersions))

	// Direct deps, with locked version substituted when available.
	for name, spec := range direct {
		d := buildDep(name, spec, true)
		if locked, ok := resolvedVersions[name]; ok && d.Ecosystem == "npm" {
			d.Version = locked
		}
		deps = append(deps, d)
	}

	// Transitive deps: anything in the lockfile that isn't a direct.
	for name, version := range resolvedVersions {
		if _, isDirect := direct[name]; isDirect {
			continue
		}
		d := buildDep(name, version, false)
		// The name came from the lockfile itself — never a non-
		// registry spec — so d.Ecosystem is always "npm" here.
		// Keep the version-from-lockfile write explicit for clarity.
		d.Version = version
		deps = append(deps, d)
	}

	// Stable iteration order. Map iteration in Go is randomized;
	// without a sort, repeated runs produce differently-ordered
	// Dep slices and make tests (and diff-style comparisons) noisy.
	slices.SortStableFunc(deps, func(a, b manifest.Dep) int {
		return cmp.Compare(a.Name, b.Name)
	})

	return info, deps, nil
}

// packageJSON models the subset of package.json signatory reads.
// Unknown fields are ignored by default — lenient mode is correct
// here because the schema is open (npm allows arbitrary scripts,
// custom configuration blocks, bundler-specific keys). Strict mode
// would fail on fields we don't model, which is most fields.
type packageJSON struct {
	Name                 string            `json:"name"`
	Version              string            `json:"version"`
	Engines              *engines          `json:"engines,omitempty"`
	Dependencies         map[string]string `json:"dependencies"`
	DevDependencies      map[string]string `json:"devDependencies"`
	PeerDependencies     map[string]string `json:"peerDependencies"`
	OptionalDependencies map[string]string `json:"optionalDependencies"`
}

type engines struct {
	Node string `json:"node"`
}

// packageLock models the subset of package-lock.json v2/v3 the
// parser reads. The `packages` map is keyed by path
// ("node_modules/<name>" or nested equivalents); we only consult
// top-level entries to produce the flat resolved-version map.
type packageLock struct {
	LockfileVersion int                         `json:"lockfileVersion"`
	Packages        map[string]packageLockEntry `json:"packages"`
}

type packageLockEntry struct {
	Version string `json:"version"`
}

// parseLockfile reads a package-lock.json and returns a map from
// package name to resolved version for the TOP-LEVEL node_modules
// entries. Nested entries (node_modules/x/node_modules/y, which
// indicate npm could not dedupe y to the root) are skipped — they
// represent version conflicts that the survey's flat view doesn't
// currently model.
//
// Returns os.ErrNotExist (unwrapped via errors.Is) when the lockfile
// doesn't exist, so the caller can distinguish "no lockfile" from
// "lockfile is corrupted." Other errors are wrapped.
//
// lockfileVersion 1 (pre-npm 7) is not supported — returns a
// typed error the caller may choose to surface or downgrade to
// "treat as absent lockfile." v0.1 surfaces it as a parse error.
func parseLockfile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: operator-supplied manifest directory
	if err != nil {
		return nil, err
	}

	var lock packageLock
	if err := json.Unmarshal(data, &lock); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if lock.LockfileVersion != 2 && lock.LockfileVersion != 3 {
		return nil, fmt.Errorf("unsupported lockfileVersion %d (signatory v0.1 supports 2 and 3)",
			lock.LockfileVersion)
	}

	resolved := make(map[string]string)
	for pkgPath, entry := range lock.Packages {
		if pkgPath == "" {
			// The empty-key entry describes the root project
			// itself — not a dependency to emit.
			continue
		}
		if !isRootLevelLockPath(pkgPath) {
			continue
		}
		name := strings.TrimPrefix(pkgPath, "node_modules/")
		if name == "" || entry.Version == "" {
			continue
		}
		resolved[name] = entry.Version
	}
	return resolved, nil
}

// isRootLevelLockPath reports whether a lockfile packages-map key
// represents a top-level node_modules/<name> entry (scoped names
// included) rather than a nested one.
//
//	node_modules/express                          → true
//	node_modules/@types/node                      → true
//	node_modules/express/node_modules/body-parser → false (nested)
//
// Detection: root-level paths have exactly one "node_modules/"
// occurrence; nested paths have two or more.
func isRootLevelLockPath(path string) bool {
	if !strings.HasPrefix(path, "node_modules/") {
		return false
	}
	return strings.Count(path, "node_modules/") == 1
}

// buildDep constructs a Dep for an npm dependency, classifying
// registry vs non-registry specs and setting the canonical URI
// accordingly. Package names that don't match npm's name grammar
// (e.g., a lockfile key parsed to "../../etc" by a malicious or
// malformed lockfile) get the "npm" ecosystem slug but NO canonical
// URI — the name is still surfaced to the operator but no bad URI
// is stamped into the store or downstream lookups.
func buildDep(name, spec string, direct bool) manifest.Dep {
	d := manifest.Dep{
		Name:    name,
		Version: spec,
		Direct:  direct,
	}
	if isNonRegistrySpec(spec) {
		d.Ecosystem = "npm-local"
		// CanonicalURI left empty: survey renders these specially.
		return d
	}
	d.Ecosystem = "npm"
	if isValidPackageName(name) {
		d.CanonicalURI = "pkg:npm/" + name
	}
	// Invalid names leave CanonicalURI empty — prevents stamping
	// pkg:npm/../../etc (or similar lockfile-traversal-names) into
	// persisted state. The Dep still lands so surveys see the
	// malformed entry.
	return d
}

// Package-name grammar, per the npm spec's practical subset. Kept
// deliberately identical to the validator in
// internal/signal/registry/npm/client.go — a mismatch between the
// two would let the manifest parser produce URIs the registry
// collector would refuse. Drift detection: the validator sets
// exercised by both packages assert overlapping accepts and rejects.
var (
	npmUnscopedName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	npmScopedName   = regexp.MustCompile(`^@[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

const maxNpmNameLength = 214

// isValidPackageName returns true when name conforms to the npm
// published name grammar. Matches the validator in
// internal/signal/registry/npm — see note above.
func isValidPackageName(name string) bool {
	if name == "" || len(name) > maxNpmNameLength {
		return false
	}
	if strings.HasPrefix(name, "@") {
		return npmScopedName.MatchString(name)
	}
	return npmUnscopedName.MatchString(name)
}

// isNonRegistrySpec returns true when spec points somewhere other
// than the public npm registry. Covers the documented dep-spec
// prefixes that npm and Yarn accept for local paths, git sources,
// tarballs, aliases, and workspace protocols.
//
// Comparing case-insensitively is defensive: specs are conventionally
// lowercase but the parser accepts mixed case in practice.
func isNonRegistrySpec(spec string) bool {
	s := strings.ToLower(strings.TrimSpace(spec))
	for _, prefix := range []string{
		"file:",
		"git:",
		"git+",
		"github:",
		"gitlab:",
		"bitbucket:",
		"http:",
		"https:",
		"npm:", // alias spec: "npm:<realname>@<version>"
		"workspace:",
		"portal:", // Yarn 2+ workspace portals
		"link:",
	} {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}
