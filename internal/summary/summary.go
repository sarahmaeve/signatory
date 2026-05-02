// Package summary defines the Summary value type and the Assembler
// contract for the M7 "signatory summary" verb: one composed view
// of everything signatory knows about a target identity — posture,
// burn, analyses rollup, related URIs — rolled up in shallow form
// so agents don't have to query four separate endpoints to piece
// together a decision view.
//
// Summary is deliberately shallow: it reports counts, latest-per-
// analyst snapshots, and identity relationships, but does NOT embed
// full conclusion/observation/methodology trees. A caller that
// needs that detail drills in with signatory_show_conclusions,
// signatory_show_methodology, signatory_detail, or signatory_signals.
// This keeps summary cheap to assemble, compact to transmit (MCP
// tool response), and easy to reason about as the "first-read"
// surface for any new target inspection.
//
// Callers: signatory summary CLI, signatory_summary MCP tool,
// eventually the M6 synthesist handoff (with extra evidence fields
// layered on top), and the M8 /analyze-skill replacement
// (short-circuiting the pipeline when a fresh summary already
// exists).
package summary

import (
	"time"

	"github.com/sarahmaeve/signatory/internal/exchange"
)

// Summary is the rolled-up decision view for one target identity.
//
// Every field can independently be absent:
//   - Posture may be nil when no tier has been recorded.
//   - Burn may be nil when the entity has never been burned or the
//     burn was withdrawn.
//   - Analyses may be empty when no analyst has contributed yet.
//   - RelatedURIs may be empty when no identity-resolution hop
//     touches this entity (neither incoming nor outgoing).
//
// A Summary with all of those empty is still a valid response —
// "we know the entity exists, but there's nothing recorded." The
// assembler returns a zero-value Summary wrapped in a sentinel
// error when even the entity doesn't exist.
type Summary struct {
	// CanonicalURI is the entity's canonical URI — always populated.
	CanonicalURI string `json:"canonical_uri"`

	// ShortName is the human-readable identifier for display.
	ShortName string `json:"short_name"`

	// EntityType is the entity's kind (project, package, identity,
	// patch, org). Callers rendering UI use this to pick verbs
	// ("Analyze package" vs "Analyze contributor").
	EntityType string `json:"entity_type"`

	// URL is the clone URL or homepage when the entity maps to one,
	// empty otherwise (pkg: URIs without a resolved source repo have
	// no URL).
	URL string `json:"url,omitempty"`

	// RelatedURIs names other canonical URIs that point at this
	// entity via collected_from, OR that this entity's analyses
	// point at. Populated from the M2 reverse-walk on
	// collected_from_entity_id — lets summary surface both sides
	// of the identity-hop transparency contract (D1).
	RelatedURIs []string `json:"related_uris,omitempty"`

	// Posture is the currently-active posture, if any. Follows M1
	// version-scope semantics: if the URI is versioned
	// (pkg:npm/X@V), this is the posture on that exact version;
	// otherwise the most-recent posture across all versions.
	Posture *PostureSnapshot `json:"posture,omitempty"`

	// Burn is the currently-active burn, if any. Withdrawn burns
	// are excluded by the store's default filter — this field
	// nil-or-present is already the soft-delete semantic.
	Burn *BurnSnapshot `json:"burn,omitempty"`

	// Analyses lists every ingested analyst output that references
	// this entity (either as primary entity_id or as
	// collected_from_entity_id, per the M2 cross-URI walk). Ordered
	// newest-ingested first. Shallow: counts only — callers drill
	// in with show-conclusions / show-methodology for detail.
	Analyses []AnalysisRollup `json:"analyses,omitempty"`
}

// PostureSnapshot is the "active posture at this moment" view.
// Matches the shape of the active row in postures — the snapshot
// is a point-in-time copy, not a pointer to a live record.
type PostureSnapshot struct {
	Tier      string    `json:"tier"`
	Version   string    `json:"version,omitempty"`
	Rationale string    `json:"rationale"`
	SetBy     string    `json:"set_by"`
	SetAt     time.Time `json:"set_at"`
}

// BurnSnapshot is the "active burn" view. When nil on the parent
// Summary, the entity has no active burn. The burned_at / burned_by
// fields come through verbatim from the burns row.
//
// Path B's cascade fields:
//
//   - ViaOwnerURI is empty when the burn is direct (on the queried
//     entity itself), and populated when the burn cascaded from a
//     related identity (e.g., the repo's github owner is burned, so
//     the repo summary surfaces "burned via owner identity:github/X").
//   - ViaRole names the relation kind ("publisher", "maintainer")
//     so renderers can phrase the cascade reason precisely.
//
// Both fields are JSON-omitempty so direct-burn payloads stay
// compact and clients written before Path B continue to parse the
// shape they expected.
type BurnSnapshot struct {
	Reason      string    `json:"reason"`
	BurnedBy    string    `json:"burned_by"`
	BurnedAt    time.Time `json:"burned_at"`
	ViaOwnerURI string    `json:"via_owner_uri,omitempty"`
	ViaRole     string    `json:"via_role,omitempty"`
}

// AnalysisRollup is the per-analyst-output summary line.
//
// Severity counts sum the conclusions in the output across every
// defined SeverityValue — so a rollup with 3 medium + 1 high + 2
// positive has ConclusionCount = 6 and the breakdown matches. This
// is the "what severity weight has this analyst contributed?"
// signal agents care about when deciding whether to re-run vs.
// trust the existing analysis.
type AnalysisRollup struct {
	OutputID              string         `json:"output_id"`
	AnalystID             string         `json:"analyst_id"`
	Model                 string         `json:"model,omitempty"`
	Round                 int            `json:"round"`
	IngestedAt            time.Time      `json:"ingested_at"`
	TargetCommit          string         `json:"target_commit,omitempty"`
	CollectedFromURI      string         `json:"collected_from_uri,omitempty"`
	ConclusionCount       int            `json:"conclusion_count"`
	SeverityCounts        SeverityCounts `json:"severity_counts"`
	PositiveAbsenceCount  int            `json:"positive_absence_count"`
	ObservationCount      int            `json:"observation_count"`
	MethodologyPatternCnt int            `json:"methodology_pattern_count"`
}

// SeverityCounts breaks down conclusion counts by severity_default.
// Keys are the six SeverityValue enum values from the exchange
// package; the map form (rather than six named fields) keeps the
// struct extensible — adding a new severity level doesn't break
// consumers that range over the map.
//
// Declared as a type alias (not a distinct type) so the store layer
// can return the same type without a summary → store → summary
// import cycle. Store methods return map[exchange.SeverityValue]int
// directly; summary consumers see SeverityCounts without conversion.
type SeverityCounts = map[exchange.SeverityValue]int
