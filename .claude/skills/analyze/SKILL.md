---
name: analyze
description: >
  Run signatory's trust analysis pipeline for a target. Checks the
  signatory store first; if empty, dispatches specialist analyst agents
  using signatory handoff prompts (security + provenance), ingests their
  structured v1-schema JSON output, and synthesizes a combined assessment.
  This is the v0.1 automated pipeline that populates the signatory store
  and produces LLM-synthesized trust assessments. For manual human-readable
  analysis, use /vet-dependency instead.
allowed-tools: Bash Read Write Glob Grep Agent WebFetch
---

# Analyze — signatory trust analysis pipeline

This skill orchestrates signatory's full analysis pipeline:

```
check store → generate handoff prompts → dispatch analyst agents →
validate + ingest structured output → synthesize combined assessment
```

The target is specified as $ARGUMENTS — a GitHub/GitLab URL, package
coordinate (Go module, npm, PyPI, crates.io), or owner/repo shorthand.

This skill is NOT a monolithic analyst. It is an **orchestrator** that
dispatches specialist agents and synthesizes their output. The
specialists do the collection work; this skill manages the pipeline.

## Step 0 — Check the signatory store

Before doing anything expensive, check whether signatory already has
data for this target.

```bash
signatory show-analyses "$TARGET" 2>&1
echo "exit: $?"
```

**Check the exit code before interpreting the output:**

- **Exit 0 + output shows analyses**: data exists. Query conclusions:
  ```bash
  signatory show-conclusions --target "$TARGET" 2>&1
  ```
  Present the existing analysis to the user. Ask whether they want a
  fresh run (re-collect) or are satisfied. If satisfied, skip to
  Step 4. If re-collecting, proceed to Step 1.

- **Exit 0 + "No entity matches"**: no data in the store. Tell the
  user "no existing analysis" and proceed to Step 1.

- **Any non-zero exit code**: something is broken — a CLI syntax
  error, missing database, permissions problem, etc. **STOP.** Report
  the exact error output to the user. Do NOT proceed to Step 1.
  Common causes: wrong flag syntax (show-analyses uses a positional
  argument, not `--target`), missing `~/.signatory/signatory.db`
  (run `signatory init` first), or a corrupted database file.

## Step 0a — Preflight the TLS trust env

The pipeline service uses HTTPS because Claude Code's WebFetch tool
forces HTTPS on all URLs. Claude Code's Node TLS stack consults the
`NODE_EXTRA_CA_CERTS` env var at every handshake to load the local
(mkcert-issued) CA. When that env var is missing or stale — which
happens silently after terminal restarts, GUI launches, or fresh
shells — synthesist WebFetches fail with "unable to verify the
first certificate" and the run halts mid-pipeline.

**Always preflight before dispatching agents:**

```bash
signatory certs check
```

- **Exit 0**: env is set and points to a valid CA. Proceed to Step 1.
- **Non-zero**: the command's stderr carries the specific failure
  and a remediation hint. The most common fix is:
  ```bash
  signatory certs init --write-profile
  ```
  Then restart your terminal (or `source ~/.zshrc`) and retry.

`signatory certs init` copies the mkcert root CA into a stable,
signatory-owned path (`~/.signatory/certs/rootCA.pem`) and with
`--write-profile` appends a managed `export NODE_EXTRA_CA_CERTS=...`
block to `~/.zshrc`. Re-running is idempotent; the block is replaced
in place rather than duplicated.

Override the default cert dir with `--cert-dir PATH` and the default
shell profile with `--shell-profile-path PATH`.

First-time machine setup (only needed once, not per-run):

```bash
brew install mkcert
mkcert -install                 # installs local CA in system trust
signatory certs init --write-profile
# then: restart your terminal so NODE_EXTRA_CA_CERTS is exported
```

## Step 1 — Start pipeline service + create sessions + generate handoffs

Two session concepts, both needed:

1. **Pipeline session** (`$SESSION_ID`): transport-layer. Holds
   handoff messages the agents retrieve via WebFetch.
2. **Analysis session** (`$ANALYSIS_SID`): audit identity for this
   /analyze run. Lives in the main signatory store. Every analyst
   output the dispatched agents land will carry this id via the
   `analysis_session_id` FK, so `signatory analysis show|timing
   <id>` afterwards surfaces the linked outputs with per-analyst
   wall-clock decomposition.

```bash
# Ensure the pipeline service is running. `signatory serve status`
# exits 0 if a managed instance is up (pidfile + live PID + port
# listening), 1 otherwise. `signatory serve start` is idempotent-
# friendly: it refuses to clobber an already-running instance and
# writes a pidfile + log file so subsequent runs can Stop / Restart
# cleanly. Prefer these over `pgrep` / `lsof` / `nohup & disown`.
signatory serve status >/dev/null 2>&1 || signatory serve start

# Create the pipeline (transport) session.
SESSION_ID=$(signatory pipeline session create "$TARGET")
echo "Pipeline session: $SESSION_ID"

# Create the analysis (audit) session. --expected-analyst lets
# `signatory analysis show <id>` surface expected-vs-landed
# rollups afterwards. --pipeline-session-id threads the transport
# session id onto the audit row for cross-correlation if needed.
ANALYSIS_SID=$(signatory analysis begin "$TARGET" \
  --expected-analyst signatory-security-v1 \
  --expected-analyst signatory-provenance-v1 \
  --expected-analyst signatory-synthesis-v1 \
  --pipeline-session-id "$SESSION_ID")
echo "Analysis session: $ANALYSIS_SID"
```

Generate both handoff prompts and deposit them in the pipeline
session. `signatory handoff` handles target resolution (accepts
every form `signatory analyze` accepts: owner/repo shorthand,
github.com URL, https:// URL, `repo:` canonical URI),
language/ecosystem detection, and full-history cloning — you do
NOT need to call `gh api` or `git clone` yourself. The clone is
full (no `--depth=1`) so Step 1b's `analyze --refresh --path` can
read history-dependent signals (first_commit_date, authorship
windows, signing ratios) without rejecting it as shallow.

`--analysis-session-id "$ANALYSIS_SID"` tells the handoff renderer
to embed the session linkage instruction in the rendered prompt.
The dispatched analyst reads the handoff, sees the block, and
includes `analysis_session_id: "$ANALYSIS_SID"` in its
`signatory_ingest_analysis` call. Handoff validates the session
exists and is still in_progress before rendering — typos fail
here rather than inside a dispatched subagent.

`--deposit-to "$SESSION_ID"` posts the rendered handoff into the
pipeline session in one Go process. The rendered bytes never cross
a shell boundary, so control characters in stored analysis text
(literal newlines, quotes) reach the pipeline unchanged.

```bash
# Security handoff — clones the repo into filestore/clones/ and
# deposits the rendered prompt into the pipeline session.
signatory handoff security "$TARGET" \
  --analysis-session-id "$ANALYSIS_SID" \
  --network-precheck --clone-dir filestore/clones/ \
  --deposit-to "$SESSION_ID"

# Provenance handoff — reuses the same clone. basename on the
# target gives the short name the clone-dir step wrote to, for
# every accepted target form (owner/repo, URL, canonical URI).
TARGET_NAME=$(basename "$TARGET" .git)
signatory handoff provenance "$TARGET" \
  --analysis-session-id "$ANALYSIS_SID" \
  --network-precheck --path "filestore/clones/$TARGET_NAME" \
  --deposit-to "$SESSION_ID"
```

If a handoff command fails, its stderr carries the diagnostic
(template source, unfilled placeholders, any deposit error naming
the session id) — no `2>/dev/null` redirect to peel off.

**Unfilled placeholders** (`INTAKE_QUESTION`, `TARGET_ROLE`) are
expected and acceptable. Do NOT stop to fill them. Proceed.

## Step 1b — Refresh Layer-1 signals against the cloned target

Layer 1 (mechanistic collectors) populates the local store with
the registry / GitHub-API / git data the analysts need. Without
this step, dispatched analysts see an empty `signatory_signals`
result and reach for `gh api` / `curl` / `WebFetch` to do the
collection themselves — duplicating cached data and inflating
the run's external-call count. Pre-Step-1b dogfood runs averaged
~10 external API calls per session; this step closes most of
that gap.

Run it AFTER Step 1's handoff (the clone Step 1 produces is
required by the github + git collectors) and BEFORE Step 2's
analyst dispatch (so the store is populated when the analysts
read it).

```bash
# TARGET_NAME is already set by Step 1 (basename derivation
# matches what handoff --clone-dir wrote).
signatory analyze --refresh \
  --path "filestore/clones/$TARGET_NAME" \
  "$TARGET"
```

**What runs per ecosystem** (verified per-target):

- `pkg:golang/...` (vanity Go path or canonical purl): gopublish +
  github + git + repofiles. Full coverage.
- `pkg:npm/...` (bare or scoped): npm + github + git + repofiles
  after npm-registry repo resolution stamps `entity.URL`.
- `pkg:pypi/...`: pypi + github + git + repofiles after PyPI
  project-URL resolution stamps `entity.URL`.
- `repo:github/...`: github + git + repofiles. NOTE: when the
  cloned repo is a Go module (`go.mod` present), `signatory
  analyze` emits a stderr hint recommending the
  `pkg:golang/<modpath>` form for full Go-publish provenance
  signals. The hint is informational; don't stop the pipeline.

**Failure handling.** If `--refresh` errors (network, 404 on the
package registry, clone-origin mismatch), surface the error and
STOP — do not proceed to Step 2. Dispatching analysts against a
half-populated signal store reproduces exactly the failure mode
this step is meant to prevent. The `--refresh` path is loud-fail
by design; trust its error rather than retrying.

## Step 2 — Dispatch analyst agents IN PARALLEL

Spawn two agents in a single message. Each agent:

1. Retrieves its handoff via WebFetch from the pipeline service.
2. Performs its analysis using Read / Glob / Grep on the local
   clone, and WebFetch for registry / API lookups.
3. Serializes its final output as v1-schema JSON and calls the
   **signatory_ingest_analysis** MCP tool with it. The MCP tool
   lands the analysis directly in the store — no markdown
   intermediate, no `filestore/analysis/` scratch files, no
   post-hoc conversion by the orchestrator.

**IMPORTANT**: send BOTH Agent calls in ONE message so they run
concurrently.

**IMPORTANT**: analyst agents do NOT have Bash. They use Read,
Glob, Grep on the local clone, WebFetch for the handoff and
external lookups, and the `mcp__signatory__signatory_ingest_analysis`
tool to land their output. They do NOT need Write (nothing to
write to disk).

The URL pattern for retrieving handoffs is:
```
https://127.0.0.1:21517/api/sessions/{SESSION_ID}/messages?role={ROLE}&type=handoff&format=raw
```

The `format=raw` parameter returns plain text (not JSON), which is
what the agent needs.

```
Agent(signatory-security):
  prompt: |
    You are a security analyst for signatory's trust analysis pipeline.

    Retrieve your handoff via WebFetch:
      https://127.0.0.1:21517/api/sessions/{SESSION_ID}/messages?role=security&type=handoff&format=raw

    Follow the handoff exactly. It specifies your analyst_id, the v1
    JSON envelope, the signatory_ingest_analysis call shape, and the
    analysis_session_id you must include in your ingest call.

    One orchestrator-supplied field the handoff does not set:
        collected_from: "{TARGET}"
    Pass it verbatim. If the caller-asked URI resolves to the same
    canonical URI as analyst_output.target the MCP tool treats this
    as a no-op; otherwise it indexes the analysis under the caller's
    identity (agent-facing-contract §3.2).
  allowed-tools: Read Glob Grep WebFetch mcp__signatory__signatory_ingest_analysis

Agent(signatory-provenance):
  prompt: |
    You are a provenance analyst for signatory's trust analysis pipeline.

    Retrieve your handoff via WebFetch:
      https://127.0.0.1:21517/api/sessions/{SESSION_ID}/messages?role=provenance&type=handoff&format=raw

    Follow the handoff exactly. It specifies your analyst_id, the v1
    JSON envelope, the signatory_ingest_analysis call shape, and the
    analysis_session_id you must include in your ingest call.

    One orchestrator-supplied field the handoff does not set:
        collected_from: "{TARGET}"
    Pass it verbatim. If the caller-asked URI resolves to the same
    canonical URI as analyst_output.target the MCP tool treats this
    as a no-op; otherwise it indexes the analysis under the caller's
    identity (agent-facing-contract §3.2).
  allowed-tools: Read Glob Grep WebFetch mcp__signatory__signatory_ingest_analysis
```

Substitute `{SESSION_ID}` and `{TARGET}` into each prompt before
dispatching. `{TARGET}` is the original `$TARGET` value the user
supplied to the skill (unresolved; the MCP tool canonicalizes).
The audit session id is embedded in the handoff body (via the
handoff's SESSION_INSTRUCTION block) — the agent reads it from
WebFetch and forwards it unchanged, so the dispatch prompt doesn't
need to carry it.

Wait for BOTH agents to complete before proceeding.

## Step 3 — Verify both analysts landed their output

The agents ingested directly via the MCP tool, so the orchestrator
has nothing to convert or ingest — just confirm that both analyses
are present in the store before moving to synthesis.

```bash
# Primary view — every output ever landed for this target.
signatory show-analyses "$TARGET"

# Session-scoped view — only outputs linked to this run via the
# analysis_session_id FK. Faster to read than show-analyses when
# the target has prior rounds; also surfaces the "expected-vs-
# landed" rollup so missing analysts are obvious.
signatory analysis show "$ANALYSIS_SID"
```

Query `show-analyses` with `$TARGET` (the original caller URI),
not the resolved repo URI. Because each analyst passed
`collected_from: $TARGET`, M2 indexes the analysis under the
caller's identity; the analyst-stated target is captured as the
resolved source and the cross-URI walk surfaces it from either
direction.

Expected: two rows, one per analyst role. If only one row shows
up, the missing analyst either failed silently, skipped the
ingest call, or omitted `collected_from` / `analysis_session_id`.
Re-dispatch the missing role with explicit guidance to call
signatory_ingest_analysis with `source`, `collected_from`, and
`analysis_session_id` all set, at the end of its turn.

`signatory analysis show` names the missing analyst directly in
its "missing:" line — the most direct signal of who didn't land.

If an agent's own report indicates a v1 schema-validation error
from signatory_ingest_analysis and the agent couldn't fix it in
its turn, the agent's transcript contains the error message —
surface it to the user so they can decide whether to re-dispatch
(with extra schema guidance) or accept a gap.

## Step 4 — Dispatch synthesist agent

Generate the synthesis handoff and deposit it in the session. Since
M6c, the handoff body carries the full structured evidence — every
analyst conclusion, positive absence, and observation for this
target, ready to reason over. The synthesist does NOT query the
store, read filestore, or run CLI commands; its entire input
arrives via WebFetch.

```bash
# The synthesis handoff embeds stored analysis text (verdicts,
# rationales) with literal newlines and quotes. --deposit-to routes
# the rendered bytes Go → HTTP → Go so that content reaches the
# session verbatim.
signatory handoff synthesist "$TARGET" \
  --analysis-session-id "$ANALYSIS_SID" \
  --deposit-to "$SESSION_ID"
```

If the handoff command fails with "no entity matches" or "no
non-synthesis analyses to synthesize," Step 3 did not land any
analyst output. Re-dispatch the missing analyst role(s) before
continuing.

```
Agent(signatory-synthesis):
  prompt: |
    You are the synthesist for signatory's trust analysis pipeline.

    Retrieve your handoff via WebFetch:
      https://127.0.0.1:21517/api/sessions/{SESSION_ID}/messages?role=synthesist&type=handoff&format=raw

    Follow the handoff exactly. It inlines every analyst conclusion
    you need, specifies your analyst_id, the v1 synthesis_supplement
    shape, the signatory_ingest_analysis call, and the
    analysis_session_id you must include.

    Two orchestrator-level rules not in the handoff:

    1. Do NOT pass `collected_from` to signatory_ingest_analysis —
       the synthesist inherits caller-identity indexing from the
       analyses it synthesizes. (Only the analyst roles supply it.)

    2. Report the `output_id` that signatory_ingest_analysis returns
       in your final message. The orchestrator reads it to offer
       `signatory posture accept <output-id>` in Step 5; without it
       the pipeline stalls.

    D9 tool-capability note: Read/Glob/Grep are in your toolset
    ONLY so Claude Code's HTTPS client can load the mkcert CA file
    referenced by NODE_EXTRA_CA_CERTS at TLS handshake time —
    without file-read capability the WebFetch above fails with
    "unable to verify the first certificate"
    (see design/open-architecture-question.md). You MUST NOT use
    them to browse filestore, prior analyses, source code, or any
    other evidence beyond what the handoff body carries.
  allowed-tools: Read Glob Grep WebFetch mcp__signatory__signatory_ingest_analysis
```

The synthesist's output is a v1-schema synthesis record; the
human-readable markdown rendering is produced on demand by
`signatory show-synthesis <output-id>` against the stored record.
The store, not the filestore, is the canonical copy.

## Step 5 — Record posture (with user confirmation)

**The decision is the user's.** Present the synthesist's
`proposed_posture` (tier + rationale_summary) and wait for explicit
approval before recording anything.

Primary path — accept the synthesist's proposal verbatim:

```bash
# Capture the synthesis output_id from Step 4's ingest response
# (the agent's transcript reports it).
signatory posture accept "$SYNTHESIS_OUTPUT_ID" --yes
```

With overrides, when the user disagrees with a specific field but
wants to keep the rest:

```bash
# Example: user disagrees with the synthesist's tier, keeping
# everything else from the proposal. The override is recorded in
# the audit detail under proposed_tier so the deviation is
# auditable.
signatory posture accept "$SYNTHESIS_OUTPUT_ID" \
  --tier rejected --yes

# Or override the rationale via a file.
signatory posture accept "$SYNTHESIS_OUTPUT_ID" \
  --rationale-file /tmp/user-rationale.md --yes
```

Fallback — if the user wants to record a posture that diverges
from the proposal in multiple fields at once, or wants a fresh
rationale unrelated to the synthesist's framing:

```bash
signatory posture set --tier "$TIER" \
  --rationale-file /tmp/user-rationale.md "$CANONICAL_URI"
```

"Analysis only — no posture recorded" is a valid terminal state for
non-dependency targets. Preview with `--dry-run` on either verb
before the real write.

## Step 6 — Close the analysis session

After the posture decision is recorded (or explicitly declined), mark
the analysis session terminal so `signatory analysis list` no longer
shows it as in_progress and `signatory analysis timing` surfaces the
wall-clock total with a proper `ended_at` stamp.

```bash
# Typical close: the run reached synthesis successfully and posture
# was recorded. --synthesis-output-id threads the synthesis row's
# id into the session for one-query audit-trail lookups.
signatory analysis end "$ANALYSIS_SID" \
  --status completed \
  --synthesis-output-id "$SYNTHESIS_OUTPUT_ID"
```

Alternative terminal statuses when things went sideways:

- `--status failed`: a required step broke (e.g. an analyst never
  ingested and couldn't be re-dispatched, synthesis refused the
  evidence, handoff validation kept failing). Records the fact of
  the run without claiming its conclusions.
- `--status partial`: some analysts landed, others didn't, and the
  user accepted the partial result rather than re-running. Use
  when the synthesis output exists but skipped a dimension.

The close is one-way — once terminal, the session stays terminal.
A re-run of /analyze on the same target begins a fresh session.

## Important constraints

- **Do not skip Step 0.** The store may already have the answer.
  Re-collecting signals from GitHub's API is slow, rate-limited, and
  redundant if the data exists.

- **Do not merge the two analysts into one agent.** The dual-analyst
  pattern exists because security and provenance require different
  focus areas, different methodologies, and different blind spots.
  Merging them produces a generalist that's weaker at both.

- **Trust the ingest-tool validator.** signatory_ingest_analysis
  runs the v1 schema validator before writing; an invalid payload
  returns CodeSchemaViolation naming the offending field. Agents
  should fix the JSON and retry in the same turn rather than
  dropping fields or writing markdown to a file.

- **Do not skip synthesis.** Raw conclusions without synthesis are
  data without interpretation. The synthesis is what makes the
  pipeline useful — it's the step where an LLM adds the value that
  a database query cannot.

- **The handoff IS the instructions.** Do not invent your own
  analyst instructions. `signatory handoff` generates role-specific
  prompts from templates that encode signatory's trust model. Those
  templates are the single source of truth for what each analyst
  should do.
