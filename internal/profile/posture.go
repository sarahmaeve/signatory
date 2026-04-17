package profile

import "time"

// PostureTier represents an organizational trust decision about a dependency.
//
// The five tiers correspond to the posture vocabulary specified in
// design/trust-policy-v1.md and documented in the synthesis handoff
// template (templates/handoffs/synthesis-v1.md §"How to synthesize").
// Ordering (strongest-endorsement to strongest-rejection):
//
//	vetted-frozen > trusted-for-now > unexamined > unknown-provenance > rejected
//
// rejected is the terminal "do not adopt" decision — used when analysts
// find unresolved concerns serious enough to recommend against adoption,
// and separately produced by Layer-0 burn-shortcircuit in the policy
// evaluator.
//
// "analysis-only" is NOT a stored tier — it's the pipeline's terminal
// state when the target isn't a dependency and no adoption decision
// applies. It corresponds to "no posture row recorded."
type PostureTier string

const (
	PostureVettedFrozen      PostureTier = "vetted-frozen"
	PostureTrustedForNow     PostureTier = "trusted-for-now"
	PostureUnexamined        PostureTier = "unexamined"
	PostureUnknownProvenance PostureTier = "unknown-provenance"
	PostureRejected          PostureTier = "rejected"
)

// Posture records an organizational trust decision.
type Posture struct {
	EntityID  string      `json:"entity_id"`
	Tier      PostureTier `json:"tier"`
	Version   string      `json:"version,omitempty"`
	Rationale string      `json:"rationale"`
	SetBy     string      `json:"set_by"`
	SetAt     time.Time   `json:"set_at"`
}

// BurnSource identifies where a burn originated.
type BurnSource string

const (
	BurnSourceLocal     BurnSource = "local"
	BurnSourceInherited BurnSource = "inherited"
)

// Burn records a trust revocation against an entity.
type Burn struct {
	EntityID  string     `json:"entity_id"`
	Reason    string     `json:"reason"`
	Source    BurnSource `json:"source"`
	SourceOrg string     `json:"source_org,omitempty"`
	BurnedAt  time.Time  `json:"burned_at"`
	BurnedBy  string     `json:"burned_by"`
}
