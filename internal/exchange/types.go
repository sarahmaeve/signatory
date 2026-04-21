// Package exchange defines the structured data model for agent-to-agent
// analyst exchange in signatory's dual-analyst architecture.
//
// An AnalystOutput is the top-level envelope produced by a provenance
// or security analyst. It contains conclusions, positive absences,
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
//
// SynthesisSupplement carries synthesis-specific payload (narrative
// reasoning, concordance/contradiction analysis across prior analysts,
// a proposed posture) that has no natural home in the conclusion /
// observation / absence model. Gated by attribution.analyst_id:
// outputs whose analyst_id starts with "signatory-synthesis" must
// carry a supplement; other roles must not. See
// design/m6-synthesis-contract.md.
type AnalystOutput struct {
	Attribution         AgentAttribution     `json:"attribution" yaml:"attribution"`
	Target              string               `json:"target" yaml:"target"`
	TargetCommit        string               `json:"target_commit,omitempty" yaml:"target_commit,omitempty"`
	Conclusions         []Conclusion         `json:"conclusions" yaml:"conclusions"`
	PositiveAbsences    []PositiveAbsence    `json:"positive_absences,omitempty" yaml:"positive_absences,omitempty"`
	Observations        []Observation        `json:"observations,omitempty" yaml:"observations,omitempty"`
	MethodologyTrace    *MethodologyCatalog  `json:"methodology_trace,omitempty" yaml:"methodology_trace,omitempty"`
	Supersedes          []Supersession       `json:"supersedes,omitempty" yaml:"supersedes,omitempty"`
	ReframesFrom        []string             `json:"reframes_from,omitempty" yaml:"reframes_from,omitempty"`
	RoundNotes          string               `json:"round_notes,omitempty" yaml:"round_notes,omitempty"`
	SynthesisSupplement *SynthesisSupplement `json:"synthesis_supplement,omitempty" yaml:"synthesis_supplement,omitempty"`
}

// SynthesisSupplement carries the Layer-3 synthesis payload: the
// narrative that justifies the proposed tier, the cross-analyst
// agreement/contradiction analysis, the weighted conclusion
// references that make the synthesis auditable back to specific
// analyst findings, and the proposed posture itself. See
// design/m6-synthesis-contract.md §3 for the target shape.
type SynthesisSupplement struct {
	// ProposedPosture is the synthesist's tier recommendation.
	// Required when the supplement is present; `signatory posture
	// accept <output-id>` promotes this into a real Posture row.
	ProposedPosture ProposedPosture `json:"proposed_posture" yaml:"proposed_posture"`

	// Reasoning justifies the proposed tier, traced to specific
	// conclusion IDs. Markdown-bodied; required when supplement
	// present.
	Reasoning string `json:"reasoning" yaml:"reasoning"`

	// Summary is the 2-3 sentence overview compressed from the
	// reasoning. Markdown; required when supplement present.
	Summary string `json:"summary" yaml:"summary"`

	// ConcordanceStrengths lists places where two or more analysts
	// independently reached compatible conclusions. High-confidence
	// evidence class — what the synthesist's calibration notes name
	// as "Agreement" in the agreement/contradiction/silence framework.
	ConcordanceStrengths []ConcordanceEntry `json:"concordance_strengths,omitempty" yaml:"concordance_strengths,omitempty"`

	// ContradictionsDetected lists places where two analysts disagree.
	// The synthesist must state a ResolutionPreference per entry —
	// silent unresolved contradictions are a failure mode of Layer 3
	// (leaves the reader to pick a side unaided).
	ContradictionsDetected []ContradictionEntry `json:"contradictions_detected,omitempty" yaml:"contradictions_detected,omitempty"`

	// KeyConclusionRefs is the weighted, ranked citation list: the
	// synthesist's pointers back to specific analyst conclusions, with
	// the relative weight each carried in the tier decision. Makes
	// the synthesis auditable against the source findings without
	// duplicating their bodies.
	KeyConclusionRefs []ConclusionRef `json:"key_conclusion_refs,omitempty" yaml:"key_conclusion_refs,omitempty"`

	// Gaps names what neither analyst could determine and what a
	// future round would need to resolve it. Distinct from
	// ActionItems — gaps are questions; action items are answers.
	Gaps []string `json:"gaps,omitempty" yaml:"gaps,omitempty"`

	// ActionItems is the concrete next steps for the consumer:
	// pinning, filing upstream patches, auditing a specific
	// transitive, re-running under a new round question, etc.
	ActionItems []string `json:"action_items,omitempty" yaml:"action_items,omitempty"`

	// Notes is the synthesist's escape hatch for commentary that
	// doesn't fit Reasoning, Gaps, or ActionItems — calibration
	// remarks, meta-observations about the synthesis process, hedges
	// worth recording without inflating Reasoning. Parallels
	// AnalystOutput.RoundNotes but at synthesis-body scope. Optional.
	Notes string `json:"notes,omitempty" yaml:"notes,omitempty"`
}

// ConcordanceEntry records one point of agreement across analysts.
// Populated into SynthesisSupplement.ConcordanceStrengths. The
// ConclusionIDs link back to the specific analyst findings that
// underpin the agreement.
type ConcordanceEntry struct {
	Topic         string   `json:"topic" yaml:"topic"`
	Description   string   `json:"description" yaml:"description"`
	AnalystRefs   []string `json:"analyst_refs" yaml:"analyst_refs"`
	ConclusionIDs []string `json:"conclusion_ids,omitempty" yaml:"conclusion_ids,omitempty"`
	Confidence    string   `json:"confidence" yaml:"confidence"` // "HIGH"/"MEDIUM"/"LOW"
}

// ContradictionEntry records one disagreement across analysts. The
// ResolutionPreference field expresses the synthesist's call on which
// side carried the day, and why — the synthesist's job is to take the
// call, not defer.
type ContradictionEntry struct {
	Topic                string   `json:"topic" yaml:"topic"`
	Description          string   `json:"description" yaml:"description"`
	SupportingAnalystA   string   `json:"supporting_analyst_a" yaml:"supporting_analyst_a"`
	SupportingAnalystB   string   `json:"supporting_analyst_b" yaml:"supporting_analyst_b"`
	ConclusionIDsA       []string `json:"conclusion_ids_a,omitempty" yaml:"conclusion_ids_a,omitempty"`
	ConclusionIDsB       []string `json:"conclusion_ids_b,omitempty" yaml:"conclusion_ids_b,omitempty"`
	ResolutionPreference string   `json:"resolution_preference,omitempty" yaml:"resolution_preference,omitempty"`
}

// ConclusionRef is a weighted pointer to an analyst conclusion. Lives
// in SynthesisSupplement.KeyConclusionRefs. Weight encodes rank within
// the synthesis's own prioritization: Weight=1 is most-load-bearing
// on the tier decision.
type ConclusionRef struct {
	OutputID          string `json:"output_id" yaml:"output_id"`                     // analyst_outputs.id
	ConclusionLocalID string `json:"conclusion_local_id" yaml:"conclusion_local_id"` // F001 etc.
	Weight            int    `json:"weight" yaml:"weight"`
	ForgeryResistance string `json:"forgery_resistance" yaml:"forgery_resistance"` // VERY HIGH/HIGH/MEDIUM/LOW
	RelevanceNote     string `json:"relevance_note,omitempty" yaml:"relevance_note,omitempty"`
}

// ProposedPosture is the synthesist's tier recommendation. Lives on
// SynthesisSupplement. The `signatory posture accept` verb reads
// Tier + VersionScope + RationaleSummary to write a real Posture row.
type ProposedPosture struct {
	// Tier matches profile.PostureTier string values (vetted-frozen,
	// trusted-for-now, unexamined, unknown-provenance, rejected).
	// The exchange package does not import profile to avoid a cycle;
	// validation is performed against the canonical vocabulary
	// maintained in enums.go.
	Tier string `json:"tier" yaml:"tier"`

	// VersionScope is the pkg @version this proposal applies to, or
	// empty for unversioned. Copied verbatim into the posture row's
	// version field on accept. Optional — an empty value is a valid
	// proposal (unversioned posture).
	VersionScope string `json:"version_scope,omitempty" yaml:"version_scope,omitempty"`

	// RationaleSummary is the one-paragraph distillation the accept
	// verb copies into the posture row's rationale. Required.
	RationaleSummary string `json:"rationale_summary" yaml:"rationale_summary"`
}

// AgentAttribution identifies which agent produced an output, on which
// model, with which system-prompt version, and when. Run-level
// telemetry (token cost, duration) lives at the MCP response envelope
// layer per design/mcp-dual-analyst-architecture.md — not here.
type AgentAttribution struct {
	AnalystID     string `json:"analyst_id" yaml:"analyst_id"`
	Model         string `json:"model" yaml:"model"`
	PromptVersion string `json:"prompt_version,omitempty" yaml:"prompt_version,omitempty"`
	InvokedAt     string `json:"invoked_at" yaml:"invoked_at"` // RFC3339
	Round         int    `json:"round,omitempty" yaml:"round,omitempty"`
}

// Conclusion describes a single issue the analyst surfaced. Verdict is
// the one-sentence distilled answer; Rationale is the markdown-bodied
// justification. Severity may be conditional on deployment context.
//
// Prerequisites captures exploit preconditions ("requires sync-server
// compromise") as structured data rather than prose. RemediationHints
// gives machine-consumable fix suggestions. Supersedes marks conclusions
// that revise earlier analysis rounds.
type Conclusion struct {
	ID                 string         `json:"id" yaml:"id"`
	Verdict            string         `json:"verdict" yaml:"verdict"`
	Rationale          string         `json:"rationale" yaml:"rationale"`
	Severity           Severity       `json:"severity" yaml:"severity"`
	DesignIntent       bool           `json:"design_intent,omitempty" yaml:"design_intent,omitempty"`
	Category           string         `json:"category" yaml:"category"`
	SignalType         *string        `json:"signal_type,omitempty" yaml:"signal_type,omitempty"`
	Citations          []Citation     `json:"citations,omitempty" yaml:"citations,omitempty"`
	Prerequisites      []string       `json:"prerequisites,omitempty" yaml:"prerequisites,omitempty"`
	RemediationHints   []string       `json:"remediation_hints,omitempty" yaml:"remediation_hints,omitempty"`
	Supersedes         []Supersession `json:"supersedes,omitempty" yaml:"supersedes,omitempty"`
	AnswersQuestion    *string        `json:"answers_question,omitempty" yaml:"answers_question,omitempty"`
	RelatedConclusions []string       `json:"related_conclusions,omitempty" yaml:"related_conclusions,omitempty"`
}

// Severity captures a finding's severity, optionally varying by
// deployment context. Default is always set; ByContext overrides
// apply when their ContextSpec matches the consuming environment.
type Severity struct {
	Default   SeverityValue        `json:"default" yaml:"default"`
	ByContext []ContextualSeverity `json:"by_context,omitempty" yaml:"by_context,omitempty"`
}

// ContextualSeverity pairs a deployment context with the severity
// that applies in that context.
type ContextualSeverity struct {
	Context ContextSpec   `json:"context" yaml:"context"`
	Value   SeverityValue `json:"value" yaml:"value"`
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
	HostIsolation string `json:"host_isolation,omitempty" yaml:"host_isolation,omitempty"`
	Platform      string `json:"platform,omitempty" yaml:"platform,omitempty"`
}

// Citation references source-level evidence. Exactly one of
// (Path + LineStart) or Scope must be set: line-based citations point
// at specific lines in a file; scope-based citations point at a
// broader range (a whole crate, directory, or workspace) for findings
// that apply uniformly across many lines.
type Citation struct {
	Path      string    `json:"path,omitempty" yaml:"path,omitempty"`
	LineStart *int      `json:"line_start,omitempty" yaml:"line_start,omitempty"`
	LineEnd   *int      `json:"line_end,omitempty" yaml:"line_end,omitempty"`
	Scope     *ScopeRef `json:"scope,omitempty" yaml:"scope,omitempty"`
	CommitSHA *string   `json:"commit_sha,omitempty" yaml:"commit_sha,omitempty"`
	Quoted    *string   `json:"quoted,omitempty" yaml:"quoted,omitempty"`
}

// ScopeRef references a broader-than-line scope of source — used
// when a conclusion or absence applies to a whole crate, directory,
// tree, or workspace rather than specific lines.
type ScopeRef struct {
	Kind string `json:"kind" yaml:"kind"` // see enums.go
	Path string `json:"path" yaml:"path"`
}

// PositiveAbsence records a pattern the analyst checked for and
// found absent — distinct from "never examined." Moves known-good
// evidence into the structured record.
//
// PatternRef optionally links to a MethodologyPattern elsewhere in
// the output (or in a cross-referenced catalog) that this absence
// corresponds to.
type PositiveAbsence struct {
	PatternChecked string     `json:"pattern_checked" yaml:"pattern_checked"`
	Description    string     `json:"description" yaml:"description"`
	Citations      []Citation `json:"citations,omitempty" yaml:"citations,omitempty"`
	Confidence     Confidence `json:"confidence" yaml:"confidence"`
	PatternRef     *string    `json:"pattern_ref,omitempty" yaml:"pattern_ref,omitempty"`
}

// Observation holds trust-model-relevant analysis that isn't a
// conclusion, positive absence, or methodology pattern. Typical uses:
// contributor-trajectory notes, project-personality texture,
// cross-cutting context that resists the Conclusion shape.
//
// Introduced in v1 after the atuin trial surfaced the need for a
// slot for the "Michelle Tilley clean-trajectory" analysis, which
// fit nowhere else structurally.
type Observation struct {
	ID         string     `json:"id" yaml:"id"`
	Title      string     `json:"title" yaml:"title"`
	Body       string     `json:"body" yaml:"body"`
	Category   string     `json:"category" yaml:"category"`
	SignalType *string    `json:"signal_type,omitempty" yaml:"signal_type,omitempty"`
	Citations  []Citation `json:"citations,omitempty" yaml:"citations,omitempty"`
}

// MethodologyCatalog is the analyst's documented "patterns I check
// for" list — the direct input to Layer 1 collector design. Analysts
// emit this on request to make their otherwise-implicit heuristics
// mechanically consumable.
type MethodologyCatalog struct {
	Source   AgentAttribution     `json:"source" yaml:"source"`
	Notes    string               `json:"notes,omitempty" yaml:"notes,omitempty"`
	Patterns []MethodologyPattern `json:"patterns" yaml:"patterns"`
}

// MethodologyPattern describes one pattern the analyst checks for
// across projects. CollectorHint tells downstream whether a
// deterministic collector can implement the pattern and how precisely.
// ComposesWith lists other pattern IDs whose co-occurrence surfaces
// conclusions neither pattern alone can produce.
type MethodologyPattern struct {
	ID                 string        `json:"id" yaml:"id"`
	SignalGroup        string        `json:"signal_group" yaml:"signal_group"`
	Description        string        `json:"description" yaml:"description"`
	Pattern            *string       `json:"pattern,omitempty" yaml:"pattern,omitempty"`
	CollectorHint      CollectorHint `json:"collector_hint" yaml:"collector_hint"`
	ComposesWith       []string      `json:"composes_with,omitempty" yaml:"composes_with,omitempty"`
	FalsePositiveNotes string        `json:"false_positive_notes,omitempty" yaml:"false_positive_notes,omitempty"`
	HitOnTarget        *bool         `json:"hit_on_target,omitempty" yaml:"hit_on_target,omitempty"`
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
	GrepPrecision  GrepPrecision  `json:"grep_precision" yaml:"grep_precision"`
	ReasoningDepth ReasoningDepth `json:"reasoning_depth" yaml:"reasoning_depth"`
	MissMode       MissMode       `json:"miss_mode,omitempty" yaml:"miss_mode,omitempty"`
}

// Supersession marks that a later conclusion or AnalystOutput replaces
// an earlier one. Kind distinguishes correction (prior was wrong)
// from refinement (prior was incomplete) from deprecation (prior no
// longer applies — e.g., upstream fixed it).
type Supersession struct {
	PriorID    string           `json:"prior_id" yaml:"prior_id"`
	PriorRound int              `json:"prior_round,omitempty" yaml:"prior_round,omitempty"`
	Kind       SupersessionKind `json:"kind" yaml:"kind"`
}
