// Package resolver maps `pkg:<ecosystem>/<name>` canonical URIs to
// their declared source repositories. It is the pluggable extension
// point agent-facing-contract M3 calls out: every caller that needs
// "given a pkg URI, where's the source?" — `--network-precheck`,
// `--clone-dir`, the /analyze skill's source resolution — goes
// through this package instead of hand-rolling per-ecosystem branches.
//
// Registered resolvers are queried by ecosystem name (`npm`, `go`,
// `pypi`, `cargo`). Callers that need to support a new ecosystem
// implement Resolver and call Register on the registry; nothing else
// in the codebase needs to change.
//
// Contract note (agent-facing-contract.md §3.7): every DeclaredSource
// a caller receives is self-reported — publishers declare their
// repository URL in registry metadata, and we trust that declaration
// with appropriate disclosure to the end user (the precheck stderr
// report discloses the resolved URL verbatim). Cryptographic binding
// between a package and its source is a v0.2+ topic; this interface
// leaves room for it via DeclaredSource.SelfReported.
package resolver

import (
	"context"
	"errors"
	"fmt"
)

// Resolver maps a package name (within a single ecosystem) to its
// declared source repository. Implementations live in sibling files
// in this package — npm.go, gomod.go, etc. — and register themselves
// with the default Registry at init time.
//
// Input `name` is the package name *within the ecosystem*, not a
// canonical URI — the registry strips the `pkg:<eco>/` prefix before
// calling. For scoped npm packages this is the full `@scope/name`.
type Resolver interface {
	// ResolveSource returns the declared source repository for a
	// package. Returns (empty DeclaredSource, nil) when the registry
	// has the package but declares no source — a legitimate case
	// callers distinguish from "the package doesn't exist" (which
	// surfaces as an error) or "we couldn't reach the registry"
	// (also an error).
	ResolveSource(ctx context.Context, name string) (DeclaredSource, error)
}

// DeclaredSource describes a package's source repository as declared
// by the package publisher (or, in future, as cryptographically
// verified).
type DeclaredSource struct {
	// URI is the canonical signatory URI of the source — e.g.
	// "repo:github/expressjs/express". Empty when the ecosystem has
	// the package but declares no source repository.
	URI string

	// URL is the clone URL for the source, convenient for callers
	// that need to `git clone` rather than normalize. Empty
	// whenever URI is empty. For github sources this is
	// "https://github.com/<owner>/<name>".
	URL string

	// SelfReported is true when the URI / URL come from publisher-
	// supplied registry metadata (the current case for every
	// shipped resolver). A future resolver that cross-checks a
	// signed provenance attestation would set this to false and
	// populate VerifiedBy below.
	//
	// The precheck / clone / analyze callers that route through
	// this package surface SelfReported in their stderr reports so
	// a human skimming the output can see that the source-repo
	// claim is uncryptographically-bound.
	SelfReported bool

	// VerifiedBy names the attestation chain that verified this
	// source-of-package binding, if any. Empty when SelfReported
	// is true. Reserved for v0.2+ (sigstore, SLSA, etc.).
	VerifiedBy string
}

// ErrNoResolver is returned by Registry.Resolve when no resolver is
// registered for the ecosystem the caller requested. Callers check
// with errors.Is to distinguish "we don't support this ecosystem yet"
// from other failure modes.
var ErrNoResolver = errors.New("no resolver registered for ecosystem")

// Registry holds the set of registered resolvers, keyed by ecosystem
// name. The zero value is usable (empty registry); typical use is
// the package-level Default instance below.
type Registry struct {
	resolvers map[string]Resolver
}

// NewRegistry returns an empty registry. Callers that want a
// standalone registry (e.g. tests constructing one with stubbed
// resolvers) use this; production code goes through Default.
func NewRegistry() *Registry {
	return &Registry{resolvers: map[string]Resolver{}}
}

// Register associates resolver with ecosystem. Safe to call from
// init(), since production callers only read Default after init
// completes. Overwriting an existing registration is allowed — tests
// use this to swap a stubbed resolver for a real one.
func (r *Registry) Register(ecosystem string, resolver Resolver) {
	if r.resolvers == nil {
		r.resolvers = map[string]Resolver{}
	}
	r.resolvers[ecosystem] = resolver
}

// Resolve dispatches to the registered resolver for ecosystem. Wraps
// ErrNoResolver when the ecosystem has no registered resolver, and
// returns the resolver's error verbatim otherwise.
func (r *Registry) Resolve(ctx context.Context, ecosystem, name string) (DeclaredSource, error) {
	if r.resolvers == nil {
		return DeclaredSource{}, fmt.Errorf("%w: %q", ErrNoResolver, ecosystem)
	}
	res, ok := r.resolvers[ecosystem]
	if !ok {
		return DeclaredSource{}, fmt.Errorf("%w: %q", ErrNoResolver, ecosystem)
	}
	return res.ResolveSource(ctx, name)
}

// Ecosystems returns the sorted list of ecosystems this registry can
// resolve. Useful for error messages that list supported ecosystems,
// and for `signatory --help`-style diagnostics.
func (r *Registry) Ecosystems() []string {
	out := make([]string, 0, len(r.resolvers))
	for k := range r.resolvers {
		out = append(out, k)
	}
	// Simple insertion sort — the list is tiny and package-level
	// sort imports would pull more weight than this is worth.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// Default is the process-global registry. Sibling files (npm.go,
// gomod.go) register their resolvers into Default during init.
// Callers that need test isolation construct a fresh *Registry via
// NewRegistry.
var Default = NewRegistry()
