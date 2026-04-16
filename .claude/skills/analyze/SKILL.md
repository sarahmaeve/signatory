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

## Step 2 — Generate handoff prompts

Use `--network-precheck` to auto-detect language and ecosystem from
the GitHub API. Use `--force` to overwrite stale files from prior runs.

```bash
signatory handoff security "$TARGET" \
  --network-precheck --force \
  -o /tmp/signatory-handoff-security.md

signatory handoff provenance "$TARGET" \
  --network-precheck --force \
  -o /tmp/signatory-handoff-provenance.md
```

`--network-precheck` calls the GitHub API to detect the language and
ecosystem (Python→pypi, Go→go, Rust→crates, JS→npm) and fills the
`--language` and `--ecosystem` flags automatically. The binary does
the detection — you don't need to call `gh api` yourself.

**Unfilled placeholders** (`INTAKE_QUESTION`, `TARGET_PATH`) are
expected and acceptable. Do NOT stop to fill them. Proceed to Step 3.

## Step 3 — Dispatch analyst agents IN PARALLEL

Spawn two agents in a single message (parallel dispatch). Each agent
follows its handoff prompt and produces **structured markdown** — NOT
JSON. The agent's job is analysis; the binary handles serialization.

**IMPORTANT**: send BOTH Agent calls in ONE message so they run
concurrently. Do NOT wait for one to finish before starting the other.

```
Agent(security-analyst):
  prompt: |
    You are a security analyst. Follow the instructions in
    /tmp/signatory-handoff-security.md exactly.
    
    Write your output as STRUCTURED MARKDOWN (not JSON) to:
      /tmp/signatory-output-security.md
    
    The output format is defined in your handoff instructions.
    Use the structured section format (## Conclusion: F001,
    Severity/Category/Verdict fields, Citation lines, rationale
    as body text). The orchestrator will convert your markdown to
    v1-schema JSON using `signatory build-output`.
    
    Do NOT write JSON. Do NOT run format-check. Focus entirely on
    analysis quality — the pipeline handles serialization.
  allowed-tools: Read Write Glob Grep WebFetch

Agent(provenance-analyst):
  prompt: |
    You are a provenance analyst. Follow the instructions in
    /tmp/signatory-handoff-provenance.md exactly.
    
    Write your output as STRUCTURED MARKDOWN (not JSON) to:
      /tmp/signatory-output-provenance.md
    
    The output format is defined in your handoff instructions.
    Use the structured section format (## Conclusion: F001,
    Severity/Category/Verdict fields, Citation lines, rationale
    as body text). The orchestrator will convert your markdown to
    v1-schema JSON using `signatory build-output`.
    
    Do NOT write JSON. Do NOT run format-check. Focus entirely on
    analysis quality — the pipeline handles serialization.
  allowed-tools: Read Write Glob Grep WebFetch
```

Note: Bash is NOT in the agents' allowed-tools. They don't need it —
they use Read/Write/Glob/Grep for code inspection and WebFetch for
registry APIs. The orchestrator handles all CLI commands.

Wait for BOTH agents to complete before proceeding.

## Step 4 — Convert + validate + ingest

The orchestrator (you) converts structured markdown to v1 JSON,
validates, and ingests. The agents never touch JSON.

```bash
# Convert structured text → v1 JSON
signatory build-output /tmp/signatory-output-security.md \
  --target "$CANONICAL_URI" --force \
  -o filestore/analysis/{target-name}-security-v1.json

signatory build-output /tmp/signatory-output-provenance.md \
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
signatory handoff synthesist "$TARGET" --force -o /tmp/signatory-handoff-synthesis.md
```

Read the generated prompt to verify it rendered correctly.

Then dispatch the synthesist:

```
Agent(synthesist):
  prompt: |
    You are a synthesist. Follow the instructions in
    /tmp/signatory-handoff-synthesis.md exactly.
    
    Your output is a narrative trust assessment (markdown), not
    v1-schema JSON. Present it directly — it will be shown to the
    user as the final pipeline output.
    
    IMPORTANT: Read ALL conclusions and positive absences from the
    store before writing anything. The posture tier goes at the TOP
    of your output, before the reasoning — commit first, justify
    second.
  allowed-tools: Bash Read Write Glob Grep
```

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
