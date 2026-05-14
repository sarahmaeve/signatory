// Package httpx provides defensive HTTP-client primitives shared by
// signatory's signal collectors. It centralizes the discipline that
// each per-ecosystem client (npm, pypi, cargo, github, forgejo,
// gitlab, gopublish, maven, openssf, adoption) previously re-derived
// independently:
//
//   - 60s per-request timeout (configurable)
//   - HTTPS-only redirect policy with a 10-hop ceiling
//   - Bounded response reads with explicit ErrResponseTooLarge
//   - Drain-and-discard on non-2xx responses (#93 — body never reaches
//     the error string because it can carry server-debug payloads,
//     secrets, or attacker-influenced bytes)
//   - Configurable not-found-status set (404 by default; gopublish
//     wants {404, 410} so retracted modules surface uniformly)
//   - Optional StatusInterceptor for typed error translation (e.g.,
//     github's RateLimitError on 403/429 with X-RateLimit-Reset)
//
// One SecureClient per upstream forge / registry. Per-ecosystem
// packages own input validation, sentinel-error wrapping, and
// response decoding; the shared layer owns the network discipline.
package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ErrNotFound is the canonical "absent upstream" sentinel. Returned
// (wrapped via %w) when the response status is in the SecureClient's
// configured not-found set — by default just 404. Per-ecosystem
// packages wrap this so both error checks work:
//
//	var ErrNotFound = fmt.Errorf("npm: %w", httpx.ErrNotFound)
//
// Callers can compare with errors.Is(err, npm.ErrNotFound) for
// ecosystem-specific routing, or errors.Is(err, httpx.ErrNotFound)
// for ecosystem-agnostic "absent" detection.
var ErrNotFound = errors.New("httpx: not found")

// ErrResponseTooLarge is returned when the response body exceeds the
// configured MaxBytes cap (default 10 MiB; overridable per-request
// via WithRequestMaxBytes).
var ErrResponseTooLarge = errors.New("httpx: response exceeds size cap")

// Defaults match the per-ecosystem clients' existing constants so
// porting them is behavior-preserving.
const (
	defaultTimeout   = 60 * time.Second
	defaultMaxBytes  = 10 * 1024 * 1024
	defaultUserAgent = "signatory/0.1"
)

// SecureClient wraps *http.Client with the signatory defensive
// defaults. See the package doc for the rationale.
type SecureClient struct {
	httpClient       *http.Client
	baseURL          string
	userAgent        string
	maxBytes         int64
	notFoundStatuses map[int]struct{}
}

// StatusInterceptor lets a caller translate a non-2xx response into a
// typed error before the SecureClient's default classification runs.
// Returning a non-nil error short-circuits with that error; returning
// nil falls through to the default classify (ErrNotFound mapping or
// status-only error).
//
// The interceptor receives the response after status is known but
// after the body has been drained — only headers and StatusCode are
// meaningful. Reading resp.Body inside the interceptor will return
// no data.
type StatusInterceptor func(resp *http.Response) error

// Option configures a SecureClient at construction.
type Option func(*scConfig)

// RequestOption configures a single request.
type RequestOption func(*reqConfig)

// scConfig is the internal mutable configuration assembled inside
// NewSecureClient. Splitting it from SecureClient keeps the public
// type read-only after construction.
type scConfig struct {
	baseURL          string
	timeout          time.Duration
	userAgent        string
	maxBytes         int64
	notFoundStatuses map[int]struct{}
	transport        http.RoundTripper
}

// reqConfig is the internal per-request configuration assembled by
// parseRequestOpts.
type reqConfig struct {
	headers     http.Header
	maxBytes    int64 // 0 = use client default
	strictJSON  bool
	interceptor StatusInterceptor
}

// NewSecureClient constructs a SecureClient with the signatory
// defensive defaults: 60s timeout, HTTPS-only redirects, 10 MiB
// response cap, signatory/0.1 user agent, {404} not-found set.
func NewSecureClient(opts ...Option) *SecureClient {
	cfg := &scConfig{
		timeout:          defaultTimeout,
		userAgent:        defaultUserAgent,
		maxBytes:         defaultMaxBytes,
		notFoundStatuses: map[int]struct{}{http.StatusNotFound: {}},
	}
	for _, opt := range opts {
		opt(cfg)
	}
	httpClient := &http.Client{
		Timeout:       cfg.timeout,
		CheckRedirect: checkRedirect,
	}
	if cfg.transport != nil {
		httpClient.Transport = cfg.transport
	}
	return &SecureClient{
		httpClient:       httpClient,
		baseURL:          cfg.baseURL,
		userAgent:        cfg.userAgent,
		maxBytes:         cfg.maxBytes,
		notFoundStatuses: cfg.notFoundStatuses,
	}
}

// WithBaseURL sets the URL prefix that's concatenated to each request
// path. The full request URL is baseURL + path. The caller is
// responsible for URL-escaping any user-influenced bytes in path
// (url.PathEscape) — the SecureClient does not escape on the caller's
// behalf, matching the existing per-ecosystem clients' discipline.
func WithBaseURL(base string) Option {
	return func(c *scConfig) { c.baseURL = base }
}

// WithTimeout overrides the default 60s per-request timeout.
// Non-positive values are ignored — the previous value (default or
// earlier override) is preserved. This is so a stray time.Duration(0)
// can't silently disable the timeout (http.Client.Timeout=0 means
// "no timeout" in the stdlib).
func WithTimeout(d time.Duration) Option {
	return func(c *scConfig) {
		if d <= 0 {
			return
		}
		c.timeout = d
	}
}

// WithUserAgent overrides the default User-Agent header
// (signatory/0.1). Per-ecosystem clients that want richer UA strings
// (e.g., cargo's "signatory/0.1 (https://github.com/...)" form) pass
// them here.
func WithUserAgent(ua string) Option {
	return func(c *scConfig) { c.userAgent = ua }
}

// WithMaxBytes overrides the default 10 MiB response-body cap.
// Non-positive values are ignored — the previous value (default or
// earlier override) is preserved. A zero cap would otherwise make
// every non-empty response fail with ErrResponseTooLarge (fail
// closed, but operationally broken).
func WithMaxBytes(n int64) Option {
	return func(c *scConfig) {
		if n <= 0 {
			return
		}
		c.maxBytes = n
	}
}

// WithTransport replaces the underlying http.RoundTripper used by
// the SecureClient. Test-only escape hatch: lets test code inject a
// transport that simulates network failures or interpolates request
// detail into error strings (the github security tests use a leaking
// transport to verify the token-redaction defer fires; another test
// uses an httptest.NewTLSServer-backed transport to test scheme
// downgrade refusal end-to-end). The transport still goes through
// the SecureClient's redirect policy, timeout, and body-cap pipeline
// because those live on the http.Client wrapping the transport.
// Production code should not call this — the default transport
// (Go's stdlib default) is the right answer.
func WithTransport(rt http.RoundTripper) Option {
	return func(c *scConfig) { c.transport = rt }
}

// WithNotFoundStatuses replaces the not-found-status set. By default
// only 404 maps to ErrNotFound; gopublish passes {404, 410} so
// retracted modules surface uniformly.
func WithNotFoundStatuses(codes ...int) Option {
	return func(c *scConfig) {
		c.notFoundStatuses = make(map[int]struct{}, len(codes))
		for _, code := range codes {
			c.notFoundStatuses[code] = struct{}{}
		}
	}
}

// WithHeader sets a request header. Repeated calls add additional
// headers; setting the same header twice overwrites the prior value.
// Authorization, Accept, and other per-ecosystem headers go through
// here.
func WithHeader(key, value string) RequestOption {
	return func(r *reqConfig) {
		if r.headers == nil {
			r.headers = make(http.Header)
		}
		r.headers.Set(key, value)
	}
}

// WithRequestMaxBytes overrides the SecureClient's MaxBytes cap for a
// single request. Use for endpoints with smaller expected payloads
// (npm's downloads endpoint at 64 KiB, pypi attestation at 256 KiB)
// where the tighter bound is a tighter abuse defense.
//
// Non-positive values are ignored: the per-request cap stays unset
// and the client default applies. Zero would otherwise be ambiguous
// — does it mean "use client default" (the impl's current fall-
// through semantic) or "enforce zero-byte cap"? Ignoring zero
// removes the ambiguity.
func WithRequestMaxBytes(n int64) RequestOption {
	return func(r *reqConfig) {
		if n <= 0 {
			return
		}
		r.maxBytes = n
	}
}

// WithStrictJSONDecode enables DisallowUnknownFields on GetJSON's
// JSON decoder. Use only when the upstream schema is narrow and
// stable (npm.GetWeeklyDownloads, pypi attestation publisher blocks);
// large registry responses with optional fields should use the
// default lax decode so unmodeled fields don't fail the call.
func WithStrictJSONDecode() RequestOption {
	return func(r *reqConfig) { r.strictJSON = true }
}

// WithStatusInterceptor installs a per-request StatusInterceptor. See
// the StatusInterceptor type doc for semantics.
func WithStatusInterceptor(fn StatusInterceptor) RequestOption {
	return func(r *reqConfig) { r.interceptor = fn }
}

// parseRequestOpts evaluates opts against a fresh reqConfig.
func parseRequestOpts(opts []RequestOption) *reqConfig {
	cfg := &reqConfig{}
	for _, opt := range opts {
		opt(cfg)
	}
	return cfg
}

// checkRedirect is the shared redirect policy: refuse any redirect
// target that isn't HTTPS, and bound the chain to <10 hops. Go's
// stdlib http.Client already strips Authorization on cross-origin
// redirects per its shouldCopyHeaderOnRedirect rule, so we don't
// duplicate that defense here. Issue #89 lesson: scheme downgrade on
// redirect has no legitimate use case on the public registries /
// forges signatory talks to.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if req.URL.Scheme != "https" {
		return fmt.Errorf("refusing redirect to non-HTTPS URL %s", req.URL.Redacted())
	}
	if len(via) >= 10 {
		return fmt.Errorf("too many redirects")
	}
	return nil
}

// Get sends an HTTP GET, applies the defensive pipeline, and returns
// the response body bytes. ErrNotFound (wrapped) for statuses in the
// configured not-found set; status-only error (no body) for other
// non-2xx; ErrResponseTooLarge if the body exceeds the configured cap.
func (c *SecureClient) Get(ctx context.Context, path string, opts ...RequestOption) ([]byte, error) {
	_, body, err := c.do(ctx, http.MethodGet, path, opts)
	return body, err
}

// GetJSON sends a GET, applies the defensive pipeline, and decodes
// the response body as JSON into result. Status classification fires
// BEFORE the decode is attempted — callers that errors.Is(err,
// ErrNotFound) get the expected sentinel rather than a decode error
// on an HTML 404 body.
func (c *SecureClient) GetJSON(ctx context.Context, path string, result any, opts ...RequestOption) error {
	body, err := c.Get(ctx, path, opts...)
	if err != nil {
		return err
	}
	rcfg := parseRequestOpts(opts)
	if rcfg.strictJSON {
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		if derr := dec.Decode(result); derr != nil {
			return fmt.Errorf("decode JSON response: %w", derr)
		}
		return nil
	}
	if derr := json.Unmarshal(body, result); derr != nil {
		return fmt.Errorf("decode JSON response: %w", derr)
	}
	return nil
}

// GetWithResponse sends an HTTP GET, applies the defensive pipeline,
// and returns the response body bytes along with the response headers
// and status code. The same status classification as Get applies (404
// → wrapped ErrNotFound; other non-2xx → status-only error;
// ErrResponseTooLarge if body exceeds the cap). The extra return
// values are useful when the caller needs response-header data
// alongside the body — e.g., GitHub's pagination via the Link
// response header on commit-list endpoints.
//
// On error, the returned status reflects the response status when one
// was received (0 otherwise). Headers may be non-nil even on error
// paths so callers can inspect rate-limit headers, etc.
func (c *SecureClient) GetWithResponse(ctx context.Context, path string, opts ...RequestOption) ([]byte, http.Header, int, error) {
	resp, body, err := c.do(ctx, http.MethodGet, path, opts)
	var status int
	var headers http.Header
	if resp != nil {
		status = resp.StatusCode
		headers = resp.Header
	}
	return body, headers, status, err
}

// Head sends an HTTP HEAD, applies the redirect / timeout pipeline,
// and returns the response headers plus the status code. ErrNotFound
// fires for statuses in the configured not-found set; the status code
// is returned alongside the error so callers can route on it without
// needing to inspect the error chain (maven.CheckSignature returns
// (false, nil) on 404 absent of error).
func (c *SecureClient) Head(ctx context.Context, path string, opts ...RequestOption) (http.Header, int, error) {
	resp, _, err := c.do(ctx, http.MethodHead, path, opts)
	var status int
	var headers http.Header
	if resp != nil {
		status = resp.StatusCode
		headers = resp.Header
	}
	if err != nil {
		return nil, status, err
	}
	return headers, status, nil
}

// do is the shared request pipeline. Returns the response (for HEAD
// header access on success and for status access on error) along with
// any body bytes (for GET).
func (c *SecureClient) do(ctx context.Context, method, path string, opts []RequestOption) (*http.Response, []byte, error) {
	rcfg := parseRequestOpts(opts)
	maxBytes := c.maxBytes
	if rcfg.maxBytes > 0 {
		maxBytes = rcfg.maxBytes
	}

	req, err := c.buildRequest(ctx, method, path, rcfg)
	if err != nil {
		return nil, nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("execute request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // body close at end of do; err is not actionable

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return c.handleErrorResponse(resp, method, path, maxBytes, rcfg.interceptor)
	}

	if method == http.MethodHead {
		return resp, nil, nil
	}

	body, err := readBoundedBody(resp.Body, maxBytes)
	if err != nil {
		return resp, nil, err
	}
	return resp, body, nil
}

// buildRequest constructs the *http.Request with User-Agent and any
// caller-supplied headers applied.
func (c *SecureClient) buildRequest(ctx context.Context, method, path string, rcfg *reqConfig) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}
	for k, vs := range rcfg.headers {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}
	return req, nil
}

// handleErrorResponse drains the body (#93), runs the optional
// interceptor, and returns either the interceptor's error, an
// ErrNotFound wrap, or a status-only error. The drain runs BEFORE
// classification so the connection is reusable regardless of which
// branch fires.
func (c *SecureClient) handleErrorResponse(resp *http.Response, method, path string, maxBytes int64, interceptor StatusInterceptor) (*http.Response, []byte, error) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBytes))

	if interceptor != nil {
		if e := interceptor(resp); e != nil {
			return resp, nil, e
		}
	}
	if _, ok := c.notFoundStatuses[resp.StatusCode]; ok {
		return resp, nil, fmt.Errorf("%w: %s %s", ErrNotFound, method, path)
	}
	return resp, nil, fmt.Errorf("upstream returned status %d", resp.StatusCode)
}

// readBoundedBody reads up to maxBytes+1 from body and reports
// ErrResponseTooLarge if the cap is exceeded.
func readBoundedBody(body io.Reader, maxBytes int64) ([]byte, error) {
	limited := io.LimitReader(body, maxBytes+1)
	buf, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}
	if int64(len(buf)) > maxBytes {
		return nil, fmt.Errorf("%w: %d > %d", ErrResponseTooLarge, len(buf), maxBytes)
	}
	return buf, nil
}
