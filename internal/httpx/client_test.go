// Tests for the SecureClient defensive defaults. Each test pins one
// property the per-ecosystem signal collectors previously re-derived
// independently (HTTPS-only redirects, drain-on-error, bounded reads,
// configurable not-found-status set, etc.). The test surface is the
// behavior contract — when we later port npm / pypi / cargo / github
// / forgejo / gitlab / gopublish / maven / openssf / adoption onto
// SecureClient, these tests are the safety net that catches any
// regression in the shared defenses.
//
// White-box (package httpx, not httpx_test) to match the codebase
// convention used by npm/, store/, etc. Test helpers and types live
// alongside the impl in the same package.
package httpx

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sample is the decode target for GetJSON tests. Single string field
// keeps the JSON small and the assertion obvious.
type sample struct {
	Name string `json:"name"`
}

// sentinelRateLimit is the test stand-in for github.RateLimitError —
// proves a StatusInterceptor can return a caller-defined typed error
// that survives the boundary intact (errors.As recovers it).
type sentinelRateLimit struct{}

func (*sentinelRateLimit) Error() string { return "rate limit hit" }

// newSrv builds a hermetic httptest server and registers its
// teardown on the test. Every test that uses it gets its own port +
// own handler; no shared state across tests.
func newSrv(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// --------------------------------------------------------------------
// Get: happy path + error classification
// --------------------------------------------------------------------

func TestSecureClient_Get_HappyPath(t *testing.T) {
	t.Parallel()
	srv := newSrv(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/some/path", r.URL.Path)
		assert.Equal(t, http.MethodGet, r.Method)
		_, _ = w.Write([]byte(`hello`))
	})

	c := NewSecureClient(WithBaseURL(srv.URL))
	body, err := c.Get(t.Context(), "/some/path")
	require.NoError(t, err)
	assert.Equal(t, []byte("hello"), body)
}

func TestSecureClient_Get_EmptyBodyOK(t *testing.T) {
	t.Parallel()
	srv := newSrv(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	c := NewSecureClient(WithBaseURL(srv.URL))
	body, err := c.Get(t.Context(), "/")
	require.NoError(t, err)
	assert.Empty(t, body)
}

func TestSecureClient_Get_NotFound(t *testing.T) {
	t.Parallel()
	srv := newSrv(t, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})

	c := NewSecureClient(WithBaseURL(srv.URL))
	body, err := c.Get(t.Context(), "/missing")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotFound),
		"expected errors.Is(err, ErrNotFound); got: %v", err)
	assert.Nil(t, body)
}

func TestSecureClient_Get_CustomNotFoundStatuses(t *testing.T) {
	t.Parallel()
	// gopublish maps 410 Gone (module retracted) to ErrNotFound —
	// validate that the not-found set is configurable.
	srv := newSrv(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusGone)
	})

	c := NewSecureClient(
		WithBaseURL(srv.URL),
		WithNotFoundStatuses(http.StatusNotFound, http.StatusGone),
	)
	_, err := c.Get(t.Context(), "/retracted")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotFound),
		"410 with custom not-found set should map to ErrNotFound; got: %v", err)
}

func TestSecureClient_Get_OtherNon2xx_NoBodyInError(t *testing.T) {
	t.Parallel()
	// Issue #93 lesson: response body content must never reach the
	// error string. Bodies are attacker-influenceable; they can carry
	// secrets, internal IPs, or controlled tokens.
	//
	// We construct the body from three distinctive substrings and
	// assert NONE of them appear in the error. A partial leak — say,
	// a refactor that included the first 20 bytes of the body in the
	// error "for diagnostics" — would defeat a NotContains check on
	// the full-body string (substring semantics: the partial leak
	// doesn't contain the full pattern). Asserting against three
	// distinct prefixes/keys catches the partial-leak case.
	const (
		debugPrefix = "DEBUG-leak-prefix:"
		secretKey   = "SecretTokenName"
		secretValue = "abc123xyz789payload"
	)
	const secretBody = debugPrefix + " " + secretKey + "=" + secretValue
	srv := newSrv(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, secretBody)
	})

	c := NewSecureClient(WithBaseURL(srv.URL))
	_, err := c.Get(t.Context(), "/explode")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "500", "status code should be in error string")
	assert.NotContains(t, err.Error(), secretBody,
		"#93: full body must not leak into error string")
	assert.NotContains(t, err.Error(), debugPrefix,
		"#93: even a body prefix must not reach the error string")
	assert.NotContains(t, err.Error(), secretKey,
		"#93: token-shaped substring must not reach the error string")
	assert.NotContains(t, err.Error(), secretValue,
		"#93: any portion of an attacker-controlled body must stay sealed")
	assert.False(t, errors.Is(err, ErrNotFound),
		"500 should not be classified as not-found")
}

// --------------------------------------------------------------------
// Get: body size caps
// --------------------------------------------------------------------

func TestSecureClient_Get_DefaultBodyCap(t *testing.T) {
	t.Parallel()
	// Default cap is 10 MiB (matching the per-ecosystem clients'
	// existing constant). Serve 10 MiB + 1 to trip it.
	big := make([]byte, 10*1024*1024+1)
	srv := newSrv(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(big)
	})

	c := NewSecureClient(WithBaseURL(srv.URL))
	_, err := c.Get(t.Context(), "/big")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrResponseTooLarge),
		"expected ErrResponseTooLarge; got: %v", err)
}

func TestSecureClient_Get_ClientBodyCap(t *testing.T) {
	t.Parallel()
	srv := newSrv(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("0123456789ABCDEF")) // 16 bytes
	})

	c := NewSecureClient(
		WithBaseURL(srv.URL),
		WithMaxBytes(8),
	)
	_, err := c.Get(t.Context(), "/")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrResponseTooLarge),
		"client-level cap should trip; got: %v", err)
}

func TestSecureClient_Get_PerRequestBodyCap(t *testing.T) {
	t.Parallel()
	// npm's downloads endpoint caps at 64 KiB even though the
	// registry endpoint uses 10 MiB. Per-request cap overrides
	// client default.
	srv := newSrv(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("0123456789ABCDEF"))
	})

	c := NewSecureClient(WithBaseURL(srv.URL)) // default 10 MiB
	_, err := c.Get(t.Context(), "/", WithRequestMaxBytes(8))
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrResponseTooLarge),
		"per-request cap should trip; got: %v", err)
}

func TestSecureClient_Get_PerRequestCap_BelowDefault_PassesIfFits(t *testing.T) {
	t.Parallel()
	srv := newSrv(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("12345"))
	})

	c := NewSecureClient(WithBaseURL(srv.URL))
	body, err := c.Get(t.Context(), "/", WithRequestMaxBytes(8))
	require.NoError(t, err)
	assert.Equal(t, []byte("12345"), body)
}

// --------------------------------------------------------------------
// Get: headers
// --------------------------------------------------------------------

func TestSecureClient_Get_DefaultUserAgent(t *testing.T) {
	t.Parallel()
	var seenUA string
	srv := newSrv(t, func(w http.ResponseWriter, r *http.Request) {
		seenUA = r.Header.Get("User-Agent")
	})

	c := NewSecureClient(WithBaseURL(srv.URL))
	_, err := c.Get(t.Context(), "/")
	require.NoError(t, err)
	assert.Equal(t, "signatory/0.1", seenUA,
		"default User-Agent matches the per-ecosystem clients' existing string")
}

func TestSecureClient_Get_UserAgentOverride(t *testing.T) {
	t.Parallel()
	var seenUA string
	srv := newSrv(t, func(w http.ResponseWriter, r *http.Request) {
		seenUA = r.Header.Get("User-Agent")
	})

	c := NewSecureClient(
		WithBaseURL(srv.URL),
		WithUserAgent("signatory/0.1 (https://github.com/sarahmaeve/signatory)"),
	)
	_, err := c.Get(t.Context(), "/")
	require.NoError(t, err)
	assert.Equal(t, "signatory/0.1 (https://github.com/sarahmaeve/signatory)", seenUA)
}

func TestSecureClient_Get_HeaderOption(t *testing.T) {
	t.Parallel()
	var seenAccept, seenAuth string
	srv := newSrv(t, func(w http.ResponseWriter, r *http.Request) {
		seenAccept = r.Header.Get("Accept")
		seenAuth = r.Header.Get("Authorization")
	})

	c := NewSecureClient(WithBaseURL(srv.URL))
	_, err := c.Get(t.Context(), "/",
		WithHeader("Accept", "application/vnd.github.v3+json"),
		WithHeader("Authorization", "Bearer test-token"),
	)
	require.NoError(t, err)
	assert.Equal(t, "application/vnd.github.v3+json", seenAccept)
	assert.Equal(t, "Bearer test-token", seenAuth)
}

// --------------------------------------------------------------------
// Get: context cancellation
// --------------------------------------------------------------------

func TestSecureClient_Get_ContextCancel(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	srv := newSrv(t, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	})

	c := NewSecureClient(WithBaseURL(srv.URL))
	ctx, cancel := context.WithCancel(t.Context())
	errCh := make(chan error, 1)
	go func() {
		_, err := c.Get(ctx, "/slow")
		errCh <- err
	}()
	<-started
	cancel()

	select {
	case err := <-errCh:
		require.Error(t, err)
		assert.True(t, errors.Is(err, context.Canceled),
			"expected context.Canceled in chain; got: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("Get did not return within 2s after context cancellation")
	}
}

// --------------------------------------------------------------------
// Get: status interceptor (github RateLimitError analog)
// --------------------------------------------------------------------

func TestSecureClient_Get_StatusInterceptor_ShortCircuit(t *testing.T) {
	t.Parallel()
	srv := newSrv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Reset", "9999999999")
		w.WriteHeader(http.StatusForbidden)
	})

	customErr := &sentinelRateLimit{}
	c := NewSecureClient(WithBaseURL(srv.URL))
	_, err := c.Get(t.Context(), "/",
		WithStatusInterceptor(func(resp *http.Response) error {
			if resp.StatusCode == http.StatusForbidden ||
				resp.StatusCode == http.StatusTooManyRequests {
				// Interceptor sees the status AND headers so caller can
				// translate (e.g., parse X-RateLimit-Reset).
				assert.NotEmpty(t, resp.Header.Get("X-RateLimit-Reset"))
				return customErr
			}
			return nil
		}),
	)
	require.Error(t, err)

	var got *sentinelRateLimit
	assert.True(t, errors.As(err, &got),
		"interceptor's typed error must survive; got: %T (%v)", err, err)
}

func TestSecureClient_Get_StatusInterceptor_FallThrough(t *testing.T) {
	t.Parallel()
	srv := newSrv(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	interceptorCalled := false
	c := NewSecureClient(WithBaseURL(srv.URL))
	_, err := c.Get(t.Context(), "/",
		WithStatusInterceptor(func(resp *http.Response) error {
			interceptorCalled = true
			return nil // fall through to default classification
		}),
	)
	require.Error(t, err)
	assert.True(t, interceptorCalled, "interceptor should run on non-2xx")
	assert.True(t, errors.Is(err, ErrNotFound),
		"after fall-through, default 404 classification must run; got: %v", err)
}

// --------------------------------------------------------------------
// Redirect policy
// --------------------------------------------------------------------

func TestSecureClient_Get_RefusesNonHTTPSRedirect(t *testing.T) {
	t.Parallel()
	// Issue #89 lesson: scheme downgrade on redirect has no
	// legitimate use case on the public registries / forges
	// signatory talks to. Refuse loudly rather than follow silently.
	srv := newSrv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "http://example.com/elsewhere")
		w.WriteHeader(http.StatusFound)
	})

	c := NewSecureClient(WithBaseURL(srv.URL))
	_, err := c.Get(t.Context(), "/start")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "non-HTTPS",
		"refusal reason should be visible; got: %v", err)
}

// --------------------------------------------------------------------
// GetJSON
// --------------------------------------------------------------------

func TestSecureClient_GetJSON_HappyPath(t *testing.T) {
	t.Parallel()
	srv := newSrv(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"name":"alice"}`)
	})

	c := NewSecureClient(WithBaseURL(srv.URL))
	var s sample
	require.NoError(t, c.GetJSON(t.Context(), "/", &s))
	assert.Equal(t, "alice", s.Name)
}

func TestSecureClient_GetJSON_Strict_RejectsUnknown(t *testing.T) {
	t.Parallel()
	// npm.GetWeeklyDownloads + pypi attestation publisher fields use
	// strict-decode for stable / load-bearing schemas. The unknown-
	// field rejection is the property to pin.
	srv := newSrv(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"name":"alice","extra":"field"}`)
	})

	c := NewSecureClient(WithBaseURL(srv.URL))
	var s sample
	err := c.GetJSON(t.Context(), "/", &s, WithStrictJSONDecode())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown field",
		"strict decode should reject unknown fields; got: %v", err)
}

func TestSecureClient_GetJSON_Lax_TolerantOfUnknown(t *testing.T) {
	t.Parallel()
	// npm's GetPackage uses lax decode because the registry emits
	// dozens of unmodeled fields. Drift on those fields must not fail
	// the call.
	srv := newSrv(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"name":"alice","extra":"field","_id":"X"}`)
	})

	c := NewSecureClient(WithBaseURL(srv.URL))
	var s sample
	require.NoError(t, c.GetJSON(t.Context(), "/", &s))
	assert.Equal(t, "alice", s.Name)
}

func TestSecureClient_GetJSON_MalformedReturnsError(t *testing.T) {
	t.Parallel()
	srv := newSrv(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{not even json}`)
	})

	c := NewSecureClient(WithBaseURL(srv.URL))
	var s sample
	err := c.GetJSON(t.Context(), "/", &s)
	require.Error(t, err)
}

func TestSecureClient_GetJSON_NotFoundPropagates(t *testing.T) {
	t.Parallel()
	// 404 must surface as ErrNotFound, not as a JSON-decode error on
	// the empty/HTML 404 body. The classification happens BEFORE the
	// decode is even attempted.
	srv := newSrv(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `<html>not found</html>`)
	})

	c := NewSecureClient(WithBaseURL(srv.URL))
	var s sample
	err := c.GetJSON(t.Context(), "/", &s)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotFound),
		"404 must propagate as ErrNotFound rather than decode error; got: %v", err)
}

// --------------------------------------------------------------------
// GetWithResponse (github's pagination via Link header)
// --------------------------------------------------------------------

func TestSecureClient_GetWithResponse_ReturnsBodyAndHeaders(t *testing.T) {
	t.Parallel()
	srv := newSrv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Link", `<https://example/?page=2>; rel="next", <https://example/?page=467>; rel="last"`)
		_, _ = io.WriteString(w, `{"items":[]}`)
	})

	c := NewSecureClient(WithBaseURL(srv.URL))
	body, headers, status, err := c.GetWithResponse(t.Context(), "/commits?per_page=1")
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, []byte(`{"items":[]}`), body)
	assert.Contains(t, headers.Get("Link"), `rel="last"`,
		"Link header must round-trip for github pagination")
}

func TestSecureClient_GetWithResponse_NotFoundCarriesStatus(t *testing.T) {
	t.Parallel()
	srv := newSrv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Reset", "9999999999")
		w.WriteHeader(http.StatusNotFound)
	})

	c := NewSecureClient(WithBaseURL(srv.URL))
	body, headers, status, err := c.GetWithResponse(t.Context(), "/missing")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotFound))
	assert.Equal(t, http.StatusNotFound, status,
		"status returned alongside error for diagnostic")
	assert.Empty(t, body)
	// Headers preserved on error so callers can inspect rate-limit etc.
	assert.NotEmpty(t, headers.Get("X-RateLimit-Reset"))
}

// --------------------------------------------------------------------
// Head (maven's Last-Modified + signature checks)
// --------------------------------------------------------------------

func TestSecureClient_Head_HappyPath(t *testing.T) {
	t.Parallel()
	srv := newSrv(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodHead, r.Method,
			"Head must issue an HTTP HEAD")
		w.Header().Set("Last-Modified", "Mon, 02 Jan 2006 15:04:05 GMT")
		w.WriteHeader(http.StatusOK)
	})

	c := NewSecureClient(WithBaseURL(srv.URL))
	headers, status, err := c.Head(t.Context(), "/some.jar")
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, "Mon, 02 Jan 2006 15:04:05 GMT", headers.Get("Last-Modified"))
}

func TestSecureClient_Head_NotFound(t *testing.T) {
	t.Parallel()
	// maven.CheckSignature does Head; 404 maps to "no signature
	// present" via the caller doing errors.Is(err, ErrNotFound).
	srv := newSrv(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	c := NewSecureClient(WithBaseURL(srv.URL))
	_, status, err := c.Head(t.Context(), "/missing.jar.asc")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotFound))
	assert.Equal(t, http.StatusNotFound, status,
		"status code returned alongside error for diagnostic / control flow")
}

// --------------------------------------------------------------------
// Redirect policy — direct unit tests
//
// The integration test above (RefusesNonHTTPSRedirect) covers the
// httpx → http.Client → checkRedirect end-to-end path. The unit
// tests below pin each branch of the policy directly so failures
// localize to the rule rather than the wiring, and so the 10-hop
// cap is tested without needing an https httptest chain (which
// would require TLS skip-verify).
// --------------------------------------------------------------------

func TestCheckRedirect_RefusesNonHTTPSSchemes(t *testing.T) {
	t.Parallel()

	httpsVia, err := url.Parse("https://registry.npmjs.org/start")
	require.NoError(t, err)
	via := []*http.Request{{URL: httpsVia}}

	tests := []struct {
		name   string
		target string
	}{
		{"http scheme downgrade", "http://registry.npmjs.org/elsewhere"},
		{"attacker-host http redirect", "http://attacker.example/x"},
		{"file scheme", "file:///etc/passwd"},
		{"javascript scheme", "javascript:alert(1)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			target, err := url.Parse(tc.target)
			require.NoError(t, err)
			next := &http.Request{URL: target}
			assert.Error(t, checkRedirect(next, via),
				"must refuse redirect to %q", tc.target)
		})
	}
}

func TestCheckRedirect_AllowsHTTPS(t *testing.T) {
	t.Parallel()

	viaURL, err := url.Parse("https://registry.npmjs.org/old-path")
	require.NoError(t, err)
	via := []*http.Request{{URL: viaURL}}

	nextURL, err := url.Parse("https://registry.npmjs.org/new-path")
	require.NoError(t, err)
	next := &http.Request{URL: nextURL}

	assert.NoError(t, checkRedirect(next, via))
}

func TestCheckRedirect_BoundsChainAt10(t *testing.T) {
	t.Parallel()

	viaURL, err := url.Parse("https://registry.npmjs.org/")
	require.NoError(t, err)

	via := make([]*http.Request, 10)
	for i := range via {
		via[i] = &http.Request{URL: viaURL}
	}
	nextURL, err := url.Parse("https://registry.npmjs.org/next")
	require.NoError(t, err)
	next := &http.Request{URL: nextURL}

	cerr := checkRedirect(next, via)
	require.Error(t, cerr)
	assert.Contains(t, cerr.Error(), "redirects",
		"10-hop ceiling should be reported as a redirect-count error")
}

// --------------------------------------------------------------------
// Construction options
// --------------------------------------------------------------------

// TestNonPositiveOptionsKeepDefaults pins the guard that non-positive
// values for WithTimeout / WithMaxBytes / WithRequestMaxBytes do NOT
// silently disable the defense. Without this guard,
// WithTimeout(0) → http.Client.Timeout=0 → no timeout (a defense
// disable) and WithMaxBytes(0) → every non-empty response fails with
// ErrResponseTooLarge (a fail-closed correctness break). Both are
// undocumented surprises; the guard normalizes them to the default.
//
// Doubles as the regression guard for "NewSecureClient wires the
// 60s default timeout" — without this assertion, a refactor changing
// the default to 0 would not fail any test.
func TestNonPositiveOptionsKeepDefaults(t *testing.T) {
	t.Parallel()

	c := NewSecureClient(WithTimeout(-1), WithMaxBytes(0))
	assert.Equal(t, defaultTimeout, c.httpClient.Timeout,
		"WithTimeout(<=0) must not disable the timeout")
	assert.Equal(t, int64(defaultMaxBytes), c.maxBytes,
		"WithMaxBytes(<=0) must not change the cap")

	// Per-request override: WithRequestMaxBytes(<=0) must not
	// silently fall through to the client default in a way that
	// surprises callers. The chosen semantic is "non-positive is
	// ignored," same as the client-level guards.
	rcfg := parseRequestOpts([]RequestOption{WithRequestMaxBytes(0)})
	assert.Equal(t, int64(0), rcfg.maxBytes,
		"WithRequestMaxBytes(<=0) must not set the per-request cap (treated as unset, client default applies)")
	rcfg = parseRequestOpts([]RequestOption{WithRequestMaxBytes(-5)})
	assert.Equal(t, int64(0), rcfg.maxBytes,
		"WithRequestMaxBytes(<=0) must not set the per-request cap")
}

func TestSecureClient_WithTimeout(t *testing.T) {
	t.Parallel()
	// Server blocks indefinitely. A 50ms client-side timeout must
	// fail the request fast, well under the test's 2s budget.
	srv := newSrv(t, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})

	c := NewSecureClient(
		WithBaseURL(srv.URL),
		WithTimeout(50*time.Millisecond),
	)
	start := time.Now()
	_, err := c.Get(t.Context(), "/")
	require.Error(t, err)
	assert.Less(t, time.Since(start), 2*time.Second,
		"timeout did not fire promptly; got: %v", err)
}
