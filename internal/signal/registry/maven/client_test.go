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
