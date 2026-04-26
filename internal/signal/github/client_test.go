package github

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseRepoURL(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantOwner string
		wantRepo  string
		wantErr   bool
	}{
		{name: "owner/repo", input: "alecthomas/kong", wantOwner: "alecthomas", wantRepo: "kong"},
		{name: "github.com prefix", input: "github.com/stretchr/testify", wantOwner: "stretchr", wantRepo: "testify"},
		{name: "https URL", input: "https://github.com/spf13/cobra", wantOwner: "spf13", wantRepo: "cobra"},
		{name: "http URL", input: "http://github.com/spf13/cobra", wantOwner: "spf13", wantRepo: "cobra"},
		{name: "trailing .git", input: "https://github.com/spf13/cobra.git", wantOwner: "spf13", wantRepo: "cobra"},
		{name: "with whitespace", input: "  alecthomas/kong  ", wantOwner: "alecthomas", wantRepo: "kong"},
		{name: "with subpath", input: "github.com/owner/repo/tree/main", wantOwner: "owner", wantRepo: "repo"},
		{name: "empty", input: "", wantErr: true},
		{name: "just owner", input: "alecthomas", wantErr: true},
		{name: "just slash", input: "/", wantErr: true},
		{name: "empty owner", input: "/repo", wantErr: true},
		{name: "empty repo", input: "owner/", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			owner, repo, err := ParseRepoURL(tt.input)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantOwner, owner)
			assert.Equal(t, tt.wantRepo, repo)
		})
	}
}

func TestParseTotalFromLink(t *testing.T) {
	tests := []struct {
		name string
		link string
		want int
	}{
		{
			name: "standard GitHub pagination",
			link: `<https://api.github.com/repositories/128887107/commits?per_page=1&page=2>; rel="next", <https://api.github.com/repositories/128887107/commits?per_page=1&page=467>; rel="last"`,
			want: 467,
		},
		{name: "empty", link: "", want: 1},
		{name: "no last rel", link: `<https://example.com?page=2>; rel="next"`, want: 1},
		{
			name: "single page",
			link: `<https://api.github.com/repos/x/y/commits?per_page=1&page=1>; rel="last"`,
			want: 1,
		},
		{
			name: "large count",
			link: `<https://api.github.com/repos/x/y/commits?per_page=1&page=2>; rel="next", <https://api.github.com/repos/x/y/commits?per_page=1&page=15432>; rel="last"`,
			want: 15432,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := parseTotalFromLink(tt.link)
			assert.Equal(t, tt.want, got)
		})
	}
}

// --- sanitizeError ---------------------------------------------------------
//
// sanitizeError is the defense-in-depth filter applied at the
// github-client API boundary. The bearer token lives in the
// Authorization request header, never the URL, so today's stdlib
// *url.Error from net/http transport failures (DNS, TLS, timeout,
// dial) does not include the token. Defense-in-depth: scan every
// error crossing the boundary and redact the token if it appears.
// Catches:
//
//   - Custom transports / proxy middleware that include request
//     detail in their error rendering
//   - Future Go versions that change transport-error rendering
//   - Future contributors who add an error path that interpolates
//     request structs into the message
//
// The [REDACTED-TOKEN] marker in the redacted output is part of the
// security contract, not a stylistic choice: without a known
// marker, an operator triaging "why is this CI step failing?" can't
// distinguish "token wasn't there" from "token was there and got
// cleaned" — they can't audit redaction coverage. Locking the
// literal in tests means any future marker change is intentional
// and visible.

// TestSanitizeError_RedactsTokenInErrorString is the first failing
// test driving sanitizeError into existence. It locks the
// load-bearing behavior: when the token appears in the input err's
// rendered string, the sanitized result must (a) not contain the
// token and (b) carry the [REDACTED-TOKEN] marker.
//
// TDD sequence: this test fails first with a compile error
// ("undefined: sanitizeError") — forcing the signature
// `sanitizeError(err, token) error` to come into being. A no-op
// stub then fails the assertions. The minimum implementation
// (errors.New(strings.ReplaceAll(...))) makes it pass. Subsequent
// tests refine the truth table (nil err, empty token, chain
// semantics).
//
// Revert proof: change the implementation to
// `return errors.New("error sanitized")` — the marker assertion
// passes (lucky?), but this test still fails because the token is
// gone but so is everything else, AND the no-token-passthrough
// test (added next) catches the over-broad rewrite.
func TestSanitizeError_RedactsTokenInErrorString(t *testing.T) {
	t.Parallel()
	const token = "ghp_pretendThisIsARealGitHubTokenAaa1234567890"
	underlying := errors.New("dial tcp: Authorization: Bearer " + token + " in transport error")

	got := sanitizeError(underlying, token)

	require.Error(t, got)
	assert.NotContains(t, got.Error(), token,
		"TOKEN LEAK: sanitized err must not contain the token")
	assert.Contains(t, got.Error(), "[REDACTED-TOKEN]",
		"sanitized err must carry the [REDACTED-TOKEN] marker so operators can grep for sanitization events")
}

// TestSanitizeError_NilErrorReturnsNil drives the nil-input guard.
// The minimum implementation calls err.Error() unconditionally and
// would panic on nil. Defense-in-depth helpers must be safe to call
// on every error path including the no-error return — otherwise
// the named-return + defer pattern in get() would crash on success.
//
// Revert proof: remove the `if err == nil` guard from sanitizeError;
// this test panics with a nil-pointer dereference.
func TestSanitizeError_NilErrorReturnsNil(t *testing.T) {
	t.Parallel()
	got := sanitizeError(nil, "ghp_anyToken")
	assert.NoError(t, got, "nil input must produce nil output (helper safe to apply on success paths)")
}

// TestSanitizeError_EmptyTokenReturnsErrUnchanged drives the
// empty-token branch. NewClient("") creates an unauthenticated
// client (60 req/hr); on those clients there is no bearer token to
// leak and sanitizeError must no-op. Without a guard,
// strings.ReplaceAll(s, "", marker) interleaves the marker between
// every character — corrupting every error message on every
// unauthenticated call.
//
// Asserting err returns unchanged (pointer-equal via assert.Same)
// also locks chain preservation: the input err's full chain
// remains reachable via errors.Is/As.
//
// Revert proof: remove the `if token == ""` guard; the assertion
// fails because ReplaceAll-with-empty-pattern produces a corrupted
// string that's neither equal to the input nor pointer-equal.
func TestSanitizeError_EmptyTokenReturnsErrUnchanged(t *testing.T) {
	t.Parallel()
	underlying := errors.New("dial tcp: lookup api.github.com: no such host")

	got := sanitizeError(underlying, "")

	require.Error(t, got)
	assert.Same(t, underlying, got,
		"empty-token branch must return input err unchanged (unauth client; no token to redact)")
}

// TestSanitizeError_TokenAbsentPreservesChain drives the design
// choice that the helper no-ops when redaction isn't needed.
// Callers like GetDirectoryContents do `errors.Is(err, ErrNotFound)`
// against the result of get(); after sanitizeError runs on a 404
// path (where the token never appears, since URL paths don't carry
// the token), the chain to ErrNotFound MUST still be reachable.
//
// Without this property, every passthrough would allocate a fresh
// errors.New copy and break errors.Is for callers — a regression
// that's invisible until something downstream stops handling 404s.
//
// Also locks pointer-equality (assert.Same) which is stronger than
// errors.Is alone: it certifies zero allocation on the no-op path.
//
// Revert proof: change the no-token branch to return
// `errors.New(s)` (unconditional copy); errors.Is still works
// (string-matched chain... no wait, errors.New produces no chain),
// so assert.Same fires first because the pointer differs. The
// errors.Is assertion would also fail because the chain is broken.
func TestSanitizeError_TokenAbsentPreservesChain(t *testing.T) {
	t.Parallel()
	const token = "ghp_pretendThisIsARealGitHubTokenAaa1234567890"
	// Wrap a sentinel — the kind of error get() returns on 404.
	wrapped := errors.New("wrapping context: " + ErrNotFound.Error())
	sentinelChain := errors.Join(wrapped, ErrNotFound)

	got := sanitizeError(sentinelChain, token)

	require.Error(t, got)
	assert.Same(t, sentinelChain, got,
		"passthrough must return input err unchanged (no allocation)")
	assert.ErrorIs(t, got, ErrNotFound,
		"errors.Is chain must remain intact after passthrough — callers depend on it for ErrNotFound")
}

// TestSanitizeError_DropsChainOnRedaction locks the security
// tradeoff: when redaction fires, the err chain is intentionally
// dropped. A future "improvement" that preserves Unwrap (custom
// error type with Error() returning redacted text and Unwrap()
// returning the original) would let errors.Unwrap(sanitized).Error()
// recover the token — silently undoing the redaction. This test
// catches that regression.
//
// Trade-off accepted: callers lose errors.Is/As against sentinels
// in the wrapped chain WHEN redaction fires. For this codebase
// it's a non-issue: the only sentinel callers care about
// (ErrNotFound) is constructed with a path that cannot contain
// the bearer token, so the sanitizer no-ops on those errors and
// the chain is preserved (locked by
// TestSanitizeError_TokenAbsentPreservesChain above). Errors
// where the sanitizer DOES fire don't carry meaningful sentinels.
//
// Revert proof: change sanitizeError to return a custom error
// struct with Unwrap() returning the original err; this assertion
// fails because errors.Is(got, sentinel) walks Unwrap and finds
// the sentinel.
func TestSanitizeError_DropsChainOnRedaction(t *testing.T) {
	t.Parallel()
	const token = "ghp_pretendThisIsARealGitHubTokenAaa1234567890"
	sentinel := errors.New("inner sentinel")

	// Wrap a sentinel inside an outer error that DOES contain the
	// token. Pre-sanitization, errors.Is(wrapped, sentinel) is true.
	wrapped := errors.Join(
		errors.New("transport error: Authorization Bearer "+token),
		sentinel,
	)
	require.ErrorIs(t, wrapped, sentinel,
		"test setup: sentinel must be reachable BEFORE sanitization fires")

	got := sanitizeError(wrapped, token)
	require.Error(t, got)

	assert.NotContains(t, got.Error(), token,
		"TOKEN LEAK: sanitized err must not contain the token via Error()")
	assert.False(t, errors.Is(got, sentinel),
		"chain must be dropped on redaction — preserving Unwrap could leak the token via errors.Unwrap(err).Error()")
}

func TestParseRateLimitReset(t *testing.T) {
	t.Run("valid timestamp", func(t *testing.T) {
		got := parseRateLimitReset("1712700000")
		assert.False(t, got.IsZero())
	})

	t.Run("empty", func(t *testing.T) {
		got := parseRateLimitReset("")
		assert.False(t, got.IsZero(), "should return a fallback time")
	})

	t.Run("invalid", func(t *testing.T) {
		got := parseRateLimitReset("notanumber")
		assert.False(t, got.IsZero(), "should return a fallback time")
	})
}
