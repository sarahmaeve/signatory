package store

import (
	"context"
	"errors"

	"github.com/sarahmaeve/signatory/internal/profile"
)

// EntityLookuper is the narrow interface LookupEntity uses. Defined
// here (rather than coupling the helper to the full Store interface)
// so callers with smaller subsets — and tests with hand-written fakes —
// can use the helper without satisfying every Store method.
type EntityLookuper interface {
	FindEntityByURI(ctx context.Context, canonicalURI string) (*profile.Entity, error)
	FindEntityByVersionedBaseURI(ctx context.Context, baseURI string) (*profile.Entity, error)
}

// LookupEntity finds an entity matching target, walking the canonical-
// URI alternates when exact matches miss. Centralizes the URI-fragmentation
// fallback logic so survey, MCP summary, and other lookup-side surfaces
// share the same equivalence rules.
//
// target may be a canonical URI (e.g. `pkg:go/golang.org/x/mod`) or a
// raw user input (e.g. `golang.org/x/mod`, `alecthomas/kong`,
// `https://github.com/foo/bar`). Both route through profile.ResolveTarget
// to get the canonical form; ResolveTarget owns vanity-Go-path detection,
// github shorthand parsing, and case-folding.
//
// Order:
//
//  1. Walk profile.AlternateURIs and try each via FindEntityByURI.
//     First exact-match wins.
//  2. For each unversioned alternate, fall back to
//     FindEntityByVersionedBaseURI to catch the testify-class M1
//     violation (entity row exists at <base>@V but caller queried
//     <base>). Skipping alternates that already carry @V — those
//     were tried verbatim in step 1, and a scan for "versions of
//     this version" has no useful semantics.
//
// Errors:
//
//   - ResolveTarget errors propagate verbatim (malformed input).
//   - Non-ErrNotFound errors from the lookups short-circuit the walk
//     and propagate (DB closed, etc).
//   - When every step misses, returns ErrNotFound — same sentinel
//     callers would have seen from a direct FindEntityByURI, so
//     downstream errors.Is(err, ErrNotFound) gating still works.
func LookupEntity(ctx context.Context, s EntityLookuper, target string) (*profile.Entity, error) {
	resolved, err := profile.ResolveTarget(target)
	if err != nil {
		return nil, err
	}

	alternates := profile.AlternateURIs(resolved.CanonicalURI)

	for _, uri := range alternates {
		ent, lookupErr := s.FindEntityByURI(ctx, uri)
		if lookupErr == nil {
			return ent, nil
		}
		if !errors.Is(lookupErr, ErrNotFound) {
			return nil, lookupErr
		}
	}

	for _, uri := range alternates {
		base, version := profile.SplitURIVersion(uri)
		if version != "" {
			continue
		}
		ent, lookupErr := s.FindEntityByVersionedBaseURI(ctx, base)
		if lookupErr == nil {
			return ent, nil
		}
		if !errors.Is(lookupErr, ErrNotFound) {
			return nil, lookupErr
		}
	}

	return nil, ErrNotFound
}
