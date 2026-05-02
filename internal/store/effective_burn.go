package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/sarahmaeve/signatory/internal/profile"
)

// EffectiveBurnContext describes the chain that produced an
// effective burn. Display callers (signatory analyze, summary,
// survey, MCP responses) read this to render "burned (via owner
// identity:github/X)" rather than just "burned" — so users can
// see which ledger entry actually caused the degradation.
//
// Direct == true: the burn is on the queried entity itself; the
// other fields are zero. ViaOwner == nil iff Direct == true.
//
// Direct == false: the cascade fired. ViaOwner is the identity:/
// org: entity whose burn cascades through to the queried target;
// ViaRole names the relation kind ("publisher", "maintainer") so
// renderers can phrase the cascade reason precisely.
type EffectiveBurnContext struct {
	// Direct reports whether the returned burn is on the queried
	// entity itself (true) or cascaded from a related identity
	// (false). Direct beats cascade when both apply — see
	// countercampaign.md §7.7.
	Direct bool

	// ViaOwner is the identity/org entity whose burn cascaded
	// through to the queried target. Populated only when
	// Direct == false. The renderer uses ViaOwner.CanonicalURI
	// to name the cause in user-facing output.
	ViaOwner *profile.Entity

	// ViaRole names the relation kind that produced the cascade.
	// Values today:
	//   - "publisher"  — github repo owner (owner_profile signal),
	//                    or npm per-version publisher
	//                    (publish_origin_consistency)
	//   - "maintainer" — npm Maintainers list (maintainer_count)
	//
	// Future producer collectors extend this set: "committer" /
	// "signer" for the git collector, etc. The string is stable;
	// downstream code may switch on it for role-specific rendering.
	ViaRole string
}

// EffectiveBurn returns the burn that should apply to entityID,
// walking related-identity relations encoded in the entity's
// signals. Path B's load-bearing primitive: composes existing
// store operations (GetBurn + GetLatestSignals + FindEntityByURI)
// with no new schema, so the cascade lives entirely in derivation.
//
// Resolution order:
//
//  1. Direct burn check via GetBurn(entityID). If found, return
//     the burn with Direct=true. Direct beats cascade.
//
//  2. Build the cascade-candidate list by walking the entity's
//     latest signals — the per-signal-type cases in
//     cascadeCandidates know which JSON fields name the
//     owners/maintainers/publishers for each signal kind.
//
//  3. For each candidate URI: FindEntityByURI; if the entity
//     exists AND has an active burn, return that burn with
//     Direct=false and ViaOwner / ViaRole populated. First
//     burned candidate wins.
//
//  4. None of the above → return ErrNotFound, matching GetBurn's
//     "no burn" contract so callers handle absence uniformly.
//
// Soft-delete is inherited through the underlying GetBurn calls:
// withdrawn rows filter out at that layer, so a previously-burned
// owner whose burn is withdrawn no longer cascades — no separate
// machinery needed.
//
// Cascade walks one hop only. An entity related to the queried
// target's owner does NOT cascade transitively; that's an
// identity-equivalence concern (entity-burn1.md §11) reserved for
// v0.2.
func (s *SQLite) EffectiveBurn(ctx context.Context, entityID string) (*profile.Burn, *EffectiveBurnContext, error) {
	// Step 1: direct check. Most queries land here on the happy
	// path (no cascade fires for the dominant healthy case).
	if direct, err := s.GetBurn(ctx, entityID); err == nil {
		return direct, &EffectiveBurnContext{Direct: true}, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, nil, fmt.Errorf("direct burn lookup: %w", err)
	}

	// Step 2: load the entity to know what kind of relations apply.
	// (cascadeCandidates branches on the signal type, not the
	// entity type, so loading the entity is currently informational —
	// kept here so future per-entity-type cascade rules have a
	// natural place to fire from.)
	if _, err := s.GetEntity(ctx, entityID); err != nil {
		return nil, nil, fmt.Errorf("load entity for cascade: %w", err)
	}

	// Step 3: walk the cascade candidates. Each candidate is one
	// (URI, role) pair derived from one signal value; the resolver
	// tries each in emission order and returns the first burned one.
	candidates, err := s.cascadeCandidates(ctx, entityID)
	if err != nil {
		return nil, nil, fmt.Errorf("build cascade candidates: %w", err)
	}
	for _, c := range candidates {
		owner, err := s.FindEntityByURI(ctx, c.URI)
		if errors.Is(err, ErrNotFound) {
			// Owner entity hasn't been minted yet (pre-Path-A data,
			// or analyze run that errored before reaching the mint
			// branch). No row → no burn possible → continue.
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("lookup cascade candidate %q: %w", c.URI, err)
		}

		burn, err := s.GetBurn(ctx, owner.ID)
		if errors.Is(err, ErrNotFound) {
			continue // owner exists but isn't burned
		}
		if err != nil {
			return nil, nil, fmt.Errorf("burn lookup for owner %q: %w", c.URI, err)
		}

		return burn, &EffectiveBurnContext{
			Direct:   false,
			ViaOwner: owner,
			ViaRole:  c.Role,
		}, nil
	}

	// Step 4: no direct, no cascade.
	return nil, nil, ErrNotFound
}

// cascadeCandidate is one (owner-URI, role) pair derived from a
// single signal value. The resolver iterates these to find the
// first burned related identity.
type cascadeCandidate struct {
	URI  string
	Role string
}

// cascadeCandidates extracts the related-identity URIs from an
// entity's latest signals. Each signal type the resolver knows
// about contributes zero or more candidates: github's
// owner_profile produces one (the repo owner); npm's
// maintainer_count and publish_origin_consistency each produce
// many (one per login).
//
// Signal types not in the switch contribute nothing — silent skip.
// New producer collectors extend the cascade by adding their type
// to the switch; tests in effective_burn_test.go cover each branch.
//
// Order matters for the "first burned wins" rule. Currently:
// owner_profile entries appear first (github repos have exactly
// one owner; the unambiguous case); then npm maintainers; then npm
// publishers. The latter two iterate the array in stored order,
// which is the order the npm collector emitted them.
func (s *SQLite) cascadeCandidates(ctx context.Context, entityID string) ([]cascadeCandidate, error) {
	signals, err := s.GetLatestSignals(ctx, entityID)
	if err != nil {
		return nil, fmt.Errorf("read signals: %w", err)
	}

	var out []cascadeCandidate
	for _, sig := range signals {
		switch sig.Type {
		case "owner_profile":
			// {"login":"...", "type":"User"|"Organization"} → one
			// identity:github/<login> or org:github/<login> URI.
			var v struct {
				Login string `json:"login"`
				Type  string `json:"type"`
			}
			if err := json.Unmarshal(sig.Value, &v); err != nil {
				continue // malformed value; skip rather than abort
			}
			if v.Login == "" {
				continue
			}
			var uri string
			if v.Type == "Organization" {
				uri = profile.CanonicalOrgURI("github", v.Login)
			} else {
				uri = profile.CanonicalIdentityURI("github", v.Login)
			}
			out = append(out, cascadeCandidate{URI: uri, Role: "publisher"})

		case "maintainer_count":
			// {"count":N, "logins":["a","b",...]} → one
			// identity:npm/<login> per login. Role: maintainer.
			var v struct {
				Logins []string `json:"logins"`
			}
			if err := json.Unmarshal(sig.Value, &v); err != nil {
				continue
			}
			for _, login := range v.Logins {
				if login == "" {
					continue
				}
				out = append(out, cascadeCandidate{
					URI:  profile.CanonicalIdentityURI("npm", login),
					Role: "maintainer",
				})
			}

		case "publish_origin_consistency":
			// {"publishers":["a","b",...], ...} → one
			// identity:npm/<login> per publisher. Role: publisher.
			// Publishers and maintainers can overlap; that's fine —
			// EffectiveBurn returns the first burned candidate and
			// dedupes implicitly (duplicate FindEntityByURI calls
			// for the same URI are idempotent and cheap).
			var v struct {
				Publishers []string `json:"publishers"`
			}
			if err := json.Unmarshal(sig.Value, &v); err != nil {
				continue
			}
			for _, login := range v.Publishers {
				if login == "" {
					continue
				}
				out = append(out, cascadeCandidate{
					URI:  profile.CanonicalIdentityURI("npm", login),
					Role: "publisher",
				})
			}
		}
	}
	return out, nil
}
