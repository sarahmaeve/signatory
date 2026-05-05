package maven

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mustParseURL is a parse-or-fatal helper used by the redirect-policy
// unit tests below. Mirrors the helper in the npm and gem packages.
func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err, "parse %q", raw)
	return u
}

// ----- redirect policy unit tests -----
//
// repo1.maven.org is HTTPS-only. Any redirect target other than HTTPS
// is either a misconfiguration or a MITM attempting a scheme downgrade
// to tamper with POM / SCM URL / developer metadata that feeds trust
// signals. The policy here is symmetric with the npm, PyPI, cargo, gem,
// and gopublish clients so an audit can grep for one shape.

func TestClient_CheckRedirect_RefusesNonHTTPS(t *testing.T) {
	t.Parallel()

	via := []*http.Request{{URL: mustParseURL(t, "https://repo1.maven.org/maven2/com/google/guava/guava/maven-metadata.xml")}}

	tests := []struct {
		name   string
		target string
	}{
		{"http scheme downgrade", "http://repo1.maven.org/maven2/com/google/guava/guava/maven-metadata.xml"},
		{"attacker-host http redirect", "http://attacker.example/maven2/x"},
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

	via := []*http.Request{{URL: mustParseURL(t, "https://repo1.maven.org/maven2/com/google/guava/guava/")}}
	next := &http.Request{URL: mustParseURL(t, "https://repo1.maven.org/maven2/com/google/guava/guava/maven-metadata.xml")}
	assert.NoError(t, checkRedirect(next, via))
}

func TestClient_CheckRedirect_BoundsChain(t *testing.T) {
	t.Parallel()

	via := make([]*http.Request, 10)
	for i := range via {
		via[i] = &http.Request{URL: mustParseURL(t, "https://repo1.maven.org/")}
	}
	next := &http.Request{URL: mustParseURL(t, "https://repo1.maven.org/next")}
	err := checkRedirect(next, via)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "redirects")
}

// TestClient_NewClient_WiresCheckRedirect asserts that the production
// constructor actually installs the redirect policy on the http.Client.
// Regression guard against a future refactor that adds a new code path
// and forgets to mirror the CheckRedirect wiring.
func TestClient_NewClient_WiresCheckRedirect(t *testing.T) {
	t.Parallel()

	for _, c := range []*Client{NewClient(), NewClientWithBaseURL("https://example.test")} {
		require.NotNil(t, c.httpClient.CheckRedirect,
			"NewClient must install CheckRedirect; default policy follows HTTP redirects")
	}
}
