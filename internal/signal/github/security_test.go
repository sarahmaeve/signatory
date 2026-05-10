package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal"
)

// findTokenInResult walks every reachable field of a CollectionResult
// looking for the secret token. Returns (location, true) on the first
// match, ("", false) if no field contains it. The location string
// identifies which field carried the token so failure messages are
// actionable.
//
// Issue #96: previously, TestSecurity_TokenNotLeakedInAbsenceSignals
// only iterated result.Signals() and missed result.Failures entirely.
// A regression that leaked the raw error (with the token embedded)
// into the Failures slice but not into the absence signals would have
// passed the old test. This helper closes that gap by walking BOTH
// slices, plus performing a catch-all JSON-marshal scan as defense
// in depth against any field added to CollectionResult in the future
// that the per-field checks below forget to cover.
func findTokenInResult(result *signal.CollectionResult, token string) (string, bool) {
	if result == nil {
		return "", false
	}
	for _, sig := range result.Signals() {
		if strings.Contains(string(sig.Value), token) {
			return fmt.Sprintf("signal[%s].Value", sig.ID), true
		}
		if strings.Contains(sig.ID, token) {
			return fmt.Sprintf("signal[%s].ID", sig.ID), true
		}
		if strings.Contains(sig.Source, token) {
			return fmt.Sprintf("signal[%s].Source", sig.ID), true
		}
		if strings.Contains(sig.Type, token) {
			return fmt.Sprintf("signal[%s].Type", sig.ID), true
		}
	}
	for i, failure := range result.Failures {
		if strings.Contains(failure.Reason, token) {
			return fmt.Sprintf("failure[%d].Reason", i), true
		}
		if strings.Contains(failure.SignalType, token) {
			return fmt.Sprintf("failure[%d].SignalType", i), true
		}
		if strings.Contains(failure.Source, token) {
			return fmt.Sprintf("failure[%d].Source", i), true
		}
	}
	// Catch-all: marshal the whole thing and search the JSON. This
	// catches any field we forgot to check above, including future
	// fields added to CollectionResult.
	data, err := json.Marshal(result)
	if err == nil && strings.Contains(string(data), token) {
		return "CollectionResult JSON serialization (some field not covered by per-field checks above)", true
	}
	return "", false
}

// assertNoTokenInResult fails the test if the secret token appears
// anywhere in the CollectionResult, with a location string identifying
// which field leaked.
func assertNoTokenInResult(t *testing.T, result *signal.CollectionResult, token string) {
	t.Helper()
	if loc, found := findTokenInResult(result, token); found {
		t.Errorf("secret token leaked at %s — TOKEN LEAK", loc)
	}
}

// TestSecurity_GetFileRaw_RejectsMalformedPath verifies that GetFileRaw
// validates the `path` parameter before constructing a GitHub contents
// API URL. Issue #90: GetFileRaw and GetDirectoryContents both build
// URLs via fmt.Sprintf("/repos/%s/%s/contents/%s", owner, repoName,
// path) with no path validation. owner/repoName are pre-validated by
// ParseRepoURL upstream, but path was a free-form string.
//
// Today's callers all pass hardcoded constants ("go.mod",
// ".github/workflows", etc.) so the bug is latent. But:
//
//   - A future caller that forwards user-controlled content as `path`
//     silently becomes a path-injection or query-injection bug
//   - Examples: "go.mod?ref=injected" appends a query string,
//     "../../other-repo/contents/secret" attempts cross-repo path
//     traversal, "go.mod#frag" injects a fragment, "path\x00null"
//     could truncate the URL on some servers
//
// The test uses a test server that records all requests. After the
// fix, the validator rejects malformed paths client-side and the
// test server is never called. Each row asserts:
//   - GetFileRaw returns an error (the validator's error)
//   - The test server captured ZERO requests for that test (the
//     validator fired before any HTTP call)
//
// Pre-fix behavior: GetFileRaw silently sends the malformed path to
// the API. The test server responds 404 (no route). GetFileRaw
// interprets 404 as "file not found" and returns (nil, nil) — no
// error returned to the caller, no signal that anything is wrong.
// The test fails on `require.Error` because err is nil.
func TestSecurity_GetFileRaw_RejectsMalformedPath(t *testing.T) {
	bad := []struct {
		name string
		path string
	}{
		{"empty", ""},
		{"path traversal", "../etc/passwd"},
		{"path traversal mid-path", "subdir/../../etc/passwd"},
		{"just dot-dot", ".."},
		{"query string injection", "go.mod?ref=injected"},
		{"fragment injection", "go.mod#frag"},
		{"null byte", "path\x00nullbyte"},
		{"newline", "path\nnewline"},
		{"leading slash", "/leading/slash"},
		{"trailing slash", "trailing/slash/"},
		{"double slash", "path//double"},
		{"non-ASCII Cyrillic lookalike", "lod\u0430sh"},
		{"space in path", "path with space"},
		{"shell metacharacter", "path;rm -rf /"},
		{"backtick", "path`whoami`"},
		{"too long", strings.Repeat("a/", 200)},
	}

	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			var hits int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits++
				w.WriteHeader(http.StatusNotFound)
			}))
			defer server.Close()

			client := &Client{
				httpClient: server.Client(),
				token:      "test-token",
				baseURL:    server.URL,
			}

			_, err := client.GetFileRaw(context.Background(), "owner", "repo", tc.path)
			require.Error(t, err, "GetFileRaw must reject malformed path %q", tc.path)
			assert.Zero(t, hits,
				"GetFileRaw must reject the path BEFORE any HTTP request — got %d hits to the test server", hits)
		})
	}
}

// TestSecurity_GetDirectoryContents_RejectsMalformedPath is the
// directory-contents counterpart. Same validator, same threat model,
// different entry point. Smaller test set since the underlying
// validation logic is shared with GetFileRaw above.
func TestSecurity_GetDirectoryContents_RejectsMalformedPath(t *testing.T) {
	bad := []string{
		"../etc",
		"path?ref=x",
		"path\x00null",
		"/leading",
	}
	for _, p := range bad {
		t.Run(p, func(t *testing.T) {
			var hits int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits++
				w.WriteHeader(http.StatusNotFound)
			}))
			defer server.Close()

			client := &Client{
				httpClient: server.Client(),
				token:      "test-token",
				baseURL:    server.URL,
			}

			_, err := client.GetDirectoryContents(context.Background(), "owner", "repo", p)
			require.Error(t, err, "GetDirectoryContents must reject malformed path %q", p)
			assert.Zero(t, hits,
				"GetDirectoryContents must reject the path BEFORE any HTTP request")
		})
	}
}

// TestSecurity_FindTokenInResult_DetectsKnownLeakPositions is the
// unit test for findTokenInResult. It synthetically constructs
// CollectionResults with the token at each known leak position and
// asserts the helper detects each one. This is the test of the
// test infrastructure — the corresponding integration tests below
// (TestSecurity_TokenNotLeaked*) verify the production code is
// currently clean, and rely on this helper to catch any future leak.
func TestSecurity_FindTokenInResult_DetectsKnownLeakPositions(t *testing.T) {
	const token = "ghp_synthetic_test_token_1234567890"

	makeResult := func(setup func(r *signal.CollectionResult)) *signal.CollectionResult {
		r := &signal.CollectionResult{}
		setup(r)
		return r
	}

	tests := []struct {
		name      string
		result    *signal.CollectionResult
		wantFound bool
		wantLoc   string
	}{
		{
			name:      "clean result",
			result:    &signal.CollectionResult{},
			wantFound: false,
		},
		{
			name:      "nil result",
			result:    nil,
			wantFound: false,
		},
		{
			name: "token in signal Value",
			result: makeResult(func(r *signal.CollectionResult) {
				r.Collected = append(r.Collected, signal.MakeSignal(profile.Signal{
					ID:    "sig-1",
					Type:  "stars",
					Value: json.RawMessage(`{"secret":"` + token + `"}`),
				}))
			}),
			wantFound: true,
			wantLoc:   "Value",
		},
		{
			name: "token in signal Source",
			result: makeResult(func(r *signal.CollectionResult) {
				r.Collected = append(r.Collected, signal.MakeSignal(profile.Signal{
					ID:     "sig-1",
					Type:   "stars",
					Source: token,
					Value:  json.RawMessage(`{}`),
				}))
			}),
			wantFound: true,
			wantLoc:   "Source",
		},
		{
			name: "token in failure Reason",
			result: makeResult(func(r *signal.CollectionResult) {
				r.Failures = append(r.Failures, signal.CollectionError{
					SignalType: "stars",
					Source:     "github",
					Reason:     "error with token " + token,
				})
			}),
			wantFound: true,
			wantLoc:   "failure[0].Reason",
		},
		{
			name: "token in failure SignalType",
			result: makeResult(func(r *signal.CollectionResult) {
				r.Failures = append(r.Failures, signal.CollectionError{
					SignalType: token,
					Source:     "github",
					Reason:     "ok",
				})
			}),
			wantFound: true,
			wantLoc:   "failure[0].SignalType",
		},
		{
			name: "token in failure Source",
			result: makeResult(func(r *signal.CollectionResult) {
				r.Failures = append(r.Failures, signal.CollectionError{
					SignalType: "stars",
					Source:     token,
					Reason:     "ok",
				})
			}),
			wantFound: true,
			wantLoc:   "failure[0].Source",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			loc, found := findTokenInResult(tc.result, token)
			assert.Equal(t, tc.wantFound, found, "wrong found result")
			if tc.wantFound {
				assert.Contains(t, loc, tc.wantLoc,
					"location string should identify which field leaked")
			}
		})
	}
}

// TestSecurity_TokenNotLeakedInCollectionResult verifies that the
// GITHUB_TOKEN does not appear ANYWHERE in the CollectionResult when
// API calls fail — signals, absence signals, failures, or any future
// field. Issue #96: the previous version of this test
// (TestSecurity_TokenNotLeakedInAbsenceSignals) only iterated
// result.Signals() and missed result.Failures entirely. A regression
// that leaked the token into the Failures slice would have passed.
//
// Uses assertNoTokenInResult, which walks every reachable field plus
// a JSON-marshal catch-all. See findTokenInResult for the leak
// position list.
func TestSecurity_TokenNotLeakedInCollectionResult(t *testing.T) {
	secretToken := "ghp_SuperSecretToken1234567890abcdef"

	// Server that returns 500 for everything except the repo endpoint
	// (which must succeed for collection to proceed).
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo", func(w http.ResponseWriter, r *http.Request) {
		// Verify the token IS being sent in the request.
		auth := r.Header.Get("Authorization")
		assert.Contains(t, auth, secretToken, "token should be in request header")

		json.NewEncoder(w).Encode(repo{
			Name:  "repo",
			Owner: repoOwner{Login: "owner", Type: "User"},
		})
	})

	// All other endpoints return errors that might leak the token.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Simulate an error response that includes auth info in the body
		// (some APIs do this in error messages).
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"message":"Internal error processing request with token ` + secretToken + `"}`))
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	client := &Client{
		httpClient: server.Client(),
		token:      secretToken,
		baseURL:    server.URL,
	}
	collector := NewCollectorWithClient(client)

	entity := &profile.Entity{
		ID:        "test-entity",
		Type:      profile.EntityPackage,
		ShortName: "owner/repo",
	}

	result, err := collector.Collect(context.Background(), entity)
	require.NoError(t, err, "partial collection should not return error")

	// Sanity check: collection actually produced failures so this test
	// is exercising the failure-path coverage that #96 was about. If
	// failures becomes empty, the test would pass trivially without
	// catching anything in the failures slice.
	require.NotEmpty(t, result.Failures,
		"test setup error: expected at least one failure to exercise the failure-slice token-leak coverage")

	assertNoTokenInResult(t, result, secretToken)
}

// --- CI/CD False Negative Prevention (Issue #42) ---

// TestSecurity_RateLimitedCICheckProducesRetryableAbsence verifies that
// when CI/CD config checks are rate-limited, the result is a retryable
// absence signal, NOT a false "no CI/CD detected" signal.
func TestSecurity_RateLimitedCICheckProducesRetryableAbsence(t *testing.T) {
	mux := http.NewServeMux()

	// Repo succeeds.
	mux.HandleFunc("/repos/owner/repo", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(repo{
			Name: "repo", Owner: repoOwner{Login: "owner", Type: "User"},
			StargazersCount: 100,
		})
	})

	// All other endpoints succeed normally.
	mux.HandleFunc("/repos/owner/repo/contributors", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]contributor{{Login: "owner", Contributions: 10}})
	})
	mux.HandleFunc("/repos/owner/repo/commits", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]commit{{
			Commit: commitData{Author: commitPerson{Date: time.Now()}, Verification: verification{Verified: true}},
		}})
	})
	mux.HandleFunc("/repos/owner/repo/tags", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]tag{})
	})
	mux.HandleFunc("/users/owner", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(user{Login: "owner", CreatedAt: time.Now()})
	})
	// /search/code handler removed: adoption moved to
	// internal/signal/adoption, github collector no longer calls
	// the search endpoint.

	// CI/CD checks are rate-limited.
	mux.HandleFunc("/repos/owner/repo/contents/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Reset", "1712700000")
		w.WriteHeader(http.StatusForbidden)
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	client := &Client{httpClient: server.Client(), token: "test", baseURL: server.URL}
	collector := NewCollectorWithClient(client)

	entity := &profile.Entity{ID: "test", Type: profile.EntityPackage, ShortName: "owner/repo"}
	result, err := collector.Collect(context.Background(), entity)
	require.NoError(t, err)
	signals := result.Signals()

	// Find the ci_cd signal.
	for _, sig := range signals {
		if sig.Type == "absence:ci_cd" {
			var val map[string]any
			require.NoError(t, json.Unmarshal(sig.Value, &val))

			// CRITICAL: Must be retryable, not a definitive "no CI found".
			assert.Equal(t, true, val["retryable"],
				"rate-limited CI check should be retryable, not a definitive negative")
			assert.NotEqual(t, "no CI/CD configuration detected", val["reason"],
				"rate-limited CI check should NOT produce 'no CI/CD detected' — that's a false negative")
			return
		}
		if sig.Type == "ci_cd" {
			t.Fatal("rate-limited CI check should NOT produce a positive ci_cd signal")
		}
	}

	// If we get here, there's no ci_cd or absence:ci_cd signal at all.
	t.Fatal("expected an absence:ci_cd signal for rate-limited CI check, found none")
}

// --- Missing Absence Signals (Issue #52) ---

// TestSecurity_ZeroCommitRepoProducesAbsence verifies that repos with
// no commits produce absence signals rather than silently omitting
// commit-related signals.
func TestSecurity_ZeroCommitRepoProducesAbsence(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/empty", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(repo{
			Name: "empty", Owner: repoOwner{Login: "owner", Type: "User"},
		})
	})
	mux.HandleFunc("/repos/owner/empty/contributors", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]contributor{})
	})
	mux.HandleFunc("/repos/owner/empty/commits", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("per_page") == "1" {
			json.NewEncoder(w).Encode([]commit{})
			return
		}
		json.NewEncoder(w).Encode([]commit{}) // Empty — no commits.
	})
	mux.HandleFunc("/repos/owner/empty/tags", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]tag{})
	})
	mux.HandleFunc("/users/owner", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(user{Login: "owner", CreatedAt: time.Now()})
	})
	// /search/code handler removed: see comment in the earlier
	// occurrence in this file.
	mux.HandleFunc("/repos/owner/empty/contents/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	client := &Client{httpClient: server.Client(), baseURL: server.URL}
	collector := NewCollectorWithClient(client)

	entity := &profile.Entity{ID: "test", Type: profile.EntityPackage, ShortName: "owner/empty"}
	result, err := collector.Collect(context.Background(), entity)
	require.NoError(t, err)
	signals := result.Signals()

	// Should have absence signals for last_commit and commit_signing.
	hasAbsenceLastCommit := false
	hasAbsenceCommitSigning := false
	hasAbsenceLicense := false
	for _, sig := range signals {
		switch sig.Type {
		case "absence:last_commit":
			hasAbsenceLastCommit = true
		case "absence:commit_signing":
			hasAbsenceCommitSigning = true
		case "absence:license":
			hasAbsenceLicense = true
		}
	}

	assert.True(t, hasAbsenceLastCommit, "zero-commit repo should have absence:last_commit")
	assert.True(t, hasAbsenceCommitSigning, "zero-commit repo should have absence:commit_signing")
	assert.True(t, hasAbsenceLicense, "repo with no license should have absence:license")
}

// --- Response Size Limit (Issue #28) ---

// TestSecurity_LargeResponseRejected verifies that responses exceeding
// the size limit are rejected rather than consuming unbounded memory.
func TestSecurity_LargeResponseRejected(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Write a response larger than any reasonable GitHub API response.
		// We use 11MB to exceed a 10MB limit.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"data":"`))
		buf := make([]byte, 1024)
		for i := range buf {
			buf[i] = 'x'
		}
		for i := 0; i < 11*1024; i++ { // 11MB of 'x'
			w.Write(buf)
		}
		w.Write([]byte(`"}`))
	})

	server := httptest.NewServer(handler)
	defer server.Close()

	client := &Client{
		httpClient: server.Client(),
		token:      "",
		baseURL:    server.URL,
	}

	var result map[string]any
	err := client.get(context.Background(), "/test", &result)
	assert.Error(t, err, "should reject response exceeding size limit")
}

// --- SSRF Prevention (Issue #27) ---

// TestSecurity_ParseRepoURL_RejectsPathTraversal verifies that crafted
// owner/repo names containing path traversal characters are rejected.
func TestSecurity_ParseRepoURL_RejectsPathTraversal(t *testing.T) {
	malicious := []struct {
		name  string
		input string
	}{
		{"dot-dot in owner", "../../admin/repo"},
		{"dot-dot in repo", "owner/../../etc/passwd"},
		{"query injection", "owner/repo?admin=true"},
		{"fragment injection", "owner/repo#/admin"},
		{"encoded slash", "owner%2F..%2Fadmin/repo"},
		{"newline in owner", "owner\n/repo"},
		{"space in owner", "owner /repo"},
		{"semicolon", "owner;rm -rf/repo"},
	}

	for _, tt := range malicious {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := ParseRepoURL(tt.input)
			assert.Error(t, err, "should reject malicious input: %s", tt.input)
		})
	}
}

// TestSecurity_ParseRepoURL_AcceptsValid verifies that legitimate
// names with dots, hyphens, and underscores are accepted.
func TestSecurity_ParseRepoURL_AcceptsValid(t *testing.T) {
	valid := []struct {
		name      string
		input     string
		wantOwner string
		wantRepo  string
	}{
		{"simple", "owner/repo", "owner", "repo"},
		{"with dots", "org.name/my.repo", "org.name", "my.repo"},
		{"with hyphens", "my-org/my-repo", "my-org", "my-repo"},
		{"with underscores", "my_org/my_repo", "my_org", "my_repo"},
		{"with numbers", "user123/repo456", "user123", "repo456"},
		{"full URL", "https://github.com/alecthomas/kong", "alecthomas", "kong"},
	}

	for _, tt := range valid {
		t.Run(tt.name, func(t *testing.T) {
			owner, repo, err := ParseRepoURL(tt.input)
			require.NoError(t, err)
			assert.Equal(t, tt.wantOwner, owner)
			assert.Equal(t, tt.wantRepo, repo)
		})
	}
}

// --- Token Leak Prevention (Issue #29) ---

// TestSecurity_TokenNotInCollectionErrorError verifies that
// CollectionError.Error() doesn't leak the token either.
func TestSecurity_TokenNotInCollectionErrorError(t *testing.T) {
	secretToken := "ghp_FailureErrorLeakTest1234567890"

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(repo{
			Name: "repo", Owner: repoOwner{Login: "owner", Type: "User"},
		})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"message":"error with token ` + secretToken + `"}`))
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	client := &Client{
		httpClient: server.Client(),
		token:      secretToken,
		baseURL:    server.URL,
	}
	collector := NewCollectorWithClient(client)

	entity := &profile.Entity{
		ID: "test-entity", Type: profile.EntityPackage, ShortName: "owner/repo",
	}

	// Collect will succeed partially. Check that the sanitized reason
	// in any failure path doesn't leak.
	result, err := collector.Collect(context.Background(), entity)
	require.NoError(t, err)

	// Walk every reachable field of the CollectionResult — signals,
	// absence signals, failures, plus JSON catch-all. See #96 for
	// why this is the right shape.
	assertNoTokenInResult(t, result, secretToken)

	// Also verify CollectionError.Error() rendering, which is the
	// specific scenario this test name covers (the helper above
	// covers the field-level check; this asserts the formatted
	// Error() string also doesn't leak).
	for _, failure := range result.Failures {
		assert.NotContains(t, failure.Error(), secretToken,
			"CollectionError.Error() contains the secret token — TOKEN LEAK")
	}
}

// TestSecurity_TokenNotInErrorMessages verifies that RateLimitError
// messages don't contain the token.
func TestSecurity_TokenNotInRateLimitError(t *testing.T) {
	secretToken := "ghp_AnotherSecret1234567890"

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(repo{
			Name:  "repo",
			Owner: repoOwner{Login: "owner", Type: "User"},
		})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Reset", "1712700000")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message":"API rate limit exceeded for token ` + secretToken + `"}`))
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	client := &Client{
		httpClient: server.Client(),
		token:      secretToken,
		baseURL:    server.URL,
	}
	collector := NewCollectorWithClient(client)

	entity := &profile.Entity{
		ID:        "test-entity",
		Type:      profile.EntityPackage,
		ShortName: "owner/repo",
	}

	result, err := collector.Collect(context.Background(), entity)
	require.NoError(t, err)

	// Walk every reachable field of the CollectionResult — see #96.
	assertNoTokenInResult(t, result, secretToken)
}

// TestSecurity_CheckRedirect_RefusesSchemeDowngrade verifies that the
// http.Client redirect policy refuses to follow a redirect from HTTPS
// to a non-HTTPS URL. Issue #89: the previous policy only stripped
// the Authorization header on cross-origin redirects (different host)
// — it had no scheme check at all. A 302 from https://api.github.com
// to http://api.github.com (same host, scheme downgrade) would have
// been followed with the Authorization header still attached, leaking
// the bearer token over plaintext to any network observer.
//
// This is a unit test of checkRedirect directly. The corresponding
// integration test below (TestSecurity_GitHubClient_*) verifies the
// end-to-end behavior through net/http's redirect machinery.
//
// Each row crafts a request whose URL.Scheme is something other than
// "https" and asserts that checkRedirect returns an error. Because
// returning an error from CheckRedirect aborts the redirect chain
// before any further request is sent, no token can leak.
func TestSecurity_CheckRedirect_RefusesSchemeDowngrade(t *testing.T) {
	httpsVia := mustParseURL(t, "https://api.github.com/repos/x/y")
	via := []*http.Request{{URL: httpsVia, Header: http.Header{"Authorization": []string{"Bearer secret-token"}}}}

	tests := []struct {
		name   string
		target string
	}{
		{"same host downgrade to http", "http://api.github.com/redirected"},
		{"different host downgrade to http", "http://attacker.example/exfiltrate"},
		{"file scheme", "file:///etc/passwd"},
		{"javascript scheme", "javascript:alert(1)"},
		{"empty scheme", "//api.github.com/redirected"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			next := &http.Request{
				URL:    mustParseURL(t, tc.target),
				Header: http.Header{"Authorization": []string{"Bearer secret-token"}},
			}
			err := checkRedirect(next, via)
			require.Error(t, err, "checkRedirect must reject non-HTTPS redirect target %q", tc.target)
		})
	}
}

// TestSecurity_CheckRedirect_AllowsLegitimate verifies the negative
// case: legitimate redirects (same host HTTPS → HTTPS, or cross-origin
// HTTPS → HTTPS with auth stripped) must continue to work after the
// scheme-downgrade fix. This locks in that the fix didn't accidentally
// break the existing redirect-following behavior.
func TestSecurity_CheckRedirect_AllowsLegitimate(t *testing.T) {
	httpsVia := mustParseURL(t, "https://api.github.com/repos/x/y")
	via := []*http.Request{{URL: httpsVia, Header: http.Header{"Authorization": []string{"Bearer secret-token"}}}}

	t.Run("same host https redirect keeps auth", func(t *testing.T) {
		next := &http.Request{
			URL:    mustParseURL(t, "https://api.github.com/repos/x/y/contents/go.mod"),
			Header: http.Header{"Authorization": []string{"Bearer secret-token"}},
		}
		err := checkRedirect(next, via)
		require.NoError(t, err)
		assert.Equal(t, "Bearer secret-token", next.Header.Get("Authorization"),
			"same-host redirect must keep auth header")
	})

	t.Run("cross-origin https redirect strips auth", func(t *testing.T) {
		next := &http.Request{
			URL:    mustParseURL(t, "https://codeload.github.com/x/y/tarball/main"),
			Header: http.Header{"Authorization": []string{"Bearer secret-token"}},
		}
		err := checkRedirect(next, via)
		require.NoError(t, err)
		assert.Empty(t, next.Header.Get("Authorization"),
			"cross-origin redirect must strip auth header")
	})
}

// TestSecurity_GitHubClient_RefusesHTTPSToHTTPRedirect is the end-to-end
// integration counterpart to TestSecurity_CheckRedirect_RefusesSchemeDowngrade.
// It uses real httptest TLS and HTTP servers to verify that a Client
// going through net/http's redirect machinery refuses to follow an
// HTTPS→HTTP redirect AND that the bearer token never reaches the
// HTTP target server.
func TestSecurity_GitHubClient_RefusesHTTPSToHTTPRedirect(t *testing.T) {
	const secretToken = "ghp_secret_token_that_must_not_leak"

	// HTTP target server (the redirect destination). If the redirect
	// were followed, this server would receive the request and we'd
	// see the Authorization header. With the fix in place, the redirect
	// is refused and this handler is never called.
	var capturedAuth string
	var hits int
	httpTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		capturedAuth = r.Header.Get("Authorization")
		w.WriteHeader(200)
	}))
	defer httpTarget.Close()

	// HTTPS source server. Returns a 302 redirecting to the HTTP target
	// (same scheme would be `https`, the bug allows scheme downgrade).
	httpsSource := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, httpTarget.URL+r.URL.Path, http.StatusFound)
	}))
	defer httpsSource.Close()

	// Construct a Client that uses the production checkRedirect AND
	// trusts the test TLS server's self-signed cert (via httpsSource.Client()
	// which returns a pre-configured *http.Client).
	// We have to copy the production CheckRedirect into this httpClient
	// because httpsSource.Client() doesn't set one — it returns a default
	// client that follows redirects unconditionally.
	testHTTPClient := httpsSource.Client()
	testHTTPClient.CheckRedirect = checkRedirect

	client := &Client{
		httpClient: testHTTPClient,
		token:      secretToken,
		baseURL:    httpsSource.URL,
	}

	// Make any request through the client. The request goes to httpsSource,
	// which redirects to httpTarget. With the fix, the client returns an
	// error from the redirect handler and never reaches httpTarget.
	var ignored map[string]any
	err := client.get(context.Background(), "/repos/owner/repo", &ignored)
	require.Error(t, err, "client must return an error when the redirect chain hits a non-HTTPS URL")

	// THE CRITICAL ASSERTION: the HTTP target server was never reached.
	// The redirect was refused before any plaintext request was sent.
	assert.Zero(t, hits, "HTTP target server must not have been hit — redirect should have been refused")
	assert.Empty(t, capturedAuth, "Authorization header must never reach an HTTP URL — token leak otherwise")
}

// mustParseURL is a small test helper that fails the test on parse error.
func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err, "url.Parse(%q)", raw)
	return u
}

// TestSecurity_ClientErrorDoesNotLeakBody verifies that error responses
// from the GitHub API never echo the response body content into the
// returned error string. Issue #93: client.get and client.getWithLinkHeader
// embedded up to 4096 bytes of attacker-influenceable body into
// `fmt.Errorf("GitHub API error %d: %s", status, body)`. The error then
// propagated through collector.Collect → analyze.go → main.go's
// fmt.Fprintln(os.Stderr, err), landing in CI logs, SIEM ingest pipelines,
// and LLM agent transcripts that treat error output as ground truth.
//
// sanitizeErrorForStorage existed and was called on the absence-signal
// path, but NOT on the GetRepo abort-collection path that fails fastest.
// The cleanest fix is to drop the body at the source — the status code
// is the only useful structured signal for the caller; the body is
// attacker-controlled bytes that should never reach stderr.
//
// The test exercises both code paths:
//
//   - GetRepo → client.get (line 192) → error embedded body
//   - GetContributors → client.getWithLinkHeader (line 242) → same bug
//
// And it sweeps the relevant non-OK status codes that hit the buggy
// path. 403/429 are excluded because they return RateLimitError without
// reading the body. 404 is excluded because it returns "not found: <path>"
// without reading the body either. The remaining non-OK statuses
// (401, 422, 500, 502, 503) all hit the body-embedding path.
func TestSecurity_ClientErrorDoesNotLeakBody(t *testing.T) {
	const sentinel = "LEAKED-INTERNAL-IP-10.0.0.42-DO-NOT-DISCLOSE"
	body := []byte(`{"error":"server error","debug":"` + sentinel + `","internal_email":"oncall@example.internal"}`)

	statuses := []int{401, 422, 500, 502, 503}

	for _, status := range statuses {
		t.Run("status_"+strconv.Itoa(status), func(t *testing.T) {
			mux := http.NewServeMux()
			handler := func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				w.Write(body)
			}
			mux.HandleFunc("/repos/owner/repo", handler)
			mux.HandleFunc("/repos/owner/repo/contributors", handler)

			server := httptest.NewServer(mux)
			defer server.Close()

			client := &Client{
				httpClient: server.Client(),
				token:      "test-token",
				baseURL:    server.URL,
			}

			t.Run("GetRepo (client.get path)", func(t *testing.T) {
				_, err := client.GetRepo(context.Background(), "owner", "repo")
				require.Error(t, err)
				assert.NotContains(t, err.Error(), sentinel,
					"GetRepo error must not leak response body content (%d)", status)
				assert.NotContains(t, err.Error(), "oncall@example.internal",
					"GetRepo error must not leak any body content (%d)", status)
			})

			t.Run("GetContributors (client.getWithLinkHeader path)", func(t *testing.T) {
				_, err := client.GetContributors(context.Background(), "owner", "repo")
				require.Error(t, err)
				assert.NotContains(t, err.Error(), sentinel,
					"GetContributors error must not leak response body content (%d)", status)
				assert.NotContains(t, err.Error(), "oncall@example.internal",
					"GetContributors error must not leak any body content (%d)", status)
			})
		})
	}
}

// TestSecurity_ParseGoModDeps_RejectsOversizedInput verifies that
// parseGoModDeps refuses to process go.mod content beyond
// maxGoModSize. Issue #108: the previous implementation called
// strings.Split(content, "\n") with no size check, while the upstream
// GetFileRaw could fetch up to 10MB. An attacker controlling a repo's
// go.mod could return 10MB of newlines and force signatory to allocate
// ~10M empty strings, producing memory pressure and GC churn.
//
// Real-world go.mod files are well under 64KB (Kubernetes is around
// 50KB; most projects are under 10KB), so the cap is generous slack.
// Inputs above the cap are rejected with an explicit error so the
// caller can record an absence rather than silently allocating.
func TestSecurity_ParseGoModDeps_RejectsOversizedInput(t *testing.T) {
	tests := []struct {
		name      string
		size      int
		wantError bool
	}{
		{"empty", 0, false},
		{"small valid", 1024, false},
		{"at limit", maxGoModSize, false},
		{"one byte over limit", maxGoModSize + 1, true},
		{"1MB of newlines (the documented attack)", 1 << 20, true},
		{"10MB max-fetch DoS", 10 << 20, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Build a content blob of the requested size. Use newlines
			// because they hit the worst case for strings.Split — every
			// byte produces a separate empty string in the result.
			content := strings.Repeat("\n", tc.size)
			_, err := parseGoModDeps(content)
			if tc.wantError {
				require.Error(t, err, "parseGoModDeps must reject oversized input")
				assert.Contains(t, err.Error(), "exceeds maximum",
					"error must explain the size limit was exceeded")
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// TestSecurity_CollectGoDeps_AbsenceOnOversizedGoMod is the integration
// counterpart that verifies the collector turns parseGoModDeps's
// oversized-input error into an absence signal (not retryable) rather
// than failing the whole collection or recording bogus dep counts.
//
// Uses an httptest server that returns a giant go.mod (1MB of newlines,
// base64-encoded inside the GitHub contents API JSON envelope).
func TestSecurity_CollectGoDeps_AbsenceOnOversizedGoMod(t *testing.T) {
	mux := http.NewServeMux()

	mux.HandleFunc("/repos/owner/repo", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(repo{
			Name: "repo", Owner: repoOwner{Login: "owner", Type: "User"},
			StargazersCount: 100,
		})
	})
	// Stub the other endpoints so the rest of Collect() doesn't error.
	mux.HandleFunc("/repos/owner/repo/contributors", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]contributor{{Login: "owner", Contributions: 10}})
	})
	mux.HandleFunc("/repos/owner/repo/commits", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]commit{{
			Commit: commitData{Author: commitPerson{Date: time.Now()}, Verification: verification{Verified: true}},
		}})
	})
	mux.HandleFunc("/repos/owner/repo/tags", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]tag{})
	})
	mux.HandleFunc("/users/owner", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(user{Login: "owner", CreatedAt: time.Now()})
	})
	// /search/code handler removed: see comment in the earlier
	// occurrence in this file.
	// Default for any other path: 404 (CI checks etc. — they handle 404
	// as "not present" without erroring).
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	// The go.mod fetch returns a 1MB blob of newlines, base64-encoded
	// to match GitHub's contents API response shape.
	mux.HandleFunc("/repos/owner/repo/contents/go.mod", func(w http.ResponseWriter, r *http.Request) {
		bigContent := strings.Repeat("\n", 1<<20) // 1MB
		encoded := base64.StdEncoding.EncodeToString([]byte(bigContent))
		json.NewEncoder(w).Encode(fileContent{
			Content:  encoded,
			Encoding: "base64",
		})
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	client := &Client{httpClient: server.Client(), token: "test", baseURL: server.URL}
	collector := NewCollectorWithClient(client)

	entity := &profile.Entity{
		ID:        "test-entity",
		Type:      profile.EntityProject,
		ShortName: "owner/repo",
	}

	result, err := collector.Collect(context.Background(), entity)
	require.NoError(t, err, "Collect must not fail when go.mod is oversized — it's a single signal absence, not a fatal error")

	// Find the go_dependencies signal in the result. It should be an
	// absence signal with the size-limit reason and not retryable.
	var goDepsSignal *profile.Signal
	for i, sig := range result.Signals() {
		if sig.Type == "absence:go_dependencies" {
			s := result.Signals()[i]
			goDepsSignal = &s
			break
		}
	}
	require.NotNil(t, goDepsSignal, "go_dependencies must be recorded as an absence when the file is oversized")

	// The absence reason must indicate the size limit, not echo the
	// raw parser error (which would leak the byte counts).
	var absenceData map[string]any
	require.NoError(t, json.Unmarshal(goDepsSignal.Value, &absenceData))
	assert.Equal(t, "go.mod too large to parse safely", absenceData["reason"],
		"oversized go.mod must produce a structured absence reason, not the raw parser error")
	assert.Equal(t, false, absenceData["retryable"],
		"oversized go.mod is not retryable — the file won't shrink on retry")
}

// TestSecurity_ClientErrorPreservesStatusCode verifies the negative
// invariant of the #93 fix: even after dropping the body, the error
// must still tell the caller WHICH status code occurred so isRetryable
// and other classifiers can do their job, and so a human reading the
// stderr knows whether they're looking at a 5xx (server problem) or a
// 4xx (client problem). If this test breaks, the body-stripping fix
// went too far and removed useful structured information.
// --- sanitizeError wiring through get() and getWithLinkHeader() ------------
//
// The sanitizeError function (unit-tested in client_test.go) only
// protects callers if every error-return path in get() and
// getWithLinkHeader() actually applies it. The named-return + defer
// pattern is the wiring; these tests prove the wiring is in place
// by injecting a custom http.RoundTripper that returns a transport-
// layer error containing the bearer token, then asserting the
// returned error is redacted.
//
// Why a custom RoundTripper, not httptest.NewServer. The threat
// model is *transport-layer* error rendering — the failure happens
// inside http.Client.Do before any HTTP exchange. A real server
// can't reproduce this; the request never reaches it. The
// RoundTripper returns the error directly, mimicking what a
// hostile/buggy proxy middleware would produce.
//
// Why two tests, one per private method. get() and
// getWithLinkHeader() are independent error-return surfaces.
// Sanitization on get() doesn't imply sanitization on
// getWithLinkHeader(); a future contributor could add the defer to
// one and forget the other. Each method gets its own wiring test
// via the public method that invokes it (GetRepo for get,
// GetTotalCommitCount for getWithLinkHeader).

// tokenLeakingRoundTripper is a synthetic transport that returns
// an error containing the request's Authorization header in its
// rendered string. Mimics the threat model: a hostile/buggy
// transport or proxy middleware that interpolates request detail
// into transport error messages.
type tokenLeakingRoundTripper struct{}

func (t *tokenLeakingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("synthetic transport failure: header=%s", req.Header.Get("Authorization"))
}

// TestSecurity_TokenSanitized_FromGet proves the named-return +
// defer wiring in client.get() actually applies sanitizeError to
// transport-layer errors before they reach the caller.
//
// Calls GetRepo (which uses get internally) with a Client whose
// http.Transport leaks the Authorization header into its error
// string. Without the defer in get(), the bearer token surfaces in
// the returned error. With the defer, the token is replaced by
// [REDACTED-TOKEN].
//
// Revert proof: remove the `defer func() { err = sanitizeError(err,
// c.token) }()` line from client.get(); this test fails because the
// returned err.Error() contains the raw token.
func TestSecurity_TokenSanitized_FromGet(t *testing.T) {
	t.Parallel()
	const token = "ghp_pretendThisIsARealGitHubTokenAaa1234567890"

	client := &Client{
		httpClient: &http.Client{Transport: &tokenLeakingRoundTripper{}},
		token:      token,
		baseURL:    "https://api.github.invalid",
	}

	_, err := client.GetRepo(context.Background(), "owner", "repo")
	require.Error(t, err, "leaking transport must produce an error")

	assert.NotContains(t, err.Error(), token,
		"TOKEN LEAK from get(): named-return + defer wiring missing or broken")
	assert.Contains(t, err.Error(), "[REDACTED-TOKEN]",
		"sanitization marker must appear so operators can grep for redaction events")
}

// TestSecurity_TokenSanitized_FromGetWithLinkHeader is the
// independent wiring proof for getWithLinkHeader. The two paginated
// and non-paginated GET helpers are separate error-return surfaces;
// both need the defer.
//
// Calls GetTotalCommitCount (which uses getWithLinkHeader) with the
// same leaking transport. Same shape, separate wiring.
//
// Revert proof: remove the `defer func() { err = sanitizeError(err,
// c.token) }()` line from client.getWithLinkHeader; this test fails.
func TestSecurity_TokenSanitized_FromGetWithLinkHeader(t *testing.T) {
	t.Parallel()
	const token = "ghp_pretendThisIsARealGitHubTokenAaa1234567890"

	client := &Client{
		httpClient: &http.Client{Transport: &tokenLeakingRoundTripper{}},
		token:      token,
		baseURL:    "https://api.github.invalid",
	}

	_, err := client.GetTotalCommitCount(context.Background(), "owner", "repo")
	require.Error(t, err, "leaking transport must produce an error")

	assert.NotContains(t, err.Error(), token,
		"TOKEN LEAK from getWithLinkHeader(): named-return + defer wiring missing or broken")
	assert.Contains(t, err.Error(), "[REDACTED-TOKEN]",
		"sanitization marker must appear so operators can grep for redaction events")
}

func TestSecurity_ClientErrorPreservesStatusCode(t *testing.T) {
	statuses := []int{401, 422, 500, 502, 503}

	for _, status := range statuses {
		t.Run("status_"+strconv.Itoa(status), func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/repos/owner/repo", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
			})

			server := httptest.NewServer(mux)
			defer server.Close()

			client := &Client{
				httpClient: server.Client(),
				token:      "test-token",
				baseURL:    server.URL,
			}

			_, err := client.GetRepo(context.Background(), "owner", "repo")
			require.Error(t, err)
			assert.Contains(t, err.Error(), strconv.Itoa(status),
				"error must include the status code so callers can classify")
		})
	}
}
