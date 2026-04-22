package survey

import (
	"time"

	"github.com/sarahmaeve/signatory/internal/manifest"
)

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

	// OtherVersions summarizes what the store knows about this
	// entity at versions OTHER than the queried one. Populated
	// only when no exact-version posture matched — the residual
	// "we have history for this dep, but not for this version"
	// case. Nil when no other-version postures exist.
	//
	// Surfaced by survey rendering to aid navigation: the user
	// sees the most-recent verdict on a different version plus a
	// posture-count, and can decide whether to consult the prior
	// review or commission a fresh one. Survey deliberately does
	// NOT recommend an action — visibility only.
	//
	// Replaces the prior HasOtherVersions bool which signaled
	// existence without surfacing what was there.
	OtherVersions *OtherVersionsSummary
}

// OtherVersionPosture summarizes a single posture recorded on a
// version other than the one the survey queried. Carried in
// OtherVersionsSummary.MostRecent so renderers can surface the
// version, tier, and rationale without a second store round-trip.
type OtherVersionPosture struct {
	// Version is the version string the posture was set against.
	// Always different from the queried Dep.Version (by
	// construction — exact matches return earlier in resolveDep).
	Version string

	// Tier is the posture's tier mapped through the same
	// postureTierToSurveyTier helper used elsewhere.
	Tier Tier

	// SetAt is the posture's set_at timestamp. Drives the
	// most-recent tiebreak when multiple other-version postures
	// exist.
	SetAt time.Time

	// Rationale is the posture's rationale text as stored.
	// Truncation, if any, happens at render time — the data layer
	// passes it through unmodified.
	Rationale string
}

// OtherVersionsSummary aggregates the metadata about postures the
// store holds for an entity at versions other than the one the
// survey queried. Populated by resolveDep when no exact-version
// posture matched but other postures exist for the entity.
//
// Burns are intentionally NOT carried here. In the current data
// model burns are entity-level and absolute: an entity with a
// burn returns Tier=TierBurned at the top of resolveDep, before
// any posture lookup runs. The OtherVersions branch is therefore
// unreachable for burned entities, and adding burn fields to this
// struct would imply a per-version-burn model that doesn't exist.
type OtherVersionsSummary struct {
	// MostRecent is the posture with the largest SetAt across
	// all other-version postures. Nil only if the summary itself
	// is nil — when populated, MostRecent is always set.
	MostRecent *OtherVersionPosture

	// TotalPostures is the count of all postures recorded on
	// this entity (across all versions). Always equal to the
	// number of postures GetPostures returned, which by
	// construction excludes the queried-version match (since
	// that match would have caused an early return).
	TotalPostures int
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
