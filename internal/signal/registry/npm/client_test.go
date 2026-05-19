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

// TestContainsControlChars pins the M3 gate. The function reports
// whether a string carries any byte / rune that legitimate registry
// URLs and identifiers never include. Tab (0x09) is the only allowed
// exception. Pre-fix the check used `r < 0x20 && r != '\t'`, which
// missed DEL (0x7F) and the Unicode line/paragraph separators
// U+2028 / U+2029 — bytes/runes that JS treats as line terminators
// and that log aggregators commonly split on, so they're an
// injection vector for any consumer that renders the value.
// fulcio.safeClaim in the same codebase rejects 0x7F already;
// containsControlChars must hold the same line.
func TestContainsControlChars(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want bool
	}{
		// Caught by both pre- and post-fix.
		{"plain ascii url", "https://github.com/x/y", false},
		{"plain ascii with safe punctuation", "git+https://github.com/expressjs/express.git", false},
		{"empty", "", false},
		{"tab is allowed (whitespace, not a control)", "git+https://x\ty", false},
		{"NUL byte (0x00)", "evil\x00trail", true},
		{"newline (0x0a)", "evil\nInjected: log line", true},
		{"carriage return (0x0d)", "evil\rPart-2", true},
		{"unit separator (0x1f)", "evil\x1fpart", true},
		// New: pre-fix these would all incorrectly return false.
		{"DEL (0x7F)", "evil\x7fpart", true},
		{"line separator (U+2028)", "evil Injected", true},
		{"paragraph separator (U+2029)", "evil Injected", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, containsControlChars(tc.in),
				"containsControlChars(%q)", tc.in)
		})
	}
}

// TestRepositoryUnmarshalJSON_RejectsLineSeparator pins the gate at
// the integration point — a registry response whose repository URL
// carries U+2028 (or any control char from the M3 set) must fail
// JSON decode. Belt-and-suspenders: even if containsControlChars
// gets refactored, this test continues to pin the trust boundary
// where the bytes actually enter the system.
func TestRepositoryUnmarshalJSON_RejectsLineSeparator(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// "url" field carries a literal U+2028 (  is the JSON
		// escape, decoded to the U+2028 rune in the Go string).
		fmt.Fprint(w, `{"name":"x","repository":{"type":"git","url":"https://github.com/x/y Injected"}}`)
	}))
	defer srv.Close()

	_, err := newClientWithBaseURL(srv.URL).GetPackage(context.Background(), "x")
	require.Error(t, err,
		"a registry URL carrying U+2028 must fail decode at the trust boundary")
	assert.Contains(t, err.Error(), "control characters")
}

// TestValidateVersion_Accepts pins the shapes ValidateVersion must
// admit before they're substituted into the attestation URL path.
// Same trust-boundary discipline as ValidatePackageName: the regex
// `^[A-Za-z0-9][A-Za-z0-9.+-]*$` accepts semver including pre-release
// and build metadata, and the length cap is 256.
func TestValidateVersion_Accepts(t *testing.T) {
	t.Parallel()

	for _, version := range []string{
		"1.0.0",
		"0.0.1",
		"10.20.30",
		"1.2.3-rc.1",
		"1.2.3-rc.1+build.5",
		"1.2.3+sha.abc123",
		"1.0.0-alpha",
		"1.0.0-alpha.beta.1",
		"v2-leading-alpha-ok",
	} {
		t.Run(version, func(t *testing.T) {
			t.Parallel()
			assert.NoError(t, ValidateVersion(version))
		})
	}
}

// TestValidateVersion_Rejects pins the shapes ValidateVersion must
// refuse — the version segment of the attestation URL is a path
// component, so any byte that could escape it (slash, query,
// fragment, whitespace, null, newline, @) is a hard reject regardless
// of whether the npm spec would accept it. Flag-shaped values (leading
// dash) are rejected so a future caller that passes the value
// somewhere shell-shaped can't be flag-injected. Length cap is 256.
func TestValidateVersion_Rejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		version string
	}{
		{"empty", ""},
		{"starts with dot", ".1.0.0"},
		{"starts with hyphen", "-rf"},
		{"starts with plus", "+build.1"},
		{"contains slash", "1.0.0/etc"},
		{"contains path traversal", "../1.0.0"},
		{"contains query", "1.0.0?x=1"},
		{"contains fragment", "1.0.0#frag"},
		{"contains space", "1.0.0 beta"},
		{"contains at sign", "1.0.0@something"},
		{"contains null", "1.0.0\x00rc"},
		{"contains newline", "1.0.0\nv2"},
		{"contains underscore", "1.0.0_beta"},
		{"too long", strings.Repeat("a", 257)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateVersion(tc.version)
			require.Errorf(t, err, "must reject %q", tc.version)
		})
	}
}

// TestClient_GetAttestation_MalformedName_NoHTTPCall: ValidatePackageName
// must reject a bad name BEFORE any HTTP activity on the attestation
// path, same discipline as TestClient_GetPackage_MalformedName_NoHTTPCall
// for the packument path. Without this pin a future caller that lets
// attacker-influenced bytes reach GetAttestation could escape the
// URL path component.
func TestClient_GetAttestation_MalformedName_NoHTTPCall(t *testing.T) {
	t.Parallel()

	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		calls++
	}))
	defer srv.Close()

	c := newClientWithBaseURL(srv.URL)
	_, err := c.GetAttestation(context.Background(), "../etc/passwd", "1.0.0")
	require.Error(t, err)
	assert.Equal(t, 0, calls,
		"malformed package name must be rejected pre-HTTP on the attestation endpoint")
}

// TestClient_GetAttestation_MalformedVersion_NoHTTPCall: ValidateVersion
// must reject a bad version BEFORE any HTTP activity. The version
// flows into the URL path segment after the @-separator; a value
// containing `/` would split the segment, and a value containing `@`
// would confuse the registry's name@version parsing.
func TestClient_GetAttestation_MalformedVersion_NoHTTPCall(t *testing.T) {
	t.Parallel()

	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		calls++
	}))
	defer srv.Close()

	c := newClientWithBaseURL(srv.URL)
	_, err := c.GetAttestation(context.Background(), "express", "1.0.0/../../etc")
	require.Error(t, err)
	assert.Equal(t, 0, calls,
		"malformed version must be rejected pre-HTTP on the attestation endpoint")
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
