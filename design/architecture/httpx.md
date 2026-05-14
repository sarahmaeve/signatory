# httpx — shared HTTP discipline for outbound collectors

Last updated: 2026-05-14.

## Scope

`internal/httpx` is the shared HTTP layer every per-ecosystem signal
collector calls when it needs to reach a public registry or forge.
It owns the **network discipline** — timeouts, redirects, body caps,
drain-on-error, status classification — so per-ecosystem packages
own only their own concerns: input validation, response decoding,
sentinel-error wrapping, and translation of upstream-specific
quirks.

**In scope:** outbound GET/HEAD against public APIs (npm, PyPI,
crates.io, RubyGems, Maven Central, proxy.golang.org, sum.golang.org,
api.github.com, codeberg.org, gitlab.com, api.securityscorecards.dev).

**Out of scope:** POST channels (use `internal/pipeline`); streaming
downloads of binary artifacts (use `internal/artifact/stream`);
custom TLS trust anchors for localhost services
(`internal/pipeline`); anything writing to the network for purposes
other than collecting trust signals.

## The contract

A `SecureClient` always enforces these properties on every request,
without per-collector opt-in:

1. **HTTPS-only redirects.** A redirect target with `URL.Scheme != "https"`
   is refused with a loud error. Production targets all use HTTPS;
   any redirect to `http://` is either misconfiguration or a MITM
   scheme-downgrade attempt. (Issue #89.)
2. **10-hop redirect ceiling.** Loops and pathological chains fail
   fast.
3. **60s per-request timeout** by default. Overridable.
4. **Bounded response read.** Body capped at 10 MiB by default;
   exceeding the cap returns `httpx.ErrResponseTooLarge` rather
   than letting an oversized stream exhaust memory.
5. **Drain-on-error, body NEVER in error string.** Non-2xx responses
   are drained (so the connection is reusable) and surfaced as a
   status-code-only error. The response body is attacker-influenceable
   bytes; including it in error strings lets server-debug noise reach
   CI logs and SIEM ingest. (Issue #93.)
6. **Configurable not-found-status set.** Default `{404}`; gopublish
   passes `{404, 410}` so retracted Go-module versions surface
   uniformly as `ErrNotFound`.
7. **JSON decode errors are annotated** with `"decode JSON response"`
   so callers reading logs can tell decode failures from transport
   failures.

These properties are tested in `internal/httpx/client_test.go`. The
test suite is the source of truth — every new option or behavior
gets a test that pins it before the impl lands.

## Mental model

```
                          per-ecosystem package
                          ──────────────────────
                          ValidatePackageName   ← input grammar
                          Get*/Resolve* methods ← endpoint shape
                          ErrNotFound sentinel  ← typed error
                          response struct types ← decoding
                                  │
                                  ▼
                          httpx.SecureClient
                          ───────────────────
                          Get / GetJSON / Head / GetWithResponse
                                  │
                                  ▼
                          *http.Client (private)
                          ───────────────────────
                          timeout + checkRedirect
                          + io.LimitReader
                          + drain-on-error
                          + ErrNotFound classification
                          + status interceptor hook
```

**One SecureClient per upstream forge/registry.** Per-ecosystem
clients hold one or more SecureClients as private fields. npm has
two (registry + downloads); gopublish has three (proxy + sum +
meta-tag). Most have one.

## Adding a new collector — recipe

1. **Create the package** at `internal/signal/registry/<eco>/` (for
   package registries) or `internal/signal/<forge>/` (for forges).

2. **Declare the sentinel** wrapping `httpx.ErrNotFound` so callers
   get both ecosystem-specific and ecosystem-agnostic chain matches:

   ```go
   var ErrNotFound = fmt.Errorf("<eco>: %w", httpx.ErrNotFound)
   ```

3. **Declare validation** at the function boundary. Every byte that
   reaches a URL path component MUST be validated before substitution
   — this is the discipline from issue #90.

   ```go
   func Validate<Eco>Name(name string) error { ... }
   ```

4. **Declare the Client struct** with private `*httpx.SecureClient`
   fields. Token / config fields live alongside:

   ```go
   type Client struct {
       api   *httpx.SecureClient
       token string  // if the ecosystem needs auth
   }
   ```

5. **Declare three constructors** by convention:

   ```go
   func NewClient() *Client                                   // production
   func NewClientWithBaseURL(base string) *Client             // test seam
   func NewClientWithBaseURLAndToken(base, token string) *Client  // test seam with auth (github only so far)
   ```

   Keep them in lockstep by routing through a private `newClient`
   helper so config drift isn't possible.

6. **Wire endpoints** through `httpx.GetJSON` / `httpx.Get` /
   `httpx.Head`:

   ```go
   func (c *Client) GetThing(ctx context.Context, name string) (*Thing, error) {
       if err := Validate<Eco>Name(name); err != nil {
           return nil, fmt.Errorf("get thing: %w", err)
       }
       var t Thing
       err := c.api.GetJSON(ctx, "/api/v1/"+url.PathEscape(name), &t,
           httpx.WithHeader("Accept", "application/json"))
       if err != nil {
           if errors.Is(err, httpx.ErrNotFound) {
               return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
           }
           return nil, fmt.Errorf("<eco> request for %q: %w", name, err)
       }
       return &t, nil
   }
   ```

   The `errors.Is(err, httpx.ErrNotFound)` → wrap with ecosystem
   `ErrNotFound` pattern is **load-bearing**: it puts the ecosystem
   sentinel in the chain so callers' `errors.Is(err, <eco>.ErrNotFound)`
   keeps matching after the port.

7. **Tests:** use `NewClientWithBaseURL(server.URL)` against an
   `httptest.NewServer`. The shared layer's redirect / timeout /
   body-cap / drain-on-error properties are tested in `internal/httpx`;
   per-collector tests focus on response decoding, sentinel wrapping,
   per-endpoint URL shape, and 404-semantic translation.

## Surface reference

### Construction

| Function | Use |
|---|---|
| `NewSecureClient(opts ...Option)` | Build a SecureClient with defensive defaults. |

### Client-level options

| Option | Default | Use |
|---|---|---|
| `WithBaseURL(u)` | `""` | URL prefix concatenated to each request path. Empty allowed when caller passes full URLs (gopublish's meta-tag fetch). |
| `WithTimeout(d)` | `60s` | Per-request timeout. Tighten for narrow APIs (gem uses 15s). |
| `WithUserAgent(ua)` | `"signatory/0.1"` | Override default UA. Pass `""` to skip the header and let Go's stdlib default (`Go-http-client/1.1`) fire — only do this for behavior parity with pre-existing code. |
| `WithMaxBytes(n)` | `10 MiB` | Response-body cap. Tighten for narrow schemas (openssf uses 1 MiB). |
| `WithNotFoundStatuses(codes...)` | `{404}` | Replace the not-found-status set. gopublish uses `{404, 410}`. |
| `WithTransport(rt)` | stdlib default | **Test-only.** Inject a custom `http.RoundTripper`. Used by github security tests (leaking transport, TLS-trusting transport). Production must not call this. |

**Non-positive values are ignored** by `WithTimeout`, `WithMaxBytes`,
and `WithRequestMaxBytes`. The previous value (default or earlier
override) is preserved. This is so a stray `time.Duration(0)` or
`int64(0)` from caller arithmetic can't silently disable a defense
(`http.Client.Timeout=0` means "no timeout" in the stdlib;
`maxBytes=0` would fail every non-empty response).

### Per-request options

| Option | Use |
|---|---|
| `WithHeader(key, value)` | Add a request header. Authorization, Accept, etc. |
| `WithRequestMaxBytes(n)` | Override client-level cap for this call. npm downloads uses 64 KiB; pypi attestations uses 256 KiB. |
| `WithStrictJSONDecode()` | Enable `DisallowUnknownFields` on JSON decode. Use only when schema is narrow + stable (npm `/downloads`, pypi attestation publisher block). Lax decode is the default. |
| `WithStatusInterceptor(fn)` | Translate non-2xx into a typed error before the default classification runs. See "Typed errors" below. |

### Methods

| Method | Returns | Use |
|---|---|---|
| `Get(ctx, path, opts...)` | `([]byte, error)` | Foundation. Body bytes. Caller decodes. Use for non-JSON responses (gopublish `@v/list` newline-text, maven POM XML, sum.golang.org `/lookup`). |
| `GetJSON(ctx, path, &result, opts...)` | `error` | Convenience: `Get` + `json.Unmarshal` (with optional `DisallowUnknownFields`). Decode errors are wrapped with `"decode JSON response"`. |
| `Head(ctx, path, opts...)` | `(http.Header, int, error)` | HEAD request. Returns headers + status code. Use for existence probes and Last-Modified reads (maven `CheckSignature`, `HeadTimestamp`). |
| `GetWithResponse(ctx, path, opts...)` | `([]byte, http.Header, int, error)` | GET that also exposes response headers. Use when the caller needs a response header alongside the body (github `Link` header for pagination). |

## Error model

The chain holds across the boundary:

```
fmt.Errorf("<eco> request for %q: %w", name, err)
   ↳ fmt.Errorf("%w: %s", <eco>.ErrNotFound, name)
        ↳ <eco>.ErrNotFound  (which is fmt.Errorf("<eco>: %w", httpx.ErrNotFound))
             ↳ httpx.ErrNotFound
```

Both `errors.Is(err, npm.ErrNotFound)` and `errors.Is(err, httpx.ErrNotFound)`
match. Ecosystem-specific code uses the first; cross-ecosystem code
(resolvers, surveyors) that just needs to know "the upstream said
this is absent" uses the second.

**Sentinels in use:**

- `httpx.ErrNotFound` — wrapped by every ecosystem's `ErrNotFound`.
- `httpx.ErrResponseTooLarge` — fires when the configured body cap
  is exceeded. Surfaces verbatim to the caller; ecosystems generally
  let it propagate.

## Typed errors via `WithStatusInterceptor`

When an ecosystem needs to translate specific upstream statuses into
typed errors (not just `ErrNotFound`), use a `StatusInterceptor`:

```go
type StatusInterceptor func(resp *http.Response) error
```

The interceptor sees the response after the body has been drained.
It can read **headers and status code only** — the body is gone.
Return a non-nil error to short-circuit; return nil to fall through
to httpx's default classification.

**Two real uses today:**

- **github + adoption: rate-limit translation.** 403 / 429 with an
  `X-RateLimit-Reset` header → typed `*RateLimitError` (or
  `ErrRateLimit` sentinel) so the collector can route to a retryable
  failure.
- **gem: auth-required translation.** 401 / 403 → `ErrUnauthorized`
  so the collector knows the request needs an API key vs. a network
  error.

Typed errors are returned **unwrapped** by the interceptor so
`errors.Is` and `errors.As` work at the call site. Do not wrap the
interceptor's return value in the per-method error path.

## Patterns

### Single base URL (most common)

```go
type Client struct {
    api *httpx.SecureClient
}
func NewClient() *Client {
    return &Client{
        api: httpx.NewSecureClient(httpx.WithBaseURL("https://example.com")),
    }
}
```

### Multiple base URLs (npm, gopublish)

When an ecosystem's API is split across hosts, one SecureClient per
host. Routing happens at the method level — each method picks the
right SecureClient.

```go
type Client struct {
    registryAPI  *httpx.SecureClient  // metadata
    downloadsAPI *httpx.SecureClient  // counts
}
```

### Auth-conditional headers (github, adoption)

```go
opts := []httpx.RequestOption{httpx.WithHeader("Accept", ...)}
if c.token != "" {
    opts = append(opts, httpx.WithHeader("Authorization", "Bearer "+c.token))
}
```

### Existence probes via HEAD (maven)

```go
_, _, err := c.api.Head(ctx, "/path/to/.asc")
if err == nil {
    return true, nil  // 200 OK
}
if errors.Is(err, httpx.ErrNotFound) {
    return false, nil  // 404 → absent, not error
}
return false, fmt.Errorf(...)  // other failure
```

### Non-JSON bodies (gopublish, maven)

Use `Get` (returns `[]byte`). Decode in the caller — `xml.Unmarshal`
for maven, line-split for gopublish `@v/list`, `bytes.IndexByte` +
`strconv.ParseInt` for sum.golang.org leaf-id parsing.

### Empty baseURL + full URL per request (gopublish meta-tag)

When each request targets a different host (gopublish vanity-host
fallback), construct with no `WithBaseURL` option and pass full URLs
as the path argument. httpx's defenses still apply.

## When NOT to use httpx

`httpx` is for the read-only public-registry collection surface. Do
not route the following through it:

- **POST traffic** — use `internal/pipeline/client.go` (the local
  pipeline service uses POST + custom mkcert TLS anchor).
- **Streaming binary downloads** — `internal/artifact/stream/fetcher.go`
  returns `io.ReadCloser` with byte-level stream caps because
  `httpx.Get` loads the full body into memory.
- **Custom TLS trust** — pipeline service's mkcert anchor lives in
  `internal/certs`; integrating that into `httpx` would conflate two
  threat models.
- **Smoke / dogfood drivers** — `cmd/smoke-mcp`,
  `cmd/dogfood-metrics-smoke`, and similar test binaries can use raw
  `*http.Client` since they're testing OTHER endpoints, not collecting
  trust signals.

## Conscious omissions

These are deliberately NOT in httpx and should stay out:

- **Token redaction.** Lives in the github package via `sanitizeError`
  + `defer` pattern, because only the per-ecosystem layer knows the
  secret string to redact. httpx has no concept of secrets.
- **Retry / backoff.** No collector retries today. If retry semantics
  are added, they belong in a wrapper above httpx (per-endpoint
  policy varies; the shared layer shouldn't impose a default).
- **Per-host rate limiting.** Same reasoning — host-specific quotas
  belong in the per-ecosystem layer or a future
  `internal/ratelimit` package.
- **Response caching.** Out of scope for now; cache would belong in a
  higher layer (between the collector and the store).
- **Cross-origin Authorization stripping.** Go's stdlib `http.Client`
  already strips `Authorization` on cross-origin redirects via
  `shouldCopyHeaderOnRedirect`. The pre-port github client did this
  explicitly in `checkRedirect`; the explicit strip was redundant
  and is gone post-port. Production behavior is unchanged.

## Adding a new option

If a new ecosystem needs a behavior httpx doesn't expose, add the
option behind an `Option` or `RequestOption` function and pin its
contract with a test in `internal/httpx/client_test.go` **before**
the consuming ecosystem starts using it. Options should be:

- **Backward-compatible** — the default behavior shouldn't change
  for existing callers.
- **Behavioral, not structural** — `WithFoo(bar)` not `SetFoo(c, bar)`.
- **Tested** at the `httpx` layer, not the consuming-collector layer.
  The shared layer's tests are the source of truth for httpx behavior.

## Reference: live-port history

The eleven per-ecosystem clients ported to httpx (2026-05-14):
pypi, cargo, npm, forgejo, gopublish, gitlab, maven, adoption,
openssf, gem, github. The consolidation removed eleven copies of
`checkRedirect`, two internal `get` helpers, and the equivalent
boilerplate in every endpoint method. Aggregate complexity (gocyclo)
across the eleven files dropped by roughly 150 with the shared
layer absorbing ~50 for a net repo-wide reduction of ~100. The
shared layer is tested with 28 properties; the per-ecosystem
clients no longer need to re-test the same defenses.
