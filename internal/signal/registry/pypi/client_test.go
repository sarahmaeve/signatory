package pypi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestValidatePackageName covers the PEP 508 grammar plus the
// hardening rules: empty, length cap, disallowed characters. The
// validator runs at the function boundary so attacker-influenced
// strings can't slip into URL paths (the same lesson npm's #90
// captures).
func TestValidatePackageName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		// Accepted shapes (PEP 508 case-insensitive).
		{"simple lowercase", "requests", false},
		{"with hyphen", "python-dotenv", false},
		{"with underscore", "python_dotenv", false},
		{"with dot", "zope.interface", false},
		{"mixed case", "Requests", false},
		{"all caps", "REQUESTS", false},
		{"with digits", "boto3", false},
		{"single char", "a", false},
		{"single digit", "9", false},

		// Rejected shapes.
		{"empty", "", true},
		{"leading hyphen", "-foo", true},
		{"leading underscore", "_foo", true},
		{"leading dot", ".foo", true},
		{"trailing hyphen", "foo-", true},
		{"contains slash", "foo/bar", true},
		{"path traversal", "../etc/passwd", true},
		{"contains space", "foo bar", true},
		{"contains url chars", "foo?x=1", true},
		{"contains percent", "foo%20bar", true},
		{"too long", strings.Repeat("a", 101), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidatePackageName(tc.in)
			if tc.wantErr {
				assert.Error(t, err, "ValidatePackageName(%q) should error", tc.in)
			} else {
				assert.NoError(t, err, "ValidatePackageName(%q) should accept", tc.in)
			}
		})
	}
}

// TestGetProjectInfo_DecodesFixture is the happy-path integration:
// httptest server returns the captured python-dotenv response,
// client decodes it, the Info struct surfaces project_urls correctly.
func TestGetProjectInfo_DecodesFixture(t *testing.T) {
	t.Parallel()

	body, err := os.ReadFile("testdata/python-dotenv.json")
	require.NoError(t, err)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/pypi/python-dotenv/json", r.URL.Path)
		assert.Equal(t, "application/json", r.Header.Get("Accept"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL(srv.URL)
	info, err := c.GetProjectInfo(context.Background(), "python-dotenv")
	require.NoError(t, err)
	assert.Equal(t, "https://github.com/theskumar/python-dotenv",
		info.ProjectURLs["Repository"])
	assert.Equal(t, "https://github.com/theskumar/python-dotenv",
		info.ProjectURLs["Homepage"])
	assert.Equal(t, "https://saurabh-kumar.com/python-dotenv/",
		info.ProjectURLs["Documentation"])
	assert.Empty(t, info.HomePage, "fixture has empty home_page")
}

// TestGetProjectInfo_RejectsInvalidNameBeforeHTTP confirms that
// attacker-influenced names never reach the URL builder. The test
// server records hits — a successful-validation rejection means
// zero requests should arrive.
func TestGetProjectInfo_RejectsInvalidNameBeforeHTTP(t *testing.T) {
	t.Parallel()

	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL(srv.URL)
	_, err := c.GetProjectInfo(context.Background(), "../etc/passwd")
	assert.Error(t, err, "path-traversal name must be rejected")
	assert.Equal(t, 0, hits, "no HTTP request should fire on invalid name")
}

// TestGetProjectInfo_404IsErrNotFound pins the "package doesn't
// exist" error path: callers compare with errors.Is(ErrNotFound) to
// distinguish absence from network failures.
func TestGetProjectInfo_404IsErrNotFound(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL(srv.URL)
	_, err := c.GetProjectInfo(context.Background(), "definitely-not-a-real-package")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotFound),
		"404 must surface as ErrNotFound, got: %v", err)
}

// TestGetProjectInfo_5xxSanitizesBody guards #93's lesson: a
// hostile or misconfigured server can put attacker-controlled
// bytes in the response body of an error status, so the body
// MUST NOT appear in the surfaced error string.
func TestGetProjectInfo_5xxSanitizesBody(t *testing.T) {
	t.Parallel()

	const sentinel = "SENSITIVE-LEAK-CANARY"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"` + sentinel + `"}`))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL(srv.URL)
	_, err := c.GetProjectInfo(context.Background(), "requests")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), sentinel,
		"server response body must NOT appear in the error string")
}

// TestGetProjectInfo_BodyCap pins the response-body size cap. A
// malicious or malfunctioning server streaming an unbounded body
// would otherwise exhaust memory; the cap fails closed instead.
func TestGetProjectInfo_BodyCap(t *testing.T) {
	t.Parallel()

	// Stream more bytes than the cap, slowly enough that the test
	// completes quickly. Use a chunked write of zero-padded JSON
	// that's syntactically irrelevant — the cap fires before any
	// JSON parse attempt.
	huge := strings.Repeat(" ", maxResponseSize+1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, huge)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL(srv.URL)
	_, err := c.GetProjectInfo(context.Background(), "requests")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cap",
		"oversize response should mention the cap, got: %v", err)
}

// TestGetProjectInfo_RefusesPlaintextRedirect confirms the redirect
// hook rejects http:// downgrades — symmetric with npm's #89.
func TestGetProjectInfo_RefusesPlaintextRedirect(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Redirect to an http:// URL — must be refused.
		http.Redirect(w, r, "http://attacker.example/", http.StatusFound)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL(srv.URL)
	_, err := c.GetProjectInfo(context.Background(), "requests")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "non-HTTPS",
		"plaintext redirect must be refused with a non-HTTPS message, got: %v", err)
}

// TestGetProjectInfo_RespectsContextCancel pins context propagation:
// a cancelled context aborts the in-flight request promptly, not
// after the 60s client timeout.
func TestGetProjectInfo_RespectsContextCancel(t *testing.T) {
	t.Parallel()

	// Server that hangs forever — the client must give up via context.
	hung := make(chan struct{})
	defer close(hung)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already-cancelled context

	c := NewClientWithBaseURL(srv.URL)
	_, err := c.GetProjectInfo(ctx, "requests")
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled),
		"cancelled context should surface as context.Canceled, got: %v", err)
}

// ================================================================
// GetAttestation — PEP 740 Integrity API client tests
// ================================================================

// TestGetAttestation_RejectsInvalidInputs covers the validation
// boundary: empty/invalid project names, empty version, empty
// filename all fail before any HTTP call fires.
func TestGetAttestation_RejectsInvalidInputs(t *testing.T) {
	t.Parallel()

	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL(srv.URL)
	ctx := context.Background()

	tests := []struct {
		name     string
		project  string
		version  string
		filename string
	}{
		{"empty project", "", "1.0.0", "pkg-1.0.0.tar.gz"},
		{"invalid project", "../etc", "1.0.0", "pkg-1.0.0.tar.gz"},
		{"empty version", "requests", "", "pkg-1.0.0.tar.gz"},
		{"empty filename", "requests", "1.0.0", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := c.GetAttestation(ctx, tc.project, tc.version, tc.filename)
			assert.Error(t, err, "GetAttestation(%q, %q, %q) should error",
				tc.project, tc.version, tc.filename)
		})
	}
	assert.Equal(t, 0, hits, "invalid inputs must not fire HTTP requests")
}

// TestGetAttestation_404ReturnsNil pins the "no attestation" path:
// 404 from the Integrity API returns (nil, nil) — the caller interprets
// this as "publisher hasn't opted in," not as an error.
func TestGetAttestation_404ReturnsNil(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Contains(t, r.URL.Path, "/integrity/requests/2.31.0/requests-2.31.0-py3-none-any.whl/provenance")
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := NewClientWithBaseURL(srv.URL)
	resp, err := c.GetAttestation(context.Background(),
		"requests", "2.31.0", "requests-2.31.0-py3-none-any.whl")
	require.NoError(t, err)
	assert.Nil(t, resp, "404 must return nil response, not error")
}

// TestGetAttestation_DecodesResponse verifies the happy path: the
// Integrity API returns an attestation bundle and the client decodes
// the publisher identity correctly.
func TestGetAttestation_DecodesResponse(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"version": 1,
			"attestation_bundles": [{
				"publisher": {
					"kind": "GitHub",
					"repository": "psf/requests",
					"workflow": "publish.yml",
					"environment": "pypi"
				}
			}]
		}`))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL(srv.URL)
	resp, err := c.GetAttestation(context.Background(),
		"requests", "2.32.0", "requests-2.32.0-py3-none-any.whl")
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, 1, resp.Version)
	require.Len(t, resp.Bundles, 1)
	assert.Equal(t, "GitHub", resp.Bundles[0].Publisher.Kind)
	assert.Equal(t, "psf/requests", resp.Bundles[0].Publisher.Repository)
	assert.Equal(t, "publish.yml", resp.Bundles[0].Publisher.Workflow)
	assert.Equal(t, "pypi", resp.Bundles[0].Publisher.Environment)
}

// TestGetAttestation_5xxReturnsError confirms non-2xx non-404 surfaces
// as an error (the collector's policy is to record this as retryable
// absence).
func TestGetAttestation_5xxReturnsError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`SENSITIVE-CANARY`))
	}))
	defer srv.Close()

	c := NewClientWithBaseURL(srv.URL)
	resp, err := c.GetAttestation(context.Background(),
		"requests", "2.32.0", "requests-2.32.0-py3-none-any.whl")
	require.Error(t, err)
	assert.Nil(t, resp)
	assert.NotContains(t, err.Error(), "SENSITIVE-CANARY",
		"server body must not leak into error string")
	assert.Contains(t, err.Error(), "500")
}
