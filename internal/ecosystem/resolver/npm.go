package resolver

import (
	"context"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal/registry/npm"
)

// NpmResolver implements Resolver for pkg:npm/<name> by consulting
// the npm registry's package metadata and normalizing the declared
// repository.url field. Wraps the existing npm.Client which is also
// used for signal collection — this resolver is a thin adapter so
// the "get me the source repo for this npm package" capability is
// discoverable from one place.
type NpmResolver struct {
	client *npm.Client
}

// NewNpmResolver returns a resolver backed by a production npm.Client
// (registry.npmjs.org). Tests construct a resolver with
// NewNpmResolverWithClient and a stubbed client.
func NewNpmResolver() *NpmResolver {
	return &NpmResolver{client: npm.NewClient()}
}

// NewNpmResolverWithClient is the test-seam constructor. Pass a
// client pointed at an httptest.Server for hermetic tests.
func NewNpmResolverWithClient(c *npm.Client) *NpmResolver {
	return &NpmResolver{client: c}
}

// ResolveSource calls the npm registry for the package's declared
// repository URL, normalizes it to a github clone URL (or empty),
// and packages it as a DeclaredSource.
//
// Errors from the registry call (network, 404, invalid JSON) surface
// to the caller verbatim. Successful-but-no-source cases return an
// empty DeclaredSource with nil error — the caller distinguishes
// "package doesn't declare source" (legitimate) from "we couldn't
// reach the registry" (transient).
func (r *NpmResolver) ResolveSource(ctx context.Context, name string) (DeclaredSource, error) {
	repoURL, err := r.client.ResolveRepoURL(ctx, name)
	if err != nil {
		return DeclaredSource{}, err
	}
	if repoURL == "" {
		return DeclaredSource{SelfReported: true}, nil
	}
	// Route the normalized github URL through ResolveTarget so the
	// canonical URI and clone URL stay consistent with what every
	// other target-accepting command computes.
	resolved, err := profile.ResolveTarget(repoURL)
	if err != nil {
		// Shouldn't happen — NormalizeDeclaredRepoURL inside the
		// npm client already constrains repoURL to a form
		// ResolveTarget accepts. Surface the error rather than
		// silently producing a URL-without-URI DeclaredSource.
		return DeclaredSource{}, err
	}
	return DeclaredSource{
		URI:          resolved.CanonicalURI,
		URL:          resolved.CloneURL,
		SelfReported: true,
	}, nil
}

func init() {
	Default.Register("npm", NewNpmResolver())
}
