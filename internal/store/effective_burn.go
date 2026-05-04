package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

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
	//                    or npm/pypi per-version publisher
	//                    (publish_origin_consistency)
	//   - "maintainer" — npm/pypi Maintainers list (maintainer_count)
	//   - "signer"     — git per-developer GPG key
	//                    (commit_signing_keys)
	//
	// Future producer collectors extend this set ("committer" for
	// git author entities, etc.). The string is stable; downstream
	// code may switch on it for role-specific rendering.
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
// single signal value or from URI structure. The resolver iterates
// these to find the first burned related identity.
type cascadeCandidate struct {
	URI  string
	Role string
}

// EffectiveBurnByURI is the pre-collection-gate companion to
// EffectiveBurn: given only a canonical URI (the entity row may
// not exist yet in the store), decide whether a related identity
// is burned. Used by `signatory analyze --refresh` to refuse
// running collectors against a target whose owner is already
// burned, before doing any network or filesystem work.
//
// Two layers walked in order:
//
//  1. URI-derived candidates — for repo:github/X/Y, derive
//     identity:github/X AND org:github/X (we don't know User vs
//     Organization without a collect; check both). For scoped npm
//     packages (pkg:npm/@scope/name), derive identity:npm/scope
//     and org:npm/scope. Returns one cascadeCandidate per derived
//     URI; first burned wins.
//
//  2. Signal-derived candidates — when an entity row exists at
//     the queried URI, delegate to EffectiveBurn(entity.ID),
//     which walks the owner_profile / maintainer_count /
//     publish_origin_consistency signals as Path B already does.
//
// The two layers complement: layer 1 catches "brand-new repo by
// burned operator" (no entity, no signals); layer 2 catches the
// general case where the entity is in the store and we have its
// full relation graph cached.
//
// Returns ErrNotFound when neither layer finds a burned related
// identity — symmetric with EffectiveBurn / GetBurn so callers
// handle absence uniformly.
//
// Direct burn on the queried entity (when it exists) propagates
// through the layer-2 EffectiveBurn call with Direct=true.
func (s *SQLite) EffectiveBurnByURI(ctx context.Context, canonicalURI string) (*profile.Burn, *EffectiveBurnContext, error) {
	// Layer 2 first: if the entity already exists, EffectiveBurn
	// has the full relation graph (signals + direct check) and is
	// strictly more informative than URI-derived candidates alone.
	entity, err := s.FindEntityByURI(ctx, canonicalURI)
	if err == nil {
		burn, ebCtx, err := s.EffectiveBurn(ctx, entity.ID)
		if err == nil {
			return burn, ebCtx, nil
		}
		if !errors.Is(err, ErrNotFound) {
			return nil, nil, fmt.Errorf("entity-keyed cascade lookup: %w", err)
		}
		// Entity exists but no effective burn — fall through to
		// URI-derived candidates. (Edge case: a freshly-minted
		// repo entity might exist without an owner_profile signal
		// yet; URI-derived candidates would still catch the
		// operator burn.)
	} else if !errors.Is(err, ErrNotFound) {
		return nil, nil, fmt.Errorf("lookup entity by URI %q: %w", canonicalURI, err)
	}

	// Layer 1: URI-derived candidates.
	for _, c := range candidatesFromURI(canonicalURI) {
		owner, err := s.FindEntityByURI(ctx, c.URI)
		if errors.Is(err, ErrNotFound) {
			continue // candidate URI has no entity row (and thus no burn)
		}
		if err != nil {
			return nil, nil, fmt.Errorf("lookup URI-derived candidate %q: %w", c.URI, err)
		}
		burn, err := s.GetBurn(ctx, owner.ID)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("burn lookup for URI-derived candidate %q: %w", c.URI, err)
		}
		return burn, &EffectiveBurnContext{
			Direct:   false,
			ViaOwner: owner,
			ViaRole:  c.Role,
		}, nil
	}

	return nil, nil, ErrNotFound
}

// candidatesFromURI derives the related-identity URIs encoded by
// the structure of the canonical URI itself, with no signals
// required. This is what makes a pre-collection burn-gate
// possible: the github part of "repo:github/bufferzonecorp/grpc-
// client" alone says "owner is bufferzonecorp", and that's enough
// to refuse collection against a burned operator's brand-new repo.
//
// Coverage today:
//
//   - repo:github/<owner>/<name> → identity:github/<owner>,
//     org:github/<owner> (User vs Organization indeterminate from
//     URI alone; both checked)
//   - pkg:npm/@<scope>/<name>    → identity:npm/<scope>,
//     org:npm/<scope> (npm scopes usually map to orgs but the
//     scheme allows user-owned scopes too)
//   - pkg:maven/<groupId>/<artifact> → org:maven/<groupId>
//     (Maven Central verifies groupId namespace ownership; every
//     Maven package has one, so cascade coverage is universal)
//
// Other URI shapes (unscoped npm, pypi, golang vanity hosts,
// non-pkg/non-repo schemes) return an empty slice — the cascade
// resolver falls back to signal-derived candidates only for
// those. Adding new URI-derived patterns here is mechanical when
// a new ecosystem's URI structure encodes ownership in the path.
func candidatesFromURI(uri string) []cascadeCandidate {
	// repo:github/<owner>/<name>
	if rest, ok := strings.CutPrefix(uri, "repo:github/"); ok {
		owner, _, found := strings.Cut(rest, "/")
		if !found || owner == "" {
			return nil
		}
		return []cascadeCandidate{
			{URI: profile.CanonicalIdentityURI("github", owner), Role: "publisher"},
			{URI: profile.CanonicalOrgURI("github", owner), Role: "publisher"},
		}
	}

	// pkg:npm/@<scope>/<name> — scoped npm packages only. Unscoped
	// (pkg:npm/<name>) returns nil; the package name is NOT a
	// publisher login and must not be speculatively checked, or
	// we'd produce false-positive cascades on name collisions
	// (a malicious user `foo` would burn the unrelated package
	// pkg:npm/foo).
	if rest, ok := strings.CutPrefix(uri, "pkg:npm/@"); ok {
		scope, _, found := strings.Cut(rest, "/")
		if !found || scope == "" {
			return nil
		}
		return []cascadeCandidate{
			{URI: profile.CanonicalIdentityURI("npm", scope), Role: "maintainer"},
			{URI: profile.CanonicalOrgURI("npm", scope), Role: "maintainer"},
		}
	}

	// pkg:maven/<groupId>/<artifactId> — Maven Central packages.
	// Every Maven package has a groupId, and Sonatype verifies
	// namespace ownership before granting one, so the groupId is a
	// verified namespace entity. Burning org:maven/<groupId> cascades
	// to all artifacts under that group — universal coverage (unlike
	// npm where only scoped packages encode ownership).
	if rest, ok := strings.CutPrefix(uri, "pkg:maven/"); ok {
		groupID, _, found := strings.Cut(rest, "/")
		if !found || groupID == "" {
			return nil
		}
		return []cascadeCandidate{
			{URI: profile.CanonicalOrgURI("maven", groupID), Role: "publisher"},
		}
	}

	return nil
}

// platformForRegistrySource maps a registry-collector signal source
// (the value collectors set in profile.Signal.Source for shared
// signal types like maintainer_count and publish_origin_consistency)
// to the platform string used in CanonicalIdentityURI. Returns an
// empty string for unrecognized sources — the cascade resolver
// treats that as "no candidate", keeping the dispatch fail-shut.
//
// Adding a new ecosystem collector to the cascade requires extending
// this switch by one case and registering the same source string in
// the collector. The compile-time pairing (collector source ↔
// resolver dispatch) is intentional: silent default-to-npm would
// produce phantom-cascade candidates for any future collector that
// emitted maintainer_count from a non-npm source without thinking
// through the cascade implications.
func platformForRegistrySource(source string) string {
	switch source {
	case "npm-registry":
		return "npm"
	case "pypi-registry":
		return "pypi"
	default:
		return ""
	}
}

// cascadeCandidates extracts the related-identity URIs from an
// entity's latest signals. Each signal type the resolver knows
// about contributes zero or more candidates: github's
// owner_profile produces one (the repo owner); maintainer_count
// and publish_origin_consistency each produce many (one per login).
//
// The latter two are SHARED signal types — npm and pypi collectors
// both emit them with the same {count, logins} / {publishers, ...}
// schema — so the resolver dispatches on sig.Source via
// platformForRegistrySource to construct the right identity URI
// (identity:npm/<login> vs identity:pypi/<login>). Signals from
// unknown sources contribute no candidates (fail-shut).
//
// Signal types not in the switch contribute nothing — silent skip.
// New producer collectors extend the cascade by adding their type
// to the switch (or, for shared types, registering their source in
// platformForRegistrySource); tests in effective_burn_test.go cover
// each branch.
//
// Order matters for the "first burned wins" rule. Currently:
// owner_profile entries appear first (github repos have exactly
// one owner; the unambiguous case); then maintainers; then
// publishers. The latter two iterate the array in stored order,
// which is the order the source collector emitted them.
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
			// identity:<platform>/<login> per login. Role: maintainer.
			//
			// Platform comes from sig.Source: maintainer_count is
			// emitted by both npm-registry and pypi-registry collectors
			// with identical schema, so the resolver dispatches on
			// source to pick the right identity URI. Unknown sources
			// produce no candidates — fail-shut keeps new ecosystem
			// collectors honest about declaring their dispatch here.
			platform := platformForRegistrySource(sig.Source)
			if platform == "" {
				continue
			}
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
					URI:  profile.CanonicalIdentityURI(platform, login),
					Role: "maintainer",
				})
			}

		case "publish_origin_consistency":
			// {"publishers":["a","b",...], ...} → one
			// identity:<platform>/<login> per publisher. Role: publisher.
			// Publishers and maintainers can overlap; that's fine —
			// EffectiveBurn returns the first burned candidate and
			// dedupes implicitly (duplicate FindEntityByURI calls
			// for the same URI are idempotent and cheap).
			//
			// Same source-dispatch rationale as maintainer_count:
			// platform is read from sig.Source.
			platform := platformForRegistrySource(sig.Source)
			if platform == "" {
				continue
			}
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
					URI:  profile.CanonicalIdentityURI(platform, login),
					Role: "publisher",
				})
			}

		case "commit_signing_keys":
			// {"count":N, "key_ids":["abc","def",...]} → one
			// identity:gpg/<keyid> per distinct per-developer key.
			// Role: signer.
			//
			// No source dispatch — commit_signing_keys is git-only
			// (the signal's only emitter is the git collector) and
			// the platform is always "gpg". If a future collector
			// emits SSH-signed-commit keys or sigstore identities,
			// this case grows a sub-switch (or splits into separate
			// signal types — same call as for maintainer_count vs
			// publish_origin_consistency).
			//
			// Web-flow keys (GitHub's managed signing key) are
			// excluded at the producer side (signing.go), not here —
			// this case sees only per-developer keys by contract.
			// Limitation: same person rotating GPG keys produces
			// distinct entity rows; burning one doesn't catch the
			// other. Identity-equivalence work (entity-burn1.md §11
			// / "Pending work #4") closes that gap; for v0.1, the
			// limitation is accepted explicitly.
			var v struct {
				KeyIDs []string `json:"key_ids"`
			}
			if err := json.Unmarshal(sig.Value, &v); err != nil {
				continue
			}
			for _, keyID := range v.KeyIDs {
				if keyID == "" {
					continue
				}
				out = append(out, cascadeCandidate{
					URI:  profile.CanonicalIdentityURI("gpg", keyID),
					Role: "signer",
				})
			}
		}
	}
	return out, nil
}
