package github

import (
	"net/http"

	"github.com/sarahmaeve/signatory/internal/httpx"
)

// newClientWithTransport constructs a Client whose underlying
// transport is rt. Used by security tests that need to verify the
// sanitization defer fires on transport-layer errors, and by the
// TLS-skip-verify redirect test that exercises the production
// scheme-downgrade refusal against an httptest.NewTLSServer.
//
// Production code never uses this — it's a test-only escape hatch
// living in a _test.go file so the symbol is unavailable to any
// non-test consumer.
func newClientWithTransport(baseURL, token string, rt http.RoundTripper) *Client {
	return &Client{
		api: httpx.NewSecureClient(
			httpx.WithBaseURL(baseURL),
			httpx.WithTransport(rt),
		),
		token: token,
	}
}
