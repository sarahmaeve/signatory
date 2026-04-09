package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
		ID:   "pkg:go:github.com/alecthomas/kong",
		Type: profile.EntityPackage,
		Name: "alecthomas/kong",
		URL:  "https://github.com/alecthomas/kong",
	}

	signals, err := c.Collect(ctx, entity)
	require.NoError(t, err)
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
		"owner_profile",
	}
	for _, st := range expectedTypes {
		assert.Contains(t, byType, st, "missing signal type: %s", st)
	}
}

func TestCollector_SignalValues(t *testing.T) {
	c := newTestCollector(t, mockGitHubAPI())
	ctx := context.Background()

	entity := &profile.Entity{
		ID:   "pkg:go:github.com/alecthomas/kong",
		Type: profile.EntityPackage,
		Name: "alecthomas/kong",
	}

	signals, err := c.Collect(ctx, entity)
	require.NoError(t, err)

	byType := make(map[string]profile.Signal)
	for _, s := range signals {
		byType[s.Type] = s
	}

	// Stars value.
	var starsVal map[string]interface{}
	require.NoError(t, json.Unmarshal(byType["stars"].Value, &starsVal))
	assert.Equal(t, float64(3023), starsVal["count"])

	// Commit signing ratio.
	var signingVal map[string]interface{}
	require.NoError(t, json.Unmarshal(byType["commit_signing"].Value, &signingVal))
	assert.Equal(t, float64(2), signingVal["signed_count"])
	assert.Equal(t, float64(3), signingVal["total_count"])

	// Total commits.
	var totalVal map[string]interface{}
	require.NoError(t, json.Unmarshal(byType["total_commits"].Value, &totalVal))
	assert.Equal(t, float64(467), totalVal["count"])

	// Owner profile.
	var ownerVal map[string]interface{}
	require.NoError(t, json.Unmarshal(byType["owner_profile"].Value, &ownerVal))
	assert.Equal(t, "alecthomas", ownerVal["login"])
	assert.Equal(t, float64(1419), ownerVal["followers"])
	assert.Equal(t, float64(175), ownerVal["public_repos"])

	// Tags.
	var tagsVal map[string]interface{}
	require.NoError(t, json.Unmarshal(byType["tags"].Value, &tagsVal))
	assert.Equal(t, float64(3), tagsVal["count"])

	// Contributors.
	var contribVal map[string]interface{}
	require.NoError(t, json.Unmarshal(byType["contributors"].Value, &contribVal))
	assert.Equal(t, float64(3), contribVal["count"])
}

func TestCollector_SignalMetadata(t *testing.T) {
	c := newTestCollector(t, mockGitHubAPI())
	ctx := context.Background()

	entity := &profile.Entity{
		ID:   "test-entity",
		Type: profile.EntityPackage,
		Name: "alecthomas/kong",
	}

	signals, err := c.Collect(ctx, entity)
	require.NoError(t, err)

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
		ID:   "test-entity",
		Type: profile.EntityPackage,
		Name: "alecthomas/kong",
	}

	signals, err := c.Collect(ctx, entity)
	require.NoError(t, err)

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
		ID:   "test-entity",
		Type: profile.EntityPackage,
		Name: "alecthomas/kong",
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
		ID:   "test-entity",
		Type: profile.EntityPackage,
		Name: "alecthomas/kong",
	}

	_, err := c.Collect(ctx, entity)
	require.Error(t, err)

	var rateLimitErr *RateLimitError
	assert.ErrorAs(t, err, &rateLimitErr)
}

func TestCollector_NotFoundError(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"Not Found"}`))
	})

	c := newTestCollector(t, handler)
	ctx := context.Background()

	entity := &profile.Entity{
		ID:   "test-entity",
		Type: profile.EntityPackage,
		Name: "nonexistent/repo",
	}

	_, err := c.Collect(ctx, entity)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestCollector_UsesEntityURLOverName(t *testing.T) {
	c := newTestCollector(t, mockGitHubAPI())
	ctx := context.Background()

	entity := &profile.Entity{
		ID:   "test-entity",
		Type: profile.EntityPackage,
		Name: "some-npm-package",
		URL:  "https://github.com/alecthomas/kong",
	}

	signals, err := c.Collect(ctx, entity)
	require.NoError(t, err)
	assert.NotEmpty(t, signals)
}

func TestCollector_TemporalEraClassification(t *testing.T) {
	c := newTestCollector(t, mockGitHubAPI())
	ctx := context.Background()

	entity := &profile.Entity{
		ID:   "test-entity",
		Type: profile.EntityPackage,
		Name: "alecthomas/kong",
	}

	signals, err := c.Collect(ctx, entity)
	require.NoError(t, err)

	byType := make(map[string]profile.Signal)
	for _, s := range signals {
		byType[s.Type] = s
	}

	// Last push and last commit should include era classification.
	var pushVal map[string]interface{}
	require.NoError(t, json.Unmarshal(byType["last_push"].Value, &pushVal))
	assert.Equal(t, "modern-ai", pushVal["era"])

	var commitVal map[string]interface{}
	require.NoError(t, json.Unmarshal(byType["last_commit"].Value, &commitVal))
	assert.Equal(t, "modern-ai", commitVal["era"])
}
