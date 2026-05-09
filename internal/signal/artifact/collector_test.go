package artifact

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal"
)

// TestCollector_Collect_AbsenceWhenNoArtifactURL is the smallest
// integration: when the in-run accumulator carries no artifact_url
// signal for the entity (the upstream registry collector either
// didn't run or had no dist.tarball to emit), the artifact collector
// records a positive_absence for artifact_repo_divergence carrying
// AbsenceReasonNoArtifactURL.
//
// This is the "graceful no-op" branch that lets the dispatch path
// schedule the collector unconditionally on (registry-shaped, git-
// hosted) entities without having to pre-check upstream output. The
// absence row is itself meaningful — it says "we tried, the registry
// side wasn't there" — and lands in the entity's profile so the
// synthesist can distinguish it from "this collector didn't run."
func TestCollector_Collect_AbsenceWhenNoArtifactURL(t *testing.T) {
	t.Parallel()

	// Empty in-run result: no upstream artifact_url emission.
	// Construction mirrors what the orchestrator passes today.
	inRun := &signal.CollectionResult{}

	// Clone path is irrelevant when there's no URL — but supply
	// one to verify the absence path doesn't depend on it.
	collector := NewCollector(CollectorConfig{
		InRun:     inRun,
		ClonePath: "/tmp/some/clone/path",
	})

	entity := &profile.Entity{
		ID:           "e-test",
		CanonicalURI: "pkg:npm/test-pkg",
		Type:         profile.EntityPackage,
		Ecosystem:    "npm",
	}

	result, err := collector.Collect(context.Background(), entity)
	require.NoError(t, err,
		"missing upstream input is not a collection failure — the "+
			"absence row IS the result")
	require.NotNil(t, result)

	// Exactly one absence, no real signals. The absence carries the
	// documented wire-contract reason so the synthesist can
	// distinguish "no URL available" from "couldn't pair tarball."
	assert.Equal(t, 0, result.SignalCount(),
		"no real signals when there's no URL to fetch")
	assert.Equal(t, 1, result.AbsenceCount(),
		"exactly one absence row: no_artifact_url")

	// Absence is recorded against the divergence signal type, not
	// against artifact_url itself — the npm collector owns
	// artifact_url's absence; we own divergence's absence.
	var found bool
	var reason string
	for _, s := range result.Signals() {
		if s.Type == "absence:artifact_repo_divergence" {
			found = true
			// Absence value carries reason in a "reason" field by
			// signal.RecordAbsence's convention.
			reason = string(s.Value)
		}
	}
	require.True(t, found,
		"absence row must be on artifact_repo_divergence — that's the "+
			"signal whose absence the synthesist looks up to know whether "+
			"the comparison was attempted")
	assert.Contains(t, reason, AbsenceReasonNoArtifactURL,
		"absence reason payload must include the documented "+
			"AbsenceReasonNoArtifactURL string verbatim")
}

// TestCollector_Collect_HappyPathEmitsDivergenceSignal is the
// canonical end-to-end test: in-run carries an artifact_url, the
// stub fetcher returns an xz-shaped tarball, the stub git inspector
// returns a tag list and a path list missing the malicious m4 file,
// and the collector emits artifact_repo_divergence with the right
// payload shape.
//
// Stubs let this run as a unit test (no httptest, no real git
// fixture). The HTTP-fetcher and gitenv-inspector implementations
// have their own tests (8c / 8d).
func TestCollector_Collect_HappyPathEmitsDivergenceSignal(t *testing.T) {
	t.Parallel()

	const (
		entityID = "e-xz"
		version  = "5.6.1"
		tag      = "v5.6.1"
		commit   = "deadbeefcafebabe1234567890abcdef12345678"
	)

	// Build the xz-shaped tarball: three benign files plus the
	// malicious m4. Same fixture shape as TestCompare_XZShapedFixture.
	tarball := buildTarGz(t, []tarEntry{
		{path: "src/lzma_decoder.c", body: []byte("// real source")},
		{path: "configure.ac", body: []byte("AC_INIT([xz], [5.6.1])")},
		{path: "m4/build-to-host.m4", body: []byte("# attacker payload")},
	})
	gitPaths := []string{
		"src/lzma_decoder.c",
		"configure.ac",
	}

	inRun := &signal.CollectionResult{}
	inRun.RecordSignal(entityID, "artifact_url", "npm-registry",
		mustParseTime(t, "2026-04-01T00:00:00Z"), 24*time.Hour,
		map[string]any{
			"url":       "https://example.invalid/xz-5.6.1.tar.gz",
			"version":   version,
			"git_head":  "", // force tag-match path
			"integrity": "sha512-AAAA",
		})

	collector := NewCollector(CollectorConfig{
		InRun:     inRun,
		ClonePath: "/fake/clone",
		Fetcher:   &stubFetcher{body: tarball},
		Git: &stubGit{
			tags:        []string{"v5.6.0", tag},
			pathsByRef:  map[string][]string{tag: gitPaths},
			commitByRef: map[string]string{tag: commit},
		},
	})

	entity := &profile.Entity{
		ID:           entityID,
		CanonicalURI: "pkg:npm/xz-utils",
		Type:         profile.EntityPackage,
		Ecosystem:    "npm",
	}

	result, err := collector.Collect(context.Background(), entity)
	require.NoError(t, err)

	assert.Equal(t, 1, result.SignalCount(),
		"happy path emits exactly one artifact_repo_divergence signal")
	assert.Equal(t, 0, result.AbsenceCount(),
		"no absences on happy path")

	// Pull the signal out and verify the payload shape.
	var sig profile.Signal
	for _, s := range result.Signals() {
		if s.Type == "artifact_repo_divergence" {
			sig = s
			break
		}
	}
	require.NotEmpty(t, sig.Type,
		"artifact_repo_divergence signal must be present")

	var cmp Comparison
	require.NoError(t, json.Unmarshal(sig.Value, &cmp))

	assert.Equal(t, "https://example.invalid/xz-5.6.1.tar.gz", cmp.ArtifactURL)
	assert.Equal(t, tag, cmp.GitRef)
	assert.Equal(t, commit, cmp.GitCommit,
		"commit must be resolved via stubGit.CommitForRef when ResolvePair "+
			"returned a tag-match (no commit) — exercises the lookup branch")
	assert.Equal(t, PairConfidenceTagMatch, cmp.PairConfidence)
	assert.Equal(t, 1, cmp.ExtrasInTarballCount,
		"the m4 payload is the one tarball-only file in this fixture")

	// And the headline fact: the malicious file shows up in the
	// sample with the right category. Same assertion shape as
	// TestCompare_XZShapedFixture but flowing through the full
	// collector path.
	var found bool
	for _, e := range cmp.ExtrasInTarballSample {
		if e.Path == "m4/build-to-host.m4" {
			found = true
			assert.Equal(t, CategoryBuildGlue, e.Category)
		}
	}
	assert.True(t, found,
		"m4/build-to-host.m4 must surface through the full collector path "+
			"with category=build_glue — the load-bearing CVE-2024-3094 fact")
}

// stubFetcher returns the same body for every URL — sufficient
// for tests that exercise one Compare flow.
type stubFetcher struct{ body []byte }

func (s *stubFetcher) Fetch(_ context.Context, _ string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(s.body)), nil
}

// stubGit serves a tag list, a per-ref path list, and a per-ref
// commit. Missing-key lookups return errors so tests can exercise
// the not-found branches by omitting an entry.
type stubGit struct {
	tags        []string
	pathsByRef  map[string][]string
	commitByRef map[string]string
}

func (s *stubGit) Tags(context.Context) ([]string, error) {
	return s.tags, nil
}

func (s *stubGit) PathsAtRef(_ context.Context, ref string) ([]string, error) {
	paths, ok := s.pathsByRef[ref]
	if !ok {
		return nil, fmt.Errorf("stubGit: no paths for ref %q", ref)
	}
	return paths, nil
}

func (s *stubGit) CommitForRef(_ context.Context, ref string) (string, error) {
	commit, ok := s.commitByRef[ref]
	if !ok {
		return "", fmt.Errorf("stubGit: no commit for ref %q", ref)
	}
	return commit, nil
}

// mustParseTime is a small RFC3339 helper for the inline signal
// fixtures above. Keeps test bodies readable.
func mustParseTime(t *testing.T, s string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, s)
	require.NoError(t, err)
	return parsed
}

// TestCollector_Name pins the collector's name string. Cheap, but
// the name lands in profile.Signal.Source for every absence/signal
// the collector emits — keeping it stable across refactors matters
// for store-side queries.
func TestCollector_Name(t *testing.T) {
	t.Parallel()
	c := NewCollector(CollectorConfig{})
	assert.Equal(t, "artifact-vs-repo", c.Name())
}
