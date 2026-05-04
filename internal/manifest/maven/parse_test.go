package maven

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/manifest"
)

// TestParse_Simple covers the basic case: a pom.xml with explicit
// versions on all dependencies, including test-scoped ones.
//
// Test packages:
//   - com.google.guava:guava           (large, ubiquitous)
//   - org.apache.commons:commons-lang3 (medium, zero transitive deps)
//   - com.fasterxml.jackson.core:jackson-databind (has real transitives)
//   - org.junit.jupiter:junit-jupiter  (test scope)
func TestParse_Simple(t *testing.T) {
	t.Parallel()

	info, deps, err := Parse(filepath.Join("testdata", "simple", "pom.xml"))
	require.NoError(t, err)

	// ProjectInfo from pom.xml.
	assert.Equal(t, "myapp", info.Name)
	assert.Equal(t, "maven", info.Ecosystem)
	assert.Equal(t, "17", info.EcoVersion)
	assert.True(t, filepath.IsAbs(info.ManifestPath))

	byURI := indexByURI(deps)

	// Direct runtime deps — two-part canonical URI (PURL-compliant).
	guava := byURI["pkg:maven/com.google.guava/guava"]
	require.NotEmpty(t, guava.Name, "guava should be in deps")
	assert.Equal(t, "com.google.guava/guava", guava.Name)
	assert.Equal(t, "33.2.1-jre", guava.Version)
	assert.True(t, guava.Direct)
	assert.Equal(t, "maven", guava.Ecosystem)

	lang3 := byURI["pkg:maven/org.apache.commons/commons-lang3"]
	assert.Equal(t, "org.apache.commons/commons-lang3", lang3.Name)
	assert.Equal(t, "3.14.0", lang3.Version)
	assert.True(t, lang3.Direct)
	assert.Equal(t, "maven", lang3.Ecosystem)

	jackson := byURI["pkg:maven/com.fasterxml.jackson.core/jackson-databind"]
	assert.Equal(t, "com.fasterxml.jackson.core/jackson-databind", jackson.Name)
	assert.Equal(t, "2.17.1", jackson.Version)
	assert.True(t, jackson.Direct)

	// Test-scoped deps are still deps (scope is a signal, not a filter).
	junit := byURI["pkg:maven/org.junit.jupiter/junit-jupiter"]
	assert.Equal(t, "org.junit.jupiter/junit-jupiter", junit.Name)
	assert.Equal(t, "5.10.2", junit.Version)
	assert.True(t, junit.Direct)

	assert.Len(t, deps, 4, "should have exactly 4 dependencies")
}

// TestParse_WithProperties verifies that ${property} references in
// <version> elements are resolved from the <properties> block. This
// is the most common Maven version-management pattern.
func TestParse_WithProperties(t *testing.T) {
	t.Parallel()

	info, deps, err := Parse(filepath.Join("testdata", "with-properties", "pom.xml"))
	require.NoError(t, err)

	assert.Equal(t, "propapp", info.Name)
	assert.Equal(t, "21", info.EcoVersion)

	byURI := indexByURI(deps)

	// ${jackson.version} → 2.17.1
	databind := byURI["pkg:maven/com.fasterxml.jackson.core/jackson-databind"]
	assert.Equal(t, "2.17.1", databind.Version)

	core := byURI["pkg:maven/com.fasterxml.jackson.core/jackson-core"]
	assert.Equal(t, "2.17.1", core.Version)

	// ${guava.version} → 33.2.1-jre
	guava := byURI["pkg:maven/com.google.guava/guava"]
	assert.Equal(t, "33.2.1-jre", guava.Version)

	// ${project.version} → 2.0.0 (the project's own version)
	shared := byURI["pkg:maven/com.example/shared-lib"]
	assert.Equal(t, "2.0.0", shared.Version,
		"${project.version} should resolve to the POM's own <version>")
}

// TestParse_WithDependencyManagement verifies that deps without an
// explicit <version> inherit from <dependencyManagement>. This is
// the BOM / parent-POM pattern used by Dropwizard, Spring Boot, etc.
func TestParse_WithDependencyManagement(t *testing.T) {
	t.Parallel()

	_, deps, err := Parse(filepath.Join("testdata", "with-dep-management", "pom.xml"))
	require.NoError(t, err)

	byURI := indexByURI(deps)

	// dropwizard-core: no <version> in <dependencies>, but
	// dependencyManagement has dropwizard-bom at ${dropwizard.version}=4.0.7.
	// BOM imports (scope=import, type=pom) are NOT dependencies — they
	// define version pins but don't add transitive deps. The actual
	// dropwizard-core dep inherits its version from the BOM entry.
	//
	// Note: we can't resolve BOM contents without fetching the BOM POM
	// from the network (which is a Phase B concern). For Phase A, deps
	// whose version comes from an unresolvable BOM get an empty version.
	// This is the correct local-only behavior — same as Gemfile-only
	// parsing without a lockfile.
	dw := byURI["pkg:maven/io.dropwizard/dropwizard-core"]
	require.NotEmpty(t, dw.Name, "dropwizard-core should be in deps")
	assert.True(t, dw.Direct)

	// commons-lang3: version pinned explicitly in dependencyManagement,
	// not via BOM — so it IS resolvable locally.
	lang3 := byURI["pkg:maven/org.apache.commons/commons-lang3"]
	assert.Equal(t, "3.14.0", lang3.Version,
		"version should be inherited from dependencyManagement")

	// guava: explicit version in <dependencies> — should win over
	// any management entry (there is none, but the principle holds).
	guava := byURI["pkg:maven/com.google.guava/guava"]
	assert.Equal(t, "33.2.1-jre", guava.Version)

	// BOM import itself (dropwizard-bom) should NOT appear as a dep.
	_, hasBOM := byURI["pkg:maven/io.dropwizard/dropwizard-bom"]
	assert.False(t, hasBOM,
		"BOM imports (scope=import, type=pom) are not dependencies")

	assert.Len(t, deps, 3, "should have 3 real deps (BOM excluded)")
}

// TestParse_WithLocalDeps verifies that system-scoped dependencies
// (local JARs not from Maven Central) are classified as maven-local,
// matching the cargo-local / gem-local pattern for non-registry deps.
func TestParse_WithLocalDeps(t *testing.T) {
	t.Parallel()

	_, deps, err := Parse(filepath.Join("testdata", "with-local", "pom.xml"))
	require.NoError(t, err)

	byName := indexByName(deps)

	// Registry dep should have canonical URI.
	guava := byName["com.google.guava/guava"]
	assert.Equal(t, "pkg:maven/com.google.guava/guava", guava.CanonicalURI)
	assert.Equal(t, "maven", guava.Ecosystem)
	assert.True(t, guava.Direct)

	// System-scoped dep: local jar, not from Central.
	legacy := byName["com.example.internal/legacy-utils"]
	assert.Equal(t, "maven-local", legacy.Ecosystem)
	assert.Empty(t, legacy.CanonicalURI,
		"system-scoped deps have no canonical URI (not from a registry)")
	assert.True(t, legacy.Direct)

	// Test dep is still a normal registry dep.
	junit := byName["org.junit.jupiter/junit-jupiter"]
	assert.Equal(t, "maven", junit.Ecosystem)
	assert.Equal(t, "pkg:maven/org.junit.jupiter/junit-jupiter", junit.CanonicalURI)
}

// TestParse_NoPom_ReturnsError verifies that a nonexistent file
// produces an error rather than an empty result.
func TestParse_NoPom_ReturnsError(t *testing.T) {
	t.Parallel()

	_, _, err := Parse("/does/not/exist/pom.xml")
	require.Error(t, err)
}

// TestParse_EmptyPom_ReturnsError verifies that a pom.xml without
// a <project> element is rejected.
func TestParse_EmptyPom_ReturnsError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	pomPath := filepath.Join(dir, "pom.xml")
	writeTestFile(t, pomPath, `<?xml version="1.0"?><not-a-project/>`)

	_, _, err := Parse(pomPath)
	require.Error(t, err)
	// xml.Unmarshal rejects the wrong root element before we get to
	// our own validation — either error path is acceptable.
	assert.True(t,
		strings.Contains(err.Error(), "no <project>") ||
			strings.Contains(err.Error(), "expected element type <project>"),
		"error should indicate wrong/missing <project> element, got: %s", err.Error())
}

// TestParse_MalformedXML_ReturnsError verifies that broken XML is
// rejected cleanly.
func TestParse_MalformedXML_ReturnsError(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	pomPath := filepath.Join(dir, "pom.xml")
	writeTestFile(t, pomPath, `<?xml version="1.0"?><project><dependencies><broken`)

	_, _, err := Parse(pomPath)
	require.Error(t, err)
}

// TestParse_NoDeps covers a pom.xml that declares no dependencies.
// This is valid (e.g., a parent POM or a leaf library with no deps).
func TestParse_NoDeps(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	pomPath := filepath.Join(dir, "pom.xml")
	writeTestFile(t, pomPath, `<?xml version="1.0" encoding="UTF-8"?>
<project xmlns="http://maven.apache.org/POM/4.0.0">
    <modelVersion>4.0.0</modelVersion>
    <groupId>com.example</groupId>
    <artifactId>empty</artifactId>
    <version>1.0.0</version>
</project>`)

	info, deps, err := Parse(pomPath)
	require.NoError(t, err)
	assert.Equal(t, "empty", info.Name)
	assert.Empty(t, deps, "no dependencies declared")
}

// TestParse_FileSizeCap verifies defense-in-depth: a pom.xml larger
// than maxPomBytes is rejected before parsing.
func TestParse_FileSizeCap(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	pomPath := filepath.Join(dir, "pom.xml")

	// Write a file that exceeds the cap.
	big := make([]byte, maxPomBytes+1)
	for i := range big {
		big[i] = 'x'
	}
	require.NoError(t, os.WriteFile(pomPath, big, 0o600))

	_, _, err := Parse(pomPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds")
}

// --- test helpers ---

func indexByURI(deps []manifest.Dep) map[string]manifest.Dep {
	m := make(map[string]manifest.Dep, len(deps))
	for _, d := range deps {
		if d.CanonicalURI != "" {
			m[d.CanonicalURI] = d
		}
	}
	return m
}

func indexByName(deps []manifest.Dep) map[string]manifest.Dep {
	m := make(map[string]manifest.Dep, len(deps))
	for _, d := range deps {
		m[d.Name] = d
	}
	return m
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}
