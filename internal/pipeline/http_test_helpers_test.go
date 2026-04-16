package pipeline_test

import (
	"io"
	"net/http"
	"testing"
)

// HTTP helpers for tests in the pipeline package.
//
// These wrap http.NewRequestWithContext + client.Do to keep call sites
// compact and ensure every test-side request propagates t.Context().
// Context propagation matters for:
//
//   - Test timeouts: a test that exceeds its deadline should abort
//     in-flight HTTP requests rather than let them complete and
//     potentially spawn follow-up work after the test frame is gone.
//   - Race detector runs (`go test -race`): orphan goroutines holding
//     connections open past test completion trip the leak checker in
//     downstream tests and produce flaky failures.
//   - Suite cancellation: go test's outer context cancellation
//     should propagate cleanly.
//
// The lint rule `noctx` enforces this discipline. Before these helpers,
// each test site had a hand-rolled NewRequestWithContext + Do pair or,
// worse, used client.Get / client.Post which build a request with
// context.Background() under the hood. The helpers keep the compact
// surface while satisfying the rule.

// doPost issues a POST with application/json content type and t.Context().
// Every POST in this package's tests uses application/json, which is why
// Content-Type is baked in rather than parameterized.
func doPost(t *testing.T, c *http.Client, url string, body io.Reader) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.Do(req)
}

// doGet issues a GET with t.Context().
func doGet(t *testing.T, c *http.Client, url string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return c.Do(req)
}

// doRequest issues a request with an explicit method (typically DELETE,
// PUT, PATCH, or a deliberately-wrong method used by negative tests that
// exercise method/path mismatch behavior).
func doRequest(t *testing.T, c *http.Client, method, url string, body io.Reader) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, body)
	if err != nil {
		return nil, err
	}
	return c.Do(req)
}
