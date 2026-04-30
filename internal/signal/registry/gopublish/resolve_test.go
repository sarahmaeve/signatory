package gopublish

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newVanityServer returns an httptest.Server that serves a fixed
// HTML body for every request — mimicking a vanity host's response
// to GET <module>?go-get=1. Empty body means "no meta tag
// available" (the resolver's "neither path resolves" case).
//
// Cleanup is registered via t.Cleanup; callers don't need to
// defer Close themselves.
func newVanityServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newVanityServerHandler is the explicit-handler form for tests
// that want to assert on the request (e.g., "was the vanity host
// contacted?") rather than just the response body.
func newVanityServerHandler(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// TestResolveRepoURL_ProxyHasOrigin pins the happy path: proxy.golang.org
// returns @latest pointing at version V, then @v/V.info carries an
// Origin block with URL=https://github.com/foo/bar. Resolver returns
// that URL verbatim — it's already github-canonical.
//
// This is the post-Go-1.20 publish flow: any module published with a
// modern toolchain that respects the Origin spec yields a clean
// resolution without touching the vanity host.
func TestResolveRepoURL_ProxyHasOrigin(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	fs.mux.HandleFunc("/golang.org/x/sync/@latest", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"Version":"v0.20.0","Time":"2026-04-15T10:00:00Z"}`)
	})
	fs.mux.HandleFunc("/golang.org/x/sync/@v/v0.20.0.info", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
            "Version":"v0.20.0",
            "Time":"2026-04-15T10:00:00Z",
            "Origin":{
                "VCS":"git",
                "URL":"https://github.com/golang/sync",
                "Ref":"refs/tags/v0.20.0",
                "Hash":"abc123"
            }
        }`)
	})

	c := NewClientWithBaseURL(fs.URL(), fs.URL())
	got, err := c.ResolveRepoURL(context.Background(), "golang.org/x/sync")
	if err != nil {
		t.Fatalf("ResolveRepoURL returned %v; want nil", err)
	}
	want := "https://github.com/golang/sync"
	if got != want {
		t.Errorf("ResolveRepoURL = %q; want %q", got, want)
	}
}

// TestResolveRepoURL_ProxyOriginNonGitHub guards the v0.1 scope:
// signatory only consumes github-hosted sources for its github + git
// + repofiles + openssf collectors, so a proxy Origin pointing at
// gitlab.com / bitbucket.org / a private GitHub Enterprise instance
// is treated as "not resolvable to a github URL" — empty string,
// not an error. The user gets registry-only signals (publish
// integrity, transparency log) and a posture decision based on those.
//
// Future: if/when signatory grows non-github source support, this
// test flips to assert the gitlab URL resolves directly.
func TestResolveRepoURL_ProxyOriginNonGitHub(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	fs.mux.HandleFunc("/example.org/foo/@latest", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"Version":"v1.0.0","Time":"2026-01-01T00:00:00Z"}`)
	})
	fs.mux.HandleFunc("/example.org/foo/@v/v1.0.0.info", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
            "Version":"v1.0.0",
            "Time":"2026-01-01T00:00:00Z",
            "Origin":{
                "VCS":"git",
                "URL":"https://gitlab.com/foo/bar"
            }
        }`)
	})

	c := NewClientWithBaseURL(fs.URL(), fs.URL())
	got, err := c.ResolveRepoURL(context.Background(), "example.org/foo")
	if err != nil {
		t.Fatalf("ResolveRepoURL returned %v; want nil for non-github resolvable", err)
	}
	if got != "" {
		t.Errorf("ResolveRepoURL = %q; want empty string for non-github source", got)
	}
}

// TestResolveRepoURL_ProxyLatestNotFound_NoMetaTag covers the
// "fully exhausted" path: proxy 404 AND vanity host has no
// go-import meta tag. Returns empty — the orchestrator records an
// absence:repo_declaration signal and the user gets registry-only
// signals.
func TestResolveRepoURL_ProxyLatestNotFound_NoMetaTag(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	vanity := newVanityServer(t, "")

	c := NewClientWithBaseURLs(fs.URL(), fs.URL(), vanity.URL)
	got, err := c.ResolveRepoURL(context.Background(), "example.org/notfound")
	if err != nil {
		t.Fatalf("ResolveRepoURL returned %v; want nil when proxy 404s and vanity has no meta tag", err)
	}
	if got != "" {
		t.Errorf("ResolveRepoURL = %q; want empty when neither proxy nor meta tag resolves", got)
	}
}

// TestResolveRepoURL_FallsBackToMetaTag_WhenProxy404 covers the
// canonical vanity-host case: the proxy doesn't have the module,
// but the vanity host serves a go-import meta tag pointing at
// github. Resolver follows the fallback chain and returns the
// github URL.
func TestResolveRepoURL_FallsBackToMetaTag_WhenProxy404(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	vanity := newVanityServer(t, `<html><head>
		<meta name="go-import" content="example.org/notfound git https://github.com/foo/bar">
	</head></html>`)

	c := NewClientWithBaseURLs(fs.URL(), fs.URL(), vanity.URL)
	got, err := c.ResolveRepoURL(context.Background(), "example.org/notfound")
	if err != nil {
		t.Fatalf("ResolveRepoURL returned %v; want nil", err)
	}
	want := "https://github.com/foo/bar"
	if got != want {
		t.Errorf("ResolveRepoURL = %q; want %q (meta-tag fallback after proxy 404)", got, want)
	}
}

// TestResolveRepoURL_FallsBackToMetaTag_WhenOriginAbsent covers the
// pre-Go-1.20 vanity-host case (the gopkg.in/yaml.v3 dogfood
// symptom): proxy has the module but no Origin block, so the
// resolver falls back to the meta tag and finds the github URL
// there.
func TestResolveRepoURL_FallsBackToMetaTag_WhenOriginAbsent(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	fs.mux.HandleFunc("/gopkg.in/yaml.v3/@latest", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"Version":"v3.0.1","Time":"2022-05-27T08:35:30Z"}`)
	})
	fs.mux.HandleFunc("/gopkg.in/yaml.v3/@v/v3.0.1.info", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// No Origin block — pre-1.20 publish.
		fmt.Fprint(w, `{"Version":"v3.0.1","Time":"2022-05-27T08:35:30Z"}`)
	})
	vanity := newVanityServer(t, `<html><head>
		<meta name="go-import" content="gopkg.in/yaml.v3 git https://github.com/go-yaml/yaml">
	</head></html>`)

	c := NewClientWithBaseURLs(fs.URL(), fs.URL(), vanity.URL)
	got, err := c.ResolveRepoURL(context.Background(), "gopkg.in/yaml.v3")
	if err != nil {
		t.Fatalf("ResolveRepoURL returned %v; want nil", err)
	}
	want := "https://github.com/go-yaml/yaml"
	if got != want {
		t.Errorf("ResolveRepoURL = %q; want %q (meta-tag fallback after empty Origin)", got, want)
	}
}

// TestResolveRepoURL_NoMetaTagFallback_WhenProxy5xx pins the
// "transient infrastructure → don't fall back" rule. A 5xx response
// from the proxy is a retry-this-later condition; we don't switch
// to vanity-host fetch because the module probably exists. Falling
// back here would mask transient proxy outages and produce
// inconsistent results across retries.
func TestResolveRepoURL_NoMetaTagFallback_WhenProxy5xx(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	fs.handler = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "proxy is on fire")
	}
	// Vanity server present and would serve a meta tag, but we
	// expect the resolver NOT to call it on 5xx.
	called := false
	vanity := newVanityServerHandler(t, func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<meta name="go-import" content="example.org/foo git https://github.com/foo/bar">`)
	})

	c := NewClientWithBaseURLs(fs.URL(), fs.URL(), vanity.URL)
	_, err := c.ResolveRepoURL(context.Background(), "example.org/foo")
	if err == nil {
		t.Fatal("ResolveRepoURL returned nil; want error on proxy 5xx")
	}
	if called {
		t.Error("vanity host was contacted after proxy 5xx; resolver must not fall back to meta tag on transient errors")
	}
}

// TestResolveRepoURL_RejectsNonGitHubMetaRoot guards the v0.1 scope
// at the meta-tag layer too: a vanity host serving a go-import
// meta tag pointing at gitlab.com (or any non-github VCS host)
// must NOT be stamped on the entity. Returns empty with no error.
func TestResolveRepoURL_RejectsNonGitHubMetaRoot(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	vanity := newVanityServer(t, `<meta name="go-import" content="modernc.org/sqlite git https://gitlab.com/cznic/sqlite">`)

	c := NewClientWithBaseURLs(fs.URL(), fs.URL(), vanity.URL)
	got, err := c.ResolveRepoURL(context.Background(), "modernc.org/sqlite")
	if err != nil {
		t.Fatalf("ResolveRepoURL returned %v; want nil", err)
	}
	if got != "" {
		t.Errorf("ResolveRepoURL = %q; want empty for non-github meta root (signatory v0.1 only stamps github sources)", got)
	}
}

// TestResolveRepoURL_FallsThroughToGoSource pins the gopkg.in
// pattern: go-import points at the vanity host itself (which
// proxies git), and only go-source carries the canonical github
// URL. The resolver must read both meta tags and prefer the
// github URL out of go-source when go-import doesn't yield one.
//
// This is the actual real-world fixture from
// https://gopkg.in/yaml.v3?go-get=1.
func TestResolveRepoURL_FallsThroughToGoSource(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	vanity := newVanityServer(t, `<html>
<head>
<meta name="go-import" content="gopkg.in/yaml.v3 git https://gopkg.in/yaml.v3">
<meta name="go-source" content="gopkg.in/yaml.v3 _ https://github.com/go-yaml/yaml/tree/v3.0.1{/dir} https://github.com/go-yaml/yaml/blob/v3.0.1{/dir}/{file}#L{line}">
</head>
</html>`)

	c := NewClientWithBaseURLs(fs.URL(), fs.URL(), vanity.URL)
	got, err := c.ResolveRepoURL(context.Background(), "gopkg.in/yaml.v3")
	if err != nil {
		t.Fatalf("ResolveRepoURL returned %v; want nil", err)
	}
	want := "https://github.com/go-yaml/yaml"
	if got != want {
		t.Errorf("ResolveRepoURL = %q; want %q (gopkg.in fallthrough: go-import is gopkg.in proxy, go-source is github)", got, want)
	}
}

// TestResolveRepoURL_GoSourceCrossOriginRejected guards the cross-
// origin defense at the go-source layer too: if a vanity host's
// go-source meta tag declares an importPrefix that doesn't match
// the requested module path, the URL is dropped. Same defense as
// the go-import path.
func TestResolveRepoURL_GoSourceCrossOriginRejected(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	vanity := newVanityServer(t, `<html><head>
<meta name="go-import" content="gopkg.in/yaml.v3 git https://gopkg.in/yaml.v3">
<meta name="go-source" content="totally.different.org/x _ https://github.com/attacker/repo/tree/main{/dir} https://github.com/attacker/repo/blob/main{/dir}/{file}#L{line}">
</head></html>`)

	c := NewClientWithBaseURLs(fs.URL(), fs.URL(), vanity.URL)
	got, err := c.ResolveRepoURL(context.Background(), "gopkg.in/yaml.v3")
	if err != nil {
		t.Fatalf("ResolveRepoURL returned %v; want nil", err)
	}
	if got != "" {
		t.Errorf("ResolveRepoURL = %q; want empty (go-source importPrefix mismatch must be rejected)", got)
	}
}

// TestResolveRepoURL_RejectsCrossOriginMeta defends the cross-
// origin attack: a vanity host claims a go-import meta tag pointing
// at github, but the importPrefix in the meta tag doesn't match the
// module path the user asked about. Possibly attacker-controlled
// HTML, possibly misconfiguration; either way, refusing is the
// safe outcome.
//
// The check: the meta tag's importPrefix (first content field)
// must equal the requested module path, OR be a prefix of it
// (covers the case where you ask about "k8s.io/client-go/tools/cache"
// and the meta tag declares "k8s.io/client-go" as the module root).
func TestResolveRepoURL_RejectsCrossOriginMeta(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	// Vanity host returns a meta tag whose importPrefix is for a
	// different module than what we asked about.
	vanity := newVanityServer(t, `<meta name="go-import" content="totally.different.org/other git https://github.com/foo/bar">`)

	c := NewClientWithBaseURLs(fs.URL(), fs.URL(), vanity.URL)
	got, err := c.ResolveRepoURL(context.Background(), "example.org/asked-about")
	if err != nil {
		t.Fatalf("ResolveRepoURL returned %v; want nil", err)
	}
	if got != "" {
		t.Errorf("ResolveRepoURL = %q; want empty for cross-origin meta tag (attribution attack defense)", got)
	}
}

// TestResolveRepoURL_ProxyServerError covers transient infrastructure
// failure: the proxy returns 5xx, signatory can't reach a definitive
// answer about whether the module has an Origin. Return an error
// (wrapping the upstream failure) so the orchestrator's --refresh
// path fails loud — this is "we couldn't reach the answer," distinct
// from "the answer is no."
//
// The 5xx case does NOT fall back to meta tag (when that lands)
// because the module probably exists; transient infrastructure
// retry is the right action.
func TestResolveRepoURL_ProxyServerError(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	fs.handler = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "upstream is on fire")
	}

	c := NewClientWithBaseURL(fs.URL(), fs.URL())
	_, err := c.ResolveRepoURL(context.Background(), "example.org/foo")
	if err == nil {
		t.Fatal("ResolveRepoURL returned nil; want error on 5xx")
	}
	if errors.Is(err, ErrNotFound) {
		t.Errorf("ResolveRepoURL wrapped ErrNotFound on 5xx; want a non-NotFound error so orchestrator distinguishes 'transient' from 'definitive negative'")
	}
}

// TestResolveRepoURL_ProxyOriginAbsent covers the pre-Go-1.20 case:
// proxy returns the version info but the Origin block is empty (the
// gopkg.in/yaml.v3 dogfood symptom). For now (proxy-only resolver)
// this returns empty string. Once the meta-tag fallback ships, the
// expected behavior flips to "fall back, find the github URL via
// the vanity host's go-import meta tag."
func TestResolveRepoURL_ProxyOriginAbsent(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	fs.mux.HandleFunc("/example.org/legacy/@latest", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"Version":"v0.1.0","Time":"2018-01-01T00:00:00Z"}`)
	})
	fs.mux.HandleFunc("/example.org/legacy/@v/v0.1.0.info", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// No Origin block — pre-1.20 publish.
		fmt.Fprint(w, `{"Version":"v0.1.0","Time":"2018-01-01T00:00:00Z"}`)
	})

	c := NewClientWithBaseURL(fs.URL(), fs.URL())
	got, err := c.ResolveRepoURL(context.Background(), "example.org/legacy")
	if err != nil {
		t.Fatalf("ResolveRepoURL returned %v; want nil for empty Origin", err)
	}
	if got != "" {
		t.Errorf("ResolveRepoURL = %q; want empty string when proxy has no Origin block (pre-1.20 publish)", got)
	}
}

// TestResolveRepoURL_PathEncoded covers the case-encoding rule: a
// module path like "github.com/AzureAD/microsoft-authentication-library"
// must be encoded as "github.com/!azure!a!d/..." when constructing
// the proxy URL. encodeModulePath already exists and is exercised
// by GetLatest tests; this test pins that ResolveRepoURL also
// flows through it (a regression that bypassed encoding would
// break uppercase-owner modules silently).
func TestResolveRepoURL_PathEncoded(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	// Note the bang-encoding: AzureAD → !azure!a!d.
	fs.mux.HandleFunc("/github.com/!azure!a!d/foo/@latest", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"Version":"v1.0.0","Time":"2026-01-01T00:00:00Z"}`)
	})
	fs.mux.HandleFunc("/github.com/!azure!a!d/foo/@v/v1.0.0.info", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
            "Version":"v1.0.0",
            "Time":"2026-01-01T00:00:00Z",
            "Origin":{"VCS":"git","URL":"https://github.com/AzureAD/foo"}
        }`)
	})

	c := NewClientWithBaseURL(fs.URL(), fs.URL())
	got, err := c.ResolveRepoURL(context.Background(), "github.com/AzureAD/foo")
	if err != nil {
		t.Fatalf("ResolveRepoURL returned %v; want nil", err)
	}
	// The Origin URL is preserved verbatim (per the proxy's response);
	// downstream stamping via signatory/profile.ResolveTarget handles
	// canonical-form lowercasing of github URLs at the entity level.
	if !strings.Contains(got, "github.com") {
		t.Errorf("ResolveRepoURL = %q; want a github.com URL", got)
	}
}
