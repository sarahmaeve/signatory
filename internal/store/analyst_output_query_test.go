package store

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/exchange"
	"github.com/sarahmaeve/signatory/internal/profile"
)

// ingestAll loads every fixture into a fresh DB and returns it.
// Mirrors the realistic post-backfill state where multiple
// engagements have been ingested.
func ingestAll(t *testing.T) *SQLite {
	t.Helper()
	s := newTestDB(t)
	ctx := context.Background()
	for _, fix := range []struct {
		path string
		out  *exchange.AnalystOutput
	}{
		{path: "../exchange/testdata/atuin-schema-trial.json",
			out: loadFixture(t, "atuin-schema-trial.json")},
		{path: "../exchange/testdata/thefuck-security-v1.json",
			out: loadFixture(t, "thefuck-security-v1.json")},
		{path: "../exchange/testdata/thefuck-provenance-v1.json",
			out: loadFixture(t, "thefuck-provenance-v1.json")},
	} {
		_, err := s.IngestAnalystOutput(ctx, fix.out, fix.path)
		require.NoError(t, err, "backfill %s", fix.path)
	}
	return s
}

func TestListAnalystOutputs_All(t *testing.T) {
	s := ingestAll(t)
	ctx := context.Background()
	out, err := s.ListAnalystOutputs(ctx, AnalystOutputFilter{})
	require.NoError(t, err)
	assert.Len(t, out, 3, "three ingested outputs")

	// Counts should be populated on each summary row.
	for _, o := range out {
		assert.NotEmpty(t, o.OutputID)
		assert.NotEmpty(t, o.EntityURI)
		assert.NotZero(t, o.IngestedAt)
		assert.NotZero(t, o.ConclusionsCount)
	}
}

func TestListAnalystOutputs_FilterByEntityURI(t *testing.T) {
	s := ingestAll(t)
	ctx := context.Background()

	// thefuck has two outputs (security + provenance) sharing one entity.
	thefuckList, err := s.ListAnalystOutputs(ctx, AnalystOutputFilter{
		EntityURI: "repo:github/nvbn/thefuck",
	})
	require.NoError(t, err)
	assert.Len(t, thefuckList, 2)

	atuinList, err := s.ListAnalystOutputs(ctx, AnalystOutputFilter{
		EntityURI: "pkg:cargo/atuin",
	})
	require.NoError(t, err)
	assert.Len(t, atuinList, 1)

	// Unknown entity now surfaces as ErrNotFound so callers can
	// distinguish "target has never been ingested" from "target is
	// known but has no outputs yet". Callers that want the old
	// "absence = empty" behavior wrap with errors.Is explicitly.
	none, err := s.ListAnalystOutputs(ctx, AnalystOutputFilter{
		EntityURI: "pkg:nonexistent/wat",
	})
	require.ErrorIs(t, err, ErrNotFound)
	assert.Empty(t, none)
}

func TestListAnalystOutputs_FilterBySince(t *testing.T) {
	// The Since filter is the SQL-level mechanism backing the
	// CLI's --max-age flag. With the just-ingested row's
	// ingested_at == approximately now, a Since set in the past
	// should include it; a Since set in the future should not.
	s := ingestAll(t)
	ctx := context.Background()

	pastList, err := s.ListAnalystOutputs(ctx, AnalystOutputFilter{
		Since: time.Now().Add(-time.Hour),
	})
	require.NoError(t, err)
	assert.Len(t, pastList, 3, "Since=1h ago includes all three just-ingested rows")

	futureList, err := s.ListAnalystOutputs(ctx, AnalystOutputFilter{
		Since: time.Now().Add(time.Hour),
	})
	require.NoError(t, err)
	assert.Len(t, futureList, 0,
		"Since=1h from now excludes everything (no row's ingested_at is in the future)")

	zeroList, err := s.ListAnalystOutputs(ctx, AnalystOutputFilter{
		Since: time.Time{},
	})
	require.NoError(t, err)
	assert.Len(t, zeroList, 3, "zero-time Since is the no-filter sentinel")
}

func TestListAnalystOutputs_FilterByAnalystID(t *testing.T) {
	s := ingestAll(t)
	ctx := context.Background()

	provList, err := s.ListAnalystOutputs(ctx, AnalystOutputFilter{
		AnalystID: "signatory-provenance-v1",
	})
	require.NoError(t, err)
	require.Len(t, provList, 1)
	assert.Equal(t, "signatory-provenance-v1", provList[0].AnalystID)

	secList, err := s.ListAnalystOutputs(ctx, AnalystOutputFilter{
		AnalystID: "external-sec-v1",
	})
	require.NoError(t, err)
	// atuin trial + thefuck security both have analyst_id=external-sec-v1
	assert.Len(t, secList, 2)
}

func TestListConclusions_FilterBySeverity(t *testing.T) {
	s := ingestAll(t)
	ctx := context.Background()

	// Filter to medium-and-up severities (medium, high, critical).
	high, err := s.ListConclusions(ctx, ConclusionFilter{
		SeverityIn: []exchange.SeverityValue{
			exchange.SeverityHigh, exchange.SeverityCritical,
		},
	})
	require.NoError(t, err)
	// Provenance F001 is the only high we emitted across the corpus.
	assert.Len(t, high, 1)
	assert.Equal(t, "F001", high[0].ConclusionLocalID)
	assert.Equal(t, "high", high[0].SeverityDefault)

	// Positive conclusions (the security analyst's F010 + atuin F001).
	positives, err := s.ListConclusions(ctx, ConclusionFilter{
		SeverityIn: []exchange.SeverityValue{exchange.SeverityPositive},
	})
	require.NoError(t, err)
	assert.Len(t, positives, 2)
}

func TestListConclusions_FilterBySignalType(t *testing.T) {
	s := ingestAll(t)
	ctx := context.Background()

	out, err := s.ListConclusions(ctx, ConclusionFilter{
		SignalType: "default_on_risky_features",
	})
	require.NoError(t, err)
	// thefuck-security has F004, F005, F008 with this signal type.
	assert.Len(t, out, 3)
	for _, f := range out {
		assert.Equal(t, "default_on_risky_features", f.SignalType)
	}
}

func TestListConclusions_FilterByDesignIntent(t *testing.T) {
	s := ingestAll(t)
	ctx := context.Background()

	out, err := s.ListConclusions(ctx, ConclusionFilter{DesignIntentOnly: true})
	require.NoError(t, err)
	// thefuck-security: F001, F004, F005, F007, F008. atuin trial F001.
	assert.Len(t, out, 6, "six design_intent conclusions across the corpus")
	for _, f := range out {
		assert.True(t, f.DesignIntent)
	}
}

func TestListConclusions_FilterByEntity(t *testing.T) {
	s := ingestAll(t)
	ctx := context.Background()

	thefuckConclusions, err := s.ListConclusions(ctx, ConclusionFilter{
		EntityURI: "repo:github/nvbn/thefuck",
	})
	require.NoError(t, err)
	// 10 security + 6 provenance = 16
	assert.Len(t, thefuckConclusions, 16)

	atuinConclusions, err := s.ListConclusions(ctx, ConclusionFilter{
		EntityURI: "pkg:cargo/atuin",
	})
	require.NoError(t, err)
	assert.Len(t, atuinConclusions, 3)
}

func TestListConclusions_PreservesSupersedesFlag(t *testing.T) {
	s := ingestAll(t)
	ctx := context.Background()

	out, err := s.ListConclusions(ctx, ConclusionFilter{
		EntityURI: "pkg:cargo/atuin",
	})
	require.NoError(t, err)
	for _, f := range out {
		if f.ConclusionLocalID == "F001" {
			assert.True(t, f.HasSupersedes,
				"atuin trial F001 supersedes r1-ai-subsystem-threat")
			return
		}
	}
	t.Fatal("atuin F001 not found")
}

func TestListMethodologyPatterns_FilterBySignalGroup(t *testing.T) {
	s := ingestAll(t)
	ctx := context.Background()

	out, err := s.ListMethodologyPatterns(ctx, MethodologyPatternFilter{
		SignalGroup: "network_endpoints",
	})
	require.NoError(t, err)
	// atuin trial: 3 (MP-NET-01/02/03). thefuck-security: 1 (MP-PY-NET-01).
	// thefuck-provenance: 0 (different naming under publication_integrity).
	assert.Len(t, out, 4)
	for _, p := range out {
		assert.Equal(t, "network_endpoints", p.SignalGroup)
	}
}

func TestListMethodologyPatterns_FilterByHitOnTarget(t *testing.T) {
	s := ingestAll(t)
	ctx := context.Background()

	hit := true
	hits, err := s.ListMethodologyPatterns(ctx, MethodologyPatternFilter{
		HitOnTarget: &hit,
	})
	require.NoError(t, err)
	miss := false
	misses, err := s.ListMethodologyPatterns(ctx, MethodologyPatternFilter{
		HitOnTarget: &miss,
	})
	require.NoError(t, err)

	// Most patterns hit; a small number didn't (e.g. atuin MP-CAP-01,
	// thefuck-security MP-PY-NET-01 + MP-PY-INSTALL-01 + MP-PY-SECRET-01).
	assert.Greater(t, len(hits), 0)
	assert.Greater(t, len(misses), 0)
	assert.Equal(t, 40, len(hits)+len(misses), "all 40 patterns accounted for via hit/miss")
}

func TestGetAnalystOutput_RoundTripsAtuinFixture(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	original := loadFixture(t, "atuin-schema-trial.json")

	res, err := s.IngestAnalystOutput(ctx, original, "test")
	require.NoError(t, err)

	got, err := s.GetAnalystOutput(ctx, res.OutputID)
	require.NoError(t, err)
	require.NotNil(t, got)

	// Headline equality checks. Full DeepEqual is too strict because
	// attribution.ingested_at and a few other fields aren't
	// round-tripped (they're DB-side timestamps, not analyst-emitted).
	assert.Equal(t, original.Target, got.Target)
	assert.Equal(t, original.Attribution.AnalystID, got.Attribution.AnalystID)
	assert.Equal(t, original.Attribution.Model, got.Attribution.Model)
	assert.Equal(t, original.Attribution.Round, got.Attribution.Round)
	assert.Equal(t, original.RoundNotes, got.RoundNotes)
	assert.Equal(t, original.TargetCommit, got.TargetCommit)

	require.Len(t, got.Conclusions, len(original.Conclusions))
	assertConclusionsEquivalent(t, original.Conclusions, got.Conclusions)

	require.Len(t, got.PositiveAbsences, len(original.PositiveAbsences))
	for i, pa := range original.PositiveAbsences {
		assert.Equal(t, pa.PatternChecked, got.PositiveAbsences[i].PatternChecked)
		assert.Equal(t, pa.Description, got.PositiveAbsences[i].Description)
		assert.Equal(t, pa.Confidence, got.PositiveAbsences[i].Confidence)
	}

	require.Len(t, got.Observations, len(original.Observations))

	require.NotNil(t, got.MethodologyTrace)
	require.Len(t, got.MethodologyTrace.Patterns, len(original.MethodologyTrace.Patterns))

	require.Len(t, got.Supersedes, len(original.Supersedes))
	for i, sup := range original.Supersedes {
		assert.Equal(t, sup.PriorID, got.Supersedes[i].PriorID)
		assert.Equal(t, sup.PriorRound, got.Supersedes[i].PriorRound)
		assert.Equal(t, sup.Kind, got.Supersedes[i].Kind)
	}
}

func TestGetAnalystOutput_NotFound(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	_, err := s.GetAnalystOutput(ctx, "nonexistent-uuid")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestGetAnalystOutput_PreservesPolyCitations(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	original := loadFixture(t, "atuin-schema-trial.json")
	res, err := s.IngestAnalystOutput(ctx, original, "")
	require.NoError(t, err)
	got, err := s.GetAnalystOutput(ctx, res.OutputID)
	require.NoError(t, err)

	// A conclusion's line-based citation should round-trip with line_start
	// set and Scope nil; a positive_absence's scope-based citation
	// should round-trip with Scope set and LineStart nil.
	for _, f := range got.Conclusions {
		if f.ID != "F001" {
			continue
		}
		require.NotEmpty(t, f.Citations)
		assert.NotNil(t, f.Citations[0].LineStart, "F001 citation is line-based")
		assert.Nil(t, f.Citations[0].Scope)
	}
	for _, pa := range got.PositiveAbsences {
		if pa.PatternChecked != "presence of `unsafe` blocks in atuin-client and atuin-ai crates" {
			continue
		}
		require.NotEmpty(t, pa.Citations)
		assert.Nil(t, pa.Citations[0].LineStart, "scope-based citation has nil line_start")
		require.NotNil(t, pa.Citations[0].Scope)
		assert.Equal(t, "crate", pa.Citations[0].Scope.Kind)
	}
}

// assertConclusionsEquivalent checks the load-bearing Conclusion fields
// match. Doesn't try DeepEqual because pointer-vs-empty on optional
// fields varies between marshal/unmarshal cycles (same omitempty
// caveat that the round-trip tests have always had).
func assertConclusionsEquivalent(t *testing.T, want, got []exchange.Conclusion) {
	t.Helper()
	for i := range want {
		w, g := &want[i], &got[i]
		assert.Equal(t, w.ID, g.ID, "conclusion[%d] ID", i)
		assert.Equal(t, w.Verdict, g.Verdict, "conclusion[%d] Verdict", i)
		assert.Equal(t, w.Rationale, g.Rationale, "conclusion[%d] Rationale", i)
		assert.Equal(t, w.Severity.Default, g.Severity.Default, "conclusion[%d] severity.default", i)
		assert.Equal(t, w.DesignIntent, g.DesignIntent, "conclusion[%d] design_intent", i)
		assert.Equal(t, w.Category, g.Category, "conclusion[%d] category", i)
		// SignalType is *string; nil-vs-pointer-to-empty-string
		// matters. Compare via deref.
		assert.Equal(t,
			derefStringPtr(w.SignalType), derefStringPtr(g.SignalType),
			"conclusion[%d] signal_type", i)
		assert.Len(t, g.Citations, len(w.Citations), "conclusion[%d] citation count", i)
		assert.Len(t, g.Supersedes, len(w.Supersedes), "conclusion[%d] supersedes count", i)
		assert.Equal(t, len(w.Severity.ByContext), len(g.Severity.ByContext),
			"conclusion[%d] by_context count", i)
		// RelatedConclusions round-trips
		assert.True(t, reflect.DeepEqual(sortedCopy(w.RelatedConclusions), sortedCopy(g.RelatedConclusions)),
			"conclusion[%d] related_conclusions", i)
	}
}

// TestGetLatestSynthesisForEntity covers the read-side primitive that
// backs the signatory_show_synthesis MCP tool. Three cases:
//
//  1. Multiple synthesis rounds for one entity → newest by ingested_at
//     wins. Synthesis rows are not superseded, so "latest" is the
//     canonical answer; multi-round synthesis (an analyst re-running
//     over fresh inputs) gets the freshest take.
//  2. Entity with analyst (non-synthesis) outputs but no synthesis row
//     → ErrNotFound. "We have analyses but no synthesist verdict" is
//     the same caller-facing answer as "no synthesis" — the tool
//     surfaces this as `specified synthesis does not exist`.
//  3. Empty entity ID → ErrNilInput, matching the contract every
//     other store method that takes an ID has.
func TestGetLatestSynthesisForEntity_LatestRoundWins(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	input := synthesisTestInput()
	sess := newSessionFixture(t, s, input.Target)
	require.NoError(t, s.CreateAnalysisSession(ctx, sess))

	// First synthesis round: ingest with one summary, then bump the
	// supplement to make a different content_hash and ingest a second
	// round. The second has a later ingested_at and is the "latest"
	// the helper must surface.
	first, err := s.IngestAnalystOutput(ctx, input, "round-1",
		WithAnalysisSession(sess.ID))
	require.NoError(t, err)

	// Brief pause so ingested_at differs second-precision-stably.
	time.Sleep(1100 * time.Millisecond)

	input2 := synthesisTestInput()
	input2.SynthesisSupplement.Summary = "second-round summary, supersedes first"
	input2.Attribution.Round = 2
	second, err := s.IngestAnalystOutput(ctx, input2, "round-2",
		WithAnalysisSession(sess.ID))
	require.NoError(t, err)
	require.NotEqual(t, first.OutputID, second.OutputID,
		"second ingest must produce a distinct row (changed Summary changes content_hash)")

	gotID, got, err := s.GetLatestSynthesisForEntity(ctx, first.EntityID)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.NotNil(t, got.SynthesisSupplement,
		"GetLatestSynthesisForEntity must return a fully-hydrated AnalystOutput including SynthesisSupplement")
	assert.Equal(t, second.OutputID, gotID,
		"the output_id of the round-2 row must surface so MCP callers can navigate to that specific row")
	assert.Equal(t, "second-round summary, supersedes first",
		got.SynthesisSupplement.Summary,
		"newest by ingested_at wins")
	assert.Equal(t, 2, got.Attribution.Round)
}

func TestGetLatestSynthesisForEntity_EntityWithoutSynthesis(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()

	// Seed an entity, then ingest a non-synthesis analyst output
	// against it. No synthesis row should mean ErrNotFound for the
	// "latest synthesis" lookup.
	entity := &profile.Entity{
		ID:           uuid.NewString(),
		CanonicalURI: "pkg:npm/no-synthesis-here",
		Type:         profile.EntityProject,
		ShortName:    "no-synthesis-here",
	}
	require.NoError(t, s.PutEntity(ctx, entity))

	nonSynth := &exchange.AnalystOutput{
		Attribution: exchange.AgentAttribution{
			AnalystID: "signatory-security-v1",
			// Model and InvokedAt server-stamped at ingest.
		},
		Target: "pkg:npm/no-synthesis-here",
		Conclusions: []exchange.Conclusion{
			{
				ID: "F001", Verdict: "v", Rationale: "r",
				Severity: exchange.Severity{Default: exchange.SeverityLow},
				Category: "c",
				Citations: []exchange.Citation{
					{Path: "src/x.go", LineStart: ptrInt(1)},
				},
			},
		},
	}
	_, err := s.IngestAnalystOutput(ctx, nonSynth, "non-synthesis-test")
	require.NoError(t, err)

	_, _, err = s.GetLatestSynthesisForEntity(ctx, entity.ID)
	assert.ErrorIs(t, err, ErrNotFound,
		"entity with only Layer-2 analyst outputs has no synthesis; ErrNotFound is the right sentinel")
}

func TestGetLatestSynthesisForEntity_EmptyEntityID(t *testing.T) {
	s := newTestDB(t)
	ctx := context.Background()
	_, _, err := s.GetLatestSynthesisForEntity(ctx, "")
	assert.ErrorIs(t, err, ErrNilInput,
		"empty entityID is malformed; surface ErrNilInput consistently with other ID-taking methods")
}

// ptrInt returns a *int holding v. Local helper because conclusion
// citations use *int for line numbers.
func ptrInt(v int) *int { return &v }

func derefStringPtr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}
