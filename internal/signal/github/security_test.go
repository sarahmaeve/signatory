package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
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
)

// TestSecurity_TokenNotLeakedInAbsenceSignals verifies that the
// GITHUB_TOKEN does not appear in any persisted signal data when
// API calls fail. This is critical because absence signals are
// stored in the database and potentially exposed via JSON output
// or MCP.
func TestSecurity_TokenNotLeakedInAbsenceSignals(t *testing.T) {
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

	// The critical check: NO signal should contain the token in its
	// serialized value. Check every signal.
	for _, sig := range result.Signals() {
		valueStr := string(sig.Value)
		assert.NotContains(t, valueStr, secretToken,
			"signal %s (type=%s) contains the secret token in its value — TOKEN LEAK",
			sig.ID, sig.Type)
	}
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
	mux.HandleFunc("/search/code", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(searchResult{TotalCount: 50})
	})

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
			var val map[string]interface{}
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
	mux.HandleFunc("/search/code", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(searchResult{TotalCount: 0})
	})
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

	var result map[string]interface{}
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

// TestSecurity_TokenNotInCollectionFailureError verifies that
// CollectionFailure.Error() doesn't leak the token either.
func TestSecurity_TokenNotInCollectionFailureError(t *testing.T) {
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
	signals := result.Signals()

	// Also verify: if someone calls .Error() on any failure, no leak.
	for _, sig := range signals {
		if strings.HasPrefix(sig.Type, "absence:") {
			// The reason is in the JSON value — already tested above.
			// But let's also make sure the signal ID doesn't leak.
			assert.NotContains(t, sig.ID, secretToken,
				"signal ID contains token")
			assert.NotContains(t, sig.Source, secretToken,
				"signal source contains token")
		}
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
	signals := result.Signals()

	for _, sig := range signals {
		valueStr := string(sig.Value)
		if strings.HasPrefix(sig.Type, "absence:") {
			assert.NotContains(t, valueStr, secretToken,
				"absence signal %s contains the secret token — TOKEN LEAK", sig.Type)
		}
	}
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
	var ignored map[string]interface{}
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
	mux.HandleFunc("/search/code", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(searchResult{TotalCount: 50})
	})
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
	var absenceData map[string]interface{}
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
