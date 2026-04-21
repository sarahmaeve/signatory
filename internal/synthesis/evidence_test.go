package synthesis

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/exchange"
	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/store"
)

// fakeStore is an in-memory AssemblerStore for hermetic tests. Each
// method returns struct-field-configured data so individual test
// cases exercise specific paths without touching SQLite. Mirrors the
// pattern used in internal/summary/assemble_test.go.
type fakeStore struct {
	entity      *profile.Entity
	entityErr   error
	outputs     []store.AnalystOutputSummary
	outputsErr  error
	fullByID    map[string]*exchange.AnalystOutput
	fullErrByID map[string]error
	relatedURIs []string
	relatedErr  error
}

func (f *fakeStore) FindEntityByURI(_ context.Context, _ string) (*profile.Entity, error) {
	if f.entityErr != nil {
		return nil, f.entityErr
	}
	return f.entity, nil
}

func (f *fakeStore) ListAnalystOutputs(_ context.Context, _ store.AnalystOutputFilter) ([]store.AnalystOutputSummary, error) {
	return f.outputs, f.outputsErr
}

func (f *fakeStore) GetAnalystOutput(_ context.Context, outputID string) (*exchange.AnalystOutput, error) {
	if err := f.fullErrByID[outputID]; err != nil {
		return nil, err
	}
	if full, ok := f.fullByID[outputID]; ok {
		return full, nil
	}
	return nil, store.ErrNotFound
}

func (f *fakeStore) ListRelatedURIs(_ context.Context, _ string) ([]string, error) {
	return f.relatedURIs, f.relatedErr
}

// TestAssemble_EntityWithNoAnalyses covers the "entity exists but
// nothing ingested" state: the assembler returns a shell Evidence
// with identity fields populated and Analyses empty. Not an error —
// "no evidence to synthesize yet" is a valid response.
func TestAssemble_EntityWithNoAnalyses(t *testing.T) {
	t.Parallel()

	f := &fakeStore{
		entity: &profile.Entity{
			ID:           "ent-1",
			CanonicalURI: "pkg:npm/example",
			Type:         profile.EntityPackage,
			ShortName:    "example",
			URL:          "https://npmjs.com/package/example",
		},
	}

	ev, err := New(f).Assemble(context.Background(), "pkg:npm/example")
	require.NoError(t, err)
	require.NotNil(t, ev)

	assert.Equal(t, "pkg:npm/example", ev.CanonicalURI)
	assert.Equal(t, "example", ev.ShortName)
	assert.Equal(t, "package", ev.EntityType)
	assert.Equal(t, "https://npmjs.com/package/example", ev.URL)
	assert.Empty(t, ev.Analyses)
	assert.Empty(t, ev.RelatedURIs)
}

// TestAssemble_EntityNotFound surfaces ErrEntityNotFound when the
// target doesn't resolve. Synthesist handoffs should never reach
// this path in practice (CLI resolves target first), but the
// assembler is defensive.
func TestAssemble_EntityNotFound(t *testing.T) {
	t.Parallel()

	f := &fakeStore{entityErr: store.ErrNotFound}
	ev, err := New(f).Assemble(context.Background(), "pkg:npm/nonexistent")
	assert.Nil(t, ev)
	assert.True(t, errors.Is(err, ErrEntityNotFound), "expected ErrEntityNotFound, got %v", err)
}

// fixedTime is a stable timestamp for test fixtures that need one.
func fixedTime() time.Time {
	return time.Date(2026, 4, 21, 12, 0, 0, 0, time.UTC)
}

// TestAssemble_OneAnalystOutput_FullContent covers the happy path for
// the deep read: a single analyst output's full conclusions +
// positive_absences + observations show up in the Evidence. This is
// what distinguishes Evidence from Summary — Summary only surfaces
// counts; Evidence carries the content bodies.
func TestAssemble_OneAnalystOutput_FullContent(t *testing.T) {
	t.Parallel()

	now := fixedTime()
	lineStart := 10
	fullDoc := &exchange.AnalystOutput{
		Attribution: exchange.AgentAttribution{
			AnalystID:     "external-sec-v1",
			Model:         "claude-opus-4",
			PromptVersion: "v1.2",
			InvokedAt:     now.Format(time.RFC3339),
			Round:         1,
		},
		Target:       "pkg:npm/example",
		TargetCommit: "abc123",
		Conclusions: []exchange.Conclusion{
			{
				ID:        "F001",
				Verdict:   "unsanitized HTTP input reaches shell",
				Rationale: "traced from handler to exec.Command via args slice",
				Severity:  exchange.Severity{Default: exchange.SeverityHigh},
				Category:  "injection",
				Citations: []exchange.Citation{
					{Path: "src/handler.go", LineStart: &lineStart},
				},
			},
		},
		PositiveAbsences: []exchange.PositiveAbsence{
			{
				PatternChecked: "sql-string-concat",
				Description:    "no SQL string concatenation across the tree",
				Confidence:     exchange.ConfidenceThoroughlyReviewed,
			},
		},
		Observations: []exchange.Observation{
			{
				ID:       "O001",
				Title:    "single-maintainer project",
				Body:     "all merges land from one contributor",
				Category: "governance",
			},
		},
		RoundNotes: "first-round baseline; no prior context",
	}

	f := &fakeStore{
		entity: &profile.Entity{
			ID: "ent-1", CanonicalURI: "pkg:npm/example",
			Type: profile.EntityPackage, ShortName: "example",
		},
		outputs: []store.AnalystOutputSummary{
			{
				OutputID:         "out-sec",
				AnalystID:        "external-sec-v1",
				Model:            "claude-opus-4",
				PromptVersion:    "v1.2",
				InvokedAt:        now.Format(time.RFC3339),
				IngestedAt:       now.Format(time.RFC3339),
				Round:            1,
				TargetCommit:     "abc123",
				CollectedFromURI: "repo:github/example/example",
			},
		},
		fullByID: map[string]*exchange.AnalystOutput{
			"out-sec": fullDoc,
		},
	}

	ev, err := New(f).Assemble(context.Background(), "pkg:npm/example")
	require.NoError(t, err)
	require.Len(t, ev.Analyses, 1)

	a := ev.Analyses[0]
	assert.Equal(t, "out-sec", a.OutputID)
	assert.Equal(t, "external-sec-v1", a.AnalystID)
	assert.Equal(t, "claude-opus-4", a.Model)
	assert.Equal(t, "v1.2", a.PromptVersion)
	assert.Equal(t, 1, a.Round)
	assert.Equal(t, "abc123", a.TargetCommit)
	assert.Equal(t, "repo:github/example/example", a.CollectedFromURI)
	assert.Equal(t, "first-round baseline; no prior context", a.RoundNotes)

	// Deep content — verified via full struct equality against the
	// source document. The assembler must NOT reshape these; they
	// flow through unchanged so the synthesist sees exactly what
	// the analyst wrote.
	assert.Equal(t, fullDoc.Conclusions, a.Conclusions)
	assert.Equal(t, fullDoc.PositiveAbsences, a.PositiveAbsences)
	assert.Equal(t, fullDoc.Observations, a.Observations)
}

// TestAssemble_ExcludesPriorSyntheses is the D9 cross-pollination
// test: prior synthesis outputs (analyst_id prefix
// "signatory-synthesis") must not appear in the Evidence the next
// synthesist reads. If this ever regresses, a re-synthesis of the
// same target will anchor on the prior synthesis's judgment —
// exactly the calibration-drift failure mode D9 was locked to
// prevent. The test passes a mix: one real analyst output + one
// prior synthesis; only the analyst output should land in Analyses.
//
// Critical: the filter must not even call GetAnalystOutput on the
// synthesis row — if it did, a malformed prior synthesis could
// surface in the error path. Use a fullErrByID entry for the
// synthesis row to assert Get is not called.
func TestAssemble_ExcludesPriorSyntheses(t *testing.T) {
	t.Parallel()

	now := fixedTime()
	realAnalyst := &exchange.AnalystOutput{
		Attribution: exchange.AgentAttribution{
			AnalystID: "external-sec-v1",
			Model:     "claude-test",
			InvokedAt: now.Format(time.RFC3339),
		},
		Target: "pkg:npm/example",
		Conclusions: []exchange.Conclusion{
			{ID: "F001", Verdict: "v", Rationale: "r",
				Severity: exchange.Severity{Default: exchange.SeverityMedium},
				Category: "c",
				Citations: []exchange.Citation{
					{Scope: &exchange.ScopeRef{Kind: exchange.ScopeKindWorkspace, Path: "."}},
				}},
		},
	}

	f := &fakeStore{
		entity: &profile.Entity{
			ID: "ent-1", CanonicalURI: "pkg:npm/example",
			Type: profile.EntityPackage, ShortName: "example",
		},
		outputs: []store.AnalystOutputSummary{
			{OutputID: "out-sec", AnalystID: "external-sec-v1", IngestedAt: now.Format(time.RFC3339)},
			{OutputID: "out-synth-v1", AnalystID: "signatory-synthesis-v1", IngestedAt: now.Format(time.RFC3339)},
			{OutputID: "out-synth-v2", AnalystID: "signatory-synthesis-v2-experimental", IngestedAt: now.Format(time.RFC3339)},
		},
		fullByID: map[string]*exchange.AnalystOutput{
			"out-sec": realAnalyst,
		},
		// Synthesis rows' Get must NEVER be called. If the assembler
		// regresses and tries to load them, these errors fail the
		// test loudly with the root cause named.
		fullErrByID: map[string]error{
			"out-synth-v1": errors.New("REGRESSION: GetAnalystOutput called on prior synthesis v1 — D9 filter bypassed"),
			"out-synth-v2": errors.New("REGRESSION: GetAnalystOutput called on prior synthesis v2 — D9 filter bypassed"),
		},
	}

	ev, err := New(f).Assemble(context.Background(), "pkg:npm/example")
	require.NoError(t, err)
	require.Len(t, ev.Analyses, 1,
		"Evidence must contain exactly the one non-synthesis analyst output; prior syntheses are filtered per D9")
	assert.Equal(t, "external-sec-v1", ev.Analyses[0].AnalystID)

	// Belt-and-suspenders: none of the analysts we recorded should
	// have a synthesis-prefix analyst_id.
	for _, a := range ev.Analyses {
		assert.False(t,
			exchange.IsSynthesistRole(a.AnalystID),
			"no synthesis outputs may appear in Evidence.Analyses; found %s",
			a.AnalystID)
	}
}

// TestAssemble_PopulatesRelatedURIs surfaces the M2 cross-identity
// hops in the Evidence so the synthesist sees both sides of any
// resolution link (pkg:npm/X → repo:github/Y or vice versa).
func TestAssemble_PopulatesRelatedURIs(t *testing.T) {
	t.Parallel()

	f := &fakeStore{
		entity: &profile.Entity{
			ID: "ent-1", CanonicalURI: "pkg:npm/example",
			Type: profile.EntityPackage, ShortName: "example",
		},
		relatedURIs: []string{
			"repo:github/example/example",
			"identity:github/example-maintainer",
		},
	}

	ev, err := New(f).Assemble(context.Background(), "pkg:npm/example")
	require.NoError(t, err)
	assert.Equal(t,
		[]string{"repo:github/example/example", "identity:github/example-maintainer"},
		ev.RelatedURIs)
}
