// Package exchange defines the structured data model for agent-to-agent
// analyst exchange in signatory's dual-analyst architecture.
//
// An AnalystOutput is the top-level envelope produced by a provenance
// or security analyst. It contains findings, positive absences,
// observations, an optional methodology catalog, and supersession /
// cross-analyst reference metadata. The shape was validated against
// a three-round analysis of atuin (see
// design/analysis/atuin-schema-trial-response.json and
// design/analysis/atuin-schema-trial-feedback.md); the v1 shape
// incorporates every analyst-flagged gap from that trial.
//
// See design/mcp-dual-analyst-architecture.md for the architectural
// motivation and the list of revisions the trial drove.
package exchange

// AnalystOutput is the top-level document emitted by an analyst role.
// Produced by signatory://analyze/provenance and ://analyze/security;
// consumed by signatory://analyze/synthesis and by the CLI's
// rendering / persistence layers.
type AnalystOutput struct {
	Attribution      AgentAttribution    `json:"attribution"`
	Target           string              `json:"target"`
	TargetCommit     string              `json:"target_commit,omitempty"`
	Findings         []Finding           `json:"findings"`
	PositiveAbsences []PositiveAbsence   `json:"positive_absences,omitempty"`
	Observations     []Observation       `json:"observations,omitempty"`
	MethodologyTrace *MethodologyCatalog `json:"methodology_trace,omitempty"`
	Supersedes       []Supersession      `json:"supersedes,omitempty"`
	ReframesFrom     []string            `json:"reframes_from,omitempty"`
	RoundNotes       string              `json:"round_notes,omitempty"`
}

// AgentAttribution identifies which agent produced an output, on which
// model, with which system-prompt version, and when. Run-level
// telemetry (token cost, duration) lives at the MCP response envelope
// layer per design/mcp-dual-analyst-architecture.md — not here.
type AgentAttribution struct {
	AnalystID     string `json:"analyst_id"`
	Model         string `json:"model"`
	PromptVersion string `json:"prompt_version,omitempty"`
	InvokedAt     string `json:"invoked_at"` // RFC3339
	Round         int    `json:"round,omitempty"`
}

// Finding describes a single issue the analyst surfaced. Verdict is
// the one-sentence distilled answer; Rationale is the markdown-bodied
// justification. Severity may be conditional on deployment context.
//
// Prerequisites captures exploit preconditions ("requires sync-server
// compromise") as structured data rather than prose. RemediationHints
// gives machine-consumable fix suggestions. Supersedes marks findings
// that revise earlier analysis rounds.
type Finding struct {
	ID               string         `json:"id"`
	Verdict          string         `json:"verdict"`
	Rationale        string         `json:"rationale"`
	Severity         Severity       `json:"severity"`
	DesignIntent     bool           `json:"design_intent,omitempty"`
	Category         string         `json:"category"`
	SignalType       *string        `json:"signal_type,omitempty"`
	Citations        []Citation     `json:"citations,omitempty"`
	Prerequisites    []string       `json:"prerequisites,omitempty"`
	RemediationHints []string       `json:"remediation_hints,omitempty"`
	Supersedes       []Supersession `json:"supersedes,omitempty"`
	AnswersQuestion  *string        `json:"answers_question,omitempty"`
	RelatedFindings  []string       `json:"related_findings,omitempty"`
}

// Severity captures a finding's severity, optionally varying by
// deployment context. Default is always set; ByContext overrides
// apply when their ContextSpec matches the consuming environment.
type Severity struct {
	Default   SeverityValue        `json:"default"`
	ByContext []ContextualSeverity `json:"by_context,omitempty"`
}

// ContextualSeverity pairs a deployment context with the severity
// that applies in that context.
type ContextualSeverity struct {
	Context ContextSpec   `json:"context"`
	Value   SeverityValue `json:"value"`
}

// ContextSpec identifies a deployment context via orthogonal
// dimensions. A spec with multiple dimensions matches environments
// that satisfy all of them. Both fields are optional; a spec with
// only HostIsolation matches any platform.
//
// The dimension values are a controlled vocabulary. New values may
// be added as analyses surface new contexts. See the constants in
// enums.go for the current set.
type ContextSpec struct {
	HostIsolation string `json:"host_isolation,omitempty"`
	Platform      string `json:"platform,omitempty"`
}

// Citation references source-level evidence. Exactly one of
// (Path + LineStart) or Scope must be set: line-based citations point
// at specific lines in a file; scope-based citations point at a
// broader range (a whole crate, directory, or workspace) for findings
// that apply uniformly across many lines.
type Citation struct {
	Path      string    `json:"path,omitempty"`
	LineStart *int      `json:"line_start,omitempty"`
	LineEnd   *int      `json:"line_end,omitempty"`
	Scope     *ScopeRef `json:"scope,omitempty"`
	CommitSHA *string   `json:"commit_sha,omitempty"`
	Quoted    *string   `json:"quoted,omitempty"`
}

// ScopeRef references a broader-than-line scope of source — used
// when a finding or absence applies to a whole crate, directory,
// tree, or workspace rather than specific lines.
type ScopeRef struct {
	Kind string `json:"kind"` // see enums.go
	Path string `json:"path"`
}

// PositiveAbsence records a pattern the analyst checked for and
// found absent — distinct from "never examined." Moves known-good
// evidence into the structured record.
//
// PatternRef optionally links to a MethodologyPattern elsewhere in
// the output (or in a cross-referenced catalog) that this absence
// corresponds to.
type PositiveAbsence struct {
	PatternChecked string     `json:"pattern_checked"`
	Description    string     `json:"description"`
	Citations      []Citation `json:"citations,omitempty"`
	Confidence     Confidence `json:"confidence"`
	PatternRef     *string    `json:"pattern_ref,omitempty"`
}

// Observation holds trust-model-relevant analysis that isn't a
// finding, positive absence, or methodology pattern. Typical uses:
// contributor-trajectory notes, project-personality texture,
// cross-cutting context that resists the Finding shape.
//
// Introduced in v1 after the atuin trial surfaced the need for a
// slot for the "Michelle Tilley clean-trajectory" analysis, which
// fit nowhere else structurally.
type Observation struct {
	ID         string     `json:"id"`
	Title      string     `json:"title"`
	Body       string     `json:"body"`
	Category   string     `json:"category"`
	SignalType *string    `json:"signal_type,omitempty"`
	Citations  []Citation `json:"citations,omitempty"`
}

// MethodologyCatalog is the analyst's documented "patterns I check
// for" list — the direct input to Layer 1 collector design. Analysts
// emit this on request to make their otherwise-implicit heuristics
// mechanically consumable.
type MethodologyCatalog struct {
	Source   AgentAttribution     `json:"source"`
	Notes    string               `json:"notes,omitempty"`
	Patterns []MethodologyPattern `json:"patterns"`
}

// MethodologyPattern describes one pattern the analyst checks for
// across projects. CollectorHint tells downstream whether a
// deterministic collector can implement the pattern and how precisely.
// ComposesWith lists other pattern IDs whose co-occurrence surfaces
// findings neither pattern alone can produce.
type MethodologyPattern struct {
	ID                 string        `json:"id"`
	SignalGroup        string        `json:"signal_group"`
	Description        string        `json:"description"`
	Pattern            *string       `json:"pattern,omitempty"`
	CollectorHint      CollectorHint `json:"collector_hint"`
	ComposesWith       []string      `json:"composes_with,omitempty"`
	FalsePositiveNotes string        `json:"false_positive_notes,omitempty"`
	HitOnAtuin         *bool         `json:"hit_on_atuin,omitempty"`
}

// CollectorHint is a multi-axis classification of a methodology
// pattern's implementability as a deterministic collector vs. its
// need for LLM-grade reasoning.
//
// GrepPrecision: how tight the text match is. ReasoningDepth: how
// many inference hops past the grep are needed. MissMode: whether
// the pattern skews toward false-positives (noisy but complete) or
// false-negatives (silent misses).
type CollectorHint struct {
	GrepPrecision  GrepPrecision  `json:"grep_precision"`
	ReasoningDepth ReasoningDepth `json:"reasoning_depth"`
	MissMode       MissMode       `json:"miss_mode,omitempty"`
}

// Supersession marks that a later finding or AnalystOutput replaces
// an earlier one. Kind distinguishes correction (prior was wrong)
// from refinement (prior was incomplete) from deprecation (prior no
// longer applies — e.g., upstream fixed it).
type Supersession struct {
	PriorID    string           `json:"prior_id"`
	PriorRound int              `json:"prior_round,omitempty"`
	Kind       SupersessionKind `json:"kind"`
}
