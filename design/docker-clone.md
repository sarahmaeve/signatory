# Docker-Sandboxed Cloning

**Status:** Scope sketch recorded 2026-05-01 from a dogfood
conversation about analyzing a likely-compromised target. Records
the design before implementation; **no code changes proposed
here**.

**Scope:** Optional Docker-based jail for the `git clone`
subprocess, configured per-host and keyed by analysis session.
Uses signatory's existing `--clone-dir` / `--path` /
`--analysis-session-id` flags; no new top-level commands.

Cross-references:
- [`internal/gitenv/env.go`](../internal/gitenv/env.go) — the
  in-process env scrubbing this layers on top of, not replaces.
- [`cmd/signatory/collectors.go`](../cmd/signatory/collectors.go)
  §`defaultRunGit` (line 426) — the runner-injection chokepoint
  every git subprocess flows through.
- [`cmd/signatory/handoff.go`](../cmd/signatory/handoff.go)
  §`defaultGitClone` (line 861), §`safeGitCloneURL` (line 1168) —
  parallel clone path and the URL-shape allowlist.
- [`.claude/skills/analyze/SKILL.md`](../.claude/skills/analyze/SKILL.md)
  — the pipeline recipe that needs a one-line edit (§9).

## 1. Motivation

The current clone path is well-defended at the *process* level:
`gitenv` strips every `GIT_*`, `SSH_ASKPASS*`, and libcurl proxy
variable; `safeGitCloneURL` rejects ssh:// schemes, embedded
credentials, query strings, and fragments; clone subprocesses
get a `WaitDelay` to bound forked grandchildren. That stops
the most common credential-leak and process-injection shapes.

What it doesn't stop:

1. **Clone-time bugs in git itself.** Git's URL parser, ref
   resolver, and pack-protocol implementation have produced RCE
   CVEs in the past and will again. In-process hardening doesn't
   help when the bug is in the binary signatory invokes.
2. **Direct outbound network.** A compromised git can open raw
   sockets to attacker-controlled IPs to exfiltrate or pull a
   second-stage payload during the clone window. `HTTPS_PROXY`
   stripping prevents the proxy-redirection variant; it doesn't
   prevent a malicious binary from ignoring proxy vars entirely.
3. **Filesystem reach.** A clone-time RCE inherits the operator's
   UID; it can read `~/.signatory/signatory.db`, `~/.ssh/`, and
   anything else the user can.

The typical signatory user runs `/analyze` against packages they
already suspect. The threat model where this matters is exactly
that case: pointing the pipeline at a known-bad target. We want
a mode the operator can turn on for those runs without changing
the default for benign targets.

## 2. Architecture

```
                    ┌──────────────────────────────────────┐
                    │   Host                               │
                    │                                      │
                    │   ~/.signatory/clones/<sid>/<name>/  │
                    │   ▲                                  │
                    │   │ bind mount                       │
                    │   │                                  │
  signatory─────────┼───▶  docker run --rm                 │
  (handoff/         │      ──network signatory-clone-net   │
   analyze)         │      ──user $(id -u):$(id -g)        │
                    │      ──cap-drop=ALL                  │
                    │      ──read-only                     │
                    │      alpine/git clone <url> /clones  │
                    │      │                               │
                    │      ▼                               │
                    │   ┌──────────────────────┐           │
                    │   │ clone container      │           │
                    │   │ (ephemeral, no NAT)  │           │
                    │   └──────────┬───────────┘           │
                    │              │ https_proxy           │
                    │              ▼                       │
                    │   ┌──────────────────────┐           │
                    │   │ signatory-egress     │ ──┐       │
                    │   │ (CONNECT proxy,      │   │       │
                    │   │  hostname allowlist) │   │       │
                    │   └──────────────────────┘   │       │
                    └──────────────────────────────┼───────┘
                                                   ▼
                                            github.com:443
                                            proxy.golang.org:443
                                            registry.npmjs.org:443
                                            …
```

**Two Docker networks:**
- `signatory-clone-net` — `--internal` (no NAT). The clone
  container's only network. Its sole reachable peer is the
  proxy.
- `signatory-egress-net` — standard bridge. Has internet. The
  proxy container is the only member.

**Two containers:**
- `signatory-egress` — long-lived. Built from signatory's own
  binary. Runs `signatory clone-proxy serve`. Has interfaces on
  both networks. Operator brings it up once and leaves it.
- *(per-run, ephemeral)* — short-lived `alpine/git` container
  that performs one `git clone`. Created and destroyed inside a
  single signatory invocation. No name; `--rm` gone the moment
  clone exits.

The `--internal` network is what makes the proxy mandatory: the
clone container has no IP route to the public internet, so it
cannot bypass the allowlist by opening a raw socket.

## 3. Lifecycle: per-analysis-session

The clone for a given analysis lives at:

```
<host-clones-parent>/<analysis-session-id>/<repo-short-name>/
```

The path is fully derivable from config + the
`--analysis-session-id` flag + target resolution. No DB lookup
required.

**Creation.** First command in the session that needs the clone
creates the dir and runs the docker'd `git clone`. Subsequent
commands in the same session — `signatory handoff provenance`,
`signatory analyze --refresh --path`, agent `Read` calls —
operate against the same path on the host.

**Reuse.** Because the path is deterministic, the second
handoff and the `analyze --refresh` step both compute the same
location and find the existing clone. No coordination beyond the
shared session id.

**Cleanup.** `signatory analysis end <sid>` `rm -rf`s
`<host-clones-parent>/<sid>/` regardless of terminal status
(`completed`, `failed`, `partial`). The session row itself
stays — we delete the bytes, not the audit record.

**Out-of-session use.** Ad-hoc invocations (no
`--analysis-session-id`) under `clone.runner: docker` are
rejected with a clear error pointing the user at
`signatory analysis begin`. We don't auto-mint orphan session
ids; that path silently leaks dirs.

## 4. CLI integration — no new commands

The runner switch is config-only. Existing flags keep their
meaning; behavior changes only when the config opts in.

**Config block** (added to `~/.signatory/config.yaml`):

```yaml
clone:
  runner: docker            # default: direct
  network: signatory-clone-net
  proxy: signatory-egress:8443
  host-clones-parent: ~/.signatory/clones
```

Validation: under `runner: docker`, all four fields are
required. `host-clones-parent` accepts `~` expansion. Empty
config (or `runner: direct`) preserves today's behavior bit-for-
bit; existing tests stay green by default.

**Path derivation, not path passing.** The /analyze pipeline
runs four signatory commands (two `handoff`s, one
`analyze --refresh`, one `analysis end`) that all need to agree
on the clone location. Having the orchestrator script compose a
path from shell variables and pass it via `--path` works for one
runner mode but not both — the docker layout has an extra
`<sid>/` segment direct mode lacks, and a single recipe can't
hold two shapes.

The fix: signatory derives the path itself. The session id
plus target plus config is sufficient; no orchestrator
composition needed.

| Flag | Direct runner | Docker runner |
|---|---|---|
| `--clone-dir DIR` | optional; defaults to `<host-clones-parent>` | optional; if set, must equal `<host-clones-parent>` (mismatch → hard-fail) |
| `--path DIR` | optional; defaults to derived path | optional; if set, must resolve to derived path (mismatch → hard-fail) |
| `--analysis-session-id SID` | optional (preserves today's behavior) | required for any command that touches the clone |
| `--clone` (handoff and analyze) | idempotent: clones if derived path is empty, no-op if a valid clone is already there |

The derived path is:
- direct: `<host-clones-parent>/<resolved-short-name>/`
- docker: `<host-clones-parent>/<sid>/<resolved-short-name>/`

Resolved short name comes from `profile.ResolveTarget`, which
already runs at the start of every clone-touching command.

**Two small CLI additions:**

1. `signatory analyze --refresh` gains `--analysis-session-id`
   (same shape as on `signatory handoff`). Optional under direct
   mode; required under docker mode.
2. `signatory handoff <role>` gains `--clone` (matches the existing
   flag on `signatory analyze`). Idempotent — no-op if the derived
   path already holds a valid full clone of the target. With
   `--clone` absent and the path empty, handoff fails with the
   same "no clone found" error it does today.

Under both additions the default behavior under `runner: direct`
is unchanged: existing scripts that pass explicit `--path` and
`--clone-dir` keep working.

## 5. Components to build

### 5.1 `internal/cloneproxy/`

Standard HTTP CONNECT proxy in Go. Public surface is small:

```go
type Server struct {
    Addr      string   // listen address, e.g. ":8443"
    Allowlist []string // hostname suffixes, e.g. ["github.com", ".github.com"]
    Logger    *slog.Logger
}

func (s *Server) Serve(ctx context.Context) error
```

Implementation notes:
- Only the `CONNECT` method is honored. `GET`/`POST`/etc. → `405`.
- Hostname suffix match: `github.com` matches `github.com` and
  any `*.github.com`. IP literals are always rejected.
- Port whitelist: 443 only. 80, 22, anything else → `403`.
- Read once from config at startup; no SIGHUP reload (per the
  decisions on this thread).
- Per-request structured log line: timestamp, source IP, target
  host:port, allow/deny, bytes-in/out on close.

### 5.2 `signatory clone-proxy` (CLI)

Lifecycle subcommands mirror `signatory serve` and
`dogfood-metrics`. The operator runs `start`/`stop`/`status`;
`serve` is the in-process worker that runs as the container's
entrypoint and is also useful for `--no-docker` dev/test runs.

```go
type CloneProxyStartCmd struct {
    Image     string   `default:"signatory-clone-proxy:local"`
    Container string   `default:"signatory-egress"`
    EgressNet string   `name:"egress-net" default:"signatory-egress-net"`
    CloneNet  string   `name:"clone-net"  default:"signatory-clone-net"`
    Port      int      `default:"8443"`
    Allow     []string `name:"allow" help:"hostname suffix to allow; repeat. Empty → read from config."`
}

type CloneProxyStopCmd struct {
    Container  string `default:"signatory-egress"`
    KeepNetworks bool `name:"keep-networks" help:"Leave the docker networks in place; default tears them down."`
}

type CloneProxyRestartCmd struct {
    CloneProxyStartCmd  // embedded, same flags
}

type CloneProxyStatusCmd struct {
    Container string `default:"signatory-egress"`
}

type CloneProxyLogsCmd struct {
    Container string `default:"signatory-egress"`
    Follow    bool   `short:"f"`
}

type CloneProxyServeCmd struct {  // in-process worker
    Addr  string   `default:":8443"`
    Allow []string `name:"allow"`
}
```

Behavior:

- **start** — creates the two docker networks if absent, runs
  the proxy container detached on `egress-net`, connects it to
  `clone-net`. Refuses to clobber an already-running container
  (parallel to `ServeStartCmd`'s pidfile check). Idempotent re-
  invocation is fine; "already running" is exit 0 with a stderr
  note.
- **stop** — `docker rm -f` the container; remove networks
  unless `--keep-networks`. Returns `errNotRunning`-style
  sentinel when nothing was running, mirroring
  `ServeStopCmd` so scripts can use `|| true`.
- **restart** — embeds `CloneProxyStartCmd` and runs
  `Stop` then `Start` (same pattern as `ServeRestartCmd`).
- **status** — exit 0 if the container is running and reachable
  via `docker exec … echo ok`, non-zero otherwise. Prints a
  one-line summary: container state, networks attached,
  allowlist size. Drives `signatory clone-proxy status >/dev/null
  2>&1 || signatory clone-proxy start` in scripts (same idiom
  the /analyze SKILL.md already uses for `signatory serve`).
- **logs** — `docker logs <container>` with optional `-f`. Useful
  for watching allowlist denials in real time.
- **serve** — runs the proxy in-process. The Dockerfile's
  ENTRYPOINT is `signatory clone-proxy serve`, so this is what
  actually executes inside the container. Also runnable on the
  host for tests or `--no-docker` dev mode.

State location: `~/.signatory/clone-proxy/` for any cached
parameters (matches `~/.signatory/serve/` for the pipeline
service). Liveness is queried via `docker inspect`, not a host
pidfile, since the proxy lives inside Docker — `docker inspect`
is the source of truth, no separate pidfile to drift.

### 5.3 `dogfood/clone-proxy.Dockerfile`

```dockerfile
FROM scratch
COPY signatory /usr/local/bin/signatory
EXPOSE 8443
ENTRYPOINT ["/usr/local/bin/signatory", "clone-proxy", "serve"]
```

Built via a new `make clone-proxy-image` target. Tag
`signatory-clone-proxy:local`. Not published anywhere; built and
used locally.

### 5.4 `dockerRunGit` (sibling of `defaultRunGit`)

Lives alongside `defaultRunGit` in `cmd/signatory/collectors.go`.
Same signature, different body:

```go
func dockerRunGit(cfg CloneConfig) RunGitFunc {
    return func(ctx context.Context, workdir string, args ...string) error {
        // Compose `docker run` argv, bind-mount workdir or
        // resolve from session id, route through proxy.
        ...
    }
}
```

The selection between `defaultRunGit` and `dockerRunGit` happens
once per process at the same place runners are currently injected
(handoff and analyze command setup).

### 5.5 Config schema

`internal/config/` learns the `clone:` block. Validation runs at
config-load time; runner: docker without a complete block fails
loud.

### 5.6 `analysis end` cleanup hook

`signatory analysis end <sid>` already runs at the terminal step
of every /analyze run. Add a post-DB-update hook that, when
`clone.runner: docker`, removes `<host-clones-parent>/<sid>/`.
Idempotent: missing dir is fine.

## 6. Operator setup (one-time)

```bash
# 1. Build the proxy image from signatory source.
make clone-proxy-image

# 2. Edit ~/.signatory/config.yaml; set clone.runner: docker
#    and the allowlist under clone.allow:.

# 3. Bring up the proxy + networks.
signatory clone-proxy start
```

`clone-proxy start` creates both Docker networks if absent,
runs the proxy container detached, and attaches it to the
clone-side network. The allowlist is read from config; pass
`--allow` to override per invocation.

The /analyze recipe gains a preflight line, mirroring how it
already pre-flights the pipeline service:

```bash
signatory clone-proxy status >/dev/null 2>&1 \
  || signatory clone-proxy start
```

Tear-down:
```bash
signatory clone-proxy stop                 # default: also removes networks
signatory clone-proxy stop --keep-networks  # leave networks for next start
```

## 7. Failure modes

All hard-fail; no silent fallbacks.

| Trigger | Surface |
|---|---|
| Docker daemon unreachable | `docker daemon unreachable: <err>` from the first `docker run` attempt |
| `signatory-egress` not running | `clone proxy not running on signatory-clone-net (start with: docker start signatory-egress)` |
| `signatory-clone-net` missing | `network signatory-clone-net not found (re-run operator setup §6)` |
| `--clone-dir` mismatches `host-clones-parent` | `--clone-dir must equal <host-clones-parent> when clone.runner=docker (got: <user-value>)` |
| `--path` not under `<host-clones-parent>/<sid>/` | `--path must resolve under <expected> (got: <user-value>)` |
| `--analysis-session-id` missing under docker mode | `--analysis-session-id is required when clone.runner=docker` |
| Proxy denies hostname | `git clone` exits non-zero; stderr from `docker run` is surfaced verbatim, including the proxy's `403` line |
| `clone:` config block incomplete | `clone.runner=docker requires network, proxy, host-clones-parent` at config-load |

## 8. Test plan

The contract: **every existing test stays green with no
modification.** Docker mode is opt-in via config; default
(`runner: direct`) keeps every current code path identical.

### 8.1 Existing tests preserved

| Test | What it exercises today | Why it stays green |
|---|---|---|
| `analyze_clone_test.go` | Full clone via `RunGit` injection | Tests pass a custom `RunGit` closure; never reaches `defaultRunGit`. Docker swap happens upstream of the closure. |
| `analyze_git_functional_test.go` | git fixture in `t.TempDir()` | Uses a local file path as URL; runner: direct. No config block in test setup. |
| `collectors_test.go` | `ensureCloneAtPath` happy path + errors | Same as above — runner injection, no docker. |
| `handoff_test.go`, `handoff_provenance_signals_test.go` | `--clone-dir` derivation, validation, deposit | `RunGitClone` closure injection bypasses `defaultGitClone`. |

The runner-injection seams (`AnalyzeCmd.RunGit`,
`HandoffCmd.RunGitClone`) keep doing what they do; the config-
driven swap chooses between `defaultRunGit` and `dockerRunGit`
*as the fallback* when the seam is `nil`. Tests always inject,
so they never hit either default.

### 8.2 New unit tests

**`internal/cloneproxy/`:**

```
TestProxy_AllowsListedHost           // CONNECT github.com:443 → 200
TestProxy_RejectsUnlistedHost        // CONNECT attacker.example:443 → 403
TestProxy_SuffixMatch_Subdomain      // .github.com matches gist.github.com
TestProxy_SuffixMatch_NoLeak         // notgithub.com is NOT matched by github.com
TestProxy_RejectsIPLiteral           // CONNECT 8.8.8.8:443 → 403
TestProxy_RejectsBadConnect          // malformed request → 400
TestProxy_RejectsNonConnectMethod    // GET / → 405
TestProxy_RejectsBadPort             // CONNECT github.com:22 → 403
TestProxy_LogsRequest                // structured log line shape
TestProxy_GracefulShutdown           // ctx cancel drains in-flight CONNECTs
```

Implementation: drive the listener directly with a
`net.Pipe()` pair; no real TCP needed for the request-shape
tests. The "dial succeeds" path can be faked with a stub
backend dialer.

**`signatory clone-proxy` lifecycle commands** — mirror the
existing `serve_lifecycle_test.go` patterns; mock the
`docker` invocation (DI a `dockerExec` function) so tests
don't need a daemon:

```
TestCloneProxyStart_CreatesMissingNetworks
TestCloneProxyStart_ReusesExistingNetworks
TestCloneProxyStart_RefusesIfAlreadyRunning  // mirrors ServeStart pidfile check
TestCloneProxyStop_RemovesContainerAndNetworks
TestCloneProxyStop_KeepNetworksFlag
TestCloneProxyStop_ErrNotRunning_WhenAbsent  // sentinel for `|| true` scripts
TestCloneProxyStatus_Running                 // exit 0
TestCloneProxyStatus_NotRunning              // non-zero, errStatusNotRunning
TestCloneProxyRestart_ComposesStopThenStart  // verify Stop is called, then Start
TestCloneProxyLogs_PassesFollowFlag
TestCloneProxyServe_NoDockerMode             // in-process, listens on --addr
```

**`cmd/signatory` (config + flag wiring):**

```
TestCloneConfig_DefaultDirect           // empty config → runner: direct
TestCloneConfig_DockerRequiresNetwork   // runner: docker, no network → validation err
TestCloneConfig_DockerRequiresProxy     // runner: docker, no proxy → validation err
TestCloneConfig_DockerRequiresParent    // runner: docker, no host-clones-parent → err
TestCloneConfig_TildeExpansion          // ~/.signatory/clones expands
TestCloneFlag_CloneDirOutsideParent_Rejected
TestCloneFlag_PathOutsideSession_Rejected
TestCloneFlag_MissingSessionID_Rejected
TestAnalyze_AnalysisSessionID_Required_UnderDocker
TestAnalyze_AnalysisSessionID_Optional_UnderDirect
```

**Path derivation** — central to the new "no path passing"
shape. The orchestrator never composes paths, so signatory's
derivation must agree with itself across every call site:

```
TestDerivePath_Direct_FromSessionAndTarget
TestDerivePath_Docker_IncludesSessionIDSegment
TestDerivePath_AgreesAcrossCommands     // same sid+target → same path from
                                        // handoff/analyze/analysis-end paths
TestDerivePath_RespectsExplicitClonedir // explicit flag wins; no derivation override
TestDerivePath_RespectsExplicitPath     // same for --path
TestDerivePath_RejectsExplicitMismatch  // explicit flag that disagrees with
                                        // the would-be-derived path → hard-fail
```

**Idempotent `--clone` on handoff:**

```
TestHandoffClone_Idempotent_NoOpWhenValidCloneExists
TestHandoffClone_ClonesWhenPathEmpty
TestHandoffClone_ClonesWhenPathMissing
TestHandoffClone_RejectsWrongOriginAtDerivedPath  // path holds a different repo
                                                  // → ErrOriginMismatch (existing
                                                  // sentinel from collectors.go)
```

**`dockerRunGit` argv composition** (no docker daemon needed):

```
TestDockerRunGit_BuildsExpectedArgs    // url + dest + cfg → docker run argv
TestDockerRunGit_BindMountsHostPath
TestDockerRunGit_PassesProxyEnv
TestDockerRunGit_DropsCapabilities
TestDockerRunGit_UserMatchesHost       // --user $UID:$GID
```

These test the argv builder as a pure function; the actual
`exec.Command("docker", ...)` is dependency-injected so tests
assert on the constructed argv without needing Docker.

**Cleanup hook:**

```
TestAnalysisEnd_DockerMode_RemovesCloneDir       // dir present, gets rm-ed
TestAnalysisEnd_DockerMode_AbsentDir_Quiet       // no error if already gone
TestAnalysisEnd_DirectMode_NoCleanup             // direct mode never touches host-clones-parent
TestAnalysisEnd_FailedStatus_StillCleansUp       // terminal status doesn't matter
```

### 8.3 New smoke test (gated)

`cmd/clone-sandbox-smoke/main.go` — end-to-end, requires Docker.
Pattern matches the existing `cmd/dogfood-metrics-smoke/`.

```
1. Pre-flight: `docker info` must succeed; if not, skip with
   "Docker not available" (not a failure).
2. Bring up the proxy + networks via the operator-setup
   commands inside a t.TempDir() to keep state isolated.
3. Set host-clones-parent to a t.TempDir().
4. Begin an analysis session; capture the SID.
5. Run `signatory handoff security <small-public-repo>
   --analysis-session-id <sid> --clone --network-precheck`.
   (No --clone-dir, no --path — derivation under test.)
6. Assert: clone bytes appear at <host-clones-parent>/<sid>/<name>/.git/.
7. Assert: a representative file (e.g., README) is present.
8. Assert: `docker ps -a` shows no leftover clone container
   (--rm worked).
9. Run `signatory handoff provenance <same-repo>
   --analysis-session-id <sid> --clone …`. Assert it's a
   no-op against the existing clone (idempotency check).
11. Run `signatory handoff <a target on a non-allowlisted host>`;
    assert non-zero exit and that the proxy's 403 appears in
    stderr.
12. Run `signatory analysis end <sid>`; assert
    <host-clones-parent>/<sid>/ is gone.
13. Tear down networks + proxy container.
```

CI integration: gated behind `SIGNATORY_DOCKER_SMOKE=1`. The
default CI run keeps not requiring Docker. A nightly job (or
manual local run) sets the env and exercises the smoke.

The smoke test is **descriptive**, not prescriptive — it
documents what a successful local docker setup looks like and
catches regressions in the integration. It is not the security
test. The security tests are the unit tests in
`internal/cloneproxy/` (allowlist correctness) and the argv
composition tests (no shell injection, correct flags applied).

## 9. Migration impact

The /analyze recipe drops every explicit clone path. The
orchestrator script never composes a path, never calls
`basename` on the target, never differs between runner modes —
signatory derives the location from the session id at every
step.

`.claude/skills/analyze/SKILL.md` Step 1:

```diff
  signatory handoff security "$TARGET" \
    --analysis-session-id "$ANALYSIS_SID" \
-   --network-precheck --clone-dir filestore/clones/ \
+   --network-precheck --clone \
    --deposit-to "$SESSION_ID"

- TARGET_NAME=$(basename "$TARGET" .git)
  signatory handoff provenance "$TARGET" \
    --analysis-session-id "$ANALYSIS_SID" \
-   --network-precheck --path "filestore/clones/$TARGET_NAME" \
+   --network-precheck --clone \
    --deposit-to "$SESSION_ID"
```

Step 1b:

```diff
- # TARGET_NAME is already set by Step 1 (basename derivation
- # matches what handoff --clone-dir wrote).
  signatory analyze --refresh \
-   --path "filestore/clones/$TARGET_NAME" \
+   --analysis-session-id "$ANALYSIS_SID" \
    "$TARGET"
```

`--clone` on both handoffs is the idempotent shape from §4: the
first call clones; the second sees a valid clone at the same
derived path and is a no-op. Same recipe under both runners; the
only thing that changes between modes is where the bytes
physically land.

The clone location moves from `filestore/clones/<name>/` to
`~/.signatory/clones/<sid>/<name>/` (docker) or
`~/.signatory/clones/<name>/` (direct). Both locations are
outside the project repo, which was wanted independently. Users
running ad-hoc `signatory handoff` / `signatory analyze` outside
the /analyze pipeline can still pass explicit `--path` /
`--clone-dir` if they want the bytes somewhere else; the
derivation only kicks in when those flags are absent.

## 10. Out of scope

- **Sandboxing post-clone reads.** `git log`, `git for-each-ref`,
  `git rev-list`, and analyst `Read`/`Glob`/`Grep` continue to
  run on the host against the now-host-readable clone bytes.
  Containerizing those is option (2) from the threat-model
  conversation; deferred unless we see a real attack shape that
  needs it.
- **Sandboxing the analyst agents.** They run in Claude Code,
  not in signatory; we don't control their process model.
- **Linux-native alternatives** (`bubblewrap`, `firejail`).
  Could be a sibling `runner: bubblewrap` mode later; the
  config schema supports it (the runner field is a string, not
  a bool).
- **Linux/macOS feature parity for the operator setup.** Docker
  works on both; the recipe is identical. We don't ship a Lima
  template, a Colima recipe, or anything else here.
