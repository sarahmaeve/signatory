package openssf

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestValidateOwnerRepo_OK exercises owner/repo shapes the API
// accepts. "Valid" here means we'll let it through to URL
// construction; the API itself decides whether the project exists.
func TestValidateOwnerRepo_OK(t *testing.T) {
	t.Parallel()
	cases := []struct{ owner, repo string }{
		{"sarahmaeve", "signatory"},
		{"open-telemetry", "opentelemetry-proto-go"},
		{"kjd", "idna"},
		{"AzureAD", "msal4j"},  // mixed case
		{"OWASP", "OWASP-ZAP"}, // uppercase + hyphen
		{"foo", "bar.baz"},     // dots in repo
		{"foo_bar", "baz_qux"}, // underscores
		{"a", "b"},             // single-char minimums
	}
	for _, c := range cases {
		t.Run(c.owner+"/"+c.repo, func(t *testing.T) {
			t.Parallel()
			err := ValidateOwnerRepo(c.owner, c.repo)
			assert.NoError(t, err, "expected ValidateOwnerRepo(%q, %q) to accept", c.owner, c.repo)
		})
	}
}

// TestValidateOwnerRepo_Reject covers the cases that have no
// business reaching the API: empty, oversize, embedded
// path/query/fragment metacharacters, and dot-edge-cases that
// could collide with traversal sequences. Each is an injection or
// grammar violation a caller could otherwise smuggle into the URL
// path.
func TestValidateOwnerRepo_Reject(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		owner, repo string
	}{
		{"empty-owner", "", "repo"},
		{"empty-repo", "owner", ""},
		{"slash-in-owner", "owner/sub", "repo"},
		{"slash-in-repo", "owner", "repo/sub"},
		{"null-in-owner", "ow\x00ner", "repo"},
		{"newline-in-repo", "owner", "re\npo"},
		{"space-in-repo", "owner", "re po"},
		{"question-in-owner", "ow?ner", "repo"},
		{"fragment-in-repo", "owner", "repo#x"},
		{"leading-dot-owner", ".owner", "repo"}, // looks like traversal
		{"trailing-dot-repo", "owner", "repo."}, // ditto
		{"parent-traversal", "..", "repo"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateOwnerRepo(c.owner, c.repo)
			assert.Error(t, err, "expected ValidateOwnerRepo(%q, %q) to reject", c.owner, c.repo)
		})
	}
}

// sampleResponse is a real-shape Scorecard response trimmed to the
// fields the collector emits. The wire format includes per-check
// `details` and `documentation` blocks we drop; this fixture pins
// what we actually parse so a future schema drift surfaces here
// rather than as silent zero-aggregation.
const sampleResponse = `{
  "date": "2026-04-21",
  "repo": {
    "name": "github.com/kjd/idna",
    "commit": "6ebfaab9ea718dce38a7c17ddafd7fb28b0468d4"
  },
  "scorecard": {
    "version": "v5.0.0",
    "commit": "abcdef1234567890"
  },
  "score": 7.4,
  "checks": [
    {"name": "Code-Review", "score": 7, "reason": "found 3 unreviewed changesets"},
    {"name": "Branch-Protection", "score": 0, "reason": "branch protection not enabled"},
    {"name": "Signed-Releases", "score": -1, "reason": "no releases found"}
  ]
}`

// TestGetScorecard_OK exercises the happy path: 200 with full
// JSON, full parse, every field that lands in the signal value
// must round-trip from the wire.
func TestGetScorecard_OK(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/projects/github.com/kjd/idna", r.URL.Path,
			"client must build the canonical /projects/github.com/{owner}/{repo} path")
		assert.Equal(t, http.MethodGet, r.Method, "scorecard is read-only — must be GET")
		assert.Equal(t, "application/json", r.Header.Get("Accept"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, sampleResponse)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL(srv.URL)
	sc, err := c.GetScorecard(context.Background(), "kjd", "idna")
	require.NoError(t, err)
	require.NotNil(t, sc)

	assert.InDelta(t, 7.4, sc.AggregateScore, 0.0001)
	assert.Equal(t, "2026-04-21", sc.AsOf)
	assert.Equal(t, "github.com/kjd/idna", sc.Repo.Name)
	assert.Equal(t, "6ebfaab9ea718dce38a7c17ddafd7fb28b0468d4", sc.Repo.Commit)
	assert.Equal(t, "v5.0.0", sc.ScorecardVersion.Version)
	require.Len(t, sc.Checks, 3, "all three sample checks must parse")
	assert.Equal(t, "Code-Review", sc.Checks[0].Name)
	assert.Equal(t, 7, sc.Checks[0].Score)
	assert.Equal(t, -1, sc.Checks[2].Score, "N/A check must preserve -1 (not collapsed to 0)")
}

// TestGetScorecard_NotFound: 404 → ErrNotFound (wrapped). The
// collector branches on errors.Is(err, ErrNotFound) to record a
// non-retryable absence (project not in the index) vs a retryable
// failure (transient network/5xx).
func TestGetScorecard_NotFound(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL(srv.URL)
	sc, err := c.GetScorecard(context.Background(), "kjd", "idna")
	require.Error(t, err)
	assert.Nil(t, sc, "404 must not return a partial Scorecard")
	assert.True(t, errors.Is(err, ErrNotFound),
		"errors.Is must match ErrNotFound so the collector can branch on it; got %v", err)
}

// TestGetScorecard_5xx: server-error responses must be a wrapped
// generic error (NOT ErrNotFound) so the collector records a
// retryable failure rather than a "not in index" absence. Pinning
// this distinction protects against a future regression where 5xx
// gets bucketed as "not found" and the collector falsely concludes
// the project isn't indexed.
func TestGetScorecard_5xx(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream broke", http.StatusBadGateway)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL(srv.URL)
	_, err := c.GetScorecard(context.Background(), "kjd", "idna")
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrNotFound),
		"5xx must NOT be reported as ErrNotFound — it's a transient failure, not a definitive 'absent from index'")
	assert.Contains(t, err.Error(), "502", "error message should preserve the status code for diagnosis")
}

// TestGetScorecard_MalformedJSON: a 200 response with non-JSON
// body must surface as a decode error, not a partial Scorecard.
// Important because Scorecard occasionally returns HTML error
// pages with status 200 from infrastructure layers.
func TestGetScorecard_MalformedJSON(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "<html>this is not json</html>")
	}))
	defer srv.Close()

	c := NewClientWithBaseURL(srv.URL)
	sc, err := c.GetScorecard(context.Background(), "kjd", "idna")
	require.Error(t, err)
	assert.Nil(t, sc)
	assert.Contains(t, err.Error(), "decode", "error must signal decode failure for diagnostics")
}

// TestClient_BoundedResponse confirms an oversize body is
// truncated and surfaced as an error rather than read into
// memory unbounded. Defense against a misbehaving upstream
// streaming an unbounded payload.
func TestClient_BoundedResponse(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Write a body just past the cap. Fixed buffer rather than
		// streaming so the test is fast and deterministic.
		over := strings.Repeat("a", maxResponseSize+10)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, over)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL(srv.URL)
	_, err := c.GetScorecard(context.Background(), "kjd", "idna")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds")
}

// TestGetScorecard_RejectsInvalidOwnerRepo: validation runs before
// any network call. Tests with an empty owner — the test would
// otherwise trip the URL-builder; the validator catches it first.
func TestGetScorecard_RejectsInvalidOwnerRepo(t *testing.T) {
	t.Parallel()
	c := NewClient() // real client; no httptest needed since we never reach the wire
	_, err := c.GetScorecard(context.Background(), "", "repo")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "owner is empty")
}

// Redirect-policy and hop-cap coverage previously lived here. After
// the internal/httpx port, the shared SecureClient owns the redirect
// policy (HTTPS-only, max 10 hops) and its tests live in
// internal/httpx/client_test.go (TestCheckRedirect_*). The pre-port
// openssf-specific test exercised a 10-hop loop against an http
// httptest server, working around the absence of TLS; httpx's strict
// HTTPS-only policy would refuse on hop 1 of that setup, so the test
// shape doesn't carry over. Production behavior is the same: any
// non-HTTPS redirect target from Scorecard's API is refused.

// TestGetScorecard_PathHasNoExtraSegments pins a subtle bug class:
// a future refactor could accidentally double-encode the path or
// add a trailing slash, both of which the upstream rejects. The
// assertion in TestGetScorecard_OK already pins the canonical
// path; this is a minimal smoke for the negative — a path with a
// trailing slash should not be what we send.
func TestGetScorecard_NoTrailingSlash(t *testing.T) {
	t.Parallel()
	var captured string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, sampleResponse)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL(srv.URL)
	_, err := c.GetScorecard(context.Background(), "kjd", "idna")
	require.NoError(t, err)
	assert.Equal(t, "/projects/github.com/kjd/idna", captured,
		fmt.Sprintf("path must not have a trailing slash; got %q", captured))
}
