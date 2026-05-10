package gem

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mustParseURL is a parse-or-fatal helper used by the redirect-policy
// unit tests below. Mirrors the npm package's helper of the same name.
func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err, "parse %q", raw)
	return u
}

func TestClient_GetGem(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/gems/rails.json", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(GemResponse{
			Name:             "rails",
			Downloads:        500_000_000,
			VersionDownloads: 2_000_000,
			Version:          "7.1.3",
			VersionCreatedAt: "2024-01-16T00:00:00Z",
			Authors:          "David Heinemeier Hansson",
			SourceCodeURI:    "https://github.com/rails/rails",
		}) //nolint:errcheck
	}))
	defer srv.Close()

	client := NewClientWithBaseURL(srv.URL)
	gem, err := client.GetGem(context.Background(), "rails")
	require.NoError(t, err)
	assert.Equal(t, "rails", gem.Name)
	assert.Equal(t, 500_000_000, gem.Downloads)
	assert.Equal(t, "7.1.3", gem.Version)
	assert.Equal(t, "https://github.com/rails/rails", gem.SourceCodeURI)
}

func TestClient_GetVersions(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/versions/puma.json", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]VersionEntry{
			{Number: "6.4.2", CreatedAt: "2024-01-10T00:00:00Z", Platform: "ruby"},
			{Number: "6.4.1", CreatedAt: "2023-12-01T00:00:00Z", Platform: "ruby"},
			{Number: "6.4.0", CreatedAt: "2023-11-01T00:00:00Z", Yanked: true, Platform: "ruby"},
		}) //nolint:errcheck
	}))
	defer srv.Close()

	client := NewClientWithBaseURL(srv.URL)
	versions, err := client.GetVersions(context.Background(), "puma")
	require.NoError(t, err)
	require.Len(t, versions, 3)
	assert.Equal(t, "6.4.2", versions[0].Number)
	assert.True(t, versions[2].Yanked)
}

func TestClient_GetOwners(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/gems/sidekiq/owners.json", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode([]OwnerEntry{
			{Handle: "mperham"},
		}) //nolint:errcheck
	}))
	defer srv.Close()

	client := NewClientWithBaseURL(srv.URL)
	owners, err := client.GetOwners(context.Background(), "sidekiq")
	require.NoError(t, err)
	require.Len(t, owners, 1)
	assert.Equal(t, "mperham", owners[0].Handle)
}

func TestClient_GetOwners_Unauthorized(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	client := NewClientWithBaseURL(srv.URL)
	_, err := client.GetOwners(context.Background(), "sidekiq")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnauthorized)
}

func TestClient_GetGem_NotFound(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	client := NewClientWithBaseURL(srv.URL)
	_, err := client.GetGem(context.Background(), "nonexistent-gem-xyz")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestClient_ResolveRepoURL(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(GemResponse{
			Name:          "devise",
			SourceCodeURI: "https://github.com/heartcombo/devise",
			HomepageURI:   "https://github.com/heartcombo/devise",
		}) //nolint:errcheck
	}))
	defer srv.Close()

	client := NewClientWithBaseURL(srv.URL)
	url, err := client.ResolveRepoURL(context.Background(), "devise")
	require.NoError(t, err)
	assert.Equal(t, "https://github.com/heartcombo/devise", url)
}

func TestClient_ResolveRepoURL_StripsTreePath(t *testing.T) {
	t.Parallel()

	// Real-world: rubygems.org sets source_code_uri to the version's
	// tree view, e.g. https://github.com/rails/rails/tree/v8.1.3.
	// normalizeRepoURL must strip /tree/<ref> to produce a cloneable URL.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(GemResponse{
			Name:          "rails",
			SourceCodeURI: "https://github.com/rails/rails/tree/v8.1.3",
		}) //nolint:errcheck
	}))
	defer srv.Close()

	client := NewClientWithBaseURL(srv.URL)
	url, err := client.ResolveRepoURL(context.Background(), "rails")
	require.NoError(t, err)
	assert.Equal(t, "https://github.com/rails/rails", url)
}

func TestClient_ResolveRepoURL_StripsBlobPath(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(GemResponse{
			Name:          "somegem",
			SourceCodeURI: "https://github.com/org/somegem/blob/main/README.md",
		}) //nolint:errcheck
	}))
	defer srv.Close()

	client := NewClientWithBaseURL(srv.URL)
	url, err := client.ResolveRepoURL(context.Background(), "somegem")
	require.NoError(t, err)
	assert.Equal(t, "https://github.com/org/somegem", url)
}

func TestClient_ResolveRepoURL_FallsBackToHomepage(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(GemResponse{
			Name:          "oldgem",
			SourceCodeURI: "",
			HomepageURI:   "https://github.com/org/oldgem",
		}) //nolint:errcheck
	}))
	defer srv.Close()

	client := NewClientWithBaseURL(srv.URL)
	url, err := client.ResolveRepoURL(context.Background(), "oldgem")
	require.NoError(t, err)
	assert.Equal(t, "https://github.com/org/oldgem", url)
}

// TestClient_ResolveRepoURL_Codeberg pins multi-forge resolution for
// gems: a rubygems.org metadata response declaring source_code_uri
// at codeberg.org normalizes to a clone-able https URL. gem's
// resolver (isGitHostURL) recognizes github.com and gitlab.com today;
// this test pins the codeberg.org addition.
func TestClient_ResolveRepoURL_Codeberg(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(GemResponse{
			Name:          "codeberg-gem",
			SourceCodeURI: "https://codeberg.org/forgejo/runner",
		}) //nolint:errcheck
	}))
	defer srv.Close()

	client := NewClientWithBaseURL(srv.URL)
	url, err := client.ResolveRepoURL(context.Background(), "codeberg-gem")
	require.NoError(t, err)
	assert.Equal(t, "https://codeberg.org/forgejo/runner", url)
}

// TestClient_ResolveRepoURL_GitLab pins gitlab resolution — the
// existing isGitHostURL has supported gitlab.com since gem-resolver
// inception; this test makes the support explicit so a future
// refactor can't silently regress it.
func TestClient_ResolveRepoURL_GitLab(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(GemResponse{
			Name:          "gitlab-gem",
			SourceCodeURI: "https://gitlab.com/foo/bar",
		}) //nolint:errcheck
	}))
	defer srv.Close()

	client := NewClientWithBaseURL(srv.URL)
	url, err := client.ResolveRepoURL(context.Background(), "gitlab-gem")
	require.NoError(t, err)
	assert.Equal(t, "https://gitlab.com/foo/bar", url)
}

func TestClient_ResolveRepoURL_NoSource(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(GemResponse{
			Name:          "nosource",
			SourceCodeURI: "",
			HomepageURI:   "https://example.com/docs",
		}) //nolint:errcheck
	}))
	defer srv.Close()

	client := NewClientWithBaseURL(srv.URL)
	url, err := client.ResolveRepoURL(context.Background(), "nosource")
	require.NoError(t, err)
	assert.Empty(t, url, "non-github homepage should not be returned")
}

func TestClient_ValidateGemName(t *testing.T) {
	t.Parallel()

	valid := []string{"rails", "rspec-rails", "active_support", "foo.bar", "a1"}
	for _, name := range valid {
		assert.NoError(t, ValidateGemName(name), "expected valid: %q", name)
	}

	invalid := []string{"", "../etc/passwd", "a/b", "foo\x00bar", " leading"}
	for _, name := range invalid {
		assert.Error(t, ValidateGemName(name), "expected invalid: %q", name)
	}
}

// ----- redirect policy unit tests -----
//
// rubygems.org is HTTPS-only. Any redirect target other than HTTPS is
// either a misconfiguration or a MITM attempting a scheme downgrade
// to tamper with owner / version metadata that feeds trust signals.
// The policy here is symmetric with the npm, PyPI, cargo, and gopublish
// clients so that a future audit can grep for one shape.

func TestClient_CheckRedirect_RefusesNonHTTPS(t *testing.T) {
	t.Parallel()

	via := []*http.Request{{URL: mustParseURL(t, "https://rubygems.org/api/v1/gems/rails.json")}}

	tests := []struct {
		name   string
		target string
	}{
		{"http scheme downgrade", "http://rubygems.org/api/v1/gems/rails.json"},
		{"attacker-host http redirect", "http://attacker.example/x"},
		{"file scheme", "file:///etc/passwd"},
		{"javascript scheme", "javascript:alert(1)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			next := &http.Request{URL: mustParseURL(t, tc.target)}
			err := checkRedirect(next, via)
			require.Error(t, err, "must refuse redirect to %q", tc.target)
		})
	}
}

func TestClient_CheckRedirect_AllowsHTTPS(t *testing.T) {
	t.Parallel()

	via := []*http.Request{{URL: mustParseURL(t, "https://rubygems.org/old-path")}}
	next := &http.Request{URL: mustParseURL(t, "https://rubygems.org/new-path")}
	assert.NoError(t, checkRedirect(next, via))
}

func TestClient_CheckRedirect_BoundsChain(t *testing.T) {
	t.Parallel()

	via := make([]*http.Request, 10)
	for i := range via {
		via[i] = &http.Request{URL: mustParseURL(t, "https://rubygems.org/")}
	}
	next := &http.Request{URL: mustParseURL(t, "https://rubygems.org/next")}
	err := checkRedirect(next, via)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "redirects")
}

// TestClient_NewClient_WiresCheckRedirect asserts that the production
// constructor actually installs the redirect policy on the http.Client.
// Regression guard against a future refactor that adds a new code path
// (e.g. NewClientWithToken) and forgets to mirror the CheckRedirect
// wiring — which is the bug class this whole test cluster exists to
// catch.
func TestClient_NewClient_WiresCheckRedirect(t *testing.T) {
	t.Parallel()

	for _, c := range []*Client{NewClient(), NewClientWithBaseURL("https://example.test")} {
		require.NotNil(t, c.httpClient.CheckRedirect,
			"NewClient must install CheckRedirect; default policy follows HTTP redirects")
	}
}
