# Signatory: Dogfood-Surfaced Errors

Items surfaced specifically by running signatory's own pipeline
against real targets — the bugs you only find when actually using
the tool end-to-end. Distinct from `pendingfix.md` (which
aggregates findings from reviews and adversarial passes); this
file is the dogfood-specific lane so the source-class is preserved
and the "we found this by using the tool" stories don't get
diluted into the general backlog.

The file also carries worked examples of manual processes that
ran correctly — runbooks distilled from a real successful walk
so the next person doesn't have to re-derive them. Those are
treated as stable reference material, not transient bugs.

Lifecycle conventions:
- **Error entries:** when fixed, delete rather than marking done
  — the git history is the record.
- **Worked-example entries:** keep stable. Update when the
  tooling or process meaningfully changes; otherwise leave alone.

## Conventions

Each item has:
- **Found:** date + the dogfood run that surfaced it (target +
  session id, so anyone can re-walk the evidence)
- **Severity:** must-fix / should-fix / nice-to-have
- **Where:** best-guess code location to investigate (file path
  or component); refine during fix
- **Symptom:** the user-visible behavior that's wrong
- **Sketch:** what to investigate, then what to do

## Audit / observability

### dogfood-metrics OTEL trace stream not flowing from new sessions

- **Found:** 2026-04-27, first live capture against
  dogfood-metrics with a fresh `claude` session
  (`107ec35f-eb38-4b5d-93b2-7afb8121a735`) launched in another
  terminal after slice 4 landed.
- **Severity:** must-fix (blocks the entire OTEL stream that
  dogfood-metrics was built to capture; without it the report
  has no Subagent Activity / Cost / Performance data — only the
  hook-derived Tool-call Classification + External Calls +
  Source Reads sections).
- **Where:** unknown; pending diagnosis. Possible layers (in
  the order of "least to most upstream," per the project's
  discipline of NOT defaulting to "Claude Code is broken"):
  (a) our reading of the docs, (b) our setup / receiver, (c)
  our testing methodology, (d) Claude Code itself. Each
  diagnostic step below distinguishes one layer.
- **Symptom:** the new session shows hook events landing in
  `dogfood-metrics/raw/hooks-<id>.jsonl` (so
  `.claude/settings.json` IS being read for the hooks block).
  Receiver is reachable — `curl -X POST
  http://localhost:4318/v1/traces` returns 202 from any shell.
  But `dogfood-metrics/raw/traces.jsonl` contains zero spans
  for the new session id. So either traces aren't being sent,
  or they're going somewhere other than localhost:4318.
- **Worked-around for now:** `scripts/dogfood-claude.sh`
  exports the OTEL env vars in the launching shell before
  exec'ing `claude`. This is a defensive belt-and-suspenders —
  it works regardless of whether the underlying issue is in
  settings.json scope, our setup, or upstream. It's NOT a
  diagnosis of the cause.
- **Sketch (diagnostic plan):** work through each step in
  order. Stop at the first definitive answer.

  **Step 1 — Re-verify the docs reading.** Round 6 quoted "env
  block applies to every session" and the verifying agent
  interpreted that as "applies to Claude Code's process AND
  subprocesses." Re-fetch the live settings.md doc and read
  the LITERAL text without that interpretation. If the doc is
  silent on whether "session" includes Claude Code's own
  process or only its subprocesses, the interpretation chain
  is the problem and our blame in earlier rounds is unfounded.

  **Step 2 — Verify env vars actually reach Claude Code's
  process.** Two probes:

  - From a clean shell (no exports), launch `claude` directly
    (NOT via the wrapper). Inside, run a Bash tool call
    `printenv | grep -E '^(CLAUDE_CODE|OTEL)'`. Whatever
    appears is the env of a SUBPROCESS Claude Code spawned —
    confirms the settings.json env block reaches subprocess
    level (the part of the docs that's clear).

  - To check Claude Code's OWN process env, on macOS:
    `ps eww $(pgrep -n claude) | tr ' ' '\n' | grep -E '^(CLAUDE_CODE|OTEL)'`.
    If those vars are absent, the env block does NOT propagate
    to Claude Code's own startup environment — the symptom
    matches and the wrapper script is the right fix. If they
    ARE present, the bug is downstream (exporter init, network
    path, or upstream).

  **Step 3 — Verify the OTEL exporter is initialized inside
  Claude Code.** If env vars are present but no traces flow,
  the exporter might be silently failing to initialize. Probe
  candidates:

  - Run `claude --debug` (or whatever the current debug-mode
    flag is) and grep the output for "telemetry," "otlp,"
    "exporter."

  - Check `~/.claude/` for any startup log file. The MCP
    server logs there; possibly the OTEL exporter does too.

  - From inside Claude Code, do something that should
    DEFINITELY emit a trace (a tool call or LLM turn) and
    immediately check the receiver log (`tail -f
    dogfood-metrics/raw/.receiver.log`). No incoming request
    means the exporter never tried.

  **Step 4 — Verify the receiver isn't silently dropping.**
  Currently every `traces.jsonl` line is a request body
  verbatim. A 202-returning request that doesn't land
  on disk would point at our receiver code:

  - `lsof -i :4318` while Claude Code is active — confirms
    whether anything from claude's PID is connecting.

  - `tcpdump -i lo0 'port 4318'` (macOS) — sees packets
    regardless of receiver behavior.

  **Step 5 — Only after 1–4 are inconclusive: file upstream.**
  At that point we have a real reproducer ("env vars set, no
  exporter activity in claude --debug, no packets on the
  wire") and can write a minimal report. Until then, any
  upstream-blame is speculation.

- **What we know we DON'T know:**
  - Whether `.claude/settings.json`'s env block actually
    reaches Claude Code's own process environment.
  - Whether Claude Code emits any startup signal when
    telemetry initializes (or when it fails to).
  - Where Claude Code logs telemetry-related diagnostics.
  - Whether traces are batched and have a flush interval that
    we'd need to wait through.

### Handoff templates don't surface valid enum values for analysis JSON

- **Found:** 2026-04-27, same /analyze session as above.
  Source-read events flagged 2 reads of
  `internal/exchange/types.go` and `internal/exchange/enums.go`.
- **Severity:** should-fix — every underspecification adds
  tokens (the analyst loads + parses Go source), time (extra
  tool turns), and risk (analyst guesses the wrong enum
  value).
- **Where:** `templates/handoffs/security-review-v1.md`,
  `templates/handoffs/provenance-review-v1.md`, possibly
  `synthesis-v1.md`. Source of truth lives in
  `internal/exchange/{types.go,enums.go}`.
- **Symptom:** analyst agents producing the v1-schema JSON
  output need to know the valid enum values for fields like
  signal_type, posture, severity, etc. The handoff templates
  describe in prose what each field MEANS but don't enumerate
  the valid values. So the analyst (per the dogfood data) goes
  to the source to find them. Per the design/agent-otel.md
  framing, source reads are an underspecification signal: if
  the analyst needed to read internal/, the handoff didn't tell
  it what it needed.
- **Sketch:** for each enum the analysis JSON references,
  embed the valid values directly in the handoff template as a
  reference table or fenced code block. Re-run /analyze on a
  similar target after the fix and confirm the source reads of
  `internal/exchange/` go to zero. This is a clean
  measurable-impact dogfood loop: the metric (source-read
  count) directly corresponds to the fix.
- **Bonus value:** this becomes a regression test for the
  dogfood instrumentation itself. We should be able to
  measure that a v0.1 fix moves a v0.1 metric. If the metric
  doesn't move, either the fix is wrong or our
  instrumentation is wrong — both worth knowing.

### /analyze produces an unexpected number of ingest_analysis calls

- **Found:** 2026-04-27, same /analyze session as above.
  14 distinct `mcp__signatory__signatory_ingest_analysis`
  tool calls (all with unique `tool_use_id`s) spread across
  the 22:56 → 23:15 window — not the "ingest at the end"
  pattern.
- **Severity:** nice-to-have — probably benign behavior we
  just don't yet understand, but worth a closer look in case
  it's a retry loop wasting tool turns.
- **Where:** the /analyze skill's orchestration logic
  (skill prompt content), or the analyst output → ingest
  retry path in `internal/mcp/tools/ingest_analysis.go`.
- **Symptom:** one /analyze run produced 14 ingest calls.
  Typical /analyze should be ~2–3 (security analyst output,
  provenance analyst output, optionally synthesis). Either:
  (a) the orchestrator legitimately produced 14 distinct
  analyst outputs (would be unusual — what would they all
  be?), or (b) the same outputs are being re-ingested after
  schema-validation failures and retries (idempotency catches
  the dup at the store level, but the call still costs a tool
  turn + LLM context).
- **Blocking gap:** our hook classifier doesn't capture the
  `target` or `output_id` arguments passed to
  ingest_analysis. The data lives in the hook payload's
  `tool_input` field; the classifier just doesn't surface it.
  Without that, we can't tell which session was being
  ingested or whether the same output was re-ingested.
- **Sketch:**
  - Extend `cmd/dogfood-metrics/hook.go` `classify()` to
    capture key arguments for high-volume MCP tools — for
    `signatory_ingest_analysis` specifically, surface the
    `target` and `output_id` (or `analysis_session_id`) into
    the hook event's `detail` field.
  - With that data, re-run /analyze and look at the 14-ish
    ingests by target. If they're all the same target →
    retry pattern. If they're different → fan-out worth
    understanding (and maybe filing as a separate
    investigation).
  - This is also a useful general improvement: the report's
    "Tool-call classification" section would be more
    actionable if local_db rows showed argument summaries
    instead of just the bare tool name.

## Pending verifications

Items where a fix has shipped but live end-to-end verification
through the MCP surface is blocked on session lifecycle (the
MCP server is long-lived and spawned with whatever signatory
binary was installed at the start of the Claude Code session;
post-fix binaries don't reach the running MCP process until the
session restarts).

These verifications are worth doing in a fresh session — they
confirm the fix landed not just at the unit-test layer but
through the actual MCP path the original bug surfaced through.
When confirmed, delete the entry; if the live run reveals
something the unit tests missed, file it as a fresh bug entry
above.

### `signatory_analyze` returns OK on entity with Layer 2 only

- **Fix shipped:** `a72cbe0`
- **Unit-test coverage:** `TestAnalyzeTool_EntityWithPostureNoSignals`
  exercises the contract — entity with posture but no signals
  returns OK, not cache-miss.
- **Live verification needed:** in a fresh Claude Code session,
  call `mcp__signatory__signatory_analyze target=burntsushi/toml`
  (the entity that originally surfaced the bug). Expect:
  status="ok" with the trusted-for-now posture surfacing, NOT
  the `cache_miss_requires_refresh` error. Confirm the
  server_version in the response metadata is post-`a72cbe0`.

### Synthesis ingest is rejected without analysis_session_id

- **Fix shipped:** `84111cc`
- **Unit-test coverage:** `TestIngest_SynthesisRequiresAnalysisSession`
  in `internal/store/analyst_output_test.go` (store layer);
  `TestIngestAnalysisTool_SynthesisWithoutSession_SchemaViolation`
  in `internal/mcp/tools/ingest_analysis_test.go` (MCP boundary
  error mapping).
- **Live verification needed:** run the full /analyze skill in a
  fresh Claude Code session against any target. After the run
  completes, inspect the resulting analysis session via
  `signatory analysis show <session-id>`. Expect: the synthesis
  row appears in the linked-outputs body, NOT under the
  "missing" rollup. Direct DB check confirms the new linkage:
  `sqlite3 ~/.signatory/signatory.db "SELECT analyst_id,
  analysis_session_id FROM analyst_outputs WHERE id = '<output>'"`
  should show a non-empty session id for the synthesis row.
  If the synthesist agent still drops the field, the ingest
  fails with CodeSchemaViolation naming `analysis_session_id` —
  the agent retries with the field included and the linkage
  lands. Either way, no orphaned synthesis row should be possible
  post-fix.

## Manual process: worked examples

Sibling to the error catalog above. When a manual workflow runs
correctly end-to-end through signatory and is worth re-running
the same way next time, capture it here as a runbook with a
concrete worked example. These entries are NOT meant to be
deleted on resolution — they're stable reference material.

### Three-way SHA verification before pinning a Go dependency

When adopting a Go module dependency that's been through
signatory's /analyze pipeline (or any time we want defense-in-
depth before recording a pin in `go.sum`), the synthesis-cited
SHA gets cross-checked against three independent live sources
before `go get` runs. If all four sources agree, the pin lands
with the verification chain captured in the commit message. If
any disagree, the install does NOT proceed (see "If sources
diverge" below).

#### When to use

- Adopting a new Go module dependency that has a signatory
  trust verdict
- Bumping an existing dependency to a new version that's been
  re-vetted
- Any other time we want to verify a tagged release matches the
  trust evidence we have for it

#### Inputs

- Target module path (e.g., `github.com/BurntSushi/toml`)
- Target version tag (e.g., `v1.6.0`)
- Synthesis-cited SHA from the trust evidence (typically lives
  in a `prov-NNN-registry-source-match` conclusion; query via
  `signatory_show_conclusions target=<path>` and look for the
  `registry_publish_origin` signal type)

#### Steps

1. **Fetch the Go proxy's `.info` for the version** and extract
   `Origin.Hash`. Note the case-encoding rule: uppercase letters
   in the path get a `!` prefix and are lowercased.

   ```
   curl -sS 'https://proxy.golang.org/<escaped-path>/@v/<version>.info'
   ```

2. **Fetch GitHub's tag ref directly,** bypassing the Go module
   ecosystem entirely. This is the upstream's live answer.

   ```
   git ls-remote https://github.com/<owner>/<repo> refs/tags/<version>
   ```

3. **Compare three SHAs:** synthesis-cited, proxy `Origin.Hash`,
   GitHub tag ref. All three must agree before proceeding.

4. **Run `go get`** to record the pin in `go.mod` / `go.sum`.

   ```
   go get <module-path>@<version>
   ```

5. **Verify the content hash chain:** `go.sum` records two
   `h1:` content hashes for the new dep. Fetch the same hashes
   independently from `sum.golang.org` and confirm they match.
   This catches the case where our local Go install fetched
   from a tampered proxy.

   ```
   curl -sS 'https://sum.golang.org/lookup/<escaped-path>@<version>'
   ```

6. **Capture the verification record in the commit message.**
   Include all four attestation values (synthesis-cited, proxy
   `Origin.Hash`, GitHub tag ref, sum.golang.org content hashes)
   so anyone replaying the trust chain later has a starting point.

#### Worked example: github.com/BurntSushi/toml v1.6.0

Performed 2026-04-26, recorded in commit `194d007`. The escaped
proxy path for `BurntSushi` is `!burnt!sushi` (each capital
letter prefixed with `!`).

```
$ curl -sS 'https://proxy.golang.org/github.com/!burnt!sushi/toml/@v/v1.6.0.info'
{"Version":"v1.6.0","Time":"2025-12-18T12:15:22Z","Origin":{"VCS":"git","URL":"https://github.com/BurntSushi/toml","Hash":"52534926c55b4cd85b05aee90569dd0668b8cf30"}}

$ git ls-remote https://github.com/BurntSushi/toml refs/tags/v1.6.0
52534926c55b4cd85b05aee90569dd0668b8cf30	refs/tags/v1.6.0

$ go get github.com/BurntSushi/toml@v1.6.0
go: downloading github.com/BurntSushi/toml v1.6.0
go: added github.com/BurntSushi/toml v1.6.0

$ grep BurntSushi/toml go.sum
github.com/BurntSushi/toml v1.6.0 h1:dRaEfpa2VI55EwlIW72hMRHdWouJeRF7TPYhI+AUQjk=
github.com/BurntSushi/toml v1.6.0/go.mod h1:ukJfTF/6rtPPRCnwkur4qwRxa8vTRFBF0uk2lLoLwho=

$ curl -sS 'https://sum.golang.org/lookup/github.com/!burnt!sushi/toml@v1.6.0'
48288060
github.com/BurntSushi/toml v1.6.0 h1:dRaEfpa2VI55EwlIW72hMRHdWouJeRF7TPYhI+AUQjk=
github.com/BurntSushi/toml v1.6.0/go.mod h1:ukJfTF/6rtPPRCnwkur4qwRxa8vTRFBF0uk2lLoLwho=
... (Merkle tree proof omitted)
```

All four attestations agreed:

- Synthesis (prov-005-registry-source-match): `52534926c55b4cd85b05aee90569dd0668b8cf30`
- proxy.golang.org Origin.Hash: `52534926c55b4cd85b05aee90569dd0668b8cf30`
- GitHub refs/tags/v1.6.0: `52534926c55b4cd85b05aee90569dd0668b8cf30`
- go.sum content hashes match sum.golang.org's independent attestation

Proceeded to commit. The commit message preserves the four-way
record so the trust chain is replayable from `git log`.

#### If sources diverge

The verification's job is to catch this BEFORE `go.sum` is
recorded. If any source disagrees with the others, do NOT run
`go get`. Triage by disagreement pattern:

| Disagreement | What it suggests | Action |
|---|---|---|
| Synthesis disagrees, proxy/sum/GitHub all agree | Synthesis was wrong (analyst transcription error or stale snapshot) | Re-run the provenance analysis to refresh; accept the live three-way-attested SHA if confirmed; update records |
| Proxy + sum.golang.org agree, GitHub differs | Upstream tag was force-pushed AFTER the proxy locked the original — Go's "first observation wins" model. Rare-but-real upstream divergence event. | Hold. Investigate WHY (maintainer-corrected typo is benign; compromise is not). Pull the GitHub commit log around the divergence time. |
| Proxy disagrees with sum.golang.org | Proxy is compromised or seriously out of sync with the checksum DB | Stop. Report to `security@golang.org`. Do not pull from any source until resolved. |
| All three sources agree among themselves but differ from each other on which SHA they cite | Genuinely impossible if our requests reach the real services — means MITM | Check DNS, TLS chain, network path. |
| Everything matches on retry | Transient flake, proceed | — |

Capture the divergence event in this file (or a sibling
incidents log) regardless of resolution. Three reasons:

1. Even-benign divergence is calibration data for the trust model
2. If it turns out to be malicious, the timeline is already
   preserved before memories blur
3. The diagnostic walk becomes the runbook for the next person

#### Known gap (file as a separate dogfood-errors entry if not already)

The synthesis we trusted cited what proxy.golang.org told the
analyst, but signatory's store captures the analyst's prose
summary, not the raw signed proxy response. If we wanted a
bulletproof audit chain, the analyst would store the actual
JSON response (or its hash) as a citation artifact, so we could
verify the analyst really saw what they claimed. As-is, we're
trusting the analyst's transcription, with no way to
independently verify their fetch wasn't tampered with at
analysis time. Worth filing as a longer-term audit-trail
enhancement to signatory itself.
