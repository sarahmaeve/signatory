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

// VersionTimestamp pairs a version string with its publish timestamp,
// obtained via HEAD on the artifact jar.
type VersionTimestamp struct {
	Version   string
	Timestamp int64 // Unix milliseconds, matches the rest of signatory's time representation.
}
