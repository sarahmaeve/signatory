package pypi

import (
	"fmt"
	"slices"
	"strings"

	"github.com/sarahmaeve/signatory/internal/manifest"
)

// toolTable captures the [tool] table of pyproject.toml. Only
// [tool.poetry] is decoded; other [tool.*] sub-tables (hatch, pdm,
// mypy, ruff, etc.) are silently ignored.
type toolTable struct {
	Poetry *poetryTable `toml:"poetry"`
}

// poetryTable holds the Poetry-specific configuration. The
// dependency maps decode each value as `any` because Poetry's
// values are heterogeneous: a string version spec OR an inline
// table carrying version/extras/git/path/url/file/source keys.
// Discrimination happens at the value-parsing layer.
type poetryTable struct {
	Name            string                 `toml:"name"`
	Version         string                 `toml:"version"`
	Dependencies    map[string]any         `toml:"dependencies"`
	DevDependencies map[string]any         `toml:"dev-dependencies"`
	Group           map[string]poetryGroup `toml:"group"`
}

// poetryGroup is the modern Poetry 1.2+ form: a named group with
// its own dependencies map. Multiple groups flatten into a single
// dep list; the group name itself is informational and not
// surfaced in v0.1 (a future Dep.Group field would change that).
type poetryGroup struct {
	Dependencies map[string]any `toml:"dependencies"`
}

// poetryProjectMeta carries the metadata Poetry CAN provide when
// PEP 621 is absent. parsePyProject merges these into ProjectInfo
// using "PEP 621 wins when present" rules — Poetry only fills
// gaps, never overwrites.
type poetryProjectMeta struct {
	Name       string
	EcoVersion string
}

// extractPoetryDeps reads [tool.poetry] from a parsed
// pyproject.toml and returns the Poetry-derived metadata plus
// the deps from:
//
//   - [tool.poetry.dependencies] (excluding the `python` key,
//     which is the runtime pin and feeds EcoVersion)
//   - [tool.poetry.dev-dependencies] (the legacy form, pre-1.2)
//   - [tool.poetry.group.<name>.dependencies] (the modern form)
//
// All deps are Direct=true. Map iteration is sorted for
// deterministic output across runs.
//
// Returns nil meta and nil deps when the input is nil. Errors
// surface only on malformed Poetry value shapes — empty input
// is fine.
func extractPoetryDeps(p *poetryTable) (*poetryProjectMeta, []manifest.Dep, error) {
	if p == nil {
		return nil, nil, nil
	}

	meta := &poetryProjectMeta{
		Name:       p.Name,
		EcoVersion: poetryRuntimePython(p.Dependencies),
	}

	var deps []manifest.Dep

	// [tool.poetry.dependencies] (excluding python)
	mainDeps, err := convertPoetryDeps(p.Dependencies, true)
	if err != nil {
		return nil, nil, fmt.Errorf("[tool.poetry.dependencies]: %w", err)
	}
	deps = append(deps, mainDeps...)

	// [tool.poetry.dev-dependencies] (legacy form)
	devDeps, err := convertPoetryDeps(p.DevDependencies, false)
	if err != nil {
		return nil, nil, fmt.Errorf("[tool.poetry.dev-dependencies]: %w", err)
	}
	deps = append(deps, devDeps...)

	// [tool.poetry.group.<name>.dependencies] (modern form).
	// Sort group names for deterministic output.
	groupNames := make([]string, 0, len(p.Group))
	for name := range p.Group {
		groupNames = append(groupNames, name)
	}
	slices.Sort(groupNames)
	for _, name := range groupNames {
		groupDeps, err := convertPoetryDeps(p.Group[name].Dependencies, false)
		if err != nil {
			return nil, nil, fmt.Errorf("[tool.poetry.group.%s.dependencies]: %w", name, err)
		}
		deps = append(deps, groupDeps...)
	}

	return meta, deps, nil
}

// convertPoetryDeps walks a Poetry deps map and produces Deps,
// dispatching by value shape. The filterPython flag is true for
// [tool.poetry.dependencies] (where `python` is the runtime pin)
// and false for dev-dependencies and group dependencies (where a
// `python` key, if it appears, is just an unusual dep — though
// that almost certainly never happens in real projects).
//
// Sorts keys for deterministic output.
func convertPoetryDeps(raw map[string]any, filterPython bool) ([]manifest.Dep, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	names := make([]string, 0, len(raw))
	for n := range raw {
		names = append(names, n)
	}
	slices.Sort(names)

	deps := make([]manifest.Dep, 0, len(raw))
	for _, name := range names {
		if filterPython && name == "python" {
			continue
		}
		dep, err := parsePoetryDepValue(name, raw[name])
		if err != nil {
			return nil, fmt.Errorf("dep %q: %w", name, err)
		}
		deps = append(deps, dep)
	}
	return deps, nil
}

// parsePoetryDepValue converts one Poetry dep entry into a Dep.
// Recognized shapes:
//
//   - string: bare version spec ("^2.31.0", "*", "1.0").
//     Produces a registry Dep with Name=depName, Version=value,
//     CanonicalURI built from PEP 503 normalization.
//   - inline table with `git`, `path`, `url`, or `file` key:
//     non-registry source. Produces a pypi-local Dep with empty
//     CanonicalURI. The dep's key (depName) becomes Name so the
//     operator can identify which dep is non-registry.
//   - inline table with `version` key (no non-registry markers):
//     extracts version; merges `extras` array into the Name as
//     `<name>[extra1,extra2]` to match the requirements.txt
//     convention.
//
// Other shapes return an error naming the dep.
func parsePoetryDepValue(depName string, value any) (manifest.Dep, error) {
	switch v := value.(type) {
	case string:
		return registryPoetryDep(depName, v, nil), nil
	case map[string]any:
		// Non-registry markers take precedence: a table with both
		// `git` and `version` keys is still non-registry (the
		// version constraint applies to the VCS ref, not the
		// registry).
		if hasNonRegistryMarker(v) {
			return manifest.Dep{
				Name:      depName,
				Ecosystem: "pypi-local",
				Direct:    true,
			}, nil
		}
		// Registry dep with possible extras.
		version, _ := v["version"].(string)
		extras := poetryExtras(v["extras"])
		return registryPoetryDep(depName, version, extras), nil
	default:
		return manifest.Dep{}, fmt.Errorf("unsupported value shape %T (expected string or inline table)", value)
	}
}

// hasNonRegistryMarker reports whether a Poetry dep table carries
// any of the source-override keys that classify the dep as
// non-registry. Order doesn't matter — presence of any one of
// these keys flips the classification.
func hasNonRegistryMarker(t map[string]any) bool {
	for _, k := range []string{"git", "path", "url", "file"} {
		if _, ok := t[k]; ok {
			return true
		}
	}
	return false
}

// registryPoetryDep builds a Dep for a registry-sourced Poetry
// entry. extras (when non-empty) get encoded into Name as
// `<name>[extra1,extra2,...]` matching the requirements.txt
// convention; CanonicalURI is built from the bare name (extras
// stripped) under PEP 503 normalization.
func registryPoetryDep(name, version string, extras []string) manifest.Dep {
	displayName := name
	if len(extras) > 0 {
		displayName = name + "[" + strings.Join(extras, ",") + "]"
	}
	dep := manifest.Dep{
		Name:      displayName,
		Version:   version,
		Direct:    true,
		Ecosystem: "pypi",
	}
	if isValidPEP508Name(name) {
		dep.CanonicalURI = "pkg:pypi/" + pep503Normalize(name)
	}
	return dep
}

// poetryExtras pulls a string slice out of an `extras` field's
// raw value. TOML decodes arrays as []any; we coerce each element
// to string and silently skip non-strings (Poetry's spec admits
// only string elements anyway).
func poetryExtras(raw any) []string {
	arr, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// poetryRuntimePython extracts the python runtime pin from
// [tool.poetry.dependencies].python. Handles both string and
// table forms:
//
//	python = "^3.10"
//	python = { version = "^3.10" }
//
// Returns "" when python is absent or the value can't be coerced.
// Used to populate ProjectInfo.EcoVersion when [project].requires-python
// is absent.
func poetryRuntimePython(deps map[string]any) string {
	raw, ok := deps["python"]
	if !ok {
		return ""
	}
	switch v := raw.(type) {
	case string:
		return v
	case map[string]any:
		if s, ok := v["version"].(string); ok {
			return s
		}
	}
	return ""
}
