package artifact

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHTTPFetcher_Fetch_HappyPath confirms the basic plumbing:
// a 200 response with a body shorter than the cap returns a
// readable ReadCloser whose contents match the server response.
//
// This is the boring path; its value is documenting "the fetcher
// returns the body as-is on success" so subsequent tests focus on
// failure modes.
func TestHTTPFetcher_Fetch_HappyPath(t *testing.T) {
	t.Parallel()

	const body = "hello tarball"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	fetcher := NewHTTPFetcher(HTTPFetcherOptions{
		MaxBytes: 1 << 20,
		Timeout:  5 * time.Second,
	})

	rc, err := fetcher.Fetch(context.Background(), srv.URL)
	require.NoError(t, err)
	defer rc.Close()

	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, body, string(got))
}

// TestHTTPFetcher_Fetch_SizeCap verifies the size-cap defense.
// The server streams more bytes than MaxBytes; ReadAll on the
// returned body must surface ErrArtifactTooLarge.
//
// The cap is enforced at READ time, not at HEAD time. This matters
// because chunked-encoded responses don't always advertise a
// Content-Length, and an attacker who controls the server can
// send any value or none at all in the header. Defending at the
// byte-stream layer is the only honest contract.
func TestHTTPFetcher_Fetch_SizeCap(t *testing.T) {
	t.Parallel()

	const capBytes int64 = 1024
	const overflow int = int(capBytes) * 4

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Deliberately omit Content-Length; the cap defense must
		// not rely on the header to fire.
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = io.WriteString(w, strings.Repeat("X", overflow))
	}))
	defer srv.Close()

	fetcher := NewHTTPFetcher(HTTPFetcherOptions{
		MaxBytes: capBytes,
		Timeout:  5 * time.Second,
	})

	rc, err := fetcher.Fetch(context.Background(), srv.URL)
	require.NoError(t, err,
		"Fetch returns the body lazily; the cap fires during Read, not "+
			"during dial — keeps the cap surface honest about chunked "+
			"encoding without Content-Length headers")
	defer rc.Close()

	_, err = io.ReadAll(rc)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrArtifactTooLarge),
		"reading past MaxBytes must wrap ErrArtifactTooLarge sentinel; got: %v", err)
}

// TestHTTPFetcher_Fetch_Non200ReturnsError confirms upstream
// failure modes (4xx / 5xx) surface as errors rather than as a
// body containing an HTML 404 page that would then look like a
// malformed tarball downstream. Callers that see this error
// record an absence with the upstream status.
func TestHTTPFetcher_Fetch_Non200ReturnsError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, "Not Found")
	}))
	defer srv.Close()

	fetcher := NewHTTPFetcher(HTTPFetcherOptions{
		MaxBytes: 1 << 20,
		Timeout:  5 * time.Second,
	})

	_, err := fetcher.Fetch(context.Background(), srv.URL)
	require.Error(t, err,
		"non-2xx must surface as error, not return a body the caller "+
			"would mis-feed to gzip.NewReader")
	assert.Contains(t, err.Error(), "404",
		"error must include the status code so operator diagnostics "+
			"point at the right thing")
}
