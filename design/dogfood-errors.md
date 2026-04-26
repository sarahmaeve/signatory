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

### Missing MCP read surface for synthesis content

- **Found:** 2026-04-26, BurntSushi/toml dogfood session
  `a186fb43-…`, surfaced while looking up the synthesist's
  proposed posture (synthesis output `c51e76fa`).
- **Severity:** nice-to-have (working-as-designed gap, not a
  bug — but a real usability hole)
- **Where:** new MCP tool needed in `internal/mcp/tools/`;
  closest existing analog is the CLI `signatory show-synthesis
  <output-id>` verb (in `cmd/signatory/`), which renders the
  stored synthesis as markdown.
- **Symptom:** users wanting to read "what did the synthesist
  recommend?" through the MCP surface have no clean path. The
  closest tool, `signatory_show_conclusions`, never returns
  synthesis rows because the synthesist explicitly does not emit
  Conclusion records — synthesis output content lives in the
  `synthesis_supplement` field, structurally distinct from the
  Conclusion model. `signatory_show_analyses` returns metadata
  about the synthesis row but not its body. The CLI has
  `show-synthesis` for this purpose; the MCP surface doesn't.
- **Investigation result:** confirmed working-as-designed at the
  query layer. The synthesis-v1.md handoff template explicitly
  instructs synthesists to populate `synthesis_supplement` and
  NOT to produce Conclusions ("those are Layer-2 artifacts; you
  are Layer-3"). So `show_conclusions` correctly filters them
  out — the gap is a missing read surface, not a broken query.
- **Sketch:** add a new MCP tool `signatory_show_synthesis`
  taking either a `target` (return the latest synthesis output
  for that entity) or an `output_id` (return that specific
  synthesis row). Returns the structured `synthesis_supplement`
  body — proposed_posture, reasoning, summary, action_items,
  concordance_strengths, contradictions_detected, gaps. Mirrors
  the existing CLI verb but on the MCP side. Independent of any
  bug fix; it's a feature add that closes a real gap.

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
