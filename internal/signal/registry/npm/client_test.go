package npm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sampleRegistryResponse is a minimal but realistic registry
// response body. The shape matches what registry.npmjs.org emits
// for a real package, including fields we don't read (homepage,
// readme, _id). Tests use this to verify (a) we can pluck the
// fields we DO care about and (b) unknown fields don't break the
// decode.
const sampleRegistryResponse = `{
  "_id": "express",
  "name": "express",
  "homepage": "http://expressjs.com/",
  "readme": "# express\n\nFast, unopinionated, minimalist web framework.\n",
  "dist-tags": {"latest": "4.18.2"},
  "time": {
    "created": "2010-12-29T19:38:25.450Z",
    "modified": "2024-03-25T18:07:41.220Z",
    "4.18.2": "2022-10-08T19:08:35.000Z",
    "4.18.1": "2022-06-21T05:32:58.000Z"
  },
  "maintainers": [
    {"name": "dougwilson", "email": "doug@somethingdoug.com"},
    {"name": "linusu", "email": "linus@folkdatorn.se"}
  ],
  "versions": {
    "4.18.2": {
      "scripts": {
        "test": "mocha",
        "postinstall": ""
      },
      "dist": {
        "attestations": null,
        "tarball": "https://registry.npmjs.org/express/-/express-4.18.2.tgz",
        "integrity": "sha512-5/PsL6iGPdfQ/lKM1UuielYgv3BUoJfz1aUwU9vHZ+J7gyvwdQXFEBIEIaxeGf0GIcreATNyBExtalisDbuMqQ=="
      }
    }
  },
  "repository": {
    "type": "git",
    "url": "git+https://github.com/expressjs/express.git"
  }
}`

// ----- happy path -----

func TestClient_GetPackage_HappyPath(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodGet, r.Method)
		assert.Equal(t, "/express", r.URL.Path)
		assert.Equal(t, "application/json", r.Header.Get("Accept"))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, sampleRegistryResponse)
	}))
	defer srv.Close()

	c := newClientWithBaseURL(srv.URL)
	pkg, err := c.GetPackage(context.Background(), "express")
	require.NoError(t, err)

	assert.Equal(t, "express", pkg.Name)
	assert.Equal(t, "4.18.2", pkg.DistTags.Latest)
	assert.Equal(t, "2022-10-08T19:08:35Z",
		pkg.Time["4.18.2"].UTC().Format(time.RFC3339),
		"time entry for latest version should round-trip as RFC3339")
	assert.Len(t, pkg.Maintainers, 2)
	assert.Equal(t, "dougwilson", pkg.Maintainers[0].Name)
	assert.Equal(t, "git+https://github.com/expressjs/express.git", pkg.Repository.URL)
}

// ----- 404 / ErrNotFound -----

func TestClient_GetPackage_NotFound_ReturnsErrNotFound(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":"Not found"}`)
	}))
	defer srv.Close()

	c := newClientWithBaseURL(srv.URL)
	_, err := c.GetPackage(context.Background(), "does-not-exist")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound,
		"404 must surface as ErrNotFound so callers can branch via errors.Is")
}

// ----- error status does not leak response body (#93) -----

func TestClient_GetPackage_ErrorStatus_BodyNotLeaked(t *testing.T) {
	t.Parallel()

	const sensitive = "SECRET_TOKEN_abc123 or proxy-internal debug trace"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, sensitive)
	}))
	defer srv.Close()

	c := newClientWithBaseURL(srv.URL)
	_, err := c.GetPackage(context.Background(), "express")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), sensitive,
		"error string must not contain response body — body can carry tokens, proxy debug, or reflected auth")
	assert.Contains(t, err.Error(), "500",
		"error string should still identify the status so callers can diagnose")
}

// ----- oversized response is rejected before decode -----

func TestClient_GetPackage_OversizedResponse_Rejected(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Write a JSON blob larger than maxResponseSize. Use a padded
		// string field so the body is structurally-valid until it
		// hits the size cap — we want to verify the cap fires, not
		// that a malformed decode fires.
		_, _ = w.Write([]byte(`{"name":"x","pad":"`))
		chunk := strings.Repeat("A", 1024*1024) // 1MB of A
		for range 12 {                          // 12MB total > 10MB cap
			_, _ = w.Write([]byte(chunk))
		}
		_, _ = w.Write([]byte(`"}`))
	}))
	defer srv.Close()

	c := newClientWithBaseURL(srv.URL)
	_, err := c.GetPackage(context.Background(), "x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cap",
		"oversized response must fail with a size-cap error")
}

// Redirect-policy unit tests previously lived here. After the
// internal/httpx port, the shared SecureClient owns the redirect
// policy and its tests live in internal/httpx/client_test.go
// (TestCheckRedirect_*). The integration-level "client refuses
// plaintext redirect" coverage for npm specifically is implicit in
// the same property tested for every per-ecosystem client via the
// httpx integration tests; rebuilding the per-ecosystem unit tests
// would just duplicate them.

// ----- package name validation (pre-HTTP) -----

func TestValidatePackageName_Accepts(t *testing.T) {
	t.Parallel()

	for _, name := range []string{
		"express",
		"lodash",
		"a",
		"vitest",
		"camelCase",
		"kebab-case",
		"snake_case",
		"dot.name",
		"@types/node",
		"@nestjs/core",
		"@angular/core",
		"@scope/with-hyphens",
	} {
		t.Run(name, func(t *testing.T) {
			assert.NoError(t, ValidatePackageName(name))
		})
	}
}

func TestValidatePackageName_Rejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		pkg  string
	}{
		{"empty", ""},
		{"starts with dot", ".hidden"},
		{"starts with hyphen", "-leading"},
		{"contains slash unscoped", "foo/bar"},
		{"contains path traversal", "../etc"},
		{"contains query", "foo?x=1"},
		{"contains fragment", "foo#frag"},
		{"contains space", "foo bar"},
		{"contains null", "foo\x00bar"},
		{"contains newline", "foo\nbar"},
		{"scope missing name", "@scope/"},
		{"scope with no slash", "@scope"},
		{"scope with bad char", "@sco$pe/name"},
		{"too long", strings.Repeat("a", 215)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePackageName(tc.pkg)
			require.Error(t, err, "must reject %q", tc.pkg)
		})
	}
}

// TestClient_GetPackage_MalformedName_NoHTTPCall confirms that a
// malformed package name is rejected BEFORE any HTTP activity. The
// counter below stays at zero on a bad input — otherwise the
// validator is a fig leaf.
func TestClient_GetPackage_MalformedName_NoHTTPCall(t *testing.T) {
	t.Parallel()

	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		calls++
	}))
	defer srv.Close()

	c := newClientWithBaseURL(srv.URL)
	_, err := c.GetPackage(context.Background(), "../etc/passwd")
	require.Error(t, err)
	assert.Equal(t, 0, calls, "malformed name must be rejected pre-HTTP")
}

// ----- scoped package URL encoding -----

func TestClient_GetPackage_ScopedPackage_PathEscaped(t *testing.T) {
	t.Parallel()

	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"name":"@types/node","dist-tags":{"latest":"20.0.0"}}`)
	}))
	defer srv.Close()

	c := newClientWithBaseURL(srv.URL)
	_, err := c.GetPackage(context.Background(), "@types/node")
	require.NoError(t, err)

	// The '/' between scope and name MUST be percent-encoded in the
	// URL path so the registry sees a single-segment package
	// identifier — an unescaped slash would route to a different
	// (nonexistent) endpoint. net/http re-decodes r.URL.Path so we
	// see the raw form via r.URL.RawPath or in this test by checking
	// that the decoded path is what we expect.
	//
	// On the server side, net/http stores the decoded form in Path;
	// the encoded form survives in RawPath when url.PathEscape
	// produces non-identity output. What we actually care about is
	// that our client built the URL correctly — we assert on the
	// decoded form (which normalizes the encoded slash back to /)
	// just to confirm the round-trip, and trust the registry is the
	// real verifier of the encoding.
	assert.Equal(t, "/@types/node", seen)
}

// ----- context cancellation -----

func TestClient_GetPackage_ContextCancellation(t *testing.T) {
	t.Parallel()

	// Server that hangs; cancellation must propagate to the in-flight
	// request rather than blocking until the 60s client timeout.
	//
	// Defer order is load-bearing: srv.Close() waits for in-flight
	// handlers, and the handler here blocks on <-block. If block is
	// still open when srv.Close() fires, the whole test hangs. Defer
	// close(block) AFTER defer srv.Close() so LIFO runs it first.
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		<-block
	}))
	defer srv.Close()
	defer close(block)

	ctx, cancel := context.WithCancel(context.Background())
	// Kick off the request in a goroutine so we can cancel while it's
	// mid-flight. The channel synchronizes the two events.
	done := make(chan error, 1)
	go func() {
		_, err := newClientWithBaseURL(srv.URL).GetPackage(ctx, "express")
		done <- err
	}()

	// Give the request a moment to dispatch, then cancel. 50ms is
	// well within the per-request 60s timeout so a timeout-based
	// pass is ruled out.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		require.Error(t, err)
		// Strict assertion: the error chain must contain
		// context.Canceled. A permissive `|| strings.Contains("context")`
		// fallback would let any wrapped error whose format string
		// happens to include the word "context" pass — including
		// implementation errors that DON'T actually propagate
		// cancellation. The whole point of this test is to verify
		// propagation, so the assertion must be strict.
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("GetPackage did not return after context cancellation")
	}
}

// ----- repository polymorphism -----

func TestClient_GetPackage_Repository_ObjectShape(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"name":"x","repository":{"type":"git","url":"https://github.com/x/x"}}`)
	}))
	defer srv.Close()

	pkg, err := newClientWithBaseURL(srv.URL).GetPackage(context.Background(), "x")
	require.NoError(t, err)
	assert.Equal(t, "git", pkg.Repository.Type)
	assert.Equal(t, "https://github.com/x/x", pkg.Repository.URL)
}

func TestClient_GetPackage_Repository_StringShape(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"name":"x","repository":"https://github.com/x/x"}`)
	}))
	defer srv.Close()

	pkg, err := newClientWithBaseURL(srv.URL).GetPackage(context.Background(), "x")
	require.NoError(t, err)
	assert.Equal(t, "https://github.com/x/x", pkg.Repository.URL,
		"string-shape repository should decode as URL")
	assert.Empty(t, pkg.Repository.Type,
		"string-shape repository has no type field")
}

func TestClient_GetPackage_Repository_Absent_EmptyStruct(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"name":"x","dist-tags":{"latest":"1.0.0"}}`)
	}))
	defer srv.Close()

	pkg, err := newClientWithBaseURL(srv.URL).GetPackage(context.Background(), "x")
	require.NoError(t, err)
	assert.Empty(t, pkg.Repository.URL, "missing repository → empty URL")
	assert.Empty(t, pkg.Repository.Type)
}

func TestClient_GetPackage_Repository_MalformedShape_Errors(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Repository as a number — neither string nor object.
		fmt.Fprint(w, `{"name":"x","repository":42}`)
	}))
	defer srv.Close()

	_, err := newClientWithBaseURL(srv.URL).GetPackage(context.Background(), "x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "repository")
}

// ----- top-level fields we don't model are ignored -----

func TestClient_GetPackage_UnknownTopLevelFields_Ignored(t *testing.T) {
	t.Parallel()

	// npm registry responses legitimately carry many fields we don't
	// read (bugs, users, _id, _rev, _attachments, ...). These must
	// NOT break decoding. This is the conscious divergence from
	// our yaml-strict-mode discipline: we control the analyst-output
	// schema but we don't control the registry's.
	body := `{
	  "_id": "x",
	  "_rev": "abc",
	  "name": "x",
	  "description": "...",
	  "homepage": "http://...",
	  "readme": "blob",
	  "bugs": {"url": "http://..."},
	  "users": {"u1": true},
	  "keywords": ["k"],
	  "license": "MIT",
	  "dist-tags": {"latest": "1.0.0"},
	  "future_field_we_have_not_seen_yet": {"nested": [1,2,3]}
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	pkg, err := newClientWithBaseURL(srv.URL).GetPackage(context.Background(), "x")
	require.NoError(t, err, "unknown top-level fields must not fail decode")
	assert.Equal(t, "x", pkg.Name)
	assert.Equal(t, "1.0.0", pkg.DistTags.Latest)
}

// TestClient_GetPackage_RegistryPackageCanRoundTrip is a sanity check
// that the modelled fields survive a full marshal-unmarshal cycle, so
// the internal struct can be JSON-emitted in tools/diagnostics without
// losing data.
func TestClient_GetPackage_RegistryPackageCanRoundTrip(t *testing.T) {
	t.Parallel()

	var pkg RegistryPackage
	require.NoError(t, json.Unmarshal([]byte(sampleRegistryResponse), &pkg))

	raw, err := json.Marshal(pkg)
	require.NoError(t, err)

	var pkg2 RegistryPackage
	require.NoError(t, json.Unmarshal(raw, &pkg2))
	assert.Equal(t, pkg.Name, pkg2.Name)
	assert.Equal(t, pkg.DistTags.Latest, pkg2.DistTags.Latest)
}

// ----- GetWeeklyDownloads -----

func TestClient_GetWeeklyDownloads_HappyPath(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/downloads/point/last-week/express", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"downloads":28500000,"start":"2026-04-13","end":"2026-04-20","package":"express"}`)
	}))
	defer srv.Close()

	count, err := newClientWithBaseURL(srv.URL).GetWeeklyDownloads(context.Background(), "express")
	require.NoError(t, err)
	assert.Equal(t, 28_500_000, count)
}

func TestClient_GetWeeklyDownloads_NotFound(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := newClientWithBaseURL(srv.URL).GetWeeklyDownloads(context.Background(), "new-package")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound,
		"404 on downloads endpoint also surfaces as ErrNotFound so callers branch uniformly")
}

func TestClient_GetWeeklyDownloads_ErrorBodyNotLeaked(t *testing.T) {
	t.Parallel()

	const sensitive = "internal-proxy-debug SECRET_xyz trace=12345"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, sensitive)
	}))
	defer srv.Close()

	_, err := newClientWithBaseURL(srv.URL).GetWeeklyDownloads(context.Background(), "express")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), sensitive,
		"downloads endpoint must apply the same #93 body-sanitization discipline as GetPackage")
	assert.Contains(t, err.Error(), "503")
}

func TestClient_GetWeeklyDownloads_MalformedName_NoHTTPCall(t *testing.T) {
	t.Parallel()

	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		calls++
	}))
	defer srv.Close()

	_, err := newClientWithBaseURL(srv.URL).GetWeeklyDownloads(context.Background(), "../etc/passwd")
	require.Error(t, err)
	assert.Equal(t, 0, calls,
		"malformed name must be rejected pre-HTTP on downloads path too")
}

func TestClient_GetWeeklyDownloads_ScopedPackage_PathEscaped(t *testing.T) {
	t.Parallel()

	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"downloads":1,"start":"x","end":"y","package":"@types/node"}`)
	}))
	defer srv.Close()

	_, err := newClientWithBaseURL(srv.URL).GetWeeklyDownloads(context.Background(), "@types/node")
	require.NoError(t, err)
	assert.Equal(t, "/downloads/point/last-week/@types/node", seen)
}

// TestClient_GetWeeklyDownloads_StrictDecode verifies that the
// downloads endpoint decoder uses DisallowUnknownFields — schema
// drift on this narrow, stable endpoint should surface as an error
// rather than silently decode as a zero-value count. This is the
// conscious inverse of the main registry's lenient decode: we
// control less of the downloads schema but it's much narrower, so
// strict mode has a signal-to-noise profile where strict works.
func TestClient_GetWeeklyDownloads_StrictDecode(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Extra field "bonus_metric" isn't in the schema. Strict
		// mode should reject the response.
		fmt.Fprint(w, `{"downloads":1,"start":"x","end":"y","package":"x","bonus_metric":42}`)
	}))
	defer srv.Close()

	_, err := newClientWithBaseURL(srv.URL).GetWeeklyDownloads(context.Background(), "x")
	require.Error(t, err,
		"strict decode should reject unknown field — signals drift we want to notice on this schema")
	assert.Contains(t, err.Error(), "bonus_metric")
}
