package resources

import (
	"context"

	"github.com/sarahmaeve/signatory/internal/mcp"
	"github.com/sarahmaeve/signatory/internal/profile"
)

// SignalTypesResource serves signatory://signal-types — the compile-time
// signal-type registry derived from the profile package constants.
//
// V0.1: the registry is built from the SignalGroup constants defined in
// profile/signal.go, along with the ForgeryResistance scale. A full
// per-signal-type catalog (name, description, forgery resistance, example
// values) is deferred to v0.2 when a materialized registry exists.
//
// V0.2 upgrade path: replace signalTypeRegistry with a call to a
// profile.Registry() function that reads from a generated or hand-curated
// registry file (e.g., internal/profile/registry.go). The response shape
// can then carry per-type metadata including forgery resistance, source
// descriptions, and example values.
type SignalTypesResource struct{}

// URIPattern returns the literal URI for this static resource.
func (r *SignalTypesResource) URIPattern() string {
	return "signatory://signal-types"
}

// Description summarises the resource for resources/list.
func (r *SignalTypesResource) Description() string {
	return "READ THIS to understand what kinds of trust signals signatory tracks before interpreting signatory_analyze or signatory_signals output. Contains the six signal groups (vitality, governance, publication, hygiene, posture, criticality), the forgery-resistance scale, and the current placeholder catalog."
}

// signalTypeRegistry is the compile-time registry shape returned to agents.
type signalTypeRegistry struct {
	// Groups lists the known SignalGroup values with their purpose description.
	Groups []signalGroupEntry `json:"groups"`
	// ForgeryResistanceScale documents the ForgeryResistance values in
	// descending order of strength.
	ForgeryResistanceScale []string `json:"forgery_resistance_scale"`
	// Note documents the v0.1 status of this registry.
	Note string `json:"note"`
}

// signalGroupEntry describes one signal group.
type signalGroupEntry struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Read returns the static registry. No store access needed.
func (r *SignalTypesResource) Read(_ context.Context, _ string) *mcp.Response {
	return mcp.OK(signalTypeRegistry{
		Groups: []signalGroupEntry{
			{Name: string(profile.SignalGroupVitality), Description: "Is anyone home? (recent commits, issue activity, release cadence)"},
			{Name: string(profile.SignalGroupGovernance), Description: "Who is responsible? (maintainer count, commit signing, org backing)"},
			{Name: string(profile.SignalGroupPublication), Description: "How was this published? (registry presence, signing, provenance)"},
			{Name: string(profile.SignalGroupHygiene), Description: "Does it look like they care? (security policy, CI, dependency management)"},
			{Name: string(profile.SignalGroupPosture), Description: "What is the consumer's posture? (vetted-frozen, trusted-for-now, unexamined, unknown-provenance)"},
			{Name: string(profile.SignalGroupCriticality), Description: "How critical is this? (stars, downstream adoption, transitive fan-out)"},
		},
		ForgeryResistanceScale: []string{
			string(profile.ForgeryVeryHigh),
			string(profile.ForgeryHigh),
			string(profile.ForgeryMediumDeclining),
			string(profile.ForgeryLowDeclining),
		},
		Note: "v0.1: registry is derived from compile-time profile constants. " +
			"Per-signal-type metadata (forgery resistance per type, example values, " +
			"collector hints) will be available in v0.2 when the registry is materialized.",
	})
}
