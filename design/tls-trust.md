# TLS trust architecture (localhost pipeline service)

Last updated: 2026-04-23.

## Scope

Signatory's pipeline message service is a local HTTP API bound to `127.0.0.1`.
Agents (Claude Code subagents) and host-side CLI clients (the `/analyze`
skill, future Go clients) talk to it over HTTPS. This document describes the
trust architecture that makes that work and the single rule every new client
follows when talking to it.

Out of scope: the trust model signatory applies *to analyzed targets* — that
lives in [`trust-model.md`](trust-model.md) and [`hardening.md`](hardening.md).
This document is about the transport security of the internal pipeline
service only.

## Architectural commitments

1. **The pipeline service is HTTPS-only.** There is no plain-HTTP mode in the
   shipped configuration. Agents reach the service via Claude Code's
   `WebFetch`, which forces HTTPS and refuses self-signed certs; there is no
   production or development use case that plain HTTP serves. The `--no-tls`
   flag currently on `serve run` / `serve start` is drift from this rule
   and is targeted for removal — see §"Drift to fix."

2. **The server cert is mkcert-issued, not self-signed.** Self-signed certs
   force every client to opt out of verification (`curl -k`,
   `InsecureSkipVerify`, `NODE_TLS_REJECT_UNAUTHORIZED=0`). Opt-outs don't
   compose — the moment any client needs to verify something real, the opt-out
   goes wrong somewhere else. A locally-trusted CA flips the default:
   verification works transparently for clients that know about the CA, and
   clients that don't see a clear error they can act on.

3. **There is exactly one canonical CA anchor.** All signatory-managed
   clients trust the same file. Environments are transient and system-keychain
   state is user-specific and shell-specific; a stable signatory-owned path
   gives every client an unambiguous trust source that survives terminal
   restarts, GUI launches, and fresh shells.

4. **`InsecureSkipVerify=true` is never the answer, even on localhost.** The
   trust chain is what catches "server cert rotated and a client didn't
   notice," "env var points at a stale CA file," and "someone started a
   second process on the same port with a different cert." Turning
   verification off defeats the preflight machinery (`signatory certs
   check`) that makes the pipeline reliable.

## The canonical trust anchor

```
~/.signatory/certs/rootCA.pem
```

Managed by `signatory certs init`, which copies the mkcert root CA
(`$(mkcert -CAROOT)/rootCA.pem`) into this stable path. The path is a public
constant in the `certs` package: `certs.DefaultCertDir` +
`certs.CAFileName`.

Why a copy into a signatory-owned path rather than referencing mkcert's
original location directly: the original path contains OS-specific
components (`$HOME/Library/Application Support/mkcert/rootCA.pem` on macOS)
and requires `$(mkcert -CAROOT)` substitution at use time. When that
substitution happens at shell-profile load, the resulting env var is a
literal path; if mkcert later reinstalls to a different directory or the
shell profile is sourced from an environment where mkcert isn't on `PATH`,
the env var becomes stale silently. A copy into a stable signatory-owned
path decouples trust from mkcert's directory layout and from shell-profile
timing. See `internal/certs/certs.go:12-30` for the full rationale.

All signatory-managed trust flows through this file.

## Per-client trust matrix

### Server: `signatory serve`

Presents `~/.signatory/certs/127.0.0.1+1.pem`, signed by the anchor, issued
by mkcert for `127.0.0.1` and `localhost`. Refuses to start if the cert or
key files are missing; the error message directs the operator to
`signatory certs init`. No plain-HTTP mode.

### Node / Claude Code WebFetch (subagents)

Trusts the anchor via `NODE_EXTRA_CA_CERTS`, which Node's TLS stack reads
with `fs.readFileSync` on every HTTPS handshake. The env var is wired by
`signatory certs init --write-profile`, which appends a managed block to
`~/.zshrc`:

```bash
export NODE_EXTRA_CA_CERTS="$HOME/.signatory/certs/rootCA.pem"
```

**Load-bearing capability constraint:** subagents must carry a tool capable
of reading the CA file (`Read`, `Glob`, or `Grep`) so Node's
`fs.readFileSync` call is permitted at handshake time. A subagent with only
`WebFetch` fails with `unable to verify the first certificate` because the
TLS stack can't complete the chain. The synthesist agent role carries
`Read Glob Grep` for this reason even though its prompt forbids using those
tools for evidence browsing. See
[`open-architecture-question.md`](open-architecture-question.md) for the
full mechanism write-up and the hypothesis-test trail.

### Go CLI clients

Applies to: the `/analyze` skill's eventual CLI-driven path
(`signatory pipeline session create`, `signatory handoff --deposit-to`), and
any future signatory Go command that talks to the pipeline service.

Construction rule: use `internal/pipeline.NewClient(pipelineURL)`. Do not
hand-roll `http.Client`. The shared client carries the trust config,
resolves the scheme, and exposes typed methods for the pipeline surface
(`CreateSession`, `DepositMessage`, `GetLatestMessage`, ...).

Trust config, applied once per `Client` instance:

- If the pipeline URL scheme is `https://` (production default):
  - If `~/.signatory/certs/rootCA.pem` exists, load it into
    `tls.Config.RootCAs`.
  - Otherwise, fall back to the system root pool (so `mkcert -install`
    paths still work for users who haven't run `signatory certs init`).
  - Never set `InsecureSkipVerify=true`.

- If the pipeline URL scheme is `http://` (test harnesses only):
  - Skip TLS configuration entirely. Plain HTTP is reached by scheme
    dispatch, not by an `InsecureSkipVerify` flag or a `--no-tls`
    CLI option.

### External tools (curl, browsers, ad-hoc scripts)

Rely on `mkcert -install`, which populates the OS keychain / NSS store /
OpenSSL cert file with the mkcert CA. Once `-install` has been run,
verification works transparently from curl and browsers.

Signatory does not manage this trust path; it's a side effect of the
one-time mkcert install step documented in the user-facing preflight
([`.claude/skills/analyze/SKILL.md` §"Step 0a"](../.claude/skills/analyze/SKILL.md)).

**Known drift:** `.claude/skills/analyze/SKILL.md` uses `curl -sk` for
session creation and plain `curl -s` for deposits. Inconsistency by
accident, not by design. Resolves automatically when the skill moves to
`signatory pipeline session create` and `signatory handoff --deposit-to`
in the `unbodge-analysts` branch — the curl paths disappear entirely.

## Testing

The pipeline package's tests use `httptest.NewServer(srv.Handler())`, which
serves plain HTTP on a random port. Go client tests will do the same: stand
up an `httptest.Server`, pass its `.URL` into the `Client` constructor, and
let the `http://` scheme dispatch skip the trust config.

TLS-path coverage: there is room for an end-to-end TLS test that issues a
test-scope CA, configures the server to present a cert signed by it, and
points the client at the anchor. Deferred until there is a behavior change
that depends on it; the scheme-dispatch separation means TLS-path bugs are
server-side, not client-side, and the pipeline server's TLS wiring is
straight `ListenAndServeTLS`.

Trust config is never disabled for `https://` URLs, including
`https://127.0.0.1:21517` (the production default). Tests that want plain
HTTP ask for it explicitly via scheme, not via an opt-out.

## Adding a new client

Checklist for any new signatory component that talks to the pipeline
service:

1. **Go, invoked from the CLI:** use `internal/pipeline.NewClient(pipelineURL)`.
   Expose the base URL as a `--pipeline-url` flag with
   `https://127.0.0.1:21517` as default. Tests pass `httptest.Server.URL`.

2. **Outside Go** (a shell script, a different language): must either rely on
   system trust (and document the `mkcert -install` dependency in the
   script's header) or explicitly load `~/.signatory/certs/rootCA.pem` via
   the language's TLS-config API. `-k` / `InsecureSkipVerify` / equivalents
   are not acceptable as a default; a deliberately-marked debug script is
   the only acceptable exception and must call out the opt-out in its file
   header.

3. **Inside Claude Code** (a subagent): include a file-reading tool
   (`Read`, `Glob`, or `Grep`) in `allowed-tools` so Node's TLS stack can
   read `NODE_EXTRA_CA_CERTS`. Document the TLS rationale at the dispatch
   site — without the comment, future reviewers will see an unused tool
   capability and try to remove it.

4. **Tests** use `httptest.NewServer`. Plain HTTP is the test transport;
   production paths stay on HTTPS via the scheme-dispatch rule.

## Drift to fix

Tracked here so every piece of architectural debt is visible in one place:

- **`--no-tls` flag on `signatory serve run` / `serve start`** —
  `cmd/signatory/serve.go:61`, `serve_lifecycle.go:92`. Contradicts
  commitment §1 above. No production use case: agents cannot reach plain
  HTTP via WebFetch, curl can reach HTTPS transparently after
  `mkcert -install`, and the scheme-dispatch rule gives tests the plain
  HTTP transport they need without a CLI flag. Remove the flag, the
  `NoTLS` field, and the embedded `--skip-preflight` escape-hatch comment
  that refers to it.

- **`curl -sk` / `curl -s` inconsistency in the skill** —
  `.claude/skills/analyze/SKILL.md:125` vs `:172, :184, :332`. Resolves
  when the skill switches to the new CLI verbs in the `unbodge-analysts`
  branch (the curl paths disappear entirely).

- **No `client.go` in `internal/pipeline`.** Introduced in the
  `unbodge-analysts` branch alongside the CLI verbs, carrying the trust
  config per §"Go CLI clients."

## References

- `internal/certs/certs.go:12-30` — rationale for a signatory-owned CA
  path over `$(mkcert -CAROOT)` substitution.
- `cmd/signatory/serve.go:40-56` — why the server requires TLS.
- [`open-architecture-question.md`](open-architecture-question.md) — why
  WebFetch subagents need a file-reading tool in their allowed-tools set.
- [`.claude/skills/analyze/SKILL.md` §"Step 0a"](../.claude/skills/analyze/SKILL.md) —
  user-facing preflight ritual operators perform before dispatching agents.
