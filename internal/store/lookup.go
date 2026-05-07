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
//
// HasPostures is the "richness" probe used by the weight-aware
// alternate walk. Returns true when the entity has at least one
// non-withdrawn posture row attached. Used by LookupEntity to prefer
// posture-bearing alternates over thin (analyses-only or empty)
// alternates in the cross-row fragmentation case.
type EntityLookuper interface {
	FindEntityByURI(ctx context.Context, canonicalURI string) (*profile.Entity, error)
	FindEntityByVersionedBaseURI(ctx context.Context, baseURI string) (*profile.Entity, error)
	HasPostures(ctx context.Context, entityID string) (bool, error)
}

// LookupEntity finds an entity matching target, walking the canonical-
// URI alternates when exact matches miss. Centralizes the URI-fragmentation
// fallback logic so survey, MCP summary, and other lookup-side surfaces
// share the same equivalence rules.
//
// target may be a canonical URI (e.g. `pkg:golang/golang.org/x/mod`)
// or a raw user input (e.g. `golang.org/x/mod`, `alecthomas/kong`,
// `https://github.com/foo/bar`). Both route through profile.ResolveTarget
// to get the canonical form; ResolveTarget owns vanity-Go-path detection,
// github shorthand parsing, and case-folding.
//
// Walk order and weight-aware preference:
//
//  1. Walk profile.AlternateURIs through FindEntityByURI. For each
//     hit, probe HasPostures. If postures exist, return immediately
//     (rich hit short-circuits). If not, remember the first hit as
//     a fallback and keep walking.
//  2. For each unversioned alternate, fall back to
//     FindEntityByVersionedBaseURI to catch the testify-class M1
//     violation (entity row exists at <base>@V but caller queried
//     <base>). Same posture-preference rule.
//  3. If nothing rich was found but at least one thin hit was seen,
//     return that thin hit. Otherwise ErrNotFound.
//
// The weight-aware step is a safeguard against cross-row fragmentation
// surfaced by 2026-04-28 dogfood: yaml.v3 had its posture at one
// alternate URI and analyses at another, so naive first-hit walk
// returned the analyses-only row and survey reported "unexamined"
// despite the posture existing. Preferring posture-bearing hits
// resolves that without consolidating the rows. The full structural
// fix (consolidate via migration v13) remains the right long-term move.
//
// Errors:
//
//   - ResolveTarget errors propagate verbatim (malformed input).
//   - Non-ErrNotFound errors from any lookup short-circuit the walk
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

	// firstThinHit holds the earliest-by-walk-order hit whose entity
	// has no postures. Returned as the fallback if no rich hit is
	// found anywhere in the walk.
	var firstThinHit *profile.Entity

	considerHit := func(ent *profile.Entity) (*profile.Entity, error) {
		hasPostures, err := s.HasPostures(ctx, ent.ID)
		if err != nil {
			return nil, err
		}
		if hasPostures {
			return ent, nil
		}
		if firstThinHit == nil {
			firstThinHit = ent
		}
		return nil, nil
	}

	for _, uri := range alternates {
		ent, lookupErr := s.FindEntityByURI(ctx, uri)
		if errors.Is(lookupErr, ErrNotFound) {
			continue
		}
		if lookupErr != nil {
			return nil, lookupErr
		}
		rich, err := considerHit(ent)
		if err != nil {
			return nil, err
		}
		if rich != nil {
			return rich, nil
		}
	}

	for _, uri := range alternates {
		base, version := profile.SplitURIVersion(uri)
		if version != "" {
			continue
		}
		ent, lookupErr := s.FindEntityByVersionedBaseURI(ctx, base)
		if errors.Is(lookupErr, ErrNotFound) {
			continue
		}
		if lookupErr != nil {
			return nil, lookupErr
		}
		rich, err := considerHit(ent)
		if err != nil {
			return nil, err
		}
		if rich != nil {
			return rich, nil
		}
	}

	if firstThinHit != nil {
		return firstThinHit, nil
	}
	return nil, ErrNotFound
}

// LookupEntityID is the filter-resolution sugar wrapper around
// LookupEntity for callers that want an EntityID (suitable for a
// store filter's EntityID field) rather than the full *profile.Entity.
//
// Empty target returns ("", nil) — callers that treat empty input as
// "no filter" can chain straight into a filter struct without
// nil-checking. Non-empty input goes through LookupEntity and
// inherits its alternate-URI walking and weight-aware preference.
//
// Errors propagate verbatim from LookupEntity:
//
//   - ErrNotFound when the alternate walk found nothing (the show-*
//     commands' "no entity matches" branch fires from this).
//   - profile.ResolveTarget errors when the input doesn't parse as
//     any recognized URI form (caller-shape; CLI/MCP map this to
//     a usage/schema error).
//   - Other errors from underlying lookups (DB closed, etc).
//
// Added 2026-05-07 to retrofit alternate-URI walking onto the show-*
// CLI/MCP read paths. Without this, queries like `show-analyses
// golang.org/x/mod` missed analyses indexed under the equivalent
// repo:github/golang/mod even though `summary golang.org/x/mod`
// resolved them via the same walk. See lookup.go:LookupEntity for
// the underlying walk semantics this delegates to.
func LookupEntityID(ctx context.Context, s EntityLookuper, target string) (string, error) {
	if target == "" {
		return "", nil
	}
	ent, err := LookupEntity(ctx, s, target)
	if err != nil {
		return "", err
	}
	return ent.ID, nil
}
