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
