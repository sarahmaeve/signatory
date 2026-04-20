package survey

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/sarahmaeve/signatory/internal/manifest"
	"github.com/sarahmaeve/signatory/internal/manifest/gomod"
	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/store"
)

// Run surveys a project's dependency tree against the store.
//
// Steps:
//
//  1. Parse the manifest at manifestPath (ecosystem determined
//     by file suffix — only go.mod in v0.1).
//  2. For each dep, look up the entity in the store and resolve
//     the tier per the rules in this package's doc.
//  3. Aggregate counts into a Summary.
//
// Returns a Result with everything a renderer (terminal CLI, web
// UI, JSON consumer) needs. No output formatting happens here —
// that's the caller's job, intentionally, so the same Result can
// feed multiple surfaces without duplicating resolution logic.
func Run(ctx context.Context, s store.Store, manifestPath string) (Result, error) {
	info, deps, err := parseManifest(manifestPath)
	if err != nil {
		return Result{}, err
	}

	out := Result{
		Project: info,
		Deps:    make([]DepResult, 0, len(deps)),
		Summary: Summary{
			Total:  len(deps),
			ByTier: map[Tier]int{},
		},
	}

	for _, d := range deps {
		r := resolveDep(ctx, s, d)
		out.Deps = append(out.Deps, r)

		if d.Direct {
			out.Summary.Direct++
		} else {
			out.Summary.Indirect++
		}
		out.Summary.ByTier[r.Tier]++

		// Surface direct deps that need review (either unexplored
		// or never-seen) as actionable items. Indirect deps are
		// handled transitively — telling the user to analyze every
		// transitive dep floods their backlog with noise.
		if d.Direct && (r.Tier == TierNotInStore || r.Tier == TierUnexamined) {
			out.Summary.NeedsReview = append(out.Summary.NeedsReview, d.CanonicalURI)
		}
	}

	return out, nil
}

// parseManifest dispatches to the correct parser based on the
// manifest's filename. v0.1 is Go-only; additional ecosystems
// extend the switch as their parsers land.
//
// Returns error on unrecognized manifest — callers should either
// auto-detect via manifest.Detect (which already filters to known
// types) or pass a path whose extension signatory can handle.
func parseManifest(path string) (manifest.ProjectInfo, []manifest.Dep, error) {
	if path == "" {
		return manifest.ProjectInfo{}, nil, errors.New("manifest path is required")
	}
	// Filename-based dispatch. Cheap, deterministic, extensible.
	switch base := filepath.Base(path); base {
	case "go.mod":
		return gomod.Parse(path)
	default:
		return manifest.ProjectInfo{}, nil, fmt.Errorf("unrecognized manifest filename %q (supported in v0.1: go.mod)", base)
	}
}

// resolveDep is the tier-resolution logic, kept as a pure
// function over (store, dep) so it can be unit-tested without a
// real SQLite instance — the store interface is mocked in tests.
//
// Ordering matches the trust-policy sketch's Layer 0 (burn) →
// Layer 1 (explicit posture) → absent-case fallback.
func resolveDep(ctx context.Context, s store.Store, d manifest.Dep) DepResult {
	r := DepResult{Dep: d}

	// Local replaces can't be resolved against the store — there's
	// no remote source to assess. Surface as a distinct tier so
	// renderers can message "local fork, not analyzable remotely."
	if d.Ecosystem == "go-local-replace" {
		r.Tier = TierLocalReplace
		return r
	}

	// Empty canonical URI means the parser couldn't build one
	// (malformed input). Report as not-in-store — the most honest
	// tier for "nothing to look up."
	if d.CanonicalURI == "" {
		r.Tier = TierNotInStore
		return r
	}

	entity, err := s.FindEntityByURI(ctx, d.CanonicalURI)
	if errors.Is(err, store.ErrNotFound) {
		r.Tier = TierNotInStore
		return r
	}
	if err != nil {
		// Unexpected error from the store (I/O, migration mismatch,
		// etc.). Treat as not-in-store for display purposes — the
		// dep appears absent to this survey run. Log-level handling
		// is the caller's choice; we don't want a transient DB hiccup
		// to panic the whole survey.
		r.Tier = TierNotInStore
		return r
	}

	// Layer 0: burns win absolutely.
	burn, burnErr := s.GetBurn(ctx, entity.ID)
	if burnErr == nil && burn != nil {
		r.Tier = TierBurned
		r.BurnReason = burn.Reason
		return r
	}

	// Layer 1: explicit postures. Prefer exact version match.
	postures, postureErr := s.GetPostures(ctx, entity.ID)
	if postureErr != nil || len(postures) == 0 {
		// Entity exists but no postures recorded → unexamined.
		// Treat GetPostures errors the same way: conservative
		// default rather than a misleading specific tier.
		r.Tier = TierUnexamined
		return r
	}

	for _, p := range postures {
		if p.Version == d.Version {
			r.Tier = postureTierToSurveyTier(p.Tier)
			r.PostureVersion = p.Version
			r.PostureRationale = p.Rationale
			return r
		}
	}

	// Postures exist but none for this version. This is
	// "unexamined" with an informative flag — renderers can show
	// "v1.14 is vetted-frozen but you pinned v1.15."
	r.Tier = TierUnexamined
	r.HasOtherVersions = true
	return r
}

// postureTierToSurveyTier maps the profile.PostureTier constants
// to the Tier strings survey uses. They're near-identical by
// design — survey's Tier is a superset that adds TierBurned,
// TierNotInStore, and TierLocalReplace.
//
// Keeping separate types lets survey's Tier serialize cleanly
// to JSON without leaking the profile package's internal enum
// representation.
func postureTierToSurveyTier(p profile.PostureTier) Tier {
	switch p {
	case profile.PostureVettedFrozen:
		return TierVettedFrozen
	case profile.PostureTrustedForNow:
		return TierTrustedForNow
	case profile.PostureUnexamined:
		return TierUnexamined
	case profile.PostureUnknownProvenance:
		return TierUnknownProvenance
	case profile.PostureRejected:
		return TierRejected
	default:
		// Unknown posture tier — possibly from a future schema.
		// Fall back to unexamined as the most honest "we don't
		// know what to do with this" answer.
		return TierUnexamined
	}
}

