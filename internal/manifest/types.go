package manifest

import "errors"

// Edge represents one directed dependency relationship in the
// transitive module graph: Parent declares Child as a dependency.
// Both fields are canonical URIs in signatory's scheme (the same
// form as Dep.CanonicalURI), with any ecosystem-native version
// suffix stripped — version selection is handled by the ecosystem
// resolver, but reachability questions ("does any path from a
// direct lead to this indirect?") are URI-level.
type Edge struct {
	Parent string
	Child  string
}

// Graph is the full transitive dependency graph for a parsed
// manifest. Produced by per-ecosystem ParseGraph functions
// (currently gomod only; npm follow-up). Used by survey to
// compute reachability buckets — which indirect deps are reachable
// only via resolved directs (defer-safe) versus reachable via at
// least one unresolved direct (await an unresolved direct).
//
// Edges are order-preserved from the source where the source has
// a stable order; consumers should not assume any particular
// ordering, but reproducibility helps with tests and diffs.
type Graph struct {
	// RootURI is the canonical URI of the module the manifest
	// declares — the project itself. Edges rooted here represent
	// the direct dependencies; edges deeper in the graph
	// represent transitives.
	RootURI string

	// Edges is the directed edge list. A module appearing only
	// as a Parent has no further dependencies in the graph; a
	// module appearing only as a Child is a leaf dependency.
	Edges []Edge
}

// ErrGraphUnavailable signals that a parser cannot produce a
// dependency graph for this manifest in this environment.
// Callers (notably survey) should treat it as non-fatal and fall
// back to behavior that doesn't require graph data.
//
// Reasons a parser may return this:
//
//   - The ecosystem doesn't have graph extraction implemented yet
//     (npm in v0.1; PyPI / cargo / etc. when they land).
//   - The required external tooling isn't available (e.g., the Go
//     toolchain isn't on PATH for `go mod graph`).
//   - The tooling failed (e.g., go.sum is missing modules).
//
// Wrapped errors carry the underlying cause via errors.Unwrap;
// callers that just want to know "no graph available" can use
// errors.Is(err, ErrGraphUnavailable).
var ErrGraphUnavailable = errors.New("dependency graph unavailable for this manifest")

// Dep is one entry from a dependency manifest, normalized to the
// shape signatory's store uses for entity lookups.
type Dep struct {
	// Name is the ecosystem-native identifier as it appears in the
	// manifest. Examples:
	//   - Go: "github.com/alecthomas/kong", "gopkg.in/yaml.v3"
	//   - npm (future): "express", "@types/node"
	//   - PyPI (future): "requests", "python-dotenv"
	//
	// Preserved verbatim from the manifest so error messages and
	// action items can reference exactly what the user typed.
	Name string

	// CanonicalURI is the signatory-internal identifier the store
	// uses as entity.canonical_uri. Resolution rule (v0.1, Go):
	//   - Go paths under github.com/ → "repo:github/owner/repo"
	//   - Go paths elsewhere        → "pkg:go/<verbatim-path>"
	//
	// The pkg:go/ scheme preserves the go.mod-declared path
	// literally — vanity paths (modernc.org/sqlite, gopkg.in/...)
	// stay queryable under the same form the user would pass to
	// signatory analyze.
	CanonicalURI string

	// Version is the pinned version from the manifest. Semver-
	// shaped for Go and npm; PEP-440 for PyPI; etc. Preserved
	// verbatim; signatory does not parse or normalize it in v0.1.
	Version string

	// Direct is true when the dep appears as a direct requirement
	// (go.mod's require-without-indirect, package.json's
	// "dependencies" rather than transitive). Indirect deps are
	// part of the tree but not the project's chosen surface —
	// survey reports them separately because the action ("analyze
	// this") applies primarily to direct deps.
	Direct bool

	// Ecosystem is the short slug identifying the manifest kind:
	// "go", "npm", "pypi", "cargo", etc. Used for display and for
	// routing to the appropriate collector when --refresh lands.
	Ecosystem string
}

// ProjectInfo describes the project the manifest represents — the
// thing whose dependencies we're surveying, not the dependencies
// themselves.
type ProjectInfo struct {
	// Name is the project's module / package name as declared in
	// the manifest. For Go: the `module` directive. For npm: the
	// `name` field in package.json. Empty when the manifest
	// doesn't declare one.
	Name string

	// ManifestPath is the absolute path to the parsed manifest
	// file. Useful for error messages and for distinguishing
	// "survey of repo A's go.mod" from "survey of repo B's
	// go.mod" in the future project-registry view.
	ManifestPath string

	// Ecosystem matches Dep.Ecosystem: "go", "npm", ...
	Ecosystem string

	// EcoVersion is the ecosystem-native toolchain version the
	// project targets:
	//   - Go: go.mod's `go` directive ("1.25.1")
	//   - npm: package.json's engines.node
	//   - PyPI: requires-python or python_requires
	//
	// Empty when the manifest doesn't pin a version or the
	// parser can't extract one.
	EcoVersion string
}
