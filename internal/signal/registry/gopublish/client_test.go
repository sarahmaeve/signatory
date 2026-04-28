package gopublish

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestValidateModulePath_OK exercises module-path shapes the proxy
// accepts. A "valid" path here is one we'll let through to URL
// construction; the proxy itself decides whether it actually has
// the module.
func TestValidateModulePath_OK(t *testing.T) {
	t.Parallel()
	cases := []string{
		"github.com/sarahmaeve/signatory",
		"golang.org/x/sync",
		"k8s.io/client-go",
		"gopkg.in/yaml.v3",
		"example.com/foo/bar/baz",
		// Major-version subpath: legitimate Go convention.
		"github.com/owner/repo/v2",
	}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			t.Parallel()
			if err := ValidateModulePath(c); err != nil {
				t.Fatalf("ValidateModulePath(%q) returned %v; want nil", c, err)
			}
		})
	}
}

// TestValidateModulePath_Reject covers the cases that have no
// business reaching the proxy: empty, leading/trailing slash,
// embedded null, double-slash, parent-traversal, query/fragment
// metacharacters. Each is an injection or grammar violation a
// caller could otherwise smuggle into a URL path.
func TestValidateModulePath_Reject(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, in string }{
		{"empty", ""},
		{"leading-slash", "/github.com/foo/bar"},
		{"trailing-slash", "github.com/foo/bar/"},
		{"double-slash", "github.com//foo"},
		{"embedded-null", "github.com/foo\x00bar"},
		{"parent-traversal", "github.com/../etc/passwd"},
		{"contains-newline", "github.com/foo\nbar"},
		{"contains-space", "github.com/foo bar"},
		{"contains-question", "github.com/foo?bar"},
		{"contains-fragment", "github.com/foo#bar"},
		{"single-segment", "github.com"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if err := ValidateModulePath(c.in); err == nil {
				t.Fatalf("ValidateModulePath(%q) returned nil; want error", c.in)
			}
		})
	}
}

// TestEncodeModulePath verifies the `!`-prefix lowercasing rule the
// Go proxy uses to disambiguate case in module paths on
// case-insensitive filesystems. Spec:
// https://golang.org/ref/mod#goproxy-protocol — "the case of letters
// in the module path or version is encoded by replacing every
// uppercase letter with an exclamation mark followed by the
// corresponding lower-case letter."
func TestEncodeModulePath(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"github.com/foo/bar", "github.com/foo/bar"},
		{"github.com/Foo/Bar", "github.com/!foo/!bar"},
		{"github.com/AzureAD/foo", "github.com/!azure!a!d/foo"},
		{"golang.org/x/sync", "golang.org/x/sync"},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			t.Parallel()
			got := encodeModulePath(c.in)
			if got != c.want {
				t.Fatalf("encodeModulePath(%q) = %q; want %q", c.in, got, c.want)
			}
		})
	}
}

// fakeServer is a minimal multiplexer that serves both proxy and
// sum endpoints from a single httptest.Server, mirroring the npm
// client tests' pattern. Per-request handlers are wired by exact
// path so a missing route returns 404 (the real "module not in
// the proxy" shape).
type fakeServer struct {
	t       *testing.T
	mux     *http.ServeMux
	srv     *httptest.Server
	handler func(w http.ResponseWriter, r *http.Request)
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	fs := &fakeServer{t: t, mux: http.NewServeMux()}
	fs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fs.handler != nil {
			fs.handler(w, r)
			return
		}
		fs.mux.ServeHTTP(w, r)
	}))
	t.Cleanup(fs.srv.Close)
	return fs
}

func (fs *fakeServer) URL() string { return fs.srv.URL }

// TestGetLatest verifies the @latest path: the proxy returns a
// JSON {Version, Time} block; the client returns those two fields
// with Time parsed into a real time.Time.
func TestGetLatest(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	fs.mux.HandleFunc("/golang.org/x/sync/@latest", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"Version":"v0.20.0","Time":"2026-04-15T10:00:00Z"}`)
	})

	c := NewClientWithBaseURL(fs.URL(), fs.URL())
	got, err := c.GetLatest(context.Background(), "golang.org/x/sync")
	if err != nil {
		t.Fatalf("GetLatest returned %v; want nil", err)
	}
	if got.Version != "v0.20.0" {
		t.Errorf("Version = %q; want v0.20.0", got.Version)
	}
	wantTime := time.Date(2026, 4, 15, 10, 0, 0, 0, time.UTC)
	if !got.Time.Equal(wantTime) {
		t.Errorf("Time = %v; want %v", got.Time, wantTime)
	}
}

// TestGetLatest_NotFound covers the 404 case: the module simply
// isn't on the proxy. Returns ErrNotFound (wrapped) so callers
// can branch via errors.Is.
func TestGetLatest_NotFound(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	// No handlers registered — every path 404s.
	c := NewClientWithBaseURL(fs.URL(), fs.URL())
	_, err := c.GetLatest(context.Background(), "example.com/does/not/exist")
	if err == nil {
		t.Fatalf("GetLatest returned nil error; want ErrNotFound")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v; want errors.Is ErrNotFound", err)
	}
}

// TestGetLatest_AppliesCaseEncoding asserts the URL the client
// actually requests is bang-encoded. Without this, an uppercase
// owner like `AzureAD` would 404 against the real proxy because
// the proxy's filesystem is case-sensitive over the bang-encoded
// name. Plain owner names already exercise the no-op path of the
// encoder; this test specifically holds the encoder accountable
// at the URL boundary.
func TestGetLatest_AppliesCaseEncoding(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	var sawPath string
	fs.handler = func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"Version":"v1.0.0","Time":"2026-01-01T00:00:00Z"}`)
	}
	c := NewClientWithBaseURL(fs.URL(), fs.URL())
	if _, err := c.GetLatest(context.Background(), "github.com/AzureAD/foo"); err != nil {
		t.Fatalf("GetLatest returned %v; want nil", err)
	}
	want := "/github.com/!azure!a!d/foo/@latest"
	if sawPath != want {
		t.Fatalf("server saw path %q; want %q", sawPath, want)
	}
}

// TestGetVersionList verifies the @v/list path: the proxy returns
// newline-delimited version strings (no trailing field, no JSON);
// the client returns them as a slice with empty lines filtered.
func TestGetVersionList(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	fs.mux.HandleFunc("/golang.org/x/sync/@v/list", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "v0.18.0\nv0.19.0\nv0.20.0\n")
	})
	c := NewClientWithBaseURL(fs.URL(), fs.URL())
	versions, err := c.GetVersionList(context.Background(), "golang.org/x/sync")
	if err != nil {
		t.Fatalf("GetVersionList returned %v; want nil", err)
	}
	want := []string{"v0.18.0", "v0.19.0", "v0.20.0"}
	if len(versions) != len(want) {
		t.Fatalf("got %d versions; want %d (got=%v)", len(versions), len(want), versions)
	}
	for i := range want {
		if versions[i] != want[i] {
			t.Errorf("versions[%d] = %q; want %q", i, versions[i], want[i])
		}
	}
}

// TestGetVersionInfo verifies the .info path with origin metadata.
// The proxy emits {Version, Time, Origin{...}} for modules
// published with go ≥ 1.20; we read all three.
func TestGetVersionInfo(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	fs.mux.HandleFunc("/golang.org/x/sync/@v/v0.20.0.info", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"Version": "v0.20.0",
			"Time": "2026-04-15T10:00:00Z",
			"Origin": {"VCS":"git","URL":"https://go.googlesource.com/sync","Ref":"refs/tags/v0.20.0","Hash":"ec11c4a93de22cde2abe2bf74d70791033c2464c"}
		}`)
	})
	c := NewClientWithBaseURL(fs.URL(), fs.URL())
	info, err := c.GetVersionInfo(context.Background(), "golang.org/x/sync", "v0.20.0")
	if err != nil {
		t.Fatalf("GetVersionInfo returned %v; want nil", err)
	}
	if info.Version != "v0.20.0" {
		t.Errorf("Version = %q; want v0.20.0", info.Version)
	}
	if info.Origin.VCS != "git" {
		t.Errorf("Origin.VCS = %q; want git", info.Origin.VCS)
	}
	if info.Origin.URL != "https://go.googlesource.com/sync" {
		t.Errorf("Origin.URL = %q; want go.googlesource.com/sync", info.Origin.URL)
	}
	if info.Origin.Hash != "ec11c4a93de22cde2abe2bf74d70791033c2464c" {
		t.Errorf("Origin.Hash unexpected: %q", info.Origin.Hash)
	}
}

// TestLookupTransparency verifies the sum.golang.org /lookup
// endpoint: a 200 response with a multi-line transparency-log
// record means the module/version is in the global Merkle log.
// The body shape is documented at
// https://sum.golang.org/lookup/<module>@<version> and consists
// of the tree-leaf "id N" line followed by the module hash lines.
// We read presence + leaf id; the exact hash payload is not a
// signal we surface separately at v0.1.
func TestLookupTransparency(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	fs.mux.HandleFunc("/lookup/golang.org/x/sync@v0.20.0", func(w http.ResponseWriter, r *http.Request) {
		// The real sum.golang.org also serves this on the alternate
		// /lookup/<module>@<v> path. Mock body shape mirrors what
		// the public service emits.
		fmt.Fprint(w, "12345\ngolang.org/x/sync v0.20.0 h1:fakebase64\ngolang.org/x/sync v0.20.0/go.mod h1:fakebase64\n\n— sum.golang.org Az3grx...\n")
	})
	c := NewClientWithBaseURL(fs.URL(), fs.URL())
	rec, err := c.LookupTransparency(context.Background(), "golang.org/x/sync", "v0.20.0")
	if err != nil {
		t.Fatalf("LookupTransparency returned %v; want nil", err)
	}
	if rec.LeafID != 12345 {
		t.Errorf("LeafID = %d; want 12345", rec.LeafID)
	}
	if !strings.Contains(rec.RawRecord, "v0.20.0 h1:") {
		t.Errorf("RawRecord missing expected hash line: %q", rec.RawRecord)
	}
}

// TestLookupTransparency_NotFound: a module/version not in the log
// returns 404. Returns ErrNotFound (wrapped) — same shape as the
// proxy 404.
func TestLookupTransparency_NotFound(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	c := NewClientWithBaseURL(fs.URL(), fs.URL())
	_, err := c.LookupTransparency(context.Background(), "example.com/no/such", "v1.0.0")
	if err == nil || !errors.Is(err, ErrNotFound) {
		t.Fatalf("LookupTransparency err = %v; want ErrNotFound", err)
	}
}

// TestClient_BoundedResponse: streams larger than the cap return
// an error rather than OOMing the process. Uses a slow handler
// that emits a body claiming to be small but actually streams
// well past the cap.
func TestClient_BoundedResponse(t *testing.T) {
	t.Parallel()
	fs := newFakeServer(t)
	fs.handler = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Write more than maxResponseSize bytes; the LimitReader
		// inside the client should refuse to materialize it.
		buf := strings.Repeat("a", 1024)
		// 12 MiB > 10 MiB cap.
		for range 12 * 1024 {
			_, _ = fmt.Fprint(w, buf)
		}
	}
	c := NewClientWithBaseURL(fs.URL(), fs.URL())
	_, err := c.GetLatest(context.Background(), "example.com/big/module")
	if err == nil {
		t.Fatalf("GetLatest returned nil; want size-cap error")
	}
}

// TestClient_RejectsHTTPRedirect: any redirect to a non-HTTPS
// scheme is refused. Mirrors the npm + github clients' policy.
func TestClient_RejectsHTTPRedirect(t *testing.T) {
	t.Parallel()
	insecureTarget := "http://example.com/bad"
	fs := newFakeServer(t)
	fs.handler = func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, insecureTarget, http.StatusMovedPermanently)
	}
	c := NewClientWithBaseURL(fs.URL(), fs.URL())
	_, err := c.GetLatest(context.Background(), "example.com/x/y")
	if err == nil {
		t.Fatalf("GetLatest returned nil; want redirect-refused error")
	}
	if !strings.Contains(err.Error(), "non-HTTPS") {
		t.Errorf("err = %v; want non-HTTPS scheme refusal", err)
	}
}
