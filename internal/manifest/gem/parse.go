// Package gem parses Gemfile and Gemfile.lock manifests for Ruby
// projects. The parser handles:
//
//   - Gemfile: line-scanning for gem declarations (covers >95% of
//     real-world usage without requiring a Ruby interpreter)
//   - Gemfile.lock: structured text parsing for resolved versions
//     and the full transitive dependency graph
//
// When both files exist, the lockfile is the authority for resolved
// versions and transitives; the Gemfile provides direct-dep
// classification and project metadata (ruby version).
package gem

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/sarahmaeve/signatory/internal/manifest"
	"github.com/sarahmaeve/signatory/internal/profile"
)

// Size caps for untrusted file reads (defense-in-depth).
const (
	maxGemfileBytes     = 64 * 1024
	maxGemfileLockBytes = 1 * 1024 * 1024
)

// gemDeclRe matches `gem "name"` or `gem 'name'` at the start of a
// line (after optional whitespace). Captures the gem name in group 1.
var gemDeclRe = regexp.MustCompile(`^\s*gem\s+['"]([^'"]+)['"]`)

// rubyVersionRe matches `ruby "version"` or `ruby 'version'`.
var rubyVersionRe = regexp.MustCompile(`^\s*ruby\s+['"]([^'"]+)['"]`)

// localDepRe matches path: or git: or github: options on a gem line.
var localDepRe = regexp.MustCompile(`(?:path|git|github)\s*:`)

// Parse reads a Gemfile at path and, if a Gemfile.lock exists in the
// same directory, merges the resolved versions and transitive deps
// from the lockfile. Returns project metadata, the dependency list,
// and any error.
//
// When only the Gemfile exists (no lockfile), deps are returned
// without resolved versions — the Name and Direct fields are
// populated but Version is empty.
func Parse(path string) (manifest.ProjectInfo, []manifest.Dep, error) {
	data, err := readCapped(path, maxGemfileBytes)
	if err != nil {
		return manifest.ProjectInfo{}, nil, fmt.Errorf("read Gemfile: %w", err)
	}

	info := manifest.ProjectInfo{
		ManifestPath: path,
		Ecosystem:    "gem",
	}

	// Extract ruby version and gem declarations from Gemfile.
	gemfileDecls, rubyVer := parseGemfileContent(string(data))
	info.EcoVersion = rubyVer

	// Try to read the lockfile alongside the Gemfile.
	lockPath := filepath.Join(filepath.Dir(path), "Gemfile.lock")
	lockData, lockErr := readCapped(lockPath, maxGemfileLockBytes)

	if lockErr != nil {
		// No lockfile — return Gemfile-only deps (no versions).
		deps := make([]manifest.Dep, 0, len(gemfileDecls))
		for _, decl := range gemfileDecls {
			deps = append(deps, decl.toDep())
		}
		return info, deps, nil
	}

	// Parse lockfile for resolved packages and direct-dep markers.
	lockSpecs, lockDirects, lockNonGem := parseLockfileContent(string(lockData))

	// Merge: lockfile specs are the authority for resolved versions;
	// DEPENDENCIES section marks which are direct.
	deps := make([]manifest.Dep, 0, len(lockSpecs))
	for _, spec := range lockSpecs {
		isDirect := lockDirects[spec.name]
		eco := "gem"
		uri := profile.CanonicalPackageURI("gem", spec.name)

		// Non-gem sources (GIT, PATH) get gem-local ecosystem.
		if lockNonGem[spec.name] {
			eco = "gem-local"
			uri = ""
		}

		deps = append(deps, manifest.Dep{
			Name:         spec.name,
			CanonicalURI: uri,
			Version:      spec.version,
			Direct:       isDirect,
			Ecosystem:    eco,
		})
	}

	return info, deps, nil
}

// ParseGraph extracts the transitive dependency graph from a
// Gemfile.lock file. Each parent→child edge represents a direct
// dependency relationship between two packages as declared in the
// lockfile's spec sections.
//
// Returns manifest.ErrGraphUnavailable when the lockfile cannot be
// read or parsed.
func ParseGraph(lockPath string) (manifest.Graph, error) {
	data, err := readCapped(lockPath, maxGemfileLockBytes)
	if err != nil {
		return manifest.Graph{}, errors.Join(manifest.ErrGraphUnavailable, fmt.Errorf("read lockfile: %w", err))
	}

	edges := parseLockfileEdges(string(data))

	return manifest.Graph{
		Edges: edges,
	}, nil
}

// gemDecl is a gem declaration extracted from a Gemfile.
type gemDecl struct {
	name    string
	isLocal bool // path:/git:/github: dep
}

func (d gemDecl) toDep() manifest.Dep {
	eco := "gem"
	uri := profile.CanonicalPackageURI("gem", d.name)
	if d.isLocal {
		eco = "gem-local"
		uri = ""
	}
	return manifest.Dep{
		Name:         d.name,
		CanonicalURI: uri,
		Version:      "", // no lockfile, no resolved version
		Direct:       true,
		Ecosystem:    eco,
	}
}

// parseGemfileContent extracts gem declarations and the ruby version
// from Gemfile content via line scanning. Handles the common patterns:
// gem 'name', gem "name", path:/git:/github: classification.
func parseGemfileContent(content string) ([]gemDecl, string) {
	var decls []gemDecl
	var rubyVer string

	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := scanner.Text()

		// Ruby version.
		if m := rubyVersionRe.FindStringSubmatch(line); m != nil {
			rubyVer = m[1]
			continue
		}

		// Gem declaration.
		if m := gemDeclRe.FindStringSubmatch(line); m != nil {
			name := m[1]
			isLocal := localDepRe.MatchString(line)
			decls = append(decls, gemDecl{name: name, isLocal: isLocal})
		}
	}

	return decls, rubyVer
}

// lockSpec is a resolved package from a Gemfile.lock specs section.
type lockSpec struct {
	name    string
	version string
}

// parseLockfileContent parses the Gemfile.lock text format and returns:
//   - specs: all resolved packages (from GEM, GIT, PATH sections)
//   - directs: set of names that appear in the DEPENDENCIES section
//   - nonGem: set of names from GIT/PATH sections (non-registry deps)
func parseLockfileContent(content string) (specs []lockSpec, directs map[string]bool, nonGem map[string]bool) {
	directs = map[string]bool{}
	nonGem = map[string]bool{}

	type section int
	const (
		sNone section = iota
		sGEM
		sGIT
		sPATH
		sDEPS
		sPLATFORMS
		sBUNDLED
		sRUBYVER
	)

	var current section
	var inSpecs bool
	var currentIsNonGem bool

	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := scanner.Text()

		// Section headers: all-caps at column 0.
		if len(line) > 0 && line[0] >= 'A' && line[0] <= 'Z' && !strings.HasPrefix(line, " ") {
			inSpecs = false
			currentIsNonGem = false
			switch {
			case line == "GEM":
				current = sGEM
			case line == "GIT":
				current = sGIT
				currentIsNonGem = true
			case line == "PATH":
				current = sPATH
				currentIsNonGem = true
			case line == "DEPENDENCIES":
				current = sDEPS
			case line == "PLATFORMS":
				current = sPLATFORMS
			case strings.HasPrefix(line, "BUNDLED WITH"):
				current = sBUNDLED
			case strings.HasPrefix(line, "RUBY VERSION"):
				current = sRUBYVER
			default:
				current = sNone
			}
			continue
		}

		switch current {
		case sGEM, sGIT, sPATH:
			// "  specs:" marks the start of package listings.
			if strings.TrimSpace(line) == "specs:" {
				inSpecs = true
				continue
			}
			if !inSpecs {
				continue
			}
			// 4-space indent = package name (version).
			if len(line) >= 4 && line[:4] == "    " && (len(line) < 6 || line[4] != ' ') {
				name, version := parseSpecLine(line[4:])
				if name != "" {
					specs = append(specs, lockSpec{name: name, version: version})
					if currentIsNonGem {
						nonGem[name] = true
					}
				}
			}

		case sDEPS:
			// 2-space indent = dependency name.
			trimmed := strings.TrimSpace(line)
			if trimmed == "" {
				continue
			}
			name := parseDependencyLine(trimmed)
			if name != "" {
				directs[name] = true
			}
		}
	}

	return specs, directs, nonGem
}

// parseLockfileEdges extracts parent→child dependency edges from all
// specs sections in a Gemfile.lock.
func parseLockfileEdges(content string) []manifest.Edge {
	var edges []manifest.Edge

	type section int
	const (
		sNone section = iota
		sSpecs
		sOther
	)

	var current section
	var currentParent string

	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := scanner.Text()

		// Section headers.
		if len(line) > 0 && line[0] >= 'A' && line[0] <= 'Z' && !strings.HasPrefix(line, " ") {
			current = sNone
			currentParent = ""
			switch line {
			case "GEM", "GIT", "PATH":
				// Will enter specs mode when we see "  specs:"
			default:
				current = sOther
			}
			continue
		}

		// "  specs:" line starts package listings.
		if strings.TrimSpace(line) == "specs:" {
			current = sSpecs
			continue
		}

		if current != sSpecs {
			continue
		}

		// 4-space indent: package declaration (new parent).
		if len(line) >= 4 && line[:4] == "    " && (len(line) < 6 || line[4] != ' ') {
			name, _ := parseSpecLine(line[4:])
			if name != "" {
				currentParent = profile.CanonicalPackageURI("gem", name)
			}
			continue
		}

		// 6-space indent: sub-dependency of current parent.
		if len(line) >= 6 && line[:6] == "      " && currentParent != "" {
			depName := parseDepNameFromConstraint(strings.TrimSpace(line[6:]))
			if depName != "" {
				childURI := profile.CanonicalPackageURI("gem", depName)
				// Skip self-edges: a gem depending on itself is
				// nonsensical and indicates a malformed lockfile.
				if childURI != currentParent {
					edges = append(edges, manifest.Edge{
						Parent: currentParent,
						Child:  childURI,
					})
				}
			}
		}
	}

	return edges
}

// parseSpecLine parses a spec entry like "rails (7.1.3)" or
// "actionpack (7.1.3)" and returns the name and version.
func parseSpecLine(s string) (name, version string) {
	s = strings.TrimSpace(s)
	if !utf8.ValidString(s) {
		return "", ""
	}
	before, after, ok := strings.Cut(s, "(")
	if !ok {
		// No version — just a name (unusual but handle gracefully).
		return s, ""
	}
	name = strings.TrimSpace(before)
	if closeIdx := strings.IndexByte(after, ')'); closeIdx >= 0 {
		after = after[:closeIdx]
	}
	return name, strings.TrimSpace(after)
}

// parseDependencyLine extracts the gem name from a DEPENDENCIES line.
// Lines look like: "rails (~> 7.1)" or "devise!" or "mongoid (~> 9.0)!"
func parseDependencyLine(s string) string {
	if !utf8.ValidString(s) {
		return ""
	}
	// Strip trailing ! (marks non-gem source).
	s = strings.TrimSuffix(s, "!")

	// Strip version constraint in parens.
	if idx := strings.IndexByte(s, '('); idx > 0 {
		s = s[:idx]
	}

	return strings.TrimSpace(s)
}

// parseDepNameFromConstraint extracts just the gem name from a
// sub-dependency line like "actionpack (= 7.1.3)" or "nio4r (~> 2.0)".
func parseDepNameFromConstraint(s string) string {
	if !utf8.ValidString(s) {
		return ""
	}
	if idx := strings.IndexByte(s, '('); idx > 0 {
		return strings.TrimSpace(s[:idx])
	}
	// No constraint — bare name.
	return strings.TrimSpace(s)
}

// readCapped reads a file up to maxBytes. Returns an error if the file
// doesn't exist or can't be read.
func readCapped(path string, maxBytes int64) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // G304: lockfile path is the parser's input by design
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("file %s exceeds %d byte cap", filepath.Base(path), maxBytes)
	}
	return data, nil
}
