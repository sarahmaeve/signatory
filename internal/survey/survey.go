package survey

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/sarahmaeve/signatory/internal/manifest"
	"github.com/sarahmaeve/signatory/internal/manifest/gomod"
	npmmanifest "github.com/sarahmaeve/signatory/internal/manifest/npm"
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
		r, err := resolveDep(ctx, s, d)
		if err != nil {
			return Result{}, fmt.Errorf("resolve dep %s: %w", d.CanonicalURI, err)
		}
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

	// Reachability pass: attempt to extract the dep graph and
	// bucket indirects by which directs reach them. Best-effort:
	// when the parser returns ErrGraphUnavailable (graph extraction
	// not implemented for this ecosystem, or the toolchain failed),
	// we proceed with zero-valued IndirectByReachability and the
	// renderer falls back to the no-graph rendering.
	if g, err := parseGraph(ctx, manifestPath); err == nil {
		populateReachability(g, &out)
	}
	// Note: we deliberately discard the err. Graph absence is
	// non-fatal — the user-visible survey still completes — and
	// the rendering layer signals "drill-down unavailable" when
	// the breakdown is zero-valued.

	return out, nil
}

// parseGraph dispatches to the per-ecosystem graph parser based
// on the manifest's filename. Mirrors parseManifest's dispatch
// shape. Returns manifest.ErrGraphUnavailable for ecosystems
// without graph extraction implemented (npm in v0.1 — follow-up
// commit will land it).
func parseGraph(ctx context.Context, path string) (manifest.Graph, error) {
	if path == "" {
		return manifest.Graph{}, fmt.Errorf("%w: manifest path is required",
			manifest.ErrGraphUnavailable)
	}
	switch base := filepath.Base(path); base {
	case "go.mod":
		return gomod.ParseGraph(ctx, path)
	case "package.json":
		// npm graph extraction is a follow-up commit.
		return manifest.Graph{}, fmt.Errorf("%w: npm graph extraction is a v0.1 follow-up",
			manifest.ErrGraphUnavailable)
	default:
		return manifest.Graph{}, fmt.Errorf("%w: unrecognized manifest %q",
			manifest.ErrGraphUnavailable, base)
	}
}

// populateReachability runs the reachability + bucketing passes
// against an already-resolved Result and writes the outputs to
// the result in place. Split from Run() so the integration test
// can construct a result + graph in-memory without going through
// the file-based dispatch.
func populateReachability(g manifest.Graph, out *Result) {
	// Build the (URI → Tier) map for direct deps so the
	// bucketing pass can classify each indirect's reaching
	// parents in O(1).
	directs := make([]manifest.Dep, 0, out.Summary.Direct)
	directTier := make(map[string]Tier, out.Summary.Direct)
	for _, d := range out.Deps {
		if d.Dep.Direct {
			directs = append(directs, d.Dep)
			directTier[d.Dep.CanonicalURI] = d.Tier
		}
	}

	reach := computeReachability(g, directs)
	if reach == nil {
		// Empty graph or no directs — nothing to bucket. Leave
		// IndirectByReachability zero-valued so renderers fall
		// back to no-graph rendering.
		return
	}

	// Annotate each indirect DepResult with its reachability data.
	for i := range out.Deps {
		if out.Deps[i].Dep.Direct {
			continue
		}
		if r, ok := reach[out.Deps[i].Dep.CanonicalURI]; ok {
			out.Deps[i].Reachability = r
		}
	}

	out.Summary.IndirectByReachability = bucketIndirects(out.Deps, directTier)
}

// parseManifest dispatches to the correct parser based on the
// manifest's filename. v0.1 supports Go (go.mod) and npm
// (package.json); additional ecosystems extend the switch as
// their parsers land.
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
	case "package.json":
		return npmmanifest.Parse(path)
	default:
		return manifest.ProjectInfo{}, nil, fmt.Errorf("unrecognized manifest filename %q (supported in v0.1: go.mod, package.json)", base)
	}
}

// resolveDep is the tier-resolution logic, kept as a pure
// function over (store, dep) so it can be unit-tested without a
// real SQLite instance — the store interface is mocked in tests.
//
// Ordering matches the trust-policy sketch's Layer 0 (burn) →
// Layer 1 (explicit posture) → absent-case fallback.
//
// Returns a non-nil error only for unexpected store failures
// (i.e. errors that are not store.ErrNotFound). ErrNotFound is
// the normal "not seen yet" signal and is handled inline as a
// tier assignment. Propagating unexpected errors is correct on
// the trust-answering path: a storage hiccup must never silently
// render a burned dep as "unexamined."
func resolveDep(ctx context.Context, s store.Store, d manifest.Dep) (DepResult, error) {
	r := DepResult{Dep: d}

	// Local replaces can't be resolved against the store — there's
	// no remote source to assess. Surface as a distinct tier so
	// renderers can message "local fork, not analyzable remotely."
	if d.Ecosystem == "go-local-replace" {
		r.Tier = TierLocalReplace
		return r, nil
	}

	// Empty canonical URI means the parser couldn't build one
	// (malformed input). Report as not-in-store — the most honest
	// tier for "nothing to look up."
	if d.CanonicalURI == "" {
		r.Tier = TierNotInStore
		return r, nil
	}

	entity, err := s.FindEntityByURI(ctx, d.CanonicalURI)
	if errors.Is(err, store.ErrNotFound) {
		r.Tier = TierNotInStore
		return r, nil
	}
	if err != nil {
		return DepResult{}, err
	}

	// Layer 0: burns win absolutely.
	burn, burnErr := s.GetBurn(ctx, entity.ID)
	if burnErr != nil && !errors.Is(burnErr, store.ErrNotFound) {
		return DepResult{}, burnErr
	}
	if burnErr == nil && burn != nil {
		r.Tier = TierBurned
		r.BurnReason = burn.Reason
		return r, nil
	}

	// Layer 1: explicit postures. Prefer exact version match.
	postures, postureErr := s.GetPostures(ctx, entity.ID)
	if postureErr != nil && !errors.Is(postureErr, store.ErrNotFound) {
		return DepResult{}, postureErr
	}
	if postureErr != nil || len(postures) == 0 {
		// Entity exists but no postures recorded → unexamined.
		r.Tier = TierUnexamined
		return r, nil
	}

	for _, p := range postures {
		if p.Version == d.Version {
			r.Tier = postureTierToSurveyTier(p.Tier)
			r.PostureVersion = p.Version
			r.PostureRationale = p.Rationale
			return r, nil
		}
	}

	// Postures exist but none for this version. Return unexamined
	// and carry an OtherVersionsSummary so renderers can surface
	// the most-recent verdict on a different version plus a count
	// of all postures on record. Visibility only — no
	// recommendation.
	r.Tier = TierUnexamined
	r.OtherVersions = summarizeOtherVersionPostures(postures)
	return r, nil
}

// summarizeOtherVersionPostures builds an OtherVersionsSummary
// from a slice of postures that does NOT contain an exact-version
// match for the queried dep. All postures passed in are, by
// construction, for OTHER versions.
//
// Picks the most-recent-set_at posture as the "most relevant"
// view. Rationale: matches the user's likely mental model
// ("what's the latest thing my team thought about this?") and
// matches `posture get`'s default which also returns the most
// recent. Ties are broken by iteration order of the input slice,
// which is stable across runs for a given store.
//
// Returns nil when the input is empty — renderers distinguish
// "no other-version data" (no suffix) from "other-version data
// exists" (suffix shown) by checking for nil.
func summarizeOtherVersionPostures(postures []profile.Posture) *OtherVersionsSummary {
	if len(postures) == 0 {
		return nil
	}
	mostRecentIdx := 0
	for i := 1; i < len(postures); i++ {
		if postures[i].SetAt.After(postures[mostRecentIdx].SetAt) {
			mostRecentIdx = i
		}
	}
	mr := postures[mostRecentIdx]
	return &OtherVersionsSummary{
		MostRecent: &OtherVersionPosture{
			Version:   mr.Version,
			Tier:      postureTierToSurveyTier(mr.Tier),
			SetAt:     mr.SetAt,
			Rationale: mr.Rationale,
		},
		TotalPostures: len(postures),
	}
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
