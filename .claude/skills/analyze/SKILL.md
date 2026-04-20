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

## Prerequisite — one-time TLS setup

The pipeline service uses HTTPS because Claude Code's WebFetch
tool forces HTTPS on all URLs. One-time setup with mkcert:

```bash
brew install mkcert
mkcert -install  # installs local CA; needs sudo password
mkdir -p ~/.signatory/certs
cd ~/.signatory/certs && mkcert 127.0.0.1 localhost
```

Then add to your shell profile so Claude Code's HTTP client
trusts the mkcert CA:

```bash
export NODE_EXTRA_CA_CERTS="$(mkcert -CAROOT)/rootCA.pem"
```

Restart Claude Code for the env var to take effect. This setup
is one-time — subsequent pipeline runs just use the service.

## Step 1 — Start pipeline service + create session + generate handoffs

The pipeline message service eliminates /tmp files and context-window
pressure. Agents retrieve their instructions via WebFetch from a
localhost URL instead of having 500 lines inlined in their prompt.

```bash
# Ensure the pipeline service is running. `signatory serve status`
# exits 0 if a managed instance is up (pidfile + live PID + port
# listening), 1 otherwise. `signatory serve start` is idempotent-
# friendly: it refuses to clobber an already-running instance and
# writes a pidfile + log file so subsequent runs can Stop / Restart
# cleanly. Prefer these over `pgrep` / `lsof` / `nohup & disown`.
signatory serve status >/dev/null 2>&1 || signatory serve start

# Create a session for this pipeline run.
SESSION_ID=$(curl -sk -X POST https://127.0.0.1:21517/api/sessions \
  -H "Content-Type: application/json" \
  -d "{\"target\":\"$TARGET\"}" | jq -r .id)
echo "Session: $SESSION_ID"
```

Generate both handoff prompts and deposit them in the session.
`signatory handoff` handles target resolution (accepts every form
`signatory analyze` accepts: owner/repo shorthand, github.com URL,
https:// URL, `repo:` canonical URI), language/ecosystem detection,
and shallow-cloning — you do NOT need to call `gh api` or `git clone`
yourself.

Use `--json` on the handoff emission so its output is already a
JSON-escaped string; the outer POST body can interpolate it
directly without piping through `jq -Rs`. This avoids the
control-character pitfalls when the handoff body includes stored
analysis text with literal newlines (synthesis handoffs especially).

```bash
# Security handoff — clones the repo into filestore/clones/.
SECURITY_HANDOFF_JSON=$(signatory handoff security "$TARGET" \
  --network-precheck --clone-dir filestore/clones/ --json 2>/dev/null)

# Deposit in the pipeline service. $SECURITY_HANDOFF_JSON is
# already a JSON string (including surrounding quotes), so
# interpolating directly into the outer object is safe.
curl -s -X POST "https://127.0.0.1:21517/api/sessions/$SESSION_ID/messages" \
  -H "Content-Type: application/json" \
  --data-binary "{\"role\":\"security\",\"msg_type\":\"handoff\",\"content\":$SECURITY_HANDOFF_JSON}"

# Provenance handoff — reuses the same clone. basename on the
# target gives the short name the clone-dir step wrote to, for
# every accepted target form (owner/repo, URL, canonical URI).
TARGET_NAME=$(basename "$TARGET" .git)
PROVENANCE_HANDOFF_JSON=$(signatory handoff provenance "$TARGET" \
  --network-precheck --path "filestore/clones/$TARGET_NAME" --json 2>/dev/null)

curl -s -X POST "https://127.0.0.1:21517/api/sessions/$SESSION_ID/messages" \
  -H "Content-Type: application/json" \
  --data-binary "{\"role\":\"provenance\",\"msg_type\":\"handoff\",\"content\":$PROVENANCE_HANDOFF_JSON}"
```

If a handoff command fails, remove `2>/dev/null` and check stderr.

**Unfilled placeholders** (`INTAKE_QUESTION`, `TARGET_ROLE`) are
expected and acceptable. Do NOT stop to fill them. Proceed.

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
Agent(security-analyst):
  prompt: |
    You are a security analyst for signatory's trust analysis pipeline.

    FIRST: Retrieve your full handoff instructions using WebFetch:
      https://127.0.0.1:21517/api/sessions/{SESSION_ID}/messages?role=security&type=handoff&format=raw

    Follow those instructions exactly.

    OUTPUT INSTRUCTIONS:
    - Produce your output as v1-schema JSON (see internal/exchange/types.go
      for the shape; the handoff's "Output format" section carries an
      example envelope).
    - Land your output by calling the signatory_ingest_analysis MCP tool:
        analyst_output: <your v1 JSON object>
        source:         "mcp:security-analyst"
    - The tool validates your payload. If validation fails, the error
      names the first offending field — fix the JSON and retry in the
      same turn. Do NOT drop fields to get past validation.
    - Do NOT write files. Do NOT produce markdown as output. Do NOT run
      signatory commands (you have no Bash). The MCP tool is the sole
      transport for your output.
  allowed-tools: Read Glob Grep WebFetch mcp__signatory__signatory_ingest_analysis

Agent(provenance-analyst):
  prompt: |
    You are a provenance analyst for signatory's trust analysis pipeline.

    FIRST: Retrieve your full handoff instructions using WebFetch:
      https://127.0.0.1:21517/api/sessions/{SESSION_ID}/messages?role=provenance&type=handoff&format=raw

    Follow those instructions exactly.

    OUTPUT INSTRUCTIONS:
    - Produce your output as v1-schema JSON (see internal/exchange/types.go
      for the shape; the handoff's "Output format" section carries an
      example envelope).
    - Land your output by calling the signatory_ingest_analysis MCP tool:
        analyst_output: <your v1 JSON object>
        source:         "mcp:provenance-analyst"
    - The tool validates your payload. If validation fails, the error
      names the first offending field — fix the JSON and retry in the
      same turn. Do NOT drop fields to get past validation.
    - Do NOT write files. Do NOT produce markdown as output. Do NOT run
      signatory commands (you have no Bash). The MCP tool is the sole
      transport for your output.
  allowed-tools: Read Glob Grep WebFetch mcp__signatory__signatory_ingest_analysis
```

Wait for BOTH agents to complete before proceeding.

## Step 3 — Verify both analysts landed their output

The agents ingested directly via the MCP tool, so the orchestrator
has nothing to convert or ingest — just confirm that both analyses
are present in the store before moving to synthesis.

```bash
signatory show-analyses "$CANONICAL_URI"
```

Expected: two rows, one per analyst role. If only one row shows
up, the missing analyst either failed silently or skipped the
ingest call. Re-dispatch the missing role with explicit guidance
to call signatory_ingest_analysis at the end of its turn.

If an agent's own report indicates a v1 schema-validation error
from signatory_ingest_analysis and the agent couldn't fix it in
its turn, the agent's transcript contains the error message —
surface it to the user so they can decide whether to re-dispatch
(with extra schema guidance) or accept a gap.

## Step 4 — Dispatch synthesist agent

Generate the synthesis handoff, deposit it in the session, and
dispatch a synthesist agent that retrieves it via WebFetch.

```bash
# --json is especially important here: the synthesis handoff
# embeds stored analysis text (verdicts, rationales) which
# contains literal newlines. Without --json the raw emission
# would trip downstream jq -Rs on "control characters must be
# escaped."
SYNTHESIS_HANDOFF_JSON=$(signatory handoff synthesist "$TARGET" --json 2>/dev/null)

curl -s -X POST "https://127.0.0.1:21517/api/sessions/$SESSION_ID/messages" \
  -H "Content-Type: application/json" \
  --data-binary "{\"role\":\"synthesist\",\"msg_type\":\"handoff\",\"content\":$SYNTHESIS_HANDOFF_JSON}"
```

```
Agent(synthesist):
  prompt: |
    You are a synthesist for signatory's trust analysis pipeline.
    
    FIRST: Retrieve your full handoff instructions using WebFetch:
      https://127.0.0.1:21517/api/sessions/{SESSION_ID}/messages?role=synthesist&type=handoff&format=raw
    
    Follow those instructions exactly.
    
    IMPORTANT: The posture tier goes at the TOP of your output,
    before the reasoning — commit first, justify second. Read ALL
    conclusions from the store before writing anything.
    
    Your output is a narrative trust assessment (markdown).
    Write it to: filestore/analysis/{target-name}-synthesis.md
  allowed-tools: Bash Read Write Glob Grep WebFetch
```

The synthesist DOES need Bash (to run `signatory show-conclusions`,
`signatory show-analyses`, `signatory show-methodology`) and
WebFetch (to retrieve its handoff from the pipeline service).

The synthesist's output is the human-readable assessment that makes
the pipeline's data meaningful. Present it to the user as the final
result.

## Step 5 — Record posture (with user confirmation)

**The decision is the user's.** Present the recommendation; do not
record it without confirmation.

If the user approves a posture:

```bash
signatory posture set --tier "$TIER" --rationale "$RATIONALE" "$CANONICAL_URI"
```

"Analysis only — no posture recorded" is a valid terminal state for
non-dependency targets.

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
