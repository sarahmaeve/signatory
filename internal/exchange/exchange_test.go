package exchange

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// loadFixture reads the migrated atuin trial fixture from testdata/.
// The fixture is preserved verbatim from the analyst's emission with
// the schema migrations documented in testdata/README.md.
func loadFixture(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "atuin-schema-trial.json"))
	require.NoError(t, err, "read fixture")
	return raw
}

// decodeFixture parses the fixture into an AnalystOutput. Uses
// DisallowUnknownFields to catch schema drift between fixture and
// types — if someone adds a field to the JSON that doesn't exist on
// the Go types (or removes a field from the types that the JSON
// expects), the test fails loudly rather than silently dropping data.
func decodeFixture(t *testing.T) *AnalystOutput {
	t.Helper()
	raw := loadFixture(t)
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()

	var out AnalystOutput
	require.NoError(t, dec.Decode(&out), "decode fixture")
	return &out
}

func TestAtuinTrial_FixtureDecodes(t *testing.T) {
	out := decodeFixture(t)
	assert.NotNil(t, out)
}

func TestAtuinTrial_FixtureValidates(t *testing.T) {
	out := decodeFixture(t)
	require.NoError(t, out.Validate(),
		"the canonical fixture should pass validation; if this fails, "+
			"either the fixture has a real problem or the validator is too strict")
}

func TestAtuinTrial_Counts(t *testing.T) {
	out := decodeFixture(t)
	assert.Len(t, out.Conclusions, 3, "three conclusions from round 2 (§7, §8, §5)")
	assert.Len(t, out.PositiveAbsences, 2, "two positive absences from §C")

	require.NotNil(t, out.MethodologyTrace, "methodology_trace required for this fixture")
	assert.Len(t, out.MethodologyTrace.Patterns, 12, "twelve patterns from §B")
}

func TestAtuinTrial_Roundtrip(t *testing.T) {
	// Round-trip from the *normalized* form: decode the fixture,
	// re-encode (this normalizes any quirks like empty-slice vs.
	// nil-slice for omitempty fields), then re-decode and re-encode
	// once more. The two normalized forms must match exactly.
	//
	// We don't compare against the original-decoded form because
	// fields with `omitempty` collapse empty slices to nil on
	// re-encode, which fails reflect.DeepEqual against the
	// original. That's a JSON-tag artifact, not a real bug;
	// canonicalizing through one round eliminates it.
	first := decodeFixture(t)

	buf1 := &bytes.Buffer{}
	enc1 := json.NewEncoder(buf1)
	enc1.SetIndent("", "  ")
	require.NoError(t, enc1.Encode(first), "first re-encode")

	var second AnalystOutput
	require.NoError(t, json.Unmarshal(buf1.Bytes(), &second), "first re-decode")

	buf2 := &bytes.Buffer{}
	enc2 := json.NewEncoder(buf2)
	enc2.SetIndent("", "  ")
	require.NoError(t, enc2.Encode(&second), "second re-encode")

	// Compare the canonical bytes: the second encode should produce
	// identical output to the first if round-trip is idempotent
	// from canonical form.
	assert.Equal(t, buf1.String(), buf2.String(),
		"round-trip from canonical form must be byte-identical")

	// Also compare via DeepEqual on the structs as a defense-in-
	// depth check (catches cases where bytes differ due to encoding
	// quirks but the structs are equivalent).
	var third AnalystOutput
	require.NoError(t, json.Unmarshal(buf2.Bytes(), &third), "third decode")
	assert.True(t, reflect.DeepEqual(second, third),
		"second and third decode must be DeepEqual")
}

func TestAtuinTrial_F001_PositiveCorrection(t *testing.T) {
	// F001 is the canonical "self-correction" conclusion: severity
	// positive, supersedes a prior conclusion with kind=corrects,
	// design_intent=true (the capability-gating is deliberate).
	// This pattern was the primary motivator for several v1
	// schema changes, so it deserves a focused test.
	out := decodeFixture(t)
	require.NotEmpty(t, out.Conclusions)

	f := findConclusion(t, out, "F001")

	assert.Equal(t, SeverityPositive, f.Severity.Default,
		"F001 reduces a prior-assessed risk; severity should be positive")
	assert.True(t, f.DesignIntent,
		"the capability-gating defense is a deliberate architectural choice")
	require.Len(t, f.Supersedes, 1)
	assert.Equal(t, "r1-ai-subsystem-threat", f.Supersedes[0].PriorID)
	assert.Equal(t, SupersessionKindCorrects, f.Supersedes[0].Kind,
		"this is a correction (prior was wrong), not a refinement")
	require.NotNil(t, f.AnswersQuestion)
	assert.Equal(t, "§7", *f.AnswersQuestion,
		"answers the §7 question from the provenance follow-up handoff")
	require.NotNil(t, f.SignalType)
	assert.Equal(t, "ai_capability_gating_model", *f.SignalType)
}

func TestAtuinTrial_F002_NewMediumConclusion(t *testing.T) {
	// F002 is the new medium-severity conclusion (sync censorship).
	// Has prerequisites and remediation_hints — both v1 additions
	// driven by trial feedback.
	out := decodeFixture(t)
	f := findConclusion(t, out, "F002")

	assert.Equal(t, SeverityMedium, f.Severity.Default)
	assert.Empty(t, f.Severity.ByContext, "F002 severity is not context-conditional")
	assert.NotEmpty(t, f.Prerequisites,
		"F002's threat model includes the precondition 'adversary controls sync server'")
	assert.NotEmpty(t, f.RemediationHints,
		"F002 has concrete fix shapes the analyst surfaced")
	assert.Empty(t, f.Supersedes, "F002 is a new conclusion, not a revision")
}

func TestAtuinTrial_F003_ConditionalSeverity(t *testing.T) {
	// F003 exercises the dimensional ContextSpec — three by-context
	// entries covering single_user (low), shared_host (medium),
	// and multi_user+windows (medium).
	out := decodeFixture(t)
	f := findConclusion(t, out, "F003")

	assert.Equal(t, SeverityMedium, f.Severity.Default)
	require.Len(t, f.Severity.ByContext, 3,
		"three deployment contexts: single_user, shared_host, multi_user_windows")

	// Ensure the by-context entries are well-formed and use the
	// expected dimension values.
	contexts := make(map[string]SeverityValue)
	for _, bc := range f.Severity.ByContext {
		key := bc.Context.HostIsolation
		if bc.Context.Platform != "" {
			key = key + "+" + bc.Context.Platform
		}
		contexts[key] = bc.Value
	}
	assert.Equal(t, SeverityLow, contexts[HostIsolationSingleUser])
	assert.Equal(t, SeverityMedium, contexts[HostIsolationSharedHost])
	assert.Equal(t, SeverityMedium, contexts[HostIsolationMultiUser+"+"+PlatformWindows],
		"multi_user_windows splits into host_isolation + platform dimensions")
}

func TestAtuinTrial_PositiveAbsences_UseScopeRefs(t *testing.T) {
	// Migration note (testdata/README.md): the original analyst
	// emission used line_start=1 as a fudge for "I ripgrep'd the
	// whole crate." The migrated fixture uses Scope-based citations
	// for that case. This test guards that the migration didn't
	// regress to line-based citations.
	out := decodeFixture(t)
	require.Len(t, out.PositiveAbsences, 2)

	for i, pa := range out.PositiveAbsences {
		for j, c := range pa.Citations {
			assert.Nil(t, c.LineStart,
				"positive_absences[%d].citations[%d]: should be scope-based, not line-based", i, j)
			require.NotNil(t, c.Scope,
				"positive_absences[%d].citations[%d]: missing scope ref", i, j)
			assert.True(t, ValidScopeKind(c.Scope.Kind),
				"positive_absences[%d].citations[%d]: scope kind %q invalid", i, j, c.Scope.Kind)
		}
	}

	// The unsafe-block absence should be ConfidenceThoroughlyReviewed
	// (the analyst ran ripgrep across both crates).
	// The bcrypt absence should be ConfidenceSpotChecked (an explicit
	// limit they noted — "not a full crypto review").
	confidences := make(map[string]Confidence)
	for _, pa := range out.PositiveAbsences {
		confidences[pa.PatternChecked] = pa.Confidence
	}
	assert.Equal(t, ConfidenceThoroughlyReviewed,
		confidences["presence of `unsafe` blocks in atuin-client and atuin-ai crates"])
	assert.Equal(t, ConfidenceSpotChecked,
		confidences["server-side plain-text password storage or weak password hashing"])
}

func TestAtuinTrial_MethodologyCatalog_HitOnTargetFlag(t *testing.T) {
	// Validates that MethodologyPattern.HitOnTarget distinguishes
	// patterns that produced findings on this engagement (most
	// patterns) from patterns that didn't (MP-CAP-01, the multi-hop
	// AI capability-gating trace, which fired as a positive
	// finding rather than a vulnerability).
	out := decodeFixture(t)
	require.NotNil(t, out.MethodologyTrace)

	hits := 0
	misses := 0
	for _, p := range out.MethodologyTrace.Patterns {
		require.NotNil(t, p.HitOnTarget, "pattern %s missing hit_on_target", p.ID)
		if *p.HitOnTarget {
			hits++
		} else {
			misses++
		}
	}

	// Guard the analyst's documented split: 11 hit, 1 miss.
	// MP-CAP-01 is the documented miss because its detection
	// confirmed atuin's *positive* defense rather than a vuln.
	assert.Equal(t, 11, hits, "11 patterns surfaced conclusions on atuin")
	assert.Equal(t, 1, misses, "MP-CAP-01 didn't surface a vuln on atuin (positive defense)")
}

func TestAtuinTrial_MethodologyCatalog_ComposesWith(t *testing.T) {
	// MP-NET-03 and MP-ENV-01 use the composes_with field — a v1
	// addition driven by trial feedback. Validates that the
	// composes-with references are well-formed.
	out := decodeFixture(t)
	require.NotNil(t, out.MethodologyTrace)

	composers := make(map[string][]string)
	for _, p := range out.MethodologyTrace.Patterns {
		if len(p.ComposesWith) > 0 {
			composers[p.ID] = p.ComposesWith
		}
	}

	// At least these two should compose with others.
	assert.NotEmpty(t, composers["MP-NET-03"],
		"MP-NET-03 (telemetry phone-home) composes with MP-NET-01 + MP-NET-02")
	assert.NotEmpty(t, composers["MP-ENV-01"],
		"MP-ENV-01 (env-var fallback) composes with MP-CAP-01 to surface §7 story")
}

func TestAtuinTrial_TopLevelSupersedes_RefinesRound1(t *testing.T) {
	// The output as a whole refines round 1 (most findings stand,
	// one is corrected, two are new). Top-level supersession kind
	// should be "refines" rather than "corrects" or "deprecates".
	out := decodeFixture(t)
	require.Len(t, out.Supersedes, 1)
	assert.Equal(t, "r1", out.Supersedes[0].PriorID)
	assert.Equal(t, 1, out.Supersedes[0].PriorRound)
	assert.Equal(t, SupersessionKindRefines, out.Supersedes[0].Kind,
		"round 2 refines round 1 — most conclusions preserved, one corrected, new ones added")
}

func TestEmptyOutput_FailsValidation(t *testing.T) {
	out := &AnalystOutput{}
	err := out.Validate()
	require.Error(t, err)
	// The error should mention every required field that's missing.
	// Note: model and invoked_at are server-stamped, not
	// caller-required — empty values are correct, not errors. See
	// TestValidate_ServerStampedFieldsMustBeEmpty for the inverse
	// (caller-supplied values are rejected).
	msg := err.Error()
	assert.Contains(t, msg, "analyst_id")
	assert.Contains(t, msg, "target")
}

func TestNilOutput_FailsValidation(t *testing.T) {
	var out *AnalystOutput
	err := out.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil")
}

// findConclusion looks up a Conclusion by ID and fails the test if absent.
// Used so failures in the F001/F002/F003 specific tests give a clear
// "this conclusion ID is missing from the fixture" error rather than a
// nil-deref panic.
func findConclusion(t *testing.T, out *AnalystOutput, id string) *Conclusion {
	t.Helper()
	for i := range out.Conclusions {
		if out.Conclusions[i].ID == id {
			return &out.Conclusions[i]
		}
	}
	t.Fatalf("conclusion %q not found in fixture", id)
	return nil
}
