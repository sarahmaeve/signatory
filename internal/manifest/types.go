package manifest

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
