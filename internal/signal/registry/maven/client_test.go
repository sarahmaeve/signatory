package maven

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestValidateVersion pins the maven-version grammar. The function
// is the equivalent of gopublish.validateVersion: it gates strings
// before they reach URL substitution, rejecting characters that
// would re-parse the request path (/, ?, #) or smuggle traversal
// segments (..). Real Maven versions are short alphanumeric tokens
// with `.`, `-`, `+`, `_`, `~` separators.
func TestValidateVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		// Accepted shapes — examples from real Maven Central artifacts.
		{"simple semver", "33.6.0", false},
		{"semver with -jre suffix", "33.6.0-jre", false},
		{"version with RELEASE qualifier", "5.3.1.RELEASE", false},
		{"M1 milestone qualifier", "5.0.0-M1", false},
		{"RC1 release candidate", "1.0.0-RC1", false},
		{"beta-1 qualifier", "2.0.0-beta-1", false},
		{"snapshot", "1.0-SNAPSHOT", false},
		{"plus build metadata", "1.0+meta", false},
		{"tilde legacy", "1.0~rc1", false},
		{"single digit", "1", false},

		// Rejected shapes — URL-syntactic metacharacters and traversal.
		{"empty", "", true},
		{"slash", "1/0", true},
		{"backslash", "1\\0", true},
		{"path traversal", "../../etc/passwd", true},
		{"query separator", "1.0?inject=true", true},
		{"fragment", "1.0#frag", true},
		{"space", "1.0 alpha", true},
		{"null byte", "1.0\x00", true},
		{"newline", "1.0\n", true},
		{"too long", strings.Repeat("a", maxVersionLength+1), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateVersion(tc.in)
			if tc.wantErr {
				assert.Error(t, err, "ValidateVersion(%q) should error", tc.in)
			} else {
				assert.NoError(t, err, "ValidateVersion(%q) should accept", tc.in)
			}
		})
	}
}

// TestMavenClient_RejectsInvalidVersion_BeforeHTTP pins that the
// three public methods that interpolate version into a URL
// (HeadTimestamp, CheckSignature, ResolveRepoURL) reject invalid
// versions BEFORE any HTTP request fires. Counter on the test
// server stays at zero — otherwise the validator is a fig leaf.
//
// Same shape as gopublish + cargo's pre-HTTP rejection tests; the
// validator's job is to gate the URL boundary, not produce a
// pretty error message.
func TestMavenClient_RejectsInvalidVersion_BeforeHTTP(t *testing.T) {
	t.Parallel()

	const badVersion = "../../etc/passwd"

	tests := []struct {
		name string
		call func(c *Client, ctx context.Context) error
	}{
		{
			name: "HeadTimestamp",
			call: func(c *Client, ctx context.Context) error {
				_, err := c.HeadTimestamp(ctx, "com.example", "foo", badVersion)
				return err
			},
		},
		{
			name: "CheckSignature",
			call: func(c *Client, ctx context.Context) error {
				_, err := c.CheckSignature(ctx, "com.example", "foo", badVersion)
				return err
			},
		},
		{
			name: "ResolveRepoURL",
			call: func(c *Client, ctx context.Context) error {
				_, err := c.ResolveRepoURL(ctx, "com.example", "foo", badVersion)
				return err
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var hits int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				atomic.AddInt32(&hits, 1)
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(srv.Close)

			c := NewClientWithBaseURL(srv.URL)
			err := tc.call(c, context.Background())
			require.Error(t, err, "%s must reject %q", tc.name, badVersion)
			assert.Zero(t, atomic.LoadInt32(&hits),
				"%s must reject malformed version BEFORE any HTTP request fires", tc.name)
		})
	}
}

// pomWithDeps is a POM exercising the parser's precision boundaries:
// a project-level <dependencies> with a default-scope dep, an
// explicit compile dep, a test-scoped dep (must be excluded), an
// optional runtime dep (must be included — optional still declares
// the surface), and a <dependencyManagement> block whose nested
// <dependencies> must NOT be conflated with the real ones.
const pomWithDeps = `<?xml version="1.0" encoding="UTF-8"?>
<project>
  <groupId>com.example</groupId>
  <artifactId>thing</artifactId>
  <dependencyManagement>
    <dependencies>
      <dependency>
        <groupId>com.bom</groupId>
        <artifactId>managed-only</artifactId>
        <version>9.9.9</version>
      </dependency>
    </dependencies>
  </dependencyManagement>
  <dependencies>
    <dependency>
      <groupId>com.google.guava</groupId>
      <artifactId>guava</artifactId>
      <version>33.2.1-jre</version>
    </dependency>
    <dependency>
      <groupId>org.slf4j</groupId>
      <artifactId>slf4j-api</artifactId>
      <version>2.0.13</version>
      <scope>compile</scope>
    </dependency>
    <dependency>
      <groupId>org.junit.jupiter</groupId>
      <artifactId>junit-jupiter</artifactId>
      <version>5.10.2</version>
      <scope>test</scope>
    </dependency>
    <dependency>
      <groupId>com.h2database</groupId>
      <artifactId>h2</artifactId>
      <version>2.2.224</version>
      <scope>runtime</scope>
      <optional>true</optional>
    </dependency>
  </dependencies>
</project>`

// TestParseDependencies pins scope filtering and the structural
// exclusion of <dependencyManagement>. The identifier is the Maven
// groupId:artifactId coordinate; test-scoped deps are dropped (the
// dev analog, not consumed transitively); optional deps are kept.
func TestParseDependencies(t *testing.T) {
	t.Parallel()

	got := parseDependencies([]byte(pomWithDeps))

	assert.ElementsMatch(t, []string{
		"com.google.guava:guava",
		"org.slf4j:slf4j-api",
		"com.h2database:h2",
	}, got)
	assert.NotContains(t, got, "org.junit.jupiter:junit-jupiter",
		"test-scoped dependency must be excluded")
	assert.NotContains(t, got, "com.bom:managed-only",
		"<dependencyManagement> entries must not be conflated with real deps")
}

func TestParseDependencies_NoDependencies(t *testing.T) {
	t.Parallel()

	got := parseDependencies([]byte(`<?xml version="1.0"?><project><artifactId>x</artifactId></project>`))
	assert.Empty(t, got, "a POM with no <dependencies> yields an empty slice")
}

func TestFetchDependencies_Success(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/maven2/com/example/thing/1.0.0/thing-1.0.0.pom", r.URL.Path)
		w.Header().Set("Content-Type", "application/xml")
		w.Write([]byte(pomWithDeps)) //nolint:errcheck
	}))
	defer srv.Close()

	c := NewClientWithBaseURL(srv.URL)
	deps, err := c.FetchDependencies(context.Background(), "com.example", "thing", "1.0.0")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{
		"com.google.guava:guava",
		"org.slf4j:slf4j-api",
		"com.h2database:h2",
	}, deps)
}

func TestFetchDependencies_NotFound(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL(srv.URL)
	_, err := c.FetchDependencies(context.Background(), "com.example", "thing", "9.9.9")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNotFound)
}

// TestHeadTimestamp_DegradesOnBadLastModified pins the contract that a
// successful HEAD whose Last-Modified header is missing or unparseable
// yields the zero time with a NIL error — intentional graceful
// degradation, not a failure. The collector treats zero-time as "no
// timestamp for this version" and falls back; turning an unparseable
// header into a hard error would let one malformed response poison
// the whole timestamp-resolution loop. This test guards that intent
// (and backs the //nolint:nilerr on the degradation branch).
func TestHeadTimestamp_DegradesOnBadLastModified(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		lastModified string // "" means header omitted entirely
	}{
		{name: "header_absent", lastModified: ""},
		{name: "header_unparseable", lastModified: "not-a-real-date"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.lastModified != "" {
					w.Header().Set("Last-Modified", tc.lastModified)
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()

			c := NewClientWithBaseURL(srv.URL)
			ts, err := c.HeadTimestamp(context.Background(), "com.example", "thing", "1.0.0")
			require.NoError(t, err,
				"a missing/unparseable Last-Modified must degrade, not error")
			assert.True(t, ts.IsZero(),
				"degradation yields the zero time so the collector falls back")
		})
	}
}
