package maven

import "encoding/xml"

// Metadata is the parsed form of repo1.maven.org's maven-metadata.xml.
// Every artifact on Maven Central has this file at:
//
//	/maven2/<groupPath>/<artifactId>/maven-metadata.xml
//
// It provides the canonical version list and latest-release marker —
// no Solr index required.
type Metadata struct {
	XMLName    xml.Name   `xml:"metadata"`
	GroupID    string     `xml:"groupId"`
	ArtifactID string     `xml:"artifactId"`
	Versioning Versioning `xml:"versioning"`
}

// Versioning wraps the <versioning> block inside maven-metadata.xml.
type Versioning struct {
	Latest      string   `xml:"latest"`
	Release     string   `xml:"release"`
	Versions    []string `xml:"versions>version"`
	LastUpdated string   `xml:"lastUpdated"`
}

// pomForDeps is the minimal projection of a POM used to extract the
// project's directly-declared dependencies. The Dependencies field is
// tagged at the project root, so encoding/xml matches only the
// <project><dependencies> element — <dependencyManagement><dependencies>
// (BOM version pins, not real deps) and plugin-level <dependencies>
// nested under <build> are structurally excluded by path, not by ad
// hoc string trimming.
type pomForDeps struct {
	XMLName      xml.Name `xml:"project"`
	Dependencies struct {
		Dependency []pomDependency `xml:"dependency"`
	} `xml:"dependencies"`
}

// pomDependency is one <dependency> entry. Scope defaults to "compile"
// when absent (Maven's rule); only "test" is treated as the
// non-consumed dev analog. Optional is captured for completeness but
// optional deps are still part of the declared surface and are kept.
type pomDependency struct {
	GroupID    string `xml:"groupId"`
	ArtifactID string `xml:"artifactId"`
	Scope      string `xml:"scope"`
	Optional   string `xml:"optional"`
}

// VersionTimestamp pairs a version string with its publish timestamp,
// obtained via HEAD on the artifact jar.
type VersionTimestamp struct {
	Version   string
	Timestamp int64 // Unix milliseconds, matches the rest of signatory's time representation.
}
