package pypi

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/BurntSushi/toml"

	"github.com/sarahmaeve/signatory/internal/manifest"
)

// maxPyProjectBytes caps untrusted pyproject.toml input. The
// BurntSushi/toml synthesis (recorded under
// repo:github/burntsushi/toml @ v1.6.0) flagged unbounded inline-
// array nesting as a medium-severity DoS risk. A size cap is the
// cheap front-line defense — real pyproject.toml files are
// typically well under 16 KiB; 64 KiB leaves comfortable headroom
// while still ruling out pathological adversarial input. Adjust
// here if a real-world file legitimately exceeds it.
const maxPyProjectBytes = 64 * 1024

// errNoModernFormat is the package-private control-flow signal
// that parsePyProject couldn't find either PEP 621 [project] or
// PEP 735 [dependency-groups] tables. The dispatcher consumes
// this and (until Commit 6 lands the Poetry parser) translates
// it into the user-facing ErrPyProjectTOMLNotYetSupported.
//
// Lowercase because no caller outside this package needs to
// distinguish it from any other parse error.
var errNoModernFormat = errors.New("pyproject.toml has neither [project] nor [dependency-groups] table")

// pyProjectFile is the TOML-decoder target for the slice of
// pyproject.toml signatory cares about. Every field is optional
// — pyproject.toml has no required tables, and we tolerate
// arbitrary additional fields (build-system, tool.*, etc.) by
// silently ignoring them.
type pyProjectFile struct {
	Project          *projectTable    `toml:"project"`
	DependencyGroups dependencyGroups `toml:"dependency-groups"`
}

// projectTable is the PEP 621 [project] table. Many real-world
// projects use `dynamic = ["dependencies"]` to compute deps at
// build time; in that case Dependencies will be nil/empty here
// and our enumeration is incomplete. Surfaced via Dynamic so a
// future caller can warn.
type projectTable struct {
	Name                 string              `toml:"name"`
	Version              string              `toml:"version"`
	RequiresPython       string              `toml:"requires-python"`
	Dependencies         []string            `toml:"dependencies"`
	OptionalDependencies map[string][]string `toml:"optional-dependencies"`
	Dynamic              []string            `toml:"dynamic"`
}

// dependencyGroups holds PEP 735 [dependency-groups] entries.
// Each group's value is a heterogeneous array of strings (PEP 508
// dep specs) and tables (with the single key "include-group").
// We decode entries as `any` and discriminate at flatten time
// because TOML's type system has no native union representation.
type dependencyGroups map[string][]any

// parsePyProject reads and parses a pyproject.toml file, returning
// the project's metadata and dependency list. Handles two modern
// dialects:
//
//   - PEP 621 [project] table: Name, RequiresPython, Dependencies,
//     OptionalDependencies (flattened across groups)
//   - PEP 735 [dependency-groups] table: each group flattened with
//     include-group composition resolved (visited-set cycle
//     detection per spec)
//
// Returns errNoModernFormat (package-private) when the file has
// neither table — the dispatcher uses this to decide whether to
// fall through to the Poetry parser (Commit 6) or surface a
// user-facing not-yet-supported error.
//
// Enforces maxPyProjectBytes as a front-line size cap before
// invoking the TOML decoder.
func parsePyProject(path string) (manifest.ProjectInfo, []manifest.Dep, error) {
	stat, err := os.Stat(path)
	if err != nil {
		return manifest.ProjectInfo{}, nil, fmt.Errorf("stat %q: %w", path, err)
	}
	if stat.Size() > maxPyProjectBytes {
		return manifest.ProjectInfo{}, nil, fmt.Errorf(
			"pyproject.toml too large: %d bytes (cap: %d) at %s",
			stat.Size(), maxPyProjectBytes, path)
	}

	data, err := os.ReadFile(path) //nolint:gosec // G304: caller-supplied manifest path validated by survey/Detect; size-capped above
	if err != nil {
		return manifest.ProjectInfo{}, nil, fmt.Errorf("read %q: %w", path, err)
	}

	var f pyProjectFile
	if _, err := toml.Decode(string(data), &f); err != nil {
		return manifest.ProjectInfo{}, nil, fmt.Errorf("parse %q: %w", path, err)
	}

	hasProject := f.Project != nil
	hasGroups := len(f.DependencyGroups) > 0
	if !hasProject && !hasGroups {
		return manifest.ProjectInfo{}, nil, errNoModernFormat
	}

	info := manifest.ProjectInfo{Ecosystem: "pypi"}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return manifest.ProjectInfo{}, nil, fmt.Errorf("resolve %q: %w", path, err)
	}
	info.ManifestPath = absPath

	var deps []manifest.Dep

	if hasProject {
		info.Name = f.Project.Name
		info.EcoVersion = f.Project.RequiresPython
		deps = append(deps, parsePEP621Dependencies(f.Project)...)
	}

	if hasGroups {
		groupDeps, err := flattenDependencyGroups(f.DependencyGroups)
		if err != nil {
			return manifest.ProjectInfo{}, nil, err
		}
		deps = append(deps, groupDeps...)
	}

	return info, deps, nil
}

// parsePEP621Dependencies extracts every PEP 508 string from
// [project].dependencies and [project.optional-dependencies], and
// turns each into a Dep via the shared parsePEP508Requirement
// helper. All deps are Direct=true.
//
// Strings that don't parse (empty after marker stripping, or
// malformed) are silently dropped — same defensive behavior the
// requirements.txt parser uses for malformed lines. Real
// pyproject.toml files don't have malformed deps; surfacing such
// errors would just block legitimate projects on a typo.
func parsePEP621Dependencies(p *projectTable) []manifest.Dep {
	var deps []manifest.Dep
	for _, spec := range p.Dependencies {
		if d, ok := parsePEP508Requirement(spec); ok {
			deps = append(deps, d)
		}
	}
	// optional-dependencies: iterate map. Group order isn't
	// semantically meaningful (deps are flattened union); sort
	// keys for deterministic output across runs.
	groupNames := make([]string, 0, len(p.OptionalDependencies))
	for name := range p.OptionalDependencies {
		groupNames = append(groupNames, name)
	}
	sort.Strings(groupNames)
	for _, group := range groupNames {
		for _, spec := range p.OptionalDependencies[group] {
			if d, ok := parsePEP508Requirement(spec); ok {
				deps = append(deps, d)
			}
		}
	}
	return deps
}

// flattenDependencyGroups expands all PEP 735 groups into a flat
// list of Deps. Implements the spec-mandated semantics:
//
//   - Group names normalized via PEP 503 rule before any lookup
//     or comparison
//   - Duplicate normalized names → error
//   - Includes resolved recursively with visited-set cycle
//     detection
//   - No deduplication during expansion (spec-mandated)
//   - Include of an undefined group → error
//   - Invalid entry shapes (table without exactly one
//     "include-group" key, non-string non-table values) → error
func flattenDependencyGroups(rawGroups dependencyGroups) ([]manifest.Dep, error) {
	if len(rawGroups) == 0 {
		return nil, nil
	}

	// Build the normalized-name → original-name map AND
	// detect duplicates. Both surfaces in one walk so the error
	// can name the colliding originals.
	byNorm := make(map[string][]any, len(rawGroups))
	originalName := make(map[string]string, len(rawGroups))
	for name, entries := range rawGroups {
		norm := pep503Normalize(name)
		if existing, dup := originalName[norm]; dup {
			return nil, fmt.Errorf(
				"PEP 735: duplicate normalized group name %q (from original names %q and %q)",
				norm, existing, name)
		}
		originalName[norm] = name
		byNorm[norm] = entries
	}

	// Sort group names for deterministic output. Order across
	// groups isn't semantically meaningful (deps flatten as a
	// union), but reproducibility helps tests and diffs.
	groupNames := make([]string, 0, len(byNorm))
	for n := range byNorm {
		groupNames = append(groupNames, n)
	}
	sort.Strings(groupNames)

	var allDeps []manifest.Dep
	for _, normName := range groupNames {
		deps, err := resolveGroup(normName, byNorm, map[string]bool{})
		if err != nil {
			return nil, err
		}
		allDeps = append(allDeps, deps...)
	}
	return allDeps, nil
}

// resolveGroup expands one group's entries to a flat dep list,
// recursing into includes. visited is the set of normalized
// group names currently on the resolution stack — used for
// cycle detection. Mutates visited during the call (adding the
// current group on entry, removing on return) so that sibling
// includes can reference the same group without false-positive
// cycle detection.
func resolveGroup(normName string, byNorm map[string][]any, visited map[string]bool) ([]manifest.Dep, error) {
	if visited[normName] {
		return nil, fmt.Errorf("PEP 735: cycle in include resolution at group %q", normName)
	}
	visited[normName] = true
	defer delete(visited, normName)

	entries, ok := byNorm[normName]
	if !ok {
		return nil, fmt.Errorf("PEP 735: include of undefined group %q", normName)
	}

	var deps []manifest.Dep
	for _, entry := range entries {
		switch v := entry.(type) {
		case string:
			if d, ok := parsePEP508Requirement(v); ok {
				deps = append(deps, d)
			}
		case map[string]any:
			includeName, err := extractIncludeGroupKey(v)
			if err != nil {
				return nil, err
			}
			includedDeps, err := resolveGroup(pep503Normalize(includeName), byNorm, visited)
			if err != nil {
				return nil, err
			}
			deps = append(deps, includedDeps...)
		default:
			return nil, fmt.Errorf(
				"PEP 735: invalid entry type %T (expected string or {include-group = \"...\"} table)",
				entry)
		}
	}
	return deps, nil
}

// extractIncludeGroupKey enforces the spec rule that table
// entries in a dependency-group array must have exactly one key,
// "include-group", whose value is a string.
func extractIncludeGroupKey(t map[string]any) (string, error) {
	if len(t) != 1 {
		return "", fmt.Errorf(
			"PEP 735: invalid include shape, expected exactly one key 'include-group' but got %d keys",
			len(t))
	}
	raw, ok := t["include-group"]
	if !ok {
		return "", fmt.Errorf(
			"PEP 735: invalid include shape, expected 'include-group' key (got other key in table)")
	}
	name, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf(
			"PEP 735: invalid include shape, 'include-group' value must be a string (got %T)",
			raw)
	}
	return name, nil
}
