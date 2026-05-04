// Package maven parses Maven POM files (pom.xml) into the ecosystem-
// neutral types defined in internal/manifest.
//
// Handles:
//   - Standard <dependencies> with groupId:artifactId:version
//   - Property interpolation: ${property}, ${project.version}
//   - <dependencyManagement> version inheritance
//   - BOM imports (scope=import, type=pom) excluded from dep list
//   - system-scoped deps classified as maven-local
//
// Does NOT handle (by design):
//   - Gradle (build.gradle / build.gradle.kts) — deferred
//   - Parent POM inheritance from network (requires fetching)
//   - BOM expansion (requires fetching the BOM POM from Central)
//   - Multi-module reactor builds (future Phase A.5)
package maven

import (
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/sarahmaeve/signatory/internal/manifest"
	"github.com/sarahmaeve/signatory/internal/profile"
)

// maxPomBytes caps untrusted pom.xml input. Real POM files are well
// under 32 KiB even for large multi-dependency projects; 64 KiB is
// generous and prevents OOM from adversarial input. Matches the cap
// used by the cargo and gem parsers for consistency.
const maxPomBytes = 64 * 1024

// propertyRef matches ${...} property references in version strings.
var propertyRef = regexp.MustCompile(`\$\{([^}]+)\}`)

// Parse reads a pom.xml at path and returns ProjectInfo + Deps.
//
// Property interpolation resolves ${property.name} references against
// the POM's <properties> block plus built-in project properties
// (project.version, project.groupId, project.artifactId).
//
// Dependencies without an explicit <version> element inherit from
// <dependencyManagement> when a matching groupId:artifactId entry
// exists there. BOM imports (scope=import, type=pom) are excluded
// from the returned dep list — they define version pins but do not
// themselves represent runtime or test dependencies.
//
// System-scoped dependencies (local JARs) are returned with
// Ecosystem="maven-local" and an empty CanonicalURI, matching the
// cargo-local / gem-local pattern for non-registry deps.
func Parse(path string) (manifest.ProjectInfo, []manifest.Dep, error) {
	if path == "" {
		return manifest.ProjectInfo{}, nil, fmt.Errorf("maven: manifest path is empty")
	}

	data, err := readCapped(path, maxPomBytes)
	if err != nil {
		return manifest.ProjectInfo{}, nil, fmt.Errorf("maven: %w", err)
	}

	var pom pomXML
	if err := xml.Unmarshal(data, &pom); err != nil {
		return manifest.ProjectInfo{}, nil, fmt.Errorf("maven: parse %q: %w", path, err)
	}

	// Validate that we actually parsed a <project> element by checking
	// for the presence of a modelVersion or groupId (every valid POM
	// has at least one).
	if pom.ModelVersion == "" && pom.GroupID == "" && pom.ArtifactID == "" {
		return manifest.ProjectInfo{}, nil, fmt.Errorf("maven: %q has no <project> element", path)
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return manifest.ProjectInfo{}, nil, fmt.Errorf("maven: resolve %q: %w", path, err)
	}

	// Build the property map for interpolation.
	props := buildPropertyMap(&pom)

	info := manifest.ProjectInfo{
		Name:         pom.ArtifactID,
		ManifestPath: absPath,
		Ecosystem:    "maven",
		EcoVersion:   extractJavaVersion(props),
	}

	// Build dependencyManagement lookup for version inheritance.
	mgmt := buildManagementMap(pom.DependencyManagement, props)

	// Extract dependencies.
	deps := extractDeps(pom.Dependencies, mgmt, props)

	return info, deps, nil
}

// --- XML structures ---

// pomXML models the subset of pom.xml signatory reads. Uses the
// namespace-aware XMLName to handle both namespaced and non-namespaced
// POM files (some tooling emits bare <project> without xmlns).
type pomXML struct {
	XMLName              xml.Name             `xml:"project"`
	ModelVersion         string               `xml:"modelVersion"`
	GroupID              string               `xml:"groupId"`
	ArtifactID           string               `xml:"artifactId"`
	Version              string               `xml:"version"`
	Packaging            string               `xml:"packaging"`
	Properties           pomProperties        `xml:"properties"`
	Dependencies         []pomDependency      `xml:"dependencies>dependency"`
	DependencyManagement dependencyManagement `xml:"dependencyManagement"`
}

// pomProperties captures the <properties> element as raw XML so we
// can extract arbitrary child elements without knowing them at
// compile time.
type pomProperties struct {
	Inner []byte `xml:",innerxml"`
}

// dependencyManagement wraps the nested <dependencies> inside
// <dependencyManagement>.
type dependencyManagement struct {
	Dependencies []pomDependency `xml:"dependencies>dependency"`
}

// pomDependency is a single <dependency> element.
type pomDependency struct {
	GroupID    string `xml:"groupId"`
	ArtifactID string `xml:"artifactId"`
	Version    string `xml:"version"`
	Scope      string `xml:"scope"`
	Type       string `xml:"type"`
	SystemPath string `xml:"systemPath"`
}

// --- property handling ---

// buildPropertyMap constructs the property lookup table from:
//   - Explicit <properties> entries
//   - Built-in project.* properties
func buildPropertyMap(pom *pomXML) map[string]string {
	props := parseProperties(pom.Properties.Inner)

	// Built-in project properties.
	if pom.Version != "" {
		props["project.version"] = pom.Version
	}
	if pom.GroupID != "" {
		props["project.groupId"] = pom.GroupID
	}
	if pom.ArtifactID != "" {
		props["project.artifactId"] = pom.ArtifactID
	}

	return props
}

// parseProperties parses the raw inner XML of <properties> into a
// map. Each child element becomes key=tagName, value=text content.
// This handles the arbitrary-element pattern Maven uses:
//
//	<properties>
//	    <jackson.version>2.17.1</jackson.version>
//	    <guava.version>33.2.1-jre</guava.version>
//	</properties>
func parseProperties(raw []byte) map[string]string {
	props := make(map[string]string)
	if len(raw) == 0 {
		return props
	}

	// Wrap in a root element so the decoder can parse fragments.
	wrapped := "<r>" + string(raw) + "</r>"
	decoder := xml.NewDecoder(strings.NewReader(wrapped))

	// Skip the opening <r> token.
	if _, err := decoder.Token(); err != nil {
		return props
	}

	for {
		tok, err := decoder.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			name := t.Name.Local
			// Read the text content.
			var content strings.Builder
			for {
				inner, err := decoder.Token()
				if err != nil {
					break
				}
				switch v := inner.(type) {
				case xml.CharData:
					content.Write(v)
				case xml.EndElement:
					goto done
				}
			}
		done:
			props[name] = strings.TrimSpace(content.String())
		}
	}

	return props
}

// interpolate resolves ${property} references in s using the given
// property map. Supports nested resolution (one level deep — if a
// property value itself contains ${...} that is NOT recursively
// expanded, to avoid infinite loops from circular references).
func interpolate(s string, props map[string]string) string {
	if !strings.Contains(s, "${") {
		return s
	}
	return propertyRef.ReplaceAllStringFunc(s, func(match string) string {
		// Extract the property name from ${name}.
		key := match[2 : len(match)-1]
		if val, ok := props[key]; ok {
			return val
		}
		// Unresolvable — leave as-is. This happens for properties
		// inherited from parent POMs (which we can't resolve without
		// network access).
		return match
	})
}

// --- dep extraction ---

// managementKey is the lookup key for dependencyManagement entries.
type managementKey struct {
	groupID    string
	artifactID string
}

// buildManagementMap indexes <dependencyManagement> entries by
// groupId:artifactId for O(1) version lookup.
func buildManagementMap(dm dependencyManagement, props map[string]string) map[managementKey]string {
	m := make(map[managementKey]string, len(dm.Dependencies))
	for _, d := range dm.Dependencies {
		// BOM imports go into the management map too — their version
		// can pin transitive deps even though the BOM itself isn't a dep.
		version := interpolate(d.Version, props)
		key := managementKey{
			groupID:    interpolate(d.GroupID, props),
			artifactID: interpolate(d.ArtifactID, props),
		}
		m[key] = version
	}
	return m
}

// extractDeps converts the <dependencies> list into manifest.Dep
// entries, resolving versions from dependencyManagement where needed.
//
// BOM imports (scope=import AND type=pom) are excluded — they are
// version-management directives, not actual dependencies.
func extractDeps(xmlDeps []pomDependency, mgmt map[managementKey]string, props map[string]string) []manifest.Dep {
	deps := make([]manifest.Dep, 0, len(xmlDeps))
	for _, xd := range xmlDeps {
		groupID := interpolate(xd.GroupID, props)
		artifactID := interpolate(xd.ArtifactID, props)
		version := interpolate(xd.Version, props)
		scope := strings.ToLower(strings.TrimSpace(xd.Scope))
		depType := strings.ToLower(strings.TrimSpace(xd.Type))

		// Skip BOM imports: scope=import + type=pom. These are
		// dependencyManagement directives that shouldn't appear in
		// <dependencies> proper, but when they do (or in the management
		// section), they're not real deps.
		if scope == "import" && depType == "pom" {
			continue
		}

		// Resolve version from dependencyManagement if not specified.
		if version == "" {
			key := managementKey{groupID: groupID, artifactID: artifactID}
			version = mgmt[key]
		}

		name := groupID + "/" + artifactID
		d := manifest.Dep{
			Name:      name,
			Version:   version,
			Direct:    true,
			Ecosystem: "maven",
		}

		// System-scoped deps are local JARs, not from Central.
		if scope == "system" {
			d.Ecosystem = "maven-local"
			// No canonical URI for local deps.
		} else {
			d.CanonicalURI = profile.CanonicalPackageURI("maven", name)
		}

		deps = append(deps, d)
	}
	return deps
}

// --- java version extraction ---

// extractJavaVersion pulls the Java/JDK target version from
// properties, checking common property names in priority order.
func extractJavaVersion(props map[string]string) string {
	// Priority order: most specific to least.
	keys := []string{
		"java.version",           // Spring Boot convention
		"maven.compiler.release", // Maven 3.6+ (JEP 247)
		"maven.compiler.target",  // Classic maven-compiler-plugin
		"maven.compiler.source",  // Fallback
	}
	for _, k := range keys {
		if v, ok := props[k]; ok && v != "" {
			return v
		}
	}
	return ""
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
