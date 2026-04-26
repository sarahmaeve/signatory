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

### `signatory analysis show` rollup misses synthesis row

- **Found:** 2026-04-26, dogfood run on
  `repo:github/BurntSushi/toml`, analysis session
  `a186fb43-5e3e-4f6b-9300-9ce603a98e5e`
- **Severity:** should-fix (audit-trail integrity — the rollup
  is the canonical "what happened in this run" view, and missing
  the synthesis makes it actively misleading rather than just
  incomplete)
- **Where:** either `internal/mcp/tools/ingest.go`
  (signatory_ingest_analysis — does the synthesist's call drop
  `analysis_session_id`?), or wherever `signatory analysis show`
  builds its linked-outputs query (likely `cmd/signatory/` plus
  a store query in `internal/store/`)
- **Symptom:** for the run above, synthesis output `c51e76fa`
  is correctly visible in `signatory show-analyses
  repo:github/BurntSushi/toml` with the expected synthesist
  analyst_id, but `signatory analysis show a186fb43-…` lists
  it as "missing" from the session and excludes it from the
  linked-outputs body. The two views disagree.
- **Sketch:** two diagnostic forks. (A) The synthesist agent's
  `signatory_ingest_analysis` MCP call dropped
  `analysis_session_id` — verifiable by inspecting the stored
  synthesis row's session FK. (B) The rollup join in
  `signatory analysis show` excludes round=0 (synthesis) rows by
  schema mistake — verifiable by reading the query and checking
  the WHERE clause. Fix whichever is at fault. Add a regression
  test against the dogfood session id (or an in-memory
  reproduction with the same shape) so the rollup→synthesis
  linkage stays asserted.

### Synthesis output absent from `signatory_show_conclusions`

- **Found:** 2026-04-26, same BurntSushi/toml dogfood session.
  Queried `signatory_show_conclusions target=burntsushi/toml`;
  got 13 conclusions back, all from `signatory-security-v1` and
  `signatory-provenance-v1`. The synthesis output `c51e76fa`
  was not represented.
- **Severity:** nice-to-have if working-as-designed; should-fix
  if linked to the rollup bug above (likely same root cause).
  Sub-finding: a user wanting to see "what did the synthesist
  recommend?" has no MCP read path that surfaces the structured
  synthesis content — only the raw `show_analyses` listing and
  the per-conclusion search, neither of which carries the
  synthesist's narrative or action items.
- **Where:** depends on diagnosis. Possible loci:
  - The synthesist's emitted output shape (does it produce
    Conclusion records, or only `synthesis_supplement`? See
    `templates/handoffs/synthesis-v1.md` and
    `internal/exchange/` for the v1 schema)
  - The store-query layer that backs `signatory_show_conclusions`
    (does it filter by output type and exclude synthesis rows by
    design?)
  - Or a missing read surface entirely (no `show_synthesis`
    tool exists)
- **Symptom:** the synthesis is the highest-value output of a
  run — it's where the proposed posture and action items live —
  but `show_conclusions` doesn't expose any of that text.
  Combined with the rollup bug above, the synthesis is currently
  reachable only via `show_analyses` (a metadata listing, not
  the content) and not via any conclusion-style search.
- **Sketch:** investigate first. Two forks:
  - (A) The synthesist emits only `synthesis_supplement`, not
    Conclusion records — `show_conclusions` is correctly
    Conclusion-only. Then the gap is a missing read tool: add
    `signatory_show_synthesis target=X` (or similar) that
    returns the latest synthesis output's structured content
    for the entity. This is also what the rollup bug needs to
    surface synthesis content.
  - (B) The synthesist does emit Conclusions but they're not
    landing in the conclusion table. Then this is the same
    root cause as the rollup-misses-synthesis bug above; fix
    once, both surfaces light up.

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
