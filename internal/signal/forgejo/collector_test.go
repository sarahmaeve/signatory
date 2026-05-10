package forgejo

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

// Compile-time interface check pins the collector to the signal.Collector
// contract — same shape as github/openssf/etc. so collectorsFor can
// dispatch without per-collector type knowledge.
var _ signal.Collector = (*Collector)(nil)

// newTestCollector wires a Collector to an httptest.Server, mirroring
// the pattern in internal/signal/github/collector_test.go. The shared
// shape lets a reader verify the codeberg/forgejo and github paths
// have the same test discipline (deterministic stubs, no network).
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

// mockForgejoAPI returns a handler serving a realistic /repos/{owner}/{repo}
// response shape. Field names match the live Forgejo API spec:
//
//   - stars_count (NOT stargazers_count, which is github)
//   - forks_count (same shape as github)
//   - open_issues_count (same)
//   - archived (same)
//   - created_at / updated_at (same)
//   - owner.login (same; Forgejo's owner User struct lacks an explicit
//     Type field — distinguishing user vs organization needs a second
//     /users/{u} or /orgs/{o} call. owner_type is intentionally NOT
//     emitted in this Tier 1 commit; ports as a follow-up.)
//
// Note: Forgejo doesn't expose a separate `pushed_at` like github;
// `updated_at` is the closest analog (it advances on push). The
// last_push signal uses updated_at as its date — same canonical
// signal type, different upstream field.
func mockForgejoAPI() http.Handler {
	mux := http.NewServeMux()

	// Path is registered without the /api/v1 prefix because the test
	// injects baseURL=server.URL directly; production NewClient bakes
	// the /api/v1 root into baseURL so production paths read as
	// "https://codeberg.org/api/v1/repos/...". The test exercises the
	// same Client.get path-construction logic ("baseURL + path"), just
	// with a baseURL that doesn't carry an /api/v1 segment.
	mux.HandleFunc("/repos/forgejo/forgejo", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(repo{
			Name:            "forgejo",
			FullName:        "forgejo/forgejo",
			Description:     "Beyond coding. We forge.",
			Owner:           repoOwner{Login: "forgejo"},
			CreatedAt:       time.Date(2017, 1, 23, 22, 40, 11, 0, time.UTC),
			UpdatedAt:       time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
			StarsCount:      2500,
			ForksCount:      400,
			OpenIssuesCount: 250,
			Archived:        false,
		})
	})

	// /orgs/{name} probe for owner_type. 200 → organization;
	// 404 → user. The collector treats anything else as a failure
	// recorded against owner_type alone (other signals continue).
	// mockForgejoAPI's "forgejo" owner is an organization; the
	// user-account branch is exercised by a dedicated test below.
	mux.HandleFunc("/orgs/forgejo", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":          1,
			"username":    "forgejo",
			"full_name":   "Forgejo",
			"description": "Forgejo organisation",
			"visibility":  "public",
		})
	})

	return mux
}

// TestCollector_HappyPath pins the Tier 1 + Tier 1.5 signal set the
// forgejo collector emits for a codeberg-hosted entity. Seven
// signals: stars, forks, open_issues, archived, repo_age, last_push
// (Tier 1, from /repos), plus owner_type (Tier 1.5, from /orgs probe).
//
// Forgejo's /repos response doesn't carry a User/Organization
// discriminator on the owner, unlike github (Owner.Type) and gitlab
// (namespace.kind on the same call). The forgejo collector probes
// /api/v1/orgs/{name} for the owner: 200 means the owner is an
// organization, 404 means it's a user account, anything else is a
// failure recorded against owner_type without affecting the Tier 1
// signals. mockForgejoAPI() returns 200 for the "forgejo" org probe,
// so the happy path produces type="Organization".
//
// The signal types reuse the existing registry entries (stars, forks,
// archived, etc. are forge-agnostic at the signal-type layer; Source
// distinguishes "github" from "forgejo" emissions). Adding a new
// type-name per forge would fragment posture rules and break the
// "stars from any forge feed the same posture decision" property.
func TestCollector_HappyPath(t *testing.T) {
	t.Parallel()
	c := newTestCollector(t, mockForgejoAPI())
	entity := &profile.Entity{
		ID:        "e1",
		Type:      profile.EntityProject,
		ShortName: "forgejo/forgejo",
		URL:       "https://codeberg.org/forgejo/forgejo",
	}

	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)
	require.NotNil(t, result)

	bySource := func(t *testing.T, sigs []profile.Signal, name string) profile.Signal {
		t.Helper()
		for _, s := range sigs {
			if s.Type == name {
				assert.Equal(t, "forgejo", s.Source,
					"signal %q must carry source=forgejo so the store can distinguish API-derived codeberg signals from github ones", name)
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
	bySource(t, collected, "owner_type")
}

// TestCollector_OwnerType_OrganizationProbe pins the owner_type
// value when the /orgs/{name} probe returns 200. Maps onto the
// github canonical alphabet ("Organization") so cross-forge posture
// rules read the same value regardless of forge.
func TestCollector_OwnerType_OrganizationProbe(t *testing.T) {
	t.Parallel()
	c := newTestCollector(t, mockForgejoAPI())
	entity := &profile.Entity{
		ID:        "e1",
		Type:      profile.EntityProject,
		ShortName: "forgejo/forgejo",
		URL:       "https://codeberg.org/forgejo/forgejo",
	}

	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)

	for _, s := range result.Signals() {
		if s.Type != "owner_type" {
			continue
		}
		var v map[string]any
		require.NoError(t, json.Unmarshal(s.Value, &v))
		assert.Equal(t, "Organization", v["type"],
			"200 from /orgs/{name} probe must map to \"Organization\" so posture rules align with github / gitlab")
		assert.Equal(t, "forgejo", v["login"],
			"owner login (from the repo's owner.login field) must round-trip into the signal value")
		return
	}
	t.Fatalf("owner_type signal not emitted in the Organization-probe case")
}

// TestCollector_OwnerType_UserProbe pins the other branch: a 404
// from /orgs/{name} means the owner isn't an organization, so it's
// a user account. Codeberg's user repos take this path.
//
// The handler intentionally returns the repo metadata happily and
// 404s the /orgs/{name} probe — the latter is the only signal of
// "this is a user, not an org" we have without a second call.
func TestCollector_OwnerType_UserProbe(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/alice/dotfiles", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(repo{
			Name:            "dotfiles",
			FullName:        "alice/dotfiles",
			Owner:           repoOwner{Login: "alice"},
			CreatedAt:       time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
			UpdatedAt:       time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
			StarsCount:      1,
			ForksCount:      0,
			OpenIssuesCount: 0,
		})
	})
	mux.HandleFunc("/orgs/alice", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	c := newTestCollector(t, mux)
	entity := &profile.Entity{
		ID:        "e1",
		Type:      profile.EntityProject,
		ShortName: "alice/dotfiles",
		URL:       "https://codeberg.org/alice/dotfiles",
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
			"404 from /orgs/{name} probe must map to \"User\" — the owner is a user account, not an organization")
		assert.Equal(t, "alice", v["login"])
		return
	}
	t.Fatalf("owner_type signal not emitted in the User-probe case")
}

// TestCollector_OwnerType_ProbeFailure_DoesNotBreakOtherSignals
// pins the isolation property: a 5xx (or other non-200/non-404)
// from /orgs/{name} must not poison the Tier 1 signal collection.
// owner_type itself surfaces as a per-signal failure; stars, forks,
// etc. all still emit cleanly. The test would fail RED if a future
// refactor coupled the owner_type probe to the rest of Collect
// (e.g., a single error path that aborts the whole method).
func TestCollector_OwnerType_ProbeFailure_DoesNotBreakOtherSignals(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/forgejo/forgejo", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(repo{
			Name:            "forgejo",
			Owner:           repoOwner{Login: "forgejo"},
			CreatedAt:       time.Date(2017, 1, 1, 0, 0, 0, 0, time.UTC),
			UpdatedAt:       time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
			StarsCount:      100,
			ForksCount:      10,
			OpenIssuesCount: 5,
		})
	})
	mux.HandleFunc("/orgs/forgejo", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})

	c := newTestCollector(t, mux)
	entity := &profile.Entity{
		ID:        "e1",
		Type:      profile.EntityProject,
		ShortName: "forgejo/forgejo",
		URL:       "https://codeberg.org/forgejo/forgejo",
	}

	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err,
		"a 5xx from /orgs/{name} must record as a per-signal failure, not abort the whole Collect")

	// Tier 1 signals all present.
	types := make(map[string]bool)
	for _, s := range result.Signals() {
		types[s.Type] = true
	}
	for _, want := range []string{"stars", "forks", "open_issues", "archived", "repo_age", "last_push"} {
		assert.True(t, types[want],
			"Tier 1 signal %q must still emit when the owner_type probe failed — coupling them would be a regression", want)
	}

	// owner_type recorded as a failure.
	assert.NotEmpty(t, result.Failures, "5xx must surface as a CollectionError so the run summary shows it")
	assert.Equal(t, "owner_type", result.Failures[0].SignalType)
	assert.True(t, result.Failures[0].Retryable,
		"5xx is retryable — the next analyze refresh might succeed")
}

// TestCollector_StarsValueRoundTrips pins the field-mapping discipline:
// stars_count from the Forgejo response must land as count in the
// signal value. Without this, a future field-name change in the
// response struct that didn't update the emission site would
// silently zero out stars across every codeberg entity.
func TestCollector_StarsValueRoundTrips(t *testing.T) {
	t.Parallel()
	c := newTestCollector(t, mockForgejoAPI())
	entity := &profile.Entity{
		ID:        "e1",
		Type:      profile.EntityProject,
		ShortName: "forgejo/forgejo",
		URL:       "https://codeberg.org/forgejo/forgejo",
	}

	result, err := c.Collect(context.Background(), entity)
	require.NoError(t, err)

	for _, s := range result.Signals() {
		if s.Type != "stars" {
			continue
		}
		var v map[string]any
		require.NoError(t, json.Unmarshal(s.Value, &v))
		// JSON numbers decode as float64 by default; the github collector
		// uses the same shape so this is a known idiom.
		assert.Equal(t, float64(2500), v["count"],
			"stars_count from Forgejo response must reach the signal's count field; field-name drift would zero this silently")
		return
	}
	t.Fatalf("stars signal not emitted")
}

// TestCollector_NonCodebergEntity_ReturnsEmpty pins the self-gate.
// The collector is wired unconditionally for every git-hosted entity
// in collectorsFor (same dispatch-shape discipline as github / openssf),
// so the host check must live here. Without it, a github URL would
// route through to codeberg.org/api/v1 — wrong server, 404, broken
// run. Mirror of github's isGitHubHost gate.
//
// Empty URL → empty result (not gated) so the legacy ShortName
// fallback in upstream collectors keeps working; the gate fires only
// when a non-empty URL points at a non-Forgejo host.
func TestCollector_NonCodebergEntity_ReturnsEmpty(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		url  string
	}{
		{"github URL", "https://github.com/alecthomas/kong"},
		{"gitlab URL", "https://gitlab.com/gitlab-org/gitlab"},
		{"bitbucket URL", "https://bitbucket.org/team/repo"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			handler := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				t.Fatalf("non-Forgejo entity must short-circuit; "+
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
				"non-Forgejo entity must NOT surface an error — symmetric with github/openssf self-gates")
			require.NotNil(t, result,
				"Collect must return a non-nil CollectionResult so callers iterate without nil-guards")
			assert.Empty(t, result.Signals(),
				"non-Forgejo entity must produce zero signals (collector self-gates out)")
		})
	}
}

// TestCollector_NotFound_ReturnsError pins the failure path: a 404
// from the Forgejo API surfaces as a Collect error rather than a
// silent no-op. The orchestrator (collectFreshSignals in analyze.go)
// records the per-collector failure but continues with the rest of
// the dispatch — same shape as github's GetRepo error handling.
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
		URL:       "https://codeberg.org/ghost/ghost",
	}

	_, err := c.Collect(context.Background(), entity)
	require.Error(t, err, "404 must surface as a Collect error so the orchestrator can record the failure")
}

// TestCollector_Name pins the collector identifier the orchestrator
// keys on. Changing this without updating cmd/signatory/collectors.go's
// progress narration would silently mislabel the per-collector
// "[forgejo] Collected N signals" line.
func TestCollector_Name(t *testing.T) {
	t.Parallel()
	c := NewCollector()
	assert.Equal(t, "forgejo", c.Name())
}
