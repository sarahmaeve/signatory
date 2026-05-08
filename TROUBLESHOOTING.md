# Troubleshooting

Common failure modes when getting signatory running, organized by phase. Each
entry: what you see, why it happens, how to fix, and a command to confirm.

If your symptom isn't here, start with `signatory doctor` — it runs
the full breadth pass (Go runtime, env vars, TLS trust, MCP wiring,
store, service) and points at the matching section below for any
probe that fails. `signatory certs doctor` is the deeper TLS
diagnostic when the `node-extra-ca-certs` probe trips, and
`signatory show-analyses` lists what's already in the store.

## Install and build

### `signatory version` reports `dev` or no commit

**Cause.** The binary was built without ldflags — typically because you ran
`go install ./cmd/signatory` directly instead of `make install`.

**Fix.**

    make install

The Makefile stamps `version`, `commit`, and `buildDate` so a stale binary
is one command away from being spotted.

### Binary is stale after a recent commit

**Cause.** The post-commit hook isn't wired into this clone. Git's
`core.hooksPath` is per-clone and not tracked in the repo.

**Fix.**

    make install-hooks

Idempotent. Activates `.githooks/` (pre-commit gauntlet + post-commit
auto-rebuild against `$GOBIN`).

### `make check` fails on a fresh clone

**Cause.** Most likely `gofmt`, `go vet`, or `go test -race` flagged
something. `lint` is intentionally **not** in `make check` — pre-existing
lint baseline noise would make the gate useless.

**Fix.** Address the failing target. If `gofmt` flags files you didn't
touch, fix them in the same commit (project rule).

### `go install …@latest` works but produces a less useful binary

**Why.** `go install` from a module path skips the ldflags stamp, so
`signatory version` says `dev` and the MCP handshake's `serverInfo.version`
is unhelpful. Use the clone + `make install` flow when contributing or
debugging — and match builds across `~/go/bin/signatory` and your
`.mcp.json` `command` path.

## TLS and certificates

### `/analyze` errors with `unknown CA` or `x509`

**Cause.** `NODE_EXTRA_CA_CERTS` isn't set, or it points at a missing or
invalid CA.

**Fix.**

    signatory certs check
    signatory certs init     # if check fails
    signatory certs doctor   # verbose diagnostic

`certs init` installs the managed CA and offers to append the export block
to your shell profile.

### `certs check` passes but Claude Code still rejects the cert

**Cause.** The shell profile was updated, but the running Claude Code
process inherited the old environment.

**Fix.** Restart Claude Code, or re-source your profile in the terminal
that launched it. `certs doctor` prints the resolved env vars so you can
confirm what the process actually sees.

## MCP and Claude Code wiring

### `/analyze` and `/vet-dependency` aren't visible in Claude Code

**Cause.** Claude Code wasn't launched from the signatory clone, so the
project-scoped `.mcp.json` and `.claude/skills/` didn't load.

**Fix.** Either `cd` into the signatory clone and run `claude`, or copy
`.mcp.json` and `.claude/skills/` into the project you want to evaluate
deps from.

### MCP server starts but tools error immediately

**Cause.** The MCP server and the CLI are looking at different stores —
typically because `$SIGNATORY_DB` is set in one environment and not the
other.

**Fix.** Pin the database path in both. Either export `SIGNATORY_DB` in
your shell profile, or set it explicitly in `.mcp.json`'s `env` block.

### `.mcp.json` references `${HOME}/go/bin/signatory` but my `GOBIN` is elsewhere

**Cause.** The default config assumes `GOBIN=~/go/bin`.

**Fix.** Replace the `command` value in `.mcp.json` with the absolute path
of your installed binary (`which signatory`).

## /analyze pipeline

### `signatory_analyze` returns NotFound

**Not a failure.** NotFound means the target hasn't been ingested yet —
the signal to run collection. Invoke `/analyze <target>` in Claude Code.
Don't retry the lookup.

### `/analyze` re-clones a repo every run

**Cause.** The analyst agent isn't finding the cached signals, usually
because the canonical URI in the prompt doesn't match the row in the
store. Common causes: ecosystem-prefix drift (`pkg:go` vs `pkg:golang`)
or a version-suffixed URI matching nothing.

**Fix.**

    signatory summary <target>          # see what is actually stored
    signatory prune duplicates          # consolidate prefix drift
    signatory prune versioned           # remove pre-v10 @V-suffixed rows

### Analyst agent uses `curl` or `WebFetch` for data already in the store

**Bug.** Agents should hit MCP first (cache lookup) and fall back to
network only on miss. The ROADMAP's economics rule prohibits redundant
collection. File an issue with the session ID
(`signatory analysis list`).

### Agent tries to POST via `WebFetch` and fails

**Architectural constraint.** Claude Code's `WebFetch` is GET-only. The
pipeline service exists specifically to work around this; agents should
write through MCP, never POST. If a prompt asks for a POST, the prompt
is wrong.

## Store and data drift

### `pkg:npm/<name>@<version>` posture is silently ignored when `--version` is unset

**Known dogfood gap** (logged 2026-04-20). Workaround: pass `--version`
explicitly, or use the entity-level URI (`pkg:npm/<name>` without `@V`).

### Duplicate entity rows after ecosystem-prefix drift

**Symptom.** `signatory show-analyses` lists the same project under both
`pkg:go/...` and `pkg:golang/...`, or duplicates with and without `@V`
suffixes.

**Fix.**

    signatory prune duplicates    # case-fold + prefix + @V cleanup
    signatory prune versioned     # remove pre-v10 @V rows
    signatory prune orphans       # delete entities with no children

`prune` operations have dry-run plans; check them first.

### Need to undo a posture or burn

**Fix.**

    signatory posture unset <target>
    signatory burn remove <target>

Both are soft-deletes — historical rows remain in the audit log.

## Telemetry (optional)

### OTEL traces don't flow despite hooks firing

**Cause.** `.claude/settings.json`'s `env` block doesn't reliably propagate
to Claude Code's own process; the contradiction with the official docs is
unresolved.

**Fix.** Use the wrapper:

    ./scripts/dogfood-claude.sh

It exports the OTEL env vars before exec-ing `claude`, so traces land in
`dogfood-metrics/raw/traces.jsonl` regardless of which layer is right.

## More state when stuck

- `signatory doctor` — breadth pass over the local setup
- `signatory doctor --json` — same, structured for scripts
- `signatory doctor --strict` — promote any warning to a non-zero exit (CI gate)
- `signatory version` — built version + commit + build date
- `signatory certs doctor` — TLS environment dump (deeper than `doctor`'s `node-extra-ca-certs` probe)
- `signatory analysis list` — recent pipeline sessions
- `signatory analysis show <session-id>` — what an analyst session ingested
- `signatory show-analyses` — every analyst output in the store
- `signatory summary <target>` — full breadth pass on one entity
