package resolver

import (
	"context"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal/registry/gem"
)

// GemResolver implements Resolver for pkg:gem/<name> by consulting
// rubygems.org's JSON API and extracting the declared source_code_uri
// or homepage_uri. Wraps the same gem.Client that Phase B's signal
// collector uses, so the "get me the source repo for this gem"
// capability is discoverable from one place — symmetric with npm,
// pypi, and cargo.
type GemResolver struct {
	client *gem.Client
}

// NewGemResolver returns a resolver backed by a production gem.Client
// (rubygems.org). Tests construct a resolver with
// NewGemResolverWithClient and a client pointed at httptest.
func NewGemResolver() *GemResolver {
	return &GemResolver{client: gem.NewClient()}
}

// NewGemResolverWithClient is the test-seam constructor. Pass a
// client pointed at an httptest.Server for hermetic tests.
func NewGemResolverWithClient(c *gem.Client) *GemResolver {
	return &GemResolver{client: c}
}

// ResolveSource calls the rubygems.org registry for the gem's declared
// source_code_uri (falling back to homepage_uri), normalizes it to a
// github clone URL (or empty), and packages it as a DeclaredSource.
//
// Errors from the registry call (network, 404, body cap) surface to
// the caller verbatim. Successful-but-no-source cases return
// DeclaredSource{SelfReported: true} with empty URI/URL.
func (r *GemResolver) ResolveSource(ctx context.Context, name string) (DeclaredSource, error) {
	repoURL, err := r.client.ResolveRepoURL(ctx, name)
	if err != nil {
		return DeclaredSource{}, err
	}
	if repoURL == "" {
		return DeclaredSource{SelfReported: true}, nil
	}
	// Route through ResolveTarget for canonical URI + clone URL
	// consistency — same delegation as npm, pypi, and cargo resolvers.
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
	Default.Register("gem", NewGemResolver())
}
