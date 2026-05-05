package exchange

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// validBase returns a minimally-valid AnalystOutput. Tests mutate it
// to introduce specific invariant violations and check that
// Validate() flags them.
func validBase() *AnalystOutput {
	verdict := "test verdict, one sentence"
	rationale := "test rationale, multi-paragraph allowed"
	lineStart := 10
	return &AnalystOutput{
		Attribution: AgentAttribution{
			AnalystID: "test-analyst",
			Model:     "claude-test",
			InvokedAt: "2026-04-14T00:00:00Z",
		},
		Target: "pkg:test/example",
		Conclusions: []Conclusion{
			{
				ID:        "F001",
				Verdict:   verdict,
				Rationale: rationale,
				Severity:  Severity{Default: SeverityMedium},
				Category:  "test",
				Citations: []Citation{
					{Path: "src/main.rs", LineStart: &lineStart},
				},
			},
		},
	}
}

func TestValidate_ValidBaseDoesPass(t *testing.T) {
	require.NoError(t, validBase().Validate(),
		"the validBase helper should always produce a valid document; "+
			"if this fails, validBase has a bug")
}

func TestValidate_AttributionFields(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*AnalystOutput)
		wantErr string
	}{
		{
			name:    "missing analyst_id",
			mutate:  func(o *AnalystOutput) { o.Attribution.AnalystID = "" },
			wantErr: "attribution: analyst_id required",
		},
		{
			name:    "missing model",
			mutate:  func(o *AnalystOutput) { o.Attribution.Model = "" },
			wantErr: "attribution: model required",
		},
		{
			name:    "missing invoked_at",
			mutate:  func(o *AnalystOutput) { o.Attribution.InvokedAt = "" },
			wantErr: "attribution: invoked_at required",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := validBase()
			tt.mutate(o)
			err := o.Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// TestValidate_SignatoryAnalystIDMustBeCanonical asserts the
// validator rejects analyst_id values that fall in the
// "signatory-" namespace but don't match the canonical
// ^signatory-(security|provenance|synthesis)-v\d+$ form.
//
// Background: dogfood session e572ed87 stalled because the
// provenance analyst ingested with analyst_id="provenance"
// (drift form) instead of "signatory-provenance-v1". The store
// already shows 17 occurrences of "signatory-provenance" (no -v1)
// vs 11 of the canonical form, plus "provenance-analyst",
// "security-analyst", and other variants. Catching this at the
// validator stops the drift permanently for orchestrator-emitted
// signatory roles; the agent receives CodeSchemaViolation, fixes
// in the same turn (per handoff's "fix and resubmit" instruction).
//
// External (non-signatory-) analyst_ids stay unrestricted because
// other teams use their own conventions (external-sec-v1,
// external-prov-v1 in the wild today).
func TestValidate_SignatoryAnalystIDMustBeCanonical(t *testing.T) {
	tests := []struct {
		name      string
		analystID string
		wantErr   string // empty → expect Validate to pass
	}{
		// Canonical forms (non-synthesis) — must pass.
		// Synthesis canonical-form coverage lives in
		// TestValidate_SynthesisSupplementWithCanonicalSynthesisID
		// because synthesis ids additionally require a
		// synthesis_supplement (a separate validator gate).
		{"canonical security v1", "signatory-security-v1", ""},
		{"canonical provenance v1", "signatory-provenance-v1", ""},
		{"canonical security v2", "signatory-security-v2", ""},
		{"canonical provenance v10", "signatory-provenance-v10", ""},

		// Non-signatory namespace — must pass (validator only
		// gates the signatory- namespace).
		{"external sec v1", "external-sec-v1", ""},
		{"external prov v1", "external-prov-v1", ""},
		{"test analyst", "test-analyst", ""},
		{"arbitrary", "some-other-analyst", ""},

		// Drift forms inside the signatory namespace — must fail.
		{"missing -v1 suffix", "signatory-provenance",
			"signatory-provenance"},
		{"bare role only", "signatory-security",
			"signatory-security"},
		{"missing role function", "signatory-v1",
			"signatory-v1"},
		{"unknown role", "signatory-osv-supplement-v1",
			"signatory-osv-supplement-v1"},
		{"non-numeric version", "signatory-security-vbeta",
			"signatory-security-vbeta"},
		{"empty version", "signatory-security-v",
			"signatory-security-v"},
		{"role typo", "signatory-securty-v1",
			"signatory-securty-v1"},
		{"trailing junk", "signatory-security-v1-extra",
			"signatory-security-v1-extra"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := validBase()
			o.Attribution.AnalystID = tt.analystID
			err := o.Validate()
			if tt.wantErr == "" {
				require.NoError(t, err,
					"analyst_id %q should pass validation", tt.analystID)
				return
			}
			require.Error(t, err,
				"analyst_id %q should fail validation", tt.analystID)
			// Error message must include the offending value so the
			// agent can self-correct in the same turn.
			assert.Contains(t, err.Error(), tt.wantErr,
				"error must name the offending analyst_id; got: %s", err)
			// And must mention "signatory-" so the agent knows the
			// rule applies to its namespace.
			assert.Contains(t, err.Error(), "signatory-",
				"error must explain the namespace rule; got: %s", err)
		})
	}
}

// TestValidate_SynthesisSupplementWithCanonicalSynthesisID is a
// regression guard: the new analyst_id regex must not interfere
// with the existing SynthesisSupplement gate, which keys off
// IsSynthesistRole's HasPrefix logic. A canonical
// signatory-synthesis-v1 with a synthesis supplement must validate.
func TestValidate_SynthesisSupplementWithCanonicalSynthesisID(t *testing.T) {
	o := validBase()
	o.Attribution.AnalystID = "signatory-synthesis-v1"
	o.SynthesisSupplement = &SynthesisSupplement{
		ProposedPosture: ProposedPosture{
			Tier:             "trusted-for-now",
			RationaleSummary: "test",
		},
		Reasoning: "test reasoning",
		Summary:   "test summary",
	}
	require.NoError(t, o.Validate())
}

func TestValidate_TargetRequired(t *testing.T) {
	o := validBase()
	o.Target = ""
	err := o.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "target required")
}

func TestValidate_ConclusionFields(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Conclusion)
		wantErr string
	}{
		{
			name:    "missing id",
			mutate:  func(f *Conclusion) { f.ID = "" },
			wantErr: "id required",
		},
		{
			name:    "missing verdict",
			mutate:  func(f *Conclusion) { f.Verdict = "" },
			wantErr: "verdict required",
		},
		{
			name:    "missing rationale",
			mutate:  func(f *Conclusion) { f.Rationale = "" },
			wantErr: "rationale required",
		},
		{
			name:    "missing category",
			mutate:  func(f *Conclusion) { f.Category = "" },
			wantErr: "category required",
		},
		{
			name:    "invalid severity default",
			mutate:  func(f *Conclusion) { f.Severity.Default = "purple" },
			wantErr: "severity.default \"purple\" invalid",
		},
		{
			name: "invalid severity by_context value",
			mutate: func(f *Conclusion) {
				f.Severity.ByContext = []ContextualSeverity{
					{Context: ContextSpec{HostIsolation: HostIsolationSingleUser}, Value: "bogus"},
				}
			},
			wantErr: "by_context[0]: value \"bogus\" invalid",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := validBase()
			tt.mutate(&o.Conclusions[0])
			err := o.Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestValidate_DuplicateConclusionIDs(t *testing.T) {
	o := validBase()
	dup := o.Conclusions[0]
	o.Conclusions = append(o.Conclusions, dup)
	err := o.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate id \"F001\"")
}

func TestValidate_CitationLineOrScopeRequired(t *testing.T) {
	o := validBase()
	o.Conclusions[0].Citations = []Citation{
		{Path: "src/main.rs"}, // neither line_start nor scope
	}
	err := o.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must have either line_start or scope")
}

func TestValidate_CitationLineAndScopeMutuallyExclusive(t *testing.T) {
	o := validBase()
	lineStart := 10
	o.Conclusions[0].Citations = []Citation{
		{
			Path:      "src/main.rs",
			LineStart: &lineStart,
			Scope:     &ScopeRef{Kind: ScopeKindFile, Path: "src/main.rs"},
		},
	}
	err := o.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot have both line_start and scope")
}

func TestValidate_CitationLineEndBeforeStart(t *testing.T) {
	o := validBase()
	start := 50
	end := 10
	o.Conclusions[0].Citations = []Citation{
		{Path: "src/main.rs", LineStart: &start, LineEnd: &end},
	}
	err := o.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "line_end (10) < line_start (50)")
}

func TestValidate_CitationLineStartZero(t *testing.T) {
	o := validBase()
	zero := 0
	o.Conclusions[0].Citations = []Citation{
		{Path: "src/main.rs", LineStart: &zero},
	}
	err := o.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "line_start must be >= 1")
}

func TestValidate_CitationScopeKindInvalid(t *testing.T) {
	o := validBase()
	o.Conclusions[0].Citations = []Citation{
		{Scope: &ScopeRef{Kind: "moon", Path: "/"}},
	}
	err := o.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kind \"moon\" invalid")
}

func TestValidate_CitationScopeAllValidKinds(t *testing.T) {
	for _, kind := range []string{
		ScopeKindCrate, ScopeKindDir, ScopeKindTree,
		ScopeKindWorkspace, ScopeKindFile,
	} {
		t.Run(kind, func(t *testing.T) {
			o := validBase()
			o.Conclusions[0].Citations = []Citation{
				{Scope: &ScopeRef{Kind: kind, Path: "some/path"}},
			}
			require.NoError(t, o.Validate())
		})
	}
}

func TestValidate_PositiveAbsenceFields(t *testing.T) {
	o := validBase()
	o.PositiveAbsences = []PositiveAbsence{
		{
			PatternChecked: "",
			Description:    "",
			Confidence:     "vibes",
		},
	}
	err := o.Validate()
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "pattern_checked required")
	assert.Contains(t, msg, "description required")
	assert.Contains(t, msg, "confidence \"vibes\" invalid")
}

func TestValidate_ObservationFields(t *testing.T) {
	o := validBase()
	o.Observations = []Observation{{ID: "O1"}}
	err := o.Validate()
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "title required")
	assert.Contains(t, msg, "body required")
	assert.Contains(t, msg, "category required")
}

func TestValidate_DuplicateObservationIDs(t *testing.T) {
	o := validBase()
	o.Observations = []Observation{
		{ID: "O1", Title: "t", Body: "b", Category: "c"},
		{ID: "O1", Title: "t", Body: "b", Category: "c"},
	}
	err := o.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate id \"O1\"")
}

func TestValidate_MethodologyPatternFields(t *testing.T) {
	o := validBase()
	hint := CollectorHint{
		GrepPrecision:  GrepPrecisionHigh,
		ReasoningDepth: ReasoningDepthNone,
	}
	o.MethodologyTrace = &MethodologyCatalog{
		Source: AgentAttribution{
			AnalystID: "x", Model: "y", InvokedAt: "2026-04-14T00:00:00Z",
		},
		Patterns: []MethodologyPattern{
			{
				ID:            "MP-1",
				SignalGroup:   "",
				Description:   "",
				CollectorHint: hint,
			},
		},
	}
	err := o.Validate()
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "signal_group required")
	assert.Contains(t, msg, "description required")
}

func TestValidate_MethodologyPatternComposesWithUnknown(t *testing.T) {
	o := validBase()
	o.MethodologyTrace = &MethodologyCatalog{
		Source: AgentAttribution{
			AnalystID: "x", Model: "y", InvokedAt: "2026-04-14T00:00:00Z",
		},
		Patterns: []MethodologyPattern{
			{
				ID:           "MP-1",
				SignalGroup:  "test",
				Description:  "test",
				ComposesWith: []string{"MP-NONEXISTENT"},
				CollectorHint: CollectorHint{
					GrepPrecision:  GrepPrecisionUseless,
					ReasoningDepth: ReasoningDepthMultiHop,
				},
			},
		},
	}
	err := o.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "MP-NONEXISTENT")
	assert.Contains(t, err.Error(), "unknown pattern")
}

func TestValidate_MethodologyPatternHighPrecisionWithoutPattern(t *testing.T) {
	// A pattern marked grep_precision=high but with no pattern
	// string is contradictory — there's nothing to grep for. Catch it.
	o := validBase()
	o.MethodologyTrace = &MethodologyCatalog{
		Source: AgentAttribution{
			AnalystID: "x", Model: "y", InvokedAt: "2026-04-14T00:00:00Z",
		},
		Patterns: []MethodologyPattern{
			{
				ID:          "MP-1",
				SignalGroup: "test",
				Description: "test",
				// Pattern omitted intentionally
				CollectorHint: CollectorHint{
					GrepPrecision:  GrepPrecisionHigh,
					ReasoningDepth: ReasoningDepthNone,
				},
			},
		},
	}
	err := o.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no pattern string but grep_precision=high")
}

func TestValidate_SupersessionFields(t *testing.T) {
	o := validBase()
	o.Conclusions[0].Supersedes = []Supersession{
		{PriorID: "", Kind: "purple"},
	}
	err := o.Validate()
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "prior_id required")
	assert.Contains(t, msg, "kind \"purple\" invalid")
}

func TestValidate_AccumulatesMultipleErrors(t *testing.T) {
	// The validator should report every error, not just the first.
	// Useful for callers fixing a doc with several issues.
	o := &AnalystOutput{
		// missing attribution fields
		// missing target
		Conclusions: []Conclusion{
			{ID: "F1"}, // missing verdict, rationale, category, severity
			{ID: "F1"}, // duplicate id
		},
	}
	err := o.Validate()
	require.Error(t, err)
	msg := err.Error()
	// Multi-line error from errors.Join.
	lines := strings.Split(msg, "\n")
	assert.Greater(t, len(lines), 5,
		"expected several distinct errors, got %d (msg: %q)", len(lines), msg)
}

func TestValidate_EnumValues(t *testing.T) {
	// Spot-check Valid() methods against unrecognized values.
	assert.False(t, SeverityValue("").Valid())
	assert.False(t, SeverityValue("bogus").Valid())
	assert.True(t, SeverityCritical.Valid())
	assert.True(t, SeverityPositive.Valid())

	assert.False(t, Confidence("").Valid())
	assert.True(t, ConfidenceSpotChecked.Valid())

	assert.False(t, GrepPrecision("").Valid())
	assert.True(t, GrepPrecisionHigh.Valid())

	assert.True(t, MissMode("").Valid(), "miss_mode is optional; empty is valid")
	assert.False(t, MissMode("rocket").Valid())

	assert.False(t, SupersessionKind("").Valid())
	assert.True(t, SupersessionKindCorrects.Valid())

	assert.True(t, ValidScopeKind("crate"))
	assert.False(t, ValidScopeKind("planet"))
}

// TestValidate_ErrorMessagesIncludeValidValues asserts that every enum
// validation error message includes the list of valid values. This is
// the mechanistic repair for dogfood session 37864a8c: the provenance
// analyst received "severity.default \"ContextualSeverity\" invalid"
// without being told WHAT the valid values are, then went spelunking in
// signatory source (types.go, enums.go, validate.go) to discover them.
// Including valid values in the error message eliminates the need to
// read source to self-correct.
func TestValidate_ErrorMessagesIncludeValidValues(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*AnalystOutput)
		wantValues []string // each must appear in the error message
	}{
		{
			name: "severity.default lists valid severity values",
			mutate: func(o *AnalystOutput) {
				o.Conclusions[0].Severity.Default = "ContextualSeverity"
			},
			wantValues: []string{"critical", "high", "medium", "low", "informational", "positive"},
		},
		{
			name: "severity.by_context lists valid severity values",
			mutate: func(o *AnalystOutput) {
				o.Conclusions[0].Severity.ByContext = []ContextualSeverity{
					{Context: ContextSpec{HostIsolation: HostIsolationCIRunner}, Value: "extreme"},
				}
			},
			wantValues: []string{"critical", "high", "medium", "low", "informational", "positive"},
		},
		{
			name: "confidence lists valid confidence values",
			mutate: func(o *AnalystOutput) {
				o.PositiveAbsences = []PositiveAbsence{
					{PatternChecked: "x", Description: "y", Confidence: "high"},
				}
			},
			wantValues: []string{"exhaustive", "thoroughly_reviewed", "spot_checked"},
		},
		{
			name: "scope.kind lists valid scope kinds",
			mutate: func(o *AnalystOutput) {
				o.Conclusions[0].Citations = []Citation{
					{Scope: &ScopeRef{Kind: "api_response", Path: "/"}},
				}
			},
			wantValues: []string{"file", "dir", "tree", "workspace", "crate"},
		},
		{
			name: "grep_precision lists valid precision values",
			mutate: func(o *AnalystOutput) {
				o.MethodologyTrace = &MethodologyCatalog{
					Source: AgentAttribution{
						AnalystID: "x", Model: "y", InvokedAt: "2026-04-14T00:00:00Z",
					},
					Patterns: []MethodologyPattern{
						{
							ID: "P1", SignalGroup: "s", Description: "d",
							CollectorHint: CollectorHint{
								GrepPrecision:  "very_high",
								ReasoningDepth: ReasoningDepthNone,
							},
						},
					},
				}
			},
			wantValues: []string{"high", "narrows", "useless"},
		},
		{
			name: "reasoning_depth lists valid depth values",
			mutate: func(o *AnalystOutput) {
				o.MethodologyTrace = &MethodologyCatalog{
					Source: AgentAttribution{
						AnalystID: "x", Model: "y", InvokedAt: "2026-04-14T00:00:00Z",
					},
					Patterns: []MethodologyPattern{
						{
							ID: "P1", SignalGroup: "s", Description: "d",
							CollectorHint: CollectorHint{
								GrepPrecision:  GrepPrecisionUseless,
								ReasoningDepth: "deep",
							},
						},
					},
				}
			},
			wantValues: []string{"none", "one_hop", "multi_hop"},
		},
		{
			name: "miss_mode lists valid mode values",
			mutate: func(o *AnalystOutput) {
				o.MethodologyTrace = &MethodologyCatalog{
					Source: AgentAttribution{
						AnalystID: "x", Model: "y", InvokedAt: "2026-04-14T00:00:00Z",
					},
					Patterns: []MethodologyPattern{
						{
							ID: "P1", SignalGroup: "s", Description: "d",
							CollectorHint: CollectorHint{
								GrepPrecision:  GrepPrecisionUseless,
								ReasoningDepth: ReasoningDepthNone,
								MissMode:       "catastrophic",
							},
						},
					},
				}
			},
			wantValues: []string{"balanced", "false_positive_heavy", "false_negative_heavy"},
		},
		{
			name: "supersession kind lists valid kinds",
			mutate: func(o *AnalystOutput) {
				o.Conclusions[0].Supersedes = []Supersession{
					{PriorID: "F000", Kind: "overrides"},
				}
			},
			wantValues: []string{"corrects", "refines", "deprecates"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := validBase()
			tt.mutate(o)
			err := o.Validate()
			require.Error(t, err)
			msg := err.Error()
			for _, v := range tt.wantValues {
				assert.Contains(t, msg, v,
					"error message should include valid value %q so the caller can self-correct without reading source", v)
			}
		})
	}

	// Proposed posture tier requires a synthesist base.
	t.Run("proposed_posture.tier lists valid tier values", func(t *testing.T) {
		o := validSynthesisBase()
		o.SynthesisSupplement.ProposedPosture.Tier = "galactically-frozen"
		err := o.Validate()
		require.Error(t, err)
		msg := err.Error()
		for _, v := range []string{"vetted-frozen", "trusted-for-now", "unexamined", "unknown-provenance", "rejected"} {
			assert.Contains(t, msg, v,
				"error message should include valid tier %q so the caller can self-correct without reading source", v)
		}
	})
}

// --- SynthesisSupplement (M6a) ---
//
// Synthesist outputs carry a SynthesisSupplement that has no natural
// home in the conclusion/observation/absence model. The supplement is
// gated by attribution.analyst_id: outputs whose analyst_id starts
// with "signatory-synthesis" must carry a supplement; outputs from
// any other role must not. See design/m6-synthesis-contract.md §4.

// validSynthesisBase returns a minimally-valid synthesist output.
// Tests mutate it to introduce specific invariant violations and
// check that Validate() flags them.
func validSynthesisBase() *AnalystOutput {
	return &AnalystOutput{
		Attribution: AgentAttribution{
			AnalystID: "signatory-synthesis-v1",
			Model:     "claude-test",
			InvokedAt: "2026-04-21T00:00:00Z",
		},
		Target: "pkg:test/example",
		SynthesisSupplement: &SynthesisSupplement{
			ProposedPosture: ProposedPosture{
				Tier:             "trusted-for-now",
				RationaleSummary: "minimal rationale for the synthesis helper",
			},
			Reasoning: "minimal reasoning paragraph for the synthesis helper",
			Summary:   "two-sentence summary for the synthesis helper",
		},
	}
}

func TestValidate_SynthesisBaseDoesPass(t *testing.T) {
	require.NoError(t, validSynthesisBase().Validate(),
		"validSynthesisBase should always produce a valid document; "+
			"if this fails, validSynthesisBase has a bug")
}

// TestValidate_SupplementOnNonSynthesistRejected is the trust-boundary
// guard: a security or provenance analyst producing a SynthesisSupplement
// must be rejected at validation time. Only synthesist outputs (analyst_id
// prefix "signatory-synthesis") may carry a supplement — this is how M6a
// keeps the proposed_posture concept out of the analyst layer.
func TestValidate_SupplementOnNonSynthesistRejected(t *testing.T) {
	// Start from a normal analyst output (non-synthesist), attach a
	// supplement anyway, expect rejection.
	o := validBase()
	o.SynthesisSupplement = &SynthesisSupplement{
		ProposedPosture: ProposedPosture{
			Tier:             "trusted-for-now",
			RationaleSummary: "should not be accepted from a non-synthesist",
		},
		Reasoning: "irrelevant — this role cannot propose",
		Summary:   "irrelevant",
	}
	err := o.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "synthesis_supplement only allowed for synthesist role")
	assert.Contains(t, err.Error(), `"test-analyst"`,
		"error should name the offending analyst_id")
}

// TestValidate_SynthesistWithoutSupplementRejected is the other side
// of the gate: a synthesist output that omits the supplement is
// rejected. A synthesis without a proposed posture + reasoning isn't
// a synthesis — it's an empty row. Forcing the supplement to be
// present keeps the synthesist from degenerating into "Layer-2 but
// with different branding."
func TestValidate_SynthesistWithoutSupplementRejected(t *testing.T) {
	o := validSynthesisBase()
	o.SynthesisSupplement = nil
	err := o.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "synthesist output requires synthesis_supplement")
}

// TestValidate_SynthesisSupplementFields exercises the required-field
// rules inside the supplement. Table-driven so adding a new required
// field is one row; each row mutates validSynthesisBase() to violate
// exactly one rule.
func TestValidate_SynthesisSupplementFields(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*SynthesisSupplement)
		wantErr string
	}{
		{
			name:    "missing proposed_posture.tier",
			mutate:  func(s *SynthesisSupplement) { s.ProposedPosture.Tier = "" },
			wantErr: `proposed_posture.tier "" invalid`,
		},
		{
			name:    "invalid proposed_posture.tier",
			mutate:  func(s *SynthesisSupplement) { s.ProposedPosture.Tier = "galactically-frozen" },
			wantErr: `proposed_posture.tier "galactically-frozen" invalid`,
		},
		{
			name:    "missing proposed_posture.rationale_summary",
			mutate:  func(s *SynthesisSupplement) { s.ProposedPosture.RationaleSummary = "" },
			wantErr: "proposed_posture.rationale_summary required",
		},
		{
			name: "version_scope contains a full canonical URI (pkg:)",
			mutate: func(s *SynthesisSupplement) {
				s.ProposedPosture.VersionScope = "pkg:npm/example@1.2.3"
			},
			wantErr: "proposed_posture.version_scope",
		},
		{
			name: "version_scope contains a full canonical URI (repo:)",
			mutate: func(s *SynthesisSupplement) {
				s.ProposedPosture.VersionScope = "repo:github/owner/name"
			},
			wantErr: "proposed_posture.version_scope",
		},
		{
			name: "version_scope contains a URL scheme",
			mutate: func(s *SynthesisSupplement) {
				s.ProposedPosture.VersionScope = "https://example.com/v1.2.3"
			},
			wantErr: "proposed_posture.version_scope",
		},
		{
			name: "version_scope contains a newline",
			mutate: func(s *SynthesisSupplement) {
				s.ProposedPosture.VersionScope = "v1.2.3\n(with commentary)"
			},
			wantErr: "proposed_posture.version_scope",
		},
		{
			name: "version_scope exceeds length cap",
			mutate: func(s *SynthesisSupplement) {
				// 129 characters — one past the 128-byte cap.
				s.ProposedPosture.VersionScope = "v" + strings.Repeat("1", 128)
			},
			wantErr: "proposed_posture.version_scope",
		},
		{
			name:    "missing reasoning",
			mutate:  func(s *SynthesisSupplement) { s.Reasoning = "" },
			wantErr: "synthesis_supplement.reasoning required",
		},
		{
			name:    "missing summary",
			mutate:  func(s *SynthesisSupplement) { s.Summary = "" },
			wantErr: "synthesis_supplement.summary required",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := validSynthesisBase()
			tt.mutate(o.SynthesisSupplement)
			err := o.Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// TestValidate_VersionScope_AcceptsCleanShapes is the positive
// companion to the reject-cases above: confirms the validator
// does NOT false-positive on legitimate version strings across
// the ecosystems signatory targets. Without this, an over-strict
// validator (e.g., one that required a leading 'v') would silently
// break npm postures, Go pseudo-versions, or calendar-versioned
// packages.
func TestValidate_VersionScope_AcceptsCleanShapes(t *testing.T) {
	cleanShapes := []string{
		"",                                 // empty = unversioned, valid
		"v1.2.3",                           // Go / tag-style
		"1.2.3",                            // npm-style
		"1.2.3-alpha.1",                    // semver pre-release
		"1.2.3+build.meta",                 // semver build metadata
		"v0.0.0-20230101000000-abcdef0123", // Go pseudo-version
		"2026.04.15",                       // calendar version
		"4.17.21",                          // npm real-world
		"v1.49.1",                          // sqlite real-world
	}
	for _, v := range cleanShapes {
		t.Run(v, func(t *testing.T) {
			o := validSynthesisBase()
			o.SynthesisSupplement.ProposedPosture.VersionScope = v
			assert.NoError(t, o.Validate(),
				"legitimate version shape %q must pass validation", v)
		})
	}
}
