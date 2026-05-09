package artifact

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ErrArtifactTooLarge fires when a fetched response body exceeds
// HTTPFetcherOptions.MaxBytes. Wrapped via fmt.Errorf at the
// emission site so callers compare with errors.Is.
//
// Distinct from ErrArchiveTooLarge (the gunzip-stream cap inside
// Walk). Two different defenses at two different layers:
//   - ErrArtifactTooLarge protects "we won't even download more
//     than N bytes from a remote server."
//   - ErrArchiveTooLarge protects "we won't decompress more than
//     N bytes regardless of how small the on-disk file is."
//
// Both can fire for the same target, and both should: a multi-GB
// download trips ErrArtifactTooLarge first; a 5KB compression bomb
// trips ErrArchiveTooLarge during Walk.
var ErrArtifactTooLarge = errors.New("artifact response exceeded size cap")

// HTTPFetcherOptions tunes the HTTP-side defenses. Required for
// production wiring; tests pass small values for fast feedback.
type HTTPFetcherOptions struct {
	// MaxBytes caps the response body's READ stream. Reads past
	// the cap return ErrArtifactTooLarge. Required (>0).
	MaxBytes int64

	// Timeout is the per-request total budget. Applied via
	// http.Client.Timeout. Required (>0).
	Timeout time.Duration
}

// httpFetcher is the production ArtifactFetcher: net/http with a
// size-cap reader and a per-request timeout. Defaults to following
// redirects (registries serve from CDN-fronted URLs that 302 to
// blob storage).
type httpFetcher struct {
	client   *http.Client
	maxBytes int64
}

// NewHTTPFetcher returns an ArtifactFetcher configured per opts.
// Panics if MaxBytes or Timeout is non-positive — these are
// production-required values; a misconfigured fetcher is a bug to
// surface loudly at construction, not at first request.
func NewHTTPFetcher(opts HTTPFetcherOptions) ArtifactFetcher {
	if opts.MaxBytes <= 0 {
		panic("artifact.NewHTTPFetcher: MaxBytes must be positive")
	}
	if opts.Timeout <= 0 {
		panic("artifact.NewHTTPFetcher: Timeout must be positive")
	}
	return &httpFetcher{
		client:   &http.Client{Timeout: opts.Timeout},
		maxBytes: opts.MaxBytes,
	}
}

// Fetch issues a GET against url and returns the body wrapped in
// a size-capped reader. Caller must Close the returned reader.
//
// Non-2xx responses return an error — callers depend on this to
// avoid feeding HTML 404 pages to gzip.NewReader downstream and
// reporting "malformed archive" when the underlying problem is
// "registry returned 404."
//
// The size cap is applied at READ time, not via Content-Length:
// chunked responses don't always advertise length, and an attacker
// who controls the upstream can lie in the header. Honest defenses
// happen at the byte stream.
func (f *httpFetcher) Fetch(ctx context.Context, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request for %q: %w", url, err)
	}
	// Identify ourselves so registry operators can see who's
	// fetching. Conservative — no claims about features beyond
	// "we read tarballs."
	req.Header.Set("User-Agent", "signatory-artifact-fetcher/0.1")
	// Accept whatever bytes the server sends; tarballs are typed
	// inconsistently across registries (application/gzip, application/x-gzip,
	// application/octet-stream are all in the wild).
	req.Header.Set("Accept", "*/*")

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %q: %w", url, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("fetch %q: HTTP %d %s",
			url, resp.StatusCode, resp.Status)
	}

	return &cappedReadCloser{
		body: resp.Body,
		cap:  f.maxBytes,
	}, nil
}

// cappedReadCloser wraps an http.Response.Body with a size cap.
// Reads past the cap return ErrArtifactTooLarge wrapped via
// fmt.Errorf so callers see both the sentinel (for errors.Is) and
// the cap value (for diagnostics).
type cappedReadCloser struct {
	body io.ReadCloser
	cap  int64
	read int64
}

func (c *cappedReadCloser) Read(p []byte) (int, error) {
	remaining := c.cap - c.read
	if remaining <= 0 {
		return 0, fmt.Errorf("read past cap: %w (cap=%d bytes)",
			ErrArtifactTooLarge, c.cap)
	}
	if int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := c.body.Read(p)
	c.read += int64(n)
	if c.read >= c.cap && err == nil {
		// We hit the cap exactly — fail the next Read by returning
		// the sentinel here so a single-shot ReadAll surfaces it
		// rather than reporting EOF on a truncated body.
		return n, fmt.Errorf("read past cap: %w (cap=%d bytes)",
			ErrArtifactTooLarge, c.cap)
	}
	return n, err
}

func (c *cappedReadCloser) Close() error {
	return c.body.Close()
}
