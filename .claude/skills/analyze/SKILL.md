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

## Step 1 — Generate handoff prompts with clone

Generate both handoff prompts using `signatory handoff`. The binary
handles target resolution, language/ecosystem detection, and
shallow-cloning — you do NOT need to call `gh api` or `git clone`
yourself.

Capture the handoff content via stdout. Do NOT write to /tmp or any
file — subagents cannot read /tmp, and fixed filenames cause
collisions between concurrent sessions.

```bash
# The security handoff clones the repo into filestore/clones/.
# --clone-dir creates the shallow clone and fills TARGET_PATH.
# --network-precheck auto-detects language and ecosystem from GitHub.
SECURITY_HANDOFF=$(signatory handoff security "$TARGET" \
  --network-precheck --clone-dir filestore/clones/ 2>/dev/null)

# The provenance handoff reuses the same clone — pass --path
# to point at the existing checkout. Replace $TARGET_NAME with
# the inferred repo name (last path segment of the URL).
PROVENANCE_HANDOFF=$(signatory handoff provenance "$TARGET" \
  --network-precheck --path "filestore/clones/$TARGET_NAME" 2>/dev/null)
```

The binary accepts both full URLs (`https://github.com/owner/repo`)
and shorthand (`owner/repo`).

If a handoff command fails, remove `2>/dev/null` and check stderr.
Common causes: missing `--ecosystem` for non-GitHub targets (pass
it explicitly), or a clone failure (network/permissions).

**Unfilled placeholders** (`INTAKE_QUESTION`, `TARGET_ROLE`) are
expected and acceptable. Do NOT stop to fill them. Proceed.

## Step 2 — Dispatch analyst agents IN PARALLEL

Spawn two agents in a single message. **INLINE the handoff content
directly in each agent's prompt** — do not point at files. The agent
produces structured markdown and writes it to a project-local path.

**IMPORTANT**: send BOTH Agent calls in ONE message so they run
concurrently.

**IMPORTANT**: analyst agents do NOT have Bash. They use Read, Glob,
Grep to analyze the local clone, and WebFetch for registry/GitHub
API calls. The handoff template tells them what tools they have and
what URLs to fetch. Do NOT give them Bash — it leads to chaotic
shell command attempts.

For each agent prompt, paste the captured handoff content after the
preamble. The structure is:

```
Agent(security-analyst):
  prompt: |
    You are a security analyst for signatory's trust analysis pipeline.
    
    YOUR HANDOFF INSTRUCTIONS (follow these exactly):
    
    <paste $SECURITY_HANDOFF content here>
    
    IMPORTANT OUTPUT INSTRUCTIONS:
    - Write your output as STRUCTURED MARKDOWN (not JSON).
    - Write it to: filestore/analysis/{target-name}-security-structured.md
    - Use the output format from your handoff instructions:
      ## Conclusion: F001 with Severity/Category/Verdict fields,
      Citation lines, rationale as body text.
    - The orchestrator will convert your markdown to v1-schema JSON
      using `signatory build-output`. You handle analysis; the
      binary handles serialization.
    - Do NOT write JSON. Do NOT run signatory commands.
    - The source is cloned at the TARGET_PATH shown in your handoff.
      Use Read/Glob/Grep to analyze it. You do NOT have Bash.
  allowed-tools: Read Write Glob Grep WebFetch

Agent(provenance-analyst):
  prompt: |
    You are a provenance analyst for signatory's trust analysis pipeline.
    
    YOUR HANDOFF INSTRUCTIONS (follow these exactly):
    
    <paste $PROVENANCE_HANDOFF content here>
    
    IMPORTANT OUTPUT INSTRUCTIONS:
    - Write your output as STRUCTURED MARKDOWN (not JSON).
    - Write it to: filestore/analysis/{target-name}-provenance-structured.md
    - Use the output format from your handoff instructions.
    - Do NOT write JSON. Do NOT run signatory commands.
    - The source is cloned at the TARGET_PATH shown in your handoff.
      Use Read/Glob/Grep for local files and WebFetch for registry
      and GitHub API calls. You do NOT have Bash.
  allowed-tools: Read Write Glob Grep WebFetch
```

The agents write to project-local paths (filestore/analysis/) which
they DO have access to. No /tmp involvement.

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

Generate the synthesis handoff prompt and dispatch a synthesist agent.
The synthesist reads the store (not source) and produces the combined
assessment. Its instructions come from the template, not from this
skill — the template IS the single source of truth for how to
synthesize.

```bash
SYNTHESIS_HANDOFF=$(signatory handoff synthesist "$TARGET" 2>/dev/null)
```

Then dispatch the synthesist with the handoff content inlined:

```
Agent(synthesist):
  prompt: |
    You are a synthesist for signatory's trust analysis pipeline.
    
    YOUR HANDOFF INSTRUCTIONS (follow these exactly):
    
    <paste $SYNTHESIS_HANDOFF content here>
    
    IMPORTANT: The posture tier goes at the TOP of your output,
    before the reasoning — commit first, justify second. Read ALL
    conclusions from the store before writing anything.
    
    Your output is a narrative trust assessment (markdown).
    Write it to: filestore/analysis/{target-name}-synthesis.md
  allowed-tools: Bash Read Write Glob Grep
```

The synthesist DOES need Bash (to run `signatory show-conclusions`,
`signatory show-analyses`, `signatory show-methodology`).

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
