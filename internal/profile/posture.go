package profile

import "time"

// PostureTier represents an organizational trust decision about a dependency.
type PostureTier string

const (
	PostureVettedFrozen      PostureTier = "vetted-frozen"
	PostureTrustedForNow     PostureTier = "trusted-for-now"
	PostureUnexamined        PostureTier = "unexamined"
	PostureUnknownProvenance PostureTier = "unknown-provenance"
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
