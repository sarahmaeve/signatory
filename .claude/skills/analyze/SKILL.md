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
# Start the pipeline service (if not already running).
# Check first — if port 21517 is already listening, skip this.
signatory serve --port 21517 &
SERVE_PID=$!
sleep 1  # wait for server startup

# Create a session for this pipeline run.
SESSION_ID=$(curl -s -X POST https://127.0.0.1:21517/api/sessions \
  -H "Content-Type: application/json" \
  -d "{\"target\":\"$TARGET\"}" | jq -r .id)
echo "Session: $SESSION_ID"
```

Generate both handoff prompts and deposit them in the session.
`signatory handoff` handles target resolution, language/ecosystem
detection, and shallow-cloning — you do NOT need to call `gh api`
or `git clone` yourself.

```bash
# Security handoff — clones the repo into filestore/clones/.
SECURITY_HANDOFF=$(signatory handoff security "$TARGET" \
  --network-precheck --clone-dir filestore/clones/ 2>/dev/null)

# Deposit in the pipeline service.
curl -s -X POST "https://127.0.0.1:21517/api/sessions/$SESSION_ID/messages" \
  -H "Content-Type: application/json" \
  --data-binary @- <<ENDJSON
{"role":"security","msg_type":"handoff","content":$(echo "$SECURITY_HANDOFF" | jq -Rs .)}
ENDJSON

# Provenance handoff — reuses the same clone.
TARGET_NAME=$(basename "$TARGET" .git)
PROVENANCE_HANDOFF=$(signatory handoff provenance "$TARGET" \
  --network-precheck --path "filestore/clones/$TARGET_NAME" 2>/dev/null)

curl -s -X POST "https://127.0.0.1:21517/api/sessions/$SESSION_ID/messages" \
  -H "Content-Type: application/json" \
  --data-binary @- <<ENDJSON
{"role":"provenance","msg_type":"handoff","content":$(echo "$PROVENANCE_HANDOFF" | jq -Rs .)}
ENDJSON
```

If a handoff command fails, remove `2>/dev/null` and check stderr.

**Unfilled placeholders** (`INTAKE_QUESTION`, `TARGET_ROLE`) are
expected and acceptable. Do NOT stop to fill them. Proceed.

## Step 2 — Dispatch analyst agents IN PARALLEL

Spawn two agents in a single message. Agent prompts are small —
just a WebFetch URL to retrieve their instructions and a Write
path for their output.

**IMPORTANT**: send BOTH Agent calls in ONE message so they run
concurrently.

**IMPORTANT**: analyst agents do NOT have Bash. They use Read, Glob,
Grep to analyze the local clone, and WebFetch for registry/GitHub
API calls and to retrieve their handoff. Do NOT give them Bash.

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
    
    IMPORTANT OUTPUT INSTRUCTIONS:
    - Write your output as STRUCTURED MARKDOWN (not JSON).
    - Write it to: filestore/analysis/{target-name}-security-structured.md
    - The orchestrator converts your markdown to v1-schema JSON.
    - Do NOT write JSON. Do NOT run signatory commands.
    - You do NOT have Bash.
  allowed-tools: Read Write Glob Grep WebFetch

Agent(provenance-analyst):
  prompt: |
    You are a provenance analyst for signatory's trust analysis pipeline.
    
    FIRST: Retrieve your full handoff instructions using WebFetch:
      https://127.0.0.1:21517/api/sessions/{SESSION_ID}/messages?role=provenance&type=handoff&format=raw
    
    Follow those instructions exactly.
    
    IMPORTANT OUTPUT INSTRUCTIONS:
    - Write your output as STRUCTURED MARKDOWN (not JSON).
    - Write it to: filestore/analysis/{target-name}-provenance-structured.md
    - Do NOT write JSON. Do NOT run signatory commands.
    - You do NOT have Bash.
  allowed-tools: Read Write Glob Grep WebFetch
```

Wait for BOTH agents to complete before proceeding.

## Step 3 — Convert + validate + ingest

The orchestrator (you) converts structured markdown to v1 JSON,
validates, and ingests. The agents never touch JSON.

```bash
# Convert structured text → v1 JSON
signatory build-output filestore/analysis/{target-name}-security-structured.md \
  --target "$CANONICAL_URI" --force \
  -o filestore/analysis/{target-name}-security-v1.json

signatory build-output filestore/analysis/{target-name}-provenance-structured.md \
  --target "$CANONICAL_URI" --force \
  -o filestore/analysis/{target-name}-provenance-v1.json

# Ingest into the store
signatory ingest filestore/analysis/{target-name}-security-v1.json
signatory ingest filestore/analysis/{target-name}-provenance-v1.json

# Verify
signatory show-analyses "$CANONICAL_URI"
```

If `build-output` fails, it names the specific conclusion and the
missing field. Read the error, check the agent's markdown output,
and either fix the markdown or re-dispatch the agent with guidance.
Do NOT try to fix JSON — the JSON doesn't exist yet at this stage.

## Step 4 — Dispatch synthesist agent

Generate the synthesis handoff, deposit it in the session, and
dispatch a synthesist agent that retrieves it via WebFetch.

```bash
SYNTHESIS_HANDOFF=$(signatory handoff synthesist "$TARGET" 2>/dev/null)

curl -s -X POST "https://127.0.0.1:21517/api/sessions/$SESSION_ID/messages" \
  -H "Content-Type: application/json" \
  --data-binary @- <<ENDJSON
{"role":"synthesist","msg_type":"handoff","content":$(echo "$SYNTHESIS_HANDOFF" | jq -Rs .)}
ENDJSON
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

- **Do not skip format-check.** Invalid JSON silently fails at ingest.
  The analyst agents MUST validate before reporting success.

- **Do not skip synthesis.** Raw conclusions without synthesis are
  data without interpretation. The synthesis is what makes the
  pipeline useful — it's the step where an LLM adds the value that
  a database query cannot.

- **The handoff IS the instructions.** Do not invent your own
  analyst instructions. `signatory handoff` generates role-specific
  prompts from templates that encode signatory's trust model. Those
  templates are the single source of truth for what each analyst
  should do.
