package signal

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
)

// TestRegistry_CurrentGitHubTypesMatchHardcodedBehavior locks in the
// (Group, ForgeryResistance) values that the github collector currently
// hardcodes per call site. This test is the equivalence contract for
// the subsequent refactor: before the registry drives collection, these
// values are the hardcoded truth; after, they must be unchanged.
//
// When a value in this table is intentionally re-rated, update the
// registry AND this test together — the coupling is deliberate.
func TestRegistry_CurrentGitHubTypesMatchHardcodedBehavior(t *testing.T) {
	t.Parallel()

	// Each row is a signal type the github collector emits today and
	// the (Group, ForgeryResistance) it hardcodes. See
	// internal/signal/github/collector.go for the authoritative source
	// at the time this test was written.
	cases := []struct {
		signalType string
		group      profile.SignalGroup
		forgery    profile.ForgeryResistance
	}{
		{"last_push", profile.SignalGroupVitality, profile.ForgeryMediumDeclining},
		{"repo_age", profile.SignalGroupVitality, profile.ForgeryVeryHigh},
		{"stars", profile.SignalGroupCriticality, profile.ForgeryMediumDeclining},
		{"forks", profile.SignalGroupCriticality, profile.ForgeryMediumDeclining},
		{"open_issues", profile.SignalGroupVitality, profile.ForgeryMediumDeclining},
		{"archived", profile.SignalGroupVitality, profile.ForgeryHigh},
		{"owner_type", profile.SignalGroupGovernance, profile.ForgeryHigh},
		{"license", profile.SignalGroupHygiene, profile.ForgeryLowDeclining},
		{"contributors", profile.SignalGroupGovernance, profile.ForgeryHigh},
		{"last_commit", profile.SignalGroupVitality, profile.ForgeryMediumDeclining},
		{"commit_signing", profile.SignalGroupGovernance, profile.ForgeryVeryHigh},
		{"total_commits", profile.SignalGroupVitality, profile.ForgeryHigh},
		{"tags", profile.SignalGroupPublication, profile.ForgeryHigh},
		{"owner_profile", profile.SignalGroupGovernance, profile.ForgeryVeryHigh},
		{"adoption", profile.SignalGroupCriticality, profile.ForgeryHigh},
		{"ci_cd", profile.SignalGroupHygiene, profile.ForgeryMediumDeclining},
		{"go_dependencies", profile.SignalGroupGovernance, profile.ForgeryHigh},
	}

	for _, tc := range cases {
		t.Run(tc.signalType, func(t *testing.T) {
			t.Parallel()
			info, ok := GetSignalTypeInfo(tc.signalType)
			require.True(t, ok, "signal type %q must be registered — github collector emits it", tc.signalType)
			assert.Equal(t, tc.group, info.Group,
				"%q: registry Group must match github collector's hardcoded value", tc.signalType)
			assert.Equal(t, tc.forgery, info.ForgeryResistance,
				"%q: registry ForgeryResistance must match github collector's hardcoded value", tc.signalType)
		})
	}
}

// TestRegistry_GitCollectorTypesHaveExpectedShape locks in the
// (Group, ForgeryResistance) values for signal types that the
// local-clone git collector (internal/signal/git/) emits. Each type
// must be registered before the collector emits it — signal.Make
// panics on unregistered types, so registering here is a strict
// prerequisite.
//
// When adding a new git-collector signal, register it in types.go
// AND extend this table in the same commit. The coupling is
// deliberate: it prevents a silent drift where a collector emits
// a type but the registry's Group / ForgeryResistance metadata
// doesn't match the collector's intent.
//
// See design/v0.1-invariants.md §"Invariant 2" for why mechanical
// git-level collection belongs in a Go collector rather than in
// an analyst subagent's grep pass.
func TestRegistry_GitCollectorTypesHaveExpectedShape(t *testing.T) {
	t.Parallel()

	cases := []struct {
		signalType string
		group      profile.SignalGroup
		forgery    profile.ForgeryResistance
	}{
		{"first_commit_date", profile.SignalGroupVitality, profile.ForgeryMediumDeclining},
		{"tag_signing_status", profile.SignalGroupPublication, profile.ForgeryHigh},
		{"identity_graph_depth", profile.SignalGroupGovernance, profile.ForgeryVeryHigh},
		{"identity_domain_consistency", profile.SignalGroupGovernance, profile.ForgeryHigh},
		{"effective_maintainer_concentration", profile.SignalGroupGovernance, profile.ForgeryMediumDeclining},
	}

	for _, tc := range cases {
		t.Run(tc.signalType, func(t *testing.T) {
			t.Parallel()
			info, ok := GetSignalTypeInfo(tc.signalType)
			require.True(t, ok, "signal type %q must be registered — git collector emits it", tc.signalType)
			assert.Equal(t, tc.group, info.Group,
				"%q: registry Group must match git collector's intent", tc.signalType)
			assert.Equal(t, tc.forgery, info.ForgeryResistance,
				"%q: registry ForgeryResistance must match git collector's intent", tc.signalType)
		})
	}
}

// TestRegistry_RepofilesCollectorTypesHaveExpectedShape locks in the
// (Group, ForgeryResistance) values for signal types the repofiles
// collector (internal/signal/repofiles/) emits. Matches the coupling
// contract of the sibling git / github tests: registry drift and
// collector intent must stay aligned, caught in a single place.
func TestRegistry_RepofilesCollectorTypesHaveExpectedShape(t *testing.T) {
	t.Parallel()

	cases := []struct {
		signalType string
		group      profile.SignalGroup
		forgery    profile.ForgeryResistance
	}{
		{"repo_files", profile.SignalGroupHygiene, profile.ForgeryLowDeclining},
	}

	for _, tc := range cases {
		t.Run(tc.signalType, func(t *testing.T) {
			t.Parallel()
			info, ok := GetSignalTypeInfo(tc.signalType)
			require.True(t, ok, "signal type %q must be registered — repofiles collector emits it", tc.signalType)
			assert.Equal(t, tc.group, info.Group,
				"%q: registry Group must match repofiles collector's intent", tc.signalType)
			assert.Equal(t, tc.forgery, info.ForgeryResistance,
				"%q: registry ForgeryResistance must match repofiles collector's intent", tc.signalType)
		})
	}
}

// TestRegistry_GoPublishCollectorTypesHaveExpectedShape locks in the
// (Group, ForgeryResistance) values for signal types the gopublish
// collector (internal/signal/registry/gopublish/) emits. Same coupling
// contract as the sibling git/github/repofiles tests: registry drift
// and collector intent stay aligned, caught in a single place.
func TestRegistry_GoPublishCollectorTypesHaveExpectedShape(t *testing.T) {
	t.Parallel()

	cases := []struct {
		signalType string
		group      profile.SignalGroup
		forgery    profile.ForgeryResistance
	}{
		{"last_publish", profile.SignalGroupVitality, profile.ForgeryMediumDeclining},
		{"version_count", profile.SignalGroupVitality, profile.ForgeryHigh},
		{"transparency_log_present", profile.SignalGroupPublication, profile.ForgeryVeryHigh},
		{"publish_origin", profile.SignalGroupPublication, profile.ForgeryHigh},
		{"version_pin_table", profile.SignalGroupPublication, profile.ForgeryVeryHigh},
	}

	for _, tc := range cases {
		t.Run(tc.signalType, func(t *testing.T) {
			t.Parallel()
			info, ok := GetSignalTypeInfo(tc.signalType)
			require.True(t, ok, "signal type %q must be registered — gopublish collector emits it", tc.signalType)
			assert.Equal(t, tc.group, info.Group,
				"%q: registry Group must match gopublish collector's intent", tc.signalType)
			assert.Equal(t, tc.forgery, info.ForgeryResistance,
				"%q: registry ForgeryResistance must match gopublish collector's intent", tc.signalType)
		})
	}
}

// TestRegistry_AbsenceGroupInheritanceMatchesLegacyMapping locks in the
// previous signalGroupForType behavior so the post-refactor absence
// path produces the same Group assignments.
//
// The legacy switch only covered a subset of types; types it didn't
// mention defaulted to SignalGroupVitality. After refactor, absence
// uses registry lookup — which covers MORE types, not fewer — but for
// the types the switch did cover, the answer must be unchanged.
func TestRegistry_AbsenceGroupInheritanceMatchesLegacyMapping(t *testing.T) {
	t.Parallel()

	// The legacy switch from absence.go before this change:
	//   stars, forks, adoption                            → criticality
	//   owner_type, contributors, commit_signing,
	//   owner_profile, go_dependencies                    → governance
	//   tags                                              → publication
	//   license, ci_cd                                    → hygiene
	//   last_push, repo_age, open_issues, last_commit,
	//   total_commits, archived                           → vitality
	cases := map[string]profile.SignalGroup{
		"stars":           profile.SignalGroupCriticality,
		"forks":           profile.SignalGroupCriticality,
		"adoption":        profile.SignalGroupCriticality,
		"owner_type":      profile.SignalGroupGovernance,
		"contributors":    profile.SignalGroupGovernance,
		"commit_signing":  profile.SignalGroupGovernance,
		"owner_profile":   profile.SignalGroupGovernance,
		"go_dependencies": profile.SignalGroupGovernance,
		"tags":            profile.SignalGroupPublication,
		"license":         profile.SignalGroupHygiene,
		"ci_cd":           profile.SignalGroupHygiene,
		"last_push":       profile.SignalGroupVitality,
		"repo_age":        profile.SignalGroupVitality,
		"open_issues":     profile.SignalGroupVitality,
		"last_commit":     profile.SignalGroupVitality,
		"total_commits":   profile.SignalGroupVitality,
		"archived":        profile.SignalGroupVitality,
	}

	for signalType, wantGroup := range cases {
		t.Run(signalType, func(t *testing.T) {
			t.Parallel()
			info, ok := GetSignalTypeInfo(signalType)
			require.True(t, ok, "signal type %q must be registered", signalType)
			assert.Equal(t, wantGroup, info.Group,
				"%q: registry group must match the legacy signalGroupForType mapping", signalType)
		})
	}
}

// TestGetSignalTypeInfo_Unknown verifies that unregistered types return
// ok=false rather than a zero-value SignalTypeInfo with ok=true. Callers
// that treat unknown types as a programming error depend on this.
func TestGetSignalTypeInfo_Unknown(t *testing.T) {
	t.Parallel()
	_, ok := GetSignalTypeInfo("this_type_is_definitely_not_registered")
	assert.False(t, ok, "unregistered type must return ok=false")
}

// TestSignalTypes_Sorted verifies SignalTypes returns a stable sorted
// list — callers that diff registry snapshots or format them for
// human/agent consumption rely on deterministic ordering.
func TestSignalTypes_Sorted(t *testing.T) {
	t.Parallel()
	types := SignalTypes()
	require.NotEmpty(t, types)

	for i := 1; i < len(types); i++ {
		assert.Less(t, types[i-1].Type, types[i].Type,
			"SignalTypes() must be sorted; %q came before %q", types[i-1].Type, types[i].Type)
	}
}

// TestSignalTypes_IncludesAllRegistered verifies the enumerator returns
// every entry in the registry — a sanity check that a future edit
// doesn't accidentally filter some out.
func TestSignalTypes_IncludesAllRegistered(t *testing.T) {
	t.Parallel()
	got := SignalTypes()
	assert.Len(t, got, len(signalTypeRegistry),
		"SignalTypes() must return every entry in the registry")

	seen := make(map[string]bool, len(got))
	for _, info := range got {
		seen[info.Type] = true
	}
	for key := range signalTypeRegistry {
		assert.True(t, seen[key], "registry entry %q missing from SignalTypes() output", key)
	}
}

// TestRegistry_EveryEntryHasDescription enforces that registry entries
// without a description are treated as incomplete. Descriptions are the
// primary self-documentation surface for --verbose output and future
// MCP exposure — an empty Description is a silent documentation gap.
func TestRegistry_EveryEntryHasDescription(t *testing.T) {
	t.Parallel()
	for signalType, info := range signalTypeRegistry {
		assert.NotEmpty(t, info.Description,
			"registry entry %q must have a non-empty Description", signalType)
	}
}

// TestRegistry_TypeFieldMatchesMapKey enforces that the map key and the
// Type field agree — this invariant prevents a copy-paste bug where the
// key is renamed but the struct's Type field is left pointing to the
// old name. Such a bug would silently produce signals whose Type string
// disagrees with their registry entry, defeating the registry's purpose.
func TestRegistry_TypeFieldMatchesMapKey(t *testing.T) {
	t.Parallel()
	for key, info := range signalTypeRegistry {
		assert.Equal(t, key, info.Type,
			"registry entry map key %q must equal its Type field (%q)", key, info.Type)
	}
}

// TestRegistry_GroupsAreKnown verifies every entry uses a defined
// SignalGroup constant. Catches drift when someone adds a string that
// doesn't match the profile package's enumeration.
func TestRegistry_GroupsAreKnown(t *testing.T) {
	t.Parallel()
	knownGroups := map[profile.SignalGroup]bool{
		profile.SignalGroupVitality:    true,
		profile.SignalGroupGovernance:  true,
		profile.SignalGroupPublication: true,
		profile.SignalGroupHygiene:     true,
		profile.SignalGroupPosture:     true,
		profile.SignalGroupCriticality: true,
	}
	for signalType, info := range signalTypeRegistry {
		assert.True(t, knownGroups[info.Group],
			"registry entry %q has unknown SignalGroup %q", signalType, info.Group)
	}
}

// TestRegistry_ForgeryResistancesAreKnown verifies every entry uses a
// defined ForgeryResistance constant. The amplifier-role types that
// need a "not applicable" value are intentionally excluded from this
// registry pass — see the package doc for the Polarity deferral.
func TestRegistry_ForgeryResistancesAreKnown(t *testing.T) {
	t.Parallel()
	known := map[profile.ForgeryResistance]bool{
		profile.ForgeryVeryHigh:        true,
		profile.ForgeryHigh:            true,
		profile.ForgeryMediumDeclining: true,
		profile.ForgeryLowDeclining:    true,
	}
	for signalType, info := range signalTypeRegistry {
		assert.True(t, known[info.ForgeryResistance],
			"registry entry %q has unknown ForgeryResistance %q", signalType, info.ForgeryResistance)
	}
}

// TestRegistry_NoTypeNamesCollideWithAbsencePrefix guards against a
// signal type being registered under a name starting with "absence:",
// which would shadow absence records' synthesized types.
func TestRegistry_NoTypeNamesCollideWithAbsencePrefix(t *testing.T) {
	t.Parallel()
	for signalType := range signalTypeRegistry {
		assert.False(t, strings.HasPrefix(signalType, "absence:"),
			"registry entry %q must not start with 'absence:' — that prefix is reserved for absence records", signalType)
	}
}
