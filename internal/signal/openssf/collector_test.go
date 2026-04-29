package openssf

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
)

// makeEntity is a tiny helper for the eligibility tests — every
// case provides ID and URL. Type defaults to EntityProject (what
// github-hosted entities resolve to); the collector itself
// doesn't gate on Type, only on URL, so this is documentation
// rather than load-bearing.
func makeEntity(id, url string) *profile.Entity {
	return &profile.Entity{
		ID:   id,
		URL:  url,
		Type: profile.EntityProject,
	}
}

// TestCollect_NoEntity returns an empty result with nil error so
// callers can include the collector unconditionally. Symmetric
// with gopublish's nil-entity branch — defensive programming, not
// a real-world dispatch case.
func TestCollect_NoEntity(t *testing.T) {
	t.Parallel()
	c := NewCollector()
	result, err := c.Collect(context.Background(), nil)
	require.NoError(t, err)
	require.NotNil(t, result, "non-nil result is the contract; nil-result would force every caller to nil-guard")
	assert.Empty(t, result.Collected, "no signals on nil entity")
}

// TestCollect_NonGitHubEntity_EmptyResult: an entity without a
// github URL receives no scorecard call and no signal/absence.
// Eligibility filter prevents fanning out a useless API call for
// a non-applicable entity. Mirrors gopublish's empty-result
// branch for non-Go entities.
func TestCollect_NonGitHubEntity_EmptyResult(t *testing.T) {
	t.Parallel()
	c := NewCollector()
	entity := &profile.Entity{
		ID:           "ent-no-github",
		CanonicalURI: "pkg:npm/foo",
		URL:          "", // no resolved github source → not applicable
		Type:         profile.EntityPackage,
	}
	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Empty(t, result.Collected, "non-github entity must skip the API call entirely")
	assert.Empty(t, result.Failures, "skipping is not a failure — no Failures entry")
}

// TestCollect_Success_RecordsScorecardCheck pins the happy path:
// a valid github entity, 200 OK, exactly one signal of the right
// type with the right shape. The signal value's structure is the
// load-bearing contract for downstream consumers (analysts via
// signatory_signals, the synthesis renderer); changing it
// silently would break their reads.
func TestCollect_Success_RecordsScorecardCheck(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, sampleResponse)
	}))
	defer srv.Close()

	c := NewCollectorWithClient(NewClientWithBaseURL(srv.URL))
	entity := makeEntity("ent-idna", "https://github.com/kjd/idna")
	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)
	require.NotNil(t, result)

	require.Len(t, result.Collected, 1, "exactly one signal per entity")
	rec := result.Collected[0]
	require.NotNil(t, rec.Signal, "success path must emit a Signal, not an Absence")

	sig := rec.Signal
	assert.Equal(t, "ent-idna", sig.EntityID)
	assert.Equal(t, "scorecard-check", sig.Type)
	assert.Equal(t, "openssf-scorecard", sig.Source)
	assert.Equal(t, profile.SignalGroupHygiene, sig.Group, "scorecard-check belongs to Hygiene")
	assert.Equal(t, profile.ForgeryVeryHigh, sig.ForgeryResistance,
		"third-party-attested scorecard is hard to forge — must register at VeryHigh")

	// Spot-check the value blob — full round-trip is exercised by
	// client_test.go, here we just confirm the collector emitted
	// the right shape (top-level score plus the per-check map).
	assert.Contains(t, string(sig.Value), `"score":7.4`)
	assert.Contains(t, string(sig.Value), `"as_of":"2026-04-21"`)
	assert.Contains(t, string(sig.Value), `"Code-Review"`,
		"per-check breakdown must surface as a keyed map for analyst lookup")
	assert.Contains(t, string(sig.Value), `"Branch-Protection"`)

	// TTL must match the schema fixture's 7-day cadence.
	expectedExpiry := sig.CollectedAt.Add(7 * 24 * time.Hour)
	assert.True(t, sig.ExpiresAt.Equal(expectedExpiry),
		"TTL must be 7 days per the schema fixture; got CollectedAt=%v ExpiresAt=%v", sig.CollectedAt, sig.ExpiresAt)
}

// TestCollect_NotFound_RecordsAbsence: 404 is a definitive negative,
// not a failure. Project not in Scorecard's index → record absence
// with reason "not in scorecards index", retryable=false. The
// non-retryable flag is load-bearing: a retry policy that re-fetches
// non-retryable absences would burn budget without ever changing
// the result.
func TestCollect_NotFound_RecordsAbsence(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	defer srv.Close()

	c := NewCollectorWithClient(NewClientWithBaseURL(srv.URL))
	entity := makeEntity("ent-noindex", "https://github.com/private/notindexed")
	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err, "404 is not a Collector-level error — only 'cannot proceed at all' returns err")
	require.NotNil(t, result)

	require.Len(t, result.Collected, 1, "404 emits one absence record")
	rec := result.Collected[0]
	require.Nil(t, rec.Signal, "404 must NOT emit a Signal — only an Absence")
	require.NotNil(t, rec.Absence, "404 must emit an Absence record")

	abs := rec.Absence
	assert.Equal(t, "ent-noindex", abs.EntityID)
	assert.Equal(t, "scorecard-check", abs.SignalType)
	assert.Equal(t, "openssf-scorecard", abs.Source)
	assert.Equal(t, "not in scorecards index", abs.Reason,
		"the reason string is the contract analysts read — it must be stable and informative")
	assert.False(t, abs.Retryable,
		"404 is definitive: retry won't change the answer until Scorecard's index does")

	assert.Empty(t, result.Failures,
		"404 is an absence, not a failure — Failures is for transient errors")
}

// TestCollect_NetworkError_RecordsFailure: a 5xx (or any
// non-404 upstream error) lands as a Failure (which appends BOTH
// an absence AND a CollectionError). retryable=true so the next
// refresh retries instead of being suppressed as definitive.
func TestCollect_NetworkError_RecordsFailure(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream broke", http.StatusBadGateway)
	}))
	defer srv.Close()

	c := NewCollectorWithClient(NewClientWithBaseURL(srv.URL))
	entity := makeEntity("ent-flaky", "https://github.com/foo/bar")
	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)
	require.NotNil(t, result)

	// RecordFailure appends BOTH: absence in Collected, error in Failures.
	require.Len(t, result.Collected, 1, "absence must land in Collected so the schema is uniform across runs")
	require.Len(t, result.Failures, 1, "transient error must land in Failures so the run summary tracks it")

	abs := result.Collected[0].Absence
	require.NotNil(t, abs)
	assert.True(t, abs.Retryable, "5xx is transient; future refresh might succeed")
	assert.Contains(t, abs.Reason, "502", "reason should preserve the status for diagnosis")

	fail := result.Failures[0]
	assert.Equal(t, "scorecard-check", fail.SignalType)
	assert.Equal(t, "openssf-scorecard", fail.Source)
	assert.True(t, fail.Retryable)
}

// TestCollect_ContextCanceled: a canceled context produces a
// failure with the readable "collection canceled" reason rather
// than a leaked context.Canceled error string. The reason field
// is what analysts see in posture audit; readable wins.
func TestCollect_ContextCanceled(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Sleep long enough for the cancellation to land. Tests
		// shouldn't actually wait this long — context cancels
		// before the server replies.
		time.Sleep(100 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // immediate cancel — request never completes

	c := NewCollectorWithClient(NewClientWithBaseURL(srv.URL))
	entity := makeEntity("ent-cancel", "https://github.com/foo/bar")
	result, err := c.Collect(ctx, entity)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, result.Failures, 1)
	assert.Equal(t, "collection canceled", result.Failures[0].Reason,
		"cancellation must produce the friendly reason — analysts shouldn't see a raw Go error string")
}

// TestCollector_Name pins the source identifier — flows into the
// signal's Source column, the dogfood report's collector
// attribution, and the posture-audit "what was the source of this
// signal" UX. Renaming requires a deliberate change.
func TestCollector_Name(t *testing.T) {
	t.Parallel()
	c := NewCollector()
	assert.Equal(t, "openssf-scorecard", c.Name())
}

// TestExtractOwnerRepo covers the eligibility filter shapes —
// what URL shapes the collector accepts, and what shapes it
// silently passes on (returning ok=false). Coverage matters here
// because a future bug that broadens or narrows this gate would
// silently affect every dispatch decision.
func TestExtractOwnerRepo(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name                string
		entity              *profile.Entity
		wantOwner, wantRepo string
		wantOK              bool
	}{
		{"nil-entity", nil, "", "", false},
		{"empty-url", &profile.Entity{ID: "x"}, "", "", false},
		{"https-github", makeEntity("x", "https://github.com/kjd/idna"), "kjd", "idna", true},
		{"bare-github", makeEntity("x", "github.com/kjd/idna"), "kjd", "idna", true},
		{"non-github-url", makeEntity("x", "https://gitlab.com/kjd/idna"), "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			owner, repo, ok := extractOwnerRepo(c.entity)
			assert.Equal(t, c.wantOK, ok)
			assert.Equal(t, c.wantOwner, owner)
			assert.Equal(t, c.wantRepo, repo)
		})
	}
}
