package pranalyzer

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/sarahmaeve/pr-analyzer/analyzer"
	"github.com/sarahmaeve/pr-analyzer/codeshape"
	"github.com/sarahmaeve/pr-analyzer/engineerprofile"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal"
)

// fakeSource is an in-memory analyzer.PRSource for testing the collector
// without any HTTP. ListOpenPRs returns `open`; FetchPR returns prs[n],
// or an error if failOn[n] is set.
type fakeSource struct {
	open    []analyzer.PRRef
	prs     map[int]analyzer.PR
	failOn  map[int]bool
	listErr error
}

func (f *fakeSource) ListOpenPRs(_ context.Context, _, _ string) ([]analyzer.PRRef, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.open, nil
}

func (f *fakeSource) FetchPR(_ context.Context, ref analyzer.PRRef) (analyzer.PR, error) {
	if f.failOn[ref.Number] {
		return analyzer.PR{}, errors.New("simulated fetch failure")
	}
	pr, ok := f.prs[ref.Number]
	if !ok {
		return analyzer.PR{}, errors.New("no such PR")
	}
	return pr, nil
}

func sigVal(t *testing.T, res *signal.CollectionResult, typ string) map[string]any {
	t.Helper()
	for _, s := range res.Collected {
		if s.Signal != nil && s.Signal.Type == typ {
			var m map[string]any
			require.NoError(t, json.Unmarshal(s.Signal.Value, &m))
			return m
		}
	}
	t.Fatalf("signal %q not found among collected", typ)
	return nil
}

func hasAbsence(res *signal.CollectionResult, typ string) bool {
	for _, s := range res.Collected {
		if s.Absence != nil && s.Absence.SignalType == typ {
			return true
		}
	}
	return false
}

// TestAggregate exercises the pure folding logic in isolation, with
// hand-built per-PR analyses so the assertions don't depend on
// pr-analyzer's code-shape detection rules.
func TestAggregate(t *testing.T) {
	t.Parallel()

	analyses := []analyzer.Analysis{
		{
			PR: analyzer.PR{Ref: analyzer.PRRef{Number: 7}, Author: "alice"},
			CodeShape: codeshape.Signals{
				LOC:                     codeshape.LOC{Additions: 180, Deletions: 5, Total: 185},
				TestsTouched:            true,
				ManifestsTouched:        []string{"go.mod"},
				Languages:               []string{"Go"},
				AgentConfigPathsTouched: nil,
				ExceedsMaxLOC:           true,
				MaxLOCThreshold:         100,
			},
			EngineerProfile: engineerprofile.Signals{AuthorAssociation: "CONTRIBUTOR"},
		},
		{
			PR: analyzer.PR{Ref: analyzer.PRRef{Number: 5}, Author: "bob"},
			CodeShape: codeshape.Signals{
				LOC:                     codeshape.LOC{Additions: 10, Deletions: 0, Total: 10},
				TestsTouched:            false,
				Languages:               []string{"Markdown"},
				AgentConfigPathsTouched: []string{"CLAUDE.md"},
				ExceedsMaxLOC:           false,
				MaxLOCThreshold:         100,
			},
			EngineerProfile: engineerprofile.Signals{AuthorAssociation: "FIRST_TIME_CONTRIBUTOR"},
		},
	}

	now := time.Now().UTC()
	res := aggregate(aggInput{
		entityID:  "ent-1",
		analyses:  analyses,
		totalOpen: 3,
		truncated: true,
		maxLOC:    100,
	}, now, 24*time.Hour)

	assert.Equal(t, float64(3), sigVal(t, res, "open_pr_count")["count"])

	assoc := sigVal(t, res, "pr_author_association_distribution")
	assert.Equal(t, float64(2), assoc["sampled"])
	dist := assoc["distribution"].(map[string]any)
	assert.Equal(t, float64(1), dist["CONTRIBUTOR"])
	assert.Equal(t, float64(1), dist["FIRST_TIME_CONTRIBUTOR"])

	ft := sigVal(t, res, "pr_first_time_contributor_share")
	assert.Equal(t, float64(1), ft["first_time"])
	assert.Equal(t, 0.5, ft["share"])

	assert.Equal(t, 0.5, sigVal(t, res, "pr_test_touch_rate")["rate"])
	assert.Equal(t, float64(1), sigVal(t, res, "pr_test_touch_rate")["touched"])

	manifest := sigVal(t, res, "pr_dependency_manifest_touch_rate")
	assert.Equal(t, 0.5, manifest["rate"])
	assert.Equal(t, []any{"go.mod"}, manifest["manifests"])

	agent := sigVal(t, res, "pr_agent_config_touch_rate")
	assert.Equal(t, 0.5, agent["rate"])
	assert.Equal(t, []any{"CLAUDE.md"}, agent["paths"])

	over := sigVal(t, res, "pr_oversized_share")
	assert.Equal(t, 0.5, over["share"])
	assert.Equal(t, float64(100), over["threshold"])

	langs := sigVal(t, res, "pr_language_distribution")["distribution"].(map[string]any)
	assert.Equal(t, float64(1), langs["Go"])
	assert.Equal(t, float64(1), langs["Markdown"])

	samples := sigVal(t, res, "pr_queue_samples")
	assert.Equal(t, float64(2), samples["sampled"])
	assert.Equal(t, float64(3), samples["total_open"])
	assert.Equal(t, true, samples["truncated"])
	assert.Len(t, samples["samples"], 2)
}

func TestCollect_EndToEndWithTruncation(t *testing.T) {
	t.Parallel()

	src := &fakeSource{
		open: []analyzer.PRRef{{Number: 7}, {Number: 5}, {Number: 3}},
		prs: map[int]analyzer.PR{
			7: {Ref: analyzer.PRRef{Number: 7}, Author: "alice", AuthorAssociation: "CONTRIBUTOR",
				Additions: 180, Deletions: 5, Files: []analyzer.PRFile{
					{Path: "widget.go"}, {Path: "widget_test.go"}, {Path: "go.mod"},
				}},
			5: {Ref: analyzer.PRRef{Number: 5}, Author: "bob", AuthorAssociation: "FIRST_TIME_CONTRIBUTOR",
				Additions: 10, Files: []analyzer.PRFile{{Path: "CLAUDE.md"}}},
			3: {Ref: analyzer.PRRef{Number: 3}, Author: "carol", AuthorAssociation: "MEMBER",
				Additions: 1, Files: []analyzer.PRFile{{Path: "README.md"}}},
		},
	}
	c := &Collector{src: src, limit: 2, maxLOC: 100, authenticated: true}

	res, err := c.Collect(context.Background(), &profile.Entity{ID: "ent-1", URL: "https://github.com/octo/hello"})
	require.NoError(t, err)

	// open_pr_count is the true total from the full listing, not the sample.
	assert.Equal(t, float64(3), sigVal(t, res, "open_pr_count")["count"])

	samples := sigVal(t, res, "pr_queue_samples")
	assert.Equal(t, float64(2), samples["sampled"])
	assert.Equal(t, true, samples["truncated"])

	// #7 touches widget_test.go; #5 does not → 1/2.
	assert.Equal(t, 0.5, sigVal(t, res, "pr_test_touch_rate")["rate"])
	// #5 touches CLAUDE.md → 1/2.
	assert.Equal(t, 0.5, sigVal(t, res, "pr_agent_config_touch_rate")["rate"])
}

func TestCollect_PerPRFetchFailureSkipped(t *testing.T) {
	t.Parallel()

	src := &fakeSource{
		open:   []analyzer.PRRef{{Number: 7}, {Number: 5}},
		failOn: map[int]bool{5: true},
		prs: map[int]analyzer.PR{
			7: {Ref: analyzer.PRRef{Number: 7}, Author: "alice", AuthorAssociation: "CONTRIBUTOR", Additions: 5,
				Files: []analyzer.PRFile{{Path: "a.go"}}},
		},
	}
	c := &Collector{src: src, limit: 10, maxLOC: 1000, authenticated: true}

	res, err := c.Collect(context.Background(), &profile.Entity{ID: "ent-1", URL: "https://github.com/octo/hello"})
	require.NoError(t, err) // a single PR fetch failure is not fatal

	assert.Equal(t, float64(2), sigVal(t, res, "open_pr_count")["count"])
	samples := sigVal(t, res, "pr_queue_samples")
	assert.Equal(t, float64(1), samples["sampled"])
	assert.Equal(t, float64(1), samples["fetch_failures"])
}

func TestCollect_NoOpenPRs(t *testing.T) {
	t.Parallel()

	src := &fakeSource{open: nil}
	c := &Collector{src: src, limit: 30, maxLOC: 1000, authenticated: true}

	res, err := c.Collect(context.Background(), &profile.Entity{ID: "ent-1", URL: "https://github.com/octo/hello"})
	require.NoError(t, err)

	assert.Equal(t, float64(0), sigVal(t, res, "open_pr_count")["count"])
	// The rate / distribution / samples signals are undefined over an
	// empty queue → recorded as absences, not misleading zeros.
	assert.True(t, hasAbsence(res, "pr_test_touch_rate"))
	assert.True(t, hasAbsence(res, "pr_author_association_distribution"))
	assert.True(t, hasAbsence(res, "pr_queue_samples"))
	assert.Equal(t, 8, res.AbsenceCount())
	assert.Equal(t, 1, res.SignalCount()) // just open_pr_count
}

func TestCollect_ListErrorRecordsFailuresNonFatal(t *testing.T) {
	t.Parallel()

	src := &fakeSource{listErr: errors.New("rate limited")}
	c := &Collector{src: src, limit: 30, maxLOC: 1000, authenticated: true}

	res, err := c.Collect(context.Background(), &profile.Entity{ID: "ent-1", URL: "https://github.com/octo/hello"})
	require.NoError(t, err) // supplementary collector must not nuke the analyze run

	assert.True(t, res.HasFailures())
	assert.Len(t, res.Failures, 9)
	assert.Equal(t, 9, res.AbsenceCount())
}

func TestCollect_NonGitHubEntityIsGated(t *testing.T) {
	t.Parallel()

	src := &fakeSource{open: []analyzer.PRRef{{Number: 1}}}
	c := &Collector{src: src, limit: 30, maxLOC: 1000, authenticated: true}

	res, err := c.Collect(context.Background(), &profile.Entity{ID: "ent-1", URL: "https://gitlab.com/octo/hello"})
	require.NoError(t, err)
	assert.Empty(t, res.Collected)
}

// forbiddenSource is an analyzer.PRSource that fails the test if either
// of its methods is ever called. It proves the unauthenticated self-gate
// returns before any source access (and thus before any HTTP).
type forbiddenSource struct{ t *testing.T }

func (f *forbiddenSource) ListOpenPRs(_ context.Context, _, _ string) ([]analyzer.PRRef, error) {
	f.t.Fatal("ListOpenPRs called while unauthenticated: the rate-budget self-gate did not fire")
	return nil, nil
}

func (f *forbiddenSource) FetchPR(_ context.Context, _ analyzer.PRRef) (analyzer.PR, error) {
	f.t.Fatal("FetchPR called while unauthenticated: the rate-budget self-gate did not fire")
	return analyzer.PR{}, nil
}

// TestCollect_UnauthenticatedGate pins the "no network when
// unauthenticated" contract documented on NewCollector: a 30-PR sample at
// ~2 calls each would exhaust GitHub's 60-req/hour anonymous budget and
// poison sibling collectors, so an unauthenticated Collector must return
// an empty result without touching its PRSource. The forbiddenSource
// t.Fatal's on any access, so reaching the listing/fetch path fails the
// test.
func TestCollect_UnauthenticatedGate(t *testing.T) {
	t.Parallel()

	c := &Collector{src: &forbiddenSource{t: t}, limit: 30, maxLOC: 1000, authenticated: false}

	res, err := c.Collect(context.Background(), &profile.Entity{ID: "ent-1", URL: "https://github.com/octo/hello"})
	require.NoError(t, err)
	require.NotNil(t, res)

	// Empty result: no signals, no absences, no failures — and crucially
	// the source was never invoked (forbiddenSource would have fataled).
	assert.Empty(t, res.Collected)
	assert.Equal(t, 0, res.SignalCount())
	assert.Equal(t, 0, res.AbsenceCount())
	assert.False(t, res.HasFailures())
}

func TestName(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "pr-analyzer", (&Collector{}).Name())
}
