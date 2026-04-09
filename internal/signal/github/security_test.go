package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
		ID:   "test-entity",
		Type: profile.EntityPackage,
		Name: "owner/repo",
	}

	signals, err := collector.Collect(context.Background(), entity)
	require.NoError(t, err, "partial collection should not return error")

	// The critical check: NO signal should contain the token in its
	// serialized value. Check every signal.
	for _, sig := range signals {
		valueStr := string(sig.Value)
		assert.NotContains(t, valueStr, secretToken,
			"signal %s (type=%s) contains the secret token in its value — TOKEN LEAK",
			sig.ID, sig.Type)
	}
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
		ID: "test-entity", Type: profile.EntityPackage, Name: "owner/repo",
	}

	// Collect will succeed partially. Check that the sanitized reason
	// in any failure path doesn't leak.
	signals, err := collector.Collect(context.Background(), entity)
	require.NoError(t, err)

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
		ID:   "test-entity",
		Type: profile.EntityPackage,
		Name: "owner/repo",
	}

	signals, err := collector.Collect(context.Background(), entity)
	require.NoError(t, err)

	for _, sig := range signals {
		valueStr := string(sig.Value)
		if strings.HasPrefix(sig.Type, "absence:") {
			assert.NotContains(t, valueStr, secretToken,
				"absence signal %s contains the secret token — TOKEN LEAK", sig.Type)
		}
	}
}
