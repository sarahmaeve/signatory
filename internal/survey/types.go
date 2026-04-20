package survey

import "github.com/sarahmaeve/signatory/internal/manifest"

// Tier is the resolved trust tier for a single dependency after
// survey has combined store lookups (entity, postures, burns)
// with the ecosystem-specific matching rules.
//
// Beyond the five posture tiers in internal/profile, survey
// introduces two synthetic tiers that don't exist as stored
// postures but are meaningful in a dependency-tree view:
//
//   - TierBurned: the entity has a burn record. Overrides any
//     recorded posture. The "do not use" state.
//   - TierNotInStore: no entity record exists — the dependency
//     has never been analyzed. Distinct from "unexamined," which
//     means "entity exists but no posture decision has been made."
type Tier string

const (
	TierVettedFrozen      Tier = "vetted-frozen"
	TierTrustedForNow     Tier = "trusted-for-now"
	TierUnexamined        Tier = "unexamined"
	TierUnknownProvenance Tier = "unknown-provenance"
	TierRejected          Tier = "rejected"
	TierBurned            Tier = "burned"       // synthesized from burn record
	TierNotInStore        Tier = "not-in-store" // synthesized from ErrNotFound
	TierLocalReplace      Tier = "local-replace"
)

// DepResult is survey's per-dependency outcome.
type DepResult struct {
	// Dep echoes the input from the manifest parser so renderers
	// have both the raw (manifest) and resolved (store) views in
	// one place.
	Dep manifest.Dep

	// Tier is the resolved trust tier per the rules documented
	// in the package comment.
	Tier Tier

	// PostureVersion is the version string the matched posture
	// applies to. Equal to Dep.Version when the posture matches
	// the pinned version exactly; empty when no posture matched
	// (for not-in-store, burned, or posture-less entities).
	PostureVersion string

	// PostureRationale is the rationale text of the matched
	// posture, truncated at the store level. Empty when no
	// posture matched.
	PostureRationale string

	// BurnReason is populated when Tier == TierBurned. Empty
	// otherwise.
	BurnReason string

	// HasOtherVersions is true when the store has postures for
	// this entity but none match the pinned version. Signals
	// "there are decisions on record for other versions — the
	// user might want to consult those before analyzing fresh."
	HasOtherVersions bool
}

// Summary aggregates DepResults into the counts that the
// terminal output's header line and the JSON consumer's
// dashboard view both need.
type Summary struct {
	Total    int
	Direct   int
	Indirect int

	// ByTier maps each observed Tier → count. Using string keys
	// (via Tier's underlying string type) lets the summary
	// serialize cleanly to JSON without a custom marshaler.
	ByTier map[Tier]int

	// NeedsReview lists canonical URIs of direct dependencies
	// that are not-in-store or unexamined. These are the action
	// items survey surfaces at the bottom of its output.
	NeedsReview []string
}

// Result is survey's full return value: project info, per-dep
// results in manifest order, aggregate summary.
type Result struct {
	Project manifest.ProjectInfo
	Deps    []DepResult
	Summary Summary
}
