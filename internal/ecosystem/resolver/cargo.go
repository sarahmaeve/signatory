package resolver

import (
	"context"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal/registry/cargo"
)

// CargoResolver implements Resolver for pkg:cargo/<name> by consulting
// crates.io's JSON API and extracting the declared repository URL.
// Wraps the same cargo.Client that Phase B's signal collector will
// use, so the "get me the source repo for this crate" capability is
// discoverable from one place — symmetric with npm and PyPI.
type CargoResolver struct {
	client *cargo.Client
}

// NewCargoResolver returns a resolver backed by a production
// cargo.Client (crates.io). Tests construct a resolver with
// NewCargoResolverWithClient and a client pointed at httptest.
func NewCargoResolver() *CargoResolver {
	return &CargoResolver{client: cargo.NewClient()}
}

// NewCargoResolverWithClient is the test-seam constructor. Pass a
// client pointed at an httptest.Server for hermetic tests.
func NewCargoResolverWithClient(c *cargo.Client) *CargoResolver {
	return &CargoResolver{client: c}
}

// ResolveSource calls the crates.io registry for the crate's declared
// repository URL, normalizes it to a github clone URL (or empty), and
// packages it as a DeclaredSource.
//
// Errors from the registry call (network, 404, body cap) surface to
// the caller verbatim. Successful-but-no-source cases return
// DeclaredSource{SelfReported: true} with empty URI/URL.
func (r *CargoResolver) ResolveSource(ctx context.Context, name string) (DeclaredSource, error) {
	repoURL, err := r.client.ResolveRepoURL(ctx, name)
	if err != nil {
		return DeclaredSource{}, err
	}
	if repoURL == "" {
		return DeclaredSource{SelfReported: true}, nil
	}
	// Route through ResolveTarget for canonical URI + clone URL
	// consistency — same delegation as npm and PyPI resolvers.
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
	// Register under both "cargo" (purl-spec canonical, used in
	// pkg:cargo/ URIs) and "crates" (the EcosystemCrates constant
	// in ecosystem/detect.go). Mirrors Go's dual "go" + "golang"
	// registration.
	r := NewCargoResolver()
	Default.Register("cargo", r)
	Default.Register("crates", r)
}
