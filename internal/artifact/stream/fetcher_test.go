package stream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------
// Fetch: HTTP-side defenses (status, size cap, timeout, cancellation,
// redirects). Walk-side defenses are exercised by tar/zip walker tests.
// ---------------------------------------------------------------------

// TestFetcher_HappyPath confirms the basic plumbing: a 200 response
// with body shorter than the cap returns a readable ReadCloser whose
// contents match the server response.
func TestFetcher_HappyPath(t *testing.T) {
	t.Parallel()

	const body = "hello tarball"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	f := NewFetcher(FetcherOptions{Timeout: 5 * time.Second})
	rc, err := f.Fetch(t.Context(), srv.URL, 1<<20)
	require.NoError(t, err)
	defer func() { _ = rc.Close() }()

	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, body, string(got))
}

// TestFetcher_SizeCap verifies the size-cap defense: the server
// streams more bytes than the cap, ReadAll surfaces ErrLimitExceeded.
//
// The cap is enforced at READ time, not at HEAD time. Chunked
// responses don't always advertise Content-Length, and an attacker
// who controls the server can send any value or none at all in the
// header. Defending at the byte stream is the only honest contract.
func TestFetcher_SizeCap(t *testing.T) {
	t.Parallel()

	const capBytes int64 = 1024
	overflow := int(capBytes) * 4

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Deliberately omit Content-Length; the cap defense must not
		// rely on the header to fire.
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = io.WriteString(w, strings.Repeat("X", overflow))
	}))
	defer srv.Close()

	f := NewFetcher(FetcherOptions{Timeout: 5 * time.Second})
	rc, err := f.Fetch(t.Context(), srv.URL, capBytes)
	require.NoError(t, err,
		"Fetch returns the body lazily; the cap fires during Read, "+
			"not during dial — keeps the contract honest about chunked "+
			"encoding without Content-Length")
	defer func() { _ = rc.Close() }()

	_, err = io.ReadAll(rc)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrLimitExceeded),
		"reading past the cap must wrap ErrLimitExceeded; got: %v", err)
}

// TestFetcher_Non2xxReturnsError confirms upstream failure modes
// (4xx / 5xx) surface as errors rather than returning a body
// containing an HTML 404 page — which would then look like a
// malformed archive downstream and report the wrong root cause.
func TestFetcher_Non2xxReturnsError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, "Not Found")
	}))
	defer srv.Close()

	f := NewFetcher(FetcherOptions{Timeout: 5 * time.Second})
	_, err := f.Fetch(t.Context(), srv.URL, 1<<20)
	require.Error(t, err,
		"non-2xx must surface as error, not return a body the caller "+
			"would mis-feed to a format walker")
	assert.Contains(t, err.Error(), "404",
		"error must include the status code so operator diagnostics "+
			"point at the right thing")
}

// TestFetcher_ContextCancellation verifies that cancelling the
// context during a slow fetch surfaces context.Canceled.
//
// Operators need an escape hatch: a misbehaving registry that opens
// a connection and then takes minutes to start sending bytes
// shouldn't pin the analyzer.
func TestFetcher_ContextCancellation(t *testing.T) {
	t.Parallel()

	// Server holds the connection open without sending any body
	// until the test signals it to release.
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		_, _ = io.WriteString(w, "too late")
	}))
	defer srv.Close()
	defer close(release)

	f := NewFetcher(FetcherOptions{Timeout: 5 * time.Second})
	ctx, cancel := context.WithCancel(t.Context())

	// Cancel after a short delay — long enough for Fetch to issue
	// the request, short enough to keep the test fast.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err := f.Fetch(ctx, srv.URL, 1<<20)
	require.Error(t, err, "Fetch must observe context cancellation")
	assert.True(t, errors.Is(err, context.Canceled),
		"cancellation error must wrap context.Canceled so callers can "+
			"distinguish operator-initiated abort from network or HTTP errors; got: %v", err)
}

// TestFetcher_TooManyRedirects verifies that a redirect loop
// surfaces as an error rather than spinning forever.
//
// stdlib's http.Client caps redirect chains at 10 by default; the
// test forces a longer chain and asserts the cap fires. This is
// defense against malicious servers and against accidental
// misconfiguration on legitimate registries.
func TestFetcher_TooManyRedirects(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		// Always redirect to ourselves with a different path so the
		// client treats each as a fresh hop. 20 hops > stdlib's cap
		// of 10, so the chain terminates with an error.
		http.Redirect(w, r, r.URL.Path+"/r", http.StatusFound)
	}))
	defer srv.Close()

	f := NewFetcher(FetcherOptions{Timeout: 5 * time.Second})
	_, err := f.Fetch(t.Context(), srv.URL, 1<<20)
	require.Error(t, err, "infinite-redirect server must surface as error")
	assert.Less(t, hits.Load(), int32(20),
		"redirect chain must terminate well before reaching 20 hops; "+
			"got %d — stdlib's cap may have changed", hits.Load())
}

// TestFetcher_FollowsRedirect verifies that a SINGLE redirect is
// followed (registries serve from CDN-fronted URLs that 302 to
// blob storage; this is the common case, not an edge case).
func TestFetcher_FollowsRedirect(t *testing.T) {
	t.Parallel()

	const finalBody = "the actual tarball bytes"

	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, finalBody)
	}))
	defer final.Close()

	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL, http.StatusFound)
	}))
	defer front.Close()

	f := NewFetcher(FetcherOptions{Timeout: 5 * time.Second})
	rc, err := f.Fetch(t.Context(), front.URL, 1<<20)
	require.NoError(t, err)
	defer func() { _ = rc.Close() }()

	got, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.Equal(t, finalBody, string(got),
		"single redirect (CDN → blob) must be followed transparently")
}

// TestFetcher_DefaultTimeoutApplied verifies that a Fetcher built
// with zero Timeout uses DefaultFetchTimeout rather than no timeout.
//
// A misconfigured fetcher with no timeout would hang indefinitely on
// a hostile server. The default protects callers who forget — same
// fail-safe stance as DefaultLimits in Walk.
func TestFetcher_DefaultTimeoutApplied(t *testing.T) {
	t.Parallel()

	f := NewFetcher(FetcherOptions{}) // zero values
	require.NotNil(t, f.client)
	assert.Equal(t, DefaultFetchTimeout, f.client.Timeout,
		"zero Timeout must resolve to DefaultFetchTimeout, not zero "+
			"(which would mean http.Client treats it as unlimited)")
}

// TestFetcher_UserAgent verifies the default User-Agent is sent.
// Registries identify scrapers by UA; running unidentified is rude
// and risks rate-limiting.
func TestFetcher_UserAgent(t *testing.T) {
	t.Parallel()

	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("User-Agent")
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	f := NewFetcher(FetcherOptions{Timeout: 5 * time.Second})
	rc, err := f.Fetch(t.Context(), srv.URL, 1<<20)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, rc)
	_ = rc.Close()

	assert.Contains(t, got, "signatory",
		"User-Agent must identify signatory so registries can attribute traffic")
}

// ---------------------------------------------------------------------
// FetchAndWalk: integration of HTTP layer + format walker.
// ---------------------------------------------------------------------

// TestFetchAndWalk_HappyPath_TarGzip verifies the convenience
// entry point streams an HTTP tarball straight into Walk and
// returns the manifest in one call.
//
// This is the load-bearing API for the divergence collector and any
// future stream consumer — the Fetch + Walk pair without on-disk
// intermediation is the whole point of centralizing this layer.
func TestFetchAndWalk_HappyPath_TarGzip(t *testing.T) {
	t.Parallel()

	// Build a real tar.gz fixture inline.
	b := newTarGz(t).
		addFile("package/index.js", []byte("module.exports = 1")).
		addFile("package/README.md", []byte("# x"))
	tarball := b.bytes()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(tarball)
	}))
	defer srv.Close()

	f := NewFetcher(FetcherOptions{Timeout: 5 * time.Second})
	manifest, err := f.FetchAndWalk(t.Context(), srv.URL, FormatTarGzip, nil, Limits{})
	require.NoError(t, err)
	require.NotNil(t, manifest)
	assert.Len(t, manifest.Entries, 2,
		"FetchAndWalk must produce the same manifest as Fetch + Walk separately")
	assert.Equal(t, "package/", manifest.StrippedTopDir)
}

// TestFetchAndWalk_HappyPath_Zip verifies the same for zip.
func TestFetchAndWalk_HappyPath_Zip(t *testing.T) {
	t.Parallel()

	zipBytes := newZip(t).
		addFile("mypkg/setup.py", []byte("from setuptools")).
		addFile("mypkg/README.md", []byte("# pkg")).
		bytes()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(zipBytes)
	}))
	defer srv.Close()

	f := NewFetcher(FetcherOptions{Timeout: 5 * time.Second})
	manifest, err := f.FetchAndWalk(t.Context(), srv.URL, FormatZip, nil, Limits{})
	require.NoError(t, err)
	require.NotNil(t, manifest)
	assert.Len(t, manifest.Entries, 2)
	assert.Equal(t, "mypkg/", manifest.StrippedTopDir)
}

// TestFetchAndWalk_PropagatesFetchError verifies that an HTTP-layer
// failure (404, network error) surfaces from FetchAndWalk without
// being conflated with a Walk-layer error.
func TestFetchAndWalk_PropagatesFetchError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	f := NewFetcher(FetcherOptions{Timeout: 5 * time.Second})
	manifest, err := f.FetchAndWalk(t.Context(), srv.URL, FormatTarGzip, nil, Limits{})
	require.Error(t, err, "5xx must propagate from FetchAndWalk")
	assert.Nil(t, manifest)
	assert.Contains(t, err.Error(), "500",
		"error must include the status code; otherwise operators can't "+
			"distinguish 'registry down' from 'archive malformed'")
}

// TestFetchAndWalk_PropagatesWalkError verifies that a walk-layer
// failure (cap exceeded, malformed archive) surfaces from
// FetchAndWalk with the right error kind.
func TestFetchAndWalk_PropagatesWalkError(t *testing.T) {
	t.Parallel()

	junk := []byte("definitely not a gzip stream")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(junk)
	}))
	defer srv.Close()

	f := NewFetcher(FetcherOptions{Timeout: 5 * time.Second})
	_, err := f.FetchAndWalk(t.Context(), srv.URL, FormatTarGzip, nil, Limits{})
	require.Error(t, err, "malformed archive must propagate from Walk through FetchAndWalk")
	assert.False(t, errors.Is(err, ErrLimitExceeded),
		"malformed-archive error must NOT be conflated with cap error")
}

// TestFetchAndWalk_LimitsAppliedToFetch verifies that the
// MaxCompressedBytes cap from the supplied Limits is enforced
// during the HTTP read, not just during Walk.
//
// Specifically: the fetcher must NOT load the entire body into
// memory and then complain at Walk time — the whole point of
// centralizing fetch + walk is that the cap fires at the network
// layer before memory is committed.
//
// Uses FormatZip because zip's walker buffers the body via
// readCappedAll up-front; that read goes through the
// cappedHTTPBody from Fetch, so the cap fires on the network read
// rather than after the bytes have already been buffered. (For
// FormatTarGzip the test would conflate "gzip header rejected
// after a few bytes" with "cap fired" — too many ways for the
// test to pass for the wrong reason.)
func TestFetchAndWalk_LimitsAppliedToFetch(t *testing.T) {
	t.Parallel()

	const compressedCap int64 = 256
	// Build a zip larger than the cap. Use many small entries so
	// the local headers + central directory dominate (deflate would
	// otherwise collapse repeated bodies below the cap).
	b := newZip(t)
	for i := range 50 {
		b.addFile("entry-"+strings.Repeat("x", i+1), []byte("body"))
	}
	zipBytes := b.bytes()
	require.Greater(t, int64(len(zipBytes)), compressedCap,
		"fixture invariant: zip must exceed the compressed cap (got %d)", len(zipBytes))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(zipBytes)
	}))
	defer srv.Close()

	f := NewFetcher(FetcherOptions{Timeout: 5 * time.Second})
	_, err := f.FetchAndWalk(t.Context(), srv.URL, FormatZip, nil,
		Limits{MaxCompressedBytes: compressedCap})

	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrLimitExceeded),
		"compressed-input cap must fire during fetch+walk; got: %v", err)
}

// TestFetchAndWalk_PackageLevelHelper verifies the package-level
// FetchAndWalk uses a default fetcher so callers don't have to
// instantiate one for the simple case.
func TestFetchAndWalk_PackageLevelHelper(t *testing.T) {
	t.Parallel()

	tarball := newTarGz(t).addFile("a.txt", []byte("ok")).bytes()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(tarball)
	}))
	defer srv.Close()

	manifest, err := FetchAndWalk(t.Context(), srv.URL, FormatTarGzip, nil, Limits{})
	require.NoError(t, err)
	require.NotNil(t, manifest)
	assert.Len(t, manifest.Entries, 1)
}
