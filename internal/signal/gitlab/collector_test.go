package gitlab

import (
	"context"
	"encoding/json"
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

// Compile-time interface check pins the collector to the signal.Collector
// contract — same shape as github / forgejo / openssf so collectorsFor
// can dispatch without per-collector type knowledge.
var _ signal.Collector = (*Collector)(nil)

// newTestCollector wires a Collector to an httptest.Server. Mirrors
// the pattern in internal/signal/{github,forgejo}/collector_test.go;
// the shared test discipline (deterministic stubs, no network) makes
// drift between the three forge collectors visible at review time.
func newTestCollector(t *testing.T, handler http.Handler) *Collector {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	client := &Client{
		httpClient: server.Client(),
		baseURL:    server.URL,
	}
	return NewCollectorWithClient(client)
}

// mockGitLabAPI returns a handler serving a realistic
// /projects/{namespace_url_encoded} response. Field names match the
// live GitLab API spec — different from both github AND forgejo:
//
//   - star_count (NOT stars_count like forgejo, NOT stargazers_count
//     like github)
//   - last_activity_at (NOT updated_at like forgejo, NOT pushed_at
//     like github) — closest analog to "last push," advances on push
//     and on issue/MR activity.
//   - namespace.kind ("user" or "group") — present on the same call
//     unlike Forgejo, but Tier 1 still defers owner_type to keep the
//     forge collectors' signal sets symmetric. Owner_type ports for
//     both forgejo (with a second call) and gitlab (free here) in a
//     follow-up Tier 1.5 commit.
//
// GitLab projects are addressed by URL-encoded namespace path:
// gitlab-org/gitlab → gitlab-org%2Fgitlab. The handler asserts on
// r.URL.EscapedPath so the test fails loudly if the client forgets
// to encode the slash (the most likely future bug, since net/url's
// helpers around path-vs-query escaping are easy to mix up).
func mockGitLabAPI(t *testing.T) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/projects/gitlab-org%2Fgitlab", func(w http.ResponseWriter, r *http.Request) {
		const wantPath = "/projects/gitlab-org%2Fgitlab"
		if r.URL.EscapedPath() != wantPath {
			t.Errorf("expected encoded path %q, got %q (raw path %q) — slash MUST be %%2F-encoded",
				wantPath, r.URL.EscapedPath(), r.URL.RawPath)
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(project{
			ID:              278964,
			Name:            "GitLab",
			PathWithNS:      "gitlab-org/gitlab",
			Description:     "GitLab is an open source end-to-end software development platform",
			Namespace:       projectNamespace{Path: "gitlab-org", Kind: "group"},
			CreatedAt:       time.Date(2017, 11, 27, 0, 42, 45, 0, time.UTC),
			LastActivityAt:  time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
			StarCount:       23000,
			ForksCount:      5800,
			OpenIssuesCount: 32000,
			Archived:        false,
		})
	})
	// /groups/<path> for owner_profile metadata when
	// namespace.kind="group". The two API tests that drive this
	// mock (TestCollector_HappyPath / _EmitsOwnerType) own a
	// project under the "gitlab-org" group; this handler covers
	// their group lookup so the owner_profile emission has data.
	mux.HandleFunc("/groups/gitlab-org", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(group{
			ID:        9970,
			Name:      "GitLab.org",
			Path:      "gitlab-org",
			FullPath:  "gitlab-org",
			CreatedAt: time.Date(2015, 8, 25, 0, 0, 0, 0, time.UTC),
		})
	})
	return mux
}

// TestCollector_HappyPath_EmitsOwnerProfile pins the Tier 2
// addition: owner_profile carries account-age + follower-context
// metadata derived from /groups/<path> when the owner is a group,
// or /users?username=<login> when the owner is a user. The
// branching is driven by namespace.kind which Tier 1 already
// captured.
//
// Canonical field shape matches github's owner_profile so cross-
// forge posture rules read uniform fields:
//
//   - login, name, created, account_age_days, type
//
// Fields not in gitlab's basic responses (public_repos, followers
// for groups) are emitted as zero so the signal shape stays
// consistent. Downstream consumers can detect 0 and skip.
func TestCollector_HappyPath_EmitsOwnerProfile(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	// /projects/{id} for Tier 1
	mux.HandleFunc("/projects/gitlab-org%2Fgitlab", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(project{
			ID:              278964,
			Name:            "GitLab",
			PathWithNS:      "gitlab-org/gitlab",
			Namespace:       projectNamespace{Path: "gitlab-org", Kind: "group"},
			CreatedAt:       time.Date(2017, 11, 27, 0, 42, 45, 0, time.UTC),
			LastActivityAt:  time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
			StarCount:       23000,
			ForksCount:      5800,
			OpenIssuesCount: 32000,
		})
	})
	// /groups/<path> — namespace.kind="group" routes here
	mux.HandleFunc("/groups/gitlab-org", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":          9970,
			"name":        "GitLab.org",
			"path":        "gitlab-org",
			"full_path":   "gitlab-org",
			"description": "Open source software to collaborate on code",
			"created_at":  "2015-08-25T00:00:00Z",
		})
	})

	c := newTestCollector(t, mux)
	entity := &profile.Entity{
		ID:        "e1",
		Type:      profile.EntityProject,
		ShortName: "gitlab-org/gitlab",
		URL:       "https://gitlab.com/gitlab-org/gitlab",
	}

	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)

	for _, s := range result.Signals() {
		if s.Type != "owner_profile" {
			continue
		}
		assert.Equal(t, "gitlab", s.Source)
		var v map[string]any
		require.NoError(t, json.Unmarshal(s.Value, &v))
		assert.Equal(t, "gitlab-org", v["login"],
			"group path becomes the login on gitlab")
		assert.Equal(t, "GitLab.org", v["name"],
			"group name maps to the canonical name field")
		assert.Equal(t, "2015-08-25T00:00:00Z", v["created"])
		age, ok := v["account_age_days"].(float64)
		require.True(t, ok)
		assert.Greater(t, age, float64(0))
		assert.Equal(t, "Organization", v["type"],
			"namespace.kind=group → owner_profile.type=Organization")
		return
	}
	t.Fatalf("owner_profile signal not emitted for group namespace")
}

// TestCollector_UserNamespace_EmitsOwnerProfile pins the parallel
// branch: namespace.kind="user" routes through /users?username= to
// fetch user metadata. Different gitlab endpoint shape than
// /groups, so the collector's branching must produce the same
// canonical signal value either way.
func TestCollector_UserNamespace_EmitsOwnerProfile(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("/projects/alice%2Fdotfiles", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(project{
			ID:             1,
			Name:           "dotfiles",
			PathWithNS:     "alice/dotfiles",
			Namespace:      projectNamespace{Path: "alice", Kind: "user"},
			CreatedAt:      time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
			LastActivityAt: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
			StarCount:      1,
		})
	})
	// /users?username=alice — array response, first matching user.
	mux.HandleFunc("/users", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("username") != "alice" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{
				"id":         42,
				"username":   "alice",
				"name":       "Alice Example",
				"created_at": "2018-03-15T08:00:00Z",
				"state":      "active",
			},
		})
	})

	c := newTestCollector(t, mux)
	entity := &profile.Entity{
		ID:        "e1",
		Type:      profile.EntityProject,
		ShortName: "alice/dotfiles",
		URL:       "https://gitlab.com/alice/dotfiles",
	}

	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)

	for _, s := range result.Signals() {
		if s.Type != "owner_profile" {
			continue
		}
		var v map[string]any
		require.NoError(t, json.Unmarshal(s.Value, &v))
		assert.Equal(t, "alice", v["login"])
		assert.Equal(t, "Alice Example", v["name"])
		assert.Equal(t, "2018-03-15T08:00:00Z", v["created"])
		assert.Equal(t, "User", v["type"],
			"namespace.kind=user → owner_profile.type=User")
		return
	}
	t.Fatalf("owner_profile signal not emitted for user namespace")
}

// TestCollector_HappyPath_EmitsOwnerType pins the Tier 1.5 addition:
// owner_type is emitted from namespace.kind on the same /projects
// call as the other Tier 1 signals — no extra API request. Maps
// gitlab's per-forge values ("user"/"group") onto the canonical
// per-forge-agnostic values github uses ("User"/"Organization") so
// posture rules consume "owner_type=Organization" uniformly across
// forges.
func TestCollector_HappyPath_EmitsOwnerType(t *testing.T) {
	t.Parallel()
	c := newTestCollector(t, mockGitLabAPI(t))
	entity := &profile.Entity{
		ID:        "e1",
		Type:      profile.EntityProject,
		ShortName: "gitlab-org/gitlab",
		URL:       "https://gitlab.com/gitlab-org/gitlab",
	}

	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)

	for _, s := range result.Signals() {
		if s.Type != "owner_type" {
			continue
		}
		assert.Equal(t, "gitlab", s.Source)
		var v map[string]any
		require.NoError(t, json.Unmarshal(s.Value, &v))
		assert.Equal(t, "Organization", v["type"],
			"gitlab namespace.kind=\"group\" must normalize to \"Organization\" so posture rules read the same value across forges")
		assert.Equal(t, "gitlab-org", v["login"],
			"namespace.path is the owner login on gitlab")
		return
	}
	t.Fatalf("owner_type signal not emitted")
}

// TestCollector_UserNamespace_EmitsOwnerTypeUser pins the other half
// of the kind mapping: namespace.kind="user" → owner_type.type="User".
// Separate test because mockGitLabAPI returns a group-owned project;
// a user-owned project needs its own handler.
func TestCollector_UserNamespace_EmitsOwnerTypeUser(t *testing.T) {
	t.Parallel()
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(project{
			ID:              1,
			Name:            "dotfiles",
			PathWithNS:      "alice/dotfiles",
			Namespace:       projectNamespace{Path: "alice", Kind: "user"},
			CreatedAt:       time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
			LastActivityAt:  time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
			StarCount:       1,
			ForksCount:      0,
			OpenIssuesCount: 0,
		})
	})
	c := newTestCollector(t, handler)
	entity := &profile.Entity{
		ID:        "e1",
		Type:      profile.EntityProject,
		ShortName: "alice/dotfiles",
		URL:       "https://gitlab.com/alice/dotfiles",
	}

	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)

	for _, s := range result.Signals() {
		if s.Type != "owner_type" {
			continue
		}
		var v map[string]any
		require.NoError(t, json.Unmarshal(s.Value, &v))
		assert.Equal(t, "User", v["type"],
			"gitlab namespace.kind=\"user\" must normalize to \"User\"")
		return
	}
	t.Fatalf("owner_type signal not emitted")
}

// TestCollector_HappyPath pins the Tier 1 signal set the gitlab
// collector emits for a gitlab.com-hosted entity. Six signals: stars,
// forks, open_issues, archived, repo_age, last_push — derived from
// a single GET against /api/v4/projects/{namespace_encoded}, no
// second call needed.
//
// Symmetric with the forgejo Tier 1 set so analyses across the three
// forges (github / codeberg / gitlab) feed the same posture rules
// without per-forge branching at the policy layer.
func TestCollector_HappyPath(t *testing.T) {
	t.Parallel()
	c := newTestCollector(t, mockGitLabAPI(t))
	entity := &profile.Entity{
		ID:        "e1",
		Type:      profile.EntityProject,
		ShortName: "gitlab-org/gitlab",
		URL:       "https://gitlab.com/gitlab-org/gitlab",
	}

	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)
	require.NotNil(t, result)

	bySource := func(t *testing.T, sigs []profile.Signal, name string) profile.Signal {
		t.Helper()
		for _, s := range sigs {
			if s.Type == name {
				assert.Equal(t, "gitlab", s.Source,
					"signal %q must carry source=gitlab so the store can distinguish API-derived gitlab signals from github/forgejo ones", name)
				return s
			}
		}
		t.Fatalf("expected signal %q in result; got %d signals", name, len(sigs))
		return profile.Signal{}
	}

	collected := result.Signals()
	bySource(t, collected, "stars")
	bySource(t, collected, "forks")
	bySource(t, collected, "open_issues")
	bySource(t, collected, "archived")
	bySource(t, collected, "repo_age")
	bySource(t, collected, "last_push")
}

// TestCollector_StarsValueRoundTrips pins field-mapping discipline:
// star_count from GitLab response → count in the signal value. The
// trap here is that GitLab uses "star_count" while the github
// collector reads "stargazers_count" and the forgejo collector reads
// "stars_count" — three variants of the same concept across three
// forges. A future cross-collector "let's normalize the response
// field names" refactor that drops one of these would silently zero
// stars on whichever collector got missed.
func TestCollector_StarsValueRoundTrips(t *testing.T) {
	t.Parallel()
	c := newTestCollector(t, mockGitLabAPI(t))
	entity := &profile.Entity{
		ID:        "e1",
		Type:      profile.EntityProject,
		ShortName: "gitlab-org/gitlab",
		URL:       "https://gitlab.com/gitlab-org/gitlab",
	}

	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)

	for _, s := range result.Signals() {
		if s.Type != "stars" {
			continue
		}
		var v map[string]any
		require.NoError(t, json.Unmarshal(s.Value, &v))
		assert.Equal(t, float64(23000), v["count"],
			"star_count from GitLab response must reach the signal's count field; field-name drift would zero this silently")
		return
	}
	t.Fatalf("stars signal not emitted")
}

// TestCollector_NonGitLabEntity_ReturnsEmpty pins the self-gate. The
// collector is wired unconditionally for every git-hosted entity in
// collectorsFor (same dispatch-shape discipline as github / forgejo /
// openssf), so the host check must live here. Without it, a github
// or codeberg URL would route through to gitlab.com/api/v4 — wrong
// server, 404, broken run.
//
// Empty URL → empty result (not gated) so legacy ShortName fallback
// in upstream collectors keeps working; the gate fires only when a
// non-empty URL points at a non-gitlab host.
func TestCollector_NonGitLabEntity_ReturnsEmpty(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		url  string
	}{
		{"github URL", "https://github.com/alecthomas/kong"},
		{"codeberg URL", "https://codeberg.org/forgejo/forgejo"},
		{"bitbucket URL", "https://bitbucket.org/team/repo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			handler := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				t.Fatalf("non-GitLab entity must short-circuit; "+
					"handler reached unexpectedly for %s (%s)", tc.name, r.URL.Path)
			})
			c := newTestCollector(t, handler)
			entity := &profile.Entity{
				ID:        "test",
				Type:      profile.EntityProject,
				ShortName: "ignored",
				URL:       tc.url,
			}
			result, err := c.Collect(context.Background(), entity)
			require.NoError(t, err,
				"non-GitLab entity must NOT surface an error — symmetric with github/forgejo/openssf self-gates")
			require.NotNil(t, result,
				"Collect must return a non-nil CollectionResult so callers iterate without nil-guards")
			assert.Empty(t, result.Signals(),
				"non-GitLab entity must produce zero signals (collector self-gates out)")
		})
	}
}

// TestCollector_NotFound_ReturnsError pins the failure path: a 404
// from the GitLab API surfaces as a Collect error rather than a
// silent no-op. Matches forgejo / github failure handling.
func TestCollector_NotFound_ReturnsError(t *testing.T) {
	t.Parallel()
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	c := newTestCollector(t, handler)
	entity := &profile.Entity{
		ID:        "e1",
		Type:      profile.EntityProject,
		ShortName: "ghost/ghost",
		URL:       "https://gitlab.com/ghost/ghost",
	}

	_, err := c.Collect(context.Background(), entity)
	require.Error(t, err, "404 must surface as a Collect error so the orchestrator can record the failure")
}

// TestCollector_Name pins the collector identifier the orchestrator
// keys on. Changing this without updating cmd/signatory/collectors.go's
// progress narration would silently mislabel the per-collector
// "[gitlab] Collected N signals" line.
func TestCollector_Name(t *testing.T) {
	t.Parallel()
	c := NewCollector()
	assert.Equal(t, "gitlab", c.Name())
}

// TestCollector_NestedNamespace_EncodesEveryslash pins handling of
// gitlab's deeper namespace structure. Unlike github (always
// owner/repo, two segments) and codeberg/forgejo (typically two
// segments), gitlab supports nested groups: gitlab-org/security/foo.
// The full nested path is the "id" gitlab's API expects, with EVERY
// slash %2F-encoded. Without this, the client would either truncate
// (only the first slash encoded) or send a multi-segment path that
// gitlab.com routes elsewhere.
func TestCollector_NestedNamespace_EncodesEveryslash(t *testing.T) {
	t.Parallel()

	// Capture the encoded paths each endpoint sees. Both /projects
	// AND /groups need every slash %2F-encoded — the same nested
	// namespace path flows through projectIDPath for both endpoints.
	var sawProjectPath, sawGroupPath string
	mux := http.NewServeMux()
	mux.HandleFunc("/projects/", func(w http.ResponseWriter, r *http.Request) {
		sawProjectPath = r.URL.EscapedPath()
		_ = json.NewEncoder(w).Encode(project{
			ID:              1,
			Name:            "foo",
			PathWithNS:      "gitlab-org/security/foo",
			Namespace:       projectNamespace{Path: "gitlab-org/security", Kind: "group"},
			CreatedAt:       time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
			LastActivityAt:  time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
			StarCount:       1,
			ForksCount:      0,
			OpenIssuesCount: 0,
		})
	})
	mux.HandleFunc("/groups/", func(w http.ResponseWriter, r *http.Request) {
		sawGroupPath = r.URL.EscapedPath()
		_ = json.NewEncoder(w).Encode(group{
			ID:        9,
			Name:      "security",
			Path:      "security",
			FullPath:  "gitlab-org/security",
			CreatedAt: time.Date(2018, 1, 1, 0, 0, 0, 0, time.UTC),
		})
	})

	c := newTestCollector(t, mux)
	entity := &profile.Entity{
		ID:        "e1",
		Type:      profile.EntityProject,
		ShortName: "gitlab-org/security/foo",
		URL:       "https://gitlab.com/gitlab-org/security/foo",
	}
	_, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)

	// /projects path: full nested namespace + project name encoded.
	const wantProjectPath = "/projects/gitlab-org%2Fsecurity%2Ffoo"
	assert.Equal(t, wantProjectPath, sawProjectPath,
		"nested namespace must encode EVERY slash on /projects; partial encoding would route to a different gitlab path or 404")

	// /groups path: namespace.Path is "gitlab-org/security" (parent
	// group of the project). Same encoding rule — every slash must
	// be %2F. owner_profile emission depends on this encoding being
	// correct for nested groups; a regression here would 404 the
	// owner_profile lookup silently and surface as a per-signal
	// failure rather than a clean group fetch.
	const wantGroupPath = "/groups/gitlab-org%2Fsecurity"
	assert.Equal(t, wantGroupPath, sawGroupPath,
		"nested-group owner_profile lookup must encode every slash in namespace.path; otherwise the /groups fetch 404s and owner_profile lands as a failure rather than a signal")

	// Defense against a future regression that drops the wantPath
	// constants: explicitly check no raw slashes survive in the
	// id-portion of either path.
	idPart, _, _ := strings.Cut(strings.TrimPrefix(sawProjectPath, "/projects/"), "?")
	assert.NotContains(t, idPart, "/",
		"the project-ID portion of /projects must contain ZERO raw slashes")
	idPart, _, _ = strings.Cut(strings.TrimPrefix(sawGroupPath, "/groups/"), "?")
	assert.NotContains(t, idPart, "/",
		"the group-ID portion of /groups must contain ZERO raw slashes")
}
