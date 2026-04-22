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

	// Reachability records which direct deps can transitively
	// reach this dep via the manifest's dependency graph.
	// Populated only on indirect deps when the manifest parser
	// produced a graph (see manifest.Graph + manifest.ErrGraph-
	// Unavailable). Nil on direct deps and on indirects when
	// graph data was unavailable.
	//
	// Used by survey to bucket indirects into "inherit coverage
	// from a resolved direct" vs "await an unresolved direct"
	// — see Summary.IndirectByReachability and
	// IsResolvedTier for the categorization rule.
	Reachability *Reachability
}

// Reachability lists the direct deps that can reach a given
// indirect via some path in the manifest graph. Nil on direct
// deps; carried only on indirects, and only when the parser
// emitted graph data.
type Reachability struct {
	// FromDirects is the set of canonical URIs of direct deps
	// that reach this indirect. A given direct appears at most
	// once even if multiple paths from that direct exist.
	// Order is iteration-order of the BFS, not stable across
	// runs — consumers that need stable output should sort.
	FromDirects []string
}

// IndirectReachabilityBreakdown partitions the indirect dep
// count into three buckets that answer the user's "can I defer
// these?" question. Counts sum to Summary.Indirect. Zero-valued
// when the manifest parser didn't emit graph data — renderers
// detect this with HasData() and fall back accordingly.
//
// The bucketing rule (deliberately conservative — diamond deps
// fall into ViaUnresolved if ANY path crosses an unresolved
// direct, per the agreed maximum-pessimism convention):
//
//   - OwnResolved: the indirect itself has a resolved tier
//     (vetted-frozen, trusted-for-now, rejected, burned, or
//     local-replace). Bucketed first; the indirect's own
//     verdict overrides any reachability story.
//   - ViaResolved: indirect lacks its own resolved tier but
//     EVERY direct that reaches it has a resolved tier. Safe
//     to defer: any future path the user takes to resolve a
//     direct will also resolve this indirect's situation.
//   - ViaUnresolved: indirect lacks its own resolved tier AND
//     at least one reaching direct is unresolved. Address
//     after the unresolved direct gets handled.
type IndirectReachabilityBreakdown struct {
	OwnResolved   int
	ViaResolved   int
	ViaUnresolved int
}

// HasData reports whether the breakdown was populated by the
// reachability pass. False when the manifest parser returned
// ErrGraphUnavailable — renderers use this to switch between
// the three-line breakdown and the fallback "(drill-down
// unavailable on this system)" line.
//
// Note: a project with zero indirect deps will also produce
// HasData == false (no buckets ever touched). That's the
// correct behavior — there's nothing to break down.
func (b IndirectReachabilityBreakdown) HasData() bool {
	return b.OwnResolved+b.ViaResolved+b.ViaUnresolved > 0
}

// IsResolvedTier reports whether t represents a tier with a
// recorded verdict. Used by the indirect-reachability bucketing
// to distinguish "the user has decided about this" from "the
// user has not yet decided." Resolved tiers don't need further
// action; unresolved tiers do.
//
// Per the design conversation on 2026-04-22, unknown-provenance
// is treated as UNRESOLVED — it's a tier-with-a-name but the
// "verdict" it carries is "we couldn't pin identity confidently,"
// which is still pending the user's call.
func IsResolvedTier(t Tier) bool {
	switch t {
	case TierVettedFrozen, TierTrustedForNow,
		TierRejected, TierBurned, TierLocalReplace:
		return true
	}
	return false
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

	// IndirectByReachability partitions the indirect dep count
	// into resolved-on-their-own / inherit-coverage / await-an-
	// unresolved-direct buckets. Zero-valued when graph data
	// wasn't available; renderers use the breakdown's HasData()
	// to switch between the three-line breakdown and the
	// fallback rendering.
	IndirectByReachability IndirectReachabilityBreakdown
}

// Result is survey's full return value: project info, per-dep
// results in manifest order, aggregate summary.
type Result struct {
	Project manifest.ProjectInfo
	Deps    []DepResult
	Summary Summary
}
