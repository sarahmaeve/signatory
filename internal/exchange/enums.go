package exchange

// SeverityValue is the polarity and magnitude of a finding.
//
// "Positive" is first-class. A finding like "the capability model is
// tighter than we thought" reduces a prior-assessed risk, and an
// analyst that revises its own prior assessment in response to
// deeper source reading is producing higher-quality output than one
// that defends its prior position. Positive findings are the shape
// of that revision.
type SeverityValue string

const (
	SeverityCritical      SeverityValue = "critical"
	SeverityHigh          SeverityValue = "high"
	SeverityMedium        SeverityValue = "medium"
	SeverityLow           SeverityValue = "low"
	SeverityInformational SeverityValue = "informational"
	SeverityPositive      SeverityValue = "positive"
)

// Valid reports whether v is a recognized SeverityValue.
func (v SeverityValue) Valid() bool {
	switch v {
	case SeverityCritical, SeverityHigh, SeverityMedium,
		SeverityLow, SeverityInformational, SeverityPositive:
		return true
	}
	return false
}

// Confidence is the analyst's claimed depth of examination for a
// positive absence.
//
// SpotChecked: the analyst read enough to believe the pattern is
// absent but did not exhaust the search space. ThoroughlyReviewed:
// the analyst ran a systematic check (e.g., ripgrep across the
// relevant crates) and believes the pattern is absent with high
// confidence. Exhaustive: the claim is backed by a complete
// enumeration (rare; usually only possible for small targets or
// machine-decidable properties).
type Confidence string

const (
	ConfidenceSpotChecked        Confidence = "spot_checked"
	ConfidenceThoroughlyReviewed Confidence = "thoroughly_reviewed"
	ConfidenceExhaustive         Confidence = "exhaustive"
)

// Valid reports whether c is a recognized Confidence value.
func (c Confidence) Valid() bool {
	switch c {
	case ConfidenceSpotChecked, ConfidenceThoroughlyReviewed, ConfidenceExhaustive:
		return true
	}
	return false
}

// GrepPrecision describes how precisely a deterministic text search
// can locate the pattern. High = tight pattern, few false positives.
// Narrows = search reduces the universe but human/LLM triage follows.
// Useless = the pattern isn't text-searchable (needs multi-hop
// reasoning or AST traversal to detect).
type GrepPrecision string

const (
	GrepPrecisionHigh    GrepPrecision = "high"
	GrepPrecisionNarrows GrepPrecision = "narrows"
	GrepPrecisionUseless GrepPrecision = "useless"
)

// Valid reports whether g is a recognized GrepPrecision value.
func (g GrepPrecision) Valid() bool {
	switch g {
	case GrepPrecisionHigh, GrepPrecisionNarrows, GrepPrecisionUseless:
		return true
	}
	return false
}

// ReasoningDepth describes how much reasoning past a grep hit is
// needed to turn a pattern match into a finding. None = the grep
// hit is the finding. OneHop = one inference step (e.g., trace a
// function call). MultiHop = multiple layers of reasoning (e.g.,
// trace transport -> parsing -> dispatch -> execution to verify a
// capability gate).
type ReasoningDepth string

const (
	ReasoningDepthNone     ReasoningDepth = "none"
	ReasoningDepthOneHop   ReasoningDepth = "one_hop"
	ReasoningDepthMultiHop ReasoningDepth = "multi_hop"
)

// Valid reports whether r is a recognized ReasoningDepth value.
func (r ReasoningDepth) Valid() bool {
	switch r {
	case ReasoningDepthNone, ReasoningDepthOneHop, ReasoningDepthMultiHop:
		return true
	}
	return false
}

// MissMode describes which kind of error a pattern's
// false-positive / false-negative skew produces.
//
// FalsePositiveHeavy: many hits that aren't findings — wastes
// triage time but doesn't miss real issues. Preferable for security
// collectors; noisy but complete.
// FalseNegativeHeavy: tight match but silently misses real issues.
// Dangerous for security use because the miss is invisible.
// Balanced: neither skew dominates. Empty is treated as "balanced
// or unknown" and validates as absent (the field is optional).
type MissMode string

const (
	MissModeBalanced           MissMode = "balanced"
	MissModeFalsePositiveHeavy MissMode = "false_positive_heavy"
	MissModeFalseNegativeHeavy MissMode = "false_negative_heavy"
)

// Valid reports whether m is a recognized MissMode value. The empty
// string is also valid because miss_mode is optional.
func (m MissMode) Valid() bool {
	switch m {
	case "", MissModeBalanced, MissModeFalsePositiveHeavy, MissModeFalseNegativeHeavy:
		return true
	}
	return false
}

// SupersessionKind classifies the relationship between a superseding
// finding and the prior finding it replaces.
//
// Corrects: the prior finding was wrong; the new finding is the
// accurate assessment. Refines: the prior finding was incomplete;
// the new finding adds detail without contradiction. Deprecates:
// the concern the prior finding raised no longer applies — typically
// because upstream fixed it.
type SupersessionKind string

const (
	SupersessionKindCorrects   SupersessionKind = "corrects"
	SupersessionKindRefines    SupersessionKind = "refines"
	SupersessionKindDeprecates SupersessionKind = "deprecates"
)

// Valid reports whether k is a recognized SupersessionKind.
func (k SupersessionKind) Valid() bool {
	switch k {
	case SupersessionKindCorrects, SupersessionKindRefines, SupersessionKindDeprecates:
		return true
	}
	return false
}

// ScopeKind classifies the breadth of a ScopeRef.
//
// Crate: a Cargo crate (Rust) or equivalent single-package unit.
// Dir: a specific directory. Tree: a source subtree, typically used
// for cross-cutting absences ("no unsafe across the whole tree").
// Workspace: the whole repository or Cargo workspace. File: a single
// file (use when LineStart isn't applicable, e.g., "the whole file
// was reviewed").
const (
	ScopeKindCrate     = "crate"
	ScopeKindDir       = "dir"
	ScopeKindTree      = "tree"
	ScopeKindWorkspace = "workspace"
	ScopeKindFile      = "file"
)

// ValidScopeKind reports whether k is a recognized scope kind.
// Kept as a function rather than a method so ScopeRef stays a plain
// struct (the Kind field holds the raw string for flexibility).
func ValidScopeKind(k string) bool {
	switch k {
	case ScopeKindCrate, ScopeKindDir, ScopeKindTree, ScopeKindWorkspace, ScopeKindFile:
		return true
	}
	return false
}

// Host-isolation values for ContextSpec.HostIsolation. Controlled
// vocabulary for the deployment-shape dimension of conditional
// severity. Extended as analyses surface new contexts.
const (
	HostIsolationSingleUser = "single_user"
	HostIsolationSharedHost = "shared_host"
	HostIsolationMultiUser  = "multi_user"
	HostIsolationContainer  = "container"
	HostIsolationCIRunner   = "ci_runner"
)

// Platform values for ContextSpec.Platform. Coarse-grained on
// purpose — fine-grained OS distinctions rarely change trust
// analysis, and the dimensional model makes it easy to add more
// later if needed.
const (
	PlatformUnix    = "unix"
	PlatformWindows = "windows"
	PlatformAny     = "any"
)
