package resolver

import (
	"context"
	"fmt"
	"strings"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal/registry/maven"
)

// MavenResolver implements Resolver for pkg:maven/<groupId>/<artifactId>
// by consulting repo1.maven.org — maven-metadata.xml for the latest
// version, then POM fetch for the SCM URL. Single-host, no Solr.
type MavenResolver struct {
	client *maven.Client
}

// NewMavenResolver returns a resolver backed by a production
// maven.Client (repo1.maven.org). Tests construct a resolver with
// NewMavenResolverWithClient and a client pointed at httptest servers.
func NewMavenResolver() *MavenResolver {
	return &MavenResolver{client: maven.NewClient()}
}

// NewMavenResolverWithClient is the test-seam constructor. Pass a
// client pointed at an httptest server for hermetic tests.
func NewMavenResolverWithClient(c *maven.Client) *MavenResolver {
	return &MavenResolver{client: c}
}

// ResolveSource finds the source repository for a Maven artifact by:
//  1. Splitting the name on "/" to extract groupId and artifactId
//  2. Fetching maven-metadata.xml for the latest release version
//  3. Fetching the POM for that version and extracting the <scm> URL
//
// Errors from the registry calls (network, 404, body cap) surface to
// the caller verbatim. Successful-but-no-SCM cases return
// DeclaredSource{SelfReported: true} with empty URI/URL.
func (r *MavenResolver) ResolveSource(ctx context.Context, name string) (DeclaredSource, error) {
	groupID, artifactID, ok := strings.Cut(name, "/")
	if !ok || groupID == "" || artifactID == "" {
		return DeclaredSource{}, fmt.Errorf("maven resolver: invalid name %q (expected groupId/artifactId)", name)
	}

	// Step 1: fetch metadata for the latest version.
	meta, err := r.client.FetchMetadata(ctx, groupID, artifactID)
	if err != nil {
		return DeclaredSource{}, fmt.Errorf("maven resolver: metadata for %s:%s: %w", groupID, artifactID, err)
	}
	latestVersion := meta.Versioning.Release
	if latestVersion == "" {
		latestVersion = meta.Versioning.Latest
	}
	if latestVersion == "" && len(meta.Versioning.Versions) > 0 {
		latestVersion = meta.Versioning.Versions[len(meta.Versioning.Versions)-1]
	}
	if latestVersion == "" {
		return DeclaredSource{}, fmt.Errorf("maven resolver: no versions found for %s:%s", groupID, artifactID)
	}

	// Step 2: fetch the POM and extract the SCM URL.
	repoURL, err := r.client.ResolveRepoURL(ctx, groupID, artifactID, latestVersion)
	if err != nil {
		return DeclaredSource{}, fmt.Errorf("maven resolver: POM fetch for %s:%s:%s: %w",
			groupID, artifactID, latestVersion, err)
	}
	if repoURL == "" {
		return DeclaredSource{SelfReported: true}, nil
	}

	// Route through ResolveTarget for canonical URI + clone URL
	// consistency — same delegation as npm, pypi, cargo, and gem
	// resolvers.
	resolved, err := profile.ResolveTarget(repoURL)
	if err != nil {
		return DeclaredSource{}, err
	}
	return DeclaredSource{
		URI:          resolved.CanonicalURI,
		URL:          resolved.CloneURL,
		SelfReported: true,
	}, nil
}

func init() {
	Default.Register("maven", NewMavenResolver())
}
