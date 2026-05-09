package stream

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// DefaultFetchTimeout is the per-request budget applied when callers
// pass FetcherOptions{} with no Timeout. Aligned with the upper end
// of legitimate registry round-trip times for ~256 MiB artifacts —
// tighter for tests, looser only when the caller has documented
// reasoning.
const DefaultFetchTimeout = 60 * time.Second

// defaultUserAgent identifies signatory in registry logs. Conservative
// — names the project and the package, no claims about features.
const defaultUserAgent = "signatory-stream-fetcher/0.1"

// Fetcher is the HTTP source for FetchAndWalk. Encapsulates the
// http.Client + per-request defaults so callers don't redo timeout
// and User-Agent boilerplate. Safe for concurrent use across many
// goroutines (http.Client is itself goroutine-safe).
type Fetcher struct {
	client    *http.Client
	userAgent string
}

// FetcherOptions configures a Fetcher. Zero-value Timeout resolves
// to DefaultFetchTimeout; zero-value UserAgent resolves to
// defaultUserAgent. Callers can pass FetcherOptions{} for the
// out-of-the-box behavior.
type FetcherOptions struct {
	Timeout   time.Duration
	UserAgent string
}

// NewFetcher returns a Fetcher with the supplied options, falling
// back to the package defaults for any zero-valued field.
//
// Unlike the legacy NewHTTPFetcher (which panicked on zero values),
// this constructor applies defaults — same fail-safe stance as
// DefaultLimits in Walk. A misconfigured fetcher silently using a
// reasonable default is strictly less hazardous than a panic at
// startup.
func NewFetcher(opts FetcherOptions) *Fetcher {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultFetchTimeout
	}
	ua := opts.UserAgent
	if ua == "" {
		ua = defaultUserAgent
	}
	return &Fetcher{
		client:    &http.Client{Timeout: timeout},
		userAgent: ua,
	}
}

// Fetch issues an HTTP GET against url and returns the response body
// wrapped in a size-capped reader. Reads past limit return
// ErrLimitExceeded. Caller must Close the returned reader.
//
// Non-2xx responses fail with an error including the status code.
// Callers depend on this to avoid feeding HTML 404 pages to a
// format walker and reporting "malformed archive" when the
// underlying problem is "registry returned 404".
//
// The size cap is enforced at READ time, not via Content-Length:
// chunked responses don't always advertise length, and an attacker
// who controls the upstream can lie in the header. Defending at the
// byte stream is the only honest contract.
func (f *Fetcher) Fetch(ctx context.Context, url string, limit int64) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("stream: build request for %q: %w", url, err)
	}
	req.Header.Set("User-Agent", f.userAgent)
	// Accept whatever the server sends; archive types are inconsistent
	// across registries (application/gzip, application/x-gzip,
	// application/octet-stream are all in the wild for tarballs).
	req.Header.Set("Accept", "*/*")

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("stream: fetch %q: %w", url, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("stream: fetch %q: HTTP %d %s",
			url, resp.StatusCode, resp.Status)
	}

	return &cappedHTTPBody{body: resp.Body, limit: limit}, nil
}

// FetchAndWalk is the convenience entry point: HTTP-GET the URL,
// stream the body straight into Walk in one pass. No on-disk
// staging, no full-buffer materialization beyond what the format
// walker requires (zip buffers; tar.gz streams).
//
// Limits.MaxCompressedBytes governs both the HTTP read cap AND the
// in-memory zip buffer cap. Other Limits fields apply per-format
// during Walk.
func (f *Fetcher) FetchAndWalk(ctx context.Context, url string, format Format,
	intents []CaptureIntent, lim Limits) (*Manifest, error) {

	lim = resolveLimits(lim)

	body, err := f.Fetch(ctx, url, lim.MaxCompressedBytes)
	if err != nil {
		return nil, err
	}
	defer func() { _ = body.Close() }()

	return Walk(ctx, body, format, intents, lim)
}

// defaultFetcher is the package-level Fetcher used by the
// package-level FetchAndWalk convenience. Built with default options
// (DefaultFetchTimeout, default User-Agent). Goroutine-safe.
var defaultFetcher = NewFetcher(FetcherOptions{})

// cappedHTTPBody wraps an http.Response.Body with a hard byte cap.
// Reads past the cap return ErrLimitExceeded so a single-shot
// io.ReadAll surfaces the cap rather than reporting EOF on a
// truncated body.
type cappedHTTPBody struct {
	body  io.ReadCloser
	limit int64
	read  int64
}

func (c *cappedHTTPBody) Read(p []byte) (int, error) {
	remaining := c.limit - c.read
	if remaining <= 0 {
		return 0, fmt.Errorf("%w: HTTP body read past %d-byte cap",
			ErrLimitExceeded, c.limit)
	}
	if int64(len(p)) > remaining {
		p = p[:remaining]
	}
	n, err := c.body.Read(p)
	c.read += int64(n)
	if c.read >= c.limit && err == nil {
		// Hit the cap exactly with no underlying error — synthesize
		// the cap error here so a single-shot ReadAll observes it
		// rather than reporting EOF on a truncated body.
		return n, fmt.Errorf("%w: HTTP body read past %d-byte cap",
			ErrLimitExceeded, c.limit)
	}
	return n, err
}

func (c *cappedHTTPBody) Close() error {
	return c.body.Close()
}
