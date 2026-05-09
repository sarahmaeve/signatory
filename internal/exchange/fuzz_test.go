package exchange

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// --- Fuzz targets for AnalystOutput unmarshal + validation ---
//
// AnalystOutput is the boundary where AI-generated content enters the
// signatory trust store. The ingestion tool (signatory_ingest_analysis)
// decodes arbitrary JSON from an agent → exchange.AnalystOutput → Validate().
// A compromised or confused agent can produce anything: invalid enums,
// missing required fields, control chars in load-bearing strings,
// pathological array sizes, or circular composition references.
//
// These fuzz tests exercise the unmarshal+validate pipeline in isolation
// (no store, no MCP envelope) and prove safety invariants on any input
// that passes validation.

// --- FuzzAnalystOutputValidate ---
//
// The primary target: arbitrary bytes → json.Unmarshal → Validate().
// Proves that the pipeline never panics and that any document passing
// validation satisfies structural invariants.

func FuzzAnalystOutputValidate(f *testing.F) {
	// Minimal valid document
	f.Add([]byte(`{"attribution":{"analyst_id":"test","model":"claude","invoked_at":"2026-01-01T00:00:00Z"},"target":"pkg:test/x","conclusions":[{"id":"F001","verdict":"v","rationale":"r","severity":{"default":"medium"},"category":"c","citations":[{"path":"f.go","line_start":1}]}]}`))
	// Synthesist with supplement
	f.Add([]byte(`{"attribution":{"analyst_id":"signatory-synthesis-v1","model":"claude","invoked_at":"2026-01-01T00:00:00Z"},"target":"pkg:test/x","conclusions":[],"synthesis_supplement":{"proposed_posture":{"tier":"trusted-for-now","rationale_summary":"looks good"},"reasoning":"r","summary":"s"}}`))
	// Multiple conclusions with ByContext severity
	f.Add([]byte(`{"attribution":{"analyst_id":"test","model":"m","invoked_at":"2026-01-01T00:00:00Z"},"target":"pkg:test/x","conclusions":[{"id":"F001","verdict":"v","rationale":"r","severity":{"default":"high","by_context":[{"context":{"host_isolation":"container"},"value":"low"}]},"category":"c","citations":[{"scope":{"kind":"tree","path":"/"}}]},{"id":"F002","verdict":"v2","rationale":"r2","severity":{"default":"informational"},"category":"c2","citations":[{"path":"a.rs","line_start":5,"line_end":10}]}]}`))
	// With positive absences
	f.Add([]byte(`{"attribution":{"analyst_id":"test","model":"m","invoked_at":"2026-01-01T00:00:00Z"},"target":"pkg:test/x","conclusions":[],"positive_absences":[{"pattern_checked":"unsafe","description":"no unsafe blocks","confidence":"exhaustive","citations":[{"scope":{"kind":"crate","path":"src/"}}]}]}`))
	// With methodology trace
	f.Add([]byte(`{"attribution":{"analyst_id":"test","model":"m","invoked_at":"2026-01-01T00:00:00Z"},"target":"pkg:test/x","conclusions":[],"methodology_trace":{"source":{"analyst_id":"test","model":"m","invoked_at":"2026-01-01T00:00:00Z"},"patterns":[{"id":"P001","signal_group":"capability","description":"d","collector_hint":{"grep_precision":"high","reasoning_depth":"none"},"pattern":"unsafe"}]}}`))
	// With supersedes
	f.Add([]byte(`{"attribution":{"analyst_id":"test","model":"m","invoked_at":"2026-01-01T00:00:00Z"},"target":"pkg:test/x","conclusions":[],"supersedes":[{"prior_id":"F001","kind":"corrects"}]}`))
	// With observations
	f.Add([]byte(`{"attribution":{"analyst_id":"test","model":"m","invoked_at":"2026-01-01T00:00:00Z"},"target":"pkg:test/x","conclusions":[],"observations":[{"id":"O001","title":"clean history","body":"contributor has consistent commit patterns","category":"contributor-trajectory"}]}`))
	// Empty object
	f.Add([]byte(`{}`))
	// Null
	f.Add([]byte(`null`))
	// Empty
	f.Add([]byte{})
	// Adversarial: invalid severity enum
	f.Add([]byte(`{"attribution":{"analyst_id":"test","model":"m","invoked_at":"2026-01-01T00:00:00Z"},"target":"pkg:test/x","conclusions":[{"id":"F001","verdict":"v","rationale":"r","severity":{"default":"BOGUS"},"category":"c","citations":[{"path":"f","line_start":1}]}]}`))
	// Adversarial: synthesis supplement on non-synthesist
	f.Add([]byte(`{"attribution":{"analyst_id":"regular-analyst","model":"m","invoked_at":"2026-01-01T00:00:00Z"},"target":"pkg:test/x","conclusions":[],"synthesis_supplement":{"proposed_posture":{"tier":"rejected","rationale_summary":"r"},"reasoning":"r","summary":"s"}}`))
	// Adversarial: synthesist without supplement
	f.Add([]byte(`{"attribution":{"analyst_id":"signatory-synthesis-v1","model":"m","invoked_at":"2026-01-01T00:00:00Z"},"target":"pkg:test/x","conclusions":[]}`))
	// Adversarial: duplicate conclusion IDs
	f.Add([]byte(`{"attribution":{"analyst_id":"test","model":"m","invoked_at":"2026-01-01T00:00:00Z"},"target":"pkg:test/x","conclusions":[{"id":"F001","verdict":"v","rationale":"r","severity":{"default":"low"},"category":"c","citations":[{"path":"f","line_start":1}]},{"id":"F001","verdict":"v2","rationale":"r2","severity":{"default":"high"},"category":"c2","citations":[{"path":"g","line_start":2}]}]}`))
	// Adversarial: citation with both line_start and scope
	f.Add([]byte(`{"attribution":{"analyst_id":"test","model":"m","invoked_at":"2026-01-01T00:00:00Z"},"target":"pkg:test/x","conclusions":[{"id":"F001","verdict":"v","rationale":"r","severity":{"default":"low"},"category":"c","citations":[{"path":"f","line_start":1,"scope":{"kind":"file","path":"f"}}]}]}`))
	// Adversarial: control chars in verdict via JSON escape
	f.Add([]byte(`{"attribution":{"analyst_id":"test","model":"m","invoked_at":"2026-01-01T00:00:00Z"},"target":"pkg:test/x","conclusions":[{"id":"F001","verdict":"injected\u0000null","rationale":"r","severity":{"default":"low"},"category":"c","citations":[{"path":"f","line_start":1}]}]}`))
	// Adversarial: control chars in target. The U+0001 in analyst_id
	// is built at runtime via string([]byte{0x01}) — no source-level
	// string literal contains the control byte (ST1018), but the
	// test input still carries the byte the parser should reject.
	ctlByte := string([]byte{0x01})
	f.Add([]byte(`{"attribution":{"analyst_id":"test` + ctlByte + `","model":"m","invoked_at":"2026-01-01T00:00:00Z"},"target":"pkg:test/x\u0000y","conclusions":[]}`))
	// Adversarial: very long target
	f.Add([]byte(`{"attribution":{"analyst_id":"test","model":"m","invoked_at":"2026-01-01T00:00:00Z"},"target":"` + strings.Repeat("a", 5000) + `","conclusions":[]}`))
	// Adversarial: invalid posture tier
	f.Add([]byte(`{"attribution":{"analyst_id":"signatory-synthesis-v1","model":"m","invoked_at":"2026-01-01T00:00:00Z"},"target":"pkg:test/x","conclusions":[],"synthesis_supplement":{"proposed_posture":{"tier":"INVALID","rationale_summary":"r"},"reasoning":"r","summary":"s"}}`))
	// Adversarial: version_scope with URL
	f.Add([]byte(`{"attribution":{"analyst_id":"signatory-synthesis-v1","model":"m","invoked_at":"2026-01-01T00:00:00Z"},"target":"pkg:test/x","conclusions":[],"synthesis_supplement":{"proposed_posture":{"tier":"trusted-for-now","rationale_summary":"r","version_scope":"https://example.com/v1.2.3"},"reasoning":"r","summary":"s"}}`))
	// Adversarial: composes_with referencing unknown pattern
	f.Add([]byte(`{"attribution":{"analyst_id":"test","model":"m","invoked_at":"2026-01-01T00:00:00Z"},"target":"pkg:test/x","conclusions":[],"methodology_trace":{"source":{"analyst_id":"test","model":"m","invoked_at":"2026-01-01T00:00:00Z"},"patterns":[{"id":"P001","signal_group":"s","description":"d","collector_hint":{"grep_precision":"narrows","reasoning_depth":"one_hop"},"composes_with":["P999"]}]}}`))
	// Adversarial: negative line numbers
	f.Add([]byte(`{"attribution":{"analyst_id":"test","model":"m","invoked_at":"2026-01-01T00:00:00Z"},"target":"pkg:test/x","conclusions":[{"id":"F001","verdict":"v","rationale":"r","severity":{"default":"low"},"category":"c","citations":[{"path":"f","line_start":-5}]}]}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		var out AnalystOutput
		if err := json.Unmarshal(data, &out); err != nil {
			// Malformed JSON — not our concern.
			return
		}

		err := out.Validate()

		if err != nil {
			// Validation rejected it — that's fine. But the validator
			// itself must not leave partially-validated state that looks
			// "ok enough" to a careless caller. We don't test that here
			// (it's the MCP tool's job to gate on err != nil). We only
			// need the fuzzer to prove Validate() doesn't panic.
			return
		}

		// --- The document passed validation. Verify invariants. ---

		// Invariant 1: Attribution.AnalystID is non-empty.
		// Model and InvokedAt are server-stamped (filled in at
		// ingest time), so they're EXPECTED to be empty here even
		// on validation success — the validator rejects any
		// caller-supplied value for those fields.
		if out.Attribution.AnalystID == "" {
			t.Error("passed validation but Attribution.AnalystID is empty")
		}

		// Invariant 2: Target is non-empty.
		if out.Target == "" {
			t.Error("passed validation but Target is empty")
		}

		// Invariant 3: All severity enums are valid.
		for i, c := range out.Conclusions {
			if !c.Severity.Default.Valid() {
				t.Errorf("conclusions[%d].severity.default %q passed validation but is invalid", i, c.Severity.Default)
			}
			for j, bc := range c.Severity.ByContext {
				if !bc.Value.Valid() {
					t.Errorf("conclusions[%d].severity.by_context[%d].value %q passed validation but is invalid", i, j, bc.Value)
				}
			}
		}

		// Invariant 4: Conclusion IDs are unique.
		seenIDs := make(map[string]struct{}, len(out.Conclusions))
		for i, c := range out.Conclusions {
			if _, dup := seenIDs[c.ID]; dup {
				t.Errorf("conclusions[%d] has duplicate id %q", i, c.ID)
			}
			seenIDs[c.ID] = struct{}{}
		}

		// Invariant 5: Observation IDs are unique.
		seenObsIDs := make(map[string]struct{}, len(out.Observations))
		for i, obs := range out.Observations {
			if _, dup := seenObsIDs[obs.ID]; dup {
				t.Errorf("observations[%d] has duplicate id %q", i, obs.ID)
			}
			seenObsIDs[obs.ID] = struct{}{}
		}

		// Invariant 6: Citations are either line-based or scope-based, not both.
		for i, c := range out.Conclusions {
			for j, cit := range c.Citations {
				hasLines := cit.LineStart != nil
				hasScope := cit.Scope != nil
				if hasLines && hasScope {
					t.Errorf("conclusions[%d].citations[%d] has both line_start and scope", i, j)
				}
				if !hasLines && !hasScope {
					t.Errorf("conclusions[%d].citations[%d] has neither line_start nor scope", i, j)
				}
			}
		}

		// Invariant 7: Supersession kinds are valid enums.
		for i, s := range out.Supersedes {
			if !s.Kind.Valid() {
				t.Errorf("supersedes[%d].kind %q passed validation but is invalid", i, s.Kind)
			}
		}

		// Invariant 8: PositiveAbsence confidence is valid.
		for i, pa := range out.PositiveAbsences {
			if !pa.Confidence.Valid() {
				t.Errorf("positive_absences[%d].confidence %q passed validation but is invalid", i, pa.Confidence)
			}
		}

		// Invariant 9: Synthesis trust boundary.
		isSynthesist := IsSynthesistRole(out.Attribution.AnalystID)
		hasSupplement := out.SynthesisSupplement != nil
		if isSynthesist && !hasSupplement {
			t.Error("synthesist passed validation without synthesis_supplement")
		}
		if !isSynthesist && hasSupplement {
			t.Error("non-synthesist passed validation with synthesis_supplement")
		}

		// Invariant 10: If supplement present, proposed posture tier is valid.
		if out.SynthesisSupplement != nil {
			if !ValidProposedPostureTier(out.SynthesisSupplement.ProposedPosture.Tier) {
				t.Errorf("proposed_posture.tier %q passed validation but is invalid",
					out.SynthesisSupplement.ProposedPosture.Tier)
			}
		}

		// Invariant 11: Methodology pattern IDs are unique and
		// composes_with references are resolvable within the catalog.
		if out.MethodologyTrace != nil {
			patIDs := make(map[string]struct{}, len(out.MethodologyTrace.Patterns))
			for i, p := range out.MethodologyTrace.Patterns {
				if _, dup := patIDs[p.ID]; dup {
					t.Errorf("methodology_trace.patterns[%d] has duplicate id %q", i, p.ID)
				}
				patIDs[p.ID] = struct{}{}
			}
			for i, p := range out.MethodologyTrace.Patterns {
				for j, ref := range p.ComposesWith {
					if _, ok := patIDs[ref]; !ok {
						t.Errorf("methodology_trace.patterns[%d].composes_with[%d] %q references unknown pattern", i, j, ref)
					}
				}
			}
		}

		// Invariant 12: All string fields in the validated document
		// must be valid UTF-8. (Go's json.Unmarshal guarantees this,
		// but this assertion catches any future code path that bypasses
		// json.Unmarshal, e.g., custom UnmarshalJSON methods.)
		if !utf8.ValidString(out.Target) {
			t.Errorf("target is invalid UTF-8")
		}
		if !utf8.ValidString(out.Attribution.AnalystID) {
			t.Errorf("attribution.analyst_id is invalid UTF-8")
		}
	})
}

// --- FuzzAnalystOutputValidate_SynthesisBoundary ---
//
// Focused fuzz target for the synthesis trust boundary: the rule that
// only synthesist-role analyst IDs may carry a SynthesisSupplement,
// and synthesist IDs must carry one. This is security-load-bearing —
// a regular analyst must not be able to propose posture changes.

func FuzzAnalystOutputValidate_SynthesisBoundary(f *testing.F) {
	f.Add("test-analyst", false)
	f.Add("signatory-synthesis-v1", true)
	f.Add("signatory-synthesis", true)
	f.Add("signatory-synthesis-experimental", true)
	f.Add("signatory-synthesi", false) // one char short — not a synthesist
	f.Add("signatory-synthesis\x00injected", true)
	f.Add("SIGNATORY-SYNTHESIS-V1", false) // case-sensitive
	f.Add("", false)
	f.Add(strings.Repeat("signatory-synthesis", 100), true)

	f.Fuzz(func(t *testing.T, analystID string, withSupplement bool) {
		out := &AnalystOutput{
			Attribution: AgentAttribution{
				AnalystID: analystID,
				// Model and InvokedAt server-stamped at ingest; see
				// AgentAttribution.validate.
			},
			Target: "pkg:test/x",
		}

		if withSupplement {
			out.SynthesisSupplement = &SynthesisSupplement{
				ProposedPosture: ProposedPosture{
					Tier:             "trusted-for-now",
					RationaleSummary: "test rationale",
				},
				Reasoning: "test reasoning",
				Summary:   "test summary",
			}
		}

		err := out.Validate()

		isSynthesist := IsSynthesistRole(analystID)

		// The trust boundary rule:
		// - synthesist + no supplement → must reject
		// - non-synthesist + supplement → must reject
		// - synthesist + supplement → may pass (if supplement valid)
		// - non-synthesist + no supplement → may pass

		if isSynthesist && !withSupplement {
			if err == nil {
				t.Errorf("synthesist %q without supplement passed validation", analystID)
			}
		}
		if !isSynthesist && withSupplement {
			if err == nil {
				t.Errorf("non-synthesist %q with supplement passed validation", analystID)
			}
		}
	})
}
