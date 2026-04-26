# Signatory: Pending Fixes

Items surfaced by reviews, adversarial passes, and dogfood that haven't
been addressed yet. Most are documented in the relevant commit messages
too — this file is the merged, prioritized backlog so they don't fall
out of attention.

When you fix one, delete the entry rather than marking it done — the
git history is the record.

## Conventions

Each item has:
- **Source:** which review surfaced it (commit hash + conclusion ID)
- **Severity:** must-fix / should-fix / nice-to-have
- **Sketch:** what to do (specific, not "consider improving")

## Security

### Token leakage path in `applyNetworkPrecheck` errors

- **Source:** unaided cmd-adversarial agent 2026-04-14 (priority 1
  recommendation, deferred); confirmed by skill-equipped pass
- **Severity:** should-fix
- **Where:** `internal/signal/github/client.go` error wrapping
- **Sketch:** `ghclient.Client` propagates transport-layer errors
  verbatim via `fmt.Errorf("execute request: %w", err)` (see
  `client.go:240`, `client.go:300`). The response-body leak path is
  closed (issue #93 — non-200 bodies are dropped, status code only),
  but a transport-layer error (DNS failure, TLS error, timeout) can
  still wrap an underlying error string that — depending on the
  transport implementation — may include URL or proxy detail. The
  bearer token is in the `Authorization` header, never the URL, so the
  practical leak window is narrow today. Defense-in-depth: add a
  `sanitizeError(err, token) error` helper in the github package that
  scans error strings for the token value and redacts it. Apply at
  every error-return path in `client.go`.

### `expandTilde` silently passes through when `$HOME` is unresolvable

- **Source:** unaided config reviewer 2026-04-14 (F12, deferred)
- **Severity:** nice-to-have
- **Where:** `internal/config/handoff.go:expandTilde`
- **Sketch:** When `$HOME` is unset (sudo, service manager, CI runner),
  the function returns `~/secrets` unchanged. The agent receiving the
  rendered handoff then resolves the tilde in its own context, which
  may differ. This is a silent behavior divergence between signatory's
  rendering intent and the analyst's execution context. Replace silent
  passthrough with an error: "cannot expand tilde — pass --path
  explicitly". Update tests accordingly.

### Symlink hardening for template directories

- **Source:** unaided config reviewer 2026-04-14 (F4, deferred);
  unaided path-adversarial agent 2026-04-14 (recommendation #2)
- **Severity:** nice-to-have (with caveat: if config file becomes
  attacker-controlled in any deployment, this becomes should-fix)
- **Where:** `internal/config/resolver.go:tryOpenFile`
- **Sketch:** `os.Open` follows symlinks. A user-writable template
  dir containing `handoffs/foo.md → /etc/shadow` would be silently
  followed and the content rendered into a handoff. Today template
  dirs are trusted by convention; document that explicitly in
  `resolver.go`'s package comment, and add a `Resolver.StrictSymlinks
  bool` opt-in that uses `EvalSymlinks` to verify the resolved path
  is inside the configured directory.

## Architecture / refactor

### Empty-string-in-enum trick on `--language`/`--ecosystem`/`--target-role`

- **Source:** unaided cmd reviewer 2026-04-14 (F3, F11, deferred)
- **Severity:** nice-to-have
- **Where:** `cmd/signatory/handoff.go` HandoffCmd struct tags
- **Sketch:** `enum:"python,go,"` with trailing comma admits `""` as
  a valid value so kong defaults work, but pollutes `--help` output
  with `python|go|` (confusing trailing pipe) and admits an explicit
  `--language=""` from CLI users. Use kong's `HasBeenSet` pattern
  (or a `*string` field) to distinguish "user passed it" from "user
  omitted it" without needing the empty enum member. Apply uniformly
  to TargetRole and Ecosystem too.

### `--list-templates` discoverability

- **Source:** unaided cmd reviewer 2026-04-14 (F4, deferred)
- **Severity:** nice-to-have
- **Where:** `cmd/signatory/handoff.go` (and possibly a sibling
  `signatory templates list` command)
- **Sketch:** A user has no in-CLI way to answer "what templates are
  shipped?" or "is there a Java one yet?". The resolver already has
  `ListTemplateSearchPath()` for diagnostics — wire it up. Add
  `signatory handoff --list-templates` that prints the search path
  + every template found across the layered search. Optionally a
  `--show-template <name>` that prints the raw template file for
  inspection. Unlocks self-service template debugging.

### `validateRelName`: rewrite as a whitelist instead of layered blacklist

- **Source:** unaided config reviewer 2026-04-14 (F5, deferred)
- **Severity:** nice-to-have
- **Where:** `internal/config/resolver.go:validateRelName`
- **Sketch:** Current code layers six blacklist checks (empty,
  absolute, "..", NUL, "%", "\\", >0x7E). Each guard was added in
  response to a specific attack the prior pass surfaced, and the
  ordering is fragile. Rewrite as a positive whitelist: reject
  anything that isn't `[A-Za-z0-9._/-]+` per segment, with explicit
  segment-level checks. Mirrors the key-name whitelist already used
  by the TOML parser. Whitelist > blacklist for path components.

### `HandoffSubstitutions` gate-keeping is asymmetric

- **Source:** unaided config reviewer 2026-04-14 (F13, deferred)
- **Severity:** nice-to-have
- **Where:** `internal/config/handoff.go:HandoffSubstitutions`
- **Sketch:** `TARGET_NAME` is gate-kept (errors out if the inferred
  name is empty and no override). `TARGET_URL` and `TARGET_PATH`
  aren't — they fall through to the unfilled-placeholder report.
  Either (a) extend the gate-keeping to enforce that security role
  has a TARGET_URL or TARGET_PATH, and provenance role has both
  TARGET_URL and ECOSYSTEM, or (b) document explicitly in the
  function comment why TARGET_NAME alone is special. (b) is the
  smaller change.

### MCP `maxLineBytes` frame budget — planned expansion steps

- **Source:** H2 remediation discussion 2026-04-15 (post-Opus review)
- **Severity:** nice-to-have — tracking only
- **Where:** `internal/mcp/jsonrpc.go:maxLineBytes`
- **Sketch:** The current frame cap is 64 KiB, chosen to sit well above
  any legitimate inbound frame our closed schemas accept. If a future
  tool's arguments or a client's legitimate request pattern butts
  against this ceiling, the agreed expansion steps are 128 KiB first,
  then 256 KiB as a hard cap. Beyond 256 KiB the right answer is "use
  a resource URI for big content," not "raise the frame budget" — a
  4 MiB JSON-RPC frame is an anti-pattern regardless of our server's
  ability to receive one. When expanding: bump both the value and the
  doc comment's rationale; re-run `cmd/smoke-mcp` to confirm no
  regression. No action until we observe a legitimate frame near the
  limit.

### golangci-lint baseline has ~24 pre-existing issues across cmd + internal

- **Source:** noticed while wiring up `make check` on 2026-04-15;
  re-baselined 2026-04-25
- **Severity:** should-fix (batch cleanup, not urgent)
- **Where:** `cmd/signatory/serve_lifecycle.go` (noctx + unused),
  `cmd/signatory/handoff_deposit_test.go` (noctx),
  `cmd/signatory/functional_test.go` (noctx),
  `cmd/signatory/posture.go` (staticcheck QF1002),
  `internal/pipeline/client.go` (nilerr + gosec G402 annotated),
  `internal/survey/survey.go` (nilerr), plus the residual errcheck
  set across `internal/store/`, `cmd/signatory/handoff.go`, etc.
- **Sketch:** Running `golangci-lint run ./...` today reports 24
  issues: 9 noctx (stdlib calls that should use the *Context variant
  — `exec.CommandContext`, `net.Dialer.DialContext`, `http.Client.Do`
  with a request, `sql.QueryRowContext`), 7 errcheck (mostly the
  `defer x.Close()` pattern that's harmless on read paths but
  un-annotated), 2 nilerr (early-return paths that swallow `err`
  before returning `nil`), 2 unused (dead `defaultPidPath` /
  `defaultLogPath` constants in `serve_lifecycle.go`), 2 gosec, 1
  staticcheck QF1002, 1 bodyclose. The earlier-noted toml.go QF1002
  and analyst_output.go QF1011 are already fixed. Plan: split into
  three small commits — (1) noctx pass (mechanical, propagates
  context already in scope), (2) nilerr + unused + staticcheck
  (small targeted fixes), (3) errcheck pass with `//nolint:errcheck
  // <reason>` annotations. CI does not currently gate on
  golangci-lint; pair this cleanup with a CI addition so the
  baseline can't regrow silently. `make lint` is the local-dev
  forcing function in the meantime.

## Test quality

### `captureStream` could use `goleak.VerifyTestMain`

- **Source:** skill-equipped Opus reviewer 2026-04-14 (F1
  recommendation, deferred)
- **Severity:** nice-to-have
- **Where:** `cmd/signatory/handoff_test.go:TestMain` (doesn't exist
  yet — would need to be added)
- **Sketch:** The defer-close fix in commit e073557 closes the
  immediate goroutine-leak hazard, but `go.uber.org/goleak` would
  catch any future regression and any other goroutine leaks across
  the package. Adding the dep is a project-level decision (the
  project's stance is "minimal deps, vetted carefully"). If
  acceptable, `goleak.VerifyTestMain(m)` in `TestMain` is a one-line
  addition that runs after every test in the package and fails CI
  if any non-stdlib goroutine is still alive.

### Subtest names in `TestSafeGitCloneURL` use the full URL

- **Source:** skill-equipped Opus reviewer 2026-04-14 (F11)
- **Severity:** nice-to-have
- **Where:** `cmd/signatory/handoff_test.go:TestSafeGitCloneURL_*`
- **Sketch:** Several subtests use the URL itself as the name, which
  contains `/` and `:` — `go test -run TestSafeGitCloneURL/<url>`
  becomes shell-unsafe. Add a short `name` slug column to the table
  struct (e.g., `{"github-bare", "https://github.com/foo/bar", ...}`)
  so each case has a stable, -run-friendly identifier. Also makes
  failure output legible.

### `TestHandoff_NetworkPrecheck_AppliesEcosystem` assertion is too loose

- **Source:** skill-equipped Opus reviewer 2026-04-14 (F12)
- **Severity:** nice-to-have
- **Where:** `cmd/signatory/handoff_test.go:701` —
  `assert.Contains(t, string(body), "go")`
- **Sketch:** "go" appears in the provenance template in many places
  (links, prose). The assertion would pass even if the `{ECOSYSTEM}`
  placeholder were left literal. The adjacent `assert.NotContains(...,
  "{ECOSYSTEM}")` is the only real placeholder-substitution guard.
  Tighten to a more specific string the template emits around the
  ecosystem field — e.g., `assert.Contains(body, "ecosystem: go")`
  or whatever the actual rendered structure is.

### `classifyRootFiles` missing dedup test

- **Source:** skill-equipped Opus reviewer 2026-04-14 (F8)
- **Severity:** nice-to-have
- **Where:** `internal/ecosystem/detect_test.go`
- **Sketch:** `classifyRootFiles` deduplicates via a `map[string]struct{}`
  internally; if GitHub ever returns duplicate entries in the root
  listing, the dedup is exercised but no test covers it. A future
  refactor to direct `[]string` iteration could regress. Add
  `TestClassifyRootFiles_DedupesRepeatedNames` asserting a single
  result for `[]string{"go.mod", "go.mod"}`.

## Templates / schema

### Templates hardcode `Language: Python` / `Language: Go` / `Role: CLI application`

- **Source:** dogfood analyst run on `got` 2026-04-14
- **Severity:** should-fix (blocks correct rendering for non-Python/
  non-Go targets)
- **Where:** `templates/handoffs/security-review-v1.md` and
  `security-review-go-v1.md`
- **Sketch:** Add `{LANGUAGE}` and `{ROLE}` placeholders to the
  template body, and have `applyNetworkPrecheck` populate `LANGUAGE`
  from the GitHub primary-language string. The current "Language:
  Python" heading on a TypeScript repo is exactly the footgun
  d037c68's stderr warning was added to surface — but the underlying
  fix is to thread the detected language through to the template.

### Schema has no "library" role for analysis targets

- **Source:** dogfood analyst run on `got` 2026-04-14
- **Severity:** should-fix
- **Where:** template plus possibly `internal/exchange/` schema
- **Sketch:** The `got` engagement was a library, not a CLI. The
  template's hardcoded "Role: CLI application for developers" is
  inaccurate for libraries; the analyst noted there was no clean
  schema slot for "library". Decide whether `Role` should be a
  fixed enum (CLI / library / service / build-tool / shell-augment)
  with a placeholder, or remain free-form prose in the template.

### `target_commit` guidance missing in templates

- **Source:** dogfood analyst run on `got` 2026-04-14
- **Severity:** nice-to-have
- **Where:** templates' "fill the v1 schema" section
- **Sketch:** The handoff doesn't tell the analyst how to fill
  `target_commit` if their environment can't run `git rev-parse`
  (sandboxed agents commonly can't shell out to git). Either pass
  the commit SHA from `signatory handoff` (we know it after
  `--clone-dir` runs) or document explicitly that the field is
  optional and may be left empty.

### Signal-type registry has no HTTP-client semantics

- **Source:** dogfood analyst run on `got` 2026-04-14
- **Severity:** nice-to-have
- **Where:** `design/signal-type-registry.md` and the schema's
  signal_type vocabulary
- **Sketch:** Analyst on `got` had no clean signal_type for "HTTP
  redirect credential stripping" or "diagnostics-channel leakage"
  and reused `data_minimization_policy` as a best-fit. Add HTTP-
  client-specific signal types (`http_redirect_handling`,
  `request_logging_hygiene`, `transport_layer_secrets_handling`)
  when the registry next opens for additions.

### Format-check instruction conflict in handoff template

- **Source:** dogfood analyst run on `got` 2026-04-14
- **Severity:** nice-to-have
- **Where:** `templates/handoffs/security-review-v1.md` (and go variant)
- **Sketch:** The template tells the analyst to run `signatory
  format-check` as a pre-flight, but the orchestrator (signatory)
  may also be running it after receipt. Document the contract:
  who runs format-check, when, and what the analyst should do if
  they CAN'T run it (e.g., no signatory binary in their environment).

## Comment / cleanup

### Comments reference internal review-pass IDs

- **Source:** skill-equipped Opus reviewer 2026-04-14 (F9)
- **Severity:** nice-to-have
- **Where:** `internal/config/init.go:144` ("config reviewer F11"),
  `internal/config/handoff.go:123` ("Reviewer F2"), and others
- **Sketch:** These references made sense during the review pass but
  will be impenetrable noise in 6 months. Drop the F-numbers; keep
  the rationale prose. Grep for "reviewer F" and "config reviewer"
  to find them.

### Magic 2-minute clone timeout

- **Source:** unaided cmd reviewer 2026-04-14 (F9, deferred)
- **Severity:** nice-to-have
- **Where:** `cmd/signatory/handoff.go:applyClone`
- **Sketch:** `context.WithTimeout(ctx, 2*time.Minute)` is a buried
  magic constant. Failure mode on timeout is "git clone failed:
  context deadline exceeded" — not legible as "signatory killed git
  after 2 minutes". Extract to a named constant
  (`defaultCloneTimeout = 2 * time.Minute`) at package level. Wrap
  the timeout error with "clone exceeded 2m timeout; if this is a
  slow network or large repo, clone manually and pass --path".
  Optional follow-up: a `--clone-timeout` flag.

### `ClassifyTarget` error message points the wrong way

- **Source:** unaided cmd reviewer 2026-04-14 (F5, deferred)
- **Severity:** nice-to-have
- **Where:** `internal/config/handoff.go:HandoffSubstitutions` error
  path
- **Sketch:** When `ClassifyTarget` returns `TargetUnknown`, the
  error message says "TARGET_NAME could not be inferred — pass
  --name". But the real problem is that the target wasn't
  recognized as either URL or path; `--name` alone won't fix the
  rendered handoff (TARGET_URL and TARGET_PATH will still be
  missing). Improve to: "target %q was recognized as neither a URL
  (https://…) nor a path (./foo, /abs/foo); pass --url or --path
  to disambiguate."

### TOML parser would benefit from a fuzz corpus

- **Source:** skill-equipped TOML-adversarial agent 2026-04-14
  (recommendation, skipped due to no-commit constraint at the time)
- **Severity:** nice-to-have
- **Where:** `internal/config/toml_test.go`
- **Sketch:** Add `FuzzDecodeTOML` using `testing.F` (Go 1.18+).
  Seed corpus from the existing happy-path tests + each adversarial
  case. CI runs the seed; ad-hoc `go test -fuzz=FuzzDecodeTOML
  -fuzztime=1m` runs the engine. Catches inputs the hand-crafted
  cases miss.

## v0.2 milestone gates

Items that are deferred by explicit architectural decision until a
later version. Not "should fix soon" — "audit and decide at the v0.2
boundary before acting on."

### Audit `github.com/modelcontextprotocol/go-sdk` for v0.2 MCP work

- **Source:** MCP design session 2026-04-15 (architectural decision
  locked in `design/mcp-server-architecture.md` — v0.1 hand-rolls the
  protocol; v0.2 is when the SDK's abstractions start paying off)
- **Severity:** gate — don't adopt without audit
- **Where:** dependency decision for `cmd/signatory/mcp.go` +
  `internal/mcp/`
- **Sketch:** The official Go SDK at
  <https://github.com/modelcontextprotocol/go-sdk> would save us the
  hand-rolled protocol plumbing and give us free upgrade paths for
  HTTP/SSE transport, progress notifications, and resource
  subscriptions — all v0.2+ features. Before adopting, audit:
  - Transitive dependency footprint (we care: it's in the critical
    path for every MCP call, and signatory's whole product is about
    supply-chain trust)
  - Maintenance cadence and author identity (MCP authors themselves,
    per the project URL, which is reassuring; still verify)
  - API stability commitments given MCP is a young spec
  - Size of the library vs. what we actually consume from it (a 10K-line
    library for 1K lines of our needs is a different trade than a
    2K-line library)
  - Whether the library's abstractions compose with signatory's
    uniform-response-envelope and metadata-flag confirmation patterns,
    or whether adopting it forces us to restructure those
  Audit output goes into a `design/mcp-sdk-audit.md` note. Decision at
  v0.2 planning: adopt and migrate Phase 1 code, or stay hand-rolled.



When a review surfaces something we can't fix in the same change,
append an entry with:
- A short, action-shaped heading
- The source review (commit hash / agent / conclusion ID)
- Severity per the convention above
- A specific sketch of what to do — not "consider improving"

When the fix lands, delete the entry. The git history of this file
is the record of what we deferred and why.
