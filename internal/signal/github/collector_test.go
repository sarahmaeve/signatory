package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sarahmaeve/signatory/internal/profile"
	"github.com/sarahmaeve/signatory/internal/signal"
)

// Compile-time interface check.
var _ signal.Collector = (*Collector)(nil)

// newTestCollector creates a collector backed by a mock HTTP server.
func newTestCollector(t *testing.T, handler http.Handler) *Collector {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	client := &Client{
		httpClient: server.Client(),
		token:      "test-token",
		baseURL:    server.URL,
	}
	return NewCollectorWithClient(client)
}

// mockGitHubAPI returns a handler that serves realistic GitHub API responses.
func mockGitHubAPI() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/repos/alecthomas/kong", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(repo{
			Name:            "kong",
			FullName:        "alecthomas/kong",
			Description:     "Kong is a command-line parser for Go",
			Owner:           repoOwner{Login: "alecthomas", Type: "User"},
			CreatedAt:       time.Date(2018, 4, 10, 6, 50, 0, 0, time.UTC),
			UpdatedAt:       time.Date(2026, 4, 9, 12, 0, 0, 0, time.UTC),
			PushedAt:        time.Date(2026, 4, 9, 17, 19, 0, 0, time.UTC),
			StargazersCount: 3023,
			ForksCount:      161,
			OpenIssuesCount: 41,
			License:         &license{SPDXID: "MIT"},
		})
	})

	mux.HandleFunc("/repos/alecthomas/kong/contributors", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]contributor{
			{Login: "alecthomas", Contributions: 271},
			{Login: "renovate[bot]", Contributions: 37},
			{Login: "gak", Contributions: 14},
		})
	})

	mux.HandleFunc("/repos/alecthomas/kong/commits", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("per_page") == "1" {
			w.Header().Set("Link", `<https://api.github.com/repos/alecthomas/kong/commits?per_page=1&page=2>; rel="next", <https://api.github.com/repos/alecthomas/kong/commits?per_page=1&page=467>; rel="last"`)
			json.NewEncoder(w).Encode([]commit{})
			return
		}
		json.NewEncoder(w).Encode([]commit{
			{
				SHA: "abc123",
				Commit: commitData{
					Author:       commitPerson{Name: "Alec Thomas", Date: time.Date(2026, 4, 1, 20, 57, 0, 0, time.UTC)},
					Message:      "fix: Allow the root node to define Help()",
					Verification: verification{Verified: true},
				},
			},
			{
				SHA: "def456",
				Commit: commitData{
					Author:       commitPerson{Name: "Contributor", Date: time.Date(2026, 3, 31, 9, 17, 0, 0, time.UTC)},
					Message:      "Add a generic BindFor",
					Verification: verification{Verified: true},
				},
			},
			{
				SHA: "ghi789",
				Commit: commitData{
					Author:       commitPerson{Name: "Another Dev", Date: time.Date(2026, 2, 6, 23, 6, 0, 0, time.UTC)},
					Message:      "feat: signature defaults",
					Verification: verification{Verified: false},
				},
			},
		})
	})

	mux.HandleFunc("/repos/alecthomas/kong/tags", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]tag{
			{Name: "v1.15.0"},
			{Name: "v1.14.0"},
			{Name: "v1.13.0"},
		})
	})

	mux.HandleFunc("/users/alecthomas", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(user{
			Login:       "alecthomas",
			Name:        "Alec Thomas",
			CreatedAt:   time.Date(2008, 12, 20, 9, 7, 0, 0, time.UTC),
			PublicRepos: 175,
			Followers:   1419,
			Type:        "User",
		})
	})

	mux.HandleFunc("/search/code", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(searchResult{TotalCount: 2008})
	})

	mux.HandleFunc("/repos/alecthomas/kong/contents/.github/workflows", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]repoContent{
			{Name: "ci.yml", Type: "file", Path: ".github/workflows/ci.yml"},
		})
	})

	mux.HandleFunc("/repos/alecthomas/kong/contents/renovate.json", func(w http.ResponseWriter, r *http.Request) {
		// Return a file content response.
		json.NewEncoder(w).Encode(fileContent{
			Content:  "e30K", // base64 for "{}\n"
			Encoding: "base64",
		})
	})

	mux.HandleFunc("/repos/alecthomas/kong/contents/go.mod", func(w http.ResponseWriter, r *http.Request) {
		gomod := `module github.com/alecthomas/kong

require (
	github.com/alecthomas/assert/v2 v2.11.0
	github.com/alecthomas/repr v0.5.2
)

require github.com/hexops/gotextdiff v1.0.3 // indirect

go 1.20
`
		encoded := base64.StdEncoding.EncodeToString([]byte(gomod))
		json.NewEncoder(w).Encode(fileContent{
			Content:  encoded,
			Encoding: "base64",
		})
	})

	return mux
}

func TestCollector_Name(t *testing.T) {
	c := NewCollector()
	assert.Equal(t, "github", c.Name())
}

func TestCollector_Collect(t *testing.T) {
	c := newTestCollector(t, mockGitHubAPI())
	ctx := context.Background()

	entity := &profile.Entity{
		ID:        "pkg:go:github.com/alecthomas/kong",
		Type:      profile.EntityPackage,
		ShortName: "alecthomas/kong",
		URL:       "https://github.com/alecthomas/kong",
	}

	result, err := c.Collect(ctx, entity)
	require.NoError(t, err)
	signals := result.Signals()
	assert.NotEmpty(t, signals)

	// Build a map for easier assertions.
	byType := make(map[string]profile.Signal)
	for _, s := range signals {
		byType[s.Type] = s
	}

	// Verify signal types are present.
	expectedTypes := []string{
		"last_push", "repo_age", "stars", "forks", "open_issues",
		"archived", "owner_type", "license", "contributors",
		"last_commit", "commit_signing", "total_commits", "tags",
		"owner_profile", "adoption", "ci_cd", "go_dependencies",
	}
	for _, st := range expectedTypes {
		assert.Contains(t, byType, st, "missing signal type: %s", st)
	}
}

func TestCollector_SignalValues(t *testing.T) {
	c := newTestCollector(t, mockGitHubAPI())
	ctx := context.Background()

	entity := &profile.Entity{
		ID:        "pkg:go:github.com/alecthomas/kong",
		Type:      profile.EntityPackage,
		ShortName: "alecthomas/kong",
	}

	result, err := c.Collect(ctx, entity)
	require.NoError(t, err)
	signals := result.Signals()

	byType := make(map[string]profile.Signal)
	for _, s := range signals {
		byType[s.Type] = s
	}

	// Stars value.
	var starsVal map[string]any
	require.NoError(t, json.Unmarshal(byType["stars"].Value, &starsVal))
	assert.Equal(t, float64(3023), starsVal["count"])

	// Commit signing ratio.
	var signingVal map[string]any
	require.NoError(t, json.Unmarshal(byType["commit_signing"].Value, &signingVal))
	assert.Equal(t, float64(2), signingVal["signed_count"])
	assert.Equal(t, float64(3), signingVal["total_count"])

	// Total commits.
	var totalVal map[string]any
	require.NoError(t, json.Unmarshal(byType["total_commits"].Value, &totalVal))
	assert.Equal(t, float64(467), totalVal["count"])

	// Owner profile.
	var ownerVal map[string]any
	require.NoError(t, json.Unmarshal(byType["owner_profile"].Value, &ownerVal))
	assert.Equal(t, "alecthomas", ownerVal["login"])
	assert.Equal(t, float64(1419), ownerVal["followers"])
	assert.Equal(t, float64(175), ownerVal["public_repos"])

	// Tags.
	var tagsVal map[string]any
	require.NoError(t, json.Unmarshal(byType["tags"].Value, &tagsVal))
	assert.Equal(t, float64(3), tagsVal["count"])

	// Contributors.
	var contribVal map[string]any
	require.NoError(t, json.Unmarshal(byType["contributors"].Value, &contribVal))
	assert.Equal(t, float64(3), contribVal["count"])

	// Adoption and refs-to-stars ratio.
	var adoptionVal map[string]any
	require.NoError(t, json.Unmarshal(byType["adoption"].Value, &adoptionVal))
	assert.Equal(t, float64(2008), adoptionVal["go_mod_refs"])
	assert.Equal(t, float64(3023), adoptionVal["stars"])
	assert.InDelta(t, 0.66, adoptionVal["refs_to_stars"], 0.01)
	assert.Equal(t, "direct", adoptionVal["adoption_type"])

	// CI/CD presence.
	var ciVal map[string]any
	require.NoError(t, json.Unmarshal(byType["ci_cd"].Value, &ciVal))
	providers := ciVal["providers"].([]any)
	assert.Contains(t, providers, "github-actions")
	assert.Contains(t, providers, "renovate")

	// Go dependencies.
	var depsVal map[string]any
	require.NoError(t, json.Unmarshal(byType["go_dependencies"].Value, &depsVal))
	assert.Equal(t, float64(2), depsVal["direct_count"])
	assert.Equal(t, float64(1), depsVal["indirect_count"])
	assert.Equal(t, float64(3), depsVal["total_count"])
}

func TestCollector_SignalMetadata(t *testing.T) {
	c := newTestCollector(t, mockGitHubAPI())
	ctx := context.Background()

	entity := &profile.Entity{
		ID:        "test-entity",
		Type:      profile.EntityPackage,
		ShortName: "alecthomas/kong",
	}

	result, err := c.Collect(ctx, entity)
	require.NoError(t, err)
	signals := result.Signals()

	for _, s := range signals {
		assert.Equal(t, "test-entity", s.EntityID, "signal %s has wrong entity ID", s.Type)
		assert.Equal(t, "github", s.Source, "signal %s has wrong source", s.Type)
		assert.NotEmpty(t, s.ID, "signal %s has empty ID", s.Type)
		assert.NotEmpty(t, string(s.Group), "signal %s has empty group", s.Type)
		assert.NotEmpty(t, string(s.ForgeryResistance), "signal %s has empty forgery resistance", s.Type)
		assert.False(t, s.CollectedAt.IsZero(), "signal %s has zero collected time", s.Type)
		assert.True(t, s.ExpiresAt.After(s.CollectedAt), "signal %s expires before collection", s.Type)
	}
}

func TestCollector_ForgeryResistance(t *testing.T) {
	c := newTestCollector(t, mockGitHubAPI())
	ctx := context.Background()

	entity := &profile.Entity{
		ID:        "test-entity",
		Type:      profile.EntityPackage,
		ShortName: "alecthomas/kong",
	}

	result, err := c.Collect(ctx, entity)
	require.NoError(t, err)
	signals := result.Signals()

	byType := make(map[string]profile.Signal)
	for _, s := range signals {
		byType[s.Type] = s
	}

	// Verify forgery-resistant signals are classified correctly.
	assert.Equal(t, profile.ForgeryVeryHigh, byType["repo_age"].ForgeryResistance)
	assert.Equal(t, profile.ForgeryVeryHigh, byType["commit_signing"].ForgeryResistance)
	assert.Equal(t, profile.ForgeryVeryHigh, byType["owner_profile"].ForgeryResistance)
	assert.Equal(t, profile.ForgeryHigh, byType["contributors"].ForgeryResistance)
	assert.Equal(t, profile.ForgeryMediumDeclining, byType["stars"].ForgeryResistance)
}

func TestCollector_ContextCancellation(t *testing.T) {
	c := newTestCollector(t, mockGitHubAPI())

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately.

	entity := &profile.Entity{
		ID:        "test-entity",
		Type:      profile.EntityPackage,
		ShortName: "alecthomas/kong",
	}

	_, err := c.Collect(ctx, entity)
	assert.Error(t, err)
}

func TestCollector_RateLimitError(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Reset", "1712700000")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message":"API rate limit exceeded"}`))
	})

	c := newTestCollector(t, handler)
	ctx := context.Background()

	entity := &profile.Entity{
		ID:        "test-entity",
		Type:      profile.EntityPackage,
		ShortName: "alecthomas/kong",
	}

	_, err := c.Collect(ctx, entity)
	require.Error(t, err)

	var rateLimitErr *RateLimitError
	assert.ErrorAs(t, err, &rateLimitErr)
}

func TestCollector_PartialCollection(t *testing.T) {
	// Simulate: repo metadata works, but search API and contents API are rate-limited.
	mux := http.NewServeMux()

	// Repo works.
	mux.HandleFunc("/repos/owner/repo", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(repo{
			Name: "repo", FullName: "owner/repo",
			Owner:           repoOwner{Login: "owner", Type: "User"},
			CreatedAt:       time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
			PushedAt:        time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
			StargazersCount: 100,
			License:         &license{SPDXID: "MIT"},
		})
	})

	// Contributors works.
	mux.HandleFunc("/repos/owner/repo/contributors", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]contributor{{Login: "owner", Contributions: 50}})
	})

	// Commits works.
	mux.HandleFunc("/repos/owner/repo/commits", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]commit{{
			Commit: commitData{
				Author:       commitPerson{Date: time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)},
				Verification: verification{Verified: true},
			},
		}})
	})

	// Tags works.
	mux.HandleFunc("/repos/owner/repo/tags", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]tag{{Name: "v1.0.0"}})
	})

	// User works.
	mux.HandleFunc("/users/owner", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(user{Login: "owner", CreatedAt: time.Date(2015, 1, 1, 0, 0, 0, 0, time.UTC)})
	})

	// Search API rate-limited.
	mux.HandleFunc("/search/code", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Reset", "1712700000")
		w.WriteHeader(http.StatusForbidden)
	})

	// Contents API rate-limited (CI and go.mod checks).
	mux.HandleFunc("/repos/owner/repo/contents/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Reset", "1712700000")
		w.WriteHeader(http.StatusForbidden)
	})

	c := newTestCollector(t, mux)
	ctx := context.Background()

	entity := &profile.Entity{ID: "test", Type: profile.EntityPackage, ShortName: "owner/repo"}
	result, err := c.Collect(ctx, entity)
	require.NoError(t, err, "partial collection should not return an error")
	signals := result.Signals()

	// The result should report failures.
	assert.True(t, result.HasFailures(), "partial collection should report failures")

	// Should have collected some signals and some absences.
	var absences, collected int
	for _, s := range signals {
		if strings.HasPrefix(s.Type, "absence:") {
			absences++
			// Verify absence metadata.
			var val map[string]any
			json.Unmarshal(s.Value, &val)
			assert.Equal(t, true, val["absent"])
		} else {
			collected++
		}
	}

	assert.Greater(t, collected, 0, "should have some collected signals")
	assert.Greater(t, absences, 0, "should have some absence records")
}

func TestCollector_NotFoundError(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"Not Found"}`))
	})

	c := newTestCollector(t, handler)
	ctx := context.Background()

	entity := &profile.Entity{
		ID:        "test-entity",
		Type:      profile.EntityPackage,
		ShortName: "nonexistent/repo",
	}

	_, err := c.Collect(ctx, entity)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound,
		"Collect must wrap the github ErrNotFound sentinel so callers can discriminate")
}

func TestCollector_UsesEntityURLOverName(t *testing.T) {
	c := newTestCollector(t, mockGitHubAPI())
	ctx := context.Background()

	entity := &profile.Entity{
		ID:        "test-entity",
		Type:      profile.EntityPackage,
		ShortName: "some-npm-package",
		URL:       "https://github.com/alecthomas/kong",
	}

	result, err := c.Collect(ctx, entity)
	require.NoError(t, err)
	signals := result.Signals()
	assert.NotEmpty(t, signals)
}

func TestCollector_TemporalEraClassification(t *testing.T) {
	c := newTestCollector(t, mockGitHubAPI())
	ctx := context.Background()

	entity := &profile.Entity{
		ID:        "test-entity",
		Type:      profile.EntityPackage,
		ShortName: "alecthomas/kong",
	}

	result, err := c.Collect(ctx, entity)
	require.NoError(t, err)
	signals := result.Signals()

	byType := make(map[string]profile.Signal)
	for _, s := range signals {
		byType[s.Type] = s
	}

	// Last push and last commit should include era classification.
	var pushVal map[string]any
	require.NoError(t, json.Unmarshal(byType["last_push"].Value, &pushVal))
	assert.Equal(t, "modern-ai", pushVal["era"])

	var commitVal map[string]any
	require.NoError(t, json.Unmarshal(byType["last_commit"].Value, &commitVal))
	assert.Equal(t, "modern-ai", commitVal["era"])
}

func TestParseGoModDeps(t *testing.T) {
	tests := []struct {
		name          string
		content       string
		wantDirect    int
		wantIndirect  int
		wantDirectPkg []string
	}{
		{
			name: "standard go.mod",
			content: `module example.com/mymod

go 1.21

require (
	github.com/pkg/errors v0.9.1
	github.com/stretchr/testify v1.9.0
)

require (
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
)
`,
			wantDirect:    2,
			wantIndirect:  2,
			wantDirectPkg: []string{"github.com/pkg/errors", "github.com/stretchr/testify"},
		},
		{
			name: "mixed require block",
			content: `module example.com/mymod

require (
	github.com/direct/one v1.0.0
	github.com/indirect/one v1.0.0 // indirect
	github.com/direct/two v2.0.0
)
`,
			wantDirect:    2,
			wantIndirect:  1,
			wantDirectPkg: []string{"github.com/direct/one", "github.com/direct/two"},
		},
		{
			name:         "empty go.mod",
			content:      "module example.com/mymod\n\ngo 1.21\n",
			wantDirect:   0,
			wantIndirect: 0,
		},
		{
			name: "zero deps",
			content: `module example.com/mymod

go 1.21

require (
)
`,
			wantDirect:   0,
			wantIndirect: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			deps, err := parseGoModDeps(tt.content)
			require.NoError(t, err)
			assert.Equal(t, tt.wantDirect, deps.directCount)
			assert.Equal(t, tt.wantIndirect, deps.indirectCount)
			if tt.wantDirectPkg != nil {
				assert.Equal(t, tt.wantDirectPkg, deps.direct)
			}
		})
	}
}

// TestSanitizeErrorForStorage_SurfacesStatusCode pins the contract that
// when client.get fails with the sanitized "GitHub API returned status
// NNN" format (per #93's body-removal), the reason string surfaces the
// numeric status code rather than collapsing to a blanket "client
// error" / "server error" label. The original generic labels lost the
// signal a user needs to act on — 401 (auth), 403 (permissions), 404
// (gone), 422 (validation), 502/503 (transient) all merit different
// remediation, and "client error" tells the user nothing about which.
//
// The status code is the only attacker-uninfluenced field in the
// upstream error (#93 dropped the body); surfacing it is safe.
func TestSanitizeErrorForStorage_SurfacesStatusCode(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		errMsg   string
		wantCode string
	}{
		{"4xx - 401 unauthorized", "GitHub API returned status 401", "GITHUB_TOKEN"},
		{"4xx - 403 forbidden", "GitHub API returned status 403", "403"},
		{"4xx - 422 unprocessable", "GitHub API returned status 422", "422"},
		{"5xx - 500 internal", "GitHub API returned status 500", "500"},
		{"5xx - 502 bad gateway", "GitHub API returned status 502", "502"},
		{"5xx - 503 unavailable", "GitHub API returned status 503", "503"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := sanitizeErrorForStorage(testErr(tc.errMsg))
			assert.Contains(t, got, tc.wantCode,
				"sanitized reason for %q must include the status code %q so the user can distinguish it from other 4xx/5xx; got %q",
				tc.errMsg, tc.wantCode, got)
		})
	}
}

// TestSanitizeErrorForStorage_FallbackOnUnknownFormat verifies that
// errors NOT matching the "GitHub API returned status NNN" prefix still
// fall through to the existing generic labels — we don't want the
// status-extraction branch to swallow unrelated errors.
func TestSanitizeErrorForStorage_FallbackOnUnknownFormat(t *testing.T) {
	t.Parallel()

	got := sanitizeErrorForStorage(testErr("something else entirely"))
	assert.Equal(t, "collection failed", got,
		"errors that don't match any classifier branch must fall through to the generic 'collection failed' label")
}

// TestSanitizeErrorForStorage_ContextSentinels pins the contract that
// context.DeadlineExceeded and context.Canceled are detected via
// errors.Is — not via strings.Contains on the surface message. The
// previous implementation pattern-matched err.Error() for "context
// deadline exceeded" and "context canceled" literals, which is brittle
// against any error chain that wraps a sentinel under a Error() string
// that doesn't include the sentinel's standard text. errors.Is unwraps
// through fmt.Errorf %w chains AND through custom Unwrap() methods,
// catching both the naked-sentinel case and the hidden-message case.
//
// Project rule (CLAUDE.md): "use errors.Is / errors.As for all sentinel
// comparisons — never ==." String matching on err.Error() is the same
// anti-pattern by another name; it breaks the moment a wrapping layer
// changes its surface message.
func TestSanitizeErrorForStorage_ContextSentinels(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "naked DeadlineExceeded",
			err:  context.DeadlineExceeded,
			want: "request timeout",
		},
		{
			name: "naked Canceled",
			err:  context.Canceled,
			want: "request cancelled",
		},
		{
			name: "fmt.Errorf %w wrapping DeadlineExceeded",
			// fmt.Errorf with %w produces an error whose Unwrap()
			// returns the inner sentinel; errors.Is unwraps the
			// chain regardless of the surface message. This is the
			// canonical wrap pattern used by the github client
			// (see fmt.Errorf("execute request: %w", err) in
			// client.go).
			err:  fmt.Errorf("execute request: %w", context.DeadlineExceeded),
			want: "request timeout",
		},
		{
			name: "hidden-message wrapper of DeadlineExceeded",
			// hiddenCtxErr.Error() returns a message that does NOT
			// include "context deadline exceeded" — the only path
			// to classification is errors.Is unwrap. This case
			// fails under string matching; it is the load-bearing
			// regression test for the errors.Is fix.
			err:  &hiddenCtxErr{inner: context.DeadlineExceeded},
			want: "request timeout",
		},
		{
			name: "hidden-message wrapper of Canceled",
			err:  &hiddenCtxErr{inner: context.Canceled},
			want: "request cancelled",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := sanitizeErrorForStorage(tc.err)
			assert.Equalf(t, tc.want, got,
				"sanitized reason for %T (msg=%q) must classify as %q via errors.Is, got %q",
				tc.err, tc.err.Error(), tc.want, got)
		})
	}
}

// TestIsRetryable_ContextSentinels pins the contract that
// context.DeadlineExceeded — wrapped or naked — is detected via
// errors.Is and treated as retryable. context.Canceled is NOT
// retryable: a cancelled request was cancelled deliberately by the
// caller, not by transient network failure, and retrying it would
// fight the cancellation intent. Same anti-pattern as
// sanitizeErrorForStorage's previous implementation; same fix.
func TestIsRetryable_ContextSentinels(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "naked DeadlineExceeded is retryable",
			err:  context.DeadlineExceeded,
			want: true,
		},
		{
			name: "fmt.Errorf %w wrapping DeadlineExceeded is retryable",
			err:  fmt.Errorf("execute request: %w", context.DeadlineExceeded),
			want: true,
		},
		{
			name: "hidden-message wrapper of DeadlineExceeded is retryable",
			// The load-bearing case for the errors.Is fix: a
			// wrapper whose Error() does NOT contain "context
			// deadline exceeded" still classifies as retryable
			// because errors.Is unwraps to the sentinel.
			err:  &hiddenCtxErr{inner: context.DeadlineExceeded},
			want: true,
		},
		{
			name: "naked Canceled is NOT retryable",
			err:  context.Canceled,
			want: false,
		},
		{
			name: "hidden-message wrapper of Canceled is NOT retryable",
			err:  &hiddenCtxErr{inner: context.Canceled},
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := isRetryable(tc.err)
			assert.Equalf(t, tc.want, got,
				"isRetryable(%T msg=%q) = %v, want %v",
				tc.err, tc.err.Error(), got, tc.want)
		})
	}
}

// testErr is a tiny helper to construct an error with a known message
// without dragging in 'errors.New' at every callsite. Local to this
// file to avoid polluting the package test surface.
func testErr(msg string) error { return &simpleErr{msg: msg} }

type simpleErr struct{ msg string }

func (e *simpleErr) Error() string { return e.msg }

// hiddenCtxErr wraps a context sentinel with an Error() message that
// deliberately does NOT include the sentinel's standard text. Used by
// TestSanitizeErrorForStorage_ContextSentinels to demonstrate that
// strings.Contains on err.Error() is insufficient — only errors.Is
// unwraps the underlying sentinel through the custom Unwrap method.
type hiddenCtxErr struct{ inner error }

func (e *hiddenCtxErr) Error() string { return "request failed for unrelated reasons" }
func (e *hiddenCtxErr) Unwrap() error { return e.inner }
