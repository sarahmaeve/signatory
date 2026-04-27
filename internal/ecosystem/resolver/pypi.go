package resolver

import (
	"context"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal/registry/pypi"
)

// PyPIResolver implements Resolver for pkg:pypi/<name> by consulting
// PyPI's JSON registry endpoint and walking the publisher's declared
// project_urls map (with the deprecated info.home_page as final
// fallback) for a github source repository. Wraps the same
// pypi.Client that Layer 5's signal collector will use when it
// ships, so the "get me the source repo for this PyPI package"
// capability is discoverable from one place — symmetric with the
// npm resolver.
type PyPIResolver struct {
	client *pypi.Client
}

// NewPyPIResolver returns a resolver backed by a production
// pypi.Client (pypi.org). Tests construct a resolver with
// NewPyPIResolverWithClient and a client pointed at httptest.
func NewPyPIResolver() *PyPIResolver {
	return &PyPIResolver{client: pypi.NewClient()}
}

// NewPyPIResolverWithClient is the test-seam constructor. Pass a
// client pointed at an httptest.Server for hermetic tests.
func NewPyPIResolverWithClient(c *pypi.Client) *PyPIResolver {
	return &PyPIResolver{client: c}
}

// ResolveSource calls the PyPI registry for the package's declared
// repository URL, normalizes it to a github clone URL (or empty),
// and packages it as a DeclaredSource.
//
// Errors from the registry call (network, 404, body cap, malformed
// JSON) surface to the caller verbatim. Successful-but-no-source
// cases return DeclaredSource{SelfReported: true} with empty
// URI/URL — the caller distinguishes "package doesn't declare a
// resolvable github source" (legitimate) from "we couldn't reach
// the registry" (transient).
func (r *PyPIResolver) ResolveSource(ctx context.Context, name string) (DeclaredSource, error) {
	repoURL, err := r.client.ResolveRepoURL(ctx, name)
	if err != nil {
		return DeclaredSource{}, err
	}
	if repoURL == "" {
		return DeclaredSource{SelfReported: true}, nil
	}
	// Route the normalized github URL through ResolveTarget so the
	// canonical URI and clone URL stay consistent with what every
	// other target-accepting command computes — same delegation
	// the npm resolver uses.
	resolved, err := profile.ResolveTarget(repoURL)
	if err != nil {
		// Shouldn't happen — NormalizeDeclaredRepoURL inside the
		// pypi client already constrains repoURL to a form
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
	Default.Register("pypi", NewPyPIResolver())
}
