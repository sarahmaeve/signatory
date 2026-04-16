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

Use `signatory handoff` to generate the specialist prompts. These
are the instructions each analyst agent will follow.

```bash
# Security analyst prompt
signatory handoff security "$TARGET" -o /tmp/signatory-handoff-security.md

# Provenance analyst prompt
signatory handoff provenance "$TARGET" -o /tmp/signatory-handoff-provenance.md
```

If the target needs flags (e.g. `--ecosystem go`, `--language python`),
pass them through. If `signatory handoff` errors because the target
format isn't recognized, resolve the target manually in Step 1 and
pass `--url` / `--name` explicitly.

Read both generated prompts to verify they rendered correctly (no
unfilled `TARGET_*` placeholders).

## Step 3 — Dispatch analyst agents IN PARALLEL

Spawn two agents in a single message (parallel dispatch). Each agent
receives the handoff prompt as its instructions and produces v1-schema
JSON as its output.

**IMPORTANT**: send BOTH Agent calls in ONE message so they run
concurrently. Do NOT wait for one to finish before starting the other.

```
Agent(security-analyst):
  prompt: |
    You are a security analyst. Follow the instructions in
    /tmp/signatory-handoff-security.md exactly.
    
    Your output is a v1-schema JSON file. Before writing it, read
    filestore/analysis/signatory-security-v1.json as a schema
    reference — your JSON must match this structure exactly.
    
    Write your output to:
      filestore/analysis/{target-name}-security-v1.json
    
    After writing, validate:
      signatory format-check filestore/analysis/{target-name}-security-v1.json
    
    If format-check fails, fix the JSON and re-validate.
    Do NOT proceed until format-check passes.
  allowed-tools: Bash Read Write Glob Grep WebFetch

Agent(provenance-analyst):
  prompt: |
    You are a provenance analyst. Follow the instructions in
    /tmp/signatory-handoff-provenance.md exactly.
    
    Your output is a v1-schema JSON file. Before writing it, read
    filestore/analysis/thefuck-provenance-v1.json as a schema
    reference — your JSON must match this structure exactly.
    
    Write your output to:
      filestore/analysis/{target-name}-provenance-v1.json
    
    After writing, validate:
      signatory format-check filestore/analysis/{target-name}-provenance-v1.json
    
    If format-check fails, fix the JSON and re-validate.
    Do NOT proceed until format-check passes.
  allowed-tools: Bash Read Write Glob Grep WebFetch
```

Wait for BOTH agents to complete before proceeding.

## Step 4 — Ingest into the signatory store

After both agents report success, ingest their output:

```bash
signatory ingest filestore/analysis/{target-name}-security-v1.json
signatory ingest filestore/analysis/{target-name}-provenance-v1.json
```

Verify both landed:

```bash
signatory show-analyses --target "$CANONICAL_URI"
```

You should see two entries (security + provenance). If either ingest
fails, check the format-check output from the agent — the JSON may
have a structural issue the agent didn't fully resolve.

## Step 5 — Synthesize

This is the step that makes the pipeline more than raw data. You
(the orchestrator) now read the ingested data and produce a combined
assessment that no single analyst could.

Query the store for everything about this target:

```bash
signatory show-conclusions --target "$CANONICAL_URI"
signatory show-methodology --target "$CANONICAL_URI"
signatory show-analyses --target "$CANONICAL_URI"
```

Produce a synthesis that:

1. **Cross-references security + provenance conclusions.** Where do
   the analysts agree? Where do they see different things? A
   provenance concern that the security analyst didn't flag (or vice
   versa) is itself a signal — one analyst's blind spot is another's
   focus area.

2. **Weighs by forgery resistance.** Conclusions backed by
   cryptographic evidence (signed commits, trusted publishing) carry
   more weight than conclusions from self-reported metadata (star
   counts, README claims). Name the resistance level when citing a
   conclusion.

3. **Names the posture recommendation with reasoning.** Tier should
   be one of: `vetted-frozen`, `trusted-for-now`, `rejected`,
   `unknown-provenance`, or `analysis-only` (if not a dependency).
   Explain why THIS tier and not a higher or lower one.

4. **Identifies gaps.** What couldn't either analyst determine? What
   would a second round need to investigate? These gaps are honest
   limitations, not failures.

5. **States action items.** What should the user do next? Version
   pin? Monitor? Set up Renovate? Audit a specific transitive dep?

Present the synthesis to the user as a clear narrative. This is the
human-readable assessment — the structured data is in the store for
machine queries; this is the LLM-produced interpretation layer that
gives the data meaning.

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
