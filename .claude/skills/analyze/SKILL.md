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
signatory show-analyses --target "$TARGET" 2>&1
```

If analyses exist, query the conclusions:

```bash
signatory show-conclusions --target "$TARGET" 2>&1
```

**If data exists**: present the existing analysis to the user. Ask
whether they want a fresh run (re-collect) or are satisfied with the
existing assessment. If satisfied, synthesize from existing data
(skip to Step 5). If re-collecting, proceed to Step 1.

**If no data**: tell the user "no existing analysis in the store"
and proceed to Step 1.

## Step 1 — Resolve the target

Parse $ARGUMENTS to determine:
- The canonical URI (e.g. `repo:github/komoot/photon`)
- The source forge (GitHub, GitLab, etc.)
- The ecosystem if applicable (Go, Rust, npm, Python)

For GitHub targets, resolve the default branch and HEAD commit:
```bash
gh api repos/{owner}/{repo} --jq '{default_branch, pushed_at}'
gh api repos/{owner}/{repo}/commits --jq '.[0].sha' | head -c 12
```

## Step 2 — Generate handoff prompts (capture to variables, NOT files)

Capture the handoff content via stdout. Do NOT write to /tmp or any
file — subagents cannot read /tmp, and fixed filenames cause
collisions between concurrent sessions.

```bash
SECURITY_HANDOFF=$(signatory handoff security "$TARGET" --network-precheck 2>/dev/null)
PROVENANCE_HANDOFF=$(signatory handoff provenance "$TARGET" --network-precheck 2>/dev/null)
```

`--network-precheck` auto-detects language and ecosystem from the
GitHub API. The binary accepts both full URLs (`https://github.com/
owner/repo`) and shorthand (`owner/repo`).

If a handoff command fails, check stderr — usually a missing
`--ecosystem` override for non-GitHub targets. Pass it explicitly.

**Unfilled placeholders** (`INTAKE_QUESTION`, `TARGET_PATH`) are
expected and acceptable. Do NOT stop to fill them. Proceed.

## Step 3 — Dispatch analyst agents IN PARALLEL

Spawn two agents in a single message. **INLINE the handoff content
directly in each agent's prompt** — do not point at files. The agent
produces structured markdown and returns it in its response (or
writes it to a project-local path). No /tmp files involved.

**IMPORTANT**: send BOTH Agent calls in ONE message so they run
concurrently.

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
    - Write it to: filestore/analysis/click-security-structured.md
      (substitute the actual target name for "click")
    - Use the output format from your handoff instructions:
      ## Conclusion: F001 with Severity/Category/Verdict fields,
      Citation lines, rationale as body text.
    - The orchestrator will convert your markdown to v1-schema JSON
      using `signatory build-output`. You handle analysis; the
      binary handles serialization.
    - Do NOT write JSON. Do NOT run signatory commands.
  allowed-tools: Read Write Glob Grep WebFetch

Agent(provenance-analyst):
  prompt: |
    You are a provenance analyst for signatory's trust analysis pipeline.
    
    YOUR HANDOFF INSTRUCTIONS (follow these exactly):
    
    <paste $PROVENANCE_HANDOFF content here>
    
    IMPORTANT OUTPUT INSTRUCTIONS:
    - Write your output as STRUCTURED MARKDOWN (not JSON).
    - Write it to: filestore/analysis/click-provenance-structured.md
      (substitute the actual target name for "click")
    - Use the output format from your handoff instructions.
    - Do NOT write JSON. Do NOT run signatory commands.
  allowed-tools: Read Write Glob Grep WebFetch
```

The agents write to project-local paths (filestore/analysis/) which
they DO have access to. No /tmp involvement.

Wait for BOTH agents to complete before proceeding.

## Step 4 — Convert + validate + ingest

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

## Step 5 — Dispatch synthesist agent

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

## Step 6 — Record posture (with user confirmation)

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
